package heimdall

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

func TestParseTranscript_RedundantHeimdallCalls_CountsWhenHookInjectedHits(t *testing.T) {
	// Turn 1: user -> UserPromptSubmit hook attachment with stdout (hits) ->
	// assistant makes 1 heimdall_* tool call. That call is redundant.
	tmp := t.TempDir()
	p := filepath.Join(tmp, "t.jsonl")
	content := `{"type":"user","message":{"role":"user","content":"q"},"sessionId":"r1","timestamp":"2026-03-01T00:00:00Z"}` + "\n" +
		`{"type":"attachment","attachment":{"type":"hook_success","hookEvent":"UserPromptSubmit","stdout":"## Heimdall context\n\nhits"},"sessionId":"r1","timestamp":"2026-03-01T00:00:00Z"}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"mcp__heimdall__heimdall_search","input":{"q":"x"}}],"usage":{"input_tokens":1,"output_tokens":1}},"sessionId":"r1","timestamp":"2026-03-01T00:00:01Z"}` + "\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sum, err := ParseTranscript(p)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sum.RedundantHeimdallCalls != 1 {
		t.Errorf("RedundantHeimdallCalls: got %d want 1", sum.RedundantHeimdallCalls)
	}
}

func TestParseTranscript_RedundantHeimdallCalls_NoCallNoRedundancy(t *testing.T) {
	// Turn with prompt-hits>0 AND no heimdall_* tool call -> not counted.
	tmp := t.TempDir()
	p := filepath.Join(tmp, "t.jsonl")
	content := `{"type":"user","message":{"role":"user","content":"q"},"sessionId":"r2","timestamp":"2026-03-01T00:00:00Z"}` + "\n" +
		`{"type":"attachment","attachment":{"type":"hook_success","hookEvent":"UserPromptSubmit","stdout":"## Heimdall context\n\nhits"},"sessionId":"r2","timestamp":"2026-03-01T00:00:00Z"}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"Read","input":{"file_path":"/x"}}],"usage":{"input_tokens":1,"output_tokens":1}},"sessionId":"r2","timestamp":"2026-03-01T00:00:01Z"}` + "\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sum, err := ParseTranscript(p)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sum.RedundantHeimdallCalls != 0 {
		t.Errorf("RedundantHeimdallCalls: got %d want 0", sum.RedundantHeimdallCalls)
	}
}

func TestParseTranscript_RedundantHeimdallCalls_NoHitsNoRedundancy(t *testing.T) {
	// Turn with prompt-hits=0 AND heimdall_* tool call -> not counted.
	// Simulates a turn where the hook skipped (no attachment / empty stdout).
	tmp := t.TempDir()
	p := filepath.Join(tmp, "t.jsonl")
	content := `{"type":"user","message":{"role":"user","content":"q"},"sessionId":"r3","timestamp":"2026-03-01T00:00:00Z"}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"mcp__heimdall__heimdall_search","input":{"q":"x"}}],"usage":{"input_tokens":1,"output_tokens":1}},"sessionId":"r3","timestamp":"2026-03-01T00:00:01Z"}` + "\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sum, err := ParseTranscript(p)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sum.RedundantHeimdallCalls != 0 {
		t.Errorf("RedundantHeimdallCalls: got %d want 0", sum.RedundantHeimdallCalls)
	}
	if sum.HeimdallToolCalls != 1 {
		t.Errorf("HeimdallToolCalls should still count: got %d want 1", sum.HeimdallToolCalls)
	}
}

func TestParseTranscript_RedundantHeimdallCalls_MultipleCallsInTurn(t *testing.T) {
	// Multiple heimdall calls in one post-hit turn -> each counted.
	tmp := t.TempDir()
	p := filepath.Join(tmp, "t.jsonl")
	content := `{"type":"user","message":{"role":"user","content":"q"},"sessionId":"r4","timestamp":"2026-03-01T00:00:00Z"}` + "\n" +
		`{"type":"attachment","attachment":{"type":"hook_success","hookEvent":"UserPromptSubmit","stdout":"## Heimdall context\n\nhits"},"sessionId":"r4","timestamp":"2026-03-01T00:00:00Z"}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[` +
		`{"type":"tool_use","id":"a","name":"mcp__heimdall__heimdall_search","input":{"q":"x"}},` +
		`{"type":"tool_use","id":"b","name":"mcp__heimdall__heimdall_recall","input":{"q":"y"}},` +
		`{"type":"tool_use","id":"c","name":"Read","input":{"file_path":"/x"}}` +
		`],"usage":{"input_tokens":1,"output_tokens":1}},"sessionId":"r4","timestamp":"2026-03-01T00:00:01Z"}` + "\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sum, err := ParseTranscript(p)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sum.RedundantHeimdallCalls != 2 {
		t.Errorf("RedundantHeimdallCalls: got %d want 2", sum.RedundantHeimdallCalls)
	}
}

func TestParseTranscript_RedundantHeimdallCalls_MultipleTurnsMixed(t *testing.T) {
	// Turn 1: hits + heimdall call (+1). Turn 2: hits + no heimdall call (+0).
	// Turn 3: no hits + heimdall call (+0). Total: 1.
	tmp := t.TempDir()
	p := filepath.Join(tmp, "t.jsonl")
	content := strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":"q1"},"sessionId":"r5","timestamp":"2026-03-01T00:00:00Z"}`,
		`{"type":"attachment","attachment":{"type":"hook_success","hookEvent":"UserPromptSubmit","stdout":"## Heimdall context\n\nhits"},"sessionId":"r5","timestamp":"2026-03-01T00:00:00Z"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"mcp__heimdall__heimdall_search","input":{"q":"x"}}],"usage":{"input_tokens":1,"output_tokens":1}},"sessionId":"r5","timestamp":"2026-03-01T00:00:01Z"}`,
		`{"type":"user","message":{"role":"user","content":"q2"},"sessionId":"r5","timestamp":"2026-03-01T00:00:02Z"}`,
		`{"type":"attachment","attachment":{"type":"hook_success","hookEvent":"UserPromptSubmit","stdout":"## Heimdall context\n\nhits"},"sessionId":"r5","timestamp":"2026-03-01T00:00:02Z"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"b","name":"Read","input":{"file_path":"/x"}}],"usage":{"input_tokens":1,"output_tokens":1}},"sessionId":"r5","timestamp":"2026-03-01T00:00:03Z"}`,
		`{"type":"user","message":{"role":"user","content":"q3"},"sessionId":"r5","timestamp":"2026-03-01T00:00:04Z"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"c","name":"mcp__heimdall__heimdall_search","input":{"q":"y"}}],"usage":{"input_tokens":1,"output_tokens":1}},"sessionId":"r5","timestamp":"2026-03-01T00:00:05Z"}`,
		"",
	}, "\n")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sum, err := ParseTranscript(p)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sum.RedundantHeimdallCalls != 1 {
		t.Errorf("RedundantHeimdallCalls: got %d want 1", sum.RedundantHeimdallCalls)
	}
}

func TestParseTranscript_RedundantHeimdallCalls_EmptyStdoutNotAHit(t *testing.T) {
	// Attachment with empty stdout = hook skip/no-hits -> don't count
	// subsequent heimdall calls as redundant.
	tmp := t.TempDir()
	p := filepath.Join(tmp, "t.jsonl")
	content := `{"type":"user","message":{"role":"user","content":"q"},"sessionId":"r6","timestamp":"2026-03-01T00:00:00Z"}` + "\n" +
		`{"type":"attachment","attachment":{"type":"hook_success","hookEvent":"UserPromptSubmit","stdout":""},"sessionId":"r6","timestamp":"2026-03-01T00:00:00Z"}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"mcp__heimdall__heimdall_search","input":{"q":"x"}}],"usage":{"input_tokens":1,"output_tokens":1}},"sessionId":"r6","timestamp":"2026-03-01T00:00:01Z"}` + "\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sum, err := ParseTranscript(p)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sum.RedundantHeimdallCalls != 0 {
		t.Errorf("RedundantHeimdallCalls: got %d want 0 (empty stdout is not a hit)", sum.RedundantHeimdallCalls)
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
