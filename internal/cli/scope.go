package cli

import (
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// findRepoRoot is a package-local alias for heimdall.FindRepoRoot. Kept as
// a thin wrapper so call sites in this package read naturally alongside
// unexported helpers; the implementation lives in the heimdall package so
// it can be shared with internal/mcp (toolRemember auto-detects the same
// way retrieval hooks do).
func findRepoRoot(startDir string) string {
	return heimdall.FindRepoRoot(startDir)
}

// computeScope is a package-local alias for heimdall.ComputeScope.
func computeScope(cwd, repoRoot string) string {
	return heimdall.ComputeScope(cwd, repoRoot)
}
