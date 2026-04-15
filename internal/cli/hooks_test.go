package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/config"
)

// seedHookLog writes a set of pre-canned log lines to a temp path pointed at
// by HEIMDALL_HOOK_LOG so HooksTail can consume it.
func seedHookLog(t *testing.T, lines []string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.log")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HEIMDALL_HOOK_LOG", path)
}

func runTail(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	code = HooksTail(nil, &out, &errb, nil, args)
	return out.String(), errb.String(), code
}

func TestHooksTail_OneShotAll(t *testing.T) {
	seedHookLog(t, []string{
		"2026-04-14T10:00:00Z INFO event=user-prompt project=alpha cached=1",
		"2026-04-14T10:00:01Z WARN event=post-edit project=beta err=ollama_down",
	})
	out, errb, code := runTail(t)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errb)
	}
	if !strings.Contains(out, "user-prompt") || !strings.Contains(out, "post-edit") {
		t.Errorf("expected both events: %q", out)
	}
}

func TestHooksTail_FilterByLevel(t *testing.T) {
	seedHookLog(t, []string{
		"2026-04-14T10:00:00Z INFO event=a",
		"2026-04-14T10:00:01Z WARN event=b",
		"2026-04-14T10:00:02Z ERROR event=c",
	})
	out, _, code := runTail(t, "--level", "warn")
	if code != 0 {
		t.Fatal(code)
	}
	if strings.Contains(out, "event=a") || strings.Contains(out, "event=c") {
		t.Errorf("level filter leaked: %q", out)
	}
	if !strings.Contains(out, "event=b") {
		t.Errorf("level filter dropped match: %q", out)
	}
}

func TestHooksTail_FilterByEvent(t *testing.T) {
	seedHookLog(t, []string{
		"2026-04-14T10:00:00Z INFO event=user-prompt project=x",
		"2026-04-14T10:00:01Z INFO event=session-start project=x",
	})
	out, _, code := runTail(t, "--event", "user-prompt")
	if code != 0 {
		t.Fatal(code)
	}
	if strings.Contains(out, "session-start") {
		t.Errorf("event filter leaked: %q", out)
	}
	if !strings.Contains(out, "user-prompt") {
		t.Errorf("event filter dropped match: %q", out)
	}
}

func TestHooksTail_FilterByProject(t *testing.T) {
	seedHookLog(t, []string{
		"2026-04-14T10:00:00Z INFO event=user-prompt project=alpha",
		"2026-04-14T10:00:01Z INFO event=user-prompt project=beta",
	})
	out, _, code := runTail(t, "--project", "beta")
	if code != 0 {
		t.Fatal(code)
	}
	if strings.Contains(out, "project=alpha") {
		t.Errorf("project filter leaked: %q", out)
	}
	if !strings.Contains(out, "project=beta") {
		t.Errorf("project filter dropped match: %q", out)
	}
}

func TestHooksTail_FilterBySince(t *testing.T) {
	// Two lines: one 2 hours ago, one 10 seconds ago.
	old := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	recent := time.Now().Add(-10 * time.Second).UTC().Format(time.RFC3339)
	seedHookLog(t, []string{
		fmt.Sprintf("%s INFO event=old project=p", old),
		fmt.Sprintf("%s INFO event=recent project=p", recent),
	})
	out, _, code := runTail(t, "--since", "5m")
	if code != 0 {
		t.Fatal(code)
	}
	if strings.Contains(out, "event=old") {
		t.Errorf("since filter leaked: %q", out)
	}
	if !strings.Contains(out, "event=recent") {
		t.Errorf("since filter dropped match: %q", out)
	}
}

func TestHooksTail_SinceAcceptsDayShorthand(t *testing.T) {
	seedHookLog(t, []string{
		"2020-01-01T00:00:00Z INFO event=ancient project=p",
	})
	out, _, code := runTail(t, "--since", "7d")
	if code != 0 {
		t.Fatal(code)
	}
	if out != "" {
		t.Errorf("expected no output for 7d with 2020 line: %q", out)
	}
}

func TestHooksTail_BadFlagReturns2(t *testing.T) {
	seedHookLog(t, []string{"2026-04-14T10:00:00Z INFO event=a"})
	_, errb, code := runTail(t, "--no-such-flag")
	if code != 2 {
		t.Errorf("expected exit 2, got %d", code)
	}
	if errb == "" {
		t.Error("expected stderr explanation")
	}
}

func TestHooksTail_BadSinceReturns2(t *testing.T) {
	seedHookLog(t, []string{"2026-04-14T10:00:00Z INFO event=a"})
	_, _, code := runTail(t, "--since", "not-a-duration")
	if code != 2 {
		t.Errorf("expected exit 2, got %d", code)
	}
}

// TEST-C-004: malformed --since values must produce a clean usage-class exit
// (2), a short human-readable stderr message, and NO stdout output. We must
// not leak internal wrapped errors or Go stack traces to the user.
func TestHooksTail_MalformedSince(t *testing.T) {
	seedHookLog(t, []string{"2026-04-14T10:00:00Z INFO event=a"})
	cases := []string{
		"invalid-date",
		"2026-13-45", // looks like a date, not a Go duration
		"",
		"5",       // no unit
		"forever", // garbage
	}
	for _, v := range cases {
		out, errb, code := runTail(t, "--since", v)
		if code != 2 {
			t.Errorf("--since %q: exit=%d want 2", v, code)
		}
		if out != "" {
			t.Errorf("--since %q: stdout not empty: %q", v, out)
		}
		if errb == "" {
			t.Errorf("--since %q: stderr empty", v)
		}
		// No Go runtime stack trace / panic artifacts leaked.
		if strings.Contains(errb, "goroutine ") || strings.Contains(errb, "runtime.") {
			t.Errorf("--since %q: stderr leaks runtime details: %q", v, errb)
		}
		// Message should mention --since for the user to self-correct.
		if !strings.Contains(errb, "--since") {
			t.Errorf("--since %q: stderr missing context: %q", v, errb)
		}
	}
}

func TestHooksTail_BadLevelReturns2(t *testing.T) {
	seedHookLog(t, []string{"2026-04-14T10:00:00Z INFO event=a"})
	_, _, code := runTail(t, "--level", "TRACE")
	if code != 2 {
		t.Errorf("expected exit 2, got %d", code)
	}
}

func TestHooksTail_EmptyLogReturns0(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HEIMDALL_HOOK_LOG", filepath.Join(dir, "hooks.log"))
	out, _, code := runTail(t)
	if code != 0 {
		t.Errorf("empty log should be exit 0, got %d", code)
	}
	if out != "" {
		t.Errorf("empty log should produce no output: %q", out)
	}
}

// ---- cache-clear / cache-stats ----

func TestHooksCacheClear_NoDB(t *testing.T) {
	dir := t.TempDir()
	var out, errb bytes.Buffer
	code := HooksCacheClear(config.Config{}, nil, &out, &errb, nil, []string{"--project", dir})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errb.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("invalid JSON: %v (%q)", err, out.String())
	}
	if _, ok := payload["cleared"]; !ok {
		t.Errorf("missing cleared key: %v", payload)
	}
}

func TestHooksCacheClear_BadFlagReturns2(t *testing.T) {
	var out, errb bytes.Buffer
	code := HooksCacheClear(config.Config{}, nil, &out, &errb, nil, []string{"--nope"})
	if code != 2 {
		t.Errorf("exit=%d", code)
	}
}

func TestHooksCacheStats_TextFormat(t *testing.T) {
	dir := t.TempDir()
	var out, errb bytes.Buffer
	code := HooksCacheStats(config.Config{}, nil, &out, &errb, nil, []string{"--project", dir})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "entries") || !strings.Contains(s, "bytes") || !strings.Contains(s, "hits") {
		t.Errorf("text format missing fields: %q", s)
	}
}

func TestHooksCacheStats_JSONFormat(t *testing.T) {
	dir := t.TempDir()
	var out, errb bytes.Buffer
	code := HooksCacheStats(config.Config{}, nil, &out, &errb, nil, []string{"--project", dir, "--format", "json"})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errb.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("invalid JSON: %v (%q)", err, out.String())
	}
	for _, k := range []string{"entries", "bytes", "hit_count"} {
		if _, ok := payload[k]; !ok {
			t.Errorf("missing %s: %v", k, payload)
		}
	}
}

func TestHooksCacheStats_BadFormat(t *testing.T) {
	var out, errb bytes.Buffer
	code := HooksCacheStats(config.Config{}, nil, &out, &errb, nil, []string{"--format", "xml"})
	if code != 2 {
		t.Errorf("exit=%d", code)
	}
}

// ---- DispatchHooks routing ----

func TestDispatchHooks_NoArgs(t *testing.T) {
	var out, errb bytes.Buffer
	code := DispatchHooks(config.Config{}, nil, &out, &errb, nil, nil)
	if code != 2 {
		t.Errorf("exit=%d", code)
	}
	if errb.String() == "" {
		t.Error("no usage on stderr")
	}
}

func TestDispatchHooks_Unknown(t *testing.T) {
	var out, errb bytes.Buffer
	code := DispatchHooks(config.Config{}, nil, &out, &errb, nil, []string{"purple"})
	if code != 2 {
		t.Errorf("exit=%d", code)
	}
}

func TestDispatchHooks_Help(t *testing.T) {
	var out, errb bytes.Buffer
	code := DispatchHooks(config.Config{}, nil, &out, &errb, nil, []string{"help"})
	if code != 0 {
		t.Errorf("exit=%d", code)
	}
	if !strings.Contains(out.String(), "hooks tail") {
		t.Errorf("help missing content: %q", out.String())
	}
}
