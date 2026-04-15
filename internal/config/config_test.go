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
