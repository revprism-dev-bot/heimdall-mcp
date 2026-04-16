package heimdall

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestParseGitLogOutput_BasicParsing(t *testing.T) {
	output := `abc123def456789abcdef0123456789abcdef0123
1700000000
feat: add user authentication

Implements JWT-based auth with refresh tokens.
Adds middleware for protected routes.
---COMMIT_END---
def456789abcdef0123456789abcdef0123456789
1699999000
fix: correct null pointer in handler
---COMMIT_END---`

	commits := parseGitLogOutput(output)
	if len(commits) != 2 {
		t.Fatalf("expected 2 commits, got %d", len(commits))
	}

	// First commit
	if commits[0].Hash != "abc123def456789abcdef0123456789abcdef0123" {
		t.Errorf("commit[0].Hash = %q", commits[0].Hash)
	}
	if commits[0].Subject != "feat: add user authentication" {
		t.Errorf("commit[0].Subject = %q", commits[0].Subject)
	}
	if commits[0].Body == "" {
		t.Error("commit[0].Body should not be empty")
	}
	if commits[0].Timestamp != 1700000000 {
		t.Errorf("commit[0].Timestamp = %d, want 1700000000", commits[0].Timestamp)
	}

	// Second commit — no body
	if commits[1].Hash != "def456789abcdef0123456789abcdef0123456789" {
		t.Errorf("commit[1].Hash = %q", commits[1].Hash)
	}
	if commits[1].Subject != "fix: correct null pointer in handler" {
		t.Errorf("commit[1].Subject = %q", commits[1].Subject)
	}
	if commits[1].Body != "" {
		t.Errorf("commit[1].Body should be empty, got %q", commits[1].Body)
	}
}

func TestParseGitLogOutput_EmptyInput(t *testing.T) {
	commits := parseGitLogOutput("")
	if commits != nil {
		t.Error("expected nil for empty input")
	}

	commits = parseGitLogOutput("   \n  ")
	if commits != nil {
		t.Error("expected nil for whitespace-only input")
	}
}

func TestParseGitLogOutput_MalformedInput(t *testing.T) {
	// Only one line per block — should be skipped
	output := `abc123
---COMMIT_END---`

	commits := parseGitLogOutput(output)
	if len(commits) != 0 {
		t.Errorf("expected 0 commits from malformed input, got %d", len(commits))
	}
}

func TestParseGitLogOutput_EmptySubjectSkipped(t *testing.T) {
	output := `abc123def456789abcdef0123456789abcdef0123
1700000000

---COMMIT_END---`

	commits := parseGitLogOutput(output)
	if len(commits) != 0 {
		t.Errorf("expected 0 commits when subject is empty, got %d", len(commits))
	}
}

func TestIndexGitCommits_Integration(t *testing.T) {
	// Create a temp git repo with some commits
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "testrepo")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Initialize git repo
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test",
			"GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@test.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}

	runGit("init")
	runGit("checkout", "-b", "main")

	// Create commits
	os.WriteFile(filepath.Join(repoDir, "a.txt"), []byte("file a"), 0644)
	runGit("add", "a.txt")
	runGit("commit", "-m", "feat: initial commit\n\nSets up the project structure.")

	os.WriteFile(filepath.Join(repoDir, "b.txt"), []byte("file b"), 0644)
	runGit("add", "b.txt")
	runGit("commit", "-m", "fix: add missing file")

	os.WriteFile(filepath.Join(repoDir, "c.txt"), []byte("file c"), 0644)
	runGit("add", "c.txt")
	runGit("commit", "-m", "docs: add documentation")

	// Set up store and mock embedder
	dbDir := filepath.Join(tmpDir, ".heimdall_db")
	store, err := OpenStore(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	embedder := &MockEmbedder{
		Vectors:   make(map[string][]float32),
		Dimension: 3,
	}

	ctx := context.Background()
	result, err := IndexGitCommits(ctx, repoDir, 10, embedder, store)
	if err != nil {
		t.Fatal(err)
	}

	if result.CommitsIndexed != 3 {
		t.Errorf("expected 3 commits indexed, got %d", result.CommitsIndexed)
	}
	if result.ChunksCreated != 3 {
		t.Errorf("expected 3 chunks created, got %d", result.ChunksCreated)
	}

	// Verify records in store
	stats := store.Stats()
	if stats.TotalRecords != 3 {
		t.Errorf("expected 3 records in store, got %d", stats.TotalRecords)
	}

	// Verify source_type filtering works
	allResults := store.SearchFiltered(context.Background(), make([]float32, 3), 0, "commit", "", nil)
	if len(allResults) != 3 {
		t.Errorf("expected 3 commit results, got %d", len(allResults))
	}

	// Verify record structure
	for _, r := range allResults {
		if r.Record.FilePath != "git:commit" {
			t.Errorf("expected FilePath 'git:commit', got %q", r.Record.FilePath)
		}
		if r.Record.Kind != "commit" {
			t.Errorf("expected Kind 'commit', got %q", r.Record.Kind)
		}
		if r.Record.SourceType != "commit" {
			t.Errorf("expected SourceType 'commit', got %q", r.Record.SourceType)
		}
	}

	// Verify embedder was called 3 times
	if embedder.CallCount != 3 {
		t.Errorf("expected 3 embedder calls, got %d", embedder.CallCount)
	}
}

func TestIndexGitCommits_ZeroDepth(t *testing.T) {
	// Create a temp git repo
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "testrepo")
	os.MkdirAll(repoDir, 0755)

	cmd := exec.Command("git", "init")
	cmd.Dir = repoDir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test",
		"GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@test.com",
	)
	cmd.Run()

	cmd = exec.Command("git", "checkout", "-b", "main")
	cmd.Dir = repoDir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test",
		"GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@test.com",
	)
	cmd.Run()

	os.WriteFile(filepath.Join(repoDir, "a.txt"), []byte("a"), 0644)

	cmd = exec.Command("git", "add", ".")
	cmd.Dir = repoDir
	cmd.Run()

	cmd = exec.Command("git", "commit", "-m", "initial")
	cmd.Dir = repoDir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test",
		"GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@test.com",
	)
	cmd.Run()

	dbDir := filepath.Join(tmpDir, ".heimdall_db")
	store, err := OpenStore(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	embedder := &MockEmbedder{Vectors: make(map[string][]float32), Dimension: 3}

	// depth=0 should default to 200
	result, err := IndexGitCommits(context.Background(), repoDir, 0, embedder, store)
	if err != nil {
		t.Fatal(err)
	}
	if result.CommitsIndexed != 1 {
		t.Errorf("expected 1 commit, got %d", result.CommitsIndexed)
	}
}

func TestIndexGitCommits_NotAGitRepo(t *testing.T) {
	tmpDir := t.TempDir()
	dbDir := filepath.Join(tmpDir, ".heimdall_db")
	store, err := OpenStore(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	embedder := &MockEmbedder{Vectors: make(map[string][]float32), Dimension: 3}

	_, err = IndexGitCommits(context.Background(), tmpDir, 10, embedder, store)
	if err == nil {
		t.Error("expected error when not a git repo")
	}
}
