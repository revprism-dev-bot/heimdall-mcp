package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caio-silva/heimdall-mcp/internal/config"
)

func TestHookStop_AppendsBuffer(t *testing.T) {
	dir := t.TempDir()
	payload := stopPayload{
		SessionID:            "test-session-1",
		CWD:                  dir,
		LastAssistantMessage: "Here is the implementation.",
		StopHookActive:       true,
		HookEventName:        "Stop",
	}
	data, _ := json.Marshal(payload)

	cfg := config.DefaultConfig()
	code := HookStop(cfg, bytes.NewReader(data), &bytes.Buffer{}, &bytes.Buffer{}, map[string]string{}, nil)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}

	bufPath := filepath.Join(dir, ".heimdall_db", "hooks", "sessions", "test-session-1.jsonl")
	content, err := os.ReadFile(bufPath)
	if err != nil {
		t.Fatalf("buffer file not created: %v", err)
	}
	if !strings.Contains(string(content), "Here is the implementation.") {
		t.Fatalf("buffer doesn't contain message: %s", content)
	}
}

func TestHookStop_EmptySession(t *testing.T) {
	payload := stopPayload{SessionID: "", LastAssistantMessage: "msg"}
	data, _ := json.Marshal(payload)
	code := HookStop(config.DefaultConfig(), bytes.NewReader(data), &bytes.Buffer{}, &bytes.Buffer{}, map[string]string{}, nil)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestHookStop_EmptyMessage(t *testing.T) {
	payload := stopPayload{SessionID: "s1", LastAssistantMessage: ""}
	data, _ := json.Marshal(payload)
	code := HookStop(config.DefaultConfig(), bytes.NewReader(data), &bytes.Buffer{}, &bytes.Buffer{}, map[string]string{}, nil)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestHookStop_MalformedJSON(t *testing.T) {
	code := HookStop(config.DefaultConfig(), strings.NewReader("{bad json"), &bytes.Buffer{}, &bytes.Buffer{}, map[string]string{}, nil)
	if code != 0 {
		t.Fatalf("expected exit 0 on malformed JSON, got %d", code)
	}
}

func TestHookStop_Disabled(t *testing.T) {
	payload := stopPayload{SessionID: "s1", LastAssistantMessage: "msg"}
	data, _ := json.Marshal(payload)
	env := map[string]string{"HEIMDALL_HOOKS": "0"}
	code := HookStop(config.DefaultConfig(), bytes.NewReader(data), &bytes.Buffer{}, &bytes.Buffer{}, env, nil)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestHookStop_MultipleAppends(t *testing.T) {
	dir := t.TempDir()
	cfg := config.DefaultConfig()

	for i := 0; i < 3; i++ {
		payload := stopPayload{
			SessionID:            "multi-session",
			CWD:                  dir,
			LastAssistantMessage: "message " + string(rune('A'+i)),
			StopHookActive:       true,
		}
		data, _ := json.Marshal(payload)
		code := HookStop(cfg, bytes.NewReader(data), &bytes.Buffer{}, &bytes.Buffer{}, map[string]string{}, nil)
		if code != 0 {
			t.Fatalf("append %d: expected exit 0, got %d", i, code)
		}
	}

	bufPath := filepath.Join(dir, ".heimdall_db", "hooks", "sessions", "multi-session.jsonl")
	content, err := os.ReadFile(bufPath)
	if err != nil {
		t.Fatalf("buffer file not created: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %s", len(lines), content)
	}
}

func TestHookSessionEnd_CleansUpBuffer(t *testing.T) {
	dir := t.TempDir()
	cfg := config.DefaultConfig()

	// First create a buffer file via Stop
	stopData, _ := json.Marshal(stopPayload{
		SessionID:            "cleanup-session",
		CWD:                  dir,
		LastAssistantMessage: "test msg",
	})
	HookStop(cfg, bytes.NewReader(stopData), &bytes.Buffer{}, &bytes.Buffer{}, map[string]string{}, nil)

	bufPath := filepath.Join(dir, ".heimdall_db", "hooks", "sessions", "cleanup-session.jsonl")
	if _, err := os.Stat(bufPath); err != nil {
		t.Fatalf("buffer should exist before session-end: %v", err)
	}

	// Now fire SessionEnd — it should clean up the buffer
	endData, _ := json.Marshal(sessionEndPayload{
		SessionID:     "cleanup-session",
		CWD:           dir,
		HookEventName: "SessionEnd",
		Reason:        "clear",
	})
	code := HookSessionEnd(cfg, bytes.NewReader(endData), &bytes.Buffer{}, &bytes.Buffer{}, map[string]string{}, nil)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}

	if _, err := os.Stat(bufPath); !os.IsNotExist(err) {
		t.Fatal("buffer file should be removed after session-end")
	}
}

func TestHookSessionEnd_EmptySession(t *testing.T) {
	data, _ := json.Marshal(sessionEndPayload{SessionID: ""})
	code := HookSessionEnd(config.DefaultConfig(), bytes.NewReader(data), &bytes.Buffer{}, &bytes.Buffer{}, map[string]string{}, nil)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestHookSessionEnd_MalformedJSON(t *testing.T) {
	code := HookSessionEnd(config.DefaultConfig(), strings.NewReader("nope"), &bytes.Buffer{}, &bytes.Buffer{}, map[string]string{}, nil)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestExtractTranscriptSummary_Empty(t *testing.T) {
	got := extractTranscriptSummary(nil)
	if got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestExtractTranscriptSummary_AssistantMessages(t *testing.T) {
	lines := []string{
		`{"role":"user","content":"hello"}`,
		`{"role":"assistant","content":"Hi there!"}`,
		`{"role":"user","content":"what time is it?"}`,
		`{"role":"assistant","content":"I don't have access to a clock."}`,
	}
	data := []byte(strings.Join(lines, "\n"))
	got := extractTranscriptSummary(data)
	if !strings.Contains(got, "Hi there!") {
		t.Fatalf("expected assistant message, got %q", got)
	}
	if !strings.Contains(got, "don't have access") {
		t.Fatalf("expected second assistant message, got %q", got)
	}
}

func TestExtractTranscriptSummary_TakesLast5(t *testing.T) {
	var lines []string
	for i := 0; i < 10; i++ {
		lines = append(lines, `{"role":"assistant","content":"msg`+string(rune('A'+i))+`"}`)
	}
	data := []byte(strings.Join(lines, "\n"))
	got := extractTranscriptSummary(data)
	// Should only contain the last 5 (F through J)
	if strings.Contains(got, "msgA") {
		t.Fatal("should not contain first messages")
	}
}
