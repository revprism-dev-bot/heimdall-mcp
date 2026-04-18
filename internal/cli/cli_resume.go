package cli

import "os"

// isStdinTTY reports whether os.Stdin is attached to a TTY. Used by
// cliIndex to decide between the interactive multi-select and the
// script-friendly resolution path in resolveIndexModels. Kept as a
// package-level helper (no extra dep) because `os.Stat` + the
// ModeCharDevice bit is the standard-library idiom and matches the rest of
// the repo.
//
// The resume flow (on-disk marker) now threads through resolveIndexModels
// directly — see internal/cli/discover.go and the tests in
// discover_test.go. The older [C]/[R]/[Q] prompt was removed in favour of
// a single multi-select whose pre-checked options carry the marker's
// recorded models with an "(in progress — resume)" annotation.
func isStdinTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	// A character device with no Setgid/Setuid bits is our proxy for a
	// TTY. Uses the standard library so we don't add a new dependency.
	return (fi.Mode() & os.ModeCharDevice) != 0
}
