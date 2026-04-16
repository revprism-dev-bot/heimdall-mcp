package heimdall

import (
	"os"
	"path/filepath"
	"strings"
)

// FindRepoRoot walks upward from startDir searching for a recognizable repo
// root marker — a `.heimdall_db/` directory (preferred) or a `.git/` entry
// (dir or file — `.git` may be a gitlink in worktrees). Returns the absolute
// path to the first ancestor containing either marker, or an empty string
// when none is found (e.g. startDir is outside any repo).
//
// The walk stops at the filesystem root. The loop is bounded at a generous
// depth to avoid any theoretical infinite loop from a pathological
// filesystem. Callers on the retrieval-hook / memory-storage path must
// tolerate an empty return and fall back to their existing behavior.
func FindRepoRoot(startDir string) string {
	if startDir == "" {
		return ""
	}
	abs, err := filepath.Abs(startDir)
	if err != nil {
		return ""
	}
	const maxDepth = 256
	dir := abs
	for i := 0; i < maxDepth; i++ {
		if hasRepoMarker(dir) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}

// hasRepoMarker reports whether dir contains a `.heimdall_db` directory or a
// `.git` entry (either directory or file — `.git` can be a gitlink in
// worktrees). Missing directories are not an error.
func hasRepoMarker(dir string) bool {
	if info, err := os.Stat(filepath.Join(dir, ".heimdall_db")); err == nil && info.IsDir() {
		return true
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return true
	}
	return false
}

// ComputeScope returns the slash-normalized relative path from repoRoot to
// cwd, or an empty string when:
//   - cwd == repoRoot (no narrowing)
//   - cwd is outside repoRoot (defensive: drop the scope rather than return
//     something like "../foo" that context_path prefixes would never match)
//   - either argument is empty or a Rel computation fails
//
// The result is exactly what retrieval hooks should pass to WithScope /
// MemoryFilter.ContextPath, and what `heimdall_remember` should store as
// the Memory.ContextPath.
func ComputeScope(cwd, repoRoot string) string {
	if cwd == "" || repoRoot == "" {
		return ""
	}
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return ""
	}
	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return ""
	}
	rel, err := filepath.Rel(absRoot, absCwd)
	if err != nil {
		return ""
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || rel == "" {
		return ""
	}
	if strings.HasPrefix(rel, "..") {
		return ""
	}
	return rel
}
