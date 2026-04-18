package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveDBDir_ExplicitFlagWinsOverAutoDetect verifies that when the user
// passes --db=<path>, we use it verbatim and do NOT walk up for a repo root.
// This keeps script / CI usage (which always pins an explicit DB) stable.
func TestResolveDBDir_ExplicitFlagWinsOverAutoDetect(t *testing.T) {
	// Fixture: a repo root with its own .heimdall_db/<model>/vectors.db that
	// would be auto-picked if we let FindRepoRoot run.
	repo := t.TempDir()
	repoDB := filepath.Join(repo, ".heimdall_db", "nomic-embed-text")
	if err := os.MkdirAll(repoDB, 0o755); err != nil {
		t.Fatalf("mkdir repo db: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDB, "vectors.db"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write repo vectors.db: %v", err)
	}

	// And an explicit DB elsewhere that the user passed via --db.
	explicit := t.TempDir()
	if err := os.WriteFile(filepath.Join(explicit, "vectors.db"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write explicit vectors.db: %v", err)
	}

	got, err := resolveDBDir(explicit, repo, "nomic-embed-text")
	if err != nil {
		t.Fatalf("resolveDBDir: %v", err)
	}
	if got != explicit {
		t.Errorf("resolveDBDir returned %q, want explicit override %q", got, explicit)
	}
}

// TestResolveDBDir_ExplicitFlagMissingVectorsDB surfaces a clear error when
// the caller-supplied path doesn't contain a vectors.db file. The bench
// binary can't read such a path, so we must refuse rather than open a broken
// store later.
func TestResolveDBDir_ExplicitFlagMissingVectorsDB(t *testing.T) {
	explicit := t.TempDir() // no vectors.db inside
	_, err := resolveDBDir(explicit, "", "nomic-embed-text")
	if err == nil {
		t.Fatalf("expected error for missing vectors.db, got nil")
	}
	if !strings.Contains(err.Error(), "vectors.db") {
		t.Errorf("error %q should mention vectors.db", err.Error())
	}
}

// TestResolveDBDir_AutoDetectFromCWD verifies that with no --db flag, we walk
// up from cwd via heimdall.FindRepoRoot, resolve the model-specific subdir
// via heimdall.ModelDBDir, and return that path when it contains vectors.db.
func TestResolveDBDir_AutoDetectFromCWD(t *testing.T) {
	repo := t.TempDir()
	modelDir := filepath.Join(repo, ".heimdall_db", "nomic-embed-text")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "vectors.db"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write vectors.db: %v", err)
	}

	// Run auto-detect from a nested subdirectory to mirror real CWDs.
	nested := filepath.Join(repo, "internal", "heimdall")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}

	got, err := resolveDBDir("", nested, "nomic-embed-text")
	if err != nil {
		t.Fatalf("resolveDBDir: %v", err)
	}
	// Resolve symlinks on both sides — on macOS /tmp is a symlink to /private/tmp
	// which breaks equality checks.
	wantAbs, _ := filepath.EvalSymlinks(modelDir)
	gotAbs, _ := filepath.EvalSymlinks(got)
	if gotAbs != wantAbs {
		t.Errorf("resolveDBDir = %q, want %q", gotAbs, wantAbs)
	}
}

// TestResolveDBDir_AutoDetectHandlesLatestSuffix verifies that ModelDBDir's
// backward-compat check (the `<model>_latest` legacy directory name) is
// honored by auto-detect. Ollama occasionally reports the model as
// "name:latest" and the indexer may have stored the index under that suffix.
func TestResolveDBDir_AutoDetectHandlesLatestSuffix(t *testing.T) {
	repo := t.TempDir()
	modelDir := filepath.Join(repo, ".heimdall_db", "nomic-embed-text_latest")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "vectors.db"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write vectors.db: %v", err)
	}

	got, err := resolveDBDir("", repo, "nomic-embed-text")
	if err != nil {
		t.Fatalf("resolveDBDir: %v", err)
	}
	wantAbs, _ := filepath.EvalSymlinks(modelDir)
	gotAbs, _ := filepath.EvalSymlinks(got)
	if gotAbs != wantAbs {
		t.Errorf("resolveDBDir = %q, want %q", gotAbs, wantAbs)
	}
}

// TestResolveDBDir_NoRepoRoot_ClearError ensures that when CWD is outside
// any repo (no .heimdall_db or .git marker up the chain), we emit an error
// that tells the user exactly how to recover — pass --db explicitly.
func TestResolveDBDir_NoRepoRoot_ClearError(t *testing.T) {
	// A plain tempdir with no .heimdall_db / .git markers above it. In CI
	// this should be enough; on dev machines t.TempDir() sits under a path
	// that typically has no repo markers up to /, but we guard below.
	orphan := t.TempDir()

	_, err := resolveDBDir("", orphan, "nomic-embed-text")
	if err == nil {
		// If this ever trips on a dev machine because /tmp happens to live
		// inside a repo root, skip rather than flake — the error path is
		// what we're actually testing.
		t.Skip("test tempdir appears to be inside a repo — cannot exercise no-repo-root branch here")
	}
	msg := err.Error()
	// The message must mention --db so the user knows the fix.
	if !strings.Contains(msg, "--db") {
		t.Errorf("error %q should mention --db flag as the fix", msg)
	}
}

// TestResolveDBDir_RepoRootButNoIndex verifies the "found a repo but no
// indexed model" case — FindRepoRoot returns a path (e.g. a git repo that
// has never run heimdall-mcp index), but there's no vectors.db to benchmark
// against. The error must be actionable.
func TestResolveDBDir_RepoRootButNoIndex(t *testing.T) {
	repo := t.TempDir()
	// Make it a repo root (marker: .git file) but don't create .heimdall_db.
	if err := os.WriteFile(filepath.Join(repo, ".git"), []byte("gitdir: /nowhere\n"), 0o644); err != nil {
		t.Fatalf("write .git: %v", err)
	}

	_, err := resolveDBDir("", repo, "nomic-embed-text")
	if err == nil {
		t.Fatalf("expected error for repo root without index, got nil")
	}
	msg := err.Error()
	// Should mention either the model name or the missing vectors.db so the
	// user knows the index is missing, not that the flag itself is wrong.
	if !(strings.Contains(msg, "nomic-embed-text") || strings.Contains(msg, "vectors.db") || strings.Contains(msg, "index")) {
		t.Errorf("error %q should explain that no index exists for the model", msg)
	}
}

// TestResolveDBDir_RepoRootModelMismatch covers the case where the repo has
// an index for model A but the bench was asked for model B. Must fail with
// a message that names what IS available so the user can pick the right
// --model or --db.
func TestResolveDBDir_RepoRootModelMismatch(t *testing.T) {
	repo := t.TempDir()
	presentModel := filepath.Join(repo, ".heimdall_db", "bge-small")
	if err := os.MkdirAll(presentModel, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(presentModel, "vectors.db"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write vectors.db: %v", err)
	}

	_, err := resolveDBDir("", repo, "nomic-embed-text")
	if err == nil {
		t.Fatalf("expected error for model mismatch, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "bge-small") {
		t.Errorf("error %q should list the available model %q", msg, "bge-small")
	}
}
