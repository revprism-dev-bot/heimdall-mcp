package heimdall

import (
	"path/filepath"
	"testing"
	"time"
)

func testMemoryStore(t *testing.T) *MemoryStore {
	t.Helper()
	dir := t.TempDir()
	store, err := OpenMemoryStore(filepath.Join(dir, "memories.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestUpsertMemory(t *testing.T) {
	store := testMemoryStore(t)

	now := time.Now().Unix()
	m := Memory{
		ID:          "mem:test:1",
		Content:     "User prefers TDD",
		Type:        MemoryTypePreference,
		Tags:        []string{"testing", "workflow"},
		Project:     "heimdall",
		Vector:      []float32{1.0, 0.0, 0.0},
		CreatedAt:   now,
		UpdatedAt:   now,
		Source:       MemorySourceExplicit,
		ContentHash: "abc123",
	}

	if err := store.UpsertMemory(m); err != nil {
		t.Fatalf("UpsertMemory: %v", err)
	}

	got, err := store.GetMemoryByID("mem:test:1")
	if err != nil {
		t.Fatalf("GetMemoryByID: %v", err)
	}
	if got == nil {
		t.Fatal("expected memory, got nil")
	}
	if got.Content != "User prefers TDD" {
		t.Errorf("content = %q, want %q", got.Content, "User prefers TDD")
	}
	if got.Type != MemoryTypePreference {
		t.Errorf("type = %q, want %q", got.Type, MemoryTypePreference)
	}
	if len(got.Tags) != 2 {
		t.Errorf("tags len = %d, want 2", len(got.Tags))
	}
}

func TestUpsertMemory_Update(t *testing.T) {
	store := testMemoryStore(t)

	now := time.Now().Unix()
	m := Memory{
		ID:          "mem:test:1",
		Content:     "Original content",
		Type:        MemoryTypeFact,
		Vector:      []float32{1.0, 0.0, 0.0},
		CreatedAt:   now,
		UpdatedAt:   now,
		Source:       MemorySourceExplicit,
		ContentHash: "hash1",
	}
	store.UpsertMemory(m)

	m.Content = "Updated content"
	m.UpdatedAt = now + 100
	store.UpsertMemory(m)

	got, _ := store.GetMemoryByID("mem:test:1")
	if got.Content != "Updated content" {
		t.Errorf("content = %q, want %q", got.Content, "Updated content")
	}
}

func TestSearchMemories_Basic(t *testing.T) {
	store := testMemoryStore(t)

	now := time.Now().Unix()
	// Insert two memories with different vectors
	store.UpsertMemory(Memory{
		ID: "mem:1", Content: "Use SQLite for storage", Type: MemoryTypeFact,
		Vector: []float32{1.0, 0.0, 0.0}, CreatedAt: now, UpdatedAt: now,
		Source: MemorySourceExplicit, ContentHash: "h1",
	})
	store.UpsertMemory(Memory{
		ID: "mem:2", Content: "Use Postgres for production", Type: MemoryTypeFact,
		Vector: []float32{0.0, 1.0, 0.0}, CreatedAt: now, UpdatedAt: now,
		Source: MemorySourceExplicit, ContentHash: "h2",
	})

	// Query close to mem:1
	results := store.SearchMemories([]float32{0.9, 0.1, 0.0}, 5, MemoryFilter{})
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	// First result should be mem:1 (closer to query)
	if results[0].Memory.ID != "mem:1" {
		t.Errorf("first result ID = %q, want %q", results[0].Memory.ID, "mem:1")
	}
	if results[0].Similarity <= results[1].Similarity {
		t.Errorf("first result should have higher similarity")
	}
}

func TestSearchMemories_FilterByType(t *testing.T) {
	store := testMemoryStore(t)

	now := time.Now().Unix()
	store.UpsertMemory(Memory{
		ID: "mem:1", Content: "Fact", Type: MemoryTypeFact,
		Vector: []float32{1.0, 0.0, 0.0}, CreatedAt: now, UpdatedAt: now,
		Source: MemorySourceExplicit, ContentHash: "h1",
	})
	store.UpsertMemory(Memory{
		ID: "mem:2", Content: "Pref", Type: MemoryTypePreference,
		Vector: []float32{1.0, 0.0, 0.0}, CreatedAt: now, UpdatedAt: now,
		Source: MemorySourceExplicit, ContentHash: "h2",
	})

	results := store.SearchMemories([]float32{1.0, 0.0, 0.0}, 5, MemoryFilter{Type: MemoryTypeFact})
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if results[0].Memory.ID != "mem:1" {
		t.Errorf("result ID = %q, want %q", results[0].Memory.ID, "mem:1")
	}
}

func TestSearchMemories_FilterByTags(t *testing.T) {
	store := testMemoryStore(t)

	now := time.Now().Unix()
	store.UpsertMemory(Memory{
		ID: "mem:1", Content: "Has tag", Type: MemoryTypeFact,
		Tags: []string{"database"}, Vector: []float32{1.0, 0.0, 0.0},
		CreatedAt: now, UpdatedAt: now, Source: MemorySourceExplicit, ContentHash: "h1",
	})
	store.UpsertMemory(Memory{
		ID: "mem:2", Content: "No tag", Type: MemoryTypeFact,
		Tags: []string{"frontend"}, Vector: []float32{1.0, 0.0, 0.0},
		CreatedAt: now, UpdatedAt: now, Source: MemorySourceExplicit, ContentHash: "h2",
	})

	results := store.SearchMemories([]float32{1.0, 0.0, 0.0}, 5, MemoryFilter{Tags: []string{"database"}})
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if results[0].Memory.ID != "mem:1" {
		t.Errorf("result ID = %q, want %q", results[0].Memory.ID, "mem:1")
	}
}

func TestSearchMemories_FilterByProject(t *testing.T) {
	store := testMemoryStore(t)

	now := time.Now().Unix()
	store.UpsertMemory(Memory{
		ID: "mem:1", Content: "ProjectA", Type: MemoryTypeFact, Project: "projectA",
		Vector: []float32{1.0, 0.0, 0.0}, CreatedAt: now, UpdatedAt: now,
		Source: MemorySourceExplicit, ContentHash: "h1",
	})
	store.UpsertMemory(Memory{
		ID: "mem:2", Content: "ProjectB", Type: MemoryTypeFact, Project: "projectB",
		Vector: []float32{1.0, 0.0, 0.0}, CreatedAt: now, UpdatedAt: now,
		Source: MemorySourceExplicit, ContentHash: "h2",
	})

	results := store.SearchMemories([]float32{1.0, 0.0, 0.0}, 5, MemoryFilter{Project: "projectA"})
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if results[0].Memory.ID != "mem:1" {
		t.Errorf("result ID = %q, want %q", results[0].Memory.ID, "mem:1")
	}
}

func TestSearchMemories_CombinedFilters(t *testing.T) {
	store := testMemoryStore(t)

	now := time.Now().Unix()
	store.UpsertMemory(Memory{
		ID: "mem:1", Content: "Match all", Type: MemoryTypeFact, Project: "p",
		Tags: []string{"go"}, Vector: []float32{1.0, 0.0, 0.0},
		CreatedAt: now, UpdatedAt: now, Source: MemorySourceExplicit, ContentHash: "h1",
	})
	store.UpsertMemory(Memory{
		ID: "mem:2", Content: "Wrong type", Type: MemoryTypeDecision, Project: "p",
		Tags: []string{"go"}, Vector: []float32{1.0, 0.0, 0.0},
		CreatedAt: now, UpdatedAt: now, Source: MemorySourceExplicit, ContentHash: "h2",
	})
	store.UpsertMemory(Memory{
		ID: "mem:3", Content: "Wrong project", Type: MemoryTypeFact, Project: "other",
		Tags: []string{"go"}, Vector: []float32{1.0, 0.0, 0.0},
		CreatedAt: now, UpdatedAt: now, Source: MemorySourceExplicit, ContentHash: "h3",
	})

	results := store.SearchMemories([]float32{1.0, 0.0, 0.0}, 5, MemoryFilter{
		Type:    MemoryTypeFact,
		Project: "p",
		Tags:    []string{"go"},
	})
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if results[0].Memory.ID != "mem:1" {
		t.Errorf("result ID = %q, want %q", results[0].Memory.ID, "mem:1")
	}
}

func TestFindSimilarMemory_AboveThreshold(t *testing.T) {
	store := testMemoryStore(t)

	now := time.Now().Unix()
	store.UpsertMemory(Memory{
		ID: "mem:1", Content: "Similar", Type: MemoryTypeFact,
		Vector: []float32{1.0, 0.0, 0.0}, CreatedAt: now, UpdatedAt: now,
		Source: MemorySourceExplicit, ContentHash: "h1",
	})

	// Very similar vector
	m, sim, err := store.FindSimilarMemory([]float32{0.99, 0.01, 0.0}, 0.92)
	if err != nil {
		t.Fatalf("FindSimilarMemory: %v", err)
	}
	if m == nil {
		t.Fatal("expected match, got nil")
	}
	if sim < 0.92 {
		t.Errorf("similarity = %.4f, want >= 0.92", sim)
	}
	if m.ID != "mem:1" {
		t.Errorf("ID = %q, want %q", m.ID, "mem:1")
	}
}

func TestFindSimilarMemory_BelowThreshold(t *testing.T) {
	store := testMemoryStore(t)

	now := time.Now().Unix()
	store.UpsertMemory(Memory{
		ID: "mem:1", Content: "Not similar", Type: MemoryTypeFact,
		Vector: []float32{1.0, 0.0, 0.0}, CreatedAt: now, UpdatedAt: now,
		Source: MemorySourceExplicit, ContentHash: "h1",
	})

	// Orthogonal vector — similarity ~0
	m, _, err := store.FindSimilarMemory([]float32{0.0, 1.0, 0.0}, 0.92)
	if err != nil {
		t.Fatalf("FindSimilarMemory: %v", err)
	}
	if m != nil {
		t.Errorf("expected nil, got %+v", m)
	}
}

func TestGetMemoryByHash(t *testing.T) {
	store := testMemoryStore(t)

	now := time.Now().Unix()
	store.UpsertMemory(Memory{
		ID: "mem:1", Content: "Hashed", Type: MemoryTypeFact,
		Vector: []float32{1.0, 0.0, 0.0}, CreatedAt: now, UpdatedAt: now,
		Source: MemorySourceExplicit, ContentHash: "unique-hash-123",
	})

	got, err := store.GetMemoryByHash("unique-hash-123")
	if err != nil {
		t.Fatalf("GetMemoryByHash: %v", err)
	}
	if got == nil {
		t.Fatal("expected memory, got nil")
	}
	if got.ID != "mem:1" {
		t.Errorf("ID = %q, want %q", got.ID, "mem:1")
	}

	// Non-existent hash
	got, err = store.GetMemoryByHash("nonexistent")
	if err != nil {
		t.Fatalf("GetMemoryByHash: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

func TestMemoryStats(t *testing.T) {
	store := testMemoryStore(t)

	now := time.Now().Unix()
	store.UpsertMemory(Memory{
		ID: "mem:1", Content: "A", Type: MemoryTypeFact,
		Vector: []float32{1.0}, CreatedAt: now, UpdatedAt: now,
		Source: MemorySourceExplicit, ContentHash: "h1",
	})
	store.UpsertMemory(Memory{
		ID: "mem:2", Content: "B", Type: MemoryTypeFact,
		Vector: []float32{1.0}, CreatedAt: now, UpdatedAt: now,
		Source: MemorySourceSession, ContentHash: "h2",
	})
	store.UpsertMemory(Memory{
		ID: "mem:3", Content: "C", Type: MemoryTypeDecision, Project: "p1",
		Vector: []float32{1.0}, CreatedAt: now, UpdatedAt: now,
		Source: MemorySourceExplicit, ContentHash: "h3",
	})

	stats := store.MemoryStats()
	if stats.TotalMemories != 3 {
		t.Errorf("TotalMemories = %d, want 3", stats.TotalMemories)
	}
	if stats.ByType["fact"] != 2 {
		t.Errorf("ByType[fact] = %d, want 2", stats.ByType["fact"])
	}
	if stats.ByType["decision"] != 1 {
		t.Errorf("ByType[decision] = %d, want 1", stats.ByType["decision"])
	}
	if stats.BySource["explicit"] != 2 {
		t.Errorf("BySource[explicit] = %d, want 2", stats.BySource["explicit"])
	}
	if stats.BySource["session"] != 1 {
		t.Errorf("BySource[session] = %d, want 1", stats.BySource["session"])
	}
}

func TestMemoryCount(t *testing.T) {
	store := testMemoryStore(t)

	if c := store.MemoryCount(); c != 0 {
		t.Errorf("MemoryCount = %d, want 0", c)
	}

	now := time.Now().Unix()
	store.UpsertMemory(Memory{
		ID: "mem:1", Content: "A", Type: MemoryTypeFact,
		Vector: []float32{1.0}, CreatedAt: now, UpdatedAt: now,
		Source: MemorySourceExplicit, ContentHash: "h1",
	})

	if c := store.MemoryCount(); c != 1 {
		t.Errorf("MemoryCount = %d, want 1", c)
	}
}

func TestLoadAllMemoryVectors(t *testing.T) {
	store := testMemoryStore(t)

	now := time.Now().Unix()
	store.UpsertMemory(Memory{
		ID: "mem:1", Content: "A", Type: MemoryTypeFact,
		Vector: []float32{1.0, 2.0, 3.0}, CreatedAt: now, UpdatedAt: now,
		Source: MemorySourceExplicit, ContentHash: "h1",
	})
	store.UpsertMemory(Memory{
		ID: "mem:2", Content: "B", Type: MemoryTypeFact,
		Vector: []float32{4.0, 5.0, 6.0}, CreatedAt: now, UpdatedAt: now,
		Source: MemorySourceExplicit, ContentHash: "h2",
	})

	entries, err := store.LoadAllMemoryVectors()
	if err != nil {
		t.Fatalf("LoadAllMemoryVectors: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
}

// --- context_path tests (Feature: memory scope filtering) -------------------

func TestUpsertMemory_PersistsContextPath(t *testing.T) {
	store := testMemoryStore(t)
	now := time.Now().Unix()
	m := Memory{
		ID: "mem:cp:1", Content: "scoped", Type: MemoryTypeFact,
		Vector:    []float32{1, 0, 0},
		CreatedAt: now, UpdatedAt: now,
		Source: MemorySourceExplicit, ContentHash: "hash-cp-1",
		ContextPath: "internal/cli",
	}
	if err := store.UpsertMemory(m); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := store.GetMemoryByID("mem:cp:1")
	if err != nil || got == nil {
		t.Fatalf("GetMemoryByID: got=%v err=%v", got, err)
	}
	if got.ContextPath != "internal/cli" {
		t.Errorf("ContextPath = %q, want internal/cli", got.ContextPath)
	}

	got2, err := store.GetMemoryByHash("hash-cp-1")
	if err != nil || got2 == nil {
		t.Fatalf("GetMemoryByHash: got=%v err=%v", got2, err)
	}
	if got2.ContextPath != "internal/cli" {
		t.Errorf("ByHash ContextPath = %q, want internal/cli", got2.ContextPath)
	}
}

func TestSearchMemories_FilterByContextPath_ExactMatch(t *testing.T) {
	store := testMemoryStore(t)
	now := time.Now().Unix()
	store.UpsertMemory(Memory{
		ID: "mem:1", Content: "cli", Type: MemoryTypeFact,
		Vector: []float32{1, 0, 0}, CreatedAt: now, UpdatedAt: now,
		Source: MemorySourceExplicit, ContentHash: "h1",
		ContextPath: "internal/cli",
	})
	store.UpsertMemory(Memory{
		ID: "mem:2", Content: "api", Type: MemoryTypeFact,
		Vector: []float32{1, 0, 0}, CreatedAt: now, UpdatedAt: now,
		Source: MemorySourceExplicit, ContentHash: "h2",
		ContextPath: "internal/api",
	})

	results := store.SearchMemories([]float32{1, 0, 0}, 5, MemoryFilter{ContextPath: "internal/cli"})
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if results[0].Memory.ID != "mem:1" {
		t.Errorf("got %q, want mem:1", results[0].Memory.ID)
	}
}

func TestSearchMemories_FilterByContextPath_PrefixMatch(t *testing.T) {
	store := testMemoryStore(t)
	now := time.Now().Unix()
	store.UpsertMemory(Memory{
		ID: "mem:1", Content: "hooks", Type: MemoryTypeFact,
		Vector: []float32{1, 0, 0}, CreatedAt: now, UpdatedAt: now,
		Source: MemorySourceExplicit, ContentHash: "h1",
		ContextPath: "internal/cli/hooks",
	})
	store.UpsertMemory(Memory{
		ID: "mem:2", Content: "sibling", Type: MemoryTypeFact,
		Vector: []float32{1, 0, 0}, CreatedAt: now, UpdatedAt: now,
		Source: MemorySourceExplicit, ContentHash: "h2",
		ContextPath: "internal/api",
	})

	// "internal/cli" should match child "internal/cli/hooks".
	results := store.SearchMemories([]float32{1, 0, 0}, 5, MemoryFilter{ContextPath: "internal/cli"})
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if results[0].Memory.ID != "mem:1" {
		t.Errorf("got %q, want mem:1", results[0].Memory.ID)
	}
}

func TestSearchMemories_ContextPathEmpty_ReturnsAll(t *testing.T) {
	store := testMemoryStore(t)
	now := time.Now().Unix()
	store.UpsertMemory(Memory{
		ID: "mem:1", Content: "one", Type: MemoryTypeFact,
		Vector: []float32{1, 0, 0}, CreatedAt: now, UpdatedAt: now,
		Source: MemorySourceExplicit, ContentHash: "h1",
		ContextPath: "internal/cli",
	})
	store.UpsertMemory(Memory{
		ID: "mem:2", Content: "two", Type: MemoryTypeFact,
		Vector: []float32{1, 0, 0}, CreatedAt: now, UpdatedAt: now,
		Source: MemorySourceExplicit, ContentHash: "h2",
	})

	results := store.SearchMemories([]float32{1, 0, 0}, 5, MemoryFilter{})
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2 (no filter)", len(results))
	}
}
