package heimdall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseHookLogLine_Happy(t *testing.T) {
	line := "2026-04-16T20:56:22Z INFO event=user-prompt bytes=1107 hits=5 model=nomic-embed-text scope= session=abc-xyz skills=3 stage=ok"
	entry, ok := parseHookLogLine(line)
	if !ok {
		t.Fatalf("expected parse ok")
	}
	if entry.Level != "INFO" {
		t.Errorf("level: got %q", entry.Level)
	}
	if entry.Event != "user-prompt" {
		t.Errorf("event: got %q", entry.Event)
	}
	if entry.Session != "abc-xyz" {
		t.Errorf("session: got %q", entry.Session)
	}
	if entry.Fields["stage"] != "ok" {
		t.Errorf("stage: got %q", entry.Fields["stage"])
	}
	want, _ := time.Parse(time.RFC3339, "2026-04-16T20:56:22Z")
	if !entry.Timestamp.Equal(want) {
		t.Errorf("ts: got %v want %v", entry.Timestamp, want)
	}
}

func TestParseHookLogLine_NoEventRejected(t *testing.T) {
	_, ok := parseHookLogLine("2026-04-16T20:56:22Z INFO no_event_here=1")
	if ok {
		t.Fatalf("expected reject")
	}
}

func TestParseHookLogLine_EmptyString(t *testing.T) {
	_, ok := parseHookLogLine("")
	if ok {
		t.Fatalf("expected reject on empty")
	}
}

func TestParseHookLogLine_ValueWithEquals(t *testing.T) {
	line := "2026-04-16T20:56:22Z WARN event=user-prompt err=ollama_embed:_status_400 session=x"
	entry, ok := parseHookLogLine(line)
	if !ok {
		t.Fatalf("parse ok")
	}
	if !strings.Contains(entry.Fields["err"], "ollama_embed") {
		t.Errorf("err: got %q", entry.Fields["err"])
	}
}

func TestReadHookLog_FiltersBySession(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "hooks.log")
	content := strings.Join([]string{
		"2026-04-16T20:00:00Z INFO event=session-start session=A stage=ok",
		"2026-04-16T20:00:01Z INFO event=user-prompt session=A stage=ok",
		"2026-04-16T20:00:02Z INFO event=user-prompt session=B stage=ok",
		"2026-04-16T20:00:03Z INFO event=pre-tool-use class=allow mode=shadow session=A",
		"",
	}, "\n")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	entries, err := ReadHookLog(ReadHookLogOpts{Path: p, Session: "A"})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d want 3 entries for session A", len(entries))
	}

	all, err := ReadHookLog(ReadHookLogOpts{Path: p})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("unfiltered count got %d want 4", len(all))
	}
}

func TestReadHookLog_MissingFileReturnsEmpty(t *testing.T) {
	entries, err := ReadHookLog(ReadHookLogOpts{Path: "does-not-exist-xxx.log"})
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected empty, got %d", len(entries))
	}
}

func TestAggregateBySession(t *testing.T) {
	entries := []HookLogEntry{
		{Event: "session-start", Session: "A", Fields: map[string]string{"stage": "ok", "bullets": "5"}},
		{Event: "user-prompt", Session: "A", Fields: map[string]string{"stage": "ok", "bytes": "1107", "hits": "5"}},
		{Event: "user-prompt", Session: "A", Fields: map[string]string{"stage": "cache_hit", "bytes": "900"}},
		{Event: "user-prompt", Session: "A", Fields: map[string]string{"stage": "skip", "reason": "prompt_too_short"}},
		{Event: "pre-tool-use", Session: "A", Fields: map[string]string{"class": "allow", "mode": "shadow"}},
		{Event: "pre-tool-use", Session: "A", Fields: map[string]string{"class": "block", "mode": "shadow"}},
		{Event: "post-edit-actor", Session: "A", Fields: map[string]string{"msg": "reindex_ok", "files": "1"}},
		{Event: "stop", Session: "A", Fields: map[string]string{"msg": "buffer_appended", "bytes": "231"}},
		{Event: "user-prompt", Session: "B", Fields: map[string]string{"stage": "ok"}},
	}
	got := AggregateHookLogBySession(entries)
	a := got["A"]
	if a.UserPromptEvents != 3 {
		t.Errorf("UserPromptEvents A: got %d want 3", a.UserPromptEvents)
	}
	if a.UserPromptCacheHits != 1 {
		t.Errorf("cache hits: got %d", a.UserPromptCacheHits)
	}
	if a.UserPromptSkips != 1 {
		t.Errorf("skips: got %d", a.UserPromptSkips)
	}
	if a.GuardrailVerdicts["allow"] != 1 || a.GuardrailVerdicts["block"] != 1 {
		t.Errorf("guardrail verdicts: %+v", a.GuardrailVerdicts)
	}
	if a.UserPromptBytesInjected != 2007 {
		t.Errorf("UserPromptBytesInjected: got %d want 2007", a.UserPromptBytesInjected)
	}
	if a.PostEditReindexes != 1 {
		t.Errorf("PostEditReindexes: got %d", a.PostEditReindexes)
	}
	if got["B"].UserPromptEvents != 1 {
		t.Errorf("B count: got %d", got["B"].UserPromptEvents)
	}
}
