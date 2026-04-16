package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain redirects HEIMDALL_HOOK_LOG to a per-binary temp file so hook
// handler tests in this package don't append to the user's real
// ~/.local/state/heimdall/hooks.log. Individual tests that need a specific
// path can still override via t.Setenv — Go restores env on test end.
//
// Context: caveat #7 in docs/plans/hooks/07-next-session-handoff.md — dogfood
// `hooks tail` showed test artifacts (pid=999999, fake `context deadline
// exceeded`, `model=test-model`) polluting real user state.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "heimdall-cli-test-hooks-*")
	if err != nil {
		os.Exit(1)
	}
	_ = os.Setenv("HEIMDALL_HOOK_LOG", filepath.Join(dir, "hooks.log"))
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
