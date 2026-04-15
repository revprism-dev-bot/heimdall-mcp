package heimdall

import (
	"os"
)

// hookDisabledMarkerSuffix is appended to the project root to locate the
// per-project disable marker. Kept as a constant so the hot path can avoid
// filepath.Join allocations — see HooksDisabled below.
const hookDisabledMarkerSuffix = "/.heimdall/hooks.disabled"

// HooksDisabled reports whether heimdall hooks should short-circuit for this
// project+environment. It is called as the very first line of every hook
// handler and must be allocation-light and faster than a single os.Stat on
// the warm-cache fast path.
//
// Check order (short-circuits in this order — §5.7):
//  1. env["HEIMDALL_HOOKS"] == "0"  → true (global kill switch)
//  2. <projectRoot>/.heimdall/hooks.disabled exists → true
//  3. Otherwise → false
//
// A missing or unreadable project directory returns false (missing project
// dir is not the same as "disabled").
func HooksDisabled(projectRoot string, env map[string]string) bool {
	// (1) Global kill switch. Checked first — cheapest and most common way
	// to disable hooks across all projects in a session.
	if env != nil {
		if v, ok := env["HEIMDALL_HOOKS"]; ok && v == "0" {
			return true
		}
	}

	// (2) Per-project marker. Build the path without filepath.Join to avoid
	// an allocation on the hot path — this function is called on every hook
	// fire including UserPromptSubmit.
	if projectRoot == "" {
		return false
	}
	// Trim a trailing slash so the concat produces exactly one separator.
	// We avoid strings.TrimRight to keep zero allocations on the warm path.
	n := len(projectRoot)
	if projectRoot[n-1] == '/' {
		n--
	}
	// The concatenation below allocates once — unavoidable unless we cache
	// resolved marker paths per project, which we explicitly don't want
	// (the marker is meant to be toggled at runtime).
	markerPath := projectRoot[:n] + hookDisabledMarkerSuffix
	if _, err := os.Stat(markerPath); err == nil {
		return true
	}
	return false
}
