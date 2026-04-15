package heimdall

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Tier B failure notes are visible to the user and cost token budget. We
// don't want the same "ollama unreachable" banner on every turn while the
// user is restarting their daemon, so we rate-limit on (project_id,
// failure_code) to one emission per window. See docs/plans/hooks 04 §2.
//
// The store lives outside any project tree so suppression survives
// `heimdall-mcp index --rebuild` and is shared across worktrees of the
// same project.

// suppressDBPath returns the XDG-honoring on-disk location for the
// suppression SQLite file. Honors $XDG_STATE_HOME with $HOME fallback.
func suppressDBPath() (string, error) {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "heimdall", "suppress.db"), nil
}

// Package-level singleton so hook-path processes keep one connection open
// for the duration of the process. Tests override via
// ShouldEmitTierBWithPath which bypasses the singleton.
var (
	suppressOnce sync.Once
	suppressDB   *sql.DB
	suppressErr  error
)

func openSuppressDBOnce() (*sql.DB, error) {
	suppressOnce.Do(func() {
		path, err := suppressDBPath()
		if err != nil {
			suppressErr = err
			return
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			suppressErr = err
			return
		}
		db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=2000")
		if err != nil {
			suppressErr = err
			return
		}
		if _, err := db.Exec(`
			CREATE TABLE IF NOT EXISTS suppression (
				key          TEXT PRIMARY KEY,
				last_emitted INTEGER NOT NULL
			);
		`); err != nil {
			suppressErr = err
			db.Close()
			return
		}
		suppressDB = db
	})
	return suppressDB, suppressErr
}

// suppressKey folds (projectID, failureCode) into a short stable hash so
// long project paths don't bloat the row and so the key space is bounded.
// We don't try to be cryptographically unique — collisions are harmless
// (they just cause extra suppression across unrelated projects, which is
// strictly better than noisy repeats).
func suppressKey(projectID, failureCode string) string {
	h := sha256.Sum256([]byte(projectID + "\x00" + failureCode))
	return hex.EncodeToString(h[:16])
}

// ShouldEmitTierB returns true if this is the first time this
// (projectID, failureCode) pair has been seen since the start of window,
// and false otherwise. On a true return it records the current time as
// the last emission so subsequent calls within window return false.
//
// Any I/O error opens the emit gate (returns true). A silent suppression
// miss is worse than a duplicate note — if the store is broken, we'd
// rather the user see the banner than have them silently fail to
// diagnose whatever is wrong with heimdall itself.
func ShouldEmitTierB(projectID, failureCode string, window time.Duration) bool {
	db, err := openSuppressDBOnce()
	if err != nil || db == nil {
		return true
	}
	return shouldEmit(db, projectID, failureCode, window, time.Now())
}

// ShouldEmitTierBWithPath is the test-friendly variant that takes an
// explicit store path and does not touch the package singleton. Keeping
// the singleton intact in tests makes parallel test runs well-behaved.
func ShouldEmitTierBWithPath(dbPath, projectID, failureCode string, window time.Duration) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return true, err
	}
	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=2000")
	if err != nil {
		return true, err
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS suppression (
			key          TEXT PRIMARY KEY,
			last_emitted INTEGER NOT NULL
		);
	`); err != nil {
		return true, err
	}
	return shouldEmit(db, projectID, failureCode, window, time.Now()), nil
}

// shouldEmit is the shared implementation. now is injected for test
// determinism (the production caller always uses time.Now()).
func shouldEmit(db *sql.DB, projectID, failureCode string, window time.Duration, now time.Time) bool {
	key := suppressKey(projectID, failureCode)

	var last int64
	err := db.QueryRow(`SELECT last_emitted FROM suppression WHERE key = ?`, key).Scan(&last)
	switch err {
	case nil:
		if now.Unix()-last < int64(window.Seconds()) {
			return false
		}
	case sql.ErrNoRows:
		// fall through — first sighting.
	default:
		return true
	}

	if _, err := db.Exec(
		`INSERT OR REPLACE INTO suppression (key, last_emitted) VALUES (?, ?)`,
		key, now.Unix(),
	); err != nil {
		return true
	}
	return true
}
