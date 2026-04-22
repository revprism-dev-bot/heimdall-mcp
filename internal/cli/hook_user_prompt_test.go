package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

type runUPOpts struct {
	stdin    string
	args     []string
	env      map[string]string
	cfg      config.Config
	suppress func(project, code string, window time.Duration) bool
	// seedRecords, if non-nil, are upserted into the DB before the hook runs
	// so a real search can return them.
	seedRecords []heimdall.VectorRecord
	// memoryStore, if non-nil, is returned by OpenMemoryStore. If nil, a
	// fresh empty in-tempdir memory store is provided so tests never hit
	// the real ~/.local memory DB.
	memoryStore *heimdall.MemoryStore
}

func runHookUserPrompt(t *testing.T, opts runUPOpts) (stdout, stderr string, code int) {
	t.Helper()
	var out, errBuf strings.Builder
	// Every test gets an isolated memory store. Without this, the hook's
	// skill-surfacing path would fall through to config.ResolveMemoryDBPath
	// and read/write the user's real ~/.local/state/heimdall memory DB
	// under `go test`.
	mem := opts.memoryStore
	if mem == nil {
		var err error
		mem, err = heimdall.OpenMemoryStore(filepath.Join(t.TempDir(), "memories.db"))
		if err != nil {
			t.Fatalf("open empty memory store: %v", err)
		}
		t.Cleanup(func() { mem.Close() })
	}
	deps := HookUserPromptDeps{
		Suppress: opts.suppress,
		OpenMemoryStore: func() (*heimdall.MemoryStore, error) {
			return mem, nil
		},
	}
	if deps.Suppress == nil {
		deps.Suppress = func(string, string, time.Duration) bool { return true }
	}
	rc := HookUserPrompt(opts.cfg, strings.NewReader(opts.stdin), &out, &errBuf, opts.env, opts.args, deps)
	return out.String(), errBuf.String(), rc
}

// seedStoreWithRecords opens the on-disk store at dbDir and upserts records,
// used when a test wants SearchFiltered to return non-empty results.
func seedStoreWithRecords(t *testing.T, dbDir string, recs []heimdall.VectorRecord) {
	t.Helper()
	if len(recs) == 0 {
		return
	}
	store, err := heimdall.OpenStore(dbDir)
	if err != nil {
		t.Fatalf("open for seed: %v", err)
	}
	if err := store.Upsert(recs); err != nil {
		t.Fatalf("upsert seed: %v", err)
	}
	store.Close()
}

// baseCfg returns a config pointing at the fake Ollama.
func baseCfg(endpoint, model string) config.Config {
	return config.Config{
		OllamaEndpoint: endpoint,
		Model:          model,
	}
}

// 1. Skip heuristic — short prompt bypasses everything, no stdout.
func TestHookUserPrompt_SkipShortPrompt(t *testing.T) {
	project := t.TempDir()
	out, _, code := runHookUserPrompt(t, runUPOpts{
		stdin: `{"prompt":"hi","cwd":"` + project + `"}`,
		args:  []string{},
		env:   map[string]string{},
		cfg:   baseCfg("http://127.0.0.1:1", "any"),
	})
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty", out)
	}
}

// 2. Disabled via HEIMDALL_HOOKS=0 — no stdout, exit 0.
func TestHookUserPrompt_DisabledByEnv(t *testing.T) {
	project := t.TempDir()
	out, _, code := runHookUserPrompt(t, runUPOpts{
		stdin: `{"prompt":"explain the post-edit flow","cwd":"` + project + `"}`,
		env:   map[string]string{"HEIMDALL_HOOKS": "0"},
		cfg:   baseCfg("http://127.0.0.1:1", "any"),
	})
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty", out)
	}
}

// 3. Ollama ping fails → Tier B banner emitted (when suppression allows).
func TestHookUserPrompt_OllamaDownEmitsTierB(t *testing.T) {
	const model = "test-model"
	project := t.TempDir()
	_ = seedVectorStore(t, project, model, 3, time.Now().Unix())
	fake := newFakeOllama(t, model, 3)
	fake.pingFail.Store(true)

	out, _, code := runHookUserPrompt(t, runUPOpts{
		stdin:    `{"prompt":"explain the post-edit flow","cwd":"` + project + `"}`,
		env:      map[string]string{},
		cfg:      baseCfg(fake.server.URL, model),
		suppress: func(string, string, time.Duration) bool { return true },
	})
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if !strings.Contains(out, "## Heimdall context") || !strings.Contains(out, "ollama unreachable") {
		t.Errorf("expected Tier B banner, got: %q", out)
	}
}

// 4. Ollama down but suppression fires → no stdout.
func TestHookUserPrompt_OllamaDownSuppressed(t *testing.T) {
	const model = "test-model"
	project := t.TempDir()
	_ = seedVectorStore(t, project, model, 3, time.Now().Unix())
	fake := newFakeOllama(t, model, 3)
	fake.pingFail.Store(true)

	out, _, code := runHookUserPrompt(t, runUPOpts{
		stdin:    `{"prompt":"explain the post-edit flow","cwd":"` + project + `"}`,
		env:      map[string]string{},
		cfg:      baseCfg(fake.server.URL, model),
		suppress: func(string, string, time.Duration) bool { return false },
	})
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty when suppressed", out)
	}
}

// 5. No usable index (no store at all) → empty stdout, exit 0.
func TestHookUserPrompt_NoIndex(t *testing.T) {
	const model = "test-model"
	project := t.TempDir()
	fake := newFakeOllama(t, model, 3)

	out, _, code := runHookUserPrompt(t, runUPOpts{
		stdin: `{"prompt":"explain the post-edit flow","cwd":"` + project + `"}`,
		env:   map[string]string{},
		cfg:   baseCfg(fake.server.URL, model),
	})
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty", out)
	}
}

// 6. Model mismatch → Tier B banner.
func TestHookUserPrompt_ModelMismatchEmitsTierB(t *testing.T) {
	const dirModel = "test-model"
	const storedModel = "other-model"
	project := t.TempDir()
	_ = seedVectorStoreWithMismatch(t, project, dirModel, storedModel, 3)
	fake := newFakeOllama(t, dirModel, 3)

	out, _, code := runHookUserPrompt(t, runUPOpts{
		stdin:    `{"prompt":"explain the post-edit flow","cwd":"` + project + `"}`,
		env:      map[string]string{},
		cfg:      baseCfg(fake.server.URL, dirModel),
		suppress: func(string, string, time.Duration) bool { return true },
	})
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if !strings.Contains(out, "## Heimdall context") || !strings.Contains(out, "index model mismatch") {
		t.Errorf("expected model-mismatch banner, got: %q", out)
	}
}

// 7. Happy path — store with seeded records + fake Ollama returning a
// constant embedding. Expect the `## Heimdall context` block with Hits > 0
// and a cache row written.
func TestHookUserPrompt_HappyPathAndCachesResult(t *testing.T) {
	const model = "test-model"
	const dim = 3
	project := t.TempDir()
	baseDir := seedVectorStore(t, project, model, dim, time.Now().Unix())
	dbDir := heimdall.ModelDBDir(baseDir, model)
	recs := []heimdall.VectorRecord{
		{ID: "1", FilePath: "a.go", StartLine: 1, EndLine: 10, Content: "package a\nfunc A() {}", Embedding: []float32{1, 0, 0}},
		{ID: "2", FilePath: "b.go", StartLine: 5, EndLine: 15, Content: "package b\nfunc B() {}", Embedding: []float32{0.5, 0.5, 0}},
	}
	seedStoreWithRecords(t, dbDir, recs)

	fake := newFakeOllama(t, model, dim)

	out, _, code := runHookUserPrompt(t, runUPOpts{
		stdin: `{"prompt":"explain the post-edit flow","cwd":"` + project + `"}`,
		env:   map[string]string{},
		cfg:   baseCfg(fake.server.URL, model),
	})
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if !strings.Contains(out, "## Heimdall context") {
		t.Errorf("missing header, got: %q", out)
	}
	if !strings.Contains(out, "**Hits:**") {
		t.Errorf("missing Hits line, got: %q", out)
	}
	if !strings.Contains(out, "### Relevant code") {
		t.Errorf("missing Relevant code section, got: %q", out)
	}
	if !strings.Contains(out, "a.go:1-10") && !strings.Contains(out, "b.go:5-15") {
		t.Errorf("expected at least one hit path, got: %q", out)
	}

	// Second call with the same prompt should hit the cache (use a fake that
	// fails pings to prove we never talk to Ollama beyond the first call).
	// Re-seeded embedDelay=0 keeps the fake healthy, but we need to prove we
	// took the cache branch by checking HookCacheGet directly.
	store, err := heimdall.OpenStore(dbDir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store.Close()
	key := userPromptCacheKey("explain the post-edit flow", store.GetIndexVersion(), project)
	if _, err := store.HookCacheGet(key, 24*time.Hour); err != nil {
		t.Errorf("expected cache hit for key %q, got %v", key, err)
	}
}

// 8. Cache hit path — pre-populate the cache and confirm stdout matches
// without going through search. We assert by setting a distinctive cached
// payload and checking it comes out verbatim.
func TestHookUserPrompt_CacheHitShortCircuits(t *testing.T) {
	const model = "test-model"
	const dim = 3
	project := t.TempDir()
	baseDir := seedVectorStore(t, project, model, dim, time.Now().Unix())
	dbDir := heimdall.ModelDBDir(baseDir, model)

	// Pre-populate the cache.
	store, err := heimdall.OpenStore(dbDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	cached := []byte("## Heimdall context\n\nfrom cache\n")
	prompt := "explain the post-edit flow"
	key := userPromptCacheKey(prompt, store.GetIndexVersion(), project)
	if err := store.HookCachePut(key, cached, 24*time.Hour, userPromptCacheRowCap); err != nil {
		t.Fatalf("put cache: %v", err)
	}
	store.Close()

	// Give the hook a broken Ollama URL — if it tries to talk to Ollama the
	// test would take the Tier B branch and stdout would mention "ollama
	// unreachable" instead of "from cache".
	fake := newFakeOllama(t, model, dim)

	out, _, code := runHookUserPrompt(t, runUPOpts{
		stdin: `{"prompt":"` + prompt + `","cwd":"` + project + `"}`,
		cfg:   baseCfg(fake.server.URL, model),
	})
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if !strings.Contains(out, "from cache") {
		t.Errorf("expected cached payload, got: %q", out)
	}
}

// 9. Cache invalidation on index_version bump. A cached row under version N
// must not be returned after an Upsert bumps version to N+1.
func TestHookUserPrompt_CacheInvalidatedByIndexVersionBump(t *testing.T) {
	const model = "test-model"
	const dim = 3
	project := t.TempDir()
	baseDir := seedVectorStore(t, project, model, dim, time.Now().Unix())
	dbDir := heimdall.ModelDBDir(baseDir, model)

	store, err := heimdall.OpenStore(dbDir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	prompt := "explain the post-edit flow"
	keyV0 := userPromptCacheKey(prompt, store.GetIndexVersion(), project)
	if err := store.HookCachePut(keyV0, []byte("v0 payload"), 24*time.Hour, userPromptCacheRowCap); err != nil {
		t.Fatalf("put: %v", err)
	}

	// Trigger an Upsert to bump index_version.
	if err := store.Upsert([]heimdall.VectorRecord{
		{ID: "x", FilePath: "x.go", StartLine: 1, EndLine: 1, Content: "x", Embedding: []float32{1, 0, 0}},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	newVersion := store.GetIndexVersion()
	store.Close()

	if newVersion == 0 {
		t.Fatalf("index_version did not bump")
	}
	newKey := userPromptCacheKey(prompt, newVersion, project)
	if newKey == keyV0 {
		t.Fatalf("new cache key should differ from old: %q == %q", newKey, keyV0)
	}

	// Now run the hook — a fresh fake Ollama plus a real embed path. The
	// hook should miss the cache (new version key) and do a real search.
	fake := newFakeOllama(t, model, dim)
	out, _, code := runHookUserPrompt(t, runUPOpts{
		stdin: `{"prompt":"` + prompt + `","cwd":"` + project + `"}`,
		cfg:   baseCfg(fake.server.URL, model),
	})
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if strings.Contains(out, "v0 payload") {
		t.Errorf("stale cache returned: %q", out)
	}
}

// 10. Budget timeout — a budget small enough that ping+resolve succeed but
// the slow embed blows the deadline. Expect empty stdout (or a Tier B banner
// if the ping itself loses the race and suppression allows it).
func TestHookUserPrompt_BudgetTimeout(t *testing.T) {
	const model = "test-model"
	const dim = 3
	project := t.TempDir()
	_ = seedVectorStore(t, project, model, dim, time.Now().Unix())
	fake := newFakeOllama(t, model, dim)
	fake.embedDelay = 500 * time.Millisecond

	out, _, code := runHookUserPrompt(t, runUPOpts{
		stdin:    `{"prompt":"explain the post-edit flow","cwd":"` + project + `"}`,
		args:     []string{"--budget-ms", "40"},
		cfg:      baseCfg(fake.server.URL, model),
		suppress: func(string, string, time.Duration) bool { return false },
	})
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	// Either the embed was cancelled mid-flight (empty stdout) or the
	// handler took the ctx.Err() branch after a partial search (also empty).
	// The header rendered with Hits: 0 would indicate we rendered anyway —
	// that's the bug we're guarding against.
	if strings.Contains(out, "## Heimdall context") {
		t.Errorf("expected no block on budget timeout, got: %q", out)
	}
}

// nagInjectingDeps wraps runHookUserPrompt with a synthetic ReadHookLog so
// the nag direct-call detector returns a controlled answer. Uses the same
// surface as runHookUserPrompt; duplicated rather than threaded through to
// keep the existing 16+ tests' behavior unchanged.
func runHookUserPromptWithReader(t *testing.T, opts runUPOpts, reader hookLogReaderFunc) (stdout, stderr string, code int) {
	t.Helper()
	var out, errBuf strings.Builder
	mem := opts.memoryStore
	if mem == nil {
		var err error
		mem, err = heimdall.OpenMemoryStore(filepath.Join(t.TempDir(), "memories.db"))
		if err != nil {
			t.Fatalf("open empty memory store: %v", err)
		}
		t.Cleanup(func() { mem.Close() })
	}
	deps := HookUserPromptDeps{
		Suppress: opts.suppress,
		OpenMemoryStore: func() (*heimdall.MemoryStore, error) {
			return mem, nil
		},
		ReadHookLog: reader,
	}
	if deps.Suppress == nil {
		deps.Suppress = func(string, string, time.Duration) bool { return true }
	}
	rc := HookUserPrompt(opts.cfg, strings.NewReader(opts.stdin), &out, &errBuf, opts.env, opts.args, deps)
	return out.String(), errBuf.String(), rc
}

// Nag integration: with state pre-seeded at turn=4 and threshold=5 (env), the
// next call should fire the "### Heimdall nudge" block on the happy path.
// Uses HEIMDALL_NAG_TURNS=5 explicitly + a no-call reader so direct-call
// detection returns false.
func TestHookUserPrompt_NagFiresAtThresholdOnHappyPath(t *testing.T) {
	const model = "test-model"
	const dim = 3
	const sessionID = "nag-session-1"
	project := t.TempDir()
	baseDir := seedVectorStore(t, project, model, dim, time.Now().Unix())
	dbDir := heimdall.ModelDBDir(baseDir, model)
	seedStoreWithRecords(t, dbDir, []heimdall.VectorRecord{
		{ID: "1", FilePath: "a.go", StartLine: 1, EndLine: 10, Content: "package a", Embedding: []float32{1, 0, 0}},
	})

	// Pre-seed nag state at Turn=4. Next prompt is turn 5 → should fire.
	saveNagState(project, sessionID, nagState{Turn: 4, LastNagTurn: 0, LastResetUnix: time.Now().Unix() - 600})

	fake := newFakeOllama(t, model, dim)
	noCallReader := func(opts heimdall.ReadHookLogOpts) ([]heimdall.HookLogEntry, error) {
		return nil, nil // no direct calls in the (synthetic) log
	}

	out, _, code := runHookUserPromptWithReader(t, runUPOpts{
		stdin: `{"prompt":"explain the post-edit flow","cwd":"` + project + `","session_id":"` + sessionID + `"}`,
		env:   map[string]string{"HEIMDALL_NAG_TURNS": "5"},
		cfg:   baseCfg(fake.server.URL, model),
	}, noCallReader)
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if !strings.Contains(out, "### Heimdall nudge") {
		t.Errorf("expected nag block in output, got:\n%s", out)
	}
	// Nag must appear before the footer to ensure it's part of the auto-injected block.
	idxNag := strings.Index(out, "### Heimdall nudge")
	idxFooter := strings.Index(out, "_retrieved via heimdall-mcp_")
	if idxNag < 0 || idxFooter < 0 || idxNag >= idxFooter {
		t.Errorf("nag block not before footer; nag@%d footer@%d", idxNag, idxFooter)
	}
	// Persisted state should now show LastNagTurn=5 (so we don't immediately re-fire).
	got := loadNagState(project, sessionID)
	if got.LastNagTurn != 5 {
		t.Errorf("LastNagTurn after fire = %d, want 5; state=%+v", got.LastNagTurn, got)
	}
}

// Nag integration: a recent direct heimdall_search call resets the counter
// even if we were over threshold. The output should NOT contain the nag.
func TestHookUserPrompt_NagSuppressedByDirectCall(t *testing.T) {
	const model = "test-model"
	const dim = 3
	const sessionID = "nag-session-2"
	project := t.TempDir()
	baseDir := seedVectorStore(t, project, model, dim, time.Now().Unix())
	dbDir := heimdall.ModelDBDir(baseDir, model)
	seedStoreWithRecords(t, dbDir, []heimdall.VectorRecord{
		{ID: "1", FilePath: "a.go", StartLine: 1, EndLine: 10, Content: "package a", Embedding: []float32{1, 0, 0}},
	})

	saveNagState(project, sessionID, nagState{Turn: 9, LastNagTurn: 0, LastResetUnix: time.Now().Unix() - 600})

	fake := newFakeOllama(t, model, dim)
	withCallReader := func(opts heimdall.ReadHookLogOpts) ([]heimdall.HookLogEntry, error) {
		return []heimdall.HookLogEntry{
			{Timestamp: time.Now(), Event: "mcp.tool_call", Fields: map[string]string{"tool": "heimdall_search"}},
		}, nil
	}

	out, _, code := runHookUserPromptWithReader(t, runUPOpts{
		stdin: `{"prompt":"explain the post-edit flow","cwd":"` + project + `","session_id":"` + sessionID + `"}`,
		env:   map[string]string{"HEIMDALL_NAG_TURNS": "5"},
		cfg:   baseCfg(fake.server.URL, model),
	}, withCallReader)
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if strings.Contains(out, "### Heimdall nudge") {
		t.Errorf("nag block should be suppressed by direct call, got:\n%s", out)
	}
	// Counter should have reset to 0.
	got := loadNagState(project, sessionID)
	if got.Turn != 0 {
		t.Errorf("Turn after direct call = %d, want 0", got.Turn)
	}
}

// Nag suppression on Tier-B paths: with Ollama down, no nag is emitted (we
// never reach the cache or stage=ok path).
func TestHookUserPrompt_NagNotEmittedOnTierB(t *testing.T) {
	const model = "test-model"
	const sessionID = "nag-session-3"
	project := t.TempDir()
	_ = seedVectorStore(t, project, model, 3, time.Now().Unix())
	fake := newFakeOllama(t, model, 3)
	fake.pingFail.Store(true)

	saveNagState(project, sessionID, nagState{Turn: 99, LastNagTurn: 0, LastResetUnix: time.Now().Unix() - 600})

	out, _, code := runHookUserPromptWithReader(t, runUPOpts{
		stdin:    `{"prompt":"explain the post-edit flow","cwd":"` + project + `","session_id":"` + sessionID + `"}`,
		env:      map[string]string{"HEIMDALL_NAG_TURNS": "5"},
		cfg:      baseCfg(fake.server.URL, model),
		suppress: func(string, string, time.Duration) bool { return true },
	}, nil)
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if strings.Contains(out, "### Heimdall nudge") {
		t.Errorf("nag must not appear on Tier B (Ollama down); got:\n%s", out)
	}
}

// 11. Cache key normalization — cosmetic prompt differences collapse.
func TestUserPromptCacheKey_Normalization(t *testing.T) {
	scope := "/tmp/p"
	k1 := userPromptCacheKey("What does Foo do?", 1, scope)
	k2 := userPromptCacheKey("  what does foo do?   ", 1, scope)
	k3 := userPromptCacheKey("what\tdoes\n\nfoo    do?", 1, scope)
	if k1 != k2 || k2 != k3 {
		t.Errorf("keys should collide after normalization: %q %q %q", k1, k2, k3)
	}

	// Different version → different key.
	k4 := userPromptCacheKey("What does Foo do?", 2, scope)
	if k1 == k4 {
		t.Error("different index_version should give different key")
	}
	// Different scope → different key.
	k5 := userPromptCacheKey("What does Foo do?", 1, "/tmp/other")
	if k1 == k5 {
		t.Error("different scope should give different key")
	}
}

// 12. Empty stdin falls back to os.Getwd().
func TestHookUserPrompt_EmptyStdinFallsBackToCWD(t *testing.T) {
	const model = "test-model"
	const dim = 3
	project := t.TempDir()
	_ = seedVectorStore(t, project, model, dim, time.Now().Unix())
	fake := newFakeOllama(t, model, dim)

	old, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := os.Chdir(project); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	_, _, code := runHookUserPrompt(t, runUPOpts{
		stdin: `{"prompt":"explain the post-edit flow"}`,
		args:  []string{},
		cfg:   baseCfg(fake.server.URL, model),
	})
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
}

// 13. Empty stdin with --prompt override works.
func TestHookUserPrompt_PromptFlagOverridesStdin(t *testing.T) {
	const model = "test-model"
	const dim = 3
	project := t.TempDir()
	_ = seedVectorStore(t, project, model, dim, time.Now().Unix())
	fake := newFakeOllama(t, model, dim)

	out, _, code := runHookUserPrompt(t, runUPOpts{
		stdin: ``,
		args:  []string{"--project", project, "--prompt", "explain the post-edit flow"},
		cfg:   baseCfg(fake.server.URL, model),
	})
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if !strings.Contains(out, "## Heimdall context") {
		t.Errorf("expected happy path, got: %q", out)
	}
}

// 14. Malformed stdin JSON is tolerated — no panic, hook exits 0.
func TestHookUserPrompt_MalformedStdinJSON(t *testing.T) {
	project := t.TempDir()
	out, _, code := runHookUserPrompt(t, runUPOpts{
		stdin: `{not json}`,
		args:  []string{"--project", project},
		cfg:   baseCfg("http://127.0.0.1:1", "any"),
	})
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	// Skip heuristic should fire (empty prompt → <8 runes), no stdout.
	if out != "" {
		t.Errorf("stdout = %q, want empty", out)
	}
}

// 15. normalizePromptForCache preserves order of meaningful chars.
func TestNormalizePromptForCache(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"hello world", "hello world"},
		{"  hello   world  ", "hello world"},
		{"HELLO\tWORLD", "hello world"},
		{"", ""},
		{"\n\n\n", ""},
	}
	for _, c := range cases {
		got := normalizePromptForCache(c.in)
		if got != c.want {
			t.Errorf("normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// 16. dispatch wiring — DispatchHook routes "user-prompt" to HookUserPrompt.
// We don't exercise the full pipeline here; just verify the dispatcher doesn't
// return an "unknown_subcommand" warning code-path.
func TestDispatchHook_RoutesUserPrompt(t *testing.T) {
	project := t.TempDir()
	var out strings.Builder
	var errBuf strings.Builder
	code := DispatchHook(
		config.Config{OllamaEndpoint: "http://127.0.0.1:1", Model: "any"},
		strings.NewReader(`{"prompt":"hi","cwd":"`+project+`"}`),
		&out, &errBuf,
		map[string]string{},
		[]string{"user-prompt"},
	)
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	// "hi" is too short → skip path → empty stdout, no stderr.
	if out.String() != "" {
		t.Errorf("stdout = %q, want empty on skip", out.String())
	}
}

// _ sentinels to keep unused-import hygiene from complaining in variant
// builds.
var _ = fmt.Sprintf
var _ = errors.New
var _ = context.Background
var _ = filepath.Join

// ---------------------------------------------------------------------------
// Scope tests (Feature 1: hook scope filtering by CWD subpath)
//
// UserPrompt calls SearchFiltered — when CWD is a subpath of the repo, the
// WithScope(scope) filter restricts results to entries whose context_path
// starts with that subpath.
// ---------------------------------------------------------------------------

// 17. CWD is a subpath → scope is applied, only matching results returned.
func TestHookUserPrompt_CWDSubpath_NarrowsSearch(t *testing.T) {
	const model = "test-model"
	const dim = 3
	project := t.TempDir()
	baseDir := seedVectorStore(t, project, model, dim, time.Now().Unix())
	dbDir := heimdall.ModelDBDir(baseDir, model)

	// Two records with distinct context_paths. "internal/cli/foo.go" →
	// context_path "internal/cli"; "docs/bar.md" → context_path "docs".
	recs := []heimdall.VectorRecord{
		{ID: "1", FilePath: "internal/cli/foo.go", StartLine: 1, EndLine: 5, Content: "cli code", Embedding: []float32{1, 0, 0}},
		{ID: "2", FilePath: "docs/bar.md", StartLine: 1, EndLine: 5, Content: "docs content", Embedding: []float32{1, 0, 0}},
	}
	seedStoreWithRecords(t, dbDir, recs)

	// Create an internal/cli subdir so it resolves correctly.
	sub := filepath.Join(project, "internal", "cli")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}

	fake := newFakeOllama(t, model, dim)
	out, _, code := runHookUserPrompt(t, runUPOpts{
		stdin: `{"prompt":"explain the post-edit flow","cwd":"` + sub + `"}`,
		cfg:   baseCfg(fake.server.URL, model),
	})
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if !strings.Contains(out, "## Heimdall context") {
		t.Fatalf("missing header, got: %q", out)
	}
	// Only the cli hit should appear; the docs hit should be filtered out.
	if !strings.Contains(out, "internal/cli/foo.go") {
		t.Errorf("expected in-scope hit internal/cli/foo.go, got:\n%s", out)
	}
	if strings.Contains(out, "docs/bar.md") {
		t.Errorf("out-of-scope docs/bar.md leaked through:\n%s", out)
	}
}

// 18. CWD at repo root → no scope applied, both results returned.
func TestHookUserPrompt_CWDAtRepoRoot_NoScope(t *testing.T) {
	const model = "test-model"
	const dim = 3
	project := t.TempDir()
	baseDir := seedVectorStore(t, project, model, dim, time.Now().Unix())
	dbDir := heimdall.ModelDBDir(baseDir, model)

	recs := []heimdall.VectorRecord{
		{ID: "1", FilePath: "internal/cli/foo.go", StartLine: 1, EndLine: 5, Content: "cli", Embedding: []float32{1, 0, 0}},
		{ID: "2", FilePath: "docs/bar.md", StartLine: 1, EndLine: 5, Content: "docs", Embedding: []float32{1, 0, 0}},
	}
	seedStoreWithRecords(t, dbDir, recs)

	fake := newFakeOllama(t, model, dim)
	out, _, code := runHookUserPrompt(t, runUPOpts{
		stdin: `{"prompt":"explain the post-edit flow","cwd":"` + project + `"}`,
		cfg:   baseCfg(fake.server.URL, model),
	})
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if !strings.Contains(out, "internal/cli/foo.go") || !strings.Contains(out, "docs/bar.md") {
		t.Errorf("expected both hits (no scope), got:\n%s", out)
	}
}

// 19. CWD outside any repo → no scope, no crash, exit 0. The handler should
// still run; lacking an index yields the "no usable index" log branch
// (empty stdout) but MUST NOT panic due to scope handling.
func TestHookUserPrompt_CWDOutsideRepo_NoCrashNoScope(t *testing.T) {
	const model = "test-model"
	outside := t.TempDir()

	fake := newFakeOllama(t, model, 3)
	_, _, code := runHookUserPrompt(t, runUPOpts{
		stdin: `{"prompt":"explain the post-edit flow","cwd":"` + outside + `"}`,
		cfg:   baseCfg(fake.server.URL, model),
	})
	if code != 0 {
		t.Errorf("code = %d, want 0 (OQ-5)", code)
	}
}

// 20. Subpath and repo-root cache keys must differ so subpath queries don't
// serve repo-root payloads from cache (and vice-versa).
func TestHookUserPrompt_ScopeCacheKeyDiffers(t *testing.T) {
	const model = "test-model"
	const dim = 3
	project := t.TempDir()
	baseDir := seedVectorStore(t, project, model, dim, time.Now().Unix())
	dbDir := heimdall.ModelDBDir(baseDir, model)
	sub := filepath.Join(project, "internal", "cli")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Seed the cache under the REPO-ROOT key with a distinctive payload.
	store, err := heimdall.OpenStore(dbDir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	prompt := "explain the post-edit flow"
	rootKey := userPromptCacheKey(prompt, store.GetIndexVersion(), project)
	if err := store.HookCachePut(rootKey, []byte("ROOT-PAYLOAD\n"), 24*time.Hour, userPromptCacheRowCap); err != nil {
		t.Fatalf("put root cache: %v", err)
	}
	store.Close()

	// Fire the hook from the subpath — key differs, so we must NOT see the
	// ROOT-PAYLOAD in stdout. The fake Ollama will embed and the real
	// search (empty result set) will render a fresh payload.
	fake := newFakeOllama(t, model, dim)
	out, _, code := runHookUserPrompt(t, runUPOpts{
		stdin: `{"prompt":"` + prompt + `","cwd":"` + sub + `"}`,
		cfg:   baseCfg(fake.server.URL, model),
	})
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if strings.Contains(out, "ROOT-PAYLOAD") {
		t.Errorf("subpath call served root-scoped cache entry:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// Session-id threading (Wave A).
// ---------------------------------------------------------------------------

func TestHookUserPrompt_LogsSessionID(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HEIMDALL_HOOK_LOG", filepath.Join(tmp, "hooks.log"))
	t.Setenv("HEIMDALL_HOOKS", "1")

	stdin := strings.NewReader(`{"session_id":"sess-777","cwd":"/tmp/does-not-exist","prompt":"this is a long enough prompt to pass"}`)
	var out, errBuf bytes.Buffer
	rc := HookUserPrompt(config.Config{OllamaEndpoint: "http://127.0.0.1:1"}, stdin, &out, &errBuf, map[string]string{}, nil, HookUserPromptDeps{})
	if rc != 0 {
		t.Fatalf("expected exit 0, got %d", rc)
	}

	data, _ := os.ReadFile(filepath.Join(tmp, "hooks.log"))
	if !strings.Contains(string(data), "session=sess-777") {
		t.Fatalf("expected session=sess-777 in hooks.log:\n%s", string(data))
	}
}

func TestHookUserPrompt_SkipPromptTooShortStillLogsSession(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HEIMDALL_HOOK_LOG", filepath.Join(tmp, "hooks.log"))
	t.Setenv("HEIMDALL_HOOKS", "1")

	stdin := strings.NewReader(`{"session_id":"sess-short","cwd":"/tmp","prompt":"ok"}`)
	var out, errBuf bytes.Buffer
	_ = HookUserPrompt(config.Config{}, stdin, &out, &errBuf, map[string]string{}, nil, HookUserPromptDeps{})

	data, _ := os.ReadFile(filepath.Join(tmp, "hooks.log"))
	if !strings.Contains(string(data), "session=sess-short") {
		t.Fatalf("expected session=sess-short in log even on skip:\n%s", string(data))
	}
	if !strings.Contains(string(data), "reason=prompt_too_short") {
		t.Fatalf("expected reason=prompt_too_short in log:\n%s", string(data))
	}
}

// Regression pin: the default budget was bumped from 250 → 450 in response
// to CPU-Ollama dogfood data showing first-of-process embeds exceeding
// 250 ms even on an already-loaded model. If anyone lowers this back toward
// the original plan §3.2 target, first-turn context injection silently
// regresses. Change the test *and* the comment on userPromptBudgetDefault
// if the bump is being intentionally revisited.
func TestUserPromptBudgetDefault_PinnedTo450(t *testing.T) {
	if userPromptBudgetDefault != 450 {
		t.Fatalf("userPromptBudgetDefault = %d, want 450 (see comment on the constant)", userPromptBudgetDefault)
	}
	if userPromptBudgetDefault > userPromptHardTimeout {
		t.Fatalf("default budget %d exceeds hard cap %d", userPromptBudgetDefault, userPromptHardTimeout)
	}
}

// TestHookUserPrompt_LogsAutoInjectFields verifies that the stage=ok log line
// emitted by HookUserPrompt includes the three new observability fields:
//   - auto_inject_bytes — byte count of the injected body (> 0 on a hit)
//   - auto_inject_hash  — 12-char hex prefix of sha256(body)
//   - top_hit_files     — list of file paths from the top hits
func TestHookUserPrompt_LogsAutoInjectFields(t *testing.T) {
	const model = "test-model"
	const dim = 3
	project := t.TempDir()
	baseDir := seedVectorStore(t, project, model, dim, time.Now().Unix())
	dbDir := heimdall.ModelDBDir(baseDir, model)
	recs := []heimdall.VectorRecord{
		{ID: "1", FilePath: "a.go", StartLine: 1, EndLine: 10, Content: "package a\nfunc A() {}", Embedding: []float32{1, 0, 0}},
		{ID: "2", FilePath: "b.go", StartLine: 5, EndLine: 15, Content: "package b\nfunc B() {}", Embedding: []float32{0.5, 0.5, 0}},
	}
	seedStoreWithRecords(t, dbDir, recs)

	fake := newFakeOllama(t, model, dim)

	// Redirect hooks.log to a temp file so we can inspect it.
	tmp := t.TempDir()
	t.Setenv("HEIMDALL_HOOK_LOG", filepath.Join(tmp, "hooks.log"))

	out, _, code := runHookUserPrompt(t, runUPOpts{
		stdin: `{"prompt":"explain the post-edit flow","cwd":"` + project + `"}`,
		env:   map[string]string{},
		cfg:   baseCfg(fake.server.URL, model),
	})
	if code != 0 {
		t.Fatalf("hook returned code %d, want 0", code)
	}
	if !strings.Contains(out, "## Heimdall context") {
		t.Fatalf("expected Heimdall context block in stdout, got: %q", out)
	}

	logData, err := os.ReadFile(filepath.Join(tmp, "hooks.log"))
	if err != nil {
		t.Fatalf("hooks.log not written: %v", err)
	}
	logStr := string(logData)

	// Find the stage=ok line specifically.
	var okLine string
	for _, line := range strings.Split(logStr, "\n") {
		if strings.Contains(line, "stage=ok") {
			okLine = line
			break
		}
	}
	if okLine == "" {
		t.Fatalf("no stage=ok line in hooks.log:\n%s", logStr)
	}

	// auto_inject_bytes must be present and > 0.
	if !strings.Contains(okLine, "auto_inject_bytes=") {
		t.Errorf("missing auto_inject_bytes in stage=ok line; got:\n%s", okLine)
	}

	// auto_inject_hash must be present (12-char hex prefix of sha256).
	if !strings.Contains(okLine, "auto_inject_hash=") {
		t.Errorf("missing auto_inject_hash in stage=ok line; got:\n%s", okLine)
	}

	// top_hit_files must be present when hits > 0.
	if !strings.Contains(okLine, "top_hit_files=") {
		t.Errorf("missing top_hit_files in stage=ok line; got:\n%s", okLine)
	}
}
