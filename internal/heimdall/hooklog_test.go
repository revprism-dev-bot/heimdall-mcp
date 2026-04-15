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

func TestLogHookEvent_RotationAt5MB(t *testing.T) {
	path := setupTmpHookLog(t)
	// Pre-fill the log to just under 5 MB so a single subsequent write
	// triggers rotation.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	initial := bytes.Repeat([]byte("x"), hookLogMaxBytes-50)
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
	if len(rb) < hookLogMaxBytes-100 {
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
	if err := os.WriteFile(path, bytes.Repeat([]byte("y"), hookLogMaxBytes-10), 0o600); err != nil {
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

func TestReadHookLog_OneShot(t *testing.T) {
	path := setupTmpHookLog(t)
	LogHookEvent("INFO", "e1", nil)
	LogHookEvent("INFO", "e2", nil)

	rc, err := ReadHookLog(0, false)
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

func TestReadHookLog_MissingFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HEIMDALL_HOOK_LOG", filepath.Join(dir, "does-not-exist.log"))
	_, err := ReadHookLog(0, false)
	if err == nil {
		t.Error("expected error on missing log")
	}
}
