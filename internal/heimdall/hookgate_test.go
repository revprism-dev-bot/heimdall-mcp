package heimdall

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHooksDisabled_EnvKillSwitch(t *testing.T) {
	dir := t.TempDir() // real project dir, no marker
	if !HooksDisabled(dir, map[string]string{"HEIMDALL_HOOKS": "0"}) {
		t.Error("HEIMDALL_HOOKS=0 should disable")
	}
}

func TestHooksDisabled_EnvNotZeroDoesNotDisable(t *testing.T) {
	dir := t.TempDir()
	cases := []string{"1", "true", "", "yes", "0 "} // "0 " with space is not exactly "0"
	for _, v := range cases {
		if HooksDisabled(dir, map[string]string{"HEIMDALL_HOOKS": v}) {
			t.Errorf("HEIMDALL_HOOKS=%q should NOT disable", v)
		}
	}
}

func TestHooksDisabled_MarkerFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".heimdall"), 0o755); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(dir, ".heimdall", "hooks.disabled")
	if err := os.WriteFile(markerPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !HooksDisabled(dir, nil) {
		t.Error("marker file should disable")
	}
	// Trailing slash on projectRoot — must still work.
	if !HooksDisabled(dir+"/", nil) {
		t.Error("marker file + trailing slash should disable")
	}
}

func TestHooksDisabled_BothEnvAndMarker(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".heimdall"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".heimdall", "hooks.disabled"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !HooksDisabled(dir, map[string]string{"HEIMDALL_HOOKS": "0"}) {
		t.Error("both env+marker should disable")
	}
}

func TestHooksDisabled_Neither(t *testing.T) {
	dir := t.TempDir()
	if HooksDisabled(dir, nil) {
		t.Error("empty state should NOT disable")
	}
	if HooksDisabled(dir, map[string]string{"HEIMDALL_HOOKS": "1"}) {
		t.Error("env=1 should NOT disable")
	}
}

func TestHooksDisabled_NonExistentProjectDirReturnsFalse(t *testing.T) {
	// Missing dir ≠ disabled (§5.7).
	if HooksDisabled("/definitely/does/not/exist/xyzzy-plugh", nil) {
		t.Error("missing dir should NOT disable")
	}
}

func TestHooksDisabled_EmptyProjectRoot(t *testing.T) {
	if HooksDisabled("", map[string]string{"HEIMDALL_HOOKS": "0"}) != true {
		t.Error("env=0 still disables even with empty root")
	}
	if HooksDisabled("", nil) {
		t.Error("empty root alone does not disable")
	}
}

// BenchmarkHooksDisabled exercises the fast-false path (no env, no marker) —
// the hot-path shape every hook incurs on every fire. Target: <1 µs on a
// warm stat. Reported via `go test -bench .`.
func BenchmarkHooksDisabled(b *testing.B) {
	dir := b.TempDir()
	env := map[string]string{} // no kill switch
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if HooksDisabled(dir, env) {
			b.Fatal("unexpected disable")
		}
	}
}

func BenchmarkHooksDisabled_EnvShortCircuit(b *testing.B) {
	env := map[string]string{"HEIMDALL_HOOKS": "0"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !HooksDisabled("/tmp/some-project", env) {
			b.Fatal("unexpected enable")
		}
	}
}
