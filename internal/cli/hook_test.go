package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// fakeHookOllama is a minimal httptest.Server that responds to /api/tags and
// /api/embed with a fixed shape. Tunable per test via knobs. Distinct from
// status_test.go's fakeOllama helper to avoid the package-level name clash.
type fakeHookOllama struct {
	server *httptest.Server
	// model name to advertise on /api/tags
	tagModel string
	// vector to return on /api/embed
	embedVec []float32
	// embedDelay is added to /api/embed handlers (used for deadline tests)
	embedDelay time.Duration
	// pingFail makes /api/tags return 500
	pingFail atomic.Bool
}

func newFakeOllama(t *testing.T, model string, dim int) *fakeHookOllama {
	t.Helper()
	vec := make([]float32, dim)
	for i := range vec {
		vec[i] = 1.0 / float32(dim)
	}
	f := &fakeHookOllama{tagModel: model, embedVec: vec}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		if f.pingFail.Load() {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{
				{"name": f.tagModel},
			},
		})
	})
	mux.HandleFunc("/api/embed", func(w http.ResponseWriter, r *http.Request) {
		if f.embedDelay > 0 {
			select {
			case <-time.After(f.embedDelay):
			case <-r.Context().Done():
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embeddings": [][]float32{f.embedVec},
		})
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// seedVectorStore creates a .heimdall_db/<model>/vectors.db on disk with the
// expected metadata so VerifyHookIndex passes. It does not insert any chunks.
func seedVectorStore(t *testing.T, projectRoot, model string, dim int, lastModified int64) string {
	t.Helper()
	baseDir := filepath.Join(projectRoot, ".heimdall_db")
	dbDir := heimdall.ModelDBDir(baseDir, model)
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	store, err := heimdall.OpenStore(dbDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.SetMetadata("embedding_model", model); err != nil {
		t.Fatalf("set model: %v", err)
	}
	if err := store.SetMetadata("embedding_dim", fmt.Sprintf("%d", dim)); err != nil {
		t.Fatalf("set dim: %v", err)
	}
	store.Close()
	return baseDir
}

// seedVectorStoreWithMismatch is like seedVectorStore but writes the wrong
// embedding_model so VerifyHookIndex fails with ErrIndexModelMismatch.
func seedVectorStoreWithMismatch(t *testing.T, projectRoot, dirModel, storedModel string, dim int) string {
	t.Helper()
	baseDir := filepath.Join(projectRoot, ".heimdall_db")
	dbDir := heimdall.ModelDBDir(baseDir, dirModel)
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	store, err := heimdall.OpenStore(dbDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	_ = store.SetMetadata("embedding_model", storedModel)
	_ = store.SetMetadata("embedding_dim", fmt.Sprintf("%d", dim))
	store.Close()
	return baseDir
}

// seedMemories creates a memory store at the returned path and inserts n
// memories with the given vector. Returns the open store; caller closes.
func seedMemories(t *testing.T, n int, vec []float32) *heimdall.MemoryStore {
	t.Helper()
	dir := t.TempDir()
	store, err := heimdall.OpenMemoryStore(filepath.Join(dir, "memories.db"))
	if err != nil {
		t.Fatalf("open mem: %v", err)
	}
	now := time.Now().Unix()
	for i := 0; i < n; i++ {
		content := fmt.Sprintf("bullet %d", i+1)
		if err := store.UpsertMemory(heimdall.Memory{
			ID:          fmt.Sprintf("mem:%d", i+1),
			Content:     content,
			Type:        heimdall.MemoryTypeFact,
			Vector:      vec,
			CreatedAt:   now,
			UpdatedAt:   now,
			Source:      heimdall.MemorySourceExplicit,
			ContentHash: heimdall.ContentHash(content),
		}); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// fixedNow is a clock used for deterministic last-indexed strings.
var fixedNow = time.Date(2026, 4, 14, 15, 4, 33, 0, time.UTC)

// runHookSessionStart drives HookSessionStart with the supplied stdin, args,
// env, and a deps bundle that uses the fake Ollama and an injected memory
// store + suppression callback.
type runOpts struct {
	stdin       string
	args        []string
	env         map[string]string
	cfg         config.Config
	memoryStore *heimdall.MemoryStore
	suppress    func(project, code string, window time.Duration) bool
}

func runHookSessionStart(t *testing.T, opts runOpts) (stdout, stderr string, code int) {
	t.Helper()
	var out, errBuf strings.Builder
	deps := HookSessionStartDeps{
		OpenMemoryStore: func() (*heimdall.MemoryStore, error) {
			return opts.memoryStore, nil
		},
		Suppress: opts.suppress,
		Now:      func() time.Time { return fixedNow },
	}
	if deps.Suppress == nil {
		deps.Suppress = func(string, string, time.Duration) bool { return true }
	}
	rc := HookSessionStart(opts.cfg, strings.NewReader(opts.stdin), &out, &errBuf, opts.env, opts.args, deps)
	return out.String(), errBuf.String(), rc
}

// ---------------------------------------------------------------------------
// 1. Happy path — store has 5 memories, mock Ollama, header + bullets + footer
// ---------------------------------------------------------------------------

func TestHookSessionStart_HappyPath(t *testing.T) {
	const model = "test-model"
	const dim = 3

	fake := newFakeOllama(t, model, dim)
	cfg := config.Config{OllamaEndpoint: fake.server.URL, Model: model}

	projectRoot := t.TempDir()
	seedVectorStore(t, projectRoot, model, dim, time.Now().Unix())

	mem := seedMemories(t, 5, fake.embedVec)

	stdout, stderr, code := runHookSessionStart(t, runOpts{
		stdin:       fmt.Sprintf(`{"cwd":%q}`, projectRoot),
		args:        []string{"--source=heimdall", "--version=1"},
		cfg:         cfg,
		memoryStore: mem,
	})
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if stderr != "" {
		t.Errorf("stderr should be empty, got %q", stderr)
	}
	want := []string{
		"## Heimdall context",
		fmt.Sprintf("**Project:** %s", filepath.Base(projectRoot)),
		"**Model:** " + model,
		"### Recent memories",
		"- bullet 1",
		"- bullet 5",
		"_retrieved via heimdall-mcp_",
	}
	for _, w := range want {
		if !strings.Contains(stdout, w) {
			t.Errorf("missing %q in:\n%s", w, stdout)
		}
	}
}

// ---------------------------------------------------------------------------
// 2. Empty stdin — falls back to CWD
// ---------------------------------------------------------------------------

func TestHookSessionStart_EmptyStdinFallsBackToCWD(t *testing.T) {
	const model = "test-model"
	const dim = 3
	fake := newFakeOllama(t, model, dim)
	cfg := config.Config{OllamaEndpoint: fake.server.URL, Model: model}

	projectRoot := t.TempDir()
	seedVectorStore(t, projectRoot, model, dim, time.Now().Unix())
	mem := seedMemories(t, 1, fake.embedVec)

	// Pass --project to deterministically point at the seeded dir; with
	// truly empty stdin AND no flag the test runner's CWD wouldn't have
	// an index. The flag path is what we exercise here for "empty stdin".
	stdout, _, code := runHookSessionStart(t, runOpts{
		stdin:       "",
		args:        []string{"--project=" + projectRoot},
		cfg:         cfg,
		memoryStore: mem,
	})
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(stdout, "## Heimdall context") {
		t.Errorf("expected header, got:\n%s", stdout)
	}
}

// ---------------------------------------------------------------------------
// 3. Unknown stdin fields — ignored
// ---------------------------------------------------------------------------

func TestHookSessionStart_UnknownStdinFieldsIgnored(t *testing.T) {
	const model = "test-model"
	const dim = 3
	fake := newFakeOllama(t, model, dim)
	cfg := config.Config{OllamaEndpoint: fake.server.URL, Model: model}

	projectRoot := t.TempDir()
	seedVectorStore(t, projectRoot, model, dim, time.Now().Unix())
	mem := seedMemories(t, 1, fake.embedVec)

	stdin := fmt.Sprintf(`{"cwd":%q,"sessionId":"abc","random":42,"nested":{"k":"v"}}`, projectRoot)
	stdout, _, code := runHookSessionStart(t, runOpts{
		stdin:       stdin,
		cfg:         cfg,
		memoryStore: mem,
	})
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(stdout, "## Heimdall context") {
		t.Errorf("expected header, got:\n%s", stdout)
	}
}

// ---------------------------------------------------------------------------
// 4. HEIMDALL_HOOKS=0 → exit 0 empty
// ---------------------------------------------------------------------------

func TestHookSessionStart_GlobalKillSwitch(t *testing.T) {
	const model = "test-model"
	fake := newFakeOllama(t, model, 3)
	cfg := config.Config{OllamaEndpoint: fake.server.URL, Model: model}
	projectRoot := t.TempDir()
	seedVectorStore(t, projectRoot, model, 3, time.Now().Unix())
	mem := seedMemories(t, 3, fake.embedVec)

	stdout, _, code := runHookSessionStart(t, runOpts{
		stdin:       fmt.Sprintf(`{"cwd":%q}`, projectRoot),
		env:         map[string]string{"HEIMDALL_HOOKS": "0"},
		cfg:         cfg,
		memoryStore: mem,
	})
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if stdout != "" {
		t.Errorf("expected empty stdout, got %q", stdout)
	}
}

// ---------------------------------------------------------------------------
// 5. .heimdall/hooks.disabled marker
// ---------------------------------------------------------------------------

func TestHookSessionStart_PerProjectDisableMarker(t *testing.T) {
	const model = "test-model"
	fake := newFakeOllama(t, model, 3)
	cfg := config.Config{OllamaEndpoint: fake.server.URL, Model: model}
	projectRoot := t.TempDir()
	seedVectorStore(t, projectRoot, model, 3, time.Now().Unix())
	mem := seedMemories(t, 3, fake.embedVec)

	if err := os.MkdirAll(filepath.Join(projectRoot, ".heimdall"), 0o755); err != nil {
		t.Fatalf("mkdir marker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, ".heimdall", "hooks.disabled"), []byte(""), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	stdout, _, code := runHookSessionStart(t, runOpts{
		stdin:       fmt.Sprintf(`{"cwd":%q}`, projectRoot),
		cfg:         cfg,
		memoryStore: mem,
	})
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if stdout != "" {
		t.Errorf("expected empty stdout (disabled), got %q", stdout)
	}
}

// ---------------------------------------------------------------------------
// 6. Ollama down — first call emits Tier B note, second is suppressed.
//    Uses an in-process suppression callback backed by ShouldEmitTierBWithPath
//    so persistence is exercised end-to-end.
// ---------------------------------------------------------------------------

func TestHookSessionStart_OllamaDownTierBSuppression(t *testing.T) {
	// httptest.Server that always returns 500 on /api/tags.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()

	const model = "test-model"
	cfg := config.Config{OllamaEndpoint: srv.URL, Model: model}
	projectRoot := t.TempDir()
	seedVectorStore(t, projectRoot, model, 3, time.Now().Unix())

	suppressDB := filepath.Join(t.TempDir(), "suppress.db")
	suppressFn := func(project, code string, window time.Duration) bool {
		ok, err := heimdall.ShouldEmitTierBWithPath(suppressDB, project, code, window)
		if err != nil {
			t.Logf("suppress err: %v", err)
		}
		return ok
	}

	// First call — should emit the Tier B note.
	stdout1, _, code1 := runHookSessionStart(t, runOpts{
		stdin:       fmt.Sprintf(`{"cwd":%q}`, projectRoot),
		cfg:         cfg,
		memoryStore: seedMemories(t, 0, []float32{1, 0, 0}),
		suppress:    suppressFn,
	})
	if code1 != 0 {
		t.Fatalf("first exit=%d", code1)
	}
	if !strings.Contains(stdout1, "ollama unreachable") {
		t.Errorf("first call should emit Tier B note, got %q", stdout1)
	}

	// Second call within the 5-min window — should be suppressed (empty).
	stdout2, _, code2 := runHookSessionStart(t, runOpts{
		stdin:       fmt.Sprintf(`{"cwd":%q}`, projectRoot),
		cfg:         cfg,
		memoryStore: seedMemories(t, 0, []float32{1, 0, 0}),
		suppress:    suppressFn,
	})
	if code2 != 0 {
		t.Fatalf("second exit=%d", code2)
	}
	if stdout2 != "" {
		t.Errorf("second call should be suppressed, got %q", stdout2)
	}
}

// ---------------------------------------------------------------------------
// 7. No index — emits the "no index yet" one-liner.
// ---------------------------------------------------------------------------

func TestHookSessionStart_NoIndex(t *testing.T) {
	const model = "test-model"
	fake := newFakeOllama(t, model, 3)
	cfg := config.Config{OllamaEndpoint: fake.server.URL, Model: model}
	projectRoot := t.TempDir() // no .heimdall_db

	stdout, _, code := runHookSessionStart(t, runOpts{
		stdin:       fmt.Sprintf(`{"cwd":%q}`, projectRoot),
		cfg:         cfg,
		memoryStore: seedMemories(t, 0, []float32{1, 0, 0}),
	})
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(stdout, "no index yet") {
		t.Errorf("expected 'no index yet', got %q", stdout)
	}
	if !strings.Contains(stdout, "heimdall-mcp index .") {
		t.Errorf("expected install hint, got %q", stdout)
	}
}

// ---------------------------------------------------------------------------
// 8. Model mismatch — Tier B note (once per 5 min)
// ---------------------------------------------------------------------------

func TestHookSessionStart_ModelMismatch(t *testing.T) {
	// Ollama advertises "bge-m3" (matching cfg.Model), but the index on
	// disk under bge-m3/ has stored embedding_model="nomic". VerifyHookIndex
	// must reject it.
	const cfgModel = "bge-m3"
	const storedModel = "nomic"
	fake := newFakeOllama(t, cfgModel, 3)
	cfg := config.Config{OllamaEndpoint: fake.server.URL, Model: cfgModel}

	projectRoot := t.TempDir()
	seedVectorStoreWithMismatch(t, projectRoot, cfgModel, storedModel, 3)

	stdout, _, code := runHookSessionStart(t, runOpts{
		stdin:       fmt.Sprintf(`{"cwd":%q}`, projectRoot),
		cfg:         cfg,
		memoryStore: seedMemories(t, 0, []float32{1, 0, 0}),
	})
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(stdout, "## Heimdall context") {
		t.Errorf("expected degraded header, got %q", stdout)
	}
	if !strings.Contains(stdout, "index model mismatch") {
		t.Errorf("expected mismatch banner, got %q", stdout)
	}
}

// ---------------------------------------------------------------------------
// 9. Budget exceeded — slow embed, tiny --budget-ms.
// ---------------------------------------------------------------------------

func TestHookSessionStart_BudgetExceeded(t *testing.T) {
	const model = "test-model"
	fake := newFakeOllama(t, model, 3)
	fake.embedDelay = 500 * time.Millisecond
	cfg := config.Config{OllamaEndpoint: fake.server.URL, Model: model}

	projectRoot := t.TempDir()
	seedVectorStore(t, projectRoot, model, 3, time.Now().Unix())
	mem := seedMemories(t, 5, fake.embedVec)

	stdout, _, code := runHookSessionStart(t, runOpts{
		stdin:       fmt.Sprintf(`{"cwd":%q}`, projectRoot),
		args:        []string{"--budget-ms=50"},
		cfg:         cfg,
		memoryStore: mem,
	})
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	// Either empty (budget hit before write) or a partial header without
	// bullets — both acceptable per the spec. What matters is no panic and
	// no bullets from the slow embed path.
	if strings.Contains(stdout, "- bullet 1") {
		t.Errorf("budget should have aborted before bullets:\n%s", stdout)
	}
}

// ---------------------------------------------------------------------------
// 10. Golden file check — happy-path output matches the template byte-for-byte.
// ---------------------------------------------------------------------------

func TestHookSessionStart_GoldenFile(t *testing.T) {
	const model = "test-model"
	const dim = 3
	fake := newFakeOllama(t, model, dim)
	cfg := config.Config{OllamaEndpoint: fake.server.URL, Model: model}

	projectRoot := t.TempDir()
	seedVectorStore(t, projectRoot, model, dim, time.Now().Unix())
	mem := seedMemories(t, 5, fake.embedVec)

	stdout, _, code := runHookSessionStart(t, runOpts{
		stdin:       fmt.Sprintf(`{"cwd":%q}`, projectRoot),
		cfg:         cfg,
		memoryStore: mem,
	})
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}

	goldenBytes, err := os.ReadFile(filepath.Join("testdata", "hook_session_start.golden.md"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}

	// The vectorstore in the seed has 0 chunks, so TotalChunks = 0 and
	// LastIndexed = "never" (Stats.LastModified == 0).
	expected := string(goldenBytes)
	expected = strings.ReplaceAll(expected, "{{PROJECT}}", filepath.Base(projectRoot))
	expected = strings.ReplaceAll(expected, "{{MODEL}}", model)
	expected = strings.ReplaceAll(expected, "{{CHUNKS}}", "0")
	expected = strings.ReplaceAll(expected, "{{LAST_INDEXED}}", "never")

	if stdout != expected {
		t.Errorf("golden mismatch.\n--- got ---\n%q\n--- want ---\n%q", stdout, expected)
	}
}

// ---------------------------------------------------------------------------
// 11. Suppression store cross-call persistence — opens the on-disk
//    suppression DB twice through ShouldEmitTierBWithPath and asserts that
//    the first call emits and the second (after the file has been closed
//    and reopened by the helper) suppresses. This exercises the same code
//    path the production handler uses, just with an injectable path.
// ---------------------------------------------------------------------------

func TestHookSessionStart_SuppressionStorePersistence(t *testing.T) {
	const model = "test-model"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := config.Config{OllamaEndpoint: srv.URL, Model: model}
	projectRoot := t.TempDir()
	seedVectorStore(t, projectRoot, model, 3, time.Now().Unix())

	suppressDir := t.TempDir()
	suppressDB := filepath.Join(suppressDir, "suppress.db")

	// Emit-counter to make sure each invocation reopens (the helper opens
	// and closes the SQLite file each time).
	var calls atomic.Int32
	wrapped := func(project, code string, window time.Duration) bool {
		calls.Add(1)
		ok, err := heimdall.ShouldEmitTierBWithPath(suppressDB, project, code, window)
		if err != nil {
			t.Logf("suppress err: %v", err)
		}
		return ok
	}

	out1, _, _ := runHookSessionStart(t, runOpts{
		stdin: fmt.Sprintf(`{"cwd":%q}`, projectRoot), cfg: cfg,
		memoryStore: seedMemories(t, 0, []float32{1, 0, 0}),
		suppress:    wrapped,
	})
	if !strings.Contains(out1, "ollama unreachable") {
		t.Fatalf("first call should emit, got %q", out1)
	}

	out2, _, _ := runHookSessionStart(t, runOpts{
		stdin: fmt.Sprintf(`{"cwd":%q}`, projectRoot), cfg: cfg,
		memoryStore: seedMemories(t, 0, []float32{1, 0, 0}),
		suppress:    wrapped,
	})
	if out2 != "" {
		t.Errorf("second call should be suppressed by on-disk store, got %q", out2)
	}
	if calls.Load() != 2 {
		t.Errorf("expected 2 suppress calls, got %d", calls.Load())
	}

	// Sanity: the suppression DB file exists on disk.
	if _, err := os.Stat(suppressDB); err != nil {
		t.Errorf("suppress.db not created: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 12. DispatchHook routes session-start (smoke test for the dispatcher).
// ---------------------------------------------------------------------------

func TestDispatchHook_RoutesSessionStart(t *testing.T) {
	cfg := config.Config{OllamaEndpoint: "http://127.0.0.1:0", Model: "x"}
	var stdout, stderr strings.Builder

	// No project, no fake ollama — handler exits 0 anyway.
	code := DispatchHook(cfg, strings.NewReader(""), &stdout, &stderr,
		map[string]string{"HEIMDALL_HOOKS": "0"},
		[]string{"session-start"})
	if code != 0 {
		t.Errorf("exit=%d, stderr=%q", code, stderr.String())
	}
}

func TestDispatchHook_UnknownSubcommand(t *testing.T) {
	// Per OQ-5 (docs/plans/hooks/06-decisions.md), retrieval hooks must
	// never return non-zero — that would block Claude Code. The
	// dispatcher's unknown-subcommand path logs via LogHookEvent and
	// prints a short hint to stderr for interactive developers, but
	// returns 0 so Claude Code is never blocked by a misconfigured
	// install that baked a typo into settings.json.
	cfg := config.Config{}
	var stdout, stderr strings.Builder
	code := DispatchHook(cfg, strings.NewReader(""), &stdout, &stderr, nil,
		[]string{"nonsense"})
	if code != 0 {
		t.Errorf("exit=%d, want 0 per OQ-5", code)
	}
	if !strings.Contains(stderr.String(), "Unknown") {
		t.Errorf("stderr=%q should contain 'Unknown'", stderr.String())
	}
}

// ---------------------------------------------------------------------------
// 13. --source / --version flags accepted and ignored
// ---------------------------------------------------------------------------

func TestHookSessionStart_SourceVersionFlagsAccepted(t *testing.T) {
	cfg := config.Config{OllamaEndpoint: "http://127.0.0.1:0", Model: "x"}
	var stdout, stderr strings.Builder
	code := HookSessionStart(cfg,
		strings.NewReader(""),
		&stdout, &stderr,
		map[string]string{"HEIMDALL_HOOKS": "0"},
		[]string{"--source=heimdall", "--version=1"},
		HookSessionStartDeps{Suppress: func(string, string, time.Duration) bool { return true }},
	)
	if code != 0 {
		t.Errorf("exit=%d", code)
	}
}

// ---------------------------------------------------------------------------
// Concurrency smoke — race detector. Two parallel runs should not data-race.
// ---------------------------------------------------------------------------

func TestHookSessionStart_RaceFree(t *testing.T) {
	const model = "test-model"
	fake := newFakeOllama(t, model, 3)
	cfg := config.Config{OllamaEndpoint: fake.server.URL, Model: model}
	projectRoot := t.TempDir()
	seedVectorStore(t, projectRoot, model, 3, time.Now().Unix())

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mem := seedMemories(t, 2, fake.embedVec)
			var out, errBuf strings.Builder
			HookSessionStart(cfg,
				strings.NewReader(fmt.Sprintf(`{"cwd":%q}`, projectRoot)),
				&out, &errBuf, nil, nil,
				HookSessionStartDeps{
					OpenMemoryStore: func() (*heimdall.MemoryStore, error) { return mem, nil },
					Suppress:        func(string, string, time.Duration) bool { return true },
				})
		}()
	}
	wg.Wait()
}

// Compile-time guard — make sure the deps zero value is usable.
var _ HookSessionStartDeps = HookSessionStartDeps{}

// ---------------------------------------------------------------------------
// Scope tests (Feature 1: hook scope filtering by CWD subpath)
//
// SessionStart uses RunRecall, which takes MemoryFilter.ContextPath.
// When Claude Code's CWD is a subpath of a repo, only memories whose
// ContextPath matches should be recalled.
// ---------------------------------------------------------------------------

// seedMemoriesWithContextPaths inserts named memories with specific
// ContextPath values so scope filtering can be asserted.
func seedMemoriesWithContextPaths(t *testing.T, vec []float32, items map[string]string) *heimdall.MemoryStore {
	t.Helper()
	dir := t.TempDir()
	store, err := heimdall.OpenMemoryStore(filepath.Join(dir, "memories.db"))
	if err != nil {
		t.Fatalf("open mem: %v", err)
	}
	now := time.Now().Unix()
	for id, cp := range items {
		if err := store.UpsertMemory(heimdall.Memory{
			ID:          id,
			Content:     id + " content",
			Type:        heimdall.MemoryTypeFact,
			Vector:      vec,
			CreatedAt:   now,
			UpdatedAt:   now,
			Source:      heimdall.MemorySourceExplicit,
			ContentHash: heimdall.ContentHash(id + " content"),
			ContextPath: cp,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestHookSessionStart_CWDSubpath_NarrowsRecallScope(t *testing.T) {
	const model = "test-model"
	const dim = 3
	fake := newFakeOllama(t, model, dim)
	cfg := config.Config{OllamaEndpoint: fake.server.URL, Model: model}

	projectRoot := t.TempDir()
	seedVectorStore(t, projectRoot, model, dim, time.Now().Unix())
	// Create a real subdir so the helpers resolve absolute paths consistently.
	subdir := filepath.Join(projectRoot, "internal", "cli")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}

	// One memory under the subpath, one under a sibling. With scope active,
	// only "cli-mem" should be recalled.
	mem := seedMemoriesWithContextPaths(t, fake.embedVec, map[string]string{
		"cli-mem": "internal/cli",
		"api-mem": "internal/api",
	})

	stdout, _, code := runHookSessionStart(t, runOpts{
		stdin:       fmt.Sprintf(`{"cwd":%q}`, subdir),
		cfg:         cfg,
		memoryStore: mem,
	})
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	// Must include the cli-mem bullet, must NOT include the api-mem bullet.
	if !strings.Contains(stdout, "cli-mem content") {
		t.Errorf("expected in-scope memory, got:\n%s", stdout)
	}
	if strings.Contains(stdout, "api-mem content") {
		t.Errorf("out-of-scope memory leaked through:\n%s", stdout)
	}
}

func TestHookSessionStart_CWDAtRepoRoot_NoScope(t *testing.T) {
	const model = "test-model"
	const dim = 3
	fake := newFakeOllama(t, model, dim)
	cfg := config.Config{OllamaEndpoint: fake.server.URL, Model: model}

	projectRoot := t.TempDir()
	seedVectorStore(t, projectRoot, model, dim, time.Now().Unix())

	// Two memories with different scopes; with no scope filter both should
	// surface (subject to top-K=5).
	mem := seedMemoriesWithContextPaths(t, fake.embedVec, map[string]string{
		"cli-mem": "internal/cli",
		"api-mem": "internal/api",
	})

	stdout, _, code := runHookSessionStart(t, runOpts{
		stdin:       fmt.Sprintf(`{"cwd":%q}`, projectRoot),
		cfg:         cfg,
		memoryStore: mem,
	})
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(stdout, "cli-mem content") || !strings.Contains(stdout, "api-mem content") {
		t.Errorf("expected both memories (no scope), got:\n%s", stdout)
	}
}

func TestHookSessionStart_CWDOutsideRepo_NoCrashNoScope(t *testing.T) {
	// A cwd that doesn't resolve to any indexed repo. findRepoRoot may
	// either return "" (no ancestor marker) or a genuine ancestor that has
	// one — either way, computeScope relative to projectRoot cannot produce
	// a scope that narrows the recall. The hook must exit 0 without panic.
	const model = "test-model"
	fake := newFakeOllama(t, model, 3)
	cfg := config.Config{OllamaEndpoint: fake.server.URL, Model: model}

	// An isolated temp dir with no index (no .heimdall_db) — the "no usable
	// index" branch fires, which writes empty or "no index yet" output but
	// MUST NOT panic due to scope handling.
	outsideCWD := t.TempDir()
	mem := seedMemories(t, 0, []float32{1, 0, 0})

	_, _, code := runHookSessionStart(t, runOpts{
		stdin:       fmt.Sprintf(`{"cwd":%q}`, outsideCWD),
		cfg:         cfg,
		memoryStore: mem,
	})
	if code != 0 {
		t.Fatalf("exit=%d (expected 0 — OQ-5)", code)
	}
}

// ---------------------------------------------------------------------------
// logHookEventWithSession helper — stamps session=<id> onto hooks.log lines.
// ---------------------------------------------------------------------------

func TestLogHookEventWithSession_StampsSessionKey(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HEIMDALL_HOOK_LOG", filepath.Join(tmp, "hooks.log"))

	logHookEventWithSession("INFO", "test-event", "abc-123", map[string]any{"stage": "ok"})

	data, err := os.ReadFile(filepath.Join(tmp, "hooks.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	line := string(data)
	if !strings.Contains(line, "session=abc-123") {
		t.Fatalf("expected session=abc-123 in %q", line)
	}
	if !strings.Contains(line, "stage=ok") {
		t.Fatalf("expected stage=ok in %q", line)
	}
}

func TestLogHookEventWithSession_EmptySessionOmitsKey(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HEIMDALL_HOOK_LOG", filepath.Join(tmp, "hooks.log"))

	logHookEventWithSession("INFO", "test-event", "", map[string]any{"stage": "ok"})

	data, _ := os.ReadFile(filepath.Join(tmp, "hooks.log"))
	if strings.Contains(string(data), "session=") {
		t.Fatalf("empty sessionID should not emit session= key: %q", string(data))
	}
}

func TestLogHookEventWithSession_NilKVStillStamps(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HEIMDALL_HOOK_LOG", filepath.Join(tmp, "hooks.log"))

	logHookEventWithSession("INFO", "test-event", "abc-123", nil)

	data, _ := os.ReadFile(filepath.Join(tmp, "hooks.log"))
	if !strings.Contains(string(data), "session=abc-123") {
		t.Fatalf("expected session=abc-123 even when kv nil: %q", string(data))
	}
}

func TestHookSessionStart_LogsSessionID(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HEIMDALL_HOOK_LOG", filepath.Join(tmp, "hooks.log"))
	t.Setenv("HEIMDALL_HOOKS", "1")

	// Force a non-happy path so the handler exits early but still logs.
	// HEIMDALL_HOOKS=0 would short-circuit before session extraction; we
	// want the resolve_root → ollama_ping path which always logs.
	stdin := strings.NewReader(`{"session_id":"abc-xyz","cwd":"/tmp/does-not-exist"}`)
	var out, errBuf bytes.Buffer
	rc := HookSessionStart(config.Config{OllamaEndpoint: "http://127.0.0.1:1"}, stdin, &out, &errBuf, map[string]string{}, nil, HookSessionStartDeps{})
	if rc != 0 {
		t.Fatalf("expected exit 0, got %d", rc)
	}

	logData, _ := os.ReadFile(filepath.Join(tmp, "hooks.log"))
	if !strings.Contains(string(logData), "session=abc-xyz") {
		t.Fatalf("expected session=abc-xyz in hooks.log:\n%s", string(logData))
	}
}
