package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// fakeMemStore returns an OpenMemoryStore closure pointing at a temp DB
// path AND a small helper to reopen the same DB for assertions. The CLI
// handler always defers Close() on its store, so we cannot reuse the same
// *MemoryStore in the test — instead, every call to openStore returns a
// freshly opened handle on the same on-disk file.
func fakeMemStore(t *testing.T) (string, func() (*heimdall.MemoryStore, error)) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "memories.db")
	return dbPath, func() (*heimdall.MemoryStore, error) {
		return heimdall.OpenMemoryStore(dbPath)
	}
}

// readMemStore opens the per-test DB for read-back assertions. Closes itself
// when the test finishes.
func readMemStore(t *testing.T, dbPath string) *heimdall.MemoryStore {
	t.Helper()
	store, err := heimdall.OpenMemoryStore(dbPath)
	if err != nil {
		t.Fatalf("reopen mem store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// fakeEmbedderFactory returns a NewEmbedder that hands back a deterministic
// stub Embed func. Lets the CLI test the import path without touching Ollama.
func fakeEmbedderFactory() func(ctx context.Context, endpoint, model string) (func(string) ([]float32, error), error) {
	return func(ctx context.Context, endpoint, model string) (func(string) ([]float32, error), error) {
		return func(content string) ([]float32, error) {
			return []float32{1, float32(len(content)), 0}, nil
		}, nil
	}
}

// copyTestdataSkills materializes the testdata/skills/ tree into a temp dir
// the test owns. Avoids any chance of a test mutating files in the repo.
func copyTestdataSkills(t *testing.T) string {
	t.Helper()
	src := filepath.Join("testdata", "skills")
	dst := t.TempDir()
	if err := copyDir(src, dst); err != nil {
		t.Fatalf("copy testdata/skills: %v", err)
	}
	return dst
}

func copyDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := os.MkdirAll(d, 0o755); err != nil {
				return err
			}
			if err := copyDir(s, d); err != nil {
				return err
			}
			continue
		}
		data, err := os.ReadFile(s)
		if err != nil {
			return err
		}
		if err := os.WriteFile(d, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func TestCLISkills_NoArgsUsage(t *testing.T) {
	var out, errb bytes.Buffer
	code := CLISkills(config.Config{}, nil, &out, &errb, nil, nil, SkillsDeps{})
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "Usage:") {
		t.Errorf("missing usage hint, got %q", errb.String())
	}
}

func TestCLISkills_UnknownSubcommand(t *testing.T) {
	var out, errb bytes.Buffer
	code := CLISkills(config.Config{}, nil, &out, &errb, nil, []string{"banana"}, SkillsDeps{})
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "Unknown skills subcommand") {
		t.Errorf("missing error, got %q", errb.String())
	}
}

func TestCLISkills_HelpExits0(t *testing.T) {
	var out, errb bytes.Buffer
	code := CLISkills(config.Config{}, nil, &out, &errb, nil, []string{"--help"}, SkillsDeps{})
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "skills import") {
		t.Errorf("help should mention import: %q", out.String())
	}
}

func TestCLISkills_ImportHappyPath(t *testing.T) {
	dir := copyTestdataSkills(t)
	dbPath, openStore := fakeMemStore(t)
	cfg := config.Config{OllamaEndpoint: "http://stub", Model: "stub-model"}

	var out, errb bytes.Buffer
	code := CLISkills(cfg, nil, &out, &errb, nil,
		[]string{"import", "--dir", dir},
		SkillsDeps{
			OpenMemoryStore: openStore,
			NewEmbedder:     fakeEmbedderFactory(),
		},
	)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "created:   2") {
		t.Errorf("expected 2 created in output, got %q", out.String())
	}
	store := readMemStore(t, dbPath)
	// Hidden directory must not be imported.
	if got, _ := store.GetMemoryByID(heimdall.SkillMemoryID("hidden-skill")); got != nil {
		t.Errorf("hidden skill should be skipped, got %+v", got)
	}
	// Sanity check: 'sample-skill' landed.
	mem, _ := store.GetMemoryByID(heimdall.SkillMemoryID("sample-skill"))
	if mem == nil {
		t.Fatalf("sample-skill not stored")
	}
	if mem.Type != heimdall.MemoryTypeSkill {
		t.Errorf("type = %q, want skill", mem.Type)
	}
}

func TestCLISkills_ImportRespectsEnvVar(t *testing.T) {
	dir := copyTestdataSkills(t)
	dbPath, openStore := fakeMemStore(t)
	cfg := config.Config{OllamaEndpoint: "http://stub", Model: "stub-model"}

	var out, errb bytes.Buffer
	code := CLISkills(cfg, nil, &out, &errb,
		map[string]string{heimdall.ClaudeSkillsDirEnv: dir},
		[]string{"import"},
		SkillsDeps{
			OpenMemoryStore: openStore,
			NewEmbedder:     fakeEmbedderFactory(),
		},
	)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, errb.String())
	}
	store := readMemStore(t, dbPath)
	if store.MemoryCount() != 2 {
		t.Errorf("memory count = %d, want 2", store.MemoryCount())
	}
}

func TestCLISkills_ImportDryRunNoWrites(t *testing.T) {
	dir := copyTestdataSkills(t)
	dbPath, openStore := fakeMemStore(t)
	cfg := config.Config{}

	var out, errb bytes.Buffer
	code := CLISkills(cfg, nil, &out, &errb, nil,
		[]string{"import", "--dir", dir, "--dry-run"},
		SkillsDeps{OpenMemoryStore: openStore},
	)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, errb.String())
	}
	store := readMemStore(t, dbPath)
	if store.MemoryCount() != 0 {
		t.Errorf("dry run wrote memories: %d", store.MemoryCount())
	}
	if !strings.Contains(out.String(), "(dry-run)") {
		t.Errorf("output should mark dry-run: %q", out.String())
	}
}

func TestCLISkills_ImportJSONFormat(t *testing.T) {
	dir := copyTestdataSkills(t)
	_, openStore := fakeMemStore(t)
	cfg := config.Config{OllamaEndpoint: "http://stub", Model: "stub-model"}

	var out, errb bytes.Buffer
	code := CLISkills(cfg, nil, &out, &errb, nil,
		[]string{"import", "--dir", dir, "--format", "json"},
		SkillsDeps{
			OpenMemoryStore: openStore,
			NewEmbedder:     fakeEmbedderFactory(),
		},
	)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, errb.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("invalid JSON output: %v\n%s", err, out.String())
	}
	if got := int(payload["created"].(float64)); got != 2 {
		t.Errorf("created = %d, want 2", got)
	}
	if payload["dir"].(string) != dir {
		t.Errorf("dir mismatch: %v vs %v", payload["dir"], dir)
	}
}

func TestCLISkills_ImportIdempotentOnRerun(t *testing.T) {
	dir := copyTestdataSkills(t)
	dbPath, openStore := fakeMemStore(t)
	cfg := config.Config{OllamaEndpoint: "http://stub", Model: "stub-model"}
	deps := SkillsDeps{
		OpenMemoryStore: openStore,
		NewEmbedder:     fakeEmbedderFactory(),
	}

	for i := 0; i < 2; i++ {
		var out, errb bytes.Buffer
		code := CLISkills(cfg, nil, &out, &errb, nil,
			[]string{"import", "--dir", dir, "--format", "json"}, deps)
		if code != 0 {
			t.Fatalf("iteration %d: code=%d stderr=%q", i, code, errb.String())
		}
		var payload map[string]any
		_ = json.Unmarshal(out.Bytes(), &payload)
		if i == 0 {
			if int(payload["created"].(float64)) != 2 {
				t.Errorf("first run created=%v, want 2", payload["created"])
			}
		} else {
			if int(payload["created"].(float64)) != 0 {
				t.Errorf("second run created=%v, want 0 (idempotent)", payload["created"])
			}
			if int(payload["unchanged"].(float64)) != 2 {
				t.Errorf("second run unchanged=%v, want 2", payload["unchanged"])
			}
		}
	}
	store := readMemStore(t, dbPath)
	if store.MemoryCount() != 2 {
		t.Errorf("final memory count = %d, want 2", store.MemoryCount())
	}
}

func TestCLISkills_ImportInvalidFormatReturns2(t *testing.T) {
	_, openStore := fakeMemStore(t)
	var out, errb bytes.Buffer
	code := CLISkills(config.Config{}, nil, &out, &errb, nil,
		[]string{"import", "--dir", t.TempDir(), "--format", "yaml"},
		SkillsDeps{OpenMemoryStore: openStore})
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
}

func TestCLISkills_ImportMissingDirIsSilentSuccess(t *testing.T) {
	dbPath, openStore := fakeMemStore(t)
	cfg := config.Config{}
	var out, errb bytes.Buffer
	code := CLISkills(cfg, nil, &out, &errb, nil,
		[]string{"import", "--dir", filepath.Join(t.TempDir(), "missing"), "--dry-run"},
		SkillsDeps{OpenMemoryStore: openStore})
	if code != 0 {
		t.Errorf("code = %d (want 0): stderr=%q", code, errb.String())
	}
	store := readMemStore(t, dbPath)
	if store.MemoryCount() != 0 {
		t.Errorf("memory count = %d, want 0", store.MemoryCount())
	}
}

func TestCLISkills_ImportEmbedFailureReportedNotFatal(t *testing.T) {
	dir := copyTestdataSkills(t)
	_, openStore := fakeMemStore(t)
	cfg := config.Config{}
	var out, errb bytes.Buffer
	deps := SkillsDeps{
		OpenMemoryStore: openStore,
		NewEmbedder: func(ctx context.Context, endpoint, model string) (func(string) ([]float32, error), error) {
			return func(string) ([]float32, error) { return nil, errors.New("nope") }, nil
		},
	}
	code := CLISkills(cfg, nil, &out, &errb, nil,
		[]string{"import", "--dir", dir, "--format", "json"}, deps)
	if code != 0 {
		t.Errorf("code = %d (want 0): stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "skill import warning") {
		t.Errorf("expected per-file warning on stderr, got %q", errb.String())
	}
}

// TestDoctor_SkillsSyncRollupReportsDrift verifies the new check #12 in
// `hooks doctor` reports drift when disk SKILL.md count != synced memory rows.
func TestDoctor_SkillsSyncRollupReportsDrift(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{
		"HOME":               home,
		"HEIMDALL_TEST_HOME": home,
		"HEIMDALL_TEST_CWD":  home,
		"XDG_STATE_HOME":     filepath.Join(home, "state"),
		"PATH":               os.Getenv("PATH"),
	}
	settingsPath := filepath.Join(home, "settings.json")
	if err := os.WriteFile(settingsPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Two SKILL.md on disk, zero in the empty memory DB.
	skillsDir := filepath.Join(home, "skills")
	for _, name := range []string{"alpha", "beta"} {
		_ = os.MkdirAll(filepath.Join(skillsDir, name), 0o755)
		_ = os.WriteFile(filepath.Join(skillsDir, name, "SKILL.md"),
			[]byte("---\nname: "+name+"\n---\nbody\n"), 0o644)
	}

	deps := doctorDeps{
		settingsPath: settingsPath,
		lookupPath:   func(string) (string, error) { return "/fake/heimdall-mcp", nil },
		runVersion:   func(string) error { return nil },
		pingOllama:   func(_ context.Context, _ string) error { return nil },
		listModels:   func(_ context.Context, _ string) ([]string, error) { return []string{"x"}, nil },
		dryFire:      func(string) error { return nil },
		hookLogPath:  filepath.Join(home, "hooks.log"),
		skillsDir:    skillsDir,
		memoryDBPath: filepath.Join(home, "memory.db"),
	}
	checks := runDoctorChecks(config.Config{Model: "x"}, env, deps)
	var skills *doctorCheck
	for i := range checks {
		if checks[i].name == "skills sync" {
			skills = &checks[i]
			break
		}
	}
	if skills == nil {
		t.Fatalf("skills sync check missing; got %d checks", len(checks))
	}
	if !strings.Contains(skills.message, "0/2") {
		t.Errorf("expected 0/2 drift report, got %q", skills.message)
	}
}

func TestCLISkills_ImportNewEmbedderInitFailureExits1(t *testing.T) {
	dir := copyTestdataSkills(t)
	_, openStore := fakeMemStore(t)
	cfg := config.Config{}
	var out, errb bytes.Buffer
	deps := SkillsDeps{
		OpenMemoryStore: openStore,
		NewEmbedder: func(ctx context.Context, endpoint, model string) (func(string) ([]float32, error), error) {
			return nil, errors.New("init failed")
		},
	}
	code := CLISkills(cfg, nil, &out, &errb, nil,
		[]string{"import", "--dir", dir}, deps)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
}
