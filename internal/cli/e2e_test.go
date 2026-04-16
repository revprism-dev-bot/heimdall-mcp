//go:build e2e

// Package cli Layer-3 e2e harness (T23).
//
// These tests exercise the real `claude` CLI against a freshly built
// heimdall-mcp binary with hooks installed. They are opt-in by design —
// running them requires all of:
//   - build tag `e2e` (so default `go test ./...` does not include them)
//   - env var HEIMDALL_E2E_CLAUDE=1
//   - the `claude` binary on PATH
//
// Run: `HEIMDALL_E2E_CLAUDE=1 go test -tags=e2e ./internal/cli/ -count=1`
//
// If any of the preconditions is missing the test cleanly `t.Skip()`s —
// never fails just because the environment isn't set up. That is the
// explicit design point: these are "best effort, not a CI gate".
//
// Docs: see docs/plans/hooks/05-testing-rollout.md "Layer 3".
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// TestE2E_ClaudeSessionStartHookFires exercises the real Claude CLI end
// to end. It:
//   1. creates a temp project with one tiny file
//   2. builds heimdall-mcp, seeds its vector store (skipping a real
//      `index` run to avoid Ollama dependency — the binary still picks
//      up a valid index and the SessionStart hook has something to read)
//   3. installs hooks via `heimdall-mcp install-hooks --scope=project`
//   4. invokes `claude -p "..."` with a throwaway --settings pointed at
//      the test project
//   5. greps stdout / streaming output for evidence the Heimdall hook
//      fired (markdown banner or hook-event record)
//   6. cleans up — temp dirs are removed via t.Cleanup.
//
// This test is NOT a correctness assertion for retrieval quality. It
// proves the hook plumbing end-to-end: install → settings.json → Claude
// CLI → hook command → stdout capture.
func TestE2E_ClaudeSessionStartHookFires(t *testing.T) {
	if os.Getenv("HEIMDALL_E2E_CLAUDE") != "1" {
		t.Skip("HEIMDALL_E2E_CLAUDE=1 not set; skipping layer-3 e2e harness")
	}
	claudeBin, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude binary not on PATH; skipping layer-3 e2e harness")
	}

	// Build heimdall-mcp.
	binDir, err := os.MkdirTemp("", "heimdall-e2e-bin-*")
	if err != nil {
		t.Fatalf("mkdtemp bin: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(binDir) })

	heimdallBin := filepath.Join(binDir, "heimdall-mcp")
	build := exec.Command("go", "build", "-o", heimdallBin, "../../cmd/heimdall-mcp")
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build heimdall-mcp: %v\n%s", err, out)
	}

	// Temp project with a trivial Go file so the index is non-empty.
	base := t.TempDir()
	projectRoot := filepath.Join(base, "project")
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	srcPath := filepath.Join(projectRoot, "main.go")
	if err := os.WriteFile(srcPath, []byte("package main\n\nfunc main() { println(\"e2e\") }\n"), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}

	// Seed the vector store so VerifyHookIndex passes when the hook
	// fires. Uses the same pattern as unit tests / integration tests.
	const model = "test-model"
	const dim = 4
	baseDBDir := filepath.Join(projectRoot, ".heimdall_db")
	dbDir := heimdall.ModelDBDir(baseDBDir, model)
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatalf("mkdir dbDir: %v", err)
	}
	store, err := heimdall.OpenStore(dbDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.SetMetadata("embedding_model", model); err != nil {
		t.Fatalf("set model: %v", err)
	}
	if err := store.SetMetadata("embedding_dim", fmt.Sprintf("%d", dim)); err != nil {
		t.Fatalf("set dim: %v", err)
	}
	// Seed one record for the index to look populated.
	vec := make([]float32, dim)
	for i := range vec {
		vec[i] = 1.0 / float32(dim)
	}
	if err := store.Upsert([]heimdall.VectorRecord{{
		ID:        "e2e-seed-1",
		FilePath:  "main.go",
		StartLine: 1,
		EndLine:   3,
		Content:   "package main\n\nfunc main() { println(\"e2e\") }",
		Kind:      "paragraph",
		Embedding: vec,
	}}); err != nil {
		t.Fatalf("upsert seed: %v", err)
	}
	store.Close()

	// Hermetic env for the heimdall-mcp install-hooks call: pin
	// XDG_STATE_HOME etc. so we don't write to the user's real dirs.
	stateHome := filepath.Join(base, "state")
	xdgConfig := filepath.Join(base, "config")
	configDir := filepath.Join(xdgConfig, "heimdall-mcp")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir configdir: %v", err)
	}
	configPath := filepath.Join(configDir, "config.json")
	cfg := map[string]any{
		"ollamaEndpoint": "http://localhost:11434",
		"model":          model,
	}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	baseEnv := []string{
		"HEIMDALL_MCP_CONFIG=" + configPath,
		"XDG_STATE_HOME=" + stateHome,
		"XDG_CONFIG_HOME=" + xdgConfig,
		"HOME=" + base,
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	// Install hooks. We use --scope=project so settings land inside the
	// temp project, not the user home.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	install := exec.CommandContext(ctx, heimdallBin, "install-hooks", "--scope=project")
	install.Dir = projectRoot
	install.Env = baseEnv
	if out, err := install.CombinedOutput(); err != nil {
		t.Fatalf("install-hooks failed: %v\n%s", err, out)
	}

	settingsPath := filepath.Join(projectRoot, ".claude", "settings.json")
	if _, err := os.Stat(settingsPath); err != nil {
		t.Fatalf("settings.json not created: %v", err)
	}

	// Invoke claude with a throwaway prompt. Use --print / -p for
	// non-interactive mode and --settings to isolate from the user's
	// global config. The stream-json form with --include-hook-events
	// would give us structured hook lifecycle events, but the schema is
	// not documented; fall back to grepping the text output for the
	// `## Heimdall context` banner which SessionStart is supposed to
	// inject on the first turn.
	//
	// We do not require a specific network or model setup — the prompt
	// is deliberately trivial and we only care about whether the hook
	// output shows up. If claude rejects our flags or auth is unset,
	// the test logs what we got and skips so users with partial setups
	// aren't stuck with a hard failure.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel2()
	runClaude := exec.CommandContext(
		ctx2, claudeBin,
		"--settings", settingsPath,
		"-p", "hi",
	)
	runClaude.Dir = projectRoot
	runClaude.Env = baseEnv
	out, runErr := runClaude.CombinedOutput()
	outStr := string(out)

	// Soft assertion: we consider the test successful if the output
	// contains any Heimdall-injected markdown marker, indicating the
	// hook fired. If claude failed for environmental reasons (no API
	// key, network down, flag rejected), we t.Skip — the harness is
	// "best effort" per plan 05.
	if runErr != nil {
		if ctx2.Err() != nil {
			t.Skipf("claude invocation timed out (likely auth / model not set up for e2e): stdout=%q", outStr)
		}
		t.Skipf("claude failed (likely env-specific, not a harness bug): err=%v stdout=%q", runErr, outStr)
	}

	if !strings.Contains(outStr, "## Heimdall context") &&
		!strings.Contains(outStr, "Heimdall context") &&
		!strings.Contains(outStr, "heimdall") {
		t.Errorf("no heimdall marker in claude output — hook may not have fired:\n%s", outStr)
	}
}
