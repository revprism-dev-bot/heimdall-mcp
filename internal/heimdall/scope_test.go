package heimdall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindRepoRoot_HeimdallDB(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".heimdall_db"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sub := filepath.Join(root, "internal", "cli")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	got := FindRepoRoot(sub)
	want, _ := filepath.Abs(root)
	if got != want {
		t.Errorf("FindRepoRoot = %q, want %q", got, want)
	}
}

func TestFindRepoRoot_GitDir(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	sub := filepath.Join(root, "x", "y")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	got := FindRepoRoot(sub)
	want, _ := filepath.Abs(root)
	if got != want {
		t.Errorf("FindRepoRoot = %q, want %q", got, want)
	}
}

func TestFindRepoRoot_GitFile(t *testing.T) {
	// Worktrees use `.git` as a file (gitlink). Must still be detected.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: /nowhere\n"), 0o644); err != nil {
		t.Fatalf("write .git: %v", err)
	}
	sub := filepath.Join(root, "x")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	got := FindRepoRoot(sub)
	want, _ := filepath.Abs(root)
	if got != want {
		t.Errorf("FindRepoRoot = %q, want %q", got, want)
	}
}

func TestFindRepoRoot_PrefersInnermost(t *testing.T) {
	outer := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outer, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir outer: %v", err)
	}
	inner := filepath.Join(outer, "nested")
	if err := os.MkdirAll(filepath.Join(inner, ".heimdall_db"), 0o755); err != nil {
		t.Fatalf("mkdir inner hd: %v", err)
	}
	sub := filepath.Join(inner, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	got := FindRepoRoot(sub)
	want, _ := filepath.Abs(inner)
	if got != want {
		t.Errorf("FindRepoRoot = %q, want %q", got, want)
	}
}

func TestFindRepoRoot_EmptyInput(t *testing.T) {
	if got := FindRepoRoot(""); got != "" {
		t.Errorf("FindRepoRoot(\"\") = %q, want empty", got)
	}
}

func TestComputeScope_Subpath(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "internal", "cli")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	got := ComputeScope(sub, root)
	if got != "internal/cli" {
		t.Errorf("ComputeScope = %q, want internal/cli", got)
	}
}

func TestComputeScope_Equal(t *testing.T) {
	root := t.TempDir()
	if got := ComputeScope(root, root); got != "" {
		t.Errorf("ComputeScope equal = %q, want empty", got)
	}
}

func TestComputeScope_Outside(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	got := ComputeScope(other, root)
	if got != "" {
		t.Errorf("ComputeScope outside = %q, want empty", got)
	}
	if strings.HasPrefix(got, "..") {
		t.Errorf("ComputeScope leaked ..: %q", got)
	}
}

func TestComputeScope_Empty(t *testing.T) {
	if got := ComputeScope("", "/x"); got != "" {
		t.Errorf("empty cwd = %q, want empty", got)
	}
	if got := ComputeScope("/x", ""); got != "" {
		t.Errorf("empty root = %q, want empty", got)
	}
}
