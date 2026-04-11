package heimdall

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// createTestProject creates a temp directory with the given files.
// Each file maps name → content. Returns the project root path.
func createTestProject(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, name)
		os.MkdirAll(filepath.Dir(path), 0755)
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestIncrementalIndex_SkipsUnchangedFiles(t *testing.T) {
	// Create a project with 5 files
	files := map[string]string{
		"a.go": "package main\nfunc a() {}",
		"b.go": "package main\nfunc b() {}",
		"c.go": "package main\nfunc c() {}",
		"d.go": "package main\nfunc d() {}",
		"e.go": "package main\nfunc e() {}",
	}
	root := createTestProject(t, files)

	dbDir := filepath.Join(t.TempDir(), ".heimdall_db")
	store, err := OpenStore(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	embedder := &MockEmbedder{Vectors: make(map[string][]float32), Dimension: 3}
	indexer := NewIndexer(root, embedder, store, ChunkerOpts{MaxChunkSize: 1500})

	ctx := context.Background()

	// First index: should index all 5 files
	result1, err := indexer.IndexAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result1.FilesScanned != 5 {
		t.Errorf("first index: FilesScanned = %d, want 5", result1.FilesScanned)
	}
	if result1.FilesIndexed != 5 {
		t.Errorf("first index: FilesIndexed = %d, want 5", result1.FilesIndexed)
	}
	firstEmbedCount := embedder.CallCount

	// Incremental re-index with no changes: should skip all 5
	result2, err := indexer.IndexIncremental(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result2.FilesSkipped != 5 {
		t.Errorf("no-change re-index: FilesSkipped = %d, want 5", result2.FilesSkipped)
	}
	if result2.FilesIndexed != 0 {
		t.Errorf("no-change re-index: FilesIndexed = %d, want 0", result2.FilesIndexed)
	}
	// Embedder should not have been called again
	if embedder.CallCount != firstEmbedCount {
		t.Errorf("no-change re-index: embedder called %d times (expected 0 additional calls)", embedder.CallCount-firstEmbedCount)
	}

	// Modify one file (c.go) — touch it with new content and a future modtime
	time.Sleep(10 * time.Millisecond) // ensure modtime differs
	newContent := "package main\nfunc c_updated() { println(\"changed\") }"
	if err := os.WriteFile(filepath.Join(root, "c.go"), []byte(newContent), 0644); err != nil {
		t.Fatal(err)
	}
	// Force modtime to be newer
	future := time.Now().Add(time.Second)
	os.Chtimes(filepath.Join(root, "c.go"), future, future)

	embedCountBefore := embedder.CallCount

	// Incremental re-index: should index only c.go
	result3, err := indexer.IndexIncremental(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result3.FilesScanned != 5 {
		t.Errorf("one-change re-index: FilesScanned = %d, want 5", result3.FilesScanned)
	}
	if result3.FilesIndexed != 1 {
		t.Errorf("one-change re-index: FilesIndexed = %d, want 1", result3.FilesIndexed)
	}
	if result3.FilesSkipped != 4 {
		t.Errorf("one-change re-index: FilesSkipped = %d, want 4", result3.FilesSkipped)
	}

	// Embedder should have been called only for the one changed file's chunks
	additionalCalls := embedder.CallCount - embedCountBefore
	if additionalCalls != result3.ChunksCreated {
		t.Errorf("one-change re-index: embedder called %d times, but %d chunks created", additionalCalls, result3.ChunksCreated)
	}
	if additionalCalls == 0 {
		t.Error("one-change re-index: embedder was not called at all")
	}
}

func TestIncrementalIndex_NewFileIsIndexed(t *testing.T) {
	files := map[string]string{
		"a.go": "package main\nfunc a() {}",
		"b.go": "package main\nfunc b() {}",
	}
	root := createTestProject(t, files)

	dbDir := filepath.Join(t.TempDir(), ".heimdall_db")
	store, err := OpenStore(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	embedder := &MockEmbedder{Vectors: make(map[string][]float32), Dimension: 3}
	indexer := NewIndexer(root, embedder, store, ChunkerOpts{MaxChunkSize: 1500})

	ctx := context.Background()

	// First index
	result1, err := indexer.IndexAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result1.FilesIndexed != 2 {
		t.Errorf("first index: FilesIndexed = %d, want 2", result1.FilesIndexed)
	}

	// Add a new file
	os.WriteFile(filepath.Join(root, "c.go"), []byte("package main\nfunc c() {}"), 0644)

	// Incremental: should pick up the new file
	result2, err := indexer.IndexIncremental(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result2.FilesIndexed != 1 {
		t.Errorf("new-file re-index: FilesIndexed = %d, want 1", result2.FilesIndexed)
	}
	if result2.FilesSkipped != 2 {
		t.Errorf("new-file re-index: FilesSkipped = %d, want 2", result2.FilesSkipped)
	}
}

func TestIncrementalIndex_ContentHashFallback(t *testing.T) {
	// Test that even if modtime changes, same content hash = skip
	files := map[string]string{
		"a.go": "package main\nfunc a() {}",
	}
	root := createTestProject(t, files)

	dbDir := filepath.Join(t.TempDir(), ".heimdall_db")
	store, err := OpenStore(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	embedder := &MockEmbedder{Vectors: make(map[string][]float32), Dimension: 3}
	indexer := NewIndexer(root, embedder, store, ChunkerOpts{MaxChunkSize: 1500})

	ctx := context.Background()

	// First index
	result1, err := indexer.IndexAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result1.FilesIndexed != 1 {
		t.Fatal("expected 1 file indexed")
	}

	// Touch file (change modtime) but keep same content
	future := time.Now().Add(10 * time.Second)
	os.Chtimes(filepath.Join(root, "a.go"), future, future)

	embedCountBefore := embedder.CallCount

	// Incremental: modtime changed but content hash matches → should skip
	result2, err := indexer.IndexIncremental(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result2.FilesSkipped != 1 {
		t.Errorf("same-content re-index: FilesSkipped = %d, want 1", result2.FilesSkipped)
	}
	if result2.FilesIndexed != 0 {
		t.Errorf("same-content re-index: FilesIndexed = %d, want 0", result2.FilesIndexed)
	}
	if embedder.CallCount != embedCountBefore {
		t.Errorf("same-content re-index: embedder was called (should not be)")
	}
}

func TestIndexAll_DoesNotSkip(t *testing.T) {
	files := map[string]string{
		"a.go": "package main\nfunc a() {}",
		"b.go": "package main\nfunc b() {}",
	}
	root := createTestProject(t, files)

	dbDir := filepath.Join(t.TempDir(), ".heimdall_db")
	store, err := OpenStore(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	embedder := &MockEmbedder{Vectors: make(map[string][]float32), Dimension: 3}
	indexer := NewIndexer(root, embedder, store, ChunkerOpts{MaxChunkSize: 1500})

	ctx := context.Background()

	// First full index
	result1, err := indexer.IndexAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result1.FilesIndexed != 2 {
		t.Fatal("expected 2 files indexed")
	}

	// Second full index — should re-index everything (not incremental)
	result2, err := indexer.IndexAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result2.FilesIndexed != 2 {
		t.Errorf("full re-index: FilesIndexed = %d, want 2", result2.FilesIndexed)
	}
	if result2.FilesSkipped != 0 {
		t.Errorf("full re-index: FilesSkipped = %d, want 0", result2.FilesSkipped)
	}
}
