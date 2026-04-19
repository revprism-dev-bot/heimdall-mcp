package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/caio-silva/heimdall-mcp/internal/config"
)

// --- Input types ---

type configureInput struct {
	Action string `json:"action"` // "get" or "set"
	Key    string `json:"key"`    // dot-notation key (optional for "get")
	Value  any    `json:"value"`  // value to set (required for "set")
}

type managePathsInput struct {
	Action string `json:"action"` // "add", "remove", "list"
	Path   string `json:"path"`   // required for add/remove
}

// --- Config key definitions ---

type configKeyDef struct {
	Type string // "bool", "int", "[]string"
	Get  func(c *config.Config) any
	Set  func(c *config.Config, v any) error
}

var configKeys = map[string]configKeyDef{
	"model": {
		Type: "string",
		Get:  func(c *config.Config) any { return c.Model },
		Set: func(c *config.Config, v any) error {
			s, ok := v.(string)
			if !ok {
				return fmt.Errorf("expected string for model")
			}
			if s == "" {
				return fmt.Errorf("model cannot be empty")
			}
			c.Model = s
			return nil
		},
	},
	"git.enabled": {
		Type: "bool",
		Get:  func(c *config.Config) any { return c.GitEnabled },
		Set: func(c *config.Config, v any) error {
			b, ok := v.(bool)
			if !ok {
				return fmt.Errorf("expected bool for git.enabled")
			}
			c.GitEnabled = b
			return nil
		},
	},
	"git.depth": {
		Type: "int",
		Get:  func(c *config.Config) any { return c.GitDepth },
		Set: func(c *config.Config, v any) error {
			n, err := toInt(v)
			if err != nil {
				return fmt.Errorf("expected int for git.depth: %w", err)
			}
			c.GitDepth = n
			return nil
		},
	},
	"git.include_diffs": {
		Type: "bool",
		Get:  func(c *config.Config) any { return c.GitIncludeDiffs },
		Set: func(c *config.Config, v any) error {
			b, ok := v.(bool)
			if !ok {
				return fmt.Errorf("expected bool for git.include_diffs")
			}
			c.GitIncludeDiffs = b
			return nil
		},
	},
	"git.branches": {
		Type: "[]string",
		Get:  func(c *config.Config) any { return c.GitBranches },
		Set: func(c *config.Config, v any) error {
			s, err := toStringSlice(v)
			if err != nil {
				return fmt.Errorf("expected []string for git.branches: %w", err)
			}
			c.GitBranches = s
			return nil
		},
	},
	"stale_timeout_minutes": {
		Type: "int",
		Get:  func(c *config.Config) any { return c.StaleTimeoutMin },
		Set: func(c *config.Config, v any) error {
			n, err := toInt(v)
			if err != nil {
				return fmt.Errorf("expected int for stale_timeout_minutes: %w", err)
			}
			c.StaleTimeoutMin = n
			return nil
		},
	},
	"lifecycle.active_days": {
		Type: "int",
		Get:  func(c *config.Config) any { return c.LifecycleActiveDays },
		Set: func(c *config.Config, v any) error {
			n, err := toInt(v)
			if err != nil {
				return fmt.Errorf("expected int for lifecycle.active_days: %w", err)
			}
			c.LifecycleActiveDays = n
			return nil
		},
	},
	"lifecycle.archive_days": {
		Type: "int",
		Get:  func(c *config.Config) any { return c.LifecycleArchiveDays },
		Set: func(c *config.Config, v any) error {
			n, err := toInt(v)
			if err != nil {
				return fmt.Errorf("expected int for lifecycle.archive_days: %w", err)
			}
			c.LifecycleArchiveDays = n
			return nil
		},
	},
	"max_chunks_per_project": {
		Type: "int",
		Get:  func(c *config.Config) any { return c.MaxChunksPerProject },
		Set: func(c *config.Config, v any) error {
			n, err := toInt(v)
			if err != nil {
				return fmt.Errorf("expected int for max_chunks_per_project: %w", err)
			}
			c.MaxChunksPerProject = n
			return nil
		},
	},
	"exclude_patterns": {
		Type: "[]string",
		Get:  func(c *config.Config) any { return c.ExcludePatterns },
		Set: func(c *config.Config, v any) error {
			s, err := toStringSlice(v)
			if err != nil {
				return fmt.Errorf("expected []string for exclude_patterns: %w", err)
			}
			// Reject absolute paths at the MCP boundary, same as the CLI
			// `--exclude` surface (plan §6 M6 resolution).
			if err := config.ValidateExcludePatterns(s); err != nil {
				return err
			}
			c.ExcludePatterns = s
			return nil
		},
	},
}

// --- Tool handlers ---

func (s *Server) toolConfigure(args json.RawMessage) MCPToolResult {
	var input configureInput
	if err := json.Unmarshal(args, &input); err != nil {
		return ErrResult("invalid arguments: " + err.Error())
	}

	switch input.Action {
	case "get":
		return s.configureGet(input.Key)
	case "set":
		return s.configureSet(input.Key, input.Value)
	case "":
		return ErrResult("action is required: must be \"get\" or \"set\"")
	default:
		return ErrResult(fmt.Sprintf("invalid action %q: must be \"get\" or \"set\"", input.Action))
	}
}

func (s *Server) configureGet(key string) MCPToolResult {
	snap := s.cfgSnapshot()
	if key == "" {
		// Return full config
		out, err := json.MarshalIndent(snap, "", "  ")
		if err != nil {
			return ErrResult("marshal error: " + err.Error())
		}
		return TextResult(string(out))
	}

	def, ok := configKeys[key]
	if !ok {
		return ErrResult(fmt.Sprintf("unknown config key %q — valid keys: %s", key, validKeysList()))
	}

	value := def.Get(&snap)
	out, _ := json.MarshalIndent(map[string]any{
		"key":   key,
		"value": value,
	}, "", "  ")
	return TextResult(string(out))
}

func (s *Server) configureSet(key string, value any) MCPToolResult {
	if key == "" {
		return ErrResult("key is required for set action")
	}
	if value == nil {
		return ErrResult("value is required for set action")
	}

	def, ok := configKeys[key]
	if !ok {
		return ErrResult(fmt.Sprintf("unknown config key %q — valid keys: %s", key, validKeysList()))
	}

	// Validate model before applying — verify it exists and can embed
	if key == "model" {
		modelName, _ := value.(string)
		if modelName != "" {
			client := s.newOllamaClient()
			ctx := context.Background()
			if err := client.VerifyModel(ctx, modelName); err != nil {
				return ErrResult(fmt.Sprintf("model verification failed: %v. The model must be pulled in Ollama and support embeddings. Run: ollama pull %s", err, modelName))
			}
		}
	}

	// Write under the Cfg write lock so concurrent readers in
	// newOllamaClient / cfgSnapshot observe a consistent Cfg. Release
	// before the (potentially slow) SaveConfig I/O so concurrent readers
	// aren't blocked on disk writes (PR #74 re-review N-1).
	s.CfgMu.Lock()
	if err := def.Set(&s.Cfg, value); err != nil {
		s.CfgMu.Unlock()
		return ErrResult(err.Error())
	}
	cfgCopy := s.Cfg
	echoValue := def.Get(&s.Cfg)
	s.CfgMu.Unlock()

	// Persist to disk using the snapshot so we don't re-read s.Cfg
	// outside the lock.
	if err := config.SaveConfig(cfgCopy); err != nil {
		return ErrResult("config saved in memory but failed to persist: " + err.Error())
	}

	response := map[string]any{
		"key":   key,
		"value": echoValue,
		"saved": true,
	}
	if key == "model" {
		response["note"] = "Model changed. Re-index your projects for this to take effect."
	}

	out, _ := json.MarshalIndent(response, "", "  ")
	return TextResult(string(out))
}

func (s *Server) toolManagePaths(args json.RawMessage) MCPToolResult {
	var input managePathsInput
	if err := json.Unmarshal(args, &input); err != nil {
		return ErrResult("invalid arguments: " + err.Error())
	}

	switch input.Action {
	case "list":
		return s.managePathsList()
	case "add":
		return s.managePathsAdd(input.Path)
	case "remove":
		return s.managePathsRemove(input.Path)
	case "":
		return ErrResult("action is required: must be \"add\", \"remove\", or \"list\"")
	default:
		return ErrResult(fmt.Sprintf("invalid action %q: must be \"add\", \"remove\", or \"list\"", input.Action))
	}
}

func (s *Server) managePathsList() MCPToolResult {
	snap := s.cfgSnapshot()
	out, _ := json.MarshalIndent(snap.IndexedPaths, "", "  ")
	return TextResult(string(out))
}

func (s *Server) managePathsAdd(path string) MCPToolResult {
	if path == "" {
		return ErrResult("path is required for add action")
	}

	// Validate path exists and is a directory
	info, err := os.Stat(path)
	if err != nil {
		return ErrResult(fmt.Sprintf("path does not exist: %s", path))
	}
	if !info.IsDir() {
		return ErrResult(fmt.Sprintf("path is not a directory: %s", path))
	}

	// Check for duplicate, then append + persist under the Cfg write
	// lock. Callers of cfgSnapshot / newOllamaClient are readers — we
	// must not race them (PR #74 re-review N-1). Release before
	// SaveConfig to keep disk I/O off the lock.
	s.CfgMu.Lock()
	for _, existing := range s.Cfg.IndexedPaths {
		if existing == path {
			s.CfgMu.Unlock()
			out, _ := json.MarshalIndent(map[string]any{
				"path":    path,
				"status":  "already indexed",
				"message": "Path is already in the indexed paths list.",
			}, "", "  ")
			return TextResult(string(out))
		}
	}
	s.Cfg.IndexedPaths = append(s.Cfg.IndexedPaths, path)
	cfgCopy := s.Cfg
	total := len(s.Cfg.IndexedPaths)
	s.CfgMu.Unlock()

	if err := config.SaveConfig(cfgCopy); err != nil {
		return ErrResult("path added in memory but failed to persist: " + err.Error())
	}

	out, _ := json.MarshalIndent(map[string]any{
		"path":   path,
		"status": "added",
		"total":  total,
	}, "", "  ")
	return TextResult(string(out))
}

func (s *Server) managePathsRemove(path string) MCPToolResult {
	if path == "" {
		return ErrResult("path is required for remove action")
	}

	s.CfgMu.Lock()
	idx := -1
	for i, p := range s.Cfg.IndexedPaths {
		if p == path {
			idx = i
			break
		}
	}
	if idx == -1 {
		s.CfgMu.Unlock()
		return ErrResult(fmt.Sprintf("path not found in indexed paths: %s", path))
	}
	s.Cfg.IndexedPaths = append(s.Cfg.IndexedPaths[:idx], s.Cfg.IndexedPaths[idx+1:]...)
	cfgCopy := s.Cfg
	total := len(s.Cfg.IndexedPaths)
	s.CfgMu.Unlock()

	if err := config.SaveConfig(cfgCopy); err != nil {
		return ErrResult("path removed in memory but failed to persist: " + err.Error())
	}

	out, _ := json.MarshalIndent(map[string]any{
		"path":   path,
		"status": "removed",
		"total":  total,
	}, "", "  ")
	return TextResult(string(out))
}

// --- Helpers ---

// toInt converts a JSON-decoded value to int. JSON numbers arrive as float64.
func toInt(v any) (int, error) {
	switch n := v.(type) {
	case float64:
		return int(n), nil
	case int:
		return n, nil
	case json.Number:
		i, err := n.Int64()
		return int(i), err
	default:
		return 0, fmt.Errorf("cannot convert %T to int", v)
	}
}

// toStringSlice converts a JSON-decoded value to []string.
func toStringSlice(v any) ([]string, error) {
	switch s := v.(type) {
	case []any:
		result := make([]string, 0, len(s))
		for _, item := range s {
			str, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("array element is not a string: %T", item)
			}
			result = append(result, str)
		}
		return result, nil
	case []string:
		return s, nil
	default:
		return nil, fmt.Errorf("cannot convert %T to []string", v)
	}
}

// validKeysList returns a comma-separated list of valid config keys.
func validKeysList() string {
	keys := make([]string, 0, len(configKeys))
	for k := range configKeys {
		keys = append(keys, k)
	}
	return fmt.Sprintf("%v", keys)
}
