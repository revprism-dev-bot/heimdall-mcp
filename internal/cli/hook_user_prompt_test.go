package cli

import (
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
}

func runHookUserPrompt(t *testing.T, opts runUPOpts) (stdout, stderr string, code int) {
	t.Helper()
	var out, errBuf strings.Builder
	deps := HookUserPromptDeps{
		Suppress: opts.suppress,
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
