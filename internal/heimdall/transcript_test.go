package heimdall

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestTranscriptPathForSession_BuildsSlug(t *testing.T) {
	got, err := TranscriptPathForSession("/home/alice/Code/proj", "abc-123", "/home/alice")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := filepath.Join("/home/alice", ".claude", "projects", "-home-alice-Code-proj", "abc-123.jsonl")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestTranscriptPathForSession_EmptySessionErrors(t *testing.T) {
	_, err := TranscriptPathForSession("/home/alice/Code/proj", "", "/home/alice")
	if err == nil {
		t.Fatalf("expected error on empty session id")
	}
}

func TestTranscriptPathForSession_WindowsPathIgnoredOnPosix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only slug rule")
	}
	got, _ := TranscriptPathForSession("/home/alice/Code/proj", "s", "/home/alice")
	if filepath.Base(filepath.Dir(got)) != "-home-alice-Code-proj" {
		t.Fatalf("unexpected slug dir: %s", got)
	}
}

func TestParseTranscript_Basic(t *testing.T) {
	sum, err := ParseTranscript("testdata/transcript_basic.jsonl")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sum.SessionID != "bas-1" {
		t.Errorf("session id: got %q", sum.SessionID)
	}
	if sum.UserMessages != 2 {
		t.Errorf("user messages: got %d want 2", sum.UserMessages)
	}
	if sum.AssistantMessages != 2 {
		t.Errorf("assistant messages: got %d want 2", sum.AssistantMessages)
	}
	if sum.TotalInputTokens != 14 {
		t.Errorf("input tokens: got %d want 14", sum.TotalInputTokens)
	}
	if sum.TotalCacheCreationTokens != 100 {
		t.Errorf("cache creation: got %d want 100", sum.TotalCacheCreationTokens)
	}
	if sum.TotalCacheReadTokens != 200 {
		t.Errorf("cache read: got %d want 200", sum.TotalCacheReadTokens)
	}
	if sum.TotalOutputTokens != 13 {
		t.Errorf("output: got %d want 13", sum.TotalOutputTokens)
	}
	if sum.ToolUseCount != 1 {
		t.Errorf("tool use count: got %d want 1", sum.ToolUseCount)
	}
	if sum.ToolUseByName["Read"] != 1 {
		t.Errorf("ToolUseByName[Read]: got %d want 1", sum.ToolUseByName["Read"])
	}
	if sum.HookSuccessCountByEvent["SessionStart"] != 1 {
		t.Errorf("hook success count SessionStart: got %d want 1", sum.HookSuccessCountByEvent["SessionStart"])
	}
	wantBytes := int64(len("## Heimdall context\n\n> test bytes"))
	if sum.HookSuccessBytesByEvent["SessionStart"] != wantBytes {
		t.Errorf("hook bytes: got %d want %d", sum.HookSuccessBytesByEvent["SessionStart"], wantBytes)
	}
}

func TestParseTranscript_FileMissing(t *testing.T) {
	_, err := ParseTranscript("testdata/does-not-exist.jsonl")
	if err == nil {
		t.Fatalf("expected error")
	}
}

func TestParseTranscript_HeimdallToolsCounted(t *testing.T) {
	sum, err := ParseTranscript("testdata/transcript_heimdall.jsonl")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sum.ToolUseCount != 3 {
		t.Errorf("ToolUseCount: got %d want 3", sum.ToolUseCount)
	}
	if sum.HeimdallToolCalls != 2 {
		t.Errorf("HeimdallToolCalls: got %d want 2", sum.HeimdallToolCalls)
	}
	if sum.ToolUseByName["mcp__heimdall__heimdall_search"] != 1 {
		t.Errorf("heimdall_search: got %d", sum.ToolUseByName["mcp__heimdall__heimdall_search"])
	}
	if sum.HookSuccessBytesByEvent["UserPromptSubmit"] == 0 {
		t.Errorf("expected non-zero UserPromptSubmit bytes")
	}
}

func TestParseTranscript_MalformedLineCounted(t *testing.T) {
	// Write a fixture with one valid line and one garbage line to a tempdir.
	tmp := t.TempDir()
	p := filepath.Join(tmp, "t.jsonl")
	content := "{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":\"ok\"},\"sessionId\":\"s\"}\n" +
		"not json at all\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sum, err := ParseTranscript(p)
	if err != nil {
		t.Fatalf("parse returned error for tolerable malformed: %v", err)
	}
	if sum.ParseErrors != 1 {
		t.Errorf("parse errors: got %d want 1", sum.ParseErrors)
	}
	if sum.UserMessages != 1 {
		t.Errorf("user messages: got %d want 1", sum.UserMessages)
	}
}
