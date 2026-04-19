package config

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
)

// Config holds the Heimdall MCP server configuration.
type Config struct {
	OllamaEndpoint   string   `json:"ollamaEndpoint"`
	Model            string   `json:"model"`
	ContextDepth     int      `json:"contextDepth"`
	MaxContextTokens int      `json:"maxContextTokens"`
	ExcludePatterns  []string `json:"excludePatterns"`
	// Phase 4-6 fields
	GitEnabled           bool     `json:"gitEnabled"`
	GitDepth             int      `json:"gitDepth"`
	GitIncludeDiffs      bool     `json:"gitIncludeDiffs"`
	GitBranches          []string `json:"gitBranches"`
	StaleTimeoutMin      int      `json:"staleTimeoutMinutes"`
	LifecycleActiveDays  int      `json:"lifecycleActiveDays"`
	LifecycleArchiveDays int      `json:"lifecycleArchiveDays"`
	MaxChunksPerProject  int      `json:"maxChunksPerProject"`
	EmbedBatchSize       int      `json:"embedBatchSize"`
	IndexedPaths         []string `json:"indexedPaths"`
	// LLMClassifierModel names the Ollama instruct model consulted by the
	// PreToolUse guardrail's optional LLM fallback. Empty (the default)
	// disables the fallback regardless of HEIMDALL_LLM_CLASSIFIER. The
	// fallback is strictly opt-in at BOTH the env-var and config layers —
	// see docs/plans/hooks/11-llm-classification-fallback.md §3.3 / §5 and
	// docs/plans/hooks/11a-design-decisions.md §5.4. Recommended value:
	// "llama3.2:3b" (primary) with "qwen2.5-coder:3b" as fallback.
	LLMClassifierModel string `json:"llmClassifierModel,omitempty"`
	// EmbedMaxConcurrent caps in-flight Ollama /api/embed requests per
	// OllamaClient. 0 means "use package default"
	// (heimdall.DefaultEmbedMaxConcurrent). Env override:
	// HEIMDALL_EMBED_MAX_CONCURRENT. Closes handoff Problem #5.
	EmbedMaxConcurrent int `json:"embedMaxConcurrent,omitempty"`
	// EmbedTimeoutMs is the per-request deadline (milliseconds) applied
	// to Ollama /api/embed when the caller ctx has none. 0 means "use
	// package default" (30 s). Env override: HEIMDALL_EMBED_TIMEOUT_MS.
	EmbedTimeoutMs int `json:"embedTimeoutMs,omitempty"`
	// EmbedMaxRetries is the number of retries applied to embed calls
	// whose per-request deadline is exceeded. Caller ctx cancellation
	// is NEVER retried. 0 disables retries. Negative = coerce to 0.
	// Default (sentinel -1 means "unset → package default" = 2 retries).
	// Env override: HEIMDALL_EMBED_MAX_RETRIES.
	EmbedMaxRetries int `json:"embedMaxRetries,omitempty"`
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{
		OllamaEndpoint:       "http://localhost:11434",
		Model:                "nomic-embed-text",
		ContextDepth:         1,
		MaxContextTokens:     4096,
		ExcludePatterns:      []string{".git", "node_modules", "vendor", ".heimdall_db", "__pycache__", ".idea", ".claude/worktrees"},
		GitEnabled:           true,
		GitDepth:             200,
		GitIncludeDiffs:      false,
		GitBranches:          []string{},
		StaleTimeoutMin:      30,
		LifecycleActiveDays:  30,
		LifecycleArchiveDays: 90,
		MaxChunksPerProject:  10000,
		EmbedBatchSize:       32,
		IndexedPaths:         []string{},
	}
}

// LoadConfig loads config from the resolved path, merging with defaults.
func LoadConfig() Config {
	migrateConfigDir()
	cfg := DefaultConfig()
	path := resolveConfigPath()
	if path == "" {
		// No config file — env overrides still apply so operators can
		// tune Ollama concurrency without writing a config first.
		applyEmbedEnvOverrides(&cfg)
		return cfg
	}

	data, err := os.ReadFile(path)
	if err != nil {
		applyEmbedEnvOverrides(&cfg)
		return cfg
	}

	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg
	}

	// Re-apply defaults for zero values
	defaults := DefaultConfig()
	if cfg.OllamaEndpoint == "" {
		cfg.OllamaEndpoint = defaults.OllamaEndpoint
	}
	if cfg.Model == "" {
		cfg.Model = defaults.Model
	}
	if cfg.MaxContextTokens == 0 {
		cfg.MaxContextTokens = defaults.MaxContextTokens
	}
	if len(cfg.ExcludePatterns) == 0 {
		cfg.ExcludePatterns = defaults.ExcludePatterns
	}
	// Phase 4-6 defaults: use a marker to distinguish "not set" from "explicitly false/zero"
	// For booleans, we cannot distinguish false from unset via JSON unmarshal into bool,
	// so we use GitDepth==0 as the sentinel (it was not in older configs).
	if cfg.GitDepth == 0 {
		cfg.GitEnabled = defaults.GitEnabled
		cfg.GitDepth = defaults.GitDepth
	}
	if cfg.StaleTimeoutMin == 0 {
		cfg.StaleTimeoutMin = defaults.StaleTimeoutMin
	}
	if cfg.LifecycleActiveDays == 0 {
		cfg.LifecycleActiveDays = defaults.LifecycleActiveDays
	}
	if cfg.LifecycleArchiveDays == 0 {
		cfg.LifecycleArchiveDays = defaults.LifecycleArchiveDays
	}
	if cfg.MaxChunksPerProject == 0 {
		cfg.MaxChunksPerProject = defaults.MaxChunksPerProject
	}
	if cfg.EmbedBatchSize == 0 {
		cfg.EmbedBatchSize = defaults.EmbedBatchSize
	}

	// Env-var escape valves for Ollama concurrency / timeout / retry.
	// These override JSON config on purpose — operators occasionally
	// need to tune these on a single machine without editing config.
	applyEmbedEnvOverrides(&cfg)

	return cfg
}

// applyEmbedEnvOverrides reads the HEIMDALL_EMBED_* env vars and overrides
// the relevant Config fields. Malformed values are logged and ignored so a
// bad env var never crashes the server.
func applyEmbedEnvOverrides(cfg *Config) {
	if v := os.Getenv("HEIMDALL_EMBED_MAX_CONCURRENT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.EmbedMaxConcurrent = n
		} else {
			log.Printf("heimdall: ignoring malformed HEIMDALL_EMBED_MAX_CONCURRENT=%q: %v", v, err)
		}
	}
	if v := os.Getenv("HEIMDALL_EMBED_TIMEOUT_MS"); v != "" {
		n, err := strconv.Atoi(v)
		switch {
		case err != nil:
			log.Printf("heimdall: ignoring malformed HEIMDALL_EMBED_TIMEOUT_MS=%q: %v", v, err)
		case n < 0:
			log.Printf("heimdall: ignoring negative HEIMDALL_EMBED_TIMEOUT_MS=%q", v)
		default:
			cfg.EmbedTimeoutMs = n
		}
	}
	if v := os.Getenv("HEIMDALL_EMBED_MAX_RETRIES"); v != "" {
		n, err := strconv.Atoi(v)
		switch {
		case err != nil:
			log.Printf("heimdall: ignoring malformed HEIMDALL_EMBED_MAX_RETRIES=%q: %v", v, err)
		case n < 0:
			log.Printf("heimdall: ignoring negative HEIMDALL_EMBED_MAX_RETRIES=%q", v)
		default:
			cfg.EmbedMaxRetries = n
		}
	}
}

// SaveConfig writes the config to the resolved config path, creating the directory if needed.
func SaveConfig(cfg Config) error {
	path := resolveConfigPath()
	if path == "" {
		// No existing config file — create one in the default location
		dir := resolveConfigDir()
		path = filepath.Join(dir, "config.json")
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// resolveConfigPath finds the config file using this precedence:
// 1. $HEIMDALL_MCP_CONFIG env var
// 2. $XDG_CONFIG_HOME/heimdall-mcp/config.json
// 3. ~/.config/heimdall-mcp/config.json
func resolveConfigPath() string {
	if envPath := os.Getenv("HEIMDALL_MCP_CONFIG"); envPath != "" {
		return envPath
	}
	xdgConfig := os.Getenv("XDG_CONFIG_HOME")
	if xdgConfig == "" {
		home, _ := os.UserHomeDir()
		xdgConfig = filepath.Join(home, ".config")
	}
	path := filepath.Join(xdgConfig, "heimdall-mcp", "config.json")
	if _, err := os.Stat(path); err == nil {
		return path
	}
	return ""
}

// ResolveMemoryDBPath returns the path to the global memory database.
// It independently resolves the config dir without depending on config file existence.
// Falls back to /tmp/heimdall-mcp if home dir cannot be determined.
func ResolveMemoryDBPath() string {
	dir := resolveConfigDir()
	return filepath.Join(dir, "memories.db")
}

// resolveConfigDir returns the heimdall-mcp config directory, creating it if needed.
func resolveConfigDir() string {
	xdgConfig := os.Getenv("XDG_CONFIG_HOME")
	if xdgConfig == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join(os.TempDir(), "heimdall-mcp")
		}
		xdgConfig = filepath.Join(home, ".config")
	}
	dir := filepath.Join(xdgConfig, "heimdall-mcp")
	os.MkdirAll(dir, 0700)
	return dir
}

// migrateConfigDir renames the config directory from openviking-mcp to heimdall-mcp.
// Uses a lock file to prevent TOCTOU races.
func migrateConfigDir() {
	xdgConfig := os.Getenv("XDG_CONFIG_HOME")
	if xdgConfig == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return
		}
		xdgConfig = filepath.Join(home, ".config")
	}

	oldDir := filepath.Join(xdgConfig, "openviking-mcp")
	newDir := filepath.Join(xdgConfig, "heimdall-mcp")

	// Quick pre-check
	if _, err := os.Stat(oldDir); err != nil {
		return
	}

	// Acquire exclusive lock file
	lockPath := filepath.Join(xdgConfig, ".heimdall-config-migrate.lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return // another process is migrating
	}
	defer os.Remove(lockPath)
	defer lock.Close()

	// Re-check after lock
	if _, err := os.Stat(oldDir); err != nil {
		return
	}
	if _, err := os.Stat(newDir); err == nil {
		return // new dir already exists
	}

	if err := os.Rename(oldDir, newDir); err != nil {
		log.Printf("heimdall: failed to migrate config %s → %s: %v", oldDir, newDir, err)
		return
	}
	log.Printf("heimdall: migrated config directory %s → %s", oldDir, newDir)
}
