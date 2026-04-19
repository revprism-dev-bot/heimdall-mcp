//go:build integration

// Integration test for PR1 (plan D-06 / B-3 / handoff Problem #1):
// `heimdall-mcp index <path>` must write the .heimdall_db to <path>,
// not to the binary's cwd. We exercise the real binary via os/exec
// from a different cwd, and assert:
//   - <path>/.heimdall_db exists after the run
//   - <cwd>/.heimdall_db does NOT (the binary must not write there)
//   - legacy `<model>_latest/` dirs are renamed to `<model>/` on first
//     access, unless HEIMDALL_DISABLE_LEGACY_MIGRATION=1 is set
//
// Run: `go test -tags=integration ./internal/cli/... -count=1`

package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// TestIntegration_CLIIndex_WritesToTargetPath — the handoff Problem #1
// binary-level regression test. Runs the compiled CLI from a different
// cwd against a target project and asserts the DB lands under that
// target, not under cwd.
func TestIntegration_CLIIndex_WritesToTargetPath(t *testing.T) {
	const model = "test-model"
	const dim = 4

	bin := builtBinaryPath(t)
	fake := startFakeOllama(t, model, dim)

	base := t.TempDir()
	// Target project — what `index` is told to scan.
	target := filepath.Join(base, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "sample.go"),
		[]byte("package sample\n\nfunc Foo() int { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// cwd for the subprocess — must not be where the DB ends up.
	runCwd := filepath.Join(base, "cwd")
	if err := os.MkdirAll(runCwd, 0o755); err != nil {
		t.Fatal(err)
	}

	// Isolated XDG so the test does not poison the developer's real
	// config or registry.
	xdg := filepath.Join(base, "xdg")
	if err := os.MkdirAll(filepath.Join(xdg, "heimdall-mcp"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(xdg, "heimdall-mcp", "config.json")
	cfgJSON := `{"ollamaEndpoint":` + quote(fake.server.URL) + `,"model":` + quote(model) + `}`
	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	env := []string{
		"HEIMDALL_MCP_CONFIG=" + cfgPath,
		"XDG_CONFIG_HOME=" + xdg,
		"HOME=" + base,
		"PATH=" + os.Getenv("PATH"),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "index", target, "--model", model)
	cmd.Env = env
	cmd.Dir = runCwd
	// Feed empty stdin — the non-TTY path should pick the model from the
	// --model flag without interactive input.
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cli index failed: %v\n--- output ---\n%s", err, out)
	}

	// Assertion 1: target has .heimdall_db.
	targetDB := filepath.Join(target, ".heimdall_db")
	if _, err := os.Stat(targetDB); err != nil {
		t.Errorf("target .heimdall_db missing after index: %v\noutput:\n%s", err, out)
	}

	// Assertion 2: cwd does NOT have .heimdall_db.
	cwdDB := filepath.Join(runCwd, ".heimdall_db")
	if _, err := os.Stat(cwdDB); err == nil {
		t.Errorf("cli index wrote .heimdall_db into cwd %q — target-path regressed", cwdDB)
	}

	// Assertion 3: the model-specific dir under the target is populated.
	modelDir := heimdall.ModelDBDir(targetDB, model)
	if _, err := os.Stat(filepath.Join(modelDir, "vectors.db")); err != nil {
		t.Errorf("vectors.db missing under %s: %v", modelDir, err)
	}
}

// TestIntegration_CLIIndex_LegacyLatestMigrated — if the target has a
// pre-existing `<model>_latest/` dir and no `<model>/`, the CLI must
// rename it to `<model>/` on first access so the canonical reader
// picks it up.
func TestIntegration_CLIIndex_LegacyLatestMigrated(t *testing.T) {
	const model = "test-model"
	const dim = 4

	bin := builtBinaryPath(t)
	fake := startFakeOllama(t, model, dim)

	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "a.go"),
		[]byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Pre-seed legacy dir at .heimdall_db/test-model_latest with a
	// vectors.db containing one row (so the content-aware path can see
	// data is there).
	legacyDir := filepath.Join(target, ".heimdall_db", model+"_latest")
	legacyStore, err := heimdall.OpenStore(legacyDir)
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	if err := legacyStore.Upsert([]heimdall.VectorRecord{{
		ID:        "legacy:seed",
		FilePath:  "legacy.txt",
		Content:   "legacy-seed",
		Embedding: []float32{1, 0, 0, 0},
		ModTime:   100,
	}}); err != nil {
		legacyStore.Close()
		t.Fatalf("upsert: %v", err)
	}
	legacyStore.Close()

	// Isolated XDG.
	xdg := filepath.Join(base, "xdg")
	if err := os.MkdirAll(filepath.Join(xdg, "heimdall-mcp"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(xdg, "heimdall-mcp", "config.json")
	cfgJSON := `{"ollamaEndpoint":` + quote(fake.server.URL) + `,"model":` + quote(model) + `}`
	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	env := []string{
		"HEIMDALL_MCP_CONFIG=" + cfgPath,
		"XDG_CONFIG_HOME=" + xdg,
		"HOME=" + base,
		"PATH=" + os.Getenv("PATH"),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "index", target, "--model", model)
	cmd.Env = env
	cmd.Dir = base
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cli index failed: %v\n--- output ---\n%s", err, out)
	}

	// Legacy dir must be gone.
	if _, err := os.Stat(legacyDir); !os.IsNotExist(err) {
		t.Errorf("legacy dir should be renamed away, stat err = %v\noutput:\n%s", err, out)
	}
	// Canonical dir must exist with data.
	canonical := filepath.Join(target, ".heimdall_db", model)
	if _, err := os.Stat(filepath.Join(canonical, "vectors.db")); err != nil {
		t.Errorf("canonical dir missing vectors.db: %v\noutput:\n%s", err, out)
	}
}

// quote returns a JSON-encoded string literal for use in small ad-hoc
// config blobs.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString("\\\\")
		case '"':
			b.WriteString("\\\"")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
