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
