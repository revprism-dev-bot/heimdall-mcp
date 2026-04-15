package heimdall

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// hook log file constants.
const (
	hookLogMaxBytes = 5 * 1024 * 1024 // 5 MB size cap per §5.8
	hookLogFileMode = 0o600           // user-only; never leak to other users
	hookLogDirMode  = 0o755
)

// hookLogMu serializes concurrent writers. A single mutex for the whole process
// is fine: the hot path writes one short line, holds the lock for microseconds,
// and callers are rate-limited by the hook cadence anyway.
var hookLogMu sync.Mutex

// HookLogPath returns the resolved absolute path to the hook log file, honoring
// XDG_STATE_HOME and falling back to ~/.local/state/heimdall/hooks.log. Exported
// for use by `hooks doctor` and tests. Returns empty string if no home can be
// resolved (in which case LogHookEvent silently no-ops — never an error).
func HookLogPath() string {
	return hookLogPathFromEnv(osEnv, osUserHome)
}

// hookLogPathFromEnv is the pure, testable core.
func hookLogPathFromEnv(getenv func(string) string, userHome func() (string, error)) string {
	if override := getenv("HEIMDALL_HOOK_LOG"); override != "" {
		return override
	}
	if xdg := getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "heimdall", "hooks.log")
	}
	home, err := userHome()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "state", "heimdall", "hooks.log")
}

func osEnv(k string) string                { return os.Getenv(k) }
func osUserHome() (string, error)          { return os.UserHomeDir() }

// LogHookEvent writes one line to the hook log.
//
// Never panics. Never returns an error. If anything goes wrong (dir create,
// open, write, rotate), the line is silently dropped — a logging failure MUST
// NEVER abort a hook (§5.9 golden rule).
//
// Format (grep-friendly):
//
//	2026-04-14T15:04:33Z LEVEL event=<name> project=<redacted> k=v ...
//
// Values are redacted: any absolute path (/home/...) is replaced with
// `<redacted>`, any token-looking string is preserved as key=value but the
// caller is expected not to put secrets in kv.
func LogHookEvent(level, event string, kv map[string]any) {
	// Panic guard — the log call path is hot and any panic in formatting,
	// file I/O, or a rogue fmt verb would abort the hook.
	defer func() {
		_ = recover()
	}()

	path := HookLogPath()
	if path == "" {
		return
	}

	line := formatHookLogLine(time.Now().UTC(), level, event, kv)

	hookLogMu.Lock()
	defer hookLogMu.Unlock()

	if err := os.MkdirAll(filepath.Dir(path), hookLogDirMode); err != nil {
		return
	}

	// Check-on-write rotation: if current file size plus this line would
	// exceed the cap, rotate first. Stat errors (file doesn't exist) are
	// ignored — we'll create it on Open.
	if fi, err := os.Stat(path); err == nil {
		if fi.Size()+int64(len(line)) > hookLogMaxBytes {
			rotateHookLog(path)
		}
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, hookLogFileMode)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write([]byte(line))
}

// rotateHookLog atomically renames hooks.log -> hooks.log.1, overwriting any
// existing .1. Called with hookLogMu held.
func rotateHookLog(path string) {
	rotated := path + ".1"
	// os.Rename is atomic within the same directory on POSIX filesystems.
	// We explicitly don't care about the error: if rename fails we'll
	// continue appending to the oversize file, which is still strictly
	// better than dropping the line.
	_ = os.Rename(path, rotated)
}

// formatHookLogLine builds a single log line (including trailing \n).
//
// Ordering: timestamp, level, event=..., then kv pairs in sorted key order
// for reproducibility. Values are redacted and escaped.
func formatHookLogLine(ts time.Time, level, event string, kv map[string]any) string {
	var b strings.Builder
	b.Grow(128)
	b.WriteString(ts.Format(time.RFC3339))
	b.WriteByte(' ')
	b.WriteString(sanitizeLogToken(level))
	b.WriteString(" event=")
	b.WriteString(sanitizeLogToken(event))

	if len(kv) > 0 {
		keys := make([]string, 0, len(kv))
		for k := range kv {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteByte(' ')
			b.WriteString(sanitizeLogToken(k))
			b.WriteByte('=')
			b.WriteString(redactLogValue(kv[k]))
		}
	}
	b.WriteByte('\n')
	return b.String()
}

// sanitizeLogToken strips whitespace, equals signs, and control chars from a
// key/level/event token. Tokens must be one whitespace-free chunk so awk/grep
// can split on space reliably.
func sanitizeLogToken(s string) string {
	if s == "" {
		return "-"
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x20 || r == ' ' || r == '=' || r == '"' {
			b.WriteByte('_')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// redactLogValue renders a kv value for logging. Rules:
//  1. Absolute filesystem paths → `<redacted>`. Plan §5.9: the log file is
//     user data, not just user-visible text; we redact here too.
//  2. Strings containing whitespace → wrapped in quotes with inner quotes
//     escaped.
//  3. Numbers/bools → fmt.Sprint.
func redactLogValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "-"
	case string:
		return redactLogString(x)
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case time.Duration:
		return x.String()
	default:
		return redactLogString(fmt.Sprint(v))
	}
}

func redactLogString(s string) string {
	// Absolute path detection: leading `/` with at least one more segment.
	// This catches /home/user/..., /tmp/..., /var/..., /Users/..., etc.
	if strings.HasPrefix(s, "/") && len(s) > 1 && !strings.ContainsAny(s[:2], " \t") {
		// Leave one-component absolute paths (e.g. `/`) alone, redact rest.
		if strings.Contains(s[1:], "/") {
			return "<redacted>"
		}
	}
	if strings.ContainsAny(s, " \t\"\n") {
		// Quote + escape quotes and newlines.
		esc := strings.ReplaceAll(s, `"`, `\"`)
		esc = strings.ReplaceAll(esc, "\n", `\n`)
		return `"` + esc + `"`
	}
	return s
}

// ReadHookLog opens the hook log for reading. The caller MUST close the
// returned io.ReadCloser. If follow is true, the reader blocks at EOF and
// polls every 200ms for appended lines until closed. Used by `hooks tail`.
//
// One-shot (follow=false) returns a ReadCloser that yields all current
// contents and then EOF. Never panics; returns a concrete error if the log
// file does not exist or cannot be opened (callers render that error as
// "no log yet" or similar).
func ReadHookLog(offset int64, follow bool) (io.ReadCloser, error) {
	path := HookLogPath()
	if path == "" {
		return nil, fmt.Errorf("hook log path unresolved (set XDG_STATE_HOME or HOME)")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	if !follow {
		return f, nil
	}
	return &followingReader{f: f, path: path, poll: 200 * time.Millisecond}, nil
}

// followingReader is a `tail -F`-style reader: returns bytes as they're
// appended. Intentionally simple — polling, no inotify. On rotation (file
// truncation or size shrinkage) the reader reopens the file from the start.
type followingReader struct {
	f    *os.File
	path string
	poll time.Duration
	done chan struct{}
	mu   sync.Mutex
}

func (r *followingReader) Read(p []byte) (int, error) {
	for {
		r.mu.Lock()
		n, err := r.f.Read(p)
		r.mu.Unlock()
		if n > 0 {
			return n, nil
		}
		if err != nil && err != io.EOF {
			return 0, err
		}
		// EOF — wait and retry. Cancellation: if Close was called, return EOF.
		select {
		case <-r.closeChan():
			return 0, io.EOF
		case <-time.After(r.poll):
		}
		r.maybeReopen()
	}
}

func (r *followingReader) closeChan() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done == nil {
		r.done = make(chan struct{})
	}
	return r.done
}

func (r *followingReader) maybeReopen() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return
	}
	cur, err := r.f.Seek(0, io.SeekCurrent)
	if err != nil {
		return
	}
	fi, err := os.Stat(r.path)
	if err != nil {
		return
	}
	if fi.Size() < cur {
		// File rotated/truncated — reopen.
		_ = r.f.Close()
		nf, err := os.Open(r.path)
		if err == nil {
			r.f = nf
		}
	}
}

func (r *followingReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done != nil {
		select {
		case <-r.done:
		default:
			close(r.done)
		}
	}
	if r.f != nil {
		err := r.f.Close()
		r.f = nil
		return err
	}
	return nil
}
