package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/config"
)

// -----------------------------------------------------------------------
// Tests for Server.newOllamaClient (PR74 review F8 / MF-1).
// Pre-fix this bridge was UNTESTED — a positional-argument swap or
// per-call semaphore regression would ship silently. Post-fix: the
// client is a Server-scoped singleton (PR74 F5 / M1) and env-var
// resolution happens on a TRANSIENT Cfg copy (H1).
// -----------------------------------------------------------------------

// TestServerNewOllamaClient_AppliesConfigCorrectly: table-driven checks
// that the four positional int params (maxConcurrent, timeoutMs,
// maxRetries) flow through the FromConfig sentinel contract correctly.
// Negative / zero / extreme values exercised.
func TestServerNewOllamaClient_AppliesConfigCorrectly(t *testing.T) {
	tests := []struct {
		name        string
		cfg         config.Config
		wantTimeout time.Duration
		wantRetries int
	}{
		{
			name: "sentinel_all_defaults",
			cfg: config.Config{
				OllamaEndpoint:     "http://localhost:11434",
				EmbedMaxConcurrent: -1,
				EmbedTimeoutMs:     0,
				EmbedMaxRetries:    -1,
			},
			wantTimeout: 30 * time.Second, // heimdall.DefaultEmbedTimeout
			wantRetries: 0,                // -1 → explicit zero retries
		},
		{
			name: "all_explicit_values",
			cfg: config.Config{
				OllamaEndpoint:     "http://example.com:11434",
				EmbedMaxConcurrent: 5,
				EmbedTimeoutMs:     7500,
				EmbedMaxRetries:    3,
			},
			wantTimeout: 7500 * time.Millisecond,
			wantRetries: 3,
		},
		{
			name: "zero_retries_and_unbounded_explicit",
			cfg: config.Config{
				OllamaEndpoint:     "http://localhost:11434",
				EmbedMaxConcurrent: 0, // explicit unbounded
				EmbedTimeoutMs:     1000,
				EmbedMaxRetries:    0, // sentinel → default (2)
			},
			wantTimeout: 1 * time.Second,
			wantRetries: 2, // DefaultEmbedMaxRetries
		},
		{
			name: "extreme_values_clamped_via_resolve",
			cfg: config.Config{
				OllamaEndpoint:     "http://localhost:11434",
				EmbedMaxConcurrent: 999999, // clamped to MaxEmbedConcurrent=64
				EmbedTimeoutMs:     1 << 40,
				EmbedMaxRetries:    1000000,
			},
			wantTimeout: time.Duration(config.MaxEmbedTimeoutMs) * time.Millisecond,
			wantRetries: config.MaxEmbedRetries,
		},
		{
			name: "sub_min_timeout_clamped_up",
			cfg: config.Config{
				OllamaEndpoint:     "http://localhost:11434",
				EmbedMaxConcurrent: 2,
				EmbedTimeoutMs:     5, // below MinEmbedTimeoutMs=100
				EmbedMaxRetries:    0,
			},
			wantTimeout: time.Duration(config.MinEmbedTimeoutMs) * time.Millisecond,
			wantRetries: 2,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Clear env overrides so tests are deterministic.
			t.Setenv("HEIMDALL_EMBED_MAX_CONCURRENT", "")
			t.Setenv("HEIMDALL_EMBED_TIMEOUT_MS", "")
			t.Setenv("HEIMDALL_EMBED_MAX_RETRIES", "")

			s := &Server{Cfg: tc.cfg}
			client := s.newOllamaClient()
			if client == nil {
				t.Fatal("newOllamaClient returned nil")
			}
			// White-box: in the same package, allowed.
			if client.TestEmbedTimeout() != tc.wantTimeout {
				t.Errorf("embedTimeout = %v, want %v", client.TestEmbedTimeout(), tc.wantTimeout)
			}
			if client.TestMaxRetries() != tc.wantRetries {
				t.Errorf("maxRetries = %d, want %d", client.TestMaxRetries(), tc.wantRetries)
			}
			// Endpoint assertion (PR #74 re-review L1). Guards against a
			// future refactor accidentally swapping positional args so
			// the Model string ends up passed as endpoint.
			if client.TestEndpoint() != tc.cfg.OllamaEndpoint {
				t.Errorf("endpoint = %q, want %q (positional-arg wiring regression)",
					client.TestEndpoint(), tc.cfg.OllamaEndpoint)
			}
		})
	}
}

// TestServerNewOllamaClient_Singleton: consecutive calls with the same
// Cfg values return the SAME *OllamaClient so the semaphore is shared
// across concurrent MCP tool invocations. Pre-fix every tool call built
// a fresh client → effective cap was N × max_concurrent. See F5 / M1.
func TestServerNewOllamaClient_Singleton(t *testing.T) {
	t.Setenv("HEIMDALL_EMBED_MAX_CONCURRENT", "")
	t.Setenv("HEIMDALL_EMBED_TIMEOUT_MS", "")
	t.Setenv("HEIMDALL_EMBED_MAX_RETRIES", "")

	s := &Server{Cfg: config.Config{
		OllamaEndpoint:     "http://localhost:11434",
		EmbedMaxConcurrent: 2,
		EmbedTimeoutMs:     1000,
		EmbedMaxRetries:    1,
	}}

	c1 := s.newOllamaClient()
	c2 := s.newOllamaClient()
	if c1 != c2 {
		t.Fatal("newOllamaClient returned different pointers on consecutive calls — singleton broken")
	}
}

// TestServerNewOllamaClient_RebuildsOnConfigChange: if the resolved
// embed config changes (e.g. toolConfigure mutated a field), the next
// call rebuilds. Verifies the key-based cache invalidates cleanly.
func TestServerNewOllamaClient_RebuildsOnConfigChange(t *testing.T) {
	t.Setenv("HEIMDALL_EMBED_MAX_CONCURRENT", "")
	t.Setenv("HEIMDALL_EMBED_TIMEOUT_MS", "")
	t.Setenv("HEIMDALL_EMBED_MAX_RETRIES", "")

	s := &Server{Cfg: config.Config{
		OllamaEndpoint:     "http://localhost:11434",
		EmbedMaxConcurrent: 2,
		EmbedTimeoutMs:     1000,
		EmbedMaxRetries:    1,
	}}

	c1 := s.newOllamaClient()
	s.Cfg.EmbedMaxConcurrent = 4 // simulate toolConfigure set
	c2 := s.newOllamaClient()
	if c1 == c2 {
		t.Fatal("newOllamaClient did not rebuild after Cfg change")
	}
}

// TestMCP_SemaphoreSharedAcrossConcurrentToolCalls: regression for F5 /
// M1. Two concurrent goroutines each invoke s.newOllamaClient() and
// issue embed calls against a slow test server. The aggregate peak
// in-flight MUST be bounded by cfg.EmbedMaxConcurrent, not
// 2 × cfg.EmbedMaxConcurrent (pre-fix behaviour).
func TestMCP_SemaphoreSharedAcrossConcurrentToolCalls(t *testing.T) {
	t.Setenv("HEIMDALL_EMBED_MAX_CONCURRENT", "")
	t.Setenv("HEIMDALL_EMBED_TIMEOUT_MS", "")
	t.Setenv("HEIMDALL_EMBED_MAX_RETRIES", "")

	release := make(chan struct{})
	var inflight, peak int64
	started := make(chan struct{}, 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := atomic.AddInt64(&inflight, 1)
		for {
			old := atomic.LoadInt64(&peak)
			if cur <= old || atomic.CompareAndSwapInt64(&peak, old, cur) {
				break
			}
		}
		started <- struct{}{}
		defer atomic.AddInt64(&inflight, -1)

		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"embeddings":[[1.0]]}`))
	}))
	t.Cleanup(srv.Close)

	const cap = 2
	s := &Server{Cfg: config.Config{
		OllamaEndpoint:     srv.URL,
		EmbedMaxConcurrent: cap,
		EmbedMaxRetries:    -1, // no retries for clean peak accounting
	}}

	// Two "tool call" goroutines, each issuing several concurrent embeds
	// via the server's singleton client.
	const toolCalls = 2
	const embedsPerCall = 4
	var wg sync.WaitGroup
	for t1 := 0; t1 < toolCalls; t1++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client := s.newOllamaClient() // should return the singleton
			var inner sync.WaitGroup
			for i := 0; i < embedsPerCall; i++ {
				inner.Add(1)
				go func() {
					defer inner.Done()
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					_, _ = client.Embed(ctx, "m", "t")
				}()
			}
			inner.Wait()
		}()
	}

	// Wait for cap goroutines to be in-flight.
	for i := 0; i < cap; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout waiting for request %d/%d to land", i+1, cap)
		}
	}
	close(release)
	wg.Wait()

	if got := atomic.LoadInt64(&peak); got > int64(cap) {
		t.Fatalf("peak in-flight = %d, want <= %d (semaphore not shared across tool calls — regression of F5)", got, cap)
	}
}

// TestServerNewOllamaClient_EnvOverrideDoesNotMutateCfg: regression for
// H1/F2. Calling newOllamaClient with HEIMDALL_EMBED_* env vars set
// resolves the effective config internally; Server.Cfg itself MUST NOT
// change, so a later SaveConfig(s.Cfg) cannot leak env values to disk.
func TestServerNewOllamaClient_EnvOverrideDoesNotMutateCfg(t *testing.T) {
	t.Setenv("HEIMDALL_EMBED_MAX_CONCURRENT", "9")
	t.Setenv("HEIMDALL_EMBED_TIMEOUT_MS", "12345")
	t.Setenv("HEIMDALL_EMBED_MAX_RETRIES", "7")

	s := &Server{Cfg: config.Config{
		OllamaEndpoint:     "http://localhost:11434",
		EmbedMaxConcurrent: 2, // on-disk value
		EmbedTimeoutMs:     1000,
		EmbedMaxRetries:    1,
	}}
	snapshot := s.Cfg // value copy for comparison
	_ = s.newOllamaClient()

	if s.Cfg.EmbedMaxConcurrent != snapshot.EmbedMaxConcurrent {
		t.Errorf("s.Cfg.EmbedMaxConcurrent = %d, want %d (unchanged — env must be transient)",
			s.Cfg.EmbedMaxConcurrent, snapshot.EmbedMaxConcurrent)
	}
	if s.Cfg.EmbedTimeoutMs != snapshot.EmbedTimeoutMs {
		t.Errorf("s.Cfg.EmbedTimeoutMs = %d, want %d (unchanged — env must be transient)",
			s.Cfg.EmbedTimeoutMs, snapshot.EmbedTimeoutMs)
	}
	if s.Cfg.EmbedMaxRetries != snapshot.EmbedMaxRetries {
		t.Errorf("s.Cfg.EmbedMaxRetries = %d, want %d (unchanged — env must be transient)",
			s.Cfg.EmbedMaxRetries, snapshot.EmbedMaxRetries)
	}
}

// TestServer_ConfigureSetRaceFreeUnderRace — PR #74 re-review N-1 race
// guard. Concurrent `heimdall_configure set embedMaxConcurrent ...` +
// `newOllamaClient()` calls must not race on Server.Cfg. Before the fix,
// `newOllamaClient` read every embed field of s.Cfg outside any lock,
// while `configureSet` wrote via `def.Set(&s.Cfg, ...)` unsynchronized.
// Run this test under `-race` to catch regressions.
func TestServer_ConfigureSetRaceFreeUnderRace(t *testing.T) {
	t.Setenv("HEIMDALL_EMBED_MAX_CONCURRENT", "")
	t.Setenv("HEIMDALL_EMBED_TIMEOUT_MS", "")
	t.Setenv("HEIMDALL_EMBED_MAX_RETRIES", "")

	// Sandbox SaveConfig so test writes don't touch the user's config.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	s := &Server{Cfg: config.Config{
		OllamaEndpoint:     "http://localhost:11434",
		Model:              "nomic-embed-text",
		EmbedMaxConcurrent: 2,
		EmbedTimeoutMs:     1000,
		EmbedMaxRetries:    1,
	}}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writer: cycle embedMaxConcurrent through a small set of values via
	// configureSet. Uses the json.RawMessage input surface the actual
	// MCP tool invokes to exercise the same code path end-to-end.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			val := 2 + (i % 4) // 2,3,4,5
			args := []byte(`{"action":"set","key":"embedMaxConcurrent","value":` +
				strconvItoa(val) + `}`)
			_ = s.toolConfigure(args)
		}
	}()

	// Readers: hammer newOllamaClient concurrently.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = s.newOllamaClient()
			}
		}()
	}

	// Let the storm run long enough for the race detector to observe
	// concurrent read/write on the Cfg fields if unprotected.
	time.Sleep(75 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestHeimdallInstructionsTriggerPairs(t *testing.T) {
	markers := []string{
		"WHEN you're about to Read a file > 200 lines",
		"WHEN the user corrects you",
		"WHEN a WebFetch, Read, or external-MCP call",
		"WHEN starting a task that references past decisions",
		"WHEN search feels wrong",
		"Last session review",
	}
	for _, m := range markers {
		if !strings.Contains(heimdallInstructions, m) {
			t.Errorf("heimdallInstructions missing marker: %q", m)
		}
	}
}

func TestHandleToolsCall_LogsEveryInvocation(t *testing.T) {
	// Capture log lines via a temporary HEIMDALL_HOOK_LOG path.
	tmp := t.TempDir()
	t.Setenv("HEIMDALL_HOOK_LOG", filepath.Join(tmp, "hooks.log"))

	// Build a minimal server. Use heimdall_configure (get action) because it
	// needs no live index or registry — the dispatcher just reads s.Cfg and
	// returns. We only need the dispatcher to execute and emit the log line.
	s := &Server{Cfg: config.DefaultConfig()}
	req := JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"heimdall_configure","arguments":{"action":"get","key":"model"}}`),
	}
	_ = s.handleToolsCall(req)

	body, err := os.ReadFile(filepath.Join(tmp, "hooks.log"))
	if err != nil {
		t.Fatalf("hooks.log not written: %v", err)
	}
	s2 := string(body)
	if !strings.Contains(s2, "event=mcp.tool_call") {
		t.Errorf("expected event=mcp.tool_call in log; got %s", s2)
	}
	if !strings.Contains(s2, `tool=heimdall_configure`) {
		t.Errorf("expected tool=heimdall_configure in log; got %s", s2)
	}
	if !strings.Contains(s2, "duration_ms=") {
		t.Errorf("expected duration_ms field in log; got %s", s2)
	}
}

// TestHandleToolsCall_LogsMCPServerKey verifies that every mcp.tool_call log
// line includes the mcp_server= field so operators can bucket calls by
// MCP-server-lifetime (B3 fix).
func TestHandleToolsCall_LogsMCPServerKey(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HEIMDALL_HOOK_LOG", filepath.Join(tmp, "hooks.log"))

	s := &Server{Cfg: config.DefaultConfig()}
	req := JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"heimdall_configure","arguments":{"action":"get","key":"model"}}`),
	}
	_ = s.handleToolsCall(req)

	body, err := os.ReadFile(filepath.Join(tmp, "hooks.log"))
	if err != nil {
		t.Fatalf("hooks.log not written: %v", err)
	}
	logStr := string(body)
	if !strings.Contains(logStr, "mcp_server=") {
		t.Errorf("expected mcp_server= field in mcp.tool_call log line; got:\n%s", logStr)
	}
	// Key must be non-empty and follow the "mcp-<host>-<pid>" pattern.
	if !strings.Contains(logStr, "mcp_server=mcp-") {
		t.Errorf("expected mcp_server=mcp-<host>-<pid> format; got:\n%s", logStr)
	}
}

// strconvItoa is a tiny local helper to keep the test independent of
// strconv import bloat at the top of the file.
func strconvItoa(n int) string {
	// We only pass small positive ints, so this is safe + allocates
	// once per call. No need for a full implementation.
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// TestServerNewOllamaClient_RebuildDisposesOldClient — PR #74 re-review
// N-3. After a config-driven rebuild, the OLD client must have had
// Dispose() called so its HTTP keep-alive pool is released. We can't
// directly observe CloseIdleConnections, but we CAN verify that the
// rebuild does not panic and that the new client is a distinct object,
// AND that Dispose() on a client with in-flight work does not break
// subsequent Embed calls.
func TestServerNewOllamaClient_RebuildDisposesOldClient(t *testing.T) {
	t.Setenv("HEIMDALL_EMBED_MAX_CONCURRENT", "")
	t.Setenv("HEIMDALL_EMBED_TIMEOUT_MS", "")
	t.Setenv("HEIMDALL_EMBED_MAX_RETRIES", "")

	// A slow HTTP server so we can keep a request in-flight on the OLD
	// client during a rebuild.
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"embeddings":[[1.0]]}`))
	}))
	t.Cleanup(srv.Close)

	s := &Server{Cfg: config.Config{
		OllamaEndpoint:     srv.URL,
		EmbedMaxConcurrent: 4,
		EmbedTimeoutMs:     5000,
		EmbedMaxRetries:    -1,
	}}

	oldClient := s.newOllamaClient()

	// Fire an in-flight request on the old client.
	embedDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := oldClient.Embed(ctx, "m", "t")
		embedDone <- err
	}()
	<-started

	// Trigger a rebuild by changing cfg. This should call Dispose() on
	// oldClient — which must NOT cancel the in-flight request.
	s.CfgMu.Lock()
	s.Cfg.EmbedMaxConcurrent = 8
	s.CfgMu.Unlock()
	newClient := s.newOllamaClient()
	if newClient == oldClient {
		t.Fatalf("rebuild did not produce a new client")
	}

	// Let the in-flight request finish — it must NOT have been
	// cancelled by Dispose.
	close(release)
	select {
	case err := <-embedDone:
		if err != nil {
			t.Errorf("in-flight embed on old client failed after Dispose: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight embed on old client never returned")
	}
}
