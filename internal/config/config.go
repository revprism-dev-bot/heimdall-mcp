package config

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
)

// Embed-tuning clamp bounds. Operators setting absurd values via
// HEIMDALL_EMBED_* env vars trigger a WARN log and a clamp to a sensible
// range. Keeps overflow / retry-amplification / unbounded-allocation
// exposure off the table without needing the caller to validate.
const (
	// MaxEmbedConcurrent is the ceiling for HEIMDALL_EMBED_MAX_CONCURRENT
	// and the embedMaxConcurrent field. 64 is well above any sane single
	// Ollama instance; higher values only invite OOM / chan-alloc surprise.
	MaxEmbedConcurrent = 64
	// MaxEmbedTimeoutMs caps HEIMDALL_EMBED_TIMEOUT_MS. 10 min is several
	// orders of magnitude longer than any realistic embed request and far
	// below time.Duration overflow.
	MaxEmbedTimeoutMs = 10 * 60 * 1000 // 10 minutes
	// MinEmbedTimeoutMs is the lower clamp — under 100ms renders the
	// timeout useless (HTTP setup alone eats that). 0 still means
	// "use default" elsewhere; this clamp only applies to non-zero ints.
	MinEmbedTimeoutMs = 100
	// MaxEmbedRetries caps HEIMDALL_EMBED_MAX_RETRIES. 10 is enough for
	// exponential-backoff strategies; higher values amplify retry storms.
	MaxEmbedRetries = 10

	// sentinelUnset is the "use package default" sentinel for
	// EmbedMaxConcurrent / EmbedMaxRetries. Callers who want to
	// explicitly opt into "unbounded" / "zero retries" set the field to 0
	// (which is distinct from -1). DefaultConfig() seeds -1 so fresh
	// installs get the safe client-side defaults without collision with
	// the documented 0 = unbounded contract.
	sentinelUnset = -1
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
	// OllamaClient. Contract (matches README):
	//   -1 → "unset / use client default" (heimdall.DefaultEmbedMaxConcurrent = 2).
	//    0 → EXPLICIT unbounded (nil semaphore). For operators who manage
	//        concurrency upstream.
	//    N → cap at N.
	// DefaultConfig() seeds -1 so fresh installs get the safe cap of 2
	// without conflicting with the documented "0 = unbounded" semantic.
	// Env override: HEIMDALL_EMBED_MAX_CONCURRENT. Closes handoff Problem #5.
	EmbedMaxConcurrent int `json:"embedMaxConcurrent,omitempty"`
	// EmbedTimeoutMs is the per-request deadline (milliseconds) applied
	// to Ollama /api/embed when the caller ctx has none. 0 means "use
	// package default" (30 s). Env override: HEIMDALL_EMBED_TIMEOUT_MS.
	EmbedTimeoutMs int `json:"embedTimeoutMs,omitempty"`
	// EmbedMaxRetries is the number of retries applied to embed calls
	// whose per-request deadline is exceeded. Caller ctx cancellation
	// is NEVER retried. Contract (matches EmbedMaxConcurrent sentinel):
	//   -1 → "explicitly disable retries" (0 retries).
	//    0 → "use package default" (DefaultEmbedMaxRetries = 2).
	//    N → apply literally.
	// DefaultConfig() seeds -1 so fresh installs get defaults without
	// conflicting with the documented opt-out. Env override:
	// HEIMDALL_EMBED_MAX_RETRIES.
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
		// Sentinel -1 = "use client-side default" (DefaultEmbedMaxConcurrent=2,
		// DefaultEmbedMaxRetries=2). 0 is reserved for the documented
		// "unbounded" / "zero retries" opt-out. See
		// heimdall.NewOllamaClientFromConfig for resolution semantics.
		EmbedMaxConcurrent: sentinelUnset,
		EmbedMaxRetries:    sentinelUnset,
	}
}

// LoadConfig loads config from the resolved path, merging with defaults.
//
// Contract: the returned Config represents ONLY what is persisted on disk
// plus JSON-absent-field defaulting. It does NOT apply HEIMDALL_EMBED_*
// env var overrides — those are applied on demand via ResolveEmbedConfig
// so env-var values never round-trip to config.json via SaveConfig. See
// the pr74-review-security-config.md H1 finding for the persistence
// hazard this avoids.
func LoadConfig() Config {
	migrateConfigDir()
	cfg := DefaultConfig()
	path := resolveConfigPath()
	if path == "" {
		// No config file — nothing else to do; caller merges env via
		// ResolveEmbedConfig when it needs the effective values.
		return cfg
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}

	// First-pass unmarshal into a generic map so we can detect WHICH keys
	// the on-disk config actually carries. JSON zero values are ambiguous
	// for EmbedMaxConcurrent / EmbedMaxRetries (0 = explicit "unbounded" /
	// "zero retries" per README contract vs. 0 = field absent → default).
	var raw map[string]json.RawMessage
	rawOK := true
	if err := json.Unmarshal(data, &raw); err != nil {
		// Malformed JSON — log and fall back to defaults. Env overrides
		// still take effect at ResolveEmbedConfig time.
		log.Printf("heimdall: ignoring malformed config %q: %v", path, err)
		rawOK = false
		raw = nil
	}

	// Second pass: unmarshal into Config struct.
	if err := json.Unmarshal(data, &cfg); err != nil {
		// Second pass can fail for type errors even if structural
		// unmarshal succeeds; fall back to defaults again.
		log.Printf("heimdall: ignoring malformed config fields in %q: %v", path, err)
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

	// EmbedMaxConcurrent / EmbedMaxRetries use the -1 "unset → use
	// package default" sentinel. If the on-disk JSON lacks the key, the
	// struct unmarshal leaves 0 — reinstate the -1 sentinel so the
	// downstream resolver picks the default (2 / 2) rather than "0 =
	// unbounded / zero retries". Legacy configs without the field get
	// the safe default; users who explicitly wrote 0 keep their opt-out.
	if rawOK {
		if _, present := raw["embedMaxConcurrent"]; !present {
			cfg.EmbedMaxConcurrent = sentinelUnset
		}
		if _, present := raw["embedMaxRetries"]; !present {
			cfg.EmbedMaxRetries = sentinelUnset
		}
	}

	return cfg
}

// ResolveEmbedConfig returns a copy of base with HEIMDALL_EMBED_* env
// overrides applied and all embed-tuning fields clamped to safe ranges.
// The returned Config is the "effective" config for the embed subsystem;
// the source `base` is NEVER mutated, so callers can pass s.Cfg without
// risking the next SaveConfig(s.Cfg) baking env values to disk.
//
// Env > JSON > default precedence. Malformed env values are logged and
// ignored (the base field stays). Values that exceed the documented
// clamp ranges (MaxEmbedConcurrent / MaxEmbedTimeoutMs / MaxEmbedRetries)
// trigger a WARN log and are clamped to the nearest boundary.
//
// Closes PR74 review H1 (env persistence) and M2 (unclamped env ints).
func ResolveEmbedConfig(base Config) Config {
	cfg := base // local copy — mutations stay here.

	// Apply env overrides.
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

	// Clamp. Runs AFTER env merge so both on-disk and env-override
	// paths share the same safety rails. Only clamp non-sentinel
	// values so "-1 = use default" isn't mis-treated as a boundary.
	if cfg.EmbedMaxConcurrent > MaxEmbedConcurrent {
		log.Printf("heimdall: embedMaxConcurrent=%d exceeds max %d; clamping", cfg.EmbedMaxConcurrent, MaxEmbedConcurrent)
		cfg.EmbedMaxConcurrent = MaxEmbedConcurrent
	}
	if cfg.EmbedTimeoutMs > MaxEmbedTimeoutMs {
		log.Printf("heimdall: embedTimeoutMs=%d exceeds max %d; clamping", cfg.EmbedTimeoutMs, MaxEmbedTimeoutMs)
		cfg.EmbedTimeoutMs = MaxEmbedTimeoutMs
	}
	if cfg.EmbedTimeoutMs > 0 && cfg.EmbedTimeoutMs < MinEmbedTimeoutMs {
		log.Printf("heimdall: embedTimeoutMs=%d below min %d; clamping", cfg.EmbedTimeoutMs, MinEmbedTimeoutMs)
		cfg.EmbedTimeoutMs = MinEmbedTimeoutMs
	}
	if cfg.EmbedMaxRetries > MaxEmbedRetries {
		log.Printf("heimdall: embedMaxRetries=%d exceeds max %d; clamping", cfg.EmbedMaxRetries, MaxEmbedRetries)
		cfg.EmbedMaxRetries = MaxEmbedRetries
	}

	return cfg
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
