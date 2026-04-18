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

// TestParseHookLogLine_QuotedValueWithSpaces is the reader-side fix for the
// motivating bug: a quoted value containing whitespace must deserialize back
// to the original string.
func TestParseHookLogLine_QuotedValueWithSpaces(t *testing.T) {
	line := `2026-04-16T20:56:22Z WARN event=pre-tool-use class=block mode=shadow reason="rm -rf at root" rule_id=RM_RF_ROOT session=S1`
	entry, ok := parseHookLogLine(line)
	if !ok {
		t.Fatalf("parse failed on quoted-value line: %q", line)
	}
	if got := entry.Fields["reason"]; got != "rm -rf at root" {
		t.Errorf("reason: got %q want %q", got, "rm -rf at root")
	}
	if got := entry.Fields["rule_id"]; got != "RM_RF_ROOT" {
		t.Errorf("rule_id: got %q want %q", got, "RM_RF_ROOT")
	}
	if got := entry.Session; got != "S1" {
		t.Errorf("session: got %q want %q", got, "S1")
	}
	if got := entry.Fields["class"]; got != "block" {
		t.Errorf("class: got %q want %q", got, "block")
	}
}

// TestParseHookLogLine_EscapedQuoteInsideValue — a quoted value that itself
// contains an escaped double quote must unescape correctly.
func TestParseHookLogLine_EscapedQuoteInsideValue(t *testing.T) {
	line := `2026-04-16T20:56:22Z INFO event=e note="she said \"hi\" loudly" session=S`
	entry, ok := parseHookLogLine(line)
	if !ok {
		t.Fatalf("parse failed")
	}
	want := `she said "hi" loudly`
	if got := entry.Fields["note"]; got != want {
		t.Errorf("note: got %q want %q", got, want)
	}
}

// TestParseHookLogLine_UnterminatedQuoteDegradesCleanly — a malformed line
// (opening quote without a matching close) must NOT panic and must NOT drop
// fields parsed before the malformed token. Unterminated value captures the
// remainder of the line verbatim (sans the opening quote).
func TestParseHookLogLine_UnterminatedQuoteDegradesCleanly(t *testing.T) {
	line := `2026-04-16T20:56:22Z INFO event=e good=ok reason="unterminated value without close`
	entry, ok := parseHookLogLine(line)
	if !ok {
		t.Fatalf("parse returned !ok; must degrade gracefully, not drop the whole line")
	}
	if entry.Event != "e" {
		t.Errorf("event lost: got %q", entry.Event)
	}
	if entry.Fields["good"] != "ok" {
		t.Errorf("fields before malformed token lost: got %q", entry.Fields["good"])
	}
	// The malformed field should still land in the map with a best-effort
	// value — what matters is we neither panic nor drop the earlier data.
	if _, present := entry.Fields["reason"]; !present {
		t.Errorf("malformed field missing from output entirely; expected best-effort capture")
	}
}

// TestParseHookLogLine_SimpleValuesUnchanged — backwards-compat canary for
// the reader. An all-simple-value line must parse exactly as before.
func TestParseHookLogLine_SimpleValuesUnchanged(t *testing.T) {
	line := "2026-04-16T20:56:22Z INFO event=user-prompt bytes=1107 hits=5 model=nomic-embed-text scope= session=abc-xyz skills=3 stage=ok"
	entry, ok := parseHookLogLine(line)
	if !ok {
		t.Fatal("parse failed")
	}
	want := map[string]string{
		"bytes":   "1107",
		"hits":    "5",
		"model":   "nomic-embed-text",
		"scope":   "",
		"session": "abc-xyz",
		"skills":  "3",
		"stage":   "ok",
	}
	for k, v := range want {
		if got := entry.Fields[k]; got != v {
			t.Errorf("field %q: got %q want %q", k, got, v)
		}
	}
}

// TestHookLogRoundTrip_WriteThenRead — the writer's quoting protocol and the
// reader's tokenizer must be lossless. For each adversarial input, write it
// via LogHookEvent, read it back via ReadHookLog, and confirm field equality.
func TestHookLogRoundTrip_WriteThenRead(t *testing.T) {
	cases := []struct {
		name string
		val  string
	}{
		{"plain", "hello"},
		{"with_spaces", "rm -rf at root"},
		{"with_embedded_quote", `she said "hi"`},
		{"with_tab", "a\tb\tc"},
		{"with_trailing_space", "trailing "},
		{"with_leading_space", " leading"},
		{"mixed_quotes_and_spaces", `one "two" three`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := setupTmpHookLog(t)
			LogHookEvent("INFO", "rt", map[string]any{
				"payload": tc.val,
				"session": "SID",
			})
			entries, err := ReadHookLog(ReadHookLogOpts{Path: path})
			if err != nil {
				t.Fatalf("ReadHookLog: %v", err)
			}
			if len(entries) != 1 {
				t.Fatalf("entries: got %d want 1", len(entries))
			}
			if got := entries[0].Fields["payload"]; got != tc.val {
				t.Errorf("round-trip: got %q want %q", got, tc.val)
			}
			if entries[0].Session != "SID" {
				t.Errorf("session round-trip: got %q", entries[0].Session)
			}
		})
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
	// UserPromptCacheTotal = stage=ok + stage=cache_hit (the "cache-eligible
	// attempts" denominator). Excludes stage=skip and degraded stages.
	if a.UserPromptCacheTotal != 2 {
		t.Errorf("UserPromptCacheTotal A: got %d want 2", a.UserPromptCacheTotal)
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
	if got["B"].UserPromptCacheTotal != 1 {
		t.Errorf("UserPromptCacheTotal B: got %d want 1", got["B"].UserPromptCacheTotal)
	}
}

// TestAggregateBySession_CacheTotalExcludesDegraded verifies that only stage=ok
// and stage=cache_hit count toward UserPromptCacheTotal. Degraded stages
// (ollama_ping, embed errors, resolve_model failures) and stage=skip MUST
// NOT be counted — those aren't attempts the cache layer got to observe.
func TestAggregateBySession_CacheTotalExcludesDegraded(t *testing.T) {
	entries := []HookLogEntry{
		{Event: "user-prompt", Session: "S", Fields: map[string]string{"stage": "ok"}},
		{Event: "user-prompt", Session: "S", Fields: map[string]string{"stage": "cache_hit"}},
		{Event: "user-prompt", Session: "S", Fields: map[string]string{"stage": "skip"}},
		{Event: "user-prompt", Session: "S", Fields: map[string]string{"stage": "ollama_ping"}},
		{Event: "user-prompt", Session: "S", Fields: map[string]string{"stage": "embed"}},
	}
	got := AggregateHookLogBySession(entries)
	s := got["S"]
	if s.UserPromptCacheHits != 1 {
		t.Errorf("cache_hits: got %d want 1", s.UserPromptCacheHits)
	}
	if s.UserPromptCacheTotal != 2 {
		t.Errorf("cache_total: got %d want 2 (stage=ok + stage=cache_hit only)", s.UserPromptCacheTotal)
	}
}
