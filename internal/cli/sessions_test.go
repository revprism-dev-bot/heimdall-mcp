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
