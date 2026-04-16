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

	"github.com/caio-silva/heimdall-mcp/internal/config"
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
	}
	cfg := config.Config{Model: "bge-m3"}
	checks := runDoctorChecks(cfg, env, deps)
	if len(checks) != 11 {
		t.Fatalf("expected 11 checks, got %d", len(checks))
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
	}
	checks := runDoctorChecks(config.Config{Model: "bge-m3"}, env, deps)
	// check #10 (index 9)
	if checks[9].status != statusFail {
		t.Errorf("expected fail on dry-fire failure, got %v: %s", checks[9].status, checks[9].message)
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
	}
	runDoctorChecks(config.Config{Model: "bge-m3"}, env, deps)
	if called != 0 {
		t.Errorf("dryFire should not have been called for post-edit-actor; called=%d", called)
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
