package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/registry"
)

// testServerWithConfig creates a test server and sets HEIMDALL_MCP_CONFIG to a temp file.
func testServerWithConfig(t *testing.T) *Server {
	t.Helper()
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	t.Setenv("HEIMDALL_MCP_CONFIG", configPath)

	cfg := config.DefaultConfig()
	// Save an initial config so round-trips work
	config.SaveConfig(cfg)

	return &Server{
		Cfg:      cfg,
		Registry: &registry.Registry{},
	}
}

// --- heimdall_configure tests ---

func TestToolConfigure_GetFull(t *testing.T) {
	s := testServerWithConfig(t)
	args, _ := json.Marshal(map[string]any{
		"action": "get",
	})
	result := s.toolConfigure(args)
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content[0].Text)
	}

	// Should contain config fields
	text := result.Content[0].Text
	if !strings.Contains(text, "ollamaEndpoint") {
		t.Error("expected full config to contain ollamaEndpoint")
	}
	if !strings.Contains(text, "gitEnabled") {
		t.Error("expected full config to contain gitEnabled")
	}
	if !strings.Contains(text, "gitDepth") {
		t.Error("expected full config to contain gitDepth")
	}
}

func TestToolConfigure_GetKey(t *testing.T) {
	s := testServerWithConfig(t)
	args, _ := json.Marshal(map[string]any{
		"action": "get",
		"key":    "git.depth",
	})
	result := s.toolConfigure(args)
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content[0].Text)
	}
	text := result.Content[0].Text
	if !strings.Contains(text, "200") {
		t.Errorf("expected git.depth=200, got: %s", text)
	}
}

func TestToolConfigure_GetKey_GitEnabled(t *testing.T) {
	s := testServerWithConfig(t)
	args, _ := json.Marshal(map[string]any{
		"action": "get",
		"key":    "git.enabled",
	})
	result := s.toolConfigure(args)
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content[0].Text)
	}
	text := result.Content[0].Text
	if !strings.Contains(text, "true") {
		t.Errorf("expected git.enabled=true, got: %s", text)
	}
}

func TestToolConfigure_GetInvalidKey(t *testing.T) {
	s := testServerWithConfig(t)
	args, _ := json.Marshal(map[string]any{
		"action": "get",
		"key":    "invalid.key",
	})
	result := s.toolConfigure(args)
	if !result.IsError {
		t.Error("expected error for invalid key")
	}
	if !strings.Contains(result.Content[0].Text, "unknown config key") {
		t.Errorf("unexpected error message: %s", result.Content[0].Text)
	}
}

func TestToolConfigure_SetKey_Int(t *testing.T) {
	s := testServerWithConfig(t)
	args, _ := json.Marshal(map[string]any{
		"action": "set",
		"key":    "git.depth",
		"value":  float64(500), // JSON numbers are float64
	})
	result := s.toolConfigure(args)
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content[0].Text)
	}

	// Verify in-memory update
	if s.Cfg.GitDepth != 500 {
		t.Errorf("expected GitDepth=500 in memory, got %d", s.Cfg.GitDepth)
	}

	// Verify persisted to disk
	loaded := config.LoadConfig()
	if loaded.GitDepth != 500 {
		t.Errorf("expected GitDepth=500 on disk, got %d", loaded.GitDepth)
	}
}

func TestToolConfigure_SetKey_Bool(t *testing.T) {
	s := testServerWithConfig(t)
	args, _ := json.Marshal(map[string]any{
		"action": "set",
		"key":    "git.enabled",
		"value":  false,
	})
	result := s.toolConfigure(args)
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content[0].Text)
	}

	if s.Cfg.GitEnabled {
		t.Error("expected GitEnabled=false in memory")
	}
}

func TestToolConfigure_SetKey_StringSlice(t *testing.T) {
	s := testServerWithConfig(t)
	args, _ := json.Marshal(map[string]any{
		"action": "set",
		"key":    "git.branches",
		"value":  []string{"main", "develop"},
	})
	result := s.toolConfigure(args)
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content[0].Text)
	}

	if len(s.Cfg.GitBranches) != 2 {
		t.Errorf("expected 2 branches, got %d", len(s.Cfg.GitBranches))
	}
}

// TestConfigureSetExcludePatterns_MCP is the G5/M6 MCP-side regression: the
// heimdall_configure tool accepts an exclude_patterns array and persists it.
func TestConfigureSetExcludePatterns_MCP(t *testing.T) {
	s := testServerWithConfig(t)
	args, _ := json.Marshal(map[string]any{
		"action": "set",
		"key":    "exclude_patterns",
		"value":  []string{"dist", "build", "generated"},
	})
	result := s.toolConfigure(args)
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content[0].Text)
	}
	if len(s.Cfg.ExcludePatterns) != 3 {
		t.Errorf("expected 3 patterns, got %d (%v)", len(s.Cfg.ExcludePatterns), s.Cfg.ExcludePatterns)
	}
	// Round-trip from disk — the set path must persist.
	loaded := config.LoadConfig()
	if len(loaded.ExcludePatterns) != 3 {
		t.Errorf("persisted config mismatch: %v", loaded.ExcludePatterns)
	}
}

// TestExclude_AbsolutePathRejectedByMCP is the M6 regression for the MCP
// surface: absolute paths are rejected, nothing is persisted.
func TestExclude_AbsolutePathRejectedByMCP(t *testing.T) {
	s := testServerWithConfig(t)
	originalPatterns := append([]string{}, s.Cfg.ExcludePatterns...)
	args, _ := json.Marshal(map[string]any{
		"action": "set",
		"key":    "exclude_patterns",
		"value":  []string{"ok", "/etc/foo"},
	})
	result := s.toolConfigure(args)
	if !result.IsError {
		t.Fatal("expected IsError for absolute path")
	}
	if !strings.Contains(result.Content[0].Text, "absolute") {
		t.Errorf("error text must mention 'absolute', got: %s", result.Content[0].Text)
	}
	// In-memory config must NOT have been mutated on validation failure.
	if len(s.Cfg.ExcludePatterns) != len(originalPatterns) {
		t.Errorf("config mutated on failure: %v", s.Cfg.ExcludePatterns)
	}
	for i, v := range originalPatterns {
		if s.Cfg.ExcludePatterns[i] != v {
			t.Errorf("config[%d] = %q, want %q (unchanged)", i, s.Cfg.ExcludePatterns[i], v)
		}
	}
}

func TestToolConfigure_SetKey_TypeMismatch(t *testing.T) {
	s := testServerWithConfig(t)

	// Try setting a bool key with a string value
	args, _ := json.Marshal(map[string]any{
		"action": "set",
		"key":    "git.enabled",
		"value":  "not_a_bool",
	})
	result := s.toolConfigure(args)
	if !result.IsError {
		t.Error("expected error for type mismatch")
	}
	if !strings.Contains(result.Content[0].Text, "expected bool") {
		t.Errorf("unexpected error: %s", result.Content[0].Text)
	}
}

func TestToolConfigure_SetKey_IntTypeMismatch(t *testing.T) {
	s := testServerWithConfig(t)
	args, _ := json.Marshal(map[string]any{
		"action": "set",
		"key":    "git.depth",
		"value":  "not_a_number",
	})
	result := s.toolConfigure(args)
	if !result.IsError {
		t.Error("expected error for int type mismatch")
	}
}

func TestToolConfigure_SetKey_UnknownKey(t *testing.T) {
	s := testServerWithConfig(t)
	args, _ := json.Marshal(map[string]any{
		"action": "set",
		"key":    "nonexistent.key",
		"value":  true,
	})
	result := s.toolConfigure(args)
	if !result.IsError {
		t.Error("expected error for unknown key")
	}
}

func TestToolConfigure_SetKey_MissingKey(t *testing.T) {
	s := testServerWithConfig(t)
	args, _ := json.Marshal(map[string]any{
		"action": "set",
		"value":  true,
	})
	result := s.toolConfigure(args)
	if !result.IsError {
		t.Error("expected error for missing key on set")
	}
}

func TestToolConfigure_SetKey_MissingValue(t *testing.T) {
	s := testServerWithConfig(t)
	args, _ := json.Marshal(map[string]any{
		"action": "set",
		"key":    "git.depth",
	})
	result := s.toolConfigure(args)
	if !result.IsError {
		t.Error("expected error for missing value on set")
	}
}

func TestToolConfigure_InvalidAction(t *testing.T) {
	s := testServerWithConfig(t)
	args, _ := json.Marshal(map[string]any{
		"action": "delete",
	})
	result := s.toolConfigure(args)
	if !result.IsError {
		t.Error("expected error for invalid action")
	}
}

func TestToolConfigure_MissingAction(t *testing.T) {
	s := testServerWithConfig(t)
	args, _ := json.Marshal(map[string]any{})
	result := s.toolConfigure(args)
	if !result.IsError {
		t.Error("expected error for missing action")
	}
}

func TestToolConfigure_SetStaleTimeout(t *testing.T) {
	s := testServerWithConfig(t)
	args, _ := json.Marshal(map[string]any{
		"action": "set",
		"key":    "stale_timeout_minutes",
		"value":  float64(60),
	})
	result := s.toolConfigure(args)
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content[0].Text)
	}
	if s.Cfg.StaleTimeoutMin != 60 {
		t.Errorf("expected StaleTimeoutMin=60, got %d", s.Cfg.StaleTimeoutMin)
	}
}

func TestToolConfigure_SetLifecycleActiveDays(t *testing.T) {
	s := testServerWithConfig(t)
	args, _ := json.Marshal(map[string]any{
		"action": "set",
		"key":    "lifecycle.active_days",
		"value":  float64(14),
	})
	result := s.toolConfigure(args)
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content[0].Text)
	}
	if s.Cfg.LifecycleActiveDays != 14 {
		t.Errorf("expected LifecycleActiveDays=14, got %d", s.Cfg.LifecycleActiveDays)
	}
}

func TestToolConfigure_SetMaxChunksPerProject(t *testing.T) {
	s := testServerWithConfig(t)
	args, _ := json.Marshal(map[string]any{
		"action": "set",
		"key":    "max_chunks_per_project",
		"value":  float64(5000),
	})
	result := s.toolConfigure(args)
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content[0].Text)
	}
	if s.Cfg.MaxChunksPerProject != 5000 {
		t.Errorf("expected MaxChunksPerProject=5000, got %d", s.Cfg.MaxChunksPerProject)
	}
}

func TestToolConfigure_SetGitIncludeDiffs(t *testing.T) {
	s := testServerWithConfig(t)
	args, _ := json.Marshal(map[string]any{
		"action": "set",
		"key":    "git.include_diffs",
		"value":  true,
	})
	result := s.toolConfigure(args)
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content[0].Text)
	}
	if !s.Cfg.GitIncludeDiffs {
		t.Error("expected GitIncludeDiffs=true")
	}
}

// --- heimdall_manage_paths tests ---

func TestToolManagePaths_List_Empty(t *testing.T) {
	s := testServerWithConfig(t)
	args, _ := json.Marshal(map[string]any{
		"action": "list",
	})
	result := s.toolManagePaths(args)
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content[0].Text)
	}
	if !strings.Contains(result.Content[0].Text, "[]") {
		t.Errorf("expected empty list, got: %s", result.Content[0].Text)
	}
}

func TestToolManagePaths_AddValidPath(t *testing.T) {
	s := testServerWithConfig(t)
	tmpDir := t.TempDir()

	args, _ := json.Marshal(map[string]any{
		"action": "add",
		"path":   tmpDir,
	})
	result := s.toolManagePaths(args)
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content[0].Text)
	}

	// Verify in-memory
	if len(s.Cfg.IndexedPaths) != 1 || s.Cfg.IndexedPaths[0] != tmpDir {
		t.Errorf("expected [%s], got %v", tmpDir, s.Cfg.IndexedPaths)
	}

	// Verify persisted
	loaded := config.LoadConfig()
	if len(loaded.IndexedPaths) != 1 || loaded.IndexedPaths[0] != tmpDir {
		t.Errorf("expected persisted [%s], got %v", tmpDir, loaded.IndexedPaths)
	}
}

func TestToolManagePaths_AddInvalidPath(t *testing.T) {
	s := testServerWithConfig(t)
	args, _ := json.Marshal(map[string]any{
		"action": "add",
		"path":   "/nonexistent/path/that/does/not/exist",
	})
	result := s.toolManagePaths(args)
	if !result.IsError {
		t.Error("expected error for nonexistent path")
	}
	if !strings.Contains(result.Content[0].Text, "does not exist") {
		t.Errorf("unexpected error: %s", result.Content[0].Text)
	}
}

func TestToolManagePaths_AddNotDirectory(t *testing.T) {
	s := testServerWithConfig(t)
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "file.txt")
	os.WriteFile(filePath, []byte("hello"), 0600)

	args, _ := json.Marshal(map[string]any{
		"action": "add",
		"path":   filePath,
	})
	result := s.toolManagePaths(args)
	if !result.IsError {
		t.Error("expected error for non-directory path")
	}
	if !strings.Contains(result.Content[0].Text, "not a directory") {
		t.Errorf("unexpected error: %s", result.Content[0].Text)
	}
}

func TestToolManagePaths_AddDuplicate(t *testing.T) {
	s := testServerWithConfig(t)
	tmpDir := t.TempDir()

	// Add once
	args, _ := json.Marshal(map[string]any{
		"action": "add",
		"path":   tmpDir,
	})
	s.toolManagePaths(args)

	// Add again
	result := s.toolManagePaths(args)
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content[0].Text)
	}

	// Should still have only one entry
	if len(s.Cfg.IndexedPaths) != 1 {
		t.Errorf("expected 1 path (no duplicate), got %d", len(s.Cfg.IndexedPaths))
	}

	// Should indicate already present
	if !strings.Contains(result.Content[0].Text, "already") {
		t.Errorf("expected 'already' message, got: %s", result.Content[0].Text)
	}
}

func TestToolManagePaths_Remove(t *testing.T) {
	s := testServerWithConfig(t)
	tmpDir := t.TempDir()

	// Add first
	addArgs, _ := json.Marshal(map[string]any{
		"action": "add",
		"path":   tmpDir,
	})
	s.toolManagePaths(addArgs)

	// Remove
	removeArgs, _ := json.Marshal(map[string]any{
		"action": "remove",
		"path":   tmpDir,
	})
	result := s.toolManagePaths(removeArgs)
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content[0].Text)
	}

	if len(s.Cfg.IndexedPaths) != 0 {
		t.Errorf("expected empty paths after remove, got %v", s.Cfg.IndexedPaths)
	}

	// Verify persisted
	loaded := config.LoadConfig()
	if len(loaded.IndexedPaths) != 0 {
		t.Errorf("expected empty persisted paths, got %v", loaded.IndexedPaths)
	}
}

func TestToolManagePaths_RemoveNonExistent(t *testing.T) {
	s := testServerWithConfig(t)
	args, _ := json.Marshal(map[string]any{
		"action": "remove",
		"path":   "/not/in/list",
	})
	result := s.toolManagePaths(args)
	if !result.IsError {
		t.Error("expected error for removing non-existent path")
	}
	if !strings.Contains(result.Content[0].Text, "not found") {
		t.Errorf("unexpected error: %s", result.Content[0].Text)
	}
}

func TestToolManagePaths_InvalidAction(t *testing.T) {
	s := testServerWithConfig(t)
	args, _ := json.Marshal(map[string]any{
		"action": "invalid",
	})
	result := s.toolManagePaths(args)
	if !result.IsError {
		t.Error("expected error for invalid action")
	}
}

func TestToolManagePaths_MissingAction(t *testing.T) {
	s := testServerWithConfig(t)
	args, _ := json.Marshal(map[string]any{})
	result := s.toolManagePaths(args)
	if !result.IsError {
		t.Error("expected error for missing action")
	}
}

func TestToolManagePaths_AddMissingPath(t *testing.T) {
	s := testServerWithConfig(t)
	args, _ := json.Marshal(map[string]any{
		"action": "add",
	})
	result := s.toolManagePaths(args)
	if !result.IsError {
		t.Error("expected error for missing path on add")
	}
}

func TestToolManagePaths_ListMultiple(t *testing.T) {
	s := testServerWithConfig(t)
	dir1 := t.TempDir()
	dir2 := t.TempDir()

	args1, _ := json.Marshal(map[string]any{"action": "add", "path": dir1})
	args2, _ := json.Marshal(map[string]any{"action": "add", "path": dir2})
	s.toolManagePaths(args1)
	s.toolManagePaths(args2)

	listArgs, _ := json.Marshal(map[string]any{"action": "list"})
	result := s.toolManagePaths(listArgs)
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content[0].Text)
	}

	text := result.Content[0].Text
	if !strings.Contains(text, dir1) || !strings.Contains(text, dir2) {
		t.Errorf("expected both dirs in list, got: %s", text)
	}
}

// --- Integration via handleToolsCall ---

func TestHandleToolsCall_Configure(t *testing.T) {
	s := testServerWithConfig(t)
	params, _ := json.Marshal(map[string]any{
		"name":      "heimdall_configure",
		"arguments": map[string]any{"action": "get"},
	})
	req := JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  params,
	}
	resp := s.Handle(req)
	if resp.Error != nil {
		t.Fatalf("unexpected RPC error: %s", resp.Error.Message)
	}
}

func TestHandleToolsCall_ManagePaths(t *testing.T) {
	s := testServerWithConfig(t)
	params, _ := json.Marshal(map[string]any{
		"name":      "heimdall_manage_paths",
		"arguments": map[string]any{"action": "list"},
	})
	req := JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      2,
		Method:  "tools/call",
		Params:  params,
	}
	resp := s.Handle(req)
	if resp.Error != nil {
		t.Fatalf("unexpected RPC error: %s", resp.Error.Message)
	}
}

func TestHandleToolsList_IncludesNewTools(t *testing.T) {
	s := testServerWithConfig(t)
	req := JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      3,
		Method:  "tools/list",
	}
	resp := s.Handle(req)
	if resp.Error != nil {
		t.Fatalf("unexpected RPC error: %s", resp.Error.Message)
	}

	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatal("expected result to be map")
	}
	tools, ok := result["tools"].([]MCPToolInfo)
	if !ok {
		t.Fatal("expected tools to be []MCPToolInfo")
	}

	foundConfigure := false
	foundManagePaths := false
	for _, tool := range tools {
		if tool.Name == "heimdall_configure" {
			foundConfigure = true
		}
		if tool.Name == "heimdall_manage_paths" {
			foundManagePaths = true
		}
	}
	if !foundConfigure {
		t.Error("heimdall_configure not found in tools list")
	}
	if !foundManagePaths {
		t.Error("heimdall_manage_paths not found in tools list")
	}
}
