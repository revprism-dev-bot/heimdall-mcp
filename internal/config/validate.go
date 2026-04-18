package config

import (
	"fmt"
	"path/filepath"
)

// ValidateExcludePatterns enforces the canonical form for exclude-pattern
// input from both the CLI (`--exclude`) and the MCP (`heimdall_configure
// set exclude_patterns`) surfaces: every entry must be a bare glob or a
// project-relative path, never an absolute path.
//
// Rejecting absolute paths at the boundary keeps the indexer's matching
// semantics predictable — filepath.Match on an absolute pattern against a
// relative path would match trivial inputs (like "/foo/bar" never matching
// the component "foo") while confusingly appearing to be a real filter.
// See plan §6 (M6 resolution).
func ValidateExcludePatterns(patterns []string) error {
	for i, p := range patterns {
		if p == "" {
			return fmt.Errorf("exclude_patterns[%d] is empty", i)
		}
		if filepath.IsAbs(p) {
			return fmt.Errorf("exclude_patterns[%d] %q is absolute; use a glob or project-relative path", i, p)
		}
	}
	return nil
}
