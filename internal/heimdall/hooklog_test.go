package heimdall

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// setupTmpHookLog points the hook log at a fresh temp dir for the duration
// of the test, via HEIMDALL_HOOK_LOG (honored by hookLogPathFromEnv).
func setupTmpHookLog(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.log")
	t.Setenv("HEIMDALL_HOOK_LOG", path)
	return path
}

func TestLogHookEvent_WritesOneLine(t *testing.T) {
	path := setupTmpHookLog(t)

	LogHookEvent("INFO", "user-prompt", map[string]any{
		"cached":     1,
		"latency_ms": 38,
		"project":    "payments-analyzer",
	})

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	line := string(b)
	if !strings.HasSuffix(line, "\n") {
		t.Fatalf("expected trailing newline, got %q", line)
	}
	if strings.Count(line, "\n") != 1 {
		t.Fatalf("expected exactly one line, got %q", line)
	}
	if !strings.Contains(line, "INFO") {
		t.Errorf("line missing level: %q", line)
	}
	if !strings.Contains(line, "event=user-prompt") {
		t.Errorf("line missing event: %q", line)
	}
	if !strings.Contains(line, "cached=1") {
		t.Errorf("line missing cached kv: %q", line)
	}
	if !strings.Contains(line, "latency_ms=38") {
		t.Errorf("line missing latency kv: %q", line)
	}
	if !strings.Contains(line, "project=payments-analyzer") {
		t.Errorf("line missing project kv: %q", line)
	}
	// Verify ISO-8601 UTC — format.RFC3339 ends in "Z" for UTC.
	if !strings.Contains(line, "Z ") {
		t.Errorf("line missing UTC Z timestamp marker: %q", line)
	}
}

func TestLogHookEvent_KeysAreSorted(t *testing.T) {
	path := setupTmpHookLog(t)
	LogHookEvent("INFO", "e", map[string]any{
		"zebra": 1, "alpha": 2, "mike": 3,
	})
	b, _ := os.ReadFile(path)
	s := string(b)
	ai := strings.Index(s, "alpha=")
	mi := strings.Index(s, "mike=")
	zi := strings.Index(s, "zebra=")
	if !(ai > 0 && ai < mi && mi < zi) {
		t.Errorf("keys not sorted: %q", s)
	}
}

func TestLogHookEvent_Redaction(t *testing.T) {
	path := setupTmpHookLog(t)

	LogHookEvent("WARN", "post-edit", map[string]any{
		"file":   "/home/alice/secret/project/main.go",
		"tmp":    "/tmp/x/y",
		"safe":   "hello",
		"spaces": "has space",
	})

	b, _ := os.ReadFile(path)
	s := string(b)
	if strings.Contains(s, "/home/alice") {
		t.Errorf("absolute path leaked: %q", s)
	}
	if strings.Contains(s, "/tmp/x/y") {
		t.Errorf("tmp path leaked: %q", s)
	}
	if !strings.Contains(s, "file=<redacted>") {
		t.Errorf("file not redacted: %q", s)
	}
	if !strings.Contains(s, "tmp=<redacted>") {
		t.Errorf("tmp not redacted: %q", s)
	}
	if !strings.Contains(s, "safe=hello") {
		t.Errorf("safe value mangled: %q", s)
	}
	if !strings.Contains(s, `spaces="has space"`) {
		t.Errorf("spaces value not quoted: %q", s)
	}
}

// SEC-008: Windows-style absolute paths (drive-letter and UNC) must redact.
// Covers both `\` and `/` separators, upper/lower case drive letters, UNC
// prefixes, plus negative cases for relative paths and empty strings.
func TestRedactLogString_WindowsPaths(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// Positive — drive-letter absolute, backslash
		{`C:\Users\alice\secret.key`, "<redacted>"},
		{`D:\Projects\code.go`, "<redacted>"},
		{`c:\lower\drive.txt`, "<redacted>"},
		// Positive — drive-letter absolute, forward slash
		{`C:/Users/alice/forward.key`, "<redacted>"},
		{`d:/projects/x.go`, "<redacted>"},
		// Positive — UNC
		{`\\server\share\file`, "<redacted>"},
		{`\\SERVER\share\file`, "<redacted>"},
		{`\\fileserver\share\data\nested\thing.bin`, "<redacted>"},
		// Negative — relative Windows-style (no drive prefix, no UNC)
		{`foo\bar`, `foo\bar`},
		// Negative — empty
		{"", ""},
		// Negative — drive letter but not absolute (no slash after colon)
		{`C:notabsolute`, `C:notabsolute`},
		// Negative — single leading backslash is NOT UNC
		{`\single\back`, `\single\back`},
	}
	for _, tc := range cases {
		got := redactLogString(tc.in)
		if got != tc.want {
			t.Errorf("redactLogString(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Keep POSIX redaction behavior unchanged.
func TestRedactLogString_PosixStillRedacts(t *testing.T) {
	for _, in := range []string{
		"/home/alice/secret.key",
		"/tmp/x/y",
		"/var/log/foo",
		"/Users/bob/Documents/a.txt",
	} {
		if got := redactLogString(in); got != "<redacted>" {
			t.Errorf("POSIX redaction regressed for %q: got %q", in, got)
		}
	}
	// `/` alone is not redacted (one-component absolute path).
	if got := redactLogString("/"); got != "/" {
		t.Errorf("single-slash should not redact: got %q", got)
	}
}

// QUAL-001: end-to-end rotation against a real (tiny) threshold. Uses
// SetHookLogMaxBytesForTest to avoid writing 5 MB per test run.
func TestLogRotationAtRealBoundary(t *testing.T) {
	path := setupTmpHookLog(t)
	restore := SetHookLogMaxBytesForTest(4096)
	defer restore()

	// Write enough entries to exceed 4 KB. Each line is well under 200 B so
	// ~50 lines is comfortably above the threshold.
	for i := 0; i < 60; i++ {
		LogHookEvent("INFO", "boundary-test", map[string]any{
			"iter": i,
			"pad":  strings.Repeat("q", 64),
		})
	}

	// hooks.log must be <= 4 KB (the live file after rotation).
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat live log: %v", err)
	}
	if fi.Size() > 4096 {
		t.Errorf("live log exceeded threshold: size=%d", fi.Size())
	}
	// hooks.log.1 must exist and hold the earlier lines.
	rotated := path + ".1"
	rb, err := os.ReadFile(rotated)
	if err != nil {
		t.Fatalf("read rotated log: %v", err)
	}
	if len(rb) == 0 {
		t.Errorf("rotated log is empty")
	}
	if !bytes.Contains(rb, []byte("event=boundary-test")) {
		t.Errorf("rotated log missing expected event lines")
	}
	// No third generation.
	if _, err := os.Stat(path + ".2"); err == nil {
		t.Errorf("unexpected hooks.log.2 was created")
	}

	// Subsequent write reopens hooks.log cleanly.
	LogHookEvent("INFO", "post-rotation", map[string]any{"k": "v"})
	fi2, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat live log after append: %v", err)
	}
	if fi2.Size() < fi.Size() {
		t.Errorf("append did not grow the live log: before=%d after=%d", fi.Size(), fi2.Size())
	}
	finalBytes, _ := os.ReadFile(path)
	if !bytes.Contains(finalBytes, []byte("event=post-rotation")) {
		t.Errorf("post-rotation append missing: %q", finalBytes)
	}
}

// TEST-C-001: external deletion of the log file mid-stream must not panic.
// Documents observed behavior: os.OpenFile with O_CREATE recreates the file,
// so the next write succeeds and lands in a fresh hooks.log.
func TestLogHookEvent_FileDeletedMidStream(t *testing.T) {
	path := setupTmpHookLog(t)

	LogHookEvent("INFO", "before-delete", map[string]any{"k": 1})
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("log missing after first write: %v", err)
	}

	// Delete the file while holding no lock — simulates a janitor script or
	// user running `rm` mid-session.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}

	// Must not panic.
	LogHookEvent("INFO", "after-delete", map[string]any{"k": 2})

	// Current behavior: the file is recreated by O_CREATE on the next
	// write. Assert that (preferred branch of §5.9).
	b, err := os.ReadFile(path)
	if err != nil {
		// Acceptable fallback: silently dropped, no recreate. In that case
		// there must still be no panic and no file — not an error.
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("read after recreate: %v", err)
	}
	if !bytes.Contains(b, []byte("event=after-delete")) {
		t.Errorf("recreated log missing post-delete line: %q", b)
	}
	if bytes.Contains(b, []byte("event=before-delete")) {
		t.Errorf("recreated log unexpectedly contains pre-delete line: %q", b)
	}
}

// TEST-C-002: OpenHookLog must tolerate a malformed trailing line (no
// terminating newline, or garbled UTF-8) without panic.
func TestOpenHookLog_HandlesPartialLastLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.log")
	t.Setenv("HEIMDALL_HOOK_LOG", path)

	// Two valid lines plus a malformed trailing line (no final newline, with
	// a raw invalid UTF-8 byte sequence thrown in for good measure).
	content := []byte(
		"2026-04-14T10:00:00Z INFO event=one\n" +
			"2026-04-14T10:00:01Z INFO event=two\n" +
			"2026-04-14T10:00:02Z INFO event=partial \xff\xfe partial-no-newline",
	)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}

	rc, err := OpenHookLog(0, false)
	if err != nil {
		t.Fatalf("OpenHookLog: %v", err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read all: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, "event=one") || !strings.Contains(s, "event=two") {
		t.Errorf("lost valid lines: %q", s)
	}

	// Subsequent reads still work (reopen).
	rc2, err := OpenHookLog(0, false)
	if err != nil {
		t.Fatalf("second OpenHookLog: %v", err)
	}
	defer rc2.Close()
	if _, err := io.ReadAll(rc2); err != nil {
		t.Fatalf("second read: %v", err)
	}
}

func TestLogHookEvent_RotationAt5MB(t *testing.T) {
	path := setupTmpHookLog(t)
	// Pre-fill the log to just under 5 MB so a single subsequent write
	// triggers rotation.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	initial := bytes.Repeat([]byte("x"), int(hookLogMaxBytes)-50)
	if err := os.WriteFile(path, initial, 0o600); err != nil {
		t.Fatal(err)
	}

	// This one write should push past 5 MB and trigger rotation.
	LogHookEvent("INFO", "rotate-trigger", map[string]any{
		"k": strings.Repeat("z", 200), // ensures overflow
	})

	rotated := path + ".1"
	if _, err := os.Stat(rotated); err != nil {
		t.Fatalf("expected rotated file at %s: %v", rotated, err)
	}

	// Fresh file should be small: only the new line.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("fresh log missing: %v", err)
	}
	if fi.Size() > 1024 {
		t.Errorf("fresh log not truncated on rotation: size=%d", fi.Size())
	}
	// Rotated file should hold the original pre-filled bytes.
	rb, _ := os.ReadFile(rotated)
	if int64(len(rb)) < hookLogMaxBytes-100 {
		t.Errorf("rotated file unexpectedly small: %d", len(rb))
	}
}

func TestLogHookEvent_RotationIdempotent(t *testing.T) {
	path := setupTmpHookLog(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-create an existing .1 — rotation must overwrite it, not fail.
	if err := os.WriteFile(path+".1", []byte("previous rotation"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("y"), int(hookLogMaxBytes)-10), 0o600); err != nil {
		t.Fatal(err)
	}

	LogHookEvent("INFO", "rotate", map[string]any{"k": strings.Repeat("q", 100)})

	// Old .1 must be replaced with the previous contents, not the string
	// "previous rotation".
	rb, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(rb, []byte("previous rotation")) {
		t.Errorf("rotation did not overwrite existing .1")
	}
}

func TestLogHookEvent_ConcurrentWritesSerialize(t *testing.T) {
	path := setupTmpHookLog(t)

	const (
		workers = 16
		perW    = 50
	)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < perW; j++ {
				LogHookEvent("INFO", "concurrent", map[string]any{
					"worker": id,
					"iter":   j,
				})
			}
		}(i)
	}
	wg.Wait()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimRight(b, "\n"), []byte("\n"))
	if got, want := len(lines), workers*perW; got != want {
		t.Errorf("line count: got %d want %d", got, want)
	}
	// Every line must start with an RFC3339 timestamp and contain
	// `event=concurrent` — proves no interleaving mid-line.
	for i, ln := range lines {
		if !bytes.Contains(ln, []byte("event=concurrent")) {
			t.Errorf("line %d not intact: %q", i, ln)
			break
		}
		if len(ln) < 20 || ln[10] != 'T' {
			t.Errorf("line %d has bad timestamp: %q", i, ln)
			break
		}
	}
}

func TestLogHookEvent_DirCreateFailureSilentlyDrops(t *testing.T) {
	// Point the log into a non-creatable path — we set parent to a file,
	// so MkdirAll can't create it underneath.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HEIMDALL_HOOK_LOG", filepath.Join(blocker, "sub", "hooks.log"))

	// Must not panic and must not error.
	done := make(chan struct{})
	go func() {
		defer close(done)
		LogHookEvent("INFO", "should-drop", map[string]any{"k": "v"})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("LogHookEvent hung")
	}
}

func TestLogHookEvent_NoPanicOnWeirdInputs(t *testing.T) {
	setupTmpHookLog(t)
	// nil map, empty strings, weird types — must not crash.
	LogHookEvent("", "", nil)
	LogHookEvent("INFO", "event with spaces=and=equals", map[string]any{
		"k with space": nil,
		"chan":         make(chan int),
	})
}

// TestRedactLogString_QuotingProtocol pins the writer-side quoting rules that
// the reader's strconv.Unquote pairing expects. The contract:
//
//  1. Simple values (no whitespace, no quote, no control) round-trip UNCHANGED
//     — this is load-bearing for every downstream grep-style consumer
//     (awk, grep, `hooks tail --project=...`).
//  2. Values containing whitespace → wrapped in double quotes using
//     strconv.Quote semantics so the reader can use strconv.Unquote for a
//     lossless round-trip.
//  3. Values containing a literal " → Go-escaped inside the quoted form.
//  4. Values containing control chars (tab, newline) → Go-escaped.
func TestRedactLogString_QuotingProtocol(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain word", "hello", "hello"},
		{"empty string leaves empty", "", ""},
		{"number-like token", "12345", "12345"},
		{"simple with dash and underscore", "rule_id-x", "rule_id-x"},
		{"space -> quoted", "rm -rf at root", `"rm -rf at root"`},
		{"embedded quote -> escaped", `she said "hi"`, `"she said \"hi\""`},
		{"tab -> escaped", "a\tb c", `"a\tb c"`},
		{"newline -> escaped", "line1\nline2", `"line1\nline2"`},
		{"backslash with space -> escaped", `a \ b`, `"a \\ b"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactLogString(tc.in)
			if got != tc.want {
				t.Errorf("redactLogString(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestLogHookEvent_SimpleValueUnquoted is the backwards-compat canary —
// any simple kv must emit as literal `k=v` with no quoting. Existing tools
// that grep for `project=xyz` or `stage=ok` patterns depend on this.
func TestLogHookEvent_SimpleValueUnquoted(t *testing.T) {
	path := setupTmpHookLog(t)
	LogHookEvent("INFO", "user-prompt", map[string]any{
		"project": "heimdall",
		"stage":   "ok",
		"bytes":   1234,
	})
	b, _ := os.ReadFile(path)
	s := string(b)
	for _, needle := range []string{"project=heimdall", "stage=ok", "bytes=1234"} {
		if !strings.Contains(s, needle) {
			t.Errorf("simple value lost its plain form: need %q in %q", needle, s)
		}
	}
	// Must NOT have wrapped any of these in quotes.
	for _, forbidden := range []string{`project="heimdall"`, `stage="ok"`, `bytes="1234"`} {
		if strings.Contains(s, forbidden) {
			t.Errorf("simple value got spuriously quoted: found %q in %q", forbidden, s)
		}
	}
}

// TestLogHookEvent_ValueWithSpaceQuoted covers the core motivating bug:
// `reason="rm -rf at root"` must round-trip cleanly for `hooks audit-guardrails`.
func TestLogHookEvent_ValueWithSpaceQuoted(t *testing.T) {
	path := setupTmpHookLog(t)
	LogHookEvent("WARN", "pre-tool-use", map[string]any{
		"class":   "block",
		"rule_id": "RM_RF_ROOT",
		"reason":  "rm -rf at root",
	})
	b, _ := os.ReadFile(path)
	s := string(b)
	if !strings.Contains(s, `reason="rm -rf at root"`) {
		t.Errorf("reason with spaces not quoted as expected; got:\n%s", s)
	}
}

// TestLogHookEvent_ValueWithEmbeddedQuoteEscaped — values holding a literal
// double quote must be escaped via strconv.Quote so the reader can unquote
// them back to the original.
func TestLogHookEvent_ValueWithEmbeddedQuoteEscaped(t *testing.T) {
	path := setupTmpHookLog(t)
	LogHookEvent("INFO", "test", map[string]any{
		"note": `she said "hi" loudly`,
	})
	b, _ := os.ReadFile(path)
	s := string(b)
	if !strings.Contains(s, `note="she said \"hi\" loudly"`) {
		t.Errorf("embedded quote not escaped as expected; got:\n%s", s)
	}
}

func TestHookLogPath_EnvOverride(t *testing.T) {
	p := hookLogPathFromEnv(
		func(k string) string {
			if k == "HEIMDALL_HOOK_LOG" {
				return "/custom/path.log"
			}
			return ""
		},
		func() (string, error) { return "/home/x", nil },
	)
	if p != "/custom/path.log" {
		t.Errorf("override not honored: %q", p)
	}
}

func TestHookLogPath_XDGStateHome(t *testing.T) {
	p := hookLogPathFromEnv(
		func(k string) string {
			if k == "XDG_STATE_HOME" {
				return "/state"
			}
			return ""
		},
		func() (string, error) { return "/home/x", nil },
	)
	if p != "/state/heimdall/hooks.log" {
		t.Errorf("xdg path wrong: %q", p)
	}
}

func TestHookLogPath_HomeFallback(t *testing.T) {
	p := hookLogPathFromEnv(
		func(string) string { return "" },
		func() (string, error) { return "/home/bob", nil },
	)
	want := "/home/bob/.local/state/heimdall/hooks.log"
	if p != want {
		t.Errorf("home fallback wrong: %q want %q", p, want)
	}
}

func TestHookLogPath_NoHomeReturnsEmpty(t *testing.T) {
	p := hookLogPathFromEnv(
		func(string) string { return "" },
		func() (string, error) { return "", os.ErrNotExist },
	)
	if p != "" {
		t.Errorf("expected empty on no-home, got %q", p)
	}
}

func TestOpenHookLog_OneShot(t *testing.T) {
	path := setupTmpHookLog(t)
	LogHookEvent("INFO", "e1", nil)
	LogHookEvent("INFO", "e2", nil)

	rc, err := OpenHookLog(0, false)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "event=e1") || !strings.Contains(s, "event=e2") {
		t.Errorf("missing events: %q", s)
	}
	_ = path
}

func TestOpenHookLog_MissingFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HEIMDALL_HOOK_LOG", filepath.Join(dir, "does-not-exist.log"))
	_, err := OpenHookLog(0, false)
	if err == nil {
		t.Error("expected error on missing log")
	}
}
