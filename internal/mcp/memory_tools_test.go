package mcp

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
	"github.com/caio-silva/heimdall-mcp/internal/registry"
)

func testServerWithMemory(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	store, err := heimdall.OpenMemoryStore(filepath.Join(dir, "memories.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	return &Server{
		Cfg:         config.DefaultConfig(),
		Registry:    registry.LoadRegistry(),
		MemoryStore: store,
	}
}

func TestToolRemember_MissingContent(t *testing.T) {
	s := testServerWithMemory(t)
	args, _ := json.Marshal(map[string]any{})
	result := s.toolRemember(args)
	if !result.IsError {
		t.Error("expected error for missing content")
	}
	if !strings.Contains(result.Content[0].Text, "content is required") {
		t.Errorf("unexpected error: %s", result.Content[0].Text)
	}
}

func TestToolRemember_InvalidType(t *testing.T) {
	s := testServerWithMemory(t)
	args, _ := json.Marshal(map[string]any{
		"content": "test",
		"type":    "invalid_type",
	})
	result := s.toolRemember(args)
	if !result.IsError {
		t.Error("expected error for invalid type")
	}
	if !strings.Contains(result.Content[0].Text, "invalid memory type") {
		t.Errorf("unexpected error: %s", result.Content[0].Text)
	}
}

func TestToolRemember_ContentTooLarge(t *testing.T) {
	s := testServerWithMemory(t)
	large := strings.Repeat("x", 101*1024) // 101KB
	args, _ := json.Marshal(map[string]any{
		"content": large,
	})
	result := s.toolRemember(args)
	if !result.IsError {
		t.Error("expected error for oversized content")
	}
	if !strings.Contains(result.Content[0].Text, "exceeds maximum") {
		t.Errorf("unexpected error: %s", result.Content[0].Text)
	}
}

func TestToolRemember_TooManyTags(t *testing.T) {
	s := testServerWithMemory(t)
	tags := make([]string, 25)
	for i := range tags {
		tags[i] = "tag"
	}
	args, _ := json.Marshal(map[string]any{
		"content": "test",
		"tags":    tags,
	})
	result := s.toolRemember(args)
	if !result.IsError {
		t.Error("expected error for too many tags")
	}
}

func TestToolRecall_MissingQuery(t *testing.T) {
	s := testServerWithMemory(t)
	args, _ := json.Marshal(map[string]any{})
	result := s.toolRecall(args)
	if !result.IsError {
		t.Error("expected error for missing query")
	}
	if !strings.Contains(result.Content[0].Text, "query is required") {
		t.Errorf("unexpected error: %s", result.Content[0].Text)
	}
}

func TestToolRecall_QueryTooLarge(t *testing.T) {
	s := testServerWithMemory(t)
	large := strings.Repeat("x", 11*1024) // 11KB
	args, _ := json.Marshal(map[string]any{
		"query": large,
	})
	result := s.toolRecall(args)
	if !result.IsError {
		t.Error("expected error for oversized query")
	}
}

func TestToolIngestSession_MissingSummary(t *testing.T) {
	s := testServerWithMemory(t)
	args, _ := json.Marshal(map[string]any{})
	result := s.toolIngestSession(args)
	if !result.IsError {
		t.Error("expected error for missing summary")
	}
	if !strings.Contains(result.Content[0].Text, "summary is required") {
		t.Errorf("unexpected error: %s", result.Content[0].Text)
	}
}

func TestToolIngestSession_SummaryTooLarge(t *testing.T) {
	s := testServerWithMemory(t)
	large := strings.Repeat("x", 51*1024) // 51KB
	args, _ := json.Marshal(map[string]any{
		"summary": large,
	})
	result := s.toolIngestSession(args)
	if !result.IsError {
		t.Error("expected error for oversized summary")
	}
}

func TestToolRemember_NilMemoryStore(t *testing.T) {
	s := &Server{
		Cfg:      config.DefaultConfig(),
		Registry: registry.LoadRegistry(),
		// MemoryStore intentionally nil
	}
	args, _ := json.Marshal(map[string]any{
		"content": "test memory",
	})
	result := s.toolRemember(args)
	if !result.IsError {
		t.Error("expected error for nil memory store")
	}
	if !strings.Contains(result.Content[0].Text, "not initialized") {
		t.Errorf("unexpected error: %s", result.Content[0].Text)
	}
}

func TestToolRecall_NilMemoryStore(t *testing.T) {
	s := &Server{
		Cfg:      config.DefaultConfig(),
		Registry: registry.LoadRegistry(),
	}
	args, _ := json.Marshal(map[string]any{
		"query": "test",
	})
	result := s.toolRecall(args)
	if !result.IsError {
		t.Error("expected error for nil memory store")
	}
}

func TestToolIngestSession_NilMemoryStore(t *testing.T) {
	s := &Server{
		Cfg:      config.DefaultConfig(),
		Registry: registry.LoadRegistry(),
	}
	args, _ := json.Marshal(map[string]any{
		"summary": "test summary",
	})
	result := s.toolIngestSession(args)
	if !result.IsError {
		t.Error("expected error for nil memory store")
	}
}

func TestMergeTags(t *testing.T) {
	result := heimdall.MergeTags([]string{"a", "b"}, []string{"b", "c"})
	if len(result) != 3 {
		t.Errorf("mergeTags = %v, want 3 items", result)
	}

	// Test cap at 20
	many := make([]string, 25)
	for i := range many {
		many[i] = string(rune('a' + i))
	}
	result = heimdall.MergeTags(many[:15], many[10:])
	if len(result) > 20 {
		t.Errorf("mergeTags exceeded cap: %d > 20", len(result))
	}
}
