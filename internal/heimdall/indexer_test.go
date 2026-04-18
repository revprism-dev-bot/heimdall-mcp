package heimdall

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

	embedder := &StubEmbedder{Vectors: make(map[string][]float32), Dimension: 3}
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

	embedder := &StubEmbedder{Vectors: make(map[string][]float32), Dimension: 3}
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

	embedder := &StubEmbedder{Vectors: make(map[string][]float32), Dimension: 3}
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

	embedder := &StubEmbedder{Vectors: make(map[string][]float32), Dimension: 3}
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

func TestIndexAll_SkipsSubRepoDirectories(t *testing.T) {
	// Create a parent project with a sub-repo (directory containing .git/)
	root := t.TempDir()

	// Parent files
	os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nfunc main() {}"), 0644)
	os.WriteFile(filepath.Join(root, "util.go"), []byte("package main\nfunc util() {}"), 0644)

	// Sub-repo with its own .git/ directory
	subRepo := filepath.Join(root, "sub-service")
	os.MkdirAll(filepath.Join(subRepo, ".git"), 0755)
	os.WriteFile(filepath.Join(subRepo, "app.go"), []byte("package sub\nfunc app() {}"), 0644)
	os.WriteFile(filepath.Join(subRepo, "handler.go"), []byte("package sub\nfunc handler() {}"), 0644)

	// Another sub-repo
	subRepo2 := filepath.Join(root, "sub-infra")
	os.MkdirAll(filepath.Join(subRepo2, ".git"), 0755)
	os.WriteFile(filepath.Join(subRepo2, "infra.go"), []byte("package infra\nfunc deploy() {}"), 0644)

	// Regular subdirectory (no .git/) should still be indexed
	os.MkdirAll(filepath.Join(root, "pkg"), 0755)
	os.WriteFile(filepath.Join(root, "pkg", "lib.go"), []byte("package pkg\nfunc lib() {}"), 0644)

	dbDir := filepath.Join(t.TempDir(), ".heimdall_db")
	store, err := OpenStore(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	embedder := &StubEmbedder{Vectors: make(map[string][]float32), Dimension: 3}
	indexer := NewIndexer(root, embedder, store, ChunkerOpts{MaxChunkSize: 1500})

	ctx := context.Background()
	result, err := indexer.IndexAll(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Should only index parent files + pkg/lib.go (3 files total), NOT sub-repo files
	if result.FilesScanned != 3 {
		t.Errorf("FilesScanned = %d, want 3 (main.go, util.go, pkg/lib.go)", result.FilesScanned)
	}
	if result.FilesIndexed != 3 {
		t.Errorf("FilesIndexed = %d, want 3", result.FilesIndexed)
	}

	// Verify sub-repo files are not in store
	if store.HasFile("sub-service/app.go") {
		t.Error("sub-repo file sub-service/app.go should not be indexed")
	}
	if store.HasFile("sub-infra/infra.go") {
		t.Error("sub-repo file sub-infra/infra.go should not be indexed")
	}

	// Verify parent files ARE in store
	if !store.HasFile("main.go") {
		t.Error("parent file main.go should be indexed")
	}
	if !store.HasFile("pkg/lib.go") {
		t.Error("regular subdir file pkg/lib.go should be indexed")
	}
}

func TestIndexAll_SubProjectTagging(t *testing.T) {
	// Sub-project tagging only applies when sub-repo files are indexed
	// (e.g. when using IncludePaths that include a sub-repo, or when
	// indexing a sub-repo directly as root). Since we skip sub-repos
	// during parent indexing, the SubProject field is primarily set via
	// the subRepoDirs detection. Let's test discoverSubRepoDirs and
	// subProjectForFile directly.

	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "service-a", ".git"), 0755)
	os.MkdirAll(filepath.Join(root, "service-b", ".git"), 0755)
	os.MkdirAll(filepath.Join(root, "lib"), 0755) // not a sub-repo

	embedder := &StubEmbedder{Vectors: make(map[string][]float32), Dimension: 3}
	dbDir := filepath.Join(t.TempDir(), ".heimdall_db")
	store, err := OpenStore(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	indexer := NewIndexer(root, embedder, store, ChunkerOpts{MaxChunkSize: 1500})
	_ = indexer

	subRepoDirs := DiscoverSubRepos(root)

	if !subRepoDirs["service-a"] {
		t.Error("expected service-a in subRepoDirs")
	}
	if !subRepoDirs["service-b"] {
		t.Error("expected service-b in subRepoDirs")
	}
	if subRepoDirs["lib"] {
		t.Error("lib should NOT be in subRepoDirs (no .git/)")
	}

	// Test subProjectForFile
	tests := []struct {
		relPath string
		want    string
	}{
		{"service-a/main.go", "service-a"},
		{"service-b/handler.go", "service-b"},
		{"lib/util.go", ""},
		{"main.go", ""},
		{"service-a/nested/deep.go", "service-a"},
	}
	for _, tt := range tests {
		got := subProjectForFile(tt.relPath, subRepoDirs)
		if got != tt.want {
			t.Errorf("subProjectForFile(%q) = %q, want %q", tt.relPath, got, tt.want)
		}
	}
}

// TestDiscoverSubRepos_GitlinkFile covers the git-worktree case where `.git`
// is a regular file ("gitlink") rather than a directory. The existing walker
// guarded sub-repo detection with info.IsDir(), which silently ignored
// worktrees and indexed them into the outer DB.
func TestDiscoverSubRepos_GitlinkFile(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "wt")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	// .git is a FILE (gitlink), not a directory
	if err := os.WriteFile(filepath.Join(sub, ".git"), []byte("gitdir: /some/worktrees/wt\n"), 0644); err != nil {
		t.Fatal(err)
	}
	got := DiscoverSubRepos(root)
	if !got["wt"] {
		t.Errorf("expected wt in subRepoDirs, got %v", got)
	}
}

func TestDiscoverSubRepoDirs_Empty(t *testing.T) {
	root := t.TempDir()
	embedder := &StubEmbedder{Vectors: make(map[string][]float32), Dimension: 3}
	dbDir := filepath.Join(t.TempDir(), ".heimdall_db")
	store, err := OpenStore(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	indexer := NewIndexer(root, embedder, store, ChunkerOpts{})
	_ = indexer

	subRepoDirs := DiscoverSubRepos(root)
	if len(subRepoDirs) != 0 {
		t.Errorf("expected empty subRepoDirs, got %v", subRepoDirs)
	}
}

// TestIndexResult_SkipBreakdownSums asserts the invariant that the four
// SkipBreakdown counters sum to FilesSkipped. This is the contract that lets
// the CLI render the categorized summary without a discrepancy against the
// legacy counter.
func TestIndexResult_SkipBreakdownSums(t *testing.T) {
	files := map[string]string{"a.go": "package a\n"}
	root := createTestProject(t, files)
	dbDir := filepath.Join(t.TempDir(), ".heimdall_db")
	store, err := OpenStore(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	embedder := &StubEmbedder{Vectors: make(map[string][]float32), Dimension: 3}
	idx := NewIndexer(root, embedder, store, ChunkerOpts{MaxChunkSize: 1500})
	result, err := idx.IndexAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := result.Skip.UserExcluded + result.Skip.SubRepo + result.Skip.Binary + result.Skip.Unchanged
	if got != result.FilesSkipped {
		t.Errorf("breakdown sum %d != FilesSkipped %d (Skip=%+v)", got, result.FilesSkipped, result.Skip)
	}
}

// TestIndexer_NoFileCountCap verifies there is no hard cap on the number of
// files indexed. Prior to the cap removal, indexing silently truncated at
// 5000 files, so 5001 is the minimum count that proves the cap is gone.
// Skipped in -short mode because full-pipeline indexing of 5001 files
// (stub embedder + real SQLite store) takes tens of seconds.
func TestIndexer_NoFileCountCap(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: exercises full pipeline with 5001 files")
	}
	const n = 5001
	files := make(map[string]string, n)
	for i := 0; i < n; i++ {
		files[filepath.Join("pkg", fmt.Sprintf("f%04d.go", i))] = "package main\n"
	}
	root := createTestProject(t, files)

	dbDir := filepath.Join(t.TempDir(), ".heimdall_db")
	store, err := OpenStore(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	embedder := &StubEmbedder{Vectors: make(map[string][]float32), Dimension: 3}
	indexer := NewIndexer(root, embedder, store, ChunkerOpts{MaxChunkSize: 1500})

	result, err := indexer.IndexAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.FilesScanned != n {
		t.Errorf("FilesScanned = %d, want %d (no file-count cap expected)", result.FilesScanned, n)
	}
	if result.FilesIndexed != n {
		t.Errorf("FilesIndexed = %d, want %d", result.FilesIndexed, n)
	}
}

// TestIndexer_NoFileSizeCap verifies that files larger than the former
// 100 KB cap are indexed end-to-end. The chunker handles the split.
func TestIndexer_NoFileSizeCap(t *testing.T) {
	// 200 KB of printable text — above the prior 100 KB cap, below binary
	// sniff triggers. One chunk per MaxChunkSize slice.
	const size = 200 * 1024
	body := strings.Repeat("package main\nfunc f() {}\n", size/24+1)[:size]
	files := map[string]string{"big.go": body}
	root := createTestProject(t, files)

	dbDir := filepath.Join(t.TempDir(), ".heimdall_db")
	store, err := OpenStore(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	embedder := &StubEmbedder{Vectors: make(map[string][]float32), Dimension: 3}
	indexer := NewIndexer(root, embedder, store, ChunkerOpts{MaxChunkSize: 1500})

	result, err := indexer.IndexAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.FilesScanned != 1 {
		t.Errorf("FilesScanned = %d, want 1", result.FilesScanned)
	}
	if result.FilesIndexed != 1 {
		t.Errorf("FilesIndexed = %d, want 1 (200 KB file should be indexed)", result.FilesIndexed)
	}
	// 200 KB ÷ 1500-char chunks = ~137 chunks minimum.
	if result.ChunksCreated < 100 {
		t.Errorf("ChunksCreated = %d, want >=100 for a 200 KB file at 1500-char chunks", result.ChunksCreated)
	}
}
