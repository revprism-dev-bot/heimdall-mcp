package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// Debouncer defaults per consolidated-plan §5.5 / §3.3.
const (
	postEditCoalesceWindow = 2 * time.Second
	postEditPendingCap     = 1000
	postEditStaleLockTTL   = 5 * time.Minute
	postEditDeadletterCap  = 10
)

// PostEditDeps carries injected dependencies so tests can exercise the
// foreground path without spawning real subprocesses. Production wiring fills
// these with the real OS calls; tests swap in fakes.
type PostEditDeps struct {
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time
	// Spawn hands off to the detached actor. In production this does
	// fork+setsid via os/exec. Returning nil signals "actor dispatched
	// successfully"; the foreground path then writes inflight.pid and
	// returns. Tests pass a spy that records the call without forking.
	Spawn func(projectRoot, sessionID string) error
	// UseFlock chooses between flock(2) (true) and the O_CREAT|O_EXCL
	// TTL-stamp fallback (false). Production defaults to true; tests use
	// false to exercise the fallback path without touching OS-level locks.
	UseFlock bool
	// KillCheck reports whether the process with the given PID is alive.
	// Defaults to a syscall.Kill(pid, 0) probe. Tests inject a fake map.
	KillCheck func(pid int) bool
}

func defaultPostEditDeps() PostEditDeps {
	return PostEditDeps{
		Now:       time.Now,
		Spawn:     spawnPostEditActor,
		UseFlock:  true,
		KillCheck: killCheck,
	}
}

// HookPostEdit is the foreground handler for `heimdall-mcp hook post-edit`.
//
// Contract (consolidated-plan §3.3, Wave 2 Stream E scope):
//   - Parses Claude Code PostToolUse event JSON from stdin.
//   - Exits 0 always. Never writes to stderr. Errors go through LogHookEvent.
//   - Foreground wall-clock target ≤50 ms.
//   - Enqueues the edited file, coalesces via flock, and — when the coalesce
//     window has elapsed — asks Deps.Spawn to fork+setsid a detached actor
//     that drains the pending list via IndexIncremental.
func HookPostEdit(cfg config.Config, stdin io.Reader, stdout, stderr io.Writer, env map[string]string, args []string) int {
	return hookPostEditWithDeps(cfg, stdin, stdout, stderr, env, args, defaultPostEditDeps())
}

// hookPostEditWithDeps is the testable core. Never called directly from the
// CLI router — use HookPostEdit.
func hookPostEditWithDeps(cfg config.Config, stdin io.Reader, stdout, stderr io.Writer, env map[string]string, args []string, deps PostEditDeps) int {
	_ = cfg
	_ = stdout
	_ = stderr

	flags, ferr := parsePostEditFlags(args)
	if ferr != nil {
		heimdall.LogHookEvent("WARN", "post-edit", map[string]any{"err": "bad_flags", "detail": ferr.Error()})
		return 0
	}

	projectRoot := flags.project
	if projectRoot == "" {
		if cwd, err := os.Getwd(); err == nil {
			projectRoot = cwd
		}
	}
	if projectRoot == "" {
		// No project — nothing sensible to do. Log and exit.
		heimdall.LogHookEvent("WARN", "post-edit", map[string]any{"err": "no_project_root"})
		return 0
	}

	if heimdall.HooksDisabled(projectRoot, env) {
		return 0
	}

	// Parse stdin (best-effort — malformed JSON is Tier A silent).
	raw, _ := io.ReadAll(io.LimitReader(stdin, 256*1024))
	editedPath, sessionID := extractPostEditStdin(raw)
	if editedPath == "" {
		// Nothing to enqueue. Log at DEBUG, exit 0.
		logHookEventWithSession("DEBUG", "post-edit", sessionID, map[string]any{"err": "no_file_path"})
		return 0
	}
	if !filepath.IsAbs(editedPath) {
		editedPath = filepath.Join(projectRoot, editedPath)
	}
	editedPath = filepath.Clean(editedPath)

	hooksDir := filepath.Join(projectRoot, ".heimdall_db", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		logHookEventWithSession("WARN", "post-edit", sessionID, map[string]any{"err": "mkdir_hooks"})
		return 0
	}

	pendingPath := filepath.Join(hooksDir, "reindex.pending")
	if err := appendPending(pendingPath, editedPath, postEditPendingCap); err != nil {
		logHookEventWithSession("WARN", "post-edit", sessionID, map[string]any{"err": "append_pending"})
		// Keep going — the lock path below may still hand off a cleanup to
		// an actor and enqueue on the next fire.
	}

	// Try to acquire the cross-process lock. If held → another actor is in
	// flight, we coalesced. If free → decide whether to spawn based on the
	// coalesce window.
	lockPath := filepath.Join(hooksDir, "reindex.lock")
	rel, acquired, err := acquirePostEditLock(lockPath, deps)
	if err != nil {
		logHookEventWithSession("WARN", "post-edit", sessionID, map[string]any{"err": "lock_err"})
		return 0
	}
	if !acquired {
		// Coalesced on a running actor.
		return 0
	}
	// Lock is held. Check for an orphaned inflight PID (previous actor died
	// without clearing state) and decide whether to spawn.
	inflightPath := filepath.Join(hooksDir, "reindex.inflight.pid")
	if pid, ok := readPID(inflightPath); ok {
		if deps.KillCheck(pid) {
			// The PID file says an actor is alive even though the lock was
			// free. That's only possible if flock is unsupported (fallback
			// path took the lock but the old actor still runs) — in which
			// case the running actor will clean up. Release and exit.
			rel()
			return 0
		}
		// Orphaned — previous actor died. Clear state and take over.
		_ = os.Remove(inflightPath)
		logHookEventWithSession("INFO", "post-edit", sessionID, map[string]any{"msg": "orphan_reaped", "pid": pid})
	}

	lastRun := readLastRun(filepath.Join(hooksDir, "reindex.last_run"))
	now := deps.Now()
	if !lastRun.IsZero() && now.Sub(lastRun) < postEditCoalesceWindow {
		rel()
		return 0
	}

	// Spawn the detached actor. On success the actor takes ownership of the
	// lockfile lifecycle — we still release *our* in-process flock handle,
	// but the actor races to write its own PID into inflight.pid.
	if err := deps.Spawn(projectRoot, sessionID); err != nil {
		logHookEventWithSession("WARN", "post-edit", sessionID, map[string]any{"err": "spawn_failed", "detail": err.Error()})
		rel()
		return 0
	}
	logHookEventWithSession("INFO", "post-edit", sessionID, map[string]any{"msg": "actor_spawned"})
	rel()
	return 0
}

// ----- flag parsing -----

type postEditFlags struct {
	project string
	source  string
	version int
	session string
}

func parsePostEditFlags(args []string) (postEditFlags, error) {
	f := postEditFlags{source: "heimdall", version: 1}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--project":
			if i+1 >= len(args) {
				return f, errors.New("--project requires a value")
			}
			i++
			f.project = args[i]
		case strings.HasPrefix(a, "--project="):
			f.project = strings.TrimPrefix(a, "--project=")
		case a == "--session":
			if i+1 >= len(args) {
				return f, errors.New("--session requires a value")
			}
			i++
			f.session = args[i]
		case strings.HasPrefix(a, "--session="):
			f.session = strings.TrimPrefix(a, "--session=")
		case a == "--source":
			if i+1 >= len(args) {
				return f, errors.New("--source requires a value")
			}
			i++
			f.source = args[i]
		case strings.HasPrefix(a, "--source="):
			f.source = strings.TrimPrefix(a, "--source=")
		case a == "--version":
			if i+1 >= len(args) {
				return f, errors.New("--version requires a value")
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil {
				return f, fmt.Errorf("--version must be integer: %w", err)
			}
			f.version = n
		case strings.HasPrefix(a, "--version="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--version="))
			if err != nil {
				return f, fmt.Errorf("--version must be integer: %w", err)
			}
			f.version = n
		default:
			// Unknown flag — log and ignore to keep the foreground path
			// forward-compatible with future Claude Code additions.
			return f, fmt.Errorf("unknown flag %q", a)
		}
	}
	return f, nil
}

// ----- stdin parsing -----

type postEditEvent struct {
	SessionID string `json:"session_id"`
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		FilePath string `json:"file_path"`
	} `json:"tool_input"`
}

// extractPostEditStdin parses the PostToolUse event JSON and returns
// (absolute edited file path, session_id). Either return can be empty when
// the payload is missing the corresponding field. Kept as a single call so
// stdin is consumed once.
func extractPostEditStdin(raw []byte) (filePath, sessionID string) {
	if len(raw) == 0 {
		return "", ""
	}
	var ev postEditEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return "", ""
	}
	return strings.TrimSpace(ev.ToolInput.FilePath), strings.TrimSpace(ev.SessionID)
}

// ----- pending file I/O -----

// appendPending adds path as a new line to pendingPath. If the file would
// exceed capLines after append, the oldest lines are dropped first.
func appendPending(pendingPath, path string, capLines int) error {
	existing, _ := os.ReadFile(pendingPath)
	var lines []string
	if len(existing) > 0 {
		lines = strings.Split(strings.TrimRight(string(existing), "\n"), "\n")
	}
	lines = append(lines, path)
	if len(lines) > capLines {
		lines = lines[len(lines)-capLines:]
	}
	body := strings.Join(lines, "\n") + "\n"
	tmp := pendingPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, pendingPath)
}

// drainPending atomically reads and removes the pending file, returning the
// list of absolute file paths to reindex. Used by the actor.
func drainPending(pendingPath string) ([]string, error) {
	drainingPath := pendingPath + ".draining"
	if err := os.Rename(pendingPath, drainingPath); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	data, err := os.ReadFile(drainingPath)
	_ = os.Remove(drainingPath)
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimRight(string(data), "\n")
	if trimmed == "" {
		return nil, nil
	}
	// Deduplicate while preserving latest-wins ordering.
	seen := map[string]bool{}
	raw := strings.Split(trimmed, "\n")
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out, nil
}

// readLastRun returns the persisted last-run time, or the zero Time if the
// file is missing or unparseable.
func readLastRun(path string) time.Time {
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(ts, 0)
}

// writeLastRun stores ts as a unix timestamp via an atomic rename.
func writeLastRun(path string, ts time.Time) error {
	tmp := path + ".tmp"
	body := strconv.FormatInt(ts.Unix(), 10)
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ----- lockfile -----

// postEditRelease is returned by acquirePostEditLock. Calling it releases
// whichever lock mechanism was in play (flock unlock or lockfile delete).
type postEditRelease func()

// acquirePostEditLock tries to take the cross-process lock at lockPath.
// Returns (release, acquired=true, nil) on success, (nil, false, nil) when
// another process holds the lock, or (nil, false, err) on unexpected errors.
func acquirePostEditLock(lockPath string, deps PostEditDeps) (postEditRelease, bool, error) {
	if deps.UseFlock {
		return acquirePostEditFlock(lockPath)
	}
	return acquirePostEditFallback(lockPath, deps.Now)
}

func acquirePostEditFlock(lockPath string) (postEditRelease, bool, error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		// flock unsupported (NFS, certain WSL) → fall back.
		return acquirePostEditFallback(lockPath, time.Now)
	}
	rel := func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
	return rel, true, nil
}

// acquirePostEditFallback uses O_CREAT|O_EXCL with an embedded TTL stamp so
// that a filesystem without flock (or a stale lockfile left behind by a
// crashed actor) can still be recovered. The stamp format is the unix time
// of the lock holder; anything older than postEditStaleLockTTL is considered
// stale and forcibly taken over.
func acquirePostEditFallback(lockPath string, now func() time.Time) (postEditRelease, bool, error) {
	nowFn := now
	if nowFn == nil {
		nowFn = time.Now
	}
	stamp := []byte(strconv.FormatInt(nowFn().Unix(), 10))
	if err := os.WriteFile(lockPath+".tmp", stamp, 0o644); err != nil {
		return nil, false, err
	}
	// Try an atomic create via link(2). If the file already exists check its
	// age; if stale, steal it.
	if err := os.Link(lockPath+".tmp", lockPath); err != nil {
		_ = os.Remove(lockPath + ".tmp")
		// Existing lock — check staleness.
		data, rerr := os.ReadFile(lockPath)
		if rerr != nil {
			return nil, false, nil
		}
		ts, perr := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
		if perr != nil || nowFn().Unix()-ts > int64(postEditStaleLockTTL/time.Second) {
			// Stale — take over.
			_ = os.Remove(lockPath)
			if werr := os.WriteFile(lockPath, stamp, 0o644); werr == nil {
				rel := func() { _ = os.Remove(lockPath) }
				return rel, true, nil
			}
			return nil, false, nil
		}
		return nil, false, nil
	}
	_ = os.Remove(lockPath + ".tmp")
	rel := func() { _ = os.Remove(lockPath) }
	return rel, true, nil
}

// ----- PID helpers -----

func readPID(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, false
	}
	return n, true
}

func writePID(path string, pid int) error {
	return os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o644)
}

// killCheck probes whether pid is alive via kill(pid, 0).
func killCheck(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	// ESRCH → dead. EPERM → alive (we're not allowed to signal it).
	return errors.Is(err, syscall.EPERM)
}

// ----- spawn -----

// spawnPostEditActor re-invokes heimdall-mcp with the hidden post-edit-actor
// subcommand, detached via setsid. Foreground returns immediately after
// Start(); the child lives on.
//
// sessionID is propagated via --session so the actor's `reindex_ok` /
// `reindex_failed` log lines can be attributed to the spawning session in
// `heimdall-mcp sessions report`. It may be empty (older stdin shapes,
// dry-fire contexts) — in that case no --session flag is passed and the
// actor falls back to session-less logging.
func spawnPostEditActor(projectRoot, sessionID string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"hook", "post-edit-actor", "--project", projectRoot}
	if sessionID != "" {
		args = append(args, "--session", sessionID)
	}
	cmd := exec.Command(self, args...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Release the child so we don't track its resources. The actor is
	// responsible for its own state from here on.
	_ = cmd.Process.Release()
	return nil
}

// ----- actor entry point -----

// HookPostEditActor is the hidden `heimdall-mcp hook post-edit-actor`
// subcommand invoked by the foreground fork+setsid path. It is NOT intended
// to be user-facing; `hooks doctor` explicitly skips it.
func HookPostEditActor(cfg config.Config, stdin io.Reader, stdout, stderr io.Writer, env map[string]string, args []string) int {
	_ = stdin
	_ = stdout
	_ = stderr
	flags, ferr := parsePostEditFlags(args)
	if ferr != nil {
		heimdall.LogHookEvent("WARN", "post-edit-actor", map[string]any{"err": "bad_flags"})
		return 0
	}
	projectRoot := flags.project
	if projectRoot == "" {
		if cwd, err := os.Getwd(); err == nil {
			projectRoot = cwd
		}
	}
	if projectRoot == "" {
		return 0
	}

	// Full actor run — this is the layer exercised directly by unit tests
	// via runPostEditActor.
	hooksDir := filepath.Join(projectRoot, ".heimdall_db", "hooks")
	runner := newRealIndexRunner(cfg, projectRoot)
	_ = runPostEditActor(context.Background(), postEditActorConfig{
		projectRoot:    projectRoot,
		hooksDir:       hooksDir,
		sessionID:      flags.session,
		coalesceWindow: postEditCoalesceWindow,
		deadletterCap:  postEditDeadletterCap,
		now:            time.Now,
		sleep:          time.Sleep,
		runner:         runner,
	})
	return 0
}

// ----- real indexer adapter -----

// indexRunner is the minimal surface the actor needs. Split into an interface
// so unit tests can inject a mock and skip Ollama entirely.
type indexRunner interface {
	Reindex(ctx context.Context, files []string) error
}

type realIndexRunner struct {
	cfg         config.Config
	projectRoot string
}

func newRealIndexRunner(cfg config.Config, projectRoot string) *realIndexRunner {
	return &realIndexRunner{cfg: cfg, projectRoot: projectRoot}
}

// Reindex runs IndexIncremental on the project. The files argument is
// advisory — IndexIncremental already walks the tree and picks changed
// files; the list is used for logging and deadletter tracking.
func (r *realIndexRunner) Reindex(ctx context.Context, files []string) error {
	_ = files
	client := heimdall.NewOllamaClient(r.cfg.OllamaEndpoint)
	if err := client.Ping(ctx); err != nil {
		return fmt.Errorf("ollama_ping: %w", err)
	}
	baseDir := filepath.Join(r.projectRoot, ".heimdall_db")
	modelDir := heimdall.ModelDBDir(baseDir, r.cfg.Model)
	store, err := heimdall.OpenStore(modelDir)
	if err != nil {
		return fmt.Errorf("open_store: %w", err)
	}
	defer store.Close()
	embedder := heimdall.NewOllamaEmbedder(client, r.cfg.Model)
	indexer := heimdall.NewIndexer(r.projectRoot, embedder, store, heimdall.ChunkerOpts{
		MaxChunkSize: 1500,
		ContextDepth: r.cfg.ContextDepth,
		ExcludeGlobs: r.cfg.ExcludePatterns,
	})
	if _, err := indexer.IndexIncremental(ctx); err != nil {
		return fmt.Errorf("index_incremental: %w", err)
	}
	return nil
}
