package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// testEnv builds a minimal env map that pins HOME (and HEIMDALL_TEST_HOME)
// at a temp path so install/uninstall never touches the real ~/.claude.
func testEnv(t *testing.T, home string) map[string]string {
	t.Helper()
	return map[string]string{
		"HOME":                 home,
		"HEIMDALL_TEST_HOME":   home,
		"HEIMDALL_TEST_CWD":    home, // no .heimdall_db here → forces user scope
		"XDG_STATE_HOME":       filepath.Join(home, "state"),
		"PATH":                 os.Getenv("PATH"),
	}
}

// readJSON loads a settings.json file into a map for assertions.
func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("parse %s: %v\ncontent:\n%s", path, err, b)
	}
	return m
}

// hookEvents returns the event keys present under "hooks".
func hookEvents(m map[string]any) []string {
	hooksAny, ok := m["hooks"].(map[string]any)
	if !ok {
		return nil
	}
	out := []string{}
	for k := range hooksAny {
		out = append(out, k)
	}
	return out
}

// ----- T14: install-hooks -----

func TestInstallHooks_HappyPath_EmptySettings(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	var stdout, stderr bytes.Buffer

	code := CLIInstallHooks(config.Config{}, nil, &stdout, &stderr, env, []string{"--scope=user"})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	if _, err := os.Stat(settingsPath); err != nil {
		t.Fatalf("settings.json not created: %v", err)
	}
	m := readJSON(t, settingsPath)
	events := hookEvents(m)
	// Phase 1b adds UserPromptSubmit alongside the phase-1a SessionStart +
	// PostToolUse. Keep the assertion in sync with len(phase1aHooks).
	if len(events) != len(phase1aHooks) {
		t.Errorf("expected %d hook events, got %d: %v", len(phase1aHooks), len(events), events)
	}
	if !strings.Contains(stdout.String(), fmt.Sprintf("Installed %d hooks", len(phase1aHooks))) {
		t.Errorf("missing success line in stdout: %s", stdout.String())
	}
}

func TestInstallHooks_DryRunDoesNotWrite(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	var stdout, stderr bytes.Buffer

	code := CLIInstallHooks(config.Config{}, nil, &stdout, &stderr, env, []string{"--scope=user", "--dry-run"})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	if _, err := os.Stat(settingsPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("dry-run should not have created settings.json (err=%v)", err)
	}
	if !strings.Contains(stdout.String(), "+ ") {
		t.Errorf("expected diff lines in dry-run output: %s", stdout.String())
	}
}

func TestInstallHooks_BackupCreatedOnRewrite(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"unrelated":"keep"}` + "\n")
	if err := os.WriteFile(settingsPath, original, 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := CLIInstallHooks(config.Config{}, nil, &stdout, &stderr, env, []string{"--scope=user"})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	matches, _ := filepath.Glob(settingsPath + ".heimdall-backup-*")
	if len(matches) != 1 {
		t.Fatalf("expected exactly one backup, got %d: %v", len(matches), matches)
	}
	backupBytes, _ := os.ReadFile(matches[0])
	if !bytes.Equal(backupBytes, original) {
		t.Errorf("backup contents differ from original; got %s", backupBytes)
	}
	// Make sure the unrelated key survived.
	m := readJSON(t, settingsPath)
	if m["unrelated"] != "keep" {
		t.Errorf("unrelated top-level key lost: %v", m)
	}
}

func TestInstallHooks_ConflictRefusesWithoutFlag(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	conflicting := map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{"type": "command", "command": "/usr/local/bin/other-tool"},
					},
				},
			},
		},
	}
	b, _ := json.MarshalIndent(conflicting, "", "  ")
	os.WriteFile(settingsPath, b, 0o600)

	var stdout, stderr bytes.Buffer
	code := CLIInstallHooks(config.Config{}, nil, &stdout, &stderr, env, []string{"--scope=user"})
	if code != 1 {
		t.Fatalf("expected exit 1 on conflict, got %d (stderr=%s)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "conflict on event") {
		t.Errorf("expected conflict message, got: %s", stderr.String())
	}
	// File must not have been touched.
	current, _ := os.ReadFile(settingsPath)
	if !bytes.Equal(current, b) {
		t.Errorf("settings.json was modified despite conflict refusal")
	}
}

func TestInstallHooks_MergeAppendsAlongsideForeign(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(settingsPath), 0o755)
	conflicting := map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{"type": "command", "command": "/usr/local/bin/other-tool"},
					},
				},
			},
		},
	}
	b, _ := json.MarshalIndent(conflicting, "", "  ")
	os.WriteFile(settingsPath, b, 0o600)

	var stdout, stderr bytes.Buffer
	code := CLIInstallHooks(config.Config{}, nil, &stdout, &stderr, env, []string{"--scope=user", "--merge"})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	m := readJSON(t, settingsPath)
	hooks := m["hooks"].(map[string]any)
	ss := hooks["SessionStart"].([]any)
	if len(ss) != 2 {
		t.Errorf("expected SessionStart to have 2 entries (foreign + heimdall), got %d", len(ss))
	}
}

func TestInstallHooks_ForceReplacesForeign(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(settingsPath), 0o755)
	conflicting := map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{"type": "command", "command": "/usr/local/bin/other-tool"},
					},
				},
			},
		},
	}
	b, _ := json.MarshalIndent(conflicting, "", "  ")
	os.WriteFile(settingsPath, b, 0o600)

	var stdout, stderr bytes.Buffer
	code := CLIInstallHooks(config.Config{}, nil, &stdout, &stderr, env, []string{"--scope=user", "--force"})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	m := readJSON(t, settingsPath)
	ss := m["hooks"].(map[string]any)["SessionStart"].([]any)
	if len(ss) != 1 {
		t.Errorf("expected SessionStart to have 1 entry after --force, got %d", len(ss))
	}
	if !isHeimdallEntry(ss[0].(map[string]any)) {
		t.Errorf("remaining entry should be heimdall: %v", ss[0])
	}
	if !strings.Contains(stdout.String(), "Replaced") {
		t.Errorf("expected Replaced summary in stdout: %s", stdout.String())
	}
}

func TestInstallHooks_SameVersionNoop(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)

	var out1, err1 bytes.Buffer
	if code := CLIInstallHooks(config.Config{}, nil, &out1, &err1, env, []string{"--scope=user"}); code != 0 {
		t.Fatalf("first install failed: %d %s", code, err1.String())
	}

	var out2, err2 bytes.Buffer
	code := CLIInstallHooks(config.Config{}, nil, &out2, &err2, env, []string{"--scope=user"})
	if code != 0 {
		t.Fatalf("second install exit=%d stderr=%s", code, err2.String())
	}
	if !strings.Contains(out2.String(), "Already installed") {
		t.Errorf("expected idempotent message, got: %s", out2.String())
	}
}

func TestInstallHooks_UpgradeFromOlderVersion(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(settingsPath), 0o755)

	older := map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{
					"source":  "heimdall",
					"version": float64(0),
					"hooks": []any{
						map[string]any{"type": "command", "command": "heimdall-mcp hook session-start --source=heimdall --version=0"},
					},
				},
			},
		},
	}
	b, _ := json.MarshalIndent(older, "", "  ")
	os.WriteFile(settingsPath, b, 0o600)

	var stdout, stderr bytes.Buffer
	code := CLIInstallHooks(config.Config{}, nil, &stdout, &stderr, env, []string{"--scope=user"})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Upgraded") {
		t.Errorf("expected upgrade message, got: %s", stdout.String())
	}
	m := readJSON(t, settingsPath)
	ss := m["hooks"].(map[string]any)["SessionStart"].([]any)
	first := ss[0].(map[string]any)
	if entryVersion(first) != heimdallHookVersion {
		t.Errorf("entry version not upgraded: %v", first["version"])
	}
}

func TestInstallHooks_OnlyRestrictsToOneEvent(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	var stdout, stderr bytes.Buffer

	code := CLIInstallHooks(config.Config{}, nil, &stdout, &stderr, env, []string{"--scope=user", "--only=SessionStart"})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	m := readJSON(t, settingsPath)
	hooks := m["hooks"].(map[string]any)
	if _, ok := hooks["SessionStart"]; !ok {
		t.Errorf("SessionStart missing")
	}
	if _, ok := hooks["PostToolUse"]; ok {
		t.Errorf("PostToolUse should not be installed under --only")
	}
}

func TestInstallHooks_OnlyMatchesNothing(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	var stdout, stderr bytes.Buffer
	code := CLIInstallHooks(config.Config{}, nil, &stdout, &stderr, env, []string{"--scope=user", "--only=Nonexistent"})
	if code != 2 {
		t.Fatalf("expected exit 2, got %d", code)
	}
}

func TestInstallHooks_InvalidJSONInSettings(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(settingsPath), 0o755)
	os.WriteFile(settingsPath, []byte("{not json"), 0o600)

	var stdout, stderr bytes.Buffer
	code := CLIInstallHooks(config.Config{}, nil, &stdout, &stderr, env, []string{"--scope=user"})
	if code != 1 {
		t.Fatalf("expected exit 1 on bad JSON, got %d", code)
	}
	if !strings.Contains(stderr.String(), "not valid JSON") {
		t.Errorf("expected JSON-error message, got: %s", stderr.String())
	}
}

func TestInstallHooks_BadFlagReturnsUsage(t *testing.T) {
	env := testEnv(t, t.TempDir())
	var stdout, stderr bytes.Buffer
	code := CLIInstallHooks(config.Config{}, nil, &stdout, &stderr, env, []string{"--nope"})
	if code != 2 {
		t.Errorf("expected 2, got %d", code)
	}
}

func TestInstallHooks_MergeAndForceMutuallyExclusive(t *testing.T) {
	env := testEnv(t, t.TempDir())
	var stdout, stderr bytes.Buffer
	code := CLIInstallHooks(config.Config{}, nil, &stdout, &stderr, env, []string{"--merge", "--force"})
	if code != 2 {
		t.Errorf("expected 2, got %d", code)
	}
}

func TestInstallHooks_WriteDeniedDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses chmod 0o444")
	}
	home := t.TempDir()
	claudeDir := filepath.Join(home, ".claude")
	os.MkdirAll(claudeDir, 0o755)
	if err := os.Chmod(claudeDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(claudeDir, 0o755) })
	env := testEnv(t, home)
	var stdout, stderr bytes.Buffer
	code := CLIInstallHooks(config.Config{}, nil, &stdout, &stderr, env, []string{"--scope=user"})
	if code != 1 {
		t.Errorf("expected 1 on permission-denied, got %d (stderr=%s)", code, stderr.String())
	}
}

// TestInstallHooks_SessionEndHasTimeout pins the regression fix for the
// 1500 ms SessionEnd SIGTERM: the SessionEnd hook entry must carry an explicit
// `timeout` on its inner command, overriding Claude Code's default budget.
// Any refactor that drops this field will re-break the nag loop silently.
// See docs/plans/claude-heimdall-self-use/04-session-end-regression-handoff.md.
func TestInstallHooks_SessionEndHasTimeout(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	var stdout, stderr bytes.Buffer

	if code := CLIInstallHooks(config.Config{}, nil, &stdout, &stderr, env, []string{"--scope=user"}); code != 0 {
		t.Fatalf("install exit=%d stderr=%s", code, stderr.String())
	}

	m := readJSON(t, filepath.Join(home, ".claude", "settings.json"))
	hooks := m["hooks"].(map[string]any)
	seList := hooks["SessionEnd"].([]any)
	if len(seList) == 0 {
		t.Fatal("SessionEnd list is empty")
	}
	entry := seList[0].(map[string]any)
	inner := entry["hooks"].([]any)
	if len(inner) == 0 {
		t.Fatal("SessionEnd inner hooks empty")
	}
	cmd := inner[0].(map[string]any)

	// The timeout lands on the inner command object. JSON round-trips numeric
	// fields through float64, hence the cast.
	raw, ok := cmd["timeout"]
	if !ok {
		t.Fatalf("SessionEnd inner command missing timeout field: %v", cmd)
	}
	got, ok := raw.(float64)
	if !ok {
		t.Fatalf("timeout must be numeric, got %T: %v", raw, raw)
	}
	if int(got) != sessionEndTimeoutSeconds {
		t.Errorf("want timeout=%d seconds, got %v", sessionEndTimeoutSeconds, got)
	}

	// Sanity: other events shouldn't have grown a spurious timeout field.
	for event, rawList := range hooks {
		if event == "SessionEnd" {
			continue
		}
		for _, raw := range rawList.([]any) {
			obj := raw.(map[string]any)
			for _, h := range obj["hooks"].([]any) {
				inner := h.(map[string]any)
				if _, has := inner["timeout"]; has {
					t.Errorf("%s entry unexpectedly carries timeout: %v", event, inner)
				}
			}
		}
	}
}

// TestAutoUpgradeHooks_RefreshesStaleVersion covers the drift-heal path:
// an install from a previous binary version (e.g. wave2-phase3) gets
// rewritten to the current template when HookSessionStart fires. Without
// this, bumping heimdallBinaryVersion would only affect fresh installs and
// existing users would stay on the buggy shape forever.
func TestAutoUpgradeHooks_RefreshesStaleVersion(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}

	// Seed a complete install under an older heimdall_version, WITHOUT the
	// timeout field on SessionEnd — the exact shape that shipped before
	// wave2-phase4.
	stale := map[string]any{
		"hooks": map[string]any{},
	}
	staleHooks := stale["hooks"].(map[string]any)
	for _, h := range phase1aHooks {
		entry := map[string]any{
			"source":  "heimdall",
			"version": float64(heimdallHookVersion),
			"x-heimdall": map[string]any{
				"installed_at":     "2026-04-16T20:43:00Z",
				"heimdall_version": "wave2-phase3", // stale
			},
			"hooks": []any{
				map[string]any{"type": "command", "command": h.command},
			},
		}
		if h.matcher != "" {
			entry["matcher"] = h.matcher
		}
		staleHooks[h.event] = []any{entry}
	}
	b, _ := json.MarshalIndent(stale, "", "  ")
	if err := os.WriteFile(settingsPath, b, 0o600); err != nil {
		t.Fatal(err)
	}

	autoUpgradeHooks(env)

	m := readJSON(t, settingsPath)
	hooks := m["hooks"].(map[string]any)
	seEntry := hooks["SessionEnd"].([]any)[0].(map[string]any)
	meta := seEntry["x-heimdall"].(map[string]any)
	if got, _ := meta["heimdall_version"].(string); got != heimdallBinaryVersion {
		t.Errorf("want heimdall_version=%q, got %q", heimdallBinaryVersion, got)
	}
	inner := seEntry["hooks"].([]any)[0].(map[string]any)
	if _, has := inner["timeout"]; !has {
		t.Errorf("refreshed SessionEnd inner hook must carry timeout, got: %v", inner)
	}
}

// ----- T15: uninstall-hooks -----

func TestUninstallHooks_RemovesHeimdallEntries(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	// First install, then uninstall.
	{
		var so, se bytes.Buffer
		if code := CLIInstallHooks(config.Config{}, nil, &so, &se, env, []string{"--scope=user"}); code != 0 {
			t.Fatalf("install failed: %d %s", code, se.String())
		}
	}
	var so, se bytes.Buffer
	code := CLIUninstallHooks(config.Config{}, nil, &so, &se, env, []string{"--scope=user"})
	if code != 0 {
		t.Fatalf("uninstall exit=%d stderr=%s", code, se.String())
	}
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	m := readJSON(t, settingsPath)
	if _, ok := m["hooks"]; ok {
		t.Errorf("hooks key should be removed when empty, got: %v", m)
	}
}

func TestUninstallHooks_LeavesForeignHooksIntact(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	// Install heimdall + add a foreign hook on a different event.
	{
		var so, se bytes.Buffer
		if code := CLIInstallHooks(config.Config{}, nil, &so, &se, env, []string{"--scope=user"}); code != 0 {
			t.Fatalf("install failed: %s", se.String())
		}
	}
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	m := readJSON(t, settingsPath)
	hooks := m["hooks"].(map[string]any)
	hooks["UserPromptSubmit"] = []any{
		map[string]any{
			"hooks": []any{
				map[string]any{"type": "command", "command": "/usr/local/bin/other"},
			},
		},
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	os.WriteFile(settingsPath, b, 0o600)

	var so, se bytes.Buffer
	code := CLIUninstallHooks(config.Config{}, nil, &so, &se, env, []string{"--scope=user"})
	if code != 0 {
		t.Fatalf("uninstall exit=%d", code)
	}
	m2 := readJSON(t, settingsPath)
	hooks2 := m2["hooks"].(map[string]any)
	if _, ok := hooks2["UserPromptSubmit"]; !ok {
		t.Errorf("foreign UserPromptSubmit should survive uninstall")
	}
	if _, ok := hooks2["SessionStart"]; ok {
		t.Errorf("heimdall SessionStart should be removed")
	}
}

func TestUninstallHooks_IdempotentSecondRun(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	{
		var so, se bytes.Buffer
		CLIInstallHooks(config.Config{}, nil, &so, &se, env, []string{"--scope=user"})
	}
	for i := 0; i < 2; i++ {
		var so, se bytes.Buffer
		code := CLIUninstallHooks(config.Config{}, nil, &so, &se, env, []string{"--scope=user"})
		if code != 0 {
			t.Fatalf("pass %d: exit=%d stderr=%s", i, code, se.String())
		}
	}
}

func TestUninstallHooks_DryRunDoesNotWrite(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	{
		var so, se bytes.Buffer
		CLIInstallHooks(config.Config{}, nil, &so, &se, env, []string{"--scope=user"})
	}
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	before, _ := os.ReadFile(settingsPath)
	var so, se bytes.Buffer
	code := CLIUninstallHooks(config.Config{}, nil, &so, &se, env, []string{"--scope=user", "--dry-run"})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, se.String())
	}
	after, _ := os.ReadFile(settingsPath)
	if !bytes.Equal(before, after) {
		t.Errorf("dry-run modified settings.json")
	}
}

func TestUninstallHooks_NoSettingsFileIsNoop(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	var so, se bytes.Buffer
	code := CLIUninstallHooks(config.Config{}, nil, &so, &se, env, []string{"--scope=user"})
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(so.String(), "nothing to do") {
		t.Errorf("expected nothing-to-do message, got: %s", so.String())
	}
}

// ----- T16: hooks doctor -----

func TestDoctorChecks_GreenWhenAllOK(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(settingsPath), 0o755)
	// Valid settings with one heimdall entry so check #3 passes.
	settings := map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{
					"source":  "heimdall",
					"version": float64(1),
					"hooks": []any{
						map[string]any{"type": "command", "command": "fake-bin --source=heimdall"},
					},
				},
			},
		},
	}
	b, _ := json.MarshalIndent(settings, "", "  ")
	os.WriteFile(settingsPath, b, 0o600)

	deps := doctorDeps{
		settingsPath: settingsPath,
		lookupPath:   func(name string) (string, error) { return "/fake/" + name, nil },
		runVersion:   func(bin string) error { return nil },
		pingOllama:   func(ctx context.Context, ep string) error { return nil },
		listModels:   func(ctx context.Context, ep string) ([]string, error) { return []string{"bge-m3"}, nil },
		dryFire:      func(cmd string) error { return nil },
		hookLogPath:  filepath.Join(home, "hooks.log"),
		projectRoot:  "", // no project — produces a yellow warn, still exit 0
		skillsDir:    filepath.Join(home, "claude-skills"),
		memoryDBPath: filepath.Join(home, "memory.db"),
	}
	cfg := config.Config{Model: "bge-m3"}
	checks := runDoctorChecks(cfg, env, deps)
	if len(checks) != 15 {
		t.Fatalf("expected 15 checks, got %d", len(checks))
	}
	for _, c := range checks {
		if c.status == statusFail {
			t.Errorf("unexpected failure: %s — %s", c.name, c.message)
		}
	}
}

func TestDoctorChecks_FailsOnMissingSettings(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	deps := doctorDeps{
		settingsPath: filepath.Join(home, "nope", "settings.json"),
		lookupPath:   func(name string) (string, error) { return "/fake/" + name, nil },
		runVersion:   func(bin string) error { return nil },
		pingOllama:   func(ctx context.Context, ep string) error { return nil },
		listModels:   func(ctx context.Context, ep string) ([]string, error) { return []string{"bge-m3"}, nil },
		dryFire:      func(cmd string) error { return nil },
		hookLogPath:  filepath.Join(home, "hooks.log"),
		skillsDir:    filepath.Join(home, "claude-skills"),
		memoryDBPath: filepath.Join(home, "memory.db"),
	}
	checks := runDoctorChecks(config.Config{Model: "bge-m3"}, env, deps)
	first := checks[0]
	if first.status != statusFail {
		t.Errorf("expected fail on missing settings, got %v: %s", first.status, first.message)
	}
}

func TestDoctorChecks_FailsOnInvalidJSON(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	settingsPath := filepath.Join(home, "settings.json")
	os.WriteFile(settingsPath, []byte("{nope"), 0o600)
	deps := doctorDeps{
		settingsPath: settingsPath,
		lookupPath:   func(name string) (string, error) { return "/fake/" + name, nil },
		runVersion:   func(bin string) error { return nil },
		pingOllama:   func(ctx context.Context, ep string) error { return nil },
		listModels:   func(ctx context.Context, ep string) ([]string, error) { return []string{"bge-m3"}, nil },
		dryFire:      func(cmd string) error { return nil },
		hookLogPath:  filepath.Join(home, "hooks.log"),
		skillsDir:    filepath.Join(home, "claude-skills"),
		memoryDBPath: filepath.Join(home, "memory.db"),
	}
	checks := runDoctorChecks(config.Config{Model: "bge-m3"}, env, deps)
	if checks[1].status != statusFail {
		t.Errorf("expected fail on invalid JSON, got %v", checks[1].status)
	}
}

func TestDoctorChecks_BinaryMissingFails(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	settingsPath := filepath.Join(home, "settings.json")
	os.WriteFile(settingsPath, []byte("{}"), 0o600)
	deps := doctorDeps{
		settingsPath: settingsPath,
		lookupPath:   func(name string) (string, error) { return "", errors.New("not found") },
		runVersion:   func(bin string) error { return nil },
		pingOllama:   func(ctx context.Context, ep string) error { return nil },
		listModels:   func(ctx context.Context, ep string) ([]string, error) { return []string{"bge-m3"}, nil },
		dryFire:      func(cmd string) error { return nil },
		hookLogPath:  filepath.Join(home, "hooks.log"),
		skillsDir:    filepath.Join(home, "claude-skills"),
		memoryDBPath: filepath.Join(home, "memory.db"),
	}
	checks := runDoctorChecks(config.Config{Model: "bge-m3"}, env, deps)
	// check #4 (index 3) is "heimdall-mcp on PATH"
	if checks[3].status != statusFail {
		t.Errorf("expected fail on missing binary, got %v: %s", checks[3].status, checks[3].message)
	}
}

func TestDoctorChecks_OllamaDownIsWarnNotFail(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	settingsPath := filepath.Join(home, "settings.json")
	os.WriteFile(settingsPath, []byte("{}"), 0o600)
	deps := doctorDeps{
		settingsPath: settingsPath,
		lookupPath:   func(name string) (string, error) { return "/fake/" + name, nil },
		runVersion:   func(bin string) error { return nil },
		pingOllama:   func(ctx context.Context, ep string) error { return errors.New("connection refused") },
		listModels:   func(ctx context.Context, ep string) ([]string, error) { return nil, errors.New("nope") },
		dryFire:      func(cmd string) error { return nil },
		hookLogPath:  filepath.Join(home, "hooks.log"),
		skillsDir:    filepath.Join(home, "claude-skills"),
		memoryDBPath: filepath.Join(home, "memory.db"),
	}
	checks := runDoctorChecks(config.Config{Model: "bge-m3"}, env, deps)
	// check #6 (index 5)
	if checks[5].status != statusWarn {
		t.Errorf("expected warn for ollama down, got %v", checks[5].status)
	}
	// nothing red anywhere except optional binary check; this exercise is
	// scoped: confirm overall is exit-0-eligible.
	for i, c := range checks {
		if c.status == statusFail {
			t.Logf("check %d (%s): %s", i, c.name, c.message)
		}
	}
}

func TestDoctorChecks_DryFireFailsRed(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	settingsPath := filepath.Join(home, "settings.json")
	os.MkdirAll(filepath.Dir(settingsPath), 0o755)
	settings := map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{
					"source":  "heimdall",
					"version": float64(1),
					"hooks": []any{
						map[string]any{"type": "command", "command": "fake-bin"},
					},
				},
			},
		},
	}
	b, _ := json.MarshalIndent(settings, "", "  ")
	os.WriteFile(settingsPath, b, 0o600)

	deps := doctorDeps{
		settingsPath: settingsPath,
		lookupPath:   func(name string) (string, error) { return "/fake/" + name, nil },
		runVersion:   func(bin string) error { return nil },
		pingOllama:   func(ctx context.Context, ep string) error { return nil },
		listModels:   func(ctx context.Context, ep string) ([]string, error) { return []string{"bge-m3"}, nil },
		dryFire:      func(cmd string) error { return errors.New("non-zero exit 1") },
		hookLogPath:  filepath.Join(home, "hooks.log"),
		skillsDir:    filepath.Join(home, "claude-skills"),
		memoryDBPath: filepath.Join(home, "memory.db"),
	}
	checks := runDoctorChecks(config.Config{Model: "bge-m3"}, env, deps)
	// check #11 (index 10) — hook dry-fire; shifted after F5 LLM-classifier
	// check inserted at index 7.
	if checks[10].status != statusFail {
		t.Errorf("expected fail on dry-fire failure, got %v: %s", checks[10].status, checks[10].message)
	}
}

func TestDoctorChecks_SkipsInternalPostEditActor(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	settingsPath := filepath.Join(home, "settings.json")
	os.MkdirAll(filepath.Dir(settingsPath), 0o755)
	settings := map[string]any{
		"hooks": map[string]any{
			"PostToolUse": []any{
				map[string]any{
					"source":  "heimdall",
					"version": float64(1),
					"hooks": []any{
						map[string]any{"type": "command", "command": "heimdall-mcp hook post-edit-actor --internal"},
					},
				},
			},
		},
	}
	b, _ := json.MarshalIndent(settings, "", "  ")
	os.WriteFile(settingsPath, b, 0o600)

	called := 0
	deps := doctorDeps{
		settingsPath: settingsPath,
		lookupPath:   func(name string) (string, error) { return "/fake/" + name, nil },
		runVersion:   func(bin string) error { return nil },
		pingOllama:   func(ctx context.Context, ep string) error { return nil },
		listModels:   func(ctx context.Context, ep string) ([]string, error) { return []string{"bge-m3"}, nil },
		dryFire:      func(cmd string) error { called++; return nil },
		hookLogPath:  filepath.Join(home, "hooks.log"),
		skillsDir:    filepath.Join(home, "claude-skills"),
		memoryDBPath: filepath.Join(home, "memory.db"),
	}
	runDoctorChecks(config.Config{Model: "bge-m3"}, env, deps)
	if called != 0 {
		t.Errorf("dryFire should not have been called for post-edit-actor; called=%d", called)
	}
}

func TestDoctorChecks_SessionsPipeline_NoLogWarns(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	settingsPath := filepath.Join(home, "settings.json")
	os.WriteFile(settingsPath, []byte(`{}`), 0o600)
	deps := doctorDeps{
		settingsPath: settingsPath,
		lookupPath:   func(name string) (string, error) { return "/fake/" + name, nil },
		runVersion:   func(bin string) error { return nil },
		pingOllama:   func(ctx context.Context, ep string) error { return nil },
		listModels:   func(ctx context.Context, ep string) ([]string, error) { return []string{"bge-m3"}, nil },
		dryFire:      func(cmd string) error { return nil },
		hookLogPath:  filepath.Join(home, "does-not-exist.log"),
		skillsDir:    filepath.Join(home, "claude-skills"),
		memoryDBPath: filepath.Join(home, "memory.db"),
	}
	checks := runDoctorChecks(config.Config{Model: "bge-m3"}, env, deps)
	if len(checks) < 15 {
		t.Fatalf("expected >=15 checks, got %d", len(checks))
	}
	last := checks[14]
	if last.name != "sessions pipeline" {
		t.Fatalf("expected sessions pipeline check at index 14, got %q", last.name)
	}
	if last.status != statusWarn {
		t.Errorf("expected warn on missing hooks.log, got %v: %s", last.status, last.message)
	}
}

func TestDoctorChecks_SessionsPipeline_ReadsSessions(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	settingsPath := filepath.Join(home, "settings.json")
	os.WriteFile(settingsPath, []byte(`{}`), 0o600)

	hookLog := filepath.Join(home, "hooks.log")
	_ = os.WriteFile(hookLog,
		[]byte("2026-04-17T10:00:00Z INFO event=user-prompt session=FOO stage=ok bytes=123\n"), 0o600)

	deps := doctorDeps{
		settingsPath: settingsPath,
		lookupPath:   func(name string) (string, error) { return "/fake/" + name, nil },
		runVersion:   func(bin string) error { return nil },
		pingOllama:   func(ctx context.Context, ep string) error { return nil },
		listModels:   func(ctx context.Context, ep string) ([]string, error) { return []string{"bge-m3"}, nil },
		dryFire:      func(cmd string) error { return nil },
		hookLogPath:  hookLog,
		skillsDir:    filepath.Join(home, "claude-skills"),
		memoryDBPath: filepath.Join(home, "memory.db"),
	}
	checks := runDoctorChecks(config.Config{Model: "bge-m3"}, env, deps)
	last := checks[len(checks)-1]
	if last.name != "sessions pipeline" {
		t.Fatalf("expected last check to be sessions pipeline, got %q", last.name)
	}
	if last.status != statusPass {
		t.Errorf("expected pass with session in log, got %v: %s", last.status, last.message)
	}
	if !strings.Contains(last.message, "1 session") {
		t.Errorf("expected session count in message, got: %s", last.message)
	}
}

func TestDoctor_HandlerWiring(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(settingsPath), 0o755)
	os.WriteFile(settingsPath, []byte(`{}`), 0o600)
	var stdout, stderr bytes.Buffer
	// HooksDoctor uses real exec for binary lookup etc.; the missing binary
	// will likely produce a fail and exit 1, which still proves wiring.
	code := HooksDoctor(config.Config{Model: "bge-m3", OllamaEndpoint: "http://127.0.0.1:1"}, nil, &stdout, &stderr, env, []string{"--scope=user"})
	if code != 0 && code != 1 {
		t.Fatalf("unexpected exit %d", code)
	}
	if !strings.Contains(stdout.String(), "settings.json present") {
		t.Errorf("expected check table in stdout, got: %s", stdout.String())
	}
}

// ----- F5: llm-classifier model_missing check (plan 11 §5.5) -----

// llmDoctorDeps is a tiny helper that returns a baseline doctorDeps with
// Ollama-up + `bge-m3` present, so each F5 case only has to override the
// fields it actually tests.
func llmDoctorDeps(t *testing.T, home string, models []string) doctorDeps {
	t.Helper()
	settingsPath := filepath.Join(home, "settings.json")
	os.WriteFile(settingsPath, []byte("{}"), 0o600)
	return doctorDeps{
		settingsPath: settingsPath,
		lookupPath:   func(name string) (string, error) { return "/fake/" + name, nil },
		runVersion:   func(bin string) error { return nil },
		pingOllama:   func(ctx context.Context, ep string) error { return nil },
		listModels:   func(ctx context.Context, ep string) ([]string, error) { return models, nil },
		dryFire:      func(cmd string) error { return nil },
		hookLogPath:  filepath.Join(home, "hooks.log"),
		skillsDir:    filepath.Join(home, "claude-skills"),
		memoryDBPath: filepath.Join(home, "memory.db"),
	}
}

// TestDoctorChecks_LLMClassifier_DisabledByEnv — default-off box: flag is
// not set, so the check must not warn. Must stay quiet even when the
// configured classifier model is unset, because the gate is AND.
func TestDoctorChecks_LLMClassifier_DisabledByEnv(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home) // no HEIMDALL_LLM_CLASSIFIER set
	deps := llmDoctorDeps(t, home, []string{"bge-m3"})
	cfg := config.Config{Model: "bge-m3", LLMClassifierModel: "llama3.2:3b"}
	checks := runDoctorChecks(cfg, env, deps)
	// Check #8 (index 7) is the new "llm classifier model" row.
	row := checks[7]
	if row.name != "llm classifier model" {
		t.Fatalf("expected llm-classifier check at index 7, got %q", row.name)
	}
	if row.status != statusPass {
		t.Errorf("expected pass when flag unset, got %v: %s", row.status, row.message)
	}
	if !strings.Contains(row.message, "disabled") {
		t.Errorf("expected 'disabled' in message, got: %s", row.message)
	}
}

// TestDoctorChecks_LLMClassifier_EnabledButNoModelConfigured — flag set
// but cfg.LLMClassifierModel empty → WARN pointing at `configure`.
// Matches the 11a §5.4 "opt-in at both layers" invariant: env alone is
// not enough.
func TestDoctorChecks_LLMClassifier_EnabledButNoModelConfigured(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	env["HEIMDALL_LLM_CLASSIFIER"] = "1"
	deps := llmDoctorDeps(t, home, []string{"bge-m3"})
	cfg := config.Config{Model: "bge-m3"} // LLMClassifierModel empty
	checks := runDoctorChecks(cfg, env, deps)
	row := checks[7]
	if row.status != statusWarn {
		t.Errorf("expected warn with unset model, got %v: %s", row.status, row.message)
	}
	if !strings.Contains(row.message, "configure --llm-classifier-model") {
		t.Errorf("expected configure hint in message, got: %s", row.message)
	}
}

// TestDoctorChecks_LLMClassifier_ModelMissing — the F5 path: flag on,
// model configured, Ollama up, model is NOT in the list. WARN with a
// copy-pasteable `ollama pull` hint.
func TestDoctorChecks_LLMClassifier_ModelMissing(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	env["HEIMDALL_LLM_CLASSIFIER"] = "1"
	// Only embedding model pulled; the classifier model is absent.
	deps := llmDoctorDeps(t, home, []string{"bge-m3"})
	cfg := config.Config{Model: "bge-m3", LLMClassifierModel: "llama3.2:3b"}
	checks := runDoctorChecks(cfg, env, deps)
	row := checks[7]
	if row.status != statusWarn {
		t.Errorf("expected warn on missing classifier model, got %v: %s", row.status, row.message)
	}
	if !strings.Contains(row.message, "ollama pull llama3.2:3b") {
		t.Errorf("expected 'ollama pull <model>' hint, got: %s", row.message)
	}
	// Must never be fatal — a missing classifier model is a paper-cut
	// warning, not a reason to exit 1.
	for _, c := range checks {
		if c.status == statusFail && c.name == "llm classifier model" {
			t.Errorf("F5 must never be fatal; got fail for %q", c.name)
		}
	}
}

// TestDoctorChecks_LLMClassifier_ModelPresent — happy path: flag on,
// model configured, Ollama up, model IS in the list (with :latest tag
// variance). Must PASS and echo the model name.
func TestDoctorChecks_LLMClassifier_ModelPresent(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	env["HEIMDALL_LLM_CLASSIFIER"] = "1"
	// `:latest` tag roundtrip: modelInList normalizes the implicit tag.
	deps := llmDoctorDeps(t, home, []string{"bge-m3", "llama3.2:3b"})
	cfg := config.Config{Model: "bge-m3", LLMClassifierModel: "llama3.2:3b"}
	checks := runDoctorChecks(cfg, env, deps)
	row := checks[7]
	if row.status != statusPass {
		t.Errorf("expected pass when model pulled, got %v: %s", row.status, row.message)
	}
	if !strings.Contains(row.message, "llama3.2:3b") {
		t.Errorf("expected model name echoed, got: %s", row.message)
	}
}

// TestDoctorChecks_LLMClassifier_OllamaDownSkipsQuietly — if Ollama isn't
// reachable, check #6 already warns. We must not warn a second time on
// the same root cause; the F5 row reports "ollama down — skipped".
func TestDoctorChecks_LLMClassifier_OllamaDownSkipsQuietly(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	env["HEIMDALL_LLM_CLASSIFIER"] = "1"
	deps := llmDoctorDeps(t, home, []string{"llama3.2:3b"})
	deps.pingOllama = func(ctx context.Context, ep string) error { return errors.New("connection refused") }
	// listModels should never be called when ping fails; guard via assertion.
	listCalls := 0
	deps.listModels = func(ctx context.Context, ep string) ([]string, error) {
		listCalls++
		return []string{"llama3.2:3b"}, nil
	}
	cfg := config.Config{Model: "bge-m3", LLMClassifierModel: "llama3.2:3b"}
	checks := runDoctorChecks(cfg, env, deps)
	row := checks[7]
	if row.status != statusWarn {
		t.Errorf("expected warn when ollama down, got %v: %s", row.status, row.message)
	}
	if !strings.Contains(row.message, "ollama down") {
		t.Errorf("expected 'ollama down' marker, got: %s", row.message)
	}
	if listCalls != 0 {
		t.Errorf("listModels must not be called when ping fails; calls=%d", listCalls)
	}
}

// TestDoctorChecks_LLMClassifier_ListModelsError — Ollama up but
// /api/tags fails (e.g. permissions, malformed response). Surface the
// underlying error as a WARN, not FAIL — the hook itself still works
// end-to-end because plan 11 §5.5 handles the runtime error.
func TestDoctorChecks_LLMClassifier_ListModelsError(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	env["HEIMDALL_LLM_CLASSIFIER"] = "1"
	deps := llmDoctorDeps(t, home, nil)
	deps.listModels = func(ctx context.Context, ep string) ([]string, error) {
		return nil, errors.New("unexpected HTTP 500")
	}
	cfg := config.Config{Model: "bge-m3", LLMClassifierModel: "llama3.2:3b"}
	checks := runDoctorChecks(cfg, env, deps)
	row := checks[7]
	if row.status != statusWarn {
		t.Errorf("expected warn on list error, got %v: %s", row.status, row.message)
	}
	if !strings.Contains(row.message, "HTTP 500") {
		t.Errorf("expected underlying error surfaced, got: %s", row.message)
	}
}

// ----- helper sanity tests -----

func TestIsHeimdallEntry_BothMarkers(t *testing.T) {
	if !isHeimdallEntry(map[string]any{"source": "heimdall"}) {
		t.Errorf("source field should match")
	}
	if !isHeimdallEntry(map[string]any{
		"hooks": []any{
			map[string]any{"command": "x --source=heimdall"},
		},
	}) {
		t.Errorf("command-string fallback should match")
	}
	if isHeimdallEntry(map[string]any{"source": "other"}) {
		t.Errorf("other source should not match")
	}
}

func TestApplyUninstall_Idempotent(t *testing.T) {
	in := map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{"source": "heimdall", "hooks": []any{}},
				map[string]any{"hooks": []any{map[string]any{"command": "/foreign"}}},
			},
		},
		"other": "key",
	}
	out, removed := applyUninstall(in)
	if removed != 1 {
		t.Errorf("expected 1 removed, got %d", removed)
	}
	if out["other"] != "key" {
		t.Errorf("non-hook keys should survive")
	}
	out2, removed2 := applyUninstall(out)
	if removed2 != 0 {
		t.Errorf("second pass should remove nothing, got %d", removed2)
	}
	_ = out2
}

// ----- hooksDetected (OQ-2 first-run hint) -----

func TestHooksDetected_TrueWhenInstalled(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)

	// Install hooks into user scope.
	var stdout, stderr bytes.Buffer
	code := CLIInstallHooks(config.Config{}, nil, &stdout, &stderr, env, []string{"--scope=user"})
	if code != 0 {
		t.Fatalf("install failed: %d %s", code, stderr.String())
	}

	if !hooksDetected(env) {
		t.Errorf("hooksDetected should return true after install-hooks")
	}
}

func TestHooksDetected_FalseWhenNoSettings(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)

	if hooksDetected(env) {
		t.Errorf("hooksDetected should return false when settings.json does not exist")
	}
}

func TestHooksDetected_FalseWhenNoHeimdallEntries(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(settingsPath), 0o755)

	// settings.json with non-heimdall hooks only.
	settings := map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{"type": "command", "command": "/usr/local/bin/other-tool"},
					},
				},
			},
		},
	}
	b, _ := json.MarshalIndent(settings, "", "  ")
	os.WriteFile(settingsPath, b, 0o600)

	if hooksDetected(env) {
		t.Errorf("hooksDetected should return false when only non-heimdall hooks exist")
	}
}

func TestHooksDetected_FalseWhenEmptyHooksMap(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(settingsPath), 0o755)

	settings := map[string]any{
		"hooks": map[string]any{},
	}
	b, _ := json.MarshalIndent(settings, "", "  ")
	os.WriteFile(settingsPath, b, 0o600)

	if hooksDetected(env) {
		t.Errorf("hooksDetected should return false when hooks map is empty")
	}
}

func TestHooksDetected_TrueWhenOnlyCommandMarker(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(settingsPath), 0o755)

	// Entry without "source":"heimdall" but with --source=heimdall in command.
	settings := map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{"type": "command", "command": "heimdall-mcp hook session-start --source=heimdall --version=1"},
					},
				},
			},
		},
	}
	b, _ := json.MarshalIndent(settings, "", "  ")
	os.WriteFile(settingsPath, b, 0o600)

	if !hooksDetected(env) {
		t.Errorf("hooksDetected should return true when command contains --source=heimdall")
	}
}

func TestHooksDetected_ProjectScope(t *testing.T) {
	home := t.TempDir()
	// Create a project root with .heimdall_db so project scope resolves.
	projectDir := filepath.Join(home, "myproject")
	os.MkdirAll(filepath.Join(projectDir, ".heimdall_db"), 0o755)

	env := map[string]string{
		"HOME":               home,
		"HEIMDALL_TEST_HOME": home,
		"HEIMDALL_TEST_CWD":  projectDir,
	}

	// No user-scope settings — only project-scope settings with heimdall entries.
	settingsPath := filepath.Join(projectDir, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(settingsPath), 0o755)
	settings := map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{
					"source":  "heimdall",
					"version": float64(1),
					"hooks": []any{
						map[string]any{"type": "command", "command": "heimdall-mcp hook session-start --source=heimdall --version=1"},
					},
				},
			},
		},
	}
	b, _ := json.MarshalIndent(settings, "", "  ")
	os.WriteFile(settingsPath, b, 0o600)

	if !hooksDetected(env) {
		t.Errorf("hooksDetected should return true when heimdall hooks exist in project scope")
	}
}

// ----- autoUpgradeHooks -----

func TestAutoUpgrade_AddsMissingHooks(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(settingsPath), 0o755)

	// Install only SessionStart — simulates an old binary that had fewer hooks.
	settings := map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{
					"source":  "heimdall",
					"version": float64(1),
					"hooks": []any{
						map[string]any{"type": "command", "command": "heimdall-mcp hook session-start --source=heimdall --version=1"},
					},
				},
			},
		},
	}
	b, _ := json.MarshalIndent(settings, "", "  ")
	os.WriteFile(settingsPath, b, 0o600)

	autoUpgradeHooks(env)

	// Should now have all hooks from phase1aHooks template.
	m := readJSON(t, settingsPath)
	events := hookEvents(m)
	for _, h := range phase1aHooks {
		found := false
		for _, e := range events {
			if e == h.event {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected event %q after auto-upgrade, got events: %v", h.event, events)
		}
	}
}

func TestAutoUpgrade_NoopWhenComplete(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)

	// Do a full install first.
	var stdout, stderr bytes.Buffer
	code := CLIInstallHooks(config.Config{}, nil, &stdout, &stderr, env, []string{"--scope=user"})
	if code != 0 {
		t.Fatalf("install failed: %d", code)
	}

	settingsPath := filepath.Join(home, ".claude", "settings.json")
	before, _ := os.ReadFile(settingsPath)

	autoUpgradeHooks(env)

	after, _ := os.ReadFile(settingsPath)
	if string(before) != string(after) {
		t.Errorf("auto-upgrade should not modify a fully installed settings.json")
	}
}

func TestAutoUpgrade_SkipsWhenNoHeimdallEntries(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(settingsPath), 0o755)

	// Non-heimdall hooks — should NOT be upgraded (OQ-2: opt-in first install).
	settings := map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{"type": "command", "command": "/usr/local/bin/other-tool"},
					},
				},
			},
		},
	}
	b, _ := json.MarshalIndent(settings, "", "  ")
	os.WriteFile(settingsPath, b, 0o600)

	autoUpgradeHooks(env)

	m := readJSON(t, settingsPath)
	events := hookEvents(m)
	if len(events) != 1 || events[0] != "SessionStart" {
		t.Errorf("auto-upgrade should not add hooks when no heimdall entries exist; got events: %v", events)
	}
}

func TestAutoUpgrade_SkipsWhenNoSettings(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	// No settings.json — should be a no-op.
	autoUpgradeHooks(env)
	// No crash = pass.
}

// ----- pre-warm Ollama on install-hooks -----

// prewarmCall records one invocation of the stubbed prewarm so tests can
// assert what was passed and whether the caller honored the timeout
// contract. Recording only what we need keeps the assertions tight.
type prewarmCall struct {
	endpoint string
	model    string
	deadline time.Time
	hadCtx   bool
}

// stubPrewarm swaps prewarmFn for a recording fake during a single test.
// The supplied function drives the fake's behavior — pass a stub that
// returns success, error, or simulates a timeout outcome as needed per
// test. The Cleanup hook restores the original prewarmFn so tests don't
// leak state into each other when run with -count=N or under -race.
//
// It also pre-stubs skillsImportFn with a harmless no-op so tests that
// exercise the post-install path don't accidentally open the real
// ~/.config/heimdall-mcp/memories.db. Individual tests that need to
// observe or drive the skills-import step should call stubSkillsImport
// explicitly — their cleanup runs after this helper's.
func stubPrewarm(t *testing.T, fn func(ctx context.Context, endpoint, model string) prewarmResult) *[]prewarmCall {
	t.Helper()
	stubSkillsImport(t, func(ctx context.Context, cfg config.Config, env map[string]string) skillsImportResult {
		return skillsImportResult{}
	})
	calls := &[]prewarmCall{}
	prev := prewarmFn
	prewarmFn = func(ctx context.Context, endpoint, model string) prewarmResult {
		dl, ok := ctx.Deadline()
		*calls = append(*calls, prewarmCall{
			endpoint: endpoint,
			model:    model,
			deadline: dl,
			hadCtx:   ok,
		})
		return fn(ctx, endpoint, model)
	}
	t.Cleanup(func() { prewarmFn = prev })
	return calls
}

// skillsImportCall records one invocation of the stubbed skills import so
// tests can assert what was passed. Keeps the helper shape symmetric with
// prewarmCall.
type skillsImportCall struct {
	endpoint string
	model    string
	dir      string // resolved at call time via heimdall.ResolveClaudeSkillsDir(env)
}

// stubSkillsImport swaps skillsImportFn for a recording fake during a single
// test. Mirrors stubPrewarm: the supplied function drives behavior; Cleanup
// restores the original. The returned slice records every call with the
// cfg endpoint/model the install handler passed so timeout / arg-shape
// assertions stay localized.
func stubSkillsImport(t *testing.T, fn skillsImportFunc) *[]skillsImportCall {
	t.Helper()
	calls := &[]skillsImportCall{}
	prev := skillsImportFn
	skillsImportFn = func(ctx context.Context, cfg config.Config, env map[string]string) skillsImportResult {
		*calls = append(*calls, skillsImportCall{
			endpoint: cfg.OllamaEndpoint,
			model:    cfg.Model,
			// env passed through verbatim; snapshotting HEIMDALL_CLAUDE_SKILLS_DIR
			// (or the default HOME-based fallback) would double-resolve and
			// give tests no new information. Tests assert by inspecting
			// stdout or the length of this slice instead.
			dir: env["HEIMDALL_CLAUDE_SKILLS_DIR"],
		})
		return fn(ctx, cfg, env)
	}
	t.Cleanup(func() { skillsImportFn = prev })
	return calls
}

// TestPrewarm_CalledOnceWithExpectedInput verifies that a successful install
// fires exactly one EmbedForHook against the configured endpoint and model,
// and renders the expected progress + success status lines on stdout.
func TestPrewarm_CalledOnceWithExpectedInput(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	cfg := config.Config{
		OllamaEndpoint: "http://stub:11434",
		Model:          "test-model",
	}
	calls := stubPrewarm(t, func(ctx context.Context, endpoint, model string) prewarmResult {
		return prewarmResult{model: model, duration: 12 * time.Millisecond}
	})

	var stdout, stderr bytes.Buffer
	code := CLIInstallHooks(cfg, nil, &stdout, &stderr, env, []string{"--scope=user"})
	if code != 0 {
		t.Fatalf("install exit=%d stderr=%s", code, stderr.String())
	}
	if got := len(*calls); got != 1 {
		t.Fatalf("expected exactly 1 prewarm call, got %d: %+v", got, *calls)
	}
	c := (*calls)[0]
	if c.endpoint != cfg.OllamaEndpoint {
		t.Errorf("prewarm endpoint = %q, want %q", c.endpoint, cfg.OllamaEndpoint)
	}
	if c.model != cfg.Model {
		t.Errorf("prewarm model = %q, want %q", c.model, cfg.Model)
	}
	if !c.hadCtx {
		t.Errorf("prewarm context should carry a deadline (got none)")
	}
	out := stdout.String()
	if !strings.Contains(out, "Pre-warming Ollama...") {
		t.Errorf("missing pre-warm progress line in stdout: %s", out)
	}
	if !strings.Contains(out, "Pre-warmed model=test-model") {
		t.Errorf("missing success line in stdout: %s", out)
	}
}

// TestPrewarm_TimeoutBounded asserts that the install side passes a real
// deadline ≤ prewarmTimeout, and that simulating a timeout-exceeded result
// from the embedder still returns 0 promptly. We verify the deadline budget
// instead of actually sleeping 5s — that keeps the test fast and
// deterministic, while still proving the contract.
func TestPrewarm_TimeoutBounded(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	cfg := config.Config{Model: "slow-model", OllamaEndpoint: "http://stub:11434"}

	stubPrewarm(t, func(ctx context.Context, endpoint, model string) prewarmResult {
		dl, ok := ctx.Deadline()
		if !ok {
			t.Errorf("prewarm: caller did not set a deadline")
			return prewarmResult{model: model, err: errors.New("no deadline")}
		}
		if remaining := time.Until(dl); remaining > prewarmTimeout+250*time.Millisecond {
			t.Errorf("prewarm deadline too far in future: remaining=%v want <= %v", remaining, prewarmTimeout)
		}
		// Simulate timeout outcome without actually blocking the test.
		return prewarmResult{model: model, err: context.DeadlineExceeded, duration: prewarmTimeout}
	})

	start := time.Now()
	var stdout, stderr bytes.Buffer
	code := CLIInstallHooks(cfg, nil, &stdout, &stderr, env, []string{"--scope=user"})
	elapsed := time.Since(start)
	if code != 0 {
		t.Fatalf("install must not fail on prewarm timeout: exit=%d stderr=%s", code, stderr.String())
	}
	// Generous safety net: the stub returns immediately, so the install
	// should finish in well under a second. We assert under 10s to catch
	// accidental real Ollama calls or a forgotten sleep.
	if elapsed > 10*time.Second {
		t.Errorf("install took too long: %v (prewarm should be bounded by %v)", elapsed, prewarmTimeout)
	}
	if !strings.Contains(stdout.String(), "Pre-warm skipped") {
		t.Errorf("expected skipped status on timeout, got: %s", stdout.String())
	}
}

// TestPrewarm_FailureDoesNotFailInstall checks that an erroring embedder
// surfaces a one-line skip message but lets the install succeed.
func TestPrewarm_FailureDoesNotFailInstall(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	cfg := config.Config{Model: "broken-model", OllamaEndpoint: "http://stub:11434"}

	stubPrewarm(t, func(ctx context.Context, endpoint, model string) prewarmResult {
		return prewarmResult{model: model, err: errors.New("ollama embed: dial: connection refused")}
	})

	var stdout, stderr bytes.Buffer
	code := CLIInstallHooks(cfg, nil, &stdout, &stderr, env, []string{"--scope=user"})
	if code != 0 {
		t.Fatalf("install must not fail on prewarm error: exit=%d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Pre-warm skipped") {
		t.Errorf("expected skipped status on error, got: %s", out)
	}
	if !strings.Contains(out, "connection refused") {
		t.Errorf("expected error reason in skip line, got: %s", out)
	}
	// Hooks must still be installed.
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	if _, err := os.Stat(settingsPath); err != nil {
		t.Errorf("settings.json should be created even when prewarm fails: %v", err)
	}
}

// TestPrewarm_NoPrewarmFlagSkips verifies that --no-prewarm bypasses the
// embedder entirely (no calls recorded) and prints the disabled status line.
func TestPrewarm_NoPrewarmFlagSkips(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	cfg := config.Config{Model: "any-model", OllamaEndpoint: "http://stub:11434"}

	calls := stubPrewarm(t, func(ctx context.Context, endpoint, model string) prewarmResult {
		t.Errorf("prewarm must not be called when --no-prewarm is set")
		return prewarmResult{}
	})

	var stdout, stderr bytes.Buffer
	code := CLIInstallHooks(cfg, nil, &stdout, &stderr, env, []string{"--scope=user", "--no-prewarm"})
	if code != 0 {
		t.Fatalf("install exit=%d stderr=%s", code, stderr.String())
	}
	if len(*calls) != 0 {
		t.Fatalf("expected zero prewarm calls under --no-prewarm, got %d", len(*calls))
	}
	if !strings.Contains(stdout.String(), "Pre-warm skipped (--no-prewarm)") {
		t.Errorf("expected --no-prewarm skip line in stdout: %s", stdout.String())
	}
}

// TestPrewarm_DryRunSkips verifies --dry-run doesn't fire the embedder
// (we never wrote settings.json, so warming makes no sense).
func TestPrewarm_DryRunSkips(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	cfg := config.Config{Model: "any-model", OllamaEndpoint: "http://stub:11434"}

	calls := stubPrewarm(t, func(ctx context.Context, endpoint, model string) prewarmResult {
		t.Errorf("prewarm must not be called in --dry-run mode")
		return prewarmResult{}
	})

	var stdout, stderr bytes.Buffer
	code := CLIInstallHooks(cfg, nil, &stdout, &stderr, env, []string{"--scope=user", "--dry-run"})
	if code != 0 {
		t.Fatalf("dry-run install exit=%d stderr=%s", code, stderr.String())
	}
	if len(*calls) != 0 {
		t.Fatalf("expected zero prewarm calls in --dry-run, got %d", len(*calls))
	}
	// Dry-run must not print a Pre-warm status (it didn't run one).
	out := stdout.String()
	if strings.Contains(out, "Pre-warm") {
		t.Errorf("dry-run should not print a Pre-warm line, got: %s", out)
	}
}

// TestPrewarm_NoModelConfiguredSkips verifies that an empty cfg.Model is
// treated as a soft skip with no embedder call. Belt-and-braces: the install
// should still succeed, since there's no useful model to warm.
func TestPrewarm_NoModelConfiguredSkips(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	cfg := config.Config{} // Model intentionally empty

	calls := stubPrewarm(t, func(ctx context.Context, endpoint, model string) prewarmResult {
		t.Errorf("prewarm must not be called when cfg.Model is empty")
		return prewarmResult{}
	})

	var stdout, stderr bytes.Buffer
	code := CLIInstallHooks(cfg, nil, &stdout, &stderr, env, []string{"--scope=user"})
	if code != 0 {
		t.Fatalf("install exit=%d stderr=%s", code, stderr.String())
	}
	if len(*calls) != 0 {
		t.Fatalf("expected zero prewarm calls when no model configured, got %d", len(*calls))
	}
	if !strings.Contains(stdout.String(), "Pre-warm skipped (no model configured)") {
		t.Errorf("expected no-model skip line in stdout: %s", stdout.String())
	}
}

// TestPrewarm_FiresOnForce verifies the prewarm path runs under --force,
// matching the spec: re-installing should always end with a warm model.
func TestPrewarm_FiresOnForce(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(settingsPath), 0o755)

	// Pre-existing foreign hook so --force has something to replace.
	conflicting := map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{"type": "command", "command": "/usr/local/bin/other-tool"},
					},
				},
			},
		},
	}
	b, _ := json.MarshalIndent(conflicting, "", "  ")
	os.WriteFile(settingsPath, b, 0o600)

	cfg := config.Config{Model: "test-model", OllamaEndpoint: "http://stub:11434"}
	calls := stubPrewarm(t, func(ctx context.Context, endpoint, model string) prewarmResult {
		return prewarmResult{model: model, duration: 5 * time.Millisecond}
	})

	var stdout, stderr bytes.Buffer
	code := CLIInstallHooks(cfg, nil, &stdout, &stderr, env, []string{"--scope=user", "--force"})
	if code != 0 {
		t.Fatalf("install --force exit=%d stderr=%s", code, stderr.String())
	}
	if len(*calls) != 1 {
		t.Fatalf("--force should still trigger one prewarm call, got %d", len(*calls))
	}
}

// ----- skills import on install-hooks -----

// TestSkillsImport_DefaultRunsImport verifies that a clean install (no
// --no-skills-import flag) invokes the skills-import step exactly once
// with the config's endpoint + model and renders the success line.
func TestSkillsImport_DefaultRunsImport(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	cfg := config.Config{OllamaEndpoint: "http://stub:11434", Model: "test-model"}

	// Prewarm stub keeps the install isolated; its cleanup runs first.
	stubPrewarm(t, func(ctx context.Context, endpoint, model string) prewarmResult {
		return prewarmResult{model: model, duration: 3 * time.Millisecond}
	})
	// Override the default no-op skills-import stub so we can observe +
	// drive its return value.
	calls := stubSkillsImport(t, func(ctx context.Context, cfg config.Config, env map[string]string) skillsImportResult {
		return skillsImportResult{
			dir: filepath.Join(home, ".claude", "skills"),
			result: heimdallSkillImportOK(t, 3, 1, 0),
			duration: 42 * time.Millisecond,
		}
	})

	var stdout, stderr bytes.Buffer
	code := CLIInstallHooks(cfg, nil, &stdout, &stderr, env, []string{"--scope=user"})
	if code != 0 {
		t.Fatalf("install exit=%d stderr=%s", code, stderr.String())
	}
	if got := len(*calls); got != 1 {
		t.Fatalf("expected exactly 1 skills-import call, got %d", got)
	}
	c := (*calls)[0]
	if c.endpoint != cfg.OllamaEndpoint {
		t.Errorf("skills import endpoint = %q, want %q", c.endpoint, cfg.OllamaEndpoint)
	}
	if c.model != cfg.Model {
		t.Errorf("skills import model = %q, want %q", c.model, cfg.Model)
	}
	out := stdout.String()
	// 3 created + 1 updated = 4 imported.
	if !strings.Contains(out, "skills import: imported 4 skills in 42ms") {
		t.Errorf("expected success line in stdout, got: %s", out)
	}
	if !strings.Contains(out, "created=3 updated=1 unchanged=0") {
		t.Errorf("expected breakdown in stdout, got: %s", out)
	}
}

// TestSkillsImport_NoSkillsImportFlagSkips verifies that --no-skills-import
// bypasses the skills-import step entirely (no recorded calls) and prints
// the disabled status line.
func TestSkillsImport_NoSkillsImportFlagSkips(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	cfg := config.Config{OllamaEndpoint: "http://stub:11434", Model: "test-model"}

	stubPrewarm(t, func(ctx context.Context, endpoint, model string) prewarmResult {
		return prewarmResult{model: model, duration: 1 * time.Millisecond}
	})
	calls := stubSkillsImport(t, func(ctx context.Context, cfg config.Config, env map[string]string) skillsImportResult {
		t.Errorf("skills import must not be called when --no-skills-import is set")
		return skillsImportResult{}
	})

	var stdout, stderr bytes.Buffer
	code := CLIInstallHooks(cfg, nil, &stdout, &stderr, env, []string{"--scope=user", "--no-skills-import"})
	if code != 0 {
		t.Fatalf("install exit=%d stderr=%s", code, stderr.String())
	}
	if len(*calls) != 0 {
		t.Fatalf("expected zero skills-import calls under --no-skills-import, got %d", len(*calls))
	}
	if !strings.Contains(stdout.String(), "skills import: skipped (--no-skills-import)") {
		t.Errorf("expected --no-skills-import skip line in stdout: %s", stdout.String())
	}
}

// TestSkillsImport_DryRunAnnouncesIntent verifies that --dry-run does not
// invoke the import function but does announce the resolved skills dir so
// the user can preview the planned side-effect.
func TestSkillsImport_DryRunAnnouncesIntent(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	cfg := config.Config{OllamaEndpoint: "http://stub:11434", Model: "test-model"}

	// Pin the skills dir so the assertion is robust.
	skillsDir := filepath.Join(home, "my-skills")
	env["HEIMDALL_CLAUDE_SKILLS_DIR"] = skillsDir

	stubPrewarm(t, func(ctx context.Context, endpoint, model string) prewarmResult {
		return prewarmResult{model: model, duration: 1 * time.Millisecond}
	})
	calls := stubSkillsImport(t, func(ctx context.Context, cfg config.Config, env map[string]string) skillsImportResult {
		t.Errorf("skills import must not be called in --dry-run mode")
		return skillsImportResult{}
	})

	var stdout, stderr bytes.Buffer
	code := CLIInstallHooks(cfg, nil, &stdout, &stderr, env, []string{"--scope=user", "--dry-run"})
	if code != 0 {
		t.Fatalf("dry-run install exit=%d stderr=%s", code, stderr.String())
	}
	if len(*calls) != 0 {
		t.Fatalf("expected zero skills-import calls in --dry-run, got %d", len(*calls))
	}
	out := stdout.String()
	// Dry-run announces intent — not the success line, which the import
	// never produced.
	if !strings.Contains(out, "Would run skills import from "+skillsDir) {
		t.Errorf("expected dry-run announcement in stdout, got: %s", out)
	}
	if strings.Contains(out, "imported") {
		t.Errorf("dry-run must not print a success line, got: %s", out)
	}
}

// TestSkillsImport_DryRunWithNoSkillsImportSuppresses verifies that a
// --dry-run + --no-skills-import combo does not even announce the intent.
func TestSkillsImport_DryRunWithNoSkillsImportSuppresses(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	cfg := config.Config{OllamaEndpoint: "http://stub:11434", Model: "test-model"}

	stubPrewarm(t, func(ctx context.Context, endpoint, model string) prewarmResult {
		return prewarmResult{model: model, duration: 1 * time.Millisecond}
	})
	stubSkillsImport(t, func(ctx context.Context, cfg config.Config, env map[string]string) skillsImportResult {
		t.Errorf("skills import must not be called")
		return skillsImportResult{}
	})

	var stdout, stderr bytes.Buffer
	code := CLIInstallHooks(cfg, nil, &stdout, &stderr, env, []string{"--scope=user", "--dry-run", "--no-skills-import"})
	if code != 0 {
		t.Fatalf("dry-run install exit=%d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "Would run skills import") {
		t.Errorf("--no-skills-import should suppress the dry-run announcement, got: %s", out)
	}
}

// TestSkillsImport_FailureDoesNotFailInstall checks that an erroring
// import surfaces a one-line "(non-fatal)" message but lets the install
// succeed with exit 0 and a valid settings.json on disk.
func TestSkillsImport_FailureDoesNotFailInstall(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	cfg := config.Config{OllamaEndpoint: "http://stub:11434", Model: "test-model"}

	stubPrewarm(t, func(ctx context.Context, endpoint, model string) prewarmResult {
		return prewarmResult{model: model, duration: 1 * time.Millisecond}
	})
	stubSkillsImport(t, func(ctx context.Context, cfg config.Config, env map[string]string) skillsImportResult {
		return skillsImportResult{
			err:      errors.New("ollama not reachable at http://stub:11434: connection refused"),
			duration: 5 * time.Millisecond,
		}
	})

	var stdout, stderr bytes.Buffer
	code := CLIInstallHooks(cfg, nil, &stdout, &stderr, env, []string{"--scope=user"})
	if code != 0 {
		t.Fatalf("install must not fail on skills-import error: exit=%d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "skills import: ") {
		t.Errorf("expected skills-import status line, got: %s", out)
	}
	if !strings.Contains(out, "(non-fatal)") {
		t.Errorf("expected (non-fatal) marker on error, got: %s", out)
	}
	if !strings.Contains(out, "connection refused") {
		t.Errorf("expected error reason surfaced in status line, got: %s", out)
	}
	// Hooks must still be installed.
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	if _, err := os.Stat(settingsPath); err != nil {
		t.Errorf("settings.json should be created even when skills import fails: %v", err)
	}
}

// TestSkillsImport_NoModelConfiguredSkips verifies that an empty cfg.Model
// is treated as a soft skip with no call to the import function, mirroring
// the prewarm behavior. The install should still succeed since we cannot
// embed without a model.
func TestSkillsImport_NoModelConfiguredSkips(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	cfg := config.Config{} // Model intentionally empty

	stubPrewarm(t, func(ctx context.Context, endpoint, model string) prewarmResult {
		t.Errorf("prewarm must not be called when cfg.Model is empty")
		return prewarmResult{}
	})
	calls := stubSkillsImport(t, func(ctx context.Context, cfg config.Config, env map[string]string) skillsImportResult {
		t.Errorf("skills import must not be called when cfg.Model is empty")
		return skillsImportResult{}
	})

	var stdout, stderr bytes.Buffer
	code := CLIInstallHooks(cfg, nil, &stdout, &stderr, env, []string{"--scope=user"})
	if code != 0 {
		t.Fatalf("install exit=%d stderr=%s", code, stderr.String())
	}
	if len(*calls) != 0 {
		t.Fatalf("expected zero skills-import calls when no model configured, got %d", len(*calls))
	}
	if !strings.Contains(stdout.String(), "skills import: skipped (no model configured)") {
		t.Errorf("expected no-model skip line in stdout: %s", stdout.String())
	}
}

// TestSkillsImport_FiresOnForce verifies the skills-import step runs under
// --force, matching the spec: re-installing should always end with both a
// warm model and a synced skills memory store.
func TestSkillsImport_FiresOnForce(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(settingsPath), 0o755)

	// Pre-existing foreign hook so --force has something to replace.
	conflicting := map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{"type": "command", "command": "/usr/local/bin/other-tool"},
					},
				},
			},
		},
	}
	b, _ := json.MarshalIndent(conflicting, "", "  ")
	os.WriteFile(settingsPath, b, 0o600)

	cfg := config.Config{OllamaEndpoint: "http://stub:11434", Model: "test-model"}
	stubPrewarm(t, func(ctx context.Context, endpoint, model string) prewarmResult {
		return prewarmResult{model: model, duration: 5 * time.Millisecond}
	})
	calls := stubSkillsImport(t, func(ctx context.Context, cfg config.Config, env map[string]string) skillsImportResult {
		return skillsImportResult{result: heimdallSkillImportOK(t, 0, 0, 5), duration: 8 * time.Millisecond}
	})

	var stdout, stderr bytes.Buffer
	code := CLIInstallHooks(cfg, nil, &stdout, &stderr, env, []string{"--scope=user", "--force"})
	if code != 0 {
		t.Fatalf("install --force exit=%d stderr=%s", code, stderr.String())
	}
	if len(*calls) != 1 {
		t.Fatalf("--force should still trigger one skills-import call, got %d", len(*calls))
	}
	if !strings.Contains(stdout.String(), "skills import: imported 0 skills in 8ms") {
		t.Errorf("expected skills-import success line on --force, got: %s", stdout.String())
	}
}

// heimdallSkillImportOK constructs a populated SkillImportResult without
// pulling in the full heimdall package surface area for every test. Keeping
// it local avoids a test-only shim in the heimdall package.
func heimdallSkillImportOK(t *testing.T, created, updated, unchanged int) heimdallSkillImportResultAlias {
	t.Helper()
	return heimdallSkillImportResultAlias{
		ScannedDirs: created + updated + unchanged,
		FilesFound:  created + updated + unchanged,
		Created:     created,
		Updated:     updated,
		Unchanged:   unchanged,
	}
}

// heimdallSkillImportResultAlias re-exports the heimdall struct under a
// test-local name so the helper signature above reads cleanly. Using the
// real type directly would force every call site to `import heimdall`,
// which is fine but noisier.
type heimdallSkillImportResultAlias = heimdall.SkillImportResult
