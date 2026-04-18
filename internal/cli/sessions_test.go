package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/config"
)

func TestSessionsList_EmptyLog(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HEIMDALL_HOOK_LOG", filepath.Join(tmp, "hooks.log"))

	var out, errBuf bytes.Buffer
	rc := DispatchSessions(config.Config{}, nil, &out, &errBuf, map[string]string{}, []string{"list"})
	if rc != 0 {
		t.Fatalf("rc: %d", rc)
	}
	if !strings.Contains(out.String(), "no sessions") {
		t.Fatalf("expected no-sessions message, got %q", out.String())
	}
}

func TestSessionsList_HappyPath(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "hooks.log")
	t.Setenv("HEIMDALL_HOOK_LOG", logPath)

	lines := strings.Join([]string{
		"2026-04-16T20:00:00Z INFO event=session-start session=A stage=ok",
		"2026-04-16T20:00:01Z INFO event=user-prompt session=A stage=ok",
		"2026-04-16T20:00:02Z INFO event=user-prompt session=A stage=cache_hit",
		"2026-04-16T20:05:00Z INFO event=session-start session=B stage=ok",
		"",
	}, "\n")
	if err := os.WriteFile(logPath, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errBuf bytes.Buffer
	rc := DispatchSessions(config.Config{}, nil, &out, &errBuf, map[string]string{}, []string{"list"})
	if rc != 0 {
		t.Fatalf("rc: %d", rc)
	}
	s := out.String()
	if !strings.Contains(s, "A") || !strings.Contains(s, "B") {
		t.Fatalf("expected both sessions in output:\n%s", s)
	}
	if !strings.Contains(s, "prompts=2") {
		t.Fatalf("expected prompts=2 for A:\n%s", s)
	}
}

func TestSessionsReport_TextFormat(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "hooks.log")
	t.Setenv("HEIMDALL_HOOK_LOG", logPath)

	lines := strings.Join([]string{
		"2026-04-16T20:00:00Z INFO event=session-start session=REPORTME stage=ok",
		"2026-04-16T20:00:01Z INFO event=user-prompt session=REPORTME stage=ok bytes=1100",
		"2026-04-16T20:00:02Z INFO event=user-prompt session=REPORTME stage=cache_hit bytes=900",
		"2026-04-16T20:00:03Z INFO event=pre-tool-use class=allow mode=shadow session=REPORTME",
		"",
	}, "\n")
	_ = os.WriteFile(logPath, []byte(lines), 0o600)

	// A fake transcript in the expected layout.
	home := filepath.Join(tmp, "home")
	slugDir := filepath.Join(home, ".claude", "projects", "-tmp-proj")
	_ = os.MkdirAll(slugDir, 0o755)
	transcriptPath := filepath.Join(slugDir, "REPORTME.jsonl")
	tdata := strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":"hi"},"sessionId":"REPORTME","timestamp":"2026-04-16T20:00:01Z"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":20,"cache_creation_input_tokens":100,"cache_read_input_tokens":500,"output_tokens":10}},"sessionId":"REPORTME","timestamp":"2026-04-16T20:00:02Z"}`,
	}, "\n")
	_ = os.WriteFile(transcriptPath, []byte(tdata), 0o600)

	t.Setenv("HOME", home)
	var out, errBuf bytes.Buffer
	rc := DispatchSessions(config.Config{}, nil, &out, &errBuf, map[string]string{},
		[]string{"report", "--session-id=REPORTME", "--cwd=/tmp/proj", "--format=text"})
	if rc != 0 {
		t.Fatalf("rc: %d stderr: %s", rc, errBuf.String())
	}
	s := out.String()
	for _, needle := range []string{"REPORTME", "input_tokens=20", "cache_read=500", "prompts=2", "cache_hits=1", "guardrail=1"} {
		if !strings.Contains(s, needle) {
			t.Errorf("expected %q in report:\n%s", needle, s)
		}
	}
}

func TestSessionsList_SinceFilter(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "hooks.log")
	t.Setenv("HEIMDALL_HOOK_LOG", logPath)

	// `OLD` session's last event is 48h before now; `NEW` is 30m before now.
	now := time.Now().UTC()
	oldTS := now.Add(-48 * time.Hour).Format(time.RFC3339)
	newTS := now.Add(-30 * time.Minute).Format(time.RFC3339)
	lines := strings.Join([]string{
		oldTS + " INFO event=session-start session=OLD stage=ok",
		oldTS + " INFO event=user-prompt session=OLD stage=ok",
		newTS + " INFO event=session-start session=NEW stage=ok",
		newTS + " INFO event=user-prompt session=NEW stage=ok",
		"",
	}, "\n")
	if err := os.WriteFile(logPath, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}

	// --since=24h must keep only NEW.
	var out, errBuf bytes.Buffer
	rc := DispatchSessions(config.Config{}, nil, &out, &errBuf, map[string]string{},
		[]string{"list", "--since=24h"})
	if rc != 0 {
		t.Fatalf("rc: %d stderr: %s", rc, errBuf.String())
	}
	if strings.Contains(out.String(), "OLD") {
		t.Errorf("--since=24h should have excluded OLD:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "NEW") {
		t.Errorf("--since=24h should have kept NEW:\n%s", out.String())
	}

	// --since=1m must keep nothing and print the empty-window message.
	out.Reset()
	errBuf.Reset()
	rc = DispatchSessions(config.Config{}, nil, &out, &errBuf, map[string]string{},
		[]string{"list", "--since=1m"})
	if rc != 0 {
		t.Fatalf("rc: %d stderr: %s", rc, errBuf.String())
	}
	if !strings.Contains(out.String(), "--since=1m window") {
		t.Errorf("expected empty-window hint, got: %q", out.String())
	}
}

func TestSessionsList_SinceInvalid(t *testing.T) {
	var out, errBuf bytes.Buffer
	rc := DispatchSessions(config.Config{}, nil, &out, &errBuf, map[string]string{},
		[]string{"list", "--since=not-a-duration"})
	if rc != 2 {
		t.Fatalf("expected rc=2 on bad duration, got %d", rc)
	}
	if !strings.Contains(errBuf.String(), "invalid --since") {
		t.Errorf("expected 'invalid --since' in stderr, got: %q", errBuf.String())
	}
}

func TestSessionsList_JSONSchemaVersion(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "hooks.log")
	t.Setenv("HEIMDALL_HOOK_LOG", logPath)
	_ = os.WriteFile(logPath,
		[]byte("2026-04-16T20:00:00Z INFO event=user-prompt session=X stage=ok\n"), 0o600)

	var out, errBuf bytes.Buffer
	rc := DispatchSessions(config.Config{}, nil, &out, &errBuf, map[string]string{},
		[]string{"list", "--format=json"})
	if rc != 0 {
		t.Fatalf("rc: %d stderr: %s", rc, errBuf.String())
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if parsed["schema_version"] != SessionsReportSchemaVersion {
		t.Errorf("schema_version: got %v, want %s", parsed["schema_version"], SessionsReportSchemaVersion)
	}
	sessions, ok := parsed["sessions"].([]interface{})
	if !ok {
		t.Fatalf("sessions key missing or wrong type: %v", parsed["sessions"])
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session row, got %d", len(sessions))
	}
	row := sessions[0].(map[string]interface{})
	if row["session_id"] != "X" {
		t.Errorf("session_id: %v", row["session_id"])
	}
	if row["first_seen"] == "" || row["last_seen"] == "" {
		t.Errorf("expected first_seen/last_seen populated: %v", row)
	}
}

func TestSessionsList_JSONEmpty(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HEIMDALL_HOOK_LOG", filepath.Join(tmp, "hooks.log"))

	var out, errBuf bytes.Buffer
	rc := DispatchSessions(config.Config{}, nil, &out, &errBuf, map[string]string{},
		[]string{"list", "--format=json"})
	if rc != 0 {
		t.Fatalf("rc: %d", rc)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if parsed["schema_version"] != SessionsReportSchemaVersion {
		t.Errorf("schema_version: %v", parsed["schema_version"])
	}
}

func TestSessionsReport_JSONSchemaVersion(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "hooks.log")
	t.Setenv("HEIMDALL_HOOK_LOG", logPath)
	_ = os.WriteFile(logPath,
		[]byte("2026-04-16T20:00:01Z INFO event=user-prompt session=V stage=ok bytes=1\n"), 0o600)

	home := filepath.Join(tmp, "home")
	slugDir := filepath.Join(home, ".claude", "projects", "-tmp-proj")
	_ = os.MkdirAll(slugDir, 0o755)
	_ = os.WriteFile(filepath.Join(slugDir, "V.jsonl"),
		[]byte(`{"type":"user","message":{"role":"user","content":"hi"},"sessionId":"V"}`+"\n"), 0o600)

	t.Setenv("HOME", home)
	var out, errBuf bytes.Buffer
	rc := DispatchSessions(config.Config{}, nil, &out, &errBuf, map[string]string{},
		[]string{"report", "--session-id=V", "--cwd=/tmp/proj", "--format=json"})
	if rc != 0 {
		t.Fatalf("rc: %d stderr: %s", rc, errBuf.String())
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if parsed["schema_version"] != SessionsReportSchemaVersion {
		t.Errorf("schema_version: got %v, want %s", parsed["schema_version"], SessionsReportSchemaVersion)
	}
}

func TestSessionsReport_JSONFormat(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "hooks.log")
	t.Setenv("HEIMDALL_HOOK_LOG", logPath)
	_ = os.WriteFile(logPath, []byte("2026-04-16T20:00:01Z INFO event=user-prompt session=J stage=ok bytes=10\n"), 0o600)

	home := filepath.Join(tmp, "home")
	slugDir := filepath.Join(home, ".claude", "projects", "-tmp-proj")
	_ = os.MkdirAll(slugDir, 0o755)
	_ = os.WriteFile(filepath.Join(slugDir, "J.jsonl"),
		[]byte(`{"type":"user","message":{"role":"user","content":"hi"},"sessionId":"J"}`+"\n"), 0o600)

	t.Setenv("HOME", home)
	var out, errBuf bytes.Buffer
	rc := DispatchSessions(config.Config{}, nil, &out, &errBuf, map[string]string{},
		[]string{"report", "--session-id=J", "--cwd=/tmp/proj", "--format=json"})
	if rc != 0 {
		t.Fatalf("rc: %d", rc)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if parsed["session_id"] != "J" {
		t.Errorf("json session id: %v", parsed["session_id"])
	}
}

func TestSessionsReport_CurrentPicksLatest(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "hooks.log")
	t.Setenv("HEIMDALL_HOOK_LOG", logPath)

	old := time.Now().UTC().Add(-6 * time.Hour).Format(time.RFC3339)
	mid := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	newest := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	lines := strings.Join([]string{
		old + " INFO event=session-start session=OLD stage=ok",
		mid + " INFO event=session-start session=MID stage=ok",
		newest + " INFO event=session-start session=LATEST stage=ok",
		"",
	}, "\n")
	_ = os.WriteFile(logPath, []byte(lines), 0o600)

	var out, errBuf bytes.Buffer
	rc := DispatchSessions(config.Config{}, nil, &out, &errBuf, map[string]string{},
		[]string{"report", "--current", "--cwd=/tmp/proj", "--format=json"})
	if rc != 0 {
		t.Fatalf("rc=%d stderr=%s", rc, errBuf.String())
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if parsed["session_id"] != "LATEST" {
		t.Errorf("--current should resolve to LATEST, got %v", parsed["session_id"])
	}
	if !strings.Contains(errBuf.String(), "resolved --current to session LATEST") {
		t.Errorf("expected resolution notice in stderr, got: %s", errBuf.String())
	}
}

func TestSessionsReport_CurrentAndSessionIDMutuallyExclusive(t *testing.T) {
	var out, errBuf bytes.Buffer
	rc := DispatchSessions(config.Config{}, nil, &out, &errBuf, map[string]string{},
		[]string{"report", "--current", "--session-id=X"})
	if rc != 2 {
		t.Fatalf("expected rc=2, got %d", rc)
	}
	if !strings.Contains(errBuf.String(), "mutually exclusive") {
		t.Errorf("expected mutually-exclusive error, got: %s", errBuf.String())
	}
}

func TestSessionsReport_CurrentOnEmptyLog(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "hooks.log")
	t.Setenv("HEIMDALL_HOOK_LOG", logPath)
	_ = os.WriteFile(logPath, []byte(""), 0o600)

	var out, errBuf bytes.Buffer
	rc := DispatchSessions(config.Config{}, nil, &out, &errBuf, map[string]string{},
		[]string{"report", "--current"})
	if rc != 1 {
		t.Fatalf("expected rc=1, got %d", rc)
	}
	if !strings.Contains(errBuf.String(), "no sessions in hooks.log") {
		t.Errorf("expected empty-log hint, got: %s", errBuf.String())
	}
}

func TestSessionsReport_JSONIncludesRedundantHeimdallCalls(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "hooks.log")
	t.Setenv("HEIMDALL_HOOK_LOG", logPath)
	_ = os.WriteFile(logPath,
		[]byte("2026-04-16T20:00:01Z INFO event=user-prompt session=RH stage=cache_hit bytes=100\n"), 0o600)

	home := filepath.Join(tmp, "home")
	slugDir := filepath.Join(home, ".claude", "projects", "-tmp-proj")
	_ = os.MkdirAll(slugDir, 0o755)
	// Transcript: user -> UserPromptSubmit hook_success (hits) -> assistant
	// with 2 heimdall tool calls + 1 non-heimdall tool call. Expect
	// redundant_heimdall_calls=2 in the JSON output.
	tdata := strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":"q"},"sessionId":"RH","timestamp":"2026-04-16T20:00:00Z"}`,
		`{"type":"attachment","attachment":{"type":"hook_success","hookEvent":"UserPromptSubmit","stdout":"## Heimdall context\n\nhits"},"sessionId":"RH","timestamp":"2026-04-16T20:00:00Z"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[` +
			`{"type":"tool_use","id":"a","name":"mcp__heimdall__heimdall_search","input":{"q":"x"}},` +
			`{"type":"tool_use","id":"b","name":"mcp__heimdall__heimdall_recall","input":{"q":"y"}},` +
			`{"type":"tool_use","id":"c","name":"Read","input":{"file_path":"/x"}}` +
			`],"usage":{"input_tokens":20,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":10}},"sessionId":"RH","timestamp":"2026-04-16T20:00:01Z"}`,
	}, "\n")
	_ = os.WriteFile(filepath.Join(slugDir, "RH.jsonl"), []byte(tdata), 0o600)

	t.Setenv("HOME", home)
	var out, errBuf bytes.Buffer
	rc := DispatchSessions(config.Config{}, nil, &out, &errBuf, map[string]string{},
		[]string{"report", "--session-id=RH", "--cwd=/tmp/proj", "--format=json"})
	if rc != 0 {
		t.Fatalf("rc=%d stderr=%s", rc, errBuf.String())
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	// Additive-field rule: schema_version MUST still be "v1".
	if parsed["schema_version"] != SessionsReportSchemaVersion {
		t.Errorf("schema_version: got %v, want %s", parsed["schema_version"], SessionsReportSchemaVersion)
	}
	toolUse, ok := parsed["tool_use"].(map[string]interface{})
	if !ok {
		t.Fatalf("tool_use missing or wrong type: %v", parsed["tool_use"])
	}
	// json.Unmarshal returns numeric fields as float64.
	got, ok := toolUse["redundant_heimdall_calls"].(float64)
	if !ok {
		t.Fatalf("redundant_heimdall_calls missing or wrong type: %T %v", toolUse["redundant_heimdall_calls"], toolUse["redundant_heimdall_calls"])
	}
	if int(got) != 2 {
		t.Errorf("redundant_heimdall_calls: got %v want 2", got)
	}
}

func TestSessionsReport_TextFormat_IncludesRedundantHeimdallCalls(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "hooks.log")
	t.Setenv("HEIMDALL_HOOK_LOG", logPath)
	_ = os.WriteFile(logPath,
		[]byte("2026-04-16T20:00:01Z INFO event=user-prompt session=TX stage=cache_hit\n"), 0o600)

	home := filepath.Join(tmp, "home")
	slugDir := filepath.Join(home, ".claude", "projects", "-tmp-proj")
	_ = os.MkdirAll(slugDir, 0o755)
	tdata := strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":"q"},"sessionId":"TX","timestamp":"2026-04-16T20:00:00Z"}`,
		`{"type":"attachment","attachment":{"type":"hook_success","hookEvent":"UserPromptSubmit","stdout":"## Heimdall context\n\nhits"},"sessionId":"TX","timestamp":"2026-04-16T20:00:00Z"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"mcp__heimdall__heimdall_search","input":{"q":"x"}}],"usage":{"input_tokens":5,"output_tokens":5}},"sessionId":"TX","timestamp":"2026-04-16T20:00:01Z"}`,
	}, "\n")
	_ = os.WriteFile(filepath.Join(slugDir, "TX.jsonl"), []byte(tdata), 0o600)

	t.Setenv("HOME", home)
	var out, errBuf bytes.Buffer
	rc := DispatchSessions(config.Config{}, nil, &out, &errBuf, map[string]string{},
		[]string{"report", "--session-id=TX", "--cwd=/tmp/proj", "--format=text"})
	if rc != 0 {
		t.Fatalf("rc=%d stderr=%s", rc, errBuf.String())
	}
	if !strings.Contains(out.String(), "redundant_heimdall_calls=1") {
		t.Errorf("expected redundant_heimdall_calls=1 in text output:\n%s", out.String())
	}
}

// TestSessionsReport_JSONIncludesUserPromptCacheCounters verifies the
// additive user_prompt_cache_hits / user_prompt_cache_total fields land on
// the SessionReport JSON at the top level. Synthetic hooks.log has 3
// user-prompt events (1 cache_hit, 2 ok) → hits=1, total=3. Schema stays v1.
func TestSessionsReport_JSONIncludesUserPromptCacheCounters(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "hooks.log")
	t.Setenv("HEIMDALL_HOOK_LOG", logPath)
	lines := strings.Join([]string{
		"2026-04-16T20:00:00Z INFO event=user-prompt session=CH stage=cache_hit bytes=100",
		"2026-04-16T20:00:01Z INFO event=user-prompt session=CH stage=ok bytes=200",
		"2026-04-16T20:00:02Z INFO event=user-prompt session=CH stage=ok bytes=300",
		"2026-04-16T20:00:03Z INFO event=user-prompt session=CH stage=skip reason=prompt_too_short",
		"",
	}, "\n")
	_ = os.WriteFile(logPath, []byte(lines), 0o600)

	home := filepath.Join(tmp, "home")
	slugDir := filepath.Join(home, ".claude", "projects", "-tmp-proj")
	_ = os.MkdirAll(slugDir, 0o755)
	_ = os.WriteFile(filepath.Join(slugDir, "CH.jsonl"),
		[]byte(`{"type":"user","message":{"role":"user","content":"hi"},"sessionId":"CH"}`+"\n"), 0o600)

	t.Setenv("HOME", home)
	var out, errBuf bytes.Buffer
	rc := DispatchSessions(config.Config{}, nil, &out, &errBuf, map[string]string{},
		[]string{"report", "--session-id=CH", "--cwd=/tmp/proj", "--format=json"})
	if rc != 0 {
		t.Fatalf("rc=%d stderr=%s", rc, errBuf.String())
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if parsed["schema_version"] != SessionsReportSchemaVersion {
		t.Errorf("schema_version: got %v, want %s", parsed["schema_version"], SessionsReportSchemaVersion)
	}
	hits, ok := parsed["user_prompt_cache_hits"].(float64)
	if !ok {
		t.Fatalf("user_prompt_cache_hits missing or wrong type: %T %v",
			parsed["user_prompt_cache_hits"], parsed["user_prompt_cache_hits"])
	}
	if int(hits) != 1 {
		t.Errorf("user_prompt_cache_hits: got %v want 1", hits)
	}
	total, ok := parsed["user_prompt_cache_total"].(float64)
	if !ok {
		t.Fatalf("user_prompt_cache_total missing or wrong type: %T %v",
			parsed["user_prompt_cache_total"], parsed["user_prompt_cache_total"])
	}
	// stage=ok + stage=cache_hit = 3; stage=skip is excluded per contract.
	if int(total) != 3 {
		t.Errorf("user_prompt_cache_total: got %v want 3", total)
	}
}

// TestSessionsReport_JSONUserPromptCacheCountersZero verifies that a session
// with zero user-prompt events renders both counters as 0 (not missing, not
// divide-by-zero-inducing) in JSON. The schema contract requires the keys
// to be present for every report.
func TestSessionsReport_JSONUserPromptCacheCountersZero(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "hooks.log")
	t.Setenv("HEIMDALL_HOOK_LOG", logPath)
	// A session with NO user-prompt events (just a session-start), so the
	// aggregator's UserPromptCacheHits/UserPromptCacheTotal are both 0.
	_ = os.WriteFile(logPath,
		[]byte("2026-04-16T20:00:00Z INFO event=session-start session=Z stage=ok\n"), 0o600)

	home := filepath.Join(tmp, "home")
	slugDir := filepath.Join(home, ".claude", "projects", "-tmp-proj")
	_ = os.MkdirAll(slugDir, 0o755)
	_ = os.WriteFile(filepath.Join(slugDir, "Z.jsonl"),
		[]byte(`{"type":"user","message":{"role":"user","content":"hi"},"sessionId":"Z"}`+"\n"), 0o600)

	t.Setenv("HOME", home)
	var out, errBuf bytes.Buffer
	rc := DispatchSessions(config.Config{}, nil, &out, &errBuf, map[string]string{},
		[]string{"report", "--session-id=Z", "--cwd=/tmp/proj", "--format=json"})
	if rc != 0 {
		t.Fatalf("rc=%d stderr=%s", rc, errBuf.String())
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	hits, hOK := parsed["user_prompt_cache_hits"].(float64)
	total, tOK := parsed["user_prompt_cache_total"].(float64)
	if !hOK || !tOK {
		t.Fatalf("both counters must be present even at zero: hits=%v total=%v",
			parsed["user_prompt_cache_hits"], parsed["user_prompt_cache_total"])
	}
	if int(hits) != 0 || int(total) != 0 {
		t.Errorf("zero session: got hits=%v total=%v, want both 0", hits, total)
	}
}

// TestSessionsReport_TextFormat_UserPromptCacheCounters verifies the
// terse text rendering. Contract: one extra line in the UserPromptSubmit
// stats block of the form "cache_hits=N/M" where M = total cache-eligible
// attempts (stage=ok + stage=cache_hit).
func TestSessionsReport_TextFormat_UserPromptCacheCounters(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "hooks.log")
	t.Setenv("HEIMDALL_HOOK_LOG", logPath)
	lines := strings.Join([]string{
		"2026-04-16T20:00:00Z INFO event=user-prompt session=TC stage=cache_hit bytes=10",
		"2026-04-16T20:00:01Z INFO event=user-prompt session=TC stage=ok bytes=20",
		"2026-04-16T20:00:02Z INFO event=user-prompt session=TC stage=ok bytes=30",
		"",
	}, "\n")
	_ = os.WriteFile(logPath, []byte(lines), 0o600)

	home := filepath.Join(tmp, "home")
	slugDir := filepath.Join(home, ".claude", "projects", "-tmp-proj")
	_ = os.MkdirAll(slugDir, 0o755)
	_ = os.WriteFile(filepath.Join(slugDir, "TC.jsonl"),
		[]byte(`{"type":"user","message":{"role":"user","content":"hi"},"sessionId":"TC"}`+"\n"), 0o600)

	t.Setenv("HOME", home)
	var out, errBuf bytes.Buffer
	rc := DispatchSessions(config.Config{}, nil, &out, &errBuf, map[string]string{},
		[]string{"report", "--session-id=TC", "--cwd=/tmp/proj", "--format=text"})
	if rc != 0 {
		t.Fatalf("rc=%d stderr=%s", rc, errBuf.String())
	}
	// Expect the new summary line "cache_hits=1/3" somewhere in text output.
	if !strings.Contains(out.String(), "cache_hits=1/3") {
		t.Errorf("expected 'cache_hits=1/3' in text output:\n%s", out.String())
	}
}

// TestSessionsReport_TextFormat_UserPromptCacheCountersZeroSafe verifies
// that zero user-prompt events does not render a divide-by-zero or broken
// line; it should render "cache_hits=0/0".
func TestSessionsReport_TextFormat_UserPromptCacheCountersZeroSafe(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "hooks.log")
	t.Setenv("HEIMDALL_HOOK_LOG", logPath)
	_ = os.WriteFile(logPath,
		[]byte("2026-04-16T20:00:00Z INFO event=session-start session=ZT stage=ok\n"), 0o600)

	home := filepath.Join(tmp, "home")
	slugDir := filepath.Join(home, ".claude", "projects", "-tmp-proj")
	_ = os.MkdirAll(slugDir, 0o755)
	_ = os.WriteFile(filepath.Join(slugDir, "ZT.jsonl"),
		[]byte(`{"type":"user","message":{"role":"user","content":"hi"},"sessionId":"ZT"}`+"\n"), 0o600)

	t.Setenv("HOME", home)
	var out, errBuf bytes.Buffer
	rc := DispatchSessions(config.Config{}, nil, &out, &errBuf, map[string]string{},
		[]string{"report", "--session-id=ZT", "--cwd=/tmp/proj", "--format=text"})
	if rc != 0 {
		t.Fatalf("rc=%d stderr=%s", rc, errBuf.String())
	}
	if !strings.Contains(out.String(), "cache_hits=0/0") {
		t.Errorf("expected 'cache_hits=0/0' in zero-event text output:\n%s", out.String())
	}
}

// TestSessionsReport_CacheHitsAliasMatchesTopLevel verifies that the
// deprecated back-compat alias heimdall_contribution.cache_hits always
// emits the same integer as the canonical top-level user_prompt_cache_hits.
// This is the contract for the alias: both read from the same counter, and
// if they ever diverge something has been broken. Keeping this test makes
// the invariant explicit for future readers who might wonder if the two
// fields have different semantics (they don't — see sessions.go toJSON
// doc comment).
func TestSessionsReport_CacheHitsAliasMatchesTopLevel(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "hooks.log")
	t.Setenv("HEIMDALL_HOOK_LOG", logPath)
	// Realistic session: 2 cache hits, 3 fresh retrievals (ok), 1 skip, 1
	// degraded stage (ollama_ping). Expected: hits=2, total=5 (skip and
	// degraded excluded).
	lines := strings.Join([]string{
		"2026-04-16T20:00:00Z INFO event=user-prompt session=AL stage=cache_hit bytes=100",
		"2026-04-16T20:00:01Z INFO event=user-prompt session=AL stage=ok bytes=200",
		"2026-04-16T20:00:02Z INFO event=user-prompt session=AL stage=cache_hit bytes=150",
		"2026-04-16T20:00:03Z INFO event=user-prompt session=AL stage=ok bytes=300",
		"2026-04-16T20:00:04Z INFO event=user-prompt session=AL stage=skip reason=prompt_too_short",
		"2026-04-16T20:00:05Z INFO event=user-prompt session=AL stage=ok bytes=250",
		"2026-04-16T20:00:06Z INFO event=user-prompt session=AL stage=ollama_ping",
		"",
	}, "\n")
	_ = os.WriteFile(logPath, []byte(lines), 0o600)

	home := filepath.Join(tmp, "home")
	slugDir := filepath.Join(home, ".claude", "projects", "-tmp-proj")
	_ = os.MkdirAll(slugDir, 0o755)
	_ = os.WriteFile(filepath.Join(slugDir, "AL.jsonl"),
		[]byte(`{"type":"user","message":{"role":"user","content":"hi"},"sessionId":"AL"}`+"\n"), 0o600)

	t.Setenv("HOME", home)
	var out, errBuf bytes.Buffer
	rc := DispatchSessions(config.Config{}, nil, &out, &errBuf, map[string]string{},
		[]string{"report", "--session-id=AL", "--cwd=/tmp/proj", "--format=json"})
	if rc != 0 {
		t.Fatalf("rc=%d stderr=%s", rc, errBuf.String())
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}

	topHits, ok := parsed["user_prompt_cache_hits"].(float64)
	if !ok {
		t.Fatalf("user_prompt_cache_hits missing or wrong type: %T %v",
			parsed["user_prompt_cache_hits"], parsed["user_prompt_cache_hits"])
	}
	topTotal, ok := parsed["user_prompt_cache_total"].(float64)
	if !ok {
		t.Fatalf("user_prompt_cache_total missing or wrong type: %T %v",
			parsed["user_prompt_cache_total"], parsed["user_prompt_cache_total"])
	}
	contrib, ok := parsed["heimdall_contribution"].(map[string]interface{})
	if !ok {
		t.Fatalf("heimdall_contribution missing or wrong type: %T %v",
			parsed["heimdall_contribution"], parsed["heimdall_contribution"])
	}
	nestedHits, ok := contrib["cache_hits"].(float64)
	if !ok {
		t.Fatalf("heimdall_contribution.cache_hits missing or wrong type: %T %v",
			contrib["cache_hits"], contrib["cache_hits"])
	}

	// Invariant: the deprecated alias mirrors the canonical top-level field.
	if int(nestedHits) != int(topHits) {
		t.Errorf("alias mismatch: heimdall_contribution.cache_hits=%v, user_prompt_cache_hits=%v; these must be equal (back-compat alias)",
			nestedHits, topHits)
	}
	// Sanity check that we actually exercised a non-trivial session.
	if int(topHits) != 2 {
		t.Errorf("user_prompt_cache_hits: got %v want 2", topHits)
	}
	if int(topTotal) != 5 {
		t.Errorf("user_prompt_cache_total: got %v want 5 (3 ok + 2 cache_hit; skip and ollama_ping excluded)", topTotal)
	}
}

// TestSessionsReport_SemanticDriftFailOpen asserts the JSON output always
// carries a `tool_use.semantic_drift` key, even when the compute path
// can't run (e.g. no Ollama, no store). On a fresh tempdir there is no
// .heimdall_db, so ResolveUsableModelDB returns ("","") and we degrade
// to diag="no_usable_index" with semantic_drift: null — the expected
// fail-open shape per plan 12 §7.
func TestSessionsReport_SemanticDriftFailOpen(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "hooks.log")
	t.Setenv("HEIMDALL_HOOK_LOG", logPath)
	t.Setenv("HEIMDALL_HOOKS", "1")
	_ = os.WriteFile(logPath, []byte("2026-04-16T20:00:00Z INFO event=user-prompt session=SD stage=ok\n"), 0o600)

	home := filepath.Join(tmp, "home")
	slugDir := filepath.Join(home, ".claude", "projects", "-sdproj")
	_ = os.MkdirAll(slugDir, 0o755)
	transcriptPath := filepath.Join(slugDir, "SD.jsonl")
	tdata := `{"type":"user","message":{"role":"user","content":"hi"},"sessionId":"SD","timestamp":"2026-04-16T20:00:01Z"}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}},"sessionId":"SD","timestamp":"2026-04-16T20:00:02Z"}` + "\n"
	_ = os.WriteFile(transcriptPath, []byte(tdata), 0o600)

	t.Setenv("HOME", home)
	var out, errBuf bytes.Buffer
	rc := DispatchSessions(config.Config{}, nil, &out, &errBuf, map[string]string{},
		[]string{"report", "--session-id=SD", "--cwd=/sdproj", "--format=json"})
	if rc != 0 {
		t.Fatalf("rc: %d stderr: %s", rc, errBuf.String())
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("invalid json: %v\nraw: %s", err, out.String())
	}
	tu, ok := parsed["tool_use"].(map[string]interface{})
	if !ok {
		t.Fatalf("no tool_use object: %s", out.String())
	}
	// semantic_drift key is always present — value may be nil (fail-open).
	if _, exists := tu["semantic_drift"]; !exists {
		t.Errorf("tool_use.semantic_drift key missing")
	}
}

// TestSessionsReport_SemanticDriftTextLine asserts the text renderer
// always emits a 2-line semantic_drift block under `## Tool use`, even
// on the fail-open path.
func TestSessionsReport_SemanticDriftTextLine(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "hooks.log")
	t.Setenv("HEIMDALL_HOOK_LOG", logPath)
	_ = os.WriteFile(logPath, []byte("2026-04-16T20:00:00Z INFO event=user-prompt session=SDT stage=ok\n"), 0o600)

	home := filepath.Join(tmp, "home")
	slugDir := filepath.Join(home, ".claude", "projects", "-sdtproj")
	_ = os.MkdirAll(slugDir, 0o755)
	transcriptPath := filepath.Join(slugDir, "SDT.jsonl")
	tdata := `{"type":"user","message":{"role":"user","content":"hi"},"sessionId":"SDT","timestamp":"2026-04-16T20:00:01Z"}` + "\n"
	_ = os.WriteFile(transcriptPath, []byte(tdata), 0o600)

	t.Setenv("HOME", home)
	var out, errBuf bytes.Buffer
	rc := DispatchSessions(config.Config{}, nil, &out, &errBuf, map[string]string{},
		[]string{"report", "--session-id=SDT", "--cwd=/sdtproj", "--format=text"})
	if rc != 0 {
		t.Fatalf("rc: %d stderr: %s", rc, errBuf.String())
	}
	if !strings.Contains(out.String(), "semantic_drift:") {
		t.Errorf("expected semantic_drift line, got:\n%s", out.String())
	}
}

// TestSessionsReport_VerboseFlagAccepted guards the --verbose flag plumbing
// (OQ-10). We don't exercise per-turn rows here (that requires a live
// compute path); we just confirm the flag parses cleanly and doesn't break
// the report.
func TestSessionsReport_VerboseFlagAccepted(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "hooks.log")
	t.Setenv("HEIMDALL_HOOK_LOG", logPath)
	_ = os.WriteFile(logPath, []byte("2026-04-16T20:00:00Z INFO event=user-prompt session=V stage=ok\n"), 0o600)

	home := filepath.Join(tmp, "home")
	slugDir := filepath.Join(home, ".claude", "projects", "-vproj")
	_ = os.MkdirAll(slugDir, 0o755)
	transcriptPath := filepath.Join(slugDir, "V.jsonl")
	tdata := `{"type":"user","message":{"role":"user","content":"hi"},"sessionId":"V","timestamp":"2026-04-16T20:00:01Z"}` + "\n"
	_ = os.WriteFile(transcriptPath, []byte(tdata), 0o600)

	t.Setenv("HOME", home)
	var out, errBuf bytes.Buffer
	rc := DispatchSessions(config.Config{}, nil, &out, &errBuf, map[string]string{},
		[]string{"report", "--session-id=V", "--cwd=/vproj", "--format=text", "--verbose"})
	if rc != 0 {
		t.Fatalf("rc: %d stderr: %s", rc, errBuf.String())
	}
}

func TestSessionsReport_NeitherFlagFails(t *testing.T) {
	var out, errBuf bytes.Buffer
	rc := DispatchSessions(config.Config{}, nil, &out, &errBuf, map[string]string{},
		[]string{"report"})
	if rc != 2 {
		t.Fatalf("expected rc=2, got %d", rc)
	}
	if !strings.Contains(errBuf.String(), "--session-id or --current is required") {
		t.Errorf("expected required-flag error, got: %s", errBuf.String())
	}
}
