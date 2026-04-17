package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// postEditActorConfig captures everything the background actor needs to run
// without touching process-global state. All time and I/O dependencies are
// injectable so unit tests can exercise the full drain/reindex/deadletter
// pipeline in-process without spawning subprocesses.
type postEditActorConfig struct {
	projectRoot    string
	hooksDir       string
	sessionID      string // spawning session id; empty ⇒ fall back to session-less logging
	coalesceWindow time.Duration
	deadletterCap  int
	now            func() time.Time
	sleep          func(time.Duration)
	runner         indexRunner
}

// deadletterEntry is the JSONL record shape written to
// `reindex.deadletter.jsonl` on actor failure. Fields are flat and stable so
// the file is grep- and jq-friendly.
type deadletterEntry struct {
	File    string `json:"file"`
	ErrCode string `json:"err_code"`
	Attempt int    `json:"attempt"`
	TS      int64  `json:"ts"`
}

// runPostEditActor is the pure actor core, unit-tested directly.
//
// Flow per consolidated-plan §3.3:
//  1. Sleep the coalesce window (2 s default) so adjacent edits pile in.
//  2. Atomically drain reindex.pending.
//  3. Write reindex.inflight.pid with os.Getpid() so a concurrent
//     foreground hook can detect an orphan via kill(pid, 0).
//  4. Invoke the injected indexRunner over the drained file list.
//  5. On success: update reindex.last_run via atomic rename.
//  6. Drain the deadletter and retry each stale entry (hard drop at N=10).
//  7. On failure at any step: append a JSONL entry to
//     reindex.deadletter.jsonl; exit 0.
//
// Always returns nil. The actor has no caller that reads its error.
func runPostEditActor(ctx context.Context, cfg postEditActorConfig) error {
	if cfg.now == nil {
		cfg.now = time.Now
	}
	if cfg.sleep == nil {
		cfg.sleep = time.Sleep
	}
	if cfg.deadletterCap <= 0 {
		cfg.deadletterCap = postEditDeadletterCap
	}
	if err := os.MkdirAll(cfg.hooksDir, 0o755); err != nil {
		logHookEventWithSession("WARN", "post-edit-actor", cfg.sessionID, map[string]any{"err": "mkdir_hooks"})
		return nil
	}

	inflightPath := filepath.Join(cfg.hooksDir, "reindex.inflight.pid")
	_ = writePID(inflightPath, os.Getpid())
	defer func() { _ = os.Remove(inflightPath) }()

	cfg.sleep(cfg.coalesceWindow)

	// Snapshot the deadletter BEFORE the live drain. Anything we append to
	// the deadletter from the current pending failure must NOT be retried
	// in the same actor pass — the spec says "stale failures, retried on
	// next successful actor run" (consolidated-plan §3.3). Same-pass retry
	// would double-bump the attempt counter on entries that just failed
	// once, racing them toward the hard-drop cap.
	dlPath := filepath.Join(cfg.hooksDir, "reindex.deadletter.jsonl")
	staleDeadletter, _ := readDeadletter(dlPath)

	pending, derr := drainPending(filepath.Join(cfg.hooksDir, "reindex.pending"))
	if derr != nil {
		logHookEventWithSession("WARN", "post-edit-actor", cfg.sessionID, map[string]any{"err": "drain_pending"})
		// Still continue into the deadletter retry pass — stale entries
		// might still be recoverable even if the live pending file is
		// momentarily wedged.
	}

	if len(pending) > 0 {
		if rerr := cfg.runner.Reindex(ctx, pending); rerr != nil {
			logHookEventWithSession("WARN", "post-edit-actor", cfg.sessionID, map[string]any{
				"err":   "reindex_failed",
				"files": len(pending),
			})
			for _, file := range pending {
				appendDeadletter(cfg.hooksDir, deadletterEntry{
					File:    file,
					ErrCode: classifyErr(rerr),
					Attempt: 1,
					TS:      cfg.now().Unix(),
				})
			}
		} else {
			_ = writeLastRun(filepath.Join(cfg.hooksDir, "reindex.last_run"), cfg.now())
			logHookEventWithSession("INFO", "post-edit-actor", cfg.sessionID, map[string]any{
				"msg":   "reindex_ok",
				"files": len(pending),
			})
		}
	}

	// Deadletter retry pass. Replays only the SNAPSHOT taken before the
	// live drain (so freshly-appended entries are deferred to the next run),
	// then re-merges the result with any new appends.
	replayStaleDeadletter(ctx, cfg, dlPath, staleDeadletter)
	return nil
}

// replayStaleDeadletter retries each entry in the snapshot. Successes are
// dropped; failures are persisted with attempt+1; entries at or above the cap
// are hard-dropped with a WARN. Entries appended *after* the snapshot (i.e.
// those produced by the current actor pass) are preserved untouched.
func replayStaleDeadletter(ctx context.Context, cfg postEditActorConfig, dlPath string, snapshot []deadletterEntry) {
	if len(snapshot) == 0 {
		return
	}
	// Build a set of snapshot files so we can split the post-snapshot
	// state into "stale" (try-then-rewrite) and "fresh" (passthrough).
	snapshotKeys := make(map[string]int, len(snapshot))
	for i, e := range snapshot {
		snapshotKeys[e.File] = i
	}

	var stillStale []deadletterEntry
	for _, e := range snapshot {
		if e.Attempt >= cfg.deadletterCap {
			logHookEventWithSession("WARN", "post-edit-actor", cfg.sessionID, map[string]any{
				"msg":     "deadletter_dropped",
				"attempt": e.Attempt,
			})
			continue
		}
		// Retry just this one file. The real runner re-walks the project
		// under IndexIncremental, but the unit-test runner is free to
		// treat the files list as authoritative.
		if rerr := cfg.runner.Reindex(ctx, []string{e.File}); rerr != nil {
			e.Attempt++
			e.ErrCode = classifyErr(rerr)
			e.TS = cfg.now().Unix()
			stillStale = append(stillStale, e)
			continue
		}
		logHookEventWithSession("INFO", "post-edit-actor", cfg.sessionID, map[string]any{
			"msg":     "deadletter_recovered",
			"attempt": e.Attempt,
		})
	}

	// Re-merge: re-read whatever the deadletter looks like *now* (it may
	// have grown if the live drain failed and appended fresh entries),
	// drop the snapshot files, and append the still-stale survivors.
	current, _ := readDeadletter(dlPath)
	merged := make([]deadletterEntry, 0, len(current)+len(stillStale))
	for _, e := range current {
		if _, isSnapshot := snapshotKeys[e.File]; isSnapshot {
			continue
		}
		merged = append(merged, e)
	}
	merged = append(merged, stillStale...)
	_ = writeDeadletter(dlPath, merged)
}

// appendDeadletter writes one JSONL line to reindex.deadletter.jsonl,
// creating the file if needed. Best-effort — failures are logged but never
// surfaced.
func appendDeadletter(hooksDir string, e deadletterEntry) {
	path := filepath.Join(hooksDir, "reindex.deadletter.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	b = append(b, '\n')
	_, _ = f.Write(b)
}

// readDeadletter returns all entries in the JSONL file. Unparseable lines
// are silently dropped (we'd rather lose one bad line than abort the whole
// retry pass).
func readDeadletter(path string) ([]deadletterEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []deadletterEntry
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e deadletterEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// writeDeadletter atomically rewrites the JSONL file with the given entries.
// An empty entries list removes the file entirely so next actor runs start
// clean.
func writeDeadletter(path string, entries []deadletterEntry) error {
	if len(entries) == 0 {
		_ = os.Remove(path)
		return nil
	}
	var b strings.Builder
	for _, e := range entries {
		buf, err := json.Marshal(e)
		if err != nil {
			continue
		}
		b.Write(buf)
		b.WriteByte('\n')
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// classifyErr boils the runner's returned error down to a short, stable
// identifier suitable for the deadletter err_code field. The full error
// string is deliberately dropped to keep the JSONL grep-friendly and to
// avoid leaking absolute paths into persistent state.
func classifyErr(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "ollama_ping"):
		return "ollama_down"
	case strings.Contains(msg, "open_store"):
		return "store_open"
	case strings.Contains(msg, "index_incremental"):
		return "index_failed"
	default:
		return "unknown"
	}
}
