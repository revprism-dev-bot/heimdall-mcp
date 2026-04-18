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

// TestHookStop_LogsSingleEventStamp is a regression guard: the Stop hook handler
// must not pass "event" in its kv map to LogHookEvent — the logger already
// stamps `event=<name>` from its own `event` parameter. Passing it in kv too
// produced a duplicate token in the log line (e.g. `event=stop ... event=stop`),
// breaking grep-friendly parsing and making the output look malformed.
func TestHookStop_LogsSingleEventStamp(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HEIMDALL_HOOK_LOG", filepath.Join(tmp, "hooks.log"))
	t.Setenv("HEIMDALL_HOOKS", "1")

	projectDir := t.TempDir()
	payload := stopPayload{
		SessionID:            "single-stamp-session",
		CWD:                  projectDir,
		LastAssistantMessage: "msg",
		StopHookActive:       true,
	}
	data, _ := json.Marshal(payload)
	code := HookStop(config.DefaultConfig(), bytes.NewReader(data), &bytes.Buffer{}, &bytes.Buffer{}, map[string]string{}, nil)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}

	logBytes, err := os.ReadFile(filepath.Join(tmp, "hooks.log"))
	if err != nil {
		t.Fatalf("hook log not written: %v", err)
	}
	logStr := string(logBytes)
	// Find the buffer_appended line — that's the one with the dup before the fix.
	var target string
	for _, line := range strings.Split(strings.TrimSpace(logStr), "\n") {
		if strings.Contains(line, "buffer_appended") {
			target = line
			break
		}
	}
	if target == "" {
		t.Fatalf("no buffer_appended line in hook log:\n%s", logStr)
	}
	if n := strings.Count(target, "event=stop"); n != 1 {
		t.Fatalf("expected exactly 1 `event=stop` token on buffer_appended line, got %d:\n%s", n, target)
	}
}

// TestHookSessionEnd_LogsSingleEventStamp is the SessionEnd counterpart to the
// Stop regression above. Same bug (`"event": "session-end"` in the kv map on
// top of the logger's own stamp), same fix.
func TestHookSessionEnd_LogsSingleEventStamp(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HEIMDALL_HOOK_LOG", filepath.Join(tmp, "hooks.log"))
	t.Setenv("HEIMDALL_HOOKS", "1")

	projectDir := t.TempDir()
	// No buffer file needed; HookSessionEnd will still log session_ended.
	payload := sessionEndPayload{
		SessionID:     "single-stamp-session-end",
		CWD:           projectDir,
		HookEventName: "SessionEnd",
		Reason:        "clear",
	}
	data, _ := json.Marshal(payload)
	code := HookSessionEnd(config.DefaultConfig(), bytes.NewReader(data), &bytes.Buffer{}, &bytes.Buffer{}, map[string]string{}, nil)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}

	logBytes, err := os.ReadFile(filepath.Join(tmp, "hooks.log"))
	if err != nil {
		t.Fatalf("hook log not written: %v", err)
	}
	logStr := string(logBytes)
	var target string
	for _, line := range strings.Split(strings.TrimSpace(logStr), "\n") {
		if strings.Contains(line, "session_ended") {
			target = line
			break
		}
	}
	if target == "" {
		t.Fatalf("no session_ended line in hook log:\n%s", logStr)
	}
	if n := strings.Count(target, "event=session-end"); n != 1 {
		t.Fatalf("expected exactly 1 `event=session-end` token on session_ended line, got %d:\n%s", n, target)
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
