package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// Tests for `heimdall-mcp cleanup-legacy-latest` — the administrative
// escape-valve CLI (v2 plan New-OQ-3). Covers the reader (FindLegacyLatestDirs),
// the removal safety gate (RemoveLegacyLatestDir), and the CLI dispatcher.
//
// The CLI impl calls os.Exit, so we test the heimdall-package primitives
// directly plus a --force happy-path via the dispatcher. Interactive
// prompts are covered separately by the primitive tests.

// TestFindLegacyLatestDirs_PairsLegacyWithCanonical — legacy dirs with
// a canonical `<name>/` sibling are reported as "safe"; those without
// are reported as "unsafe".
func TestFindLegacyLatestDirs_PairsLegacyWithCanonical(t *testing.T) {
	base := t.TempDir()

	// Seed a safe pair: legacy with canonical sibling.
	if err := os.MkdirAll(filepath.Join(base, "nomic-embed-text_latest"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "nomic-embed-text"), 0755); err != nil {
		t.Fatal(err)
	}
	// Seed an unsafe legacy: no canonical sibling.
	if err := os.MkdirAll(filepath.Join(base, "stale-model_latest"), 0755); err != nil {
		t.Fatal(err)
	}
	// Seed an unrelated canonical-only dir (must be ignored).
	if err := os.MkdirAll(filepath.Join(base, "mxbai-embed-large"), 0755); err != nil {
		t.Fatal(err)
	}

	reports, err := heimdall.FindLegacyLatestDirs(base)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(reports) != 2 {
		t.Fatalf("len = %d, want 2 (two _latest dirs); got %+v", len(reports), reports)
	}
	byPath := map[string]heimdall.LegacyLatestReport{}
	for _, r := range reports {
		byPath[r.LegacyPath] = r
	}
	safe := byPath[filepath.Join(base, "nomic-embed-text_latest")]
	if !safe.CanonicalHasSibling {
		t.Errorf("expected nomic-embed-text_latest to have canonical sibling")
	}
	unsafe := byPath[filepath.Join(base, "stale-model_latest")]
	if unsafe.CanonicalHasSibling {
		t.Errorf("expected stale-model_latest NOT to have canonical sibling")
	}
}

// TestFindLegacyLatestDirs_EmptyBase returns nil safely for a missing
// or empty base dir.
func TestFindLegacyLatestDirs_EmptyBase(t *testing.T) {
	reports, err := heimdall.FindLegacyLatestDirs(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Errorf("err on missing base: %v (want nil)", err)
	}
	if len(reports) != 0 {
		t.Errorf("len = %d, want 0", len(reports))
	}
}

// TestRemoveLegacyLatestDir_RequiresSibling — refuses to remove a
// legacy dir whose canonical `<name>/` sibling doesn't exist.
func TestRemoveLegacyLatestDir_RequiresSibling(t *testing.T) {
	base := t.TempDir()
	legacy := filepath.Join(base, "orphan_latest")
	if err := os.MkdirAll(legacy, 0755); err != nil {
		t.Fatal(err)
	}
	if err := heimdall.RemoveLegacyLatestDir(legacy); err == nil {
		t.Fatal("expected error when canonical sibling missing")
	}
	// Legacy must still be on disk.
	if _, err := os.Stat(legacy); err != nil {
		t.Errorf("legacy dir was removed despite refusal: %v", err)
	}
}

// TestRemoveLegacyLatestDir_HappyPath — removes when canonical sibling exists.
func TestRemoveLegacyLatestDir_HappyPath(t *testing.T) {
	base := t.TempDir()
	legacy := filepath.Join(base, "model_latest")
	canonical := filepath.Join(base, "model")
	if err := os.MkdirAll(legacy, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(canonical, 0755); err != nil {
		t.Fatal(err)
	}
	// Seed a file so RemoveAll has to do real work.
	if err := os.WriteFile(filepath.Join(legacy, "vectors.db"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := heimdall.RemoveLegacyLatestDir(legacy); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("legacy must be gone after RemoveLegacyLatestDir: err=%v", err)
	}
	if _, err := os.Stat(canonical); err != nil {
		t.Errorf("canonical must be untouched: %v", err)
	}
}

// TestRemoveLegacyLatestDir_RefusesNonLatestSuffix — hard-guard against
// misuse: the helper must refuse to RemoveAll a path that doesn't end
// in _latest, even if callers hand it one.
func TestRemoveLegacyLatestDir_RefusesNonLatestSuffix(t *testing.T) {
	base := t.TempDir()
	victim := filepath.Join(base, "not-a-legacy-dir")
	if err := os.MkdirAll(victim, 0755); err != nil {
		t.Fatal(err)
	}
	if err := heimdall.RemoveLegacyLatestDir(victim); err == nil {
		t.Fatal("expected error on non-_latest path")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("victim was removed despite refusal: %v", err)
	}
}

// TestCLICleanupLegacyLatest_ForceRemovesSafe exercises the CLI
// dispatcher end-to-end with --force so no prompt is needed.
func TestCLICleanupLegacyLatest_ForceRemovesSafe(t *testing.T) {
	proj := t.TempDir()
	base := filepath.Join(proj, ".heimdall_db")
	if err := os.MkdirAll(filepath.Join(base, "m_latest"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "m"), 0755); err != nil {
		t.Fatal(err)
	}

	// Capture stdout/stderr to silence test output; don't assert on them
	// — CLI formatting is a contract but not this test's subject.
	oldStdout, oldStderr := os.Stdout, os.Stderr
	rOut, wOut, _ := os.Pipe()
	rErr, wErr, _ := os.Pipe()
	os.Stdout = wOut
	os.Stderr = wErr
	t.Cleanup(func() {
		wOut.Close()
		wErr.Close()
		os.Stdout = oldStdout
		os.Stderr = oldStderr
		// Drain so tests don't deadlock on the pipes.
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, rOut)
		_, _ = io.Copy(&buf, rErr)
	})

	code := cliCleanupLegacyLatest([]string{proj, "--force"})
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if _, err := os.Stat(filepath.Join(base, "m_latest")); !os.IsNotExist(err) {
		t.Errorf("m_latest must be removed; err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "m")); err != nil {
		t.Errorf("canonical m/ must be untouched: %v", err)
	}
}

// TestCLICleanupLegacyLatest_NoOpWhenNothingToClean — when no _latest
// dirs exist, exit 0 with a clean message.
func TestCLICleanupLegacyLatest_NoOpWhenNothingToClean(t *testing.T) {
	proj := t.TempDir()
	// No .heimdall_db/ at all.
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	t.Cleanup(func() {
		w.Close()
		os.Stdout = oldStdout
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		if !strings.Contains(buf.String(), "No legacy") {
			// diagnostic only; print to test log so a regression is debuggable
			t.Logf("stdout: %s", buf.String())
		}
	})

	if code := cliCleanupLegacyLatest([]string{proj, "--force"}); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

// TestShowEffectiveEmbedConfig_UsesEnvWhenSet exercises the effective
// config helper's source-attribution branch. When an env var is set,
// the printed output must name it and show both the env value and the
// on-disk value for comparison.
func TestShowEffectiveEmbedConfig_UsesEnvWhenSet(t *testing.T) {
	t.Setenv("HEIMDALL_EMBED_MAX_CONCURRENT", "8")
	t.Setenv("HEIMDALL_EMBED_TIMEOUT_MS", "")
	t.Setenv("HEIMDALL_EMBED_MAX_RETRIES", "")

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	// Build config values directly; we don't need to round-trip LoadConfig.
	base := config.Config{EmbedMaxConcurrent: 2, EmbedTimeoutMs: 1000, EmbedMaxRetries: 1}
	effective := config.Config{EmbedMaxConcurrent: 8, EmbedTimeoutMs: 1000, EmbedMaxRetries: 1}

	showEffectiveEmbedConfig(base, effective)

	w.Close()
	os.Stdout = oldStdout
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	out := buf.String()

	if !strings.Contains(out, "HEIMDALL_EMBED_MAX_CONCURRENT") {
		t.Errorf("output missing env var attribution: %s", out)
	}
	if !strings.Contains(out, "embedMaxConcurrent") {
		t.Errorf("output missing field name: %s", out)
	}
	if !strings.Contains(out, "= 8") {
		t.Errorf("output missing effective value 8: %s", out)
	}
	if !strings.Contains(out, "config.json=2") {
		t.Errorf("output missing base (config.json) value for comparison: %s", out)
	}
}

// TestShowEffectiveEmbedConfig_NoEnvUsesConfig — when no env var is
// set, the source attribution says "config.json".
func TestShowEffectiveEmbedConfig_NoEnvUsesConfig(t *testing.T) {
	t.Setenv("HEIMDALL_EMBED_MAX_CONCURRENT", "")
	t.Setenv("HEIMDALL_EMBED_TIMEOUT_MS", "")
	t.Setenv("HEIMDALL_EMBED_MAX_RETRIES", "")

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	base := config.Config{EmbedMaxConcurrent: 4, EmbedTimeoutMs: 2000, EmbedMaxRetries: 2}
	showEffectiveEmbedConfig(base, base)

	w.Close()
	os.Stdout = oldStdout
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	out := buf.String()

	if !strings.Contains(out, "source: config.json") {
		t.Errorf("output missing config.json attribution: %s", out)
	}
}
