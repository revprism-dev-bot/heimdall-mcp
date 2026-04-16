//go:build integration

// Package cli integration tests (T20, Layer 2).
//
// These tests exercise the hook CLI commands via `os/exec` against a
// real `heimdall-mcp` binary built from source in TestMain. They run
// against a fake Ollama `httptest.Server` — no network and no real
// embeddings.
//
// Run: `go test -tags=integration ./... -count=1`
// Default `go test ./...` MUST NOT include these — they are gated by the
// `integration` build tag.
//
// What this layer catches that unit tests cannot:
//   - flag parsing and stdin buffering across the process boundary
//   - settings.json install → dispatch round-trip
//   - hooks.log line shape from a real dispatch
//   - post-edit actor fork+setsid (layered detachment hard to mock)
//   - Stop buffer append then SessionEnd ingest + cleanup
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// -- build the binary once per test binary -----------------------------------

// builtBinary is populated by integrationBuildBinary once and reused across
// integration tests. Integration tests call builtBinaryPath(t) which
// lazy-builds on first use and caches the path in builtBinary.
var (
	builtBinary     atomic.Value // string path
	builtBinaryErr  atomic.Value // error
	builtBinaryDone atomic.Bool
	builtBinaryGate = make(chan struct{}, 1)
)

// builtBinaryPath returns the path to the heimdall-mcp binary built for
// these tests, building it on first call. Fails the test if the build
// fails.
func builtBinaryPath(t *testing.T) string {
	t.Helper()
	builtBinaryGate <- struct{}{}
	defer func() { <-builtBinaryGate }()

	if p, ok := builtBinary.Load().(string); ok && p != "" {
		return p
	}
	if e, ok := builtBinaryErr.Load().(error); ok && e != nil {
		t.Fatalf("reusing prior build error: %v", e)
	}

	// t.TempDir is not available here; store the binary under os.TempDir
	// so multiple test files in the same package share one build.
	dir, err := os.MkdirTemp("", "heimdall-int-bin-*")
	if err != nil {
		builtBinaryErr.Store(err)
		t.Fatalf("mkdtemp: %v", err)
	}
	binPath := filepath.Join(dir, "heimdall-mcp")
	cmd := exec.Command("go", "build", "-o", binPath, "../../cmd/heimdall-mcp")
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		builtBinaryErr.Store(err)
		t.Fatalf("go build heimdall-mcp failed: %v\n%s", err, out)
	}
	builtBinary.Store(binPath)
	builtBinaryDone.Store(true)
	return binPath
}

// -- fake Ollama -------------------------------------------------------------

// fakeOllamaServer mimics the subset of Ollama that hook commands hit:
// GET /api/tags and POST /api/embed. Deterministic fixed-dimension
// vector for any input so identical prompts always yield identical
// embeddings — the integration tests don't care what the embedding is,
// only that it's consistent across calls so the search backend works.
type fakeOllamaServer struct {
	server *httptest.Server
	model  string
	dim    int
	// requestCount tracks how many embed requests we've served; tests can
	// use it to verify cache-hit paths don't touch Ollama.
	requestCount atomic.Int64
}

func startFakeOllama(t *testing.T, model string, dim int) *fakeOllamaServer {
	t.Helper()
	f := &fakeOllamaServer{model: model, dim: dim}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{{"name": f.model}},
		})
	})
	mux.HandleFunc("/api/embed", func(w http.ResponseWriter, r *http.Request) {
		f.requestCount.Add(1)
		var body struct {
			Input any `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		// Return one vector per input (string or []string).
		var vectors [][]float32
		makeVec := func() []float32 {
			v := make([]float32, f.dim)
			// Deterministic but not zeroed — avoids hitting the
			// "empty vector" guard in the Ollama client.
			for i := range v {
				v[i] = 1.0 / float32(f.dim)
			}
			return v
		}
		switch in := body.Input.(type) {
		case string:
			vectors = [][]float32{makeVec()}
		case []any:
			for range in {
				vectors = append(vectors, makeVec())
			}
		default:
			vectors = [][]float32{makeVec()}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": vectors})
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// -- test scaffolding --------------------------------------------------------

// integrationFixture captures everything a Layer-2 test needs: the built
// binary path, a fake Ollama, a project root with a seeded index, and a
// per-test hooks.log path. Env vars for invoking the binary are prepared
// by fixtureEnv().
type integrationFixture struct {
	t            *testing.T
	binaryPath   string
	fake         *fakeOllamaServer
	projectRoot  string
	configPath   string
	hookLogPath  string
	stateHome    string
	xdgConfig    string
	model        string
	dim          int
}

// newIntegrationFixture assembles a hermetic environment: builds the binary
// if needed, writes a config.json pointing at the fake Ollama, seeds a
// vector store so VerifyHookIndex passes. Returns the fixture and a cleanup
// registered on t.Cleanup.
func newIntegrationFixture(t *testing.T) *integrationFixture {
	t.Helper()
	const model = "test-model"
	const dim = 4

	bin := builtBinaryPath(t)
	fake := startFakeOllama(t, model, dim)

	base := t.TempDir()
	projectRoot := filepath.Join(base, "project")
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}

	// Per-test state dirs — config, state, hooks log. Keeping them inside
	// base ensures cleanup when the test ends.
	stateHome := filepath.Join(base, "state")
	xdgConfig := filepath.Join(base, "config")
	if err := os.MkdirAll(filepath.Join(xdgConfig, "heimdall-mcp"), 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(stateHome, "heimdall"), 0o755); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	configPath := filepath.Join(xdgConfig, "heimdall-mcp", "config.json")
	hookLogPath := filepath.Join(stateHome, "heimdall", "hooks.log")

	// Write config pointing at fake Ollama.
	cfgJSON := map[string]any{
		"ollamaEndpoint": fake.server.URL,
		"model":          model,
	}
	data, _ := json.Marshal(cfgJSON)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Seed the vector store so VerifyHookIndex passes. Identical shape to
	// the unit-test seedVectorStore helper.
	baseDir := filepath.Join(projectRoot, ".heimdall_db")
	dbDir := heimdall.ModelDBDir(baseDir, model)
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatalf("mkdir dbDir: %v", err)
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
	// Seed one file in the index so search has something to return.
	vec := make([]float32, dim)
	for i := range vec {
		vec[i] = 1.0 / float32(dim)
	}
	if err := store.Upsert([]heimdall.VectorRecord{{
		ID:        "seed-1",
		FilePath:  "seed.go",
		StartLine: 1,
		EndLine:   1,
		Content:   "package seed",
		Kind:      "paragraph",
		Embedding: vec,
	}}); err != nil {
		t.Fatalf("upsert seed: %v", err)
	}
	store.Close()

	return &integrationFixture{
		t:           t,
		binaryPath:  bin,
		fake:        fake,
		projectRoot: projectRoot,
		configPath:  configPath,
		hookLogPath: hookLogPath,
		stateHome:   stateHome,
		xdgConfig:   xdgConfig,
		model:       model,
		dim:         dim,
	}
}

// env returns the env slice to pass to exec.Command so heimdall-mcp runs
// fully hermetically. Pins config, state, hook-log location.
func (f *integrationFixture) env() []string {
	env := []string{
		"HEIMDALL_MCP_CONFIG=" + f.configPath,
		"HEIMDALL_HOOK_LOG=" + f.hookLogPath,
		"XDG_STATE_HOME=" + f.stateHome,
		"XDG_CONFIG_HOME=" + f.xdgConfig,
		"HOME=" + filepath.Dir(f.stateHome),
		"PATH=" + os.Getenv("PATH"),
	}
	return env
}

// run invokes the binary with the given args and stdin, returning stdout,
// stderr, and the exit code.
func (f *integrationFixture) run(args []string, stdin string, timeout time.Duration) (stdout, stderr string, code int) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, f.binaryPath, args...)
	cmd.Env = f.env()
	cmd.Dir = f.projectRoot
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	code = 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			f.t.Fatalf("run %v: %v", args, err)
		}
	}
	return outBuf.String(), errBuf.String(), code
}

// readHookLog returns the contents of hooks.log or "" if missing.
func (f *integrationFixture) readHookLog() string {
	b, err := os.ReadFile(f.hookLogPath)
	if err != nil {
		return ""
	}
	return string(b)
}

// -- T20a: session-start end-to-end -----------------------------------------

// TestIntegration_HookSessionStart_EndToEnd fires the `hook session-start`
// subcommand through the real binary and asserts stdout contains the
// `## Heimdall context` markdown block with project/model metadata, plus
// a hooks.log line recording stage=ok.
func TestIntegration_HookSessionStart_EndToEnd(t *testing.T) {
	f := newIntegrationFixture(t)

	stdin := fmt.Sprintf(`{"cwd":%q}`, f.projectRoot)
	stdout, stderr, code := f.run(
		[]string{"hook", "session-start"},
		stdin,
		30*time.Second,
	)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	// Retrieval hook contract: stderr stays empty (§5.9 / OQ-5).
	if stderr != "" {
		t.Errorf("stderr should be empty, got: %q", stderr)
	}
	if !strings.Contains(stdout, "## Heimdall context") {
		t.Errorf("missing banner in stdout:\n%s", stdout)
	}
	if !strings.Contains(stdout, "**Model:** "+f.model) {
		t.Errorf("missing model marker in stdout:\n%s", stdout)
	}
	log := f.readHookLog()
	if !strings.Contains(log, "event=session-start") {
		t.Errorf("expected event=session-start line in hooks.log:\n%s", log)
	}
	if !strings.Contains(log, "stage=ok") {
		t.Errorf("expected stage=ok in hooks.log:\n%s", log)
	}
}

// -- T20b: user-prompt cache-hit --------------------------------------------

// TestIntegration_HookUserPrompt_CacheHit fires user-prompt twice with the
// same prompt + project + index_version. The first fire must
// embed and store a cache row (stage=ok line in log). The second fire
// must hit the cache (stage=cache_hit line) and serve the same stdout.
func TestIntegration_HookUserPrompt_CacheHit(t *testing.T) {
	f := newIntegrationFixture(t)

	prompt := "explain how the hook cache works"
	stdin := fmt.Sprintf(`{"prompt":%q,"cwd":%q}`, prompt, f.projectRoot)

	// First fire — should be a cache miss → embed → store.
	out1, stderr1, code1 := f.run(
		[]string{"hook", "user-prompt"},
		stdin,
		30*time.Second,
	)
	if code1 != 0 {
		t.Fatalf("first run: exit=%d stderr=%q", code1, stderr1)
	}
	if stderr1 != "" {
		t.Errorf("first run stderr: %q", stderr1)
	}
	if !strings.Contains(out1, "## Heimdall context") {
		t.Errorf("first run missing banner:\n%s", out1)
	}

	countAfterFirst := f.fake.requestCount.Load()
	if countAfterFirst == 0 {
		t.Errorf("expected fake Ollama to be called at least once on miss, got 0")
	}

	// Second fire — identical input, must short-circuit on cache.
	out2, stderr2, code2 := f.run(
		[]string{"hook", "user-prompt"},
		stdin,
		30*time.Second,
	)
	if code2 != 0 {
		t.Fatalf("second run: exit=%d stderr=%q", code2, stderr2)
	}
	if stderr2 != "" {
		t.Errorf("second run stderr: %q", stderr2)
	}
	if out1 != out2 {
		t.Errorf("cache hit should return identical stdout\nfirst:\n%s\nsecond:\n%s", out1, out2)
	}

	// Second fire should NOT have called /api/embed on the Ollama path —
	// only /api/tags (ping) and the ResolveUsableModelDB list-models
	// probe. requestCount only increments for /api/embed, so the count
	// after the second run must equal the count after the first.
	countAfterSecond := f.fake.requestCount.Load()
	if countAfterSecond != countAfterFirst {
		t.Errorf("cache hit path still hit /api/embed: before=%d after=%d",
			countAfterFirst, countAfterSecond)
	}

	log := f.readHookLog()
	if !strings.Contains(log, "stage=cache_hit") {
		t.Errorf("expected stage=cache_hit in hooks.log:\n%s", log)
	}
}

// -- T20c: post-edit actor --------------------------------------------------

// TestIntegration_HookPostEdit_ActorReindexes fires the post-edit hook
// with a Claude Code PostToolUse event JSON. The foreground path should
// enqueue the edited file and fork+setsid the actor. We then poll
// hooks.log for the actor's `reindex_ok` message, which confirms the
// actor fully executed IndexIncremental against the fake Ollama.
func TestIntegration_HookPostEdit_ActorReindexes(t *testing.T) {
	f := newIntegrationFixture(t)

	// Write a real file so IndexIncremental has something to walk.
	edited := filepath.Join(f.projectRoot, "example.go")
	if err := os.WriteFile(edited, []byte("package example\n\nfunc Hello() string { return \"hi\" }\n"), 0o644); err != nil {
		t.Fatalf("write example.go: %v", err)
	}

	// Claude Code PostToolUse payload shape.
	payload := map[string]any{
		"tool_name": "Edit",
		"tool_input": map[string]any{
			"file_path": edited,
		},
	}
	stdinBytes, _ := json.Marshal(payload)

	_, stderr, code := f.run(
		[]string{"hook", "post-edit", "--project=" + f.projectRoot},
		string(stdinBytes),
		30*time.Second,
	)
	if code != 0 {
		t.Fatalf("post-edit foreground exit=%d stderr=%q", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr should be empty: %q", stderr)
	}

	// Poll the log for actor completion. Budget is generous — the fake
	// Ollama is in-process, so IndexIncremental should complete quickly
	// on a two-file project, but CI can be slow.
	deadline := time.Now().Add(20 * time.Second)
	var seen string
	for time.Now().Before(deadline) {
		seen = f.readHookLog()
		if strings.Contains(seen, "msg=reindex_ok") ||
			strings.Contains(seen, "reindex_ok") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(seen, "actor_spawned") {
		t.Errorf("expected actor_spawned line in hooks.log:\n%s", seen)
	}
	if !strings.Contains(seen, "reindex_ok") {
		t.Errorf("expected reindex_ok line in hooks.log within deadline:\n%s", seen)
	}
}

// -- T20d: Stop → SessionEnd buffer + cleanup -------------------------------

// TestIntegration_HookStop_ThenSessionEnd fires Stop twice (appending
// rolling-buffer lines), then SessionEnd. After SessionEnd:
//   - the sessions/<session_id>.jsonl buffer must be removed
//   - hooks.log must contain a `event=session-end` line
//
// We do not require a specific ingest result — the ingest path only runs
// if the transcript path exists and contains valid JSONL. The key
// assertion is the buffer lifecycle.
func TestIntegration_HookStop_ThenSessionEnd(t *testing.T) {
	f := newIntegrationFixture(t)

	sessionID := "int-session-xyz"
	// Two Stop fires with different assistant messages.
	for i, msg := range []string{"first assistant turn", "second assistant turn"} {
		payload := map[string]any{
			"session_id":             sessionID,
			"cwd":                    f.projectRoot,
			"last_assistant_message": msg,
			"stop_hook_active":       true,
			"hook_event_name":        "Stop",
		}
		data, _ := json.Marshal(payload)
		_, stderr, code := f.run(
			[]string{"hook", "stop"},
			string(data),
			10*time.Second,
		)
		if code != 0 {
			t.Fatalf("stop fire %d: exit=%d stderr=%q", i, code, stderr)
		}
	}

	bufPath := filepath.Join(f.projectRoot, ".heimdall_db", "hooks", "sessions", sessionID+".jsonl")
	buf, err := os.ReadFile(bufPath)
	if err != nil {
		t.Fatalf("buffer not created: %v", err)
	}
	lines := strings.Count(strings.TrimSpace(string(buf)), "\n") + 1
	if lines < 2 {
		t.Errorf("expected at least 2 buffer lines, got %d\n%s", lines, buf)
	}

	// Write a minimal transcript JSONL so the SessionEnd ingest path has
	// something to read (the happy ingest path is not what we assert
	// here; we just want to make sure the handler doesn't crash on a
	// real transcript file).
	transcriptPath := filepath.Join(f.projectRoot, "transcript.jsonl")
	transcriptLines := []string{
		`{"role":"assistant","content":"session summary: implemented feature X"}`,
	}
	if err := os.WriteFile(transcriptPath, []byte(strings.Join(transcriptLines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	endPayload := map[string]any{
		"session_id":      sessionID,
		"transcript_path": transcriptPath,
		"cwd":             f.projectRoot,
		"hook_event_name": "SessionEnd",
		"reason":          "clear",
	}
	endData, _ := json.Marshal(endPayload)

	_, stderr, code := f.run(
		[]string{"hook", "session-end"},
		string(endData),
		30*time.Second,
	)
	if code != 0 {
		t.Fatalf("session-end exit=%d stderr=%q", code, stderr)
	}

	if _, err := os.Stat(bufPath); !os.IsNotExist(err) {
		t.Errorf("session-end should remove buffer file; stat err=%v", err)
	}

	log := f.readHookLog()
	if !strings.Contains(log, "event=session-end") {
		t.Errorf("expected event=session-end line in hooks.log:\n%s", log)
	}
}
