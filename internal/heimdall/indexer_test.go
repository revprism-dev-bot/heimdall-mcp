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

// TestIndexAll_SubRepoFilesNotInOuterStore verifies that immediate sub-repos
// (directories containing a .git entry) are NOT merged into the outer store.
// Previously named TestIndexAll_SkipsSubRepoDirectories — renamed because
// the plan now indexes sub-repos as separate projects (see IndexSubRepos).
// The outer-store contract is unchanged: outer files land here, sub-repo
// files do not. Also asserts Skip.SubRepo counts discovered sub-repo
// directories (2) and that default-hygiene (.git inside each sub-repo) does
// NOT leak into UserExcluded.
func TestIndexAll_SubRepoFilesNotInOuterStore(t *testing.T) {
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

	// NEW: sub-repo discovery is surfaced on the result.
	if result.Skip.SubRepo != 2 {
		t.Errorf("Skip.SubRepo = %d, want 2 (sub-service, sub-infra)", result.Skip.SubRepo)
	}
	if len(result.SubRepos) != 2 {
		t.Errorf("SubRepos = %v, want 2 entries", result.SubRepos)
	}
	// INVARIANT: default-excluded hygiene dirs DO NOT bump UserExcluded.
	if result.Skip.UserExcluded != 0 {
		t.Errorf("Skip.UserExcluded = %d, want 0 (no user patterns configured)", result.Skip.UserExcluded)
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

// TestIndexResult_SkipBinaryCounted asserts the walker bumps Skip.Binary
// when the NUL-byte sniff trips, not the generic FilesSkipped counter alone.
func TestIndexResult_SkipBinaryCounted(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "bin.dat"), []byte{0x00, 0x01, 0x02, 0x03}, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ok.go"), []byte("package a\n"), 0644); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "db"))
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
	if result.Skip.Binary != 1 {
		t.Errorf("Skip.Binary = %d, want 1 (result=%+v)", result.Skip.Binary, result)
	}
	if result.FilesIndexed != 1 {
		t.Errorf("FilesIndexed = %d, want 1 (only ok.go)", result.FilesIndexed)
	}
}

// TestIndexResult_SkipUnchangedCounted asserts the incremental path bumps
// Skip.Unchanged (not just FilesSkipped) so the summary can label it.
func TestIndexResult_SkipUnchangedCounted(t *testing.T) {
	files := map[string]string{
		"a.go": "package main\nfunc a() {}",
		"b.go": "package main\nfunc b() {}",
	}
	root := createTestProject(t, files)
	store, err := OpenStore(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	embedder := &StubEmbedder{Vectors: make(map[string][]float32), Dimension: 3}
	idx := NewIndexer(root, embedder, store, ChunkerOpts{MaxChunkSize: 1500})
	if _, err := idx.IndexAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := idx.IndexIncremental(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Skip.Unchanged != 2 {
		t.Errorf("Skip.Unchanged = %d, want 2", result.Skip.Unchanged)
	}
	if result.Skip.UserExcluded != 0 || result.Skip.SubRepo != 0 || result.Skip.Binary != 0 {
		t.Errorf("other counters should be 0, got %+v", result.Skip)
	}
}

// TestIndexResult_UserExcludedCountsOnlyUserPatterns is the H1 regression —
// only user-configured patterns bump Skip.UserExcluded; default hygiene
// (.git, node_modules) stays silent.
func TestIndexResult_UserExcludedCountsOnlyUserPatterns(t *testing.T) {
	root := t.TempDir()
	// User pattern: "generated" — files under generated/ should be counted.
	if err := os.MkdirAll(filepath.Join(root, "generated"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "generated", "x.go"), []byte("package g\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Default-excluded: node_modules — must NOT bump the counter.
	if err := os.MkdirAll(filepath.Join(root, "node_modules"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "node_modules", "y.go"), []byte("package y\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Regular file.
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n"), 0644); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	embedder := &StubEmbedder{Vectors: make(map[string][]float32), Dimension: 3}
	idx := NewIndexer(root, embedder, store, ChunkerOpts{
		MaxChunkSize: 1500,
		ExcludeGlobs: []string{"generated"},
	})
	result, err := idx.IndexAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Skip.UserExcluded != 1 {
		t.Errorf("Skip.UserExcluded = %d, want 1 (only 'generated' dir)", result.Skip.UserExcluded)
	}
	if result.FilesIndexed != 1 {
		t.Errorf("FilesIndexed = %d, want 1 (just a.go)", result.FilesIndexed)
	}
}

// TestExclude_DefaultHygieneNotCounted is the H1 regression test: a fixture
// containing only default-excluded dirs + one regular file must produce
// FilesSkipped == 0 (matches TestIndexAll_DoesNotSkip semantics).
func TestExclude_DefaultHygieneNotCounted(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("[core]\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "node_modules"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "node_modules", "a.js"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n"), 0644); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "db"))
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
	if result.Skip.UserExcluded != 0 {
		t.Errorf("Skip.UserExcluded = %d, want 0 (only default hygiene)", result.Skip.UserExcluded)
	}
	if result.Skip.SubRepo != 0 {
		t.Errorf("Skip.SubRepo = %d, want 0 (.git is default-exclude, not sub-repo)", result.Skip.SubRepo)
	}
	if result.FilesSkipped != 0 {
		t.Errorf("FilesSkipped = %d, want 0 (default hygiene is silent)", result.FilesSkipped)
	}
	if result.FilesIndexed != 1 {
		t.Errorf("FilesIndexed = %d, want 1 (just a.go)", result.FilesIndexed)
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

// TestDiscoverSubReposAbs_ReturnsAbsolutePaths exercises the plan's §4
// helper: scan immediate subdirectories and return absolute paths of those
// containing a `.git` entry (dir OR file).
func TestDiscoverSubReposAbs_ReturnsAbsolutePaths(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "b", ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	// Not a sub-repo — plain directory.
	if err := os.MkdirAll(filepath.Join(root, "plain"), 0755); err != nil {
		t.Fatal(err)
	}
	got, err := DiscoverSubReposAbs(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2 (%v)", len(got), got)
	}
	for _, p := range got {
		if !filepath.IsAbs(p) {
			t.Errorf("expected absolute path, got %q", p)
		}
	}
}

// TestDiscoverSubReposAbs_ReadErrorPropagates is the L1 regression: an
// unreadable root must return an error rather than a silent empty slice.
func TestDiscoverSubReposAbs_ReadErrorPropagates(t *testing.T) {
	_, err := DiscoverSubReposAbs("/definitely/not/a/real/path/abc123xyz")
	if err == nil {
		t.Fatal("expected error for nonexistent root, got nil")
	}
}

// TestDiscoverSubReposAbs_IgnoresSymlinks is the M4 regression: symlinks to
// git repos are NOT auto-discovered (matches git, fd, ripgrep defaults and
// avoids double-indexing when `outer-a/child -> outer-b/child`).
func TestDiscoverSubReposAbs_IgnoresSymlinks(t *testing.T) {
	realRepo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(realRepo, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Symlink(realRepo, filepath.Join(root, "linked-sub")); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}
	got, err := DiscoverSubReposAbs(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 discovered (symlinks ignored), got %v", got)
	}
}

// TestDiscoverSubReposAbs_GitlinkFile mirrors the gitlink test for the new
// helper: worktrees must be detected.
func TestDiscoverSubReposAbs_GitlinkFile(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "wt")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, ".git"), []byte("gitdir: /some/worktrees/wt\n"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := DiscoverSubReposAbs(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d, want 1 (%v)", len(got), got)
	}
	if filepath.Base(got[0]) != "wt" {
		t.Errorf("got %q, want basename 'wt'", got[0])
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

// TestIndexSubRepos_CreatesSeparateDBs verifies that the sub-repo pass
// creates a distinct .heimdall_db under each immediate sub-repo.
func TestIndexSubRepos_CreatesSeparateDBs(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "child")
	if err := os.MkdirAll(filepath.Join(sub, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "c.go"), []byte("package c\n"), 0644); err != nil {
		t.Fatal(err)
	}

	outerStore, err := OpenStore(filepath.Join(t.TempDir(), "outer-db"))
	if err != nil {
		t.Fatal(err)
	}
	defer outerStore.Close()

	embedder := &StubEmbedder{Vectors: make(map[string][]float32), Dimension: 3}
	idx := NewIndexer(root, embedder, outerStore, ChunkerOpts{MaxChunkSize: 1500})
	if _, err := idx.IndexAll(context.Background()); err != nil {
		t.Fatal(err)
	}

	subResults, err := idx.IndexSubRepos(context.Background(), "nomic-embed-text", SubRepoOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(subResults) != 1 {
		t.Fatalf("got %d sub-results, want 1", len(subResults))
	}
	subDB := filepath.Join(sub, ".heimdall_db", "nomic-embed-text", "vectors.db")
	if _, err := os.Stat(subDB); err != nil {
		t.Errorf("sub DB not created at %s: %v", subDB, err)
	}
	sr := subResults[0]
	if sr.Err != nil {
		t.Errorf("sub %s: unexpected Err = %v", sr.Path, sr.Err)
	}
	if sr.Result == nil {
		t.Fatalf("sub %s: expected non-nil Result", sr.Path)
	}
	if sr.Result.FilesIndexed != 1 {
		t.Errorf("sub %s: FilesIndexed = %d, want 1", sr.Path, sr.Result.FilesIndexed)
	}
	if sr.Name != "child" {
		t.Errorf("sub Name = %q, want 'child'", sr.Name)
	}
}

// TestIndexSubRepos_OuterWrapperStillIndexed is the G1 regression: the outer
// DB contains the outer files and NONE of the sub-repo files; the sub-repo
// DB contains only its own files.
func TestIndexSubRepos_OuterWrapperStillIndexed(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pkg", "lib.go"), []byte("package pkg\n"), 0644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "child")
	if err := os.MkdirAll(filepath.Join(sub, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "c.go"), []byte("package c\n"), 0644); err != nil {
		t.Fatal(err)
	}

	outerStore, err := OpenStore(filepath.Join(t.TempDir(), "outer-db"))
	if err != nil {
		t.Fatal(err)
	}
	defer outerStore.Close()

	embedder := &StubEmbedder{Vectors: make(map[string][]float32), Dimension: 3}
	idx := NewIndexer(root, embedder, outerStore, ChunkerOpts{MaxChunkSize: 1500})
	if _, err := idx.IndexAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !outerStore.HasFile("main.go") {
		t.Error("outer store missing main.go")
	}
	if !outerStore.HasFile("pkg/lib.go") {
		t.Error("outer store missing pkg/lib.go")
	}
	if outerStore.HasFile("child/c.go") {
		t.Error("outer store must NOT contain child/c.go (sub-repo contents)")
	}

	// Sub-repo pass: opens a fresh store under child/.heimdall_db.
	if _, err := idx.IndexSubRepos(context.Background(), "nomic-embed-text", SubRepoOpts{}); err != nil {
		t.Fatal(err)
	}
	subStore, err := OpenStore(filepath.Join(sub, ".heimdall_db", "nomic-embed-text"))
	if err != nil {
		t.Fatal(err)
	}
	defer subStore.Close()
	if !subStore.HasFile("c.go") {
		t.Error("sub store missing c.go")
	}
	if subStore.HasFile("main.go") {
		t.Error("sub store must NOT contain main.go (outer contents)")
	}
}

// TestIndexSubRepos_PinnedModelNotClobbered is the G4 rule 1 regression:
// if a sub-repo already has an index under a different model, that pinned
// model is preserved and the caller's requested model is ignored.
func TestIndexSubRepos_PinnedModelNotClobbered(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "child")
	if err := os.MkdirAll(filepath.Join(sub, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "c.go"), []byte("package c\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Pre-create a bge-m3 store to pin the sub-repo to that model.
	pinnedDir := filepath.Join(sub, ".heimdall_db", "bge-m3")
	if err := os.MkdirAll(pinnedDir, 0755); err != nil {
		t.Fatal(err)
	}
	pinnedStore, err := OpenStore(pinnedDir)
	if err != nil {
		t.Fatal(err)
	}
	pinnedStore.Close()

	outerStore, err := OpenStore(filepath.Join(t.TempDir(), "outer-db"))
	if err != nil {
		t.Fatal(err)
	}
	defer outerStore.Close()
	embedder := &StubEmbedder{Vectors: make(map[string][]float32), Dimension: 3}
	idx := NewIndexer(root, embedder, outerStore, ChunkerOpts{MaxChunkSize: 1500})

	subResults, err := idx.IndexSubRepos(context.Background(), "nomic-embed-text", SubRepoOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(subResults) != 1 {
		t.Fatalf("got %d sub-results, want 1", len(subResults))
	}
	if subResults[0].Model != "bge-m3" {
		t.Errorf("Model = %q, want 'bge-m3' (pinned beats caller)", subResults[0].Model)
	}
	if !subResults[0].PinnedModel {
		t.Errorf("PinnedModel = false, want true (store pre-existed)")
	}
	// The caller's nomic-embed-text store must NOT have been created.
	if _, err := os.Stat(filepath.Join(sub, ".heimdall_db", "nomic-embed-text")); err == nil {
		t.Error("nomic-embed-text dir should NOT exist (pinned bge-m3 won)")
	}
}

// TestIndexSubRepos_ExcludedSubRepoSkipped verifies that a sub-repo matching
// a user-exclude pattern is NOT indexed separately (and thus no .heimdall_db
// is created under it).
func TestIndexSubRepos_ExcludedSubRepoSkipped(t *testing.T) {
	root := t.TempDir()
	subA := filepath.Join(root, "sub-a")
	subB := filepath.Join(root, "sub-b")
	for _, s := range []string{subA, subB} {
		if err := os.MkdirAll(filepath.Join(s, ".git"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s, "f.go"), []byte("package x\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	outerStore, err := OpenStore(filepath.Join(t.TempDir(), "outer-db"))
	if err != nil {
		t.Fatal(err)
	}
	defer outerStore.Close()
	embedder := &StubEmbedder{Vectors: make(map[string][]float32), Dimension: 3}
	idx := NewIndexer(root, embedder, outerStore, ChunkerOpts{
		MaxChunkSize: 1500,
		ExcludeGlobs: []string{"sub-b"},
	})

	subResults, err := idx.IndexSubRepos(context.Background(), "nomic-embed-text", SubRepoOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(subResults) != 1 {
		t.Fatalf("got %d sub-results, want 1 (sub-a only)", len(subResults))
	}
	if subResults[0].Name != "sub-a" {
		t.Errorf("sub Name = %q, want 'sub-a'", subResults[0].Name)
	}
	if _, err := os.Stat(filepath.Join(subB, ".heimdall_db")); err == nil {
		t.Error("sub-b/.heimdall_db should NOT exist (excluded by user)")
	}
}

// TestIndexSubRepos_NestedSubRepoInSubRepo covers edge case 6: a sub-repo
// that itself contains another sub-repo. The orchestrator is expected to
// recurse logically — once IndexSubRepos is called on the outer, the
// sub-repo indexer will see its own child sub-repos when its caller (the
// orchestrator) runs IndexSubRepos on it. This test exercises the direct
// recursion step: calling IndexSubRepos at level 1 leaves level-2 to be
// picked up by the orchestrator, so we verify only that the level-1
// sub-repo's own outer store has been populated but the level-2 sub-repo's
// files are NOT merged into level-1.
func TestIndexSubRepos_NestedSubRepoInSubRepo(t *testing.T) {
	root := t.TempDir()
	subA := filepath.Join(root, "a")
	if err := os.MkdirAll(filepath.Join(subA, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subA, "a.go"), []byte("package a\n"), 0644); err != nil {
		t.Fatal(err)
	}
	subB := filepath.Join(subA, "b")
	if err := os.MkdirAll(filepath.Join(subB, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subB, "b.go"), []byte("package b\n"), 0644); err != nil {
		t.Fatal(err)
	}
	outerStore, err := OpenStore(filepath.Join(t.TempDir(), "outer-db"))
	if err != nil {
		t.Fatal(err)
	}
	defer outerStore.Close()
	embedder := &StubEmbedder{Vectors: make(map[string][]float32), Dimension: 3}
	idx := NewIndexer(root, embedder, outerStore, ChunkerOpts{MaxChunkSize: 1500})
	subResults, err := idx.IndexSubRepos(context.Background(), "nomic-embed-text", SubRepoOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(subResults) != 1 {
		t.Fatalf("got %d sub-results at level 1, want 1 (sub-a only)", len(subResults))
	}
	// The sub-a store exists and holds a.go but NOT b/b.go (skip-dir fired).
	subAStore, err := OpenStore(filepath.Join(subA, ".heimdall_db", "nomic-embed-text"))
	if err != nil {
		t.Fatal(err)
	}
	defer subAStore.Close()
	if !subAStore.HasFile("a.go") {
		t.Error("sub-a store missing a.go")
	}
	if subAStore.HasFile("b/b.go") {
		t.Error("sub-a store must NOT contain b/b.go (b is itself a sub-repo)")
	}
	// level-1 result should record the inner sub-repo discovery so an
	// orchestrator can recurse.
	if len(subResults[0].Result.SubRepos) != 1 {
		t.Errorf("sub-a Result.SubRepos = %v, want 1 entry (b)", subResults[0].Result.SubRepos)
	}
}

// TestSubRepoResult_NilResultOnStoreOpenFailure is the L2 regression: if the
// store cannot be opened, the SubRepoResult has Err != nil and Result == nil.
// Callers must gate on Err before dereffing Result.
func TestSubRepoResult_NilResultOnStoreOpenFailure(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "child")
	if err := os.MkdirAll(filepath.Join(sub, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "c.go"), []byte("package c\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Put a regular FILE where OpenStore wants to create a directory. The
	// model subdir join (<sub>/.heimdall_db/<model>) will fail when mkdirall
	// hits this file, yielding a store-open error.
	if err := os.WriteFile(filepath.Join(sub, ".heimdall_db"), []byte("blocker"), 0644); err != nil {
		t.Fatal(err)
	}
	outerStore, err := OpenStore(filepath.Join(t.TempDir(), "outer-db"))
	if err != nil {
		t.Fatal(err)
	}
	defer outerStore.Close()
	embedder := &StubEmbedder{Vectors: make(map[string][]float32), Dimension: 3}
	idx := NewIndexer(root, embedder, outerStore, ChunkerOpts{MaxChunkSize: 1500})
	subResults, err := idx.IndexSubRepos(context.Background(), "nomic-embed-text", SubRepoOpts{})
	if err != nil {
		t.Fatalf("discovery-level err = %v; expected nil (failures are per-entry)", err)
	}
	if len(subResults) != 1 {
		t.Fatalf("got %d sub-results, want 1", len(subResults))
	}
	r := subResults[0]
	if r.Err == nil {
		t.Error("expected Err != nil for store-open failure")
	}
	if r.Result != nil {
		t.Errorf("Result must be nil on store-open failure, got %+v", r.Result)
	}
	if r.Path == "" || r.Name == "" || r.DBPath == "" {
		t.Errorf("Path/Name/DBPath must be populated even on failure: %+v", r)
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

// TestIndexAll_SubReposOnlyImmediateChildrenRecorded is the M2 regression:
// the outer walker previously recorded any dir with a `.git` inside at
// ARBITRARY depth, but DiscoverSubReposAbs/IndexSubRepos only handle
// immediate children. The mismatch meant a deeply-nested repo (e.g.
// outer/services/payments/.git) appeared in result.SubRepos /
// Skip.SubRepo but was never actually indexed as a separate project.
//
// Fix: the walker now records only IMMEDIATE sub-repos of idx.root,
// matching DiscoverSubReposAbs. Sub-repos deeper than one level are
// still skipped by filepath.SkipDir (so their files do NOT leak into
// the outer store) but are not reported in result.SubRepos — the
// orchestrator never claimed to handle them and the plan's "sub-repo-
// inside-sub-repo" guarantee only kicks in when each intermediate is
// itself a sub-repo.
func TestIndexAll_SubReposOnlyImmediateChildrenRecorded(t *testing.T) {
	root := t.TempDir()

	// Outer file at root.
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Deep sub-repo: outer/services/payments/.git
	deep := filepath.Join(root, "services", "payments")
	if err := os.MkdirAll(filepath.Join(deep, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "p.go"), []byte("package p\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Immediate sub-repo: outer/libx/.git
	shallow := filepath.Join(root, "libx")
	if err := os.MkdirAll(filepath.Join(shallow, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shallow, "x.go"), []byte("package x\n"), 0644); err != nil {
		t.Fatal(err)
	}

	outerStore, err := OpenStore(filepath.Join(t.TempDir(), "outer-db"))
	if err != nil {
		t.Fatal(err)
	}
	defer outerStore.Close()

	embedder := &StubEmbedder{Vectors: make(map[string][]float32), Dimension: 3}
	idx := NewIndexer(root, embedder, outerStore, ChunkerOpts{MaxChunkSize: 1500})
	result, err := idx.IndexAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// Only the IMMEDIATE sub-repo should be recorded. services/payments
	// (depth 2) must NOT appear — the orchestrator cannot index it
	// anyway, and surfacing it in the summary would be misleading.
	if len(result.SubRepos) != 1 {
		t.Fatalf("SubRepos = %v, want exactly 1 entry (immediate child 'libx'); deeper repos must not be recorded",
			result.SubRepos)
	}
	if result.SubRepos[0] != "libx" {
		t.Errorf("SubRepos[0] = %q, want 'libx'", result.SubRepos[0])
	}

	// Skip.SubRepo counter must match len(SubRepos) — invariant that keeps
	// the CLI summary renderer consistent.
	if result.Skip.SubRepo != 1 {
		t.Errorf("Skip.SubRepo = %d, want 1", result.Skip.SubRepo)
	}

	// Sanity: neither sub-repo's files leaked into the outer store.
	if outerStore.HasFile("services/payments/p.go") {
		t.Error("outer store must NOT contain services/payments/p.go (deep sub-repo files)")
	}
	if outerStore.HasFile("libx/x.go") {
		t.Error("outer store must NOT contain libx/x.go (shallow sub-repo files)")
	}

	// And the outer file IS indexed.
	if !outerStore.HasFile("main.go") {
		t.Error("outer store missing main.go")
	}
}

// TestIndexSubRepos_StampsEmbeddingDim is the H2 regression test:
// IndexSubRepos MUST stamp both embedding_model AND embedding_dim on the
// sub-repo store, matching what the outer indexer (cli.go:352-354 and
// tools.go:336-340) does for the outer store.
//
// Without embedding_dim, VerifyHookIndexDim rejects the store with
// ErrIndexDimMismatch, breaking the hook path for any search scoped to a
// sub-repo.
func TestIndexSubRepos_StampsEmbeddingDim(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "child")
	if err := os.MkdirAll(filepath.Join(sub, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "c.go"), []byte("package c\n"), 0644); err != nil {
		t.Fatal(err)
	}

	outerStore, err := OpenStore(filepath.Join(t.TempDir(), "outer-db"))
	if err != nil {
		t.Fatal(err)
	}
	defer outerStore.Close()

	const dim = 5
	embedder := &StubEmbedder{Vectors: make(map[string][]float32), Dimension: dim}
	idx := NewIndexer(root, embedder, outerStore, ChunkerOpts{MaxChunkSize: 1500})

	subResults, err := idx.IndexSubRepos(context.Background(), "test-model", SubRepoOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(subResults) != 1 {
		t.Fatalf("got %d sub-results, want 1", len(subResults))
	}
	if subResults[0].Err != nil {
		t.Fatalf("sub-repo failed: %v", subResults[0].Err)
	}

	// Open the sub-repo store and verify both metadata fields are populated.
	subDBDir := ModelDBDir(subResults[0].DBPath, "test-model")
	subStore, err := OpenStore(subDBDir)
	if err != nil {
		t.Fatalf("open sub-store: %v", err)
	}
	defer subStore.Close()

	if got := subStore.GetMetadata("embedding_model"); got != "test-model" {
		t.Errorf("embedding_model = %q, want %q", got, "test-model")
	}
	gotDim := subStore.GetMetadata("embedding_dim")
	if gotDim == "" {
		t.Fatalf("embedding_dim metadata missing — hook verification will reject this store")
	}
	wantDim := fmt.Sprintf("%d", dim)
	if gotDim != wantDim {
		t.Errorf("embedding_dim = %q, want %q", gotDim, wantDim)
	}

	// Cross-check by invoking VerifyHookIndexDim directly — this is the
	// real consumer that was failing before the fix.
	if err := VerifyHookIndexDim(subStore, dim); err != nil {
		t.Errorf("VerifyHookIndexDim(sub-store, %d) = %v, want nil", dim, err)
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

// --- sub-repo incremental + progress regression tests ---

// buildSubRepoFixture builds a fixture with an outer repo and a single
// sub-repo "child" containing N go files. Returns the outer root path.
func buildSubRepoFixture(t *testing.T, childFiles int) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "child")
	if err := os.MkdirAll(filepath.Join(sub, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < childFiles; i++ {
		name := fmt.Sprintf("f%d.go", i)
		body := fmt.Sprintf("package c\nfunc F%d() {}\n", i)
		if err := os.WriteFile(filepath.Join(sub, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestIndexSubRepos_IncrementalByDefault is the primary regression for
// PR #67: sub-repo indexing used to call IndexAll, re-embedding every file
// on every run. The sub-repo pass MUST be incremental — a second run over
// unchanged files indexes ZERO files.
func TestIndexSubRepos_IncrementalByDefault(t *testing.T) {
	root := buildSubRepoFixture(t, 3)

	outerStore, err := OpenStore(filepath.Join(t.TempDir(), "outer-db"))
	if err != nil {
		t.Fatal(err)
	}
	defer outerStore.Close()
	embedder := &StubEmbedder{Vectors: make(map[string][]float32), Dimension: 3}
	idx := NewIndexer(root, embedder, outerStore, ChunkerOpts{MaxChunkSize: 1500})

	// First pass: sub-repo store is empty — all files get indexed.
	first, err := idx.IndexSubRepos(context.Background(), "nomic-embed-text", SubRepoOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Err != nil || first[0].Result == nil {
		t.Fatalf("first pass: unexpected results %+v", first)
	}
	if first[0].Result.FilesIndexed != 3 {
		t.Fatalf("first pass: FilesIndexed = %d, want 3", first[0].Result.FilesIndexed)
	}

	// Second pass: nothing changed on disk. Incremental contract says we
	// MUST skip all 3 files as unchanged and index zero.
	second, err := idx.IndexSubRepos(context.Background(), "nomic-embed-text", SubRepoOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].Err != nil || second[0].Result == nil {
		t.Fatalf("second pass: unexpected results %+v", second)
	}
	if got := second[0].Result.FilesIndexed; got != 0 {
		t.Errorf("second pass: FilesIndexed = %d, want 0 (incremental should skip unchanged)", got)
	}
	if got := second[0].Result.Skip.Unchanged; got != 3 {
		t.Errorf("second pass: Skip.Unchanged = %d, want 3 (all files unchanged)", got)
	}
}

// TestIndexSubRepos_InvokesOnSubRepoStartCallback asserts the callback
// fires before each sub-repo with the correct name, index (1-based), and
// total, in order.
func TestIndexSubRepos_InvokesOnSubRepoStartCallback(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Two sub-repos — discovered in lexical order by filepath.WalkDir.
	for _, name := range []string{"alpha", "beta"} {
		sub := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Join(sub, ".git"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, "c.go"), []byte("package c\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	outerStore, err := OpenStore(filepath.Join(t.TempDir(), "outer-db"))
	if err != nil {
		t.Fatal(err)
	}
	defer outerStore.Close()
	embedder := &StubEmbedder{Vectors: make(map[string][]float32), Dimension: 3}
	idx := NewIndexer(root, embedder, outerStore, ChunkerOpts{MaxChunkSize: 1500})

	type startEvent struct {
		name       string
		idx, total int
	}
	var events []startEvent
	opts := SubRepoOpts{
		OnSubRepoStart: func(name string, i, total int) {
			events = append(events, startEvent{name, i, total})
		},
	}
	if _, err := idx.IndexSubRepos(context.Background(), "nomic-embed-text", opts); err != nil {
		t.Fatal(err)
	}

	if len(events) != 2 {
		t.Fatalf("got %d start events, want 2: %+v", len(events), events)
	}
	want := []startEvent{
		{"alpha", 1, 2},
		{"beta", 2, 2},
	}
	for i, e := range events {
		if e != want[i] {
			t.Errorf("event[%d] = %+v, want %+v", i, e, want[i])
		}
	}
}

// TestIndexSubRepos_InvokesOnSubRepoDoneCallback asserts OnSubRepoDone
// fires after each sub-repo with the matching SubRepoResult.
func TestIndexSubRepos_InvokesOnSubRepoDoneCallback(t *testing.T) {
	root := buildSubRepoFixture(t, 2)

	outerStore, err := OpenStore(filepath.Join(t.TempDir(), "outer-db"))
	if err != nil {
		t.Fatal(err)
	}
	defer outerStore.Close()
	embedder := &StubEmbedder{Vectors: make(map[string][]float32), Dimension: 3}
	idx := NewIndexer(root, embedder, outerStore, ChunkerOpts{MaxChunkSize: 1500})

	var doneEvents []SubRepoResult
	opts := SubRepoOpts{
		OnSubRepoDone: func(res SubRepoResult) {
			doneEvents = append(doneEvents, res)
		},
	}
	results, err := idx.IndexSubRepos(context.Background(), "nomic-embed-text", opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("results: got %d, want 1", len(results))
	}
	if len(doneEvents) != 1 {
		t.Fatalf("done events: got %d, want 1", len(doneEvents))
	}
	if doneEvents[0].Name != results[0].Name ||
		doneEvents[0].Path != results[0].Path ||
		doneEvents[0].Err != results[0].Err {
		t.Errorf("done event mismatch: got %+v, want %+v", doneEvents[0], results[0])
	}
	if doneEvents[0].Result == nil || doneEvents[0].Result.FilesIndexed != 2 {
		t.Errorf("done event Result: got %+v, want FilesIndexed=2", doneEvents[0].Result)
	}
}

// TestIndexSubRepos_OnProgressForwardsEvents asserts that when
// OnProgress is set, per-file progress events from the sub-repo's
// indexFiles pass are forwarded to the caller.
func TestIndexSubRepos_OnProgressForwardsEvents(t *testing.T) {
	root := buildSubRepoFixture(t, 3)

	outerStore, err := OpenStore(filepath.Join(t.TempDir(), "outer-db"))
	if err != nil {
		t.Fatal(err)
	}
	defer outerStore.Close()
	embedder := &StubEmbedder{Vectors: make(map[string][]float32), Dimension: 3}
	idx := NewIndexer(root, embedder, outerStore, ChunkerOpts{MaxChunkSize: 1500})

	var progress []IndexProgress
	opts := SubRepoOpts{
		OnProgress: func(p IndexProgress) {
			progress = append(progress, p)
		},
	}
	if _, err := idx.IndexSubRepos(context.Background(), "nomic-embed-text", opts); err != nil {
		t.Fatal(err)
	}
	if len(progress) == 0 {
		t.Fatalf("OnProgress received zero events; expected per-file progress updates")
	}
	// The final non-done event's Current should match the total file count.
	last := progress[len(progress)-1]
	if last.Total != 3 {
		t.Errorf("last progress Total = %d, want 3", last.Total)
	}
	if last.Current != 3 {
		t.Errorf("last progress Current = %d, want 3", last.Current)
	}
}

// TestIndexSubRepos_NilCallbacksAreSafe is the back-compat regression:
// passing an empty SubRepoOpts (all-nil callbacks) must behave exactly
// as before — no panics, sub-repo gets indexed.
func TestIndexSubRepos_NilCallbacksAreSafe(t *testing.T) {
	root := buildSubRepoFixture(t, 1)

	outerStore, err := OpenStore(filepath.Join(t.TempDir(), "outer-db"))
	if err != nil {
		t.Fatal(err)
	}
	defer outerStore.Close()
	embedder := &StubEmbedder{Vectors: make(map[string][]float32), Dimension: 3}
	idx := NewIndexer(root, embedder, outerStore, ChunkerOpts{MaxChunkSize: 1500})

	results, err := idx.IndexSubRepos(context.Background(), "nomic-embed-text", SubRepoOpts{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 || results[0].Err != nil || results[0].Result == nil {
		t.Fatalf("results: %+v", results)
	}
	if results[0].Result.FilesIndexed != 1 {
		t.Errorf("FilesIndexed = %d, want 1", results[0].Result.FilesIndexed)
	}
}
