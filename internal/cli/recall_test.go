package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

func newRecallFixture(t *testing.T) *heimdall.MemoryStore {
	t.Helper()
	dir := t.TempDir()
	store, err := heimdall.OpenMemoryStore(filepath.Join(dir, "memories.db"))
	if err != nil {
		t.Fatalf("open memory store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	now := time.Now().Unix()
	if err := store.UpsertMemory(heimdall.Memory{
		ID:          "mem:1",
		Content:     "User prefers TDD",
		Type:        heimdall.MemoryTypePreference,
		Tags:        []string{"testing"},
		Vector:      []float32{1, 0, 0},
		CreatedAt:   now,
		UpdatedAt:   now,
		Source:      heimdall.MemorySourceExplicit,
		ContentHash: heimdall.ContentHash("User prefers TDD"),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return store
}

func testRecallDeps(store *heimdall.MemoryStore) RecallDeps {
	return RecallDeps{
		LoadConfig: func() config.Config {
			return config.Config{OllamaEndpoint: "http://127.0.0.1:0", Model: "test-model"}
		},
		OpenMemoryStore: func() (*heimdall.MemoryStore, error) { return store, nil },
		NewEmbedder: func(_ context.Context, _, _ string) (heimdall.Embedder, error) {
			return &heimdall.MockEmbedder{
				Vectors:   map[string][]float32{"testing workflow": {1, 0, 0}},
				Dimension: 3,
			}, nil
		},
	}
}

func runRecall(t *testing.T, store *heimdall.MemoryStore, args ...string) (string, string, int) {
	t.Helper()
	var stdout, stderr strings.Builder
	code := CLIRecall(strings.NewReader(""), &stdout, &stderr, nil, args, testRecallDeps(store))
	return stdout.String(), stderr.String(), code
}

func TestCLIRecall_HappyPathText(t *testing.T) {
	store := newRecallFixture(t)
	stdout, stderr, code := runRecall(t, store, "--query", "testing workflow")
	if code != 0 {
		t.Fatalf("exit %d, stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "[preference]") || !strings.Contains(stdout, "User prefers TDD") {
		t.Errorf("text output missing fields: %q", stdout)
	}
	if !strings.Contains(stdout, "tags: testing") {
		t.Errorf("tags not rendered: %q", stdout)
	}
}

func TestCLIRecall_JSONShape(t *testing.T) {
	store := newRecallFixture(t)
	stdout, _, code := runRecall(t, store, "--query", "testing workflow", "--format", "json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var hits []heimdall.RecallHit
	if err := json.Unmarshal([]byte(stdout), &hits); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	if len(hits) != 1 || hits[0].ID != "mem:1" {
		t.Errorf("unexpected hits: %+v", hits)
	}
}

func TestCLIRecall_JSONEmptyIsArray(t *testing.T) {
	store := newRecallFixture(t)
	deps := testRecallDeps(store)
	// embedder returns a vector that won't match the seed — but SearchMemories
	// still returns everything scored. Use a fresh empty store.
	dir := t.TempDir()
	empty, err := heimdall.OpenMemoryStore(filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	deps.OpenMemoryStore = func() (*heimdall.MemoryStore, error) { return empty, nil }

	var stdout, stderr strings.Builder
	code := CLIRecall(strings.NewReader(""), &stdout, &stderr, nil,
		[]string{"--query", "nothing", "--format", "json"}, deps)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	trimmed := strings.TrimSpace(stdout.String())
	if trimmed != "[]" {
		t.Errorf("expected empty JSON array, got %q", trimmed)
	}
}

func TestCLIRecall_HookMdFormat(t *testing.T) {
	store := newRecallFixture(t)
	stdout, _, code := runRecall(t, store, "--query", "testing workflow", "--format", "hook-md")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	want := "- preference: User prefers TDD _(tags: testing)_"
	if !strings.Contains(stdout, want) {
		t.Errorf("hook-md output wrong:\ngot:  %q\nwant: %q", stdout, want)
	}
}

func TestCLIRecall_MissingQueryIsUsageError(t *testing.T) {
	store := newRecallFixture(t)
	_, stderr, code := runRecall(t, store)
	if code != 2 {
		t.Errorf("expected exit 2, got %d", code)
	}
	if !strings.Contains(stderr, "--query is required") {
		t.Errorf("unexpected stderr: %q", stderr)
	}
}

func TestCLIRecall_InvalidFormat(t *testing.T) {
	store := newRecallFixture(t)
	_, stderr, code := runRecall(t, store, "--query", "q", "--format", "yaml")
	if code != 2 {
		t.Errorf("expected exit 2, got %d", code)
	}
	if !strings.Contains(stderr, "invalid --format") {
		t.Errorf("unexpected stderr: %q", stderr)
	}
}

func TestCLIRecall_InvalidType(t *testing.T) {
	store := newRecallFixture(t)
	_, stderr, code := runRecall(t, store, "--query", "q", "--type", "bogus")
	if code != 2 {
		t.Errorf("expected exit 2, got %d", code)
	}
	if !strings.Contains(stderr, "invalid --type") {
		t.Errorf("unexpected stderr: %q", stderr)
	}
}

func TestCLIRecall_OllamaUnavailable(t *testing.T) {
	store := newRecallFixture(t)
	deps := testRecallDeps(store)
	deps.NewEmbedder = func(context.Context, string, string) (heimdall.Embedder, error) {
		return nil, &fakeErr{"ollama not reachable at http://127.0.0.1:0"}
	}
	var stdout, stderr strings.Builder
	code := CLIRecall(strings.NewReader(""), &stdout, &stderr, nil,
		[]string{"--query", "q"}, deps)
	if code != 1 {
		t.Errorf("expected exit 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "ollama not reachable") {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}
}

type fakeErr struct{ msg string }

func (e *fakeErr) Error() string { return e.msg }

// TEST-A-001: empty store must emit JSON "[]" (never null).
func TestCLIRecall_EmptyStore_JSONArrayNotNull(t *testing.T) {
	dir := t.TempDir()
	empty, err := heimdall.OpenMemoryStore(filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer empty.Close()

	deps := RecallDeps{
		LoadConfig: func() config.Config {
			return config.Config{OllamaEndpoint: "http://127.0.0.1:0", Model: "test-model"}
		},
		OpenMemoryStore: func() (*heimdall.MemoryStore, error) { return empty, nil },
		NewEmbedder: func(_ context.Context, _, _ string) (heimdall.Embedder, error) {
			return &heimdall.MockEmbedder{
				Vectors:   map[string][]float32{"anything": {1, 0, 0}},
				Dimension: 3,
			}, nil
		},
	}

	var stdout, stderr strings.Builder
	code := CLIRecall(strings.NewReader(""), &stdout, &stderr, nil,
		[]string{"--query", "anything", "--format", "json"}, deps)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}

	// Must not be the JSON literal null.
	raw := strings.TrimSpace(stdout.String())
	if raw == "null" {
		t.Fatalf("got literal JSON null, want an empty array")
	}

	// Must parse as an empty array (not nil after parse — explicit []).
	var hits []heimdall.RecallHit
	if err := json.Unmarshal([]byte(raw), &hits); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, raw)
	}
	if len(hits) != 0 {
		t.Errorf("expected 0 hits, got %d", len(hits))
	}
	if raw != "[]" {
		t.Errorf("expected exactly %q, got %q", "[]", raw)
	}
}

// TEST-A-004: --limit edge cases.
//
// Plan decision: --limit=0 is accepted by flag parser and flows into
// heimdall.RunRecall, which clamps non-positive values to the default (5).
// Same for negatives. This test locks that behavior so a future change
// toward usage-error semantics is a conscious break, not a silent regression.
func TestCLIRecall_LimitZero(t *testing.T) {
	store := newRecallFixture(t) // seeds 1 record
	stdout, stderr, code := runRecall(t, store,
		"--query", "testing workflow", "--limit", "0", "--format", "json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	var hits []heimdall.RecallHit
	if err := json.Unmarshal([]byte(stdout), &hits); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	// Clamped to default=5; store has 1 record, so 1 hit comes back.
	if len(hits) != 1 {
		t.Errorf("--limit 0 should clamp to default and yield all available hits (1), got %d", len(hits))
	}
}

func TestCLIRecall_LimitExceedsStore(t *testing.T) {
	store := newRecallFixture(t) // seeds 1 record (mem:1)
	// Seed two more records so the store has exactly 3.
	now := time.Now().Unix()
	if err := store.UpsertMemory(heimdall.Memory{
		ID: "mem:2", Content: "second", Type: heimdall.MemoryTypeFact,
		Vector: []float32{1, 0, 0}, CreatedAt: now, UpdatedAt: now,
		Source: heimdall.MemorySourceExplicit, ContentHash: heimdall.ContentHash("second"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertMemory(heimdall.Memory{
		ID: "mem:3", Content: "third", Type: heimdall.MemoryTypeFact,
		Vector: []float32{1, 0, 0}, CreatedAt: now, UpdatedAt: now,
		Source: heimdall.MemorySourceExplicit, ContentHash: heimdall.ContentHash("third"),
	}); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runRecall(t, store,
		"--query", "testing workflow", "--limit", "100", "--format", "json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	var hits []heimdall.RecallHit
	if err := json.Unmarshal([]byte(stdout), &hits); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	if len(hits) != 3 {
		t.Errorf("expected 3 hits, got %d: %+v", len(hits), hits)
	}
}

func TestCLIRecall_LimitOne(t *testing.T) {
	store := newRecallFixture(t) // seeds 1 record
	now := time.Now().Unix()
	if err := store.UpsertMemory(heimdall.Memory{
		ID: "mem:2", Content: "another", Type: heimdall.MemoryTypeFact,
		Vector: []float32{1, 0, 0}, CreatedAt: now, UpdatedAt: now,
		Source: heimdall.MemorySourceExplicit, ContentHash: heimdall.ContentHash("another"),
	}); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runRecall(t, store,
		"--query", "testing workflow", "--limit", "1", "--format", "json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	var hits []heimdall.RecallHit
	if err := json.Unmarshal([]byte(stdout), &hits); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	if len(hits) != 1 {
		t.Errorf("expected exactly 1 hit, got %d", len(hits))
	}
}

func TestCLIRecall_TagsCSV(t *testing.T) {
	store := newRecallFixture(t)
	// Seed with multiple tagged entries; only one matches the filter.
	now := time.Now().Unix()
	store.UpsertMemory(heimdall.Memory{
		ID: "mem:2", Content: "uses go modules", Type: heimdall.MemoryTypeFact,
		Tags: []string{"go"}, Vector: []float32{1, 0, 0},
		CreatedAt: now, UpdatedAt: now, Source: heimdall.MemorySourceExplicit,
		ContentHash: heimdall.ContentHash("uses go modules"),
	})

	stdout, _, code := runRecall(t, store, "--query", "testing workflow", "--tags", "testing")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(stdout, "uses go modules") {
		t.Errorf("tag filter failed, got: %q", stdout)
	}
	if !strings.Contains(stdout, "User prefers TDD") {
		t.Errorf("expected tagged result, got: %q", stdout)
	}
}
