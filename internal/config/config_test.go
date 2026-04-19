package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfig_NewFields(t *testing.T) {
	cfg := DefaultConfig()

	if !cfg.GitEnabled {
		t.Error("expected GitEnabled=true by default")
	}
	if cfg.GitDepth != 200 {
		t.Errorf("expected GitDepth=200, got %d", cfg.GitDepth)
	}
	if cfg.GitIncludeDiffs {
		t.Error("expected GitIncludeDiffs=false by default")
	}
	if len(cfg.GitBranches) != 0 {
		t.Errorf("expected empty GitBranches, got %v", cfg.GitBranches)
	}
	if cfg.StaleTimeoutMin != 30 {
		t.Errorf("expected StaleTimeoutMin=30, got %d", cfg.StaleTimeoutMin)
	}
	if cfg.LifecycleActiveDays != 30 {
		t.Errorf("expected LifecycleActiveDays=30, got %d", cfg.LifecycleActiveDays)
	}
	if cfg.LifecycleArchiveDays != 90 {
		t.Errorf("expected LifecycleArchiveDays=90, got %d", cfg.LifecycleArchiveDays)
	}
	if cfg.MaxChunksPerProject != 10000 {
		t.Errorf("expected MaxChunksPerProject=10000, got %d", cfg.MaxChunksPerProject)
	}
	if len(cfg.IndexedPaths) != 0 {
		t.Errorf("expected empty IndexedPaths, got %v", cfg.IndexedPaths)
	}

	wantExcludes := map[string]bool{
		".git":              true,
		"node_modules":      true,
		"vendor":            true,
		".heimdall_db":      true,
		"__pycache__":       true,
		".idea":             true,
		".claude/worktrees": true,
	}
	got := make(map[string]bool, len(cfg.ExcludePatterns))
	for _, p := range cfg.ExcludePatterns {
		got[p] = true
	}
	for want := range wantExcludes {
		if !got[want] {
			t.Errorf("default ExcludePatterns missing %q (got %v)", want, cfg.ExcludePatterns)
		}
	}
}

func TestLoadConfig_AppliesNewDefaults(t *testing.T) {
	// Create a config file with only old fields (simulating pre-Phase5 config)
	tmpDir := t.TempDir()
	configDir := filepath.Join(tmpDir, "heimdall-mcp")
	os.MkdirAll(configDir, 0700)
	configPath := filepath.Join(configDir, "config.json")

	oldConfig := map[string]any{
		"ollamaEndpoint":   "http://myhost:11434",
		"model":            "bge-m3",
		"contextDepth":     2,
		"maxContextTokens": 8192,
	}
	data, _ := json.Marshal(oldConfig)
	os.WriteFile(configPath, data, 0600)

	// Point config resolution to our temp dir
	t.Setenv("HEIMDALL_MCP_CONFIG", configPath)

	cfg := LoadConfig()

	// Old fields should be preserved
	if cfg.OllamaEndpoint != "http://myhost:11434" {
		t.Errorf("expected custom endpoint, got %s", cfg.OllamaEndpoint)
	}
	if cfg.ContextDepth != 2 {
		t.Errorf("expected contextDepth=2, got %d", cfg.ContextDepth)
	}

	// New fields should have defaults applied
	if !cfg.GitEnabled {
		t.Error("expected GitEnabled default true")
	}
	if cfg.GitDepth != 200 {
		t.Errorf("expected GitDepth default 200, got %d", cfg.GitDepth)
	}
	if cfg.StaleTimeoutMin != 30 {
		t.Errorf("expected StaleTimeoutMin default 30, got %d", cfg.StaleTimeoutMin)
	}
	if cfg.MaxChunksPerProject != 10000 {
		t.Errorf("expected MaxChunksPerProject default 10000, got %d", cfg.MaxChunksPerProject)
	}
}

func TestLoadConfig_PreservesExplicitNewFields(t *testing.T) {
	tmpDir := t.TempDir()
	configDir := filepath.Join(tmpDir, "heimdall-mcp")
	os.MkdirAll(configDir, 0700)
	configPath := filepath.Join(configDir, "config.json")

	fullConfig := map[string]any{
		"ollamaEndpoint":       "http://localhost:11434",
		"model":                "bge-m3",
		"gitEnabled":           false,
		"gitDepth":             50,
		"gitIncludeDiffs":      true,
		"gitBranches":          []string{"main", "develop"},
		"staleTimeoutMinutes":  60,
		"lifecycleActiveDays":  14,
		"lifecycleArchiveDays": 60,
		"maxChunksPerProject":  5000,
		"indexedPaths":         []string{"/home/user/project"},
	}
	data, _ := json.Marshal(fullConfig)
	os.WriteFile(configPath, data, 0600)

	t.Setenv("HEIMDALL_MCP_CONFIG", configPath)

	cfg := LoadConfig()

	if cfg.GitEnabled {
		t.Error("expected GitEnabled=false (explicitly set)")
	}
	if cfg.GitDepth != 50 {
		t.Errorf("expected GitDepth=50, got %d", cfg.GitDepth)
	}
	if !cfg.GitIncludeDiffs {
		t.Error("expected GitIncludeDiffs=true")
	}
	if len(cfg.GitBranches) != 2 {
		t.Errorf("expected 2 branches, got %d", len(cfg.GitBranches))
	}
	if cfg.StaleTimeoutMin != 60 {
		t.Errorf("expected StaleTimeoutMin=60, got %d", cfg.StaleTimeoutMin)
	}
	if cfg.LifecycleActiveDays != 14 {
		t.Errorf("expected LifecycleActiveDays=14, got %d", cfg.LifecycleActiveDays)
	}
	if cfg.LifecycleArchiveDays != 60 {
		t.Errorf("expected LifecycleArchiveDays=60, got %d", cfg.LifecycleArchiveDays)
	}
	if cfg.MaxChunksPerProject != 5000 {
		t.Errorf("expected MaxChunksPerProject=5000, got %d", cfg.MaxChunksPerProject)
	}
	if len(cfg.IndexedPaths) != 1 {
		t.Errorf("expected 1 indexed path, got %d", len(cfg.IndexedPaths))
	}
}

func TestSaveConfig_RoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	t.Setenv("HEIMDALL_MCP_CONFIG", configPath)

	cfg := DefaultConfig()
	cfg.GitDepth = 500
	cfg.GitBranches = []string{"main"}
	cfg.IndexedPaths = []string{"/tmp/project1", "/tmp/project2"}

	err := SaveConfig(cfg)
	if err != nil {
		t.Fatalf("SaveConfig failed: %v", err)
	}

	// Verify file was created
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("config file not created: %v", err)
	}

	// Load it back
	loaded := LoadConfig()
	if loaded.GitDepth != 500 {
		t.Errorf("expected GitDepth=500 after round-trip, got %d", loaded.GitDepth)
	}
	if len(loaded.GitBranches) != 1 || loaded.GitBranches[0] != "main" {
		t.Errorf("expected GitBranches=[main], got %v", loaded.GitBranches)
	}
	if len(loaded.IndexedPaths) != 2 {
		t.Errorf("expected 2 indexed paths, got %d", len(loaded.IndexedPaths))
	}
}

func TestSaveConfig_CreatesDir(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "subdir", "deep", "config.json")
	t.Setenv("HEIMDALL_MCP_CONFIG", configPath)

	cfg := DefaultConfig()
	err := SaveConfig(cfg)
	if err != nil {
		t.Fatalf("SaveConfig failed: %v", err)
	}

	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("config file not created in nested dir: %v", err)
	}
}

// -----------------------------------------------------------------------
// Embed concurrency / timeout / retry env-var override tests
// (PR5: closes handoff Problem #5 — parallel Ollama bursts deadline-exceed)
//
// After PR74 review H1 fix: env vars are NO LONGER applied by LoadConfig
// (which would bake them into config.json on next SaveConfig). They are
// applied by ResolveEmbedConfig, which returns a transient copy.
// -----------------------------------------------------------------------

// TestLoadConfig_DoesNotMutateForEnvOverrides: LoadConfig returns the
// on-disk view unchanged by HEIMDALL_EMBED_* env vars. This is a
// regression test for the env-persistence hazard (see review H1).
func TestLoadConfig_DoesNotMutateForEnvOverrides(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)
	t.Setenv("HEIMDALL_MCP_CONFIG", "")
	t.Setenv("HEIMDALL_EMBED_MAX_CONCURRENT", "5")
	t.Setenv("HEIMDALL_EMBED_TIMEOUT_MS", "7500")
	t.Setenv("HEIMDALL_EMBED_MAX_RETRIES", "3")

	cfg := LoadConfig()

	// LoadConfig MUST reflect only the defaults (sentinels), not env.
	if cfg.EmbedMaxConcurrent != sentinelUnset {
		t.Errorf("EmbedMaxConcurrent = %d, want %d (env must not mutate LoadConfig)", cfg.EmbedMaxConcurrent, sentinelUnset)
	}
	if cfg.EmbedTimeoutMs != 0 {
		t.Errorf("EmbedTimeoutMs = %d, want 0 (env must not mutate LoadConfig)", cfg.EmbedTimeoutMs)
	}
	if cfg.EmbedMaxRetries != sentinelUnset {
		t.Errorf("EmbedMaxRetries = %d, want %d (env must not mutate LoadConfig)", cfg.EmbedMaxRetries, sentinelUnset)
	}
}

// TestResolveEmbedConfig_AppliesEnvOverrides: env vars take effect at
// resolution time, not load time.
func TestResolveEmbedConfig_AppliesEnvOverrides(t *testing.T) {
	t.Setenv("HEIMDALL_EMBED_MAX_CONCURRENT", "5")
	t.Setenv("HEIMDALL_EMBED_TIMEOUT_MS", "7500")
	t.Setenv("HEIMDALL_EMBED_MAX_RETRIES", "3")

	base := DefaultConfig()
	cfg := ResolveEmbedConfig(base)

	if cfg.EmbedMaxConcurrent != 5 {
		t.Errorf("EmbedMaxConcurrent = %d, want 5", cfg.EmbedMaxConcurrent)
	}
	if cfg.EmbedTimeoutMs != 7500 {
		t.Errorf("EmbedTimeoutMs = %d, want 7500", cfg.EmbedTimeoutMs)
	}
	if cfg.EmbedMaxRetries != 3 {
		t.Errorf("EmbedMaxRetries = %d, want 3", cfg.EmbedMaxRetries)
	}

	// Critical: base must NOT have been mutated.
	if base.EmbedMaxConcurrent != sentinelUnset {
		t.Errorf("base.EmbedMaxConcurrent = %d, want %d (ResolveEmbedConfig must not mutate input)", base.EmbedMaxConcurrent, sentinelUnset)
	}
	if base.EmbedTimeoutMs != 0 {
		t.Errorf("base.EmbedTimeoutMs = %d, want 0 (ResolveEmbedConfig must not mutate input)", base.EmbedTimeoutMs)
	}
	if base.EmbedMaxRetries != sentinelUnset {
		t.Errorf("base.EmbedMaxRetries = %d, want %d (ResolveEmbedConfig must not mutate input)", base.EmbedMaxRetries, sentinelUnset)
	}
}

// TestResolveEmbedConfig_MalformedIgnored: malformed env values log and
// are ignored; base config values carry through unchanged.
func TestResolveEmbedConfig_MalformedIgnored(t *testing.T) {
	t.Setenv("HEIMDALL_EMBED_MAX_CONCURRENT", "not-a-number")
	t.Setenv("HEIMDALL_EMBED_TIMEOUT_MS", "")
	t.Setenv("HEIMDALL_EMBED_MAX_RETRIES", "-1")

	base := Config{
		EmbedMaxConcurrent: 4,
		EmbedTimeoutMs:     10000,
		EmbedMaxRetries:    2,
	}
	cfg := ResolveEmbedConfig(base)

	if cfg.EmbedMaxConcurrent != 4 {
		t.Errorf("EmbedMaxConcurrent = %d, want 4 (malformed env must be ignored)", cfg.EmbedMaxConcurrent)
	}
	if cfg.EmbedTimeoutMs != 10000 {
		t.Errorf("EmbedTimeoutMs = %d, want 10000 (empty env treated as unset)", cfg.EmbedTimeoutMs)
	}
	if cfg.EmbedMaxRetries != 2 {
		t.Errorf("EmbedMaxRetries = %d, want 2 (negative env rejected)", cfg.EmbedMaxRetries)
	}
}

// TestResolveEmbedConfig_ZeroMeansUnbounded: explicit 0 for
// HEIMDALL_EMBED_MAX_CONCURRENT means "unbounded" per README contract.
// Resolution passes it through unchanged to the client layer.
func TestResolveEmbedConfig_ZeroMeansUnbounded(t *testing.T) {
	t.Setenv("HEIMDALL_EMBED_MAX_CONCURRENT", "0")
	t.Setenv("HEIMDALL_EMBED_MAX_RETRIES", "")

	cfg := ResolveEmbedConfig(DefaultConfig())
	if cfg.EmbedMaxConcurrent != 0 {
		t.Errorf("EmbedMaxConcurrent = %d, want 0 (explicit zero = unbounded)", cfg.EmbedMaxConcurrent)
	}
}

// TestResolveEmbedConfig_NegativeConcurrentRejected: negative env values
// for HEIMDALL_EMBED_MAX_CONCURRENT are rejected at the env layer to
// match HEIMDALL_EMBED_TIMEOUT_MS / HEIMDALL_EMBED_MAX_RETRIES behavior.
// The internal `-1 = use default` sentinel is a struct-level convention
// and must NOT be exposed as a valid env input. See pr74 review N-1.
func TestResolveEmbedConfig_NegativeConcurrentRejected(t *testing.T) {
	t.Setenv("HEIMDALL_EMBED_MAX_CONCURRENT", "-1")
	t.Setenv("HEIMDALL_EMBED_TIMEOUT_MS", "")
	t.Setenv("HEIMDALL_EMBED_MAX_RETRIES", "")

	base := Config{
		EmbedMaxConcurrent: 4,
		EmbedTimeoutMs:     10000,
		EmbedMaxRetries:    2,
	}
	cfg := ResolveEmbedConfig(base)

	if cfg.EmbedMaxConcurrent != 4 {
		t.Errorf("EmbedMaxConcurrent = %d, want 4 (negative env must be rejected, base value preserved)", cfg.EmbedMaxConcurrent)
	}

	// Also guard against the more aggressive negative (e.g. -2) to
	// ensure we're not special-casing only the sentinel.
	t.Setenv("HEIMDALL_EMBED_MAX_CONCURRENT", "-42")
	cfg2 := ResolveEmbedConfig(base)
	if cfg2.EmbedMaxConcurrent != 4 {
		t.Errorf("EmbedMaxConcurrent = %d, want 4 (arbitrary negative env must be rejected)", cfg2.EmbedMaxConcurrent)
	}
}

// TestApplyEnvOverrides_DoesNotMutatePersistedConfig: regression guard
// for the pr74-review-security-config.md H1 finding. Setting an env var,
// resolving, and then saving the base config MUST NOT leak env values
// to disk.
func TestApplyEnvOverrides_DoesNotMutatePersistedConfig(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	t.Setenv("HEIMDALL_MCP_CONFIG", configPath)

	// Seed an on-disk config with NO embed tuning fields.
	seed := map[string]any{
		"model":   "bge-m3",
		"gitDepth": 50,
	}
	data, _ := json.Marshal(seed)
	_ = os.WriteFile(configPath, data, 0600)

	// Set env overrides.
	t.Setenv("HEIMDALL_EMBED_MAX_CONCURRENT", "7")
	t.Setenv("HEIMDALL_EMBED_TIMEOUT_MS", "8000")
	t.Setenv("HEIMDALL_EMBED_MAX_RETRIES", "4")

	loaded := LoadConfig()

	// Caller code (MCP server / CLI) may derive effective embed config
	// via ResolveEmbedConfig — that's a transient, not persisted.
	effective := ResolveEmbedConfig(loaded)
	if effective.EmbedMaxConcurrent != 7 {
		t.Fatalf("effective.EmbedMaxConcurrent = %d, want 7", effective.EmbedMaxConcurrent)
	}

	// Meanwhile the caller saves the (unchanged) Cfg through some
	// unrelated toolConfigure path. loaded itself must not carry env
	// values.
	if loaded.EmbedMaxConcurrent == 7 {
		t.Fatal("loaded.EmbedMaxConcurrent was mutated by env — regression of H1")
	}
	if err := SaveConfig(loaded); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	// Re-read the on-disk JSON as a map and assert it carries NO
	// embed-tuning fields — they'd only be there if LoadConfig had
	// silently baked env values in.
	raw, _ := os.ReadFile(configPath)
	var rawMap map[string]any
	_ = json.Unmarshal(raw, &rawMap)
	if _, ok := rawMap["embedMaxConcurrent"]; ok && rawMap["embedMaxConcurrent"] != float64(sentinelUnset) {
		t.Fatalf("config.json leaked embedMaxConcurrent=%v — env persistence regression", rawMap["embedMaxConcurrent"])
	}
	if v, ok := rawMap["embedTimeoutMs"]; ok && v != float64(0) {
		t.Fatalf("config.json leaked embedTimeoutMs=%v — env persistence regression", v)
	}
	if v, ok := rawMap["embedMaxRetries"]; ok && v != float64(sentinelUnset) {
		t.Fatalf("config.json leaked embedMaxRetries=%v — env persistence regression", v)
	}
}

// TestResolveEmbedConfig_Clamps: values that exceed documented clamp
// bounds are logged and clamped (retry amplification and duration
// overflow prevention). See review M2.
func TestResolveEmbedConfig_Clamps(t *testing.T) {
	// Clear env so clamp tests don't accidentally pick up values from
	// the outer test environment.
	t.Setenv("HEIMDALL_EMBED_MAX_CONCURRENT", "")
	t.Setenv("HEIMDALL_EMBED_TIMEOUT_MS", "")
	t.Setenv("HEIMDALL_EMBED_MAX_RETRIES", "")

	tests := []struct {
		name    string
		in      Config
		wantCon int
		wantTO  int
		wantRet int
	}{
		{
			name:    "all_within_bounds_passthrough",
			in:      Config{EmbedMaxConcurrent: 8, EmbedTimeoutMs: 30000, EmbedMaxRetries: 3},
			wantCon: 8, wantTO: 30000, wantRet: 3,
		},
		{
			name:    "concurrent_over_clamped",
			in:      Config{EmbedMaxConcurrent: 999999, EmbedTimeoutMs: 30000, EmbedMaxRetries: 3},
			wantCon: MaxEmbedConcurrent, wantTO: 30000, wantRet: 3,
		},
		{
			name:    "timeout_overflow_clamped",
			in:      Config{EmbedTimeoutMs: 1 << 40, EmbedMaxConcurrent: 2},
			wantCon: 2, wantTO: MaxEmbedTimeoutMs, wantRet: 0,
		},
		{
			name:    "timeout_below_min_clamped",
			in:      Config{EmbedTimeoutMs: 5, EmbedMaxConcurrent: 2},
			wantCon: 2, wantTO: MinEmbedTimeoutMs, wantRet: 0,
		},
		{
			name:    "retries_over_clamped",
			in:      Config{EmbedMaxRetries: 1000000, EmbedMaxConcurrent: 2, EmbedTimeoutMs: 30000},
			wantCon: 2, wantTO: 30000, wantRet: MaxEmbedRetries,
		},
		{
			name:    "sentinel_minus_one_preserved",
			in:      Config{EmbedMaxConcurrent: -1, EmbedMaxRetries: -1, EmbedTimeoutMs: 0},
			wantCon: -1, wantTO: 0, wantRet: -1,
		},
		{
			name:    "zero_unbounded_preserved",
			in:      Config{EmbedMaxConcurrent: 0, EmbedTimeoutMs: 30000},
			wantCon: 0, wantTO: 30000, wantRet: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveEmbedConfig(tc.in)
			if got.EmbedMaxConcurrent != tc.wantCon {
				t.Errorf("EmbedMaxConcurrent = %d, want %d", got.EmbedMaxConcurrent, tc.wantCon)
			}
			if got.EmbedTimeoutMs != tc.wantTO {
				t.Errorf("EmbedTimeoutMs = %d, want %d", got.EmbedTimeoutMs, tc.wantTO)
			}
			if got.EmbedMaxRetries != tc.wantRet {
				t.Errorf("EmbedMaxRetries = %d, want %d", got.EmbedMaxRetries, tc.wantRet)
			}
		})
	}
}

// TestResolveEmbedConfig_EnvTimeoutClampedAtParse: a pathological env
// value for TIMEOUT_MS (one that would overflow time.Duration) is
// clamped at parse time so no downstream integer overflow is possible.
func TestResolveEmbedConfig_EnvTimeoutClampedAtParse(t *testing.T) {
	// 10^13 ms = ~317 years — well below int64 overflow but orders of
	// magnitude above the documented clamp. Shows clamp takes precedence.
	t.Setenv("HEIMDALL_EMBED_TIMEOUT_MS", "10000000000000")
	t.Setenv("HEIMDALL_EMBED_MAX_CONCURRENT", "")
	t.Setenv("HEIMDALL_EMBED_MAX_RETRIES", "")

	cfg := ResolveEmbedConfig(DefaultConfig())
	if cfg.EmbedTimeoutMs != MaxEmbedTimeoutMs {
		t.Fatalf("EmbedTimeoutMs = %d, want clamp to %d", cfg.EmbedTimeoutMs, MaxEmbedTimeoutMs)
	}
}

// TestLoadConfig_NewDefaultsHaveSentinels: fresh DefaultConfig emits -1
// sentinels for EmbedMaxConcurrent / EmbedMaxRetries so the client layer
// applies its own defaults without colliding with the "0 = unbounded"
// documented contract.
func TestLoadConfig_NewDefaultsHaveSentinels(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.EmbedMaxConcurrent != sentinelUnset {
		t.Errorf("DefaultConfig.EmbedMaxConcurrent = %d, want %d (sentinel)", cfg.EmbedMaxConcurrent, sentinelUnset)
	}
	if cfg.EmbedMaxRetries != sentinelUnset {
		t.Errorf("DefaultConfig.EmbedMaxRetries = %d, want %d (sentinel)", cfg.EmbedMaxRetries, sentinelUnset)
	}
}

// TestLoadConfig_MissingFieldsKeepSentinels: a legacy config.json that
// lacks embedMaxConcurrent / embedMaxRetries yields -1 sentinels after
// load, not 0 (which would mean "explicit unbounded / zero retries").
func TestLoadConfig_MissingFieldsKeepSentinels(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	t.Setenv("HEIMDALL_MCP_CONFIG", configPath)
	// Clear env overrides.
	t.Setenv("HEIMDALL_EMBED_MAX_CONCURRENT", "")
	t.Setenv("HEIMDALL_EMBED_MAX_RETRIES", "")

	seed := map[string]any{"model": "bge-m3"}
	data, _ := json.Marshal(seed)
	_ = os.WriteFile(configPath, data, 0600)

	cfg := LoadConfig()
	if cfg.EmbedMaxConcurrent != sentinelUnset {
		t.Errorf("EmbedMaxConcurrent = %d, want %d (missing field → sentinel)", cfg.EmbedMaxConcurrent, sentinelUnset)
	}
	if cfg.EmbedMaxRetries != sentinelUnset {
		t.Errorf("EmbedMaxRetries = %d, want %d (missing field → sentinel)", cfg.EmbedMaxRetries, sentinelUnset)
	}
}

// TestLoadConfig_ExplicitZeroPreserved: JSON with embedMaxConcurrent=0
// preserves the opt-out (operators who manage concurrency upstream).
func TestLoadConfig_ExplicitZeroPreserved(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	t.Setenv("HEIMDALL_MCP_CONFIG", configPath)
	t.Setenv("HEIMDALL_EMBED_MAX_CONCURRENT", "")
	t.Setenv("HEIMDALL_EMBED_MAX_RETRIES", "")

	seed := map[string]any{
		"embedMaxConcurrent": 0,
		"embedMaxRetries":    0,
	}
	data, _ := json.Marshal(seed)
	_ = os.WriteFile(configPath, data, 0600)

	cfg := LoadConfig()
	if cfg.EmbedMaxConcurrent != 0 {
		t.Errorf("EmbedMaxConcurrent = %d, want 0 (explicit opt-out)", cfg.EmbedMaxConcurrent)
	}
	if cfg.EmbedMaxRetries != 0 {
		t.Errorf("EmbedMaxRetries = %d, want 0 (explicit opt-out)", cfg.EmbedMaxRetries)
	}
}

func TestSaveConfig_NoExistingFile(t *testing.T) {
	// When there's no existing config file and no HEIMDALL_MCP_CONFIG env,
	// SaveConfig should create one in the default location.
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)
	// Unset HEIMDALL_MCP_CONFIG so resolveConfigPath returns ""
	t.Setenv("HEIMDALL_MCP_CONFIG", "")

	cfg := DefaultConfig()
	cfg.GitDepth = 999

	err := SaveConfig(cfg)
	if err != nil {
		t.Fatalf("SaveConfig failed: %v", err)
	}

	// Should have created in $XDG_CONFIG_HOME/heimdall-mcp/config.json
	expectedPath := filepath.Join(tmpDir, "heimdall-mcp", "config.json")
	if _, err := os.Stat(expectedPath); err != nil {
		t.Fatalf("expected config at %s: %v", expectedPath, err)
	}

	// Verify content
	data, _ := os.ReadFile(expectedPath)
	var loaded Config
	json.Unmarshal(data, &loaded)
	if loaded.GitDepth != 999 {
		t.Errorf("expected GitDepth=999, got %d", loaded.GitDepth)
	}
}
