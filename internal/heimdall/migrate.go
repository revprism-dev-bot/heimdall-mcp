package heimdall

import (
	"database/sql"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DisableLegacyMigrationEnv is the environment variable callers can set to
// a truthy value to opt out of the `<model>_latest/` → `<model>/` rename
// performed by MigrateLegacyLatestDir. Everything else in the read path
// still works — ModelDBDir's mixed-state reader picks the populated dir —
// but no data is moved on disk. Useful when a user wants to audit the
// mixed state manually (e.g. `heimdall-mcp cleanup-legacy-latest`,
// follow-up New-OQ-3).
//
// Accepted truthy values are any strconv.ParseBool-recognized form
// (`1`, `t`, `true`, `TRUE`, etc.) plus the common operator reflexes
// `yes` and `on`, case-insensitive. Whitespace is trimmed. Any other
// value is treated as "not set" and migration proceeds normally — this
// fail-safe default ensures a misconfigured env var never silently
// masks a legitimate migration need.
const DisableLegacyMigrationEnv = "HEIMDALL_DISABLE_LEGACY_MIGRATION"

// staleLockThreshold is the age beyond which a lock file is assumed to
// be orphaned by a prior crashed process (Ctrl-C / SIGKILL / os.Exit
// paths that skip defers). Deliberately generous: a healthy migration
// typically takes seconds, so 10 minutes is >10x the 99th-percentile
// hot-path runtime. Stale-lock recovery logs both the detection and the
// cleanup so operators can audit.
const staleLockThreshold = 10 * time.Minute

// envDisablesLegacyMigration reports whether the opt-out env var is set
// to any recognized truthy value. Uses strconv.ParseBool semantics plus
// `yes`/`on` normalization, with whitespace tolerance. Any unrecognized
// value (including the empty string, `0`, `false`, `no`, `off`, or any
// bogus value) returns false → migration proceeds. This avoids the
// brittle `os.Getenv(...) == "1"` pattern flagged by PR #73 review.
func envDisablesLegacyMigration() bool {
	raw := strings.TrimSpace(os.Getenv(DisableLegacyMigrationEnv))
	if raw == "" {
		return false
	}
	// Normalize common reflexes that strconv.ParseBool rejects.
	switch strings.ToLower(raw) {
	case "yes", "on":
		return true
	case "no", "off":
		return false
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false
	}
	return v
}

// MigrateDBDir renames .viking_db/ to .heimdall_db/ if the old dir exists
// and the new one does not. Uses a lock file to prevent TOCTOU races when
// multiple server instances start simultaneously.
func MigrateDBDir(parentDir string) {
	oldDir := filepath.Join(parentDir, ".viking_db")
	newDir := filepath.Join(parentDir, ".heimdall_db")

	// Quick pre-check (common case: nothing to migrate)
	if _, err := os.Stat(oldDir); err != nil {
		return // old dir doesn't exist, nothing to migrate
	}

	// Acquire exclusive lock file to prevent race conditions
	lockPath := filepath.Join(parentDir, ".heimdall-migrate.lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return // another process is migrating
	}
	defer os.Remove(lockPath)
	defer lock.Close()

	// Re-check conditions after acquiring lock
	if _, err := os.Stat(oldDir); err != nil {
		return // another process already migrated
	}
	if _, err := os.Stat(newDir); err == nil {
		return // new dir already exists, skip
	}

	if err := os.Rename(oldDir, newDir); err != nil {
		log.Printf("heimdall: failed to migrate %s → %s: %v", oldDir, newDir, err)
		return
	}
	log.Printf("heimdall: migrated database directory %s → %s", oldDir, newDir)
}

// MigrateToModelDir moves a legacy single-model vectors.db into the
// appropriate model-specific subdirectory.
//
// If baseDir/vectors.db exists (old single-model format):
//   - Reads the embedding_model from the store_metadata table
//   - If it matches model or is empty, moves vectors.db into baseDir/<model>/vectors.db
//   - If it doesn't match, moves it into baseDir/<stored-model>/vectors.db
//
// This preserves existing indexes during the transition to multi-model layout.
func MigrateToModelDir(baseDir, model string) {
	oldDB := filepath.Join(baseDir, "vectors.db")
	if _, err := os.Stat(oldDB); err != nil {
		return // no legacy DB to migrate
	}

	// Acquire exclusive lock file to prevent race conditions
	lockPath := filepath.Join(baseDir, ".heimdall-model-migrate.lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return // another process is migrating
	}
	defer os.Remove(lockPath)
	defer lock.Close()

	// Re-check after acquiring lock
	if _, err := os.Stat(oldDB); err != nil {
		return // another process already migrated
	}

	// Read the embedding_model from the legacy DB
	storedModel := readEmbeddingModel(oldDB)

	// Determine which model dir to move into
	targetModel := model
	if storedModel != "" && storedModel != model {
		targetModel = storedModel
	}

	targetDir := ModelDBDir(baseDir, targetModel)

	// Don't migrate if target already has a vectors.db
	targetDB := filepath.Join(targetDir, "vectors.db")
	if _, err := os.Stat(targetDB); err == nil {
		return // target already exists, skip
	}

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		log.Printf("heimdall: failed to create model dir %s: %v", targetDir, err)
		return
	}

	// Move all files from baseDir that belong to the SQLite DB
	// (vectors.db, vectors.db-wal, vectors.db-shm)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		src := oldDB + suffix
		if _, err := os.Stat(src); err != nil {
			continue
		}
		dst := targetDB + suffix
		if err := os.Rename(src, dst); err != nil {
			log.Printf("heimdall: failed to migrate %s → %s: %v", src, dst, err)
			return
		}
	}

	log.Printf("heimdall: migrated legacy DB to model dir %s", targetDir)
}

// MigrateLegacyLatestDir renames `baseDir/<model>_latest/` to
// `baseDir/<model>/` when only the legacy dir exists. Returns
// (migrated, err) where `migrated` is true iff the physical rename
// happened in this call. Safe to call from multiple writer entry points
// (CLI index, MCP runIndex, toolIndexText) — a lock file prevents
// concurrent racers from double-migrating. The function is idempotent:
// a second call after a successful first call reports (false, nil).
//
// Behaviour summary (v2 plan PR1 §4 Problem #1 + D-12, D-13, D-16):
//   - If env var HEIMDALL_DISABLE_LEGACY_MIGRATION=1, returns (false, nil)
//     immediately — no lock acquired, no files touched.
//   - If the legacy path `<model>_latest` does NOT exist, returns
//     (false, nil). No-op.
//   - If the legacy path IS a symlink (Lstat check), log WARN and return
//     (false, nil). Never dereference — avoids the cross-device /
//     unexpected-target footgun. Follow-up is a manual operator task.
//   - If BOTH `<model>_latest` and `<model>` exist as directories, log
//     WARN with a deterministic message and return (false, nil). The
//     mixed-state reader in ModelDBDir picks whichever holds newer data.
//     A follow-up `heimdall-mcp cleanup-legacy-latest` command
//     (New-OQ-3) is the way to force a resolution.
//   - Otherwise: acquire `baseDir/.heimdall-legacy-migrate-<model>.lock`;
//     re-check preconditions; open the legacy store; call
//     store.HookCacheClear() (D-12 — invalidate stale hook_cache rows);
//     close; os.Rename(legacy, canonical); return (true, nil).
//
// HookCacheClear failures are logged as WARN but do NOT abort the
// rename (R-v2-2). Cache staleness is recoverable; leaving the bigger
// correctness bug (wrong reader path) unfixed is not.
//
// This function only renames. It never deletes data. If any invariant
// fails mid-way, we log and return false without touching the filesystem.
func MigrateLegacyLatestDir(baseDir, model string) (bool, error) {
	if envDisablesLegacyMigration() {
		return false, nil
	}
	sanitized := sanitizeModelDirName(NormalizeModelName(model))
	legacy := filepath.Join(baseDir, sanitized+"_latest")
	canonical := filepath.Join(baseDir, sanitized)

	// Pre-check: legacy must exist, and it must be a directory (not a
	// symlink). Use Lstat so a symlink is detected without following it.
	info, err := os.Lstat(legacy)
	if err != nil {
		return false, nil // nothing to migrate
	}
	if info.Mode()&os.ModeSymlink != 0 {
		log.Printf("heimdall: skipping legacy-latest migration — %s is a symlink (use `heimdall-mcp cleanup-legacy-latest` once available)", legacy)
		return false, nil
	}
	if !info.IsDir() {
		log.Printf("heimdall: skipping legacy-latest migration — %s is not a directory", legacy)
		return false, nil
	}

	// If both exist, do not move — the mixed-state reader in ModelDBDir
	// handles selection. Operator cleanup command covers the merge case.
	if _, err := os.Stat(canonical); err == nil {
		log.Printf("heimdall: legacy-latest migration skipped — both %s and %s exist; leaving in place (reader-preference handles it)", legacy, canonical)
		return false, nil
	}

	// Acquire an exclusive lock file keyed by model so two writer entry
	// points can't race to rename the same dir.
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return false, err
	}
	lockPath := filepath.Join(baseDir, ".heimdall-legacy-migrate-"+sanitized+".lock")

	lock, err := acquireMigrationLock(lockPath)
	if err != nil {
		// Contention with a healthy peer; treat as no-op for the caller.
		return false, nil
	}

	// Explicit single-defer with logged errors. LIFO defers would ALSO
	// produce the correct close-then-remove order, but relying on that is
	// fragile (a later maintainer reordering the statements would break
	// Windows cleanup). Logging surfaces cleanup failures instead of
	// silently leaking the lock file into a permanent future no-op.
	defer func() {
		if err := lock.Close(); err != nil {
			log.Printf("heimdall: lock.Close %s: %v", lockPath, err)
		}
		if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
			log.Printf("heimdall: os.Remove(%s): %v — manual cleanup may be required if it persists", lockPath, err)
		}
	}()

	// Re-check legacy exists and canonical doesn't, after acquiring the
	// lock. Another migration may have completed while we waited.
	if info, err := os.Lstat(legacy); err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, nil
	}
	if _, err := os.Stat(canonical); err == nil {
		// Canonical appeared during the race — defer to mixed-state reader.
		return false, nil
	}

	// Clear the legacy store's hook_cache before renaming. Failures are
	// logged and do NOT abort the rename (R-v2-2). Opened briefly under
	// write mode — sqlite will reject concurrent writers via the DB lock;
	// any racing writer will back off on its own.
	if store, openErr := OpenStore(legacy); openErr != nil {
		log.Printf("heimdall: legacy-latest migration — could not open %s to clear hook_cache: %v (continuing)", legacy, openErr)
	} else {
		if clearErr := store.HookCacheClear(); clearErr != nil {
			log.Printf("heimdall: legacy-latest migration — HookCacheClear on %s failed: %v (continuing)", legacy, clearErr)
		}
		if closeErr := store.Close(); closeErr != nil {
			log.Printf("heimdall: legacy-latest migration — close of legacy store %s failed: %v (continuing)", legacy, closeErr)
		}
	}

	if err := os.Rename(legacy, canonical); err != nil {
		return false, err
	}
	log.Printf("heimdall: migrated legacy model dir %s → %s", legacy, canonical)
	return true, nil
}

// acquireMigrationLock attempts O_EXCL creation of lockPath. On EEXIST it
// stat-s the existing lock file; if the file is older than
// staleLockThreshold, it logs a WARN, removes it, and retries once.
// Returns the opened *os.File on success or the underlying error if
// contention remains (typical case: a peer is legitimately running).
//
// Rationale: Go's `defer` does NOT run on os.Exit / SIGKILL, so any
// crash between lock acquisition and the deferred cleanup leaves the
// lock file permanently on disk. Without this recovery, a user who
// Ctrl-C's a long-running reindex on Mac ends up silently stuck on the
// pre-migration legacy path forever (every subsequent run sees EEXIST
// and returns false, nil). See PR #73 concurrency review CRITICAL
// finding #1.
func acquireMigrationLock(lockPath string) (*os.File, error) {
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err == nil {
		return lock, nil
	}
	if !os.IsExist(err) {
		// Any error other than "already exists" is a real failure
		// (permission denied, etc.) — surface it.
		return nil, err
	}

	// EEXIST — inspect the age of the existing lock. Use Lstat so a
	// symlink planted at lockPath (with write access to baseDir — an
	// existing trust-boundary assumption) cannot spoof an ancient
	// mtime on a target file the attacker doesn't own. Matches the
	// existing Lstat-refuses-symlink-source discipline on the migration
	// source. See PR #73 re-review N-2.
	info, statErr := os.Lstat(lockPath)
	if statErr != nil {
		// The lock file disappeared between OpenFile and Stat — treat as
		// "not ours to claim this pass" and let the caller no-op. A
		// subsequent invocation will get a clean OpenFile.
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		// Refuse to trust a symlinked lockfile — treat it as contention
		// and leave the symlink in place. Removing the symlink here
		// would still be safe (Remove on a symlink unlinks the link,
		// not the target), but treating it as contention surfaces the
		// anomaly to the operator via the caller's no-op path.
		log.Printf("heimdall: legacy-migration lock at %s is a symlink; refusing to treat as stale (possible tampering) — returning contention", lockPath)
		return nil, err
	}
	age := time.Since(info.ModTime())
	if age < staleLockThreshold {
		// A healthy peer is working; defer.
		return nil, err
	}

	log.Printf("heimdall: stale legacy-migration lock at %s (age=%s > %s); removing and retrying — likely from a prior SIGKILL/Ctrl-C crash", lockPath, age.Round(time.Second), staleLockThreshold)
	if rmErr := os.Remove(lockPath); rmErr != nil && !os.IsNotExist(rmErr) {
		log.Printf("heimdall: could not remove stale lock %s: %v (giving up this pass)", lockPath, rmErr)
		return nil, err
	}
	// Single retry. If we lose a genuine race here (another process
	// raced us to recover the stale lock), the second EEXIST is reported
	// as contention — safe.
	lock, retryErr := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if retryErr != nil {
		return nil, retryErr
	}
	return lock, nil
}

// FindLegacyLatestDirs scans baseDir for entries whose name ends in
// `_latest` and have a sibling canonical `<name>` directory. Returns
// the full paths of each legacy dir. This is the reader side for the
// `heimdall-mcp cleanup-legacy-latest` administrative command.
//
// It intentionally only reports dirs that already have a canonical
// sibling — otherwise removing the `_latest` dir would silently drop
// user data. When the canonical sibling is missing, the reader layer
// (ModelDBDir) still points at the `_latest` dir; users who want to
// migrate it to canonical should run an index (which fires
// MigrateLegacyLatestDir) rather than manually deleting.
//
// Never descends. Non-directory entries are skipped. If baseDir
// doesn't exist or is unreadable the function returns nil, nil
// (non-fatal; caller treats "nothing to clean up" as success).
func FindLegacyLatestDirs(baseDir string) ([]LegacyLatestReport, error) {
	if baseDir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	// Build a set of non-legacy dir names so we can pair them up.
	canonical := make(map[string]bool)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, "_latest") {
			canonical[name] = true
		}
	}

	var out []LegacyLatestReport
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, "_latest") {
			continue
		}
		sibling := strings.TrimSuffix(name, "_latest")
		legacyPath := filepath.Join(baseDir, name)
		canonPath := filepath.Join(baseDir, sibling)
		// Count rows if possible so the operator can see WHAT they'd
		// be deleting. Best-effort; errors become -1 rows.
		rows := countLegacyRows(legacyPath)
		out = append(out, LegacyLatestReport{
			LegacyPath:        legacyPath,
			CanonicalPath:     canonPath,
			CanonicalHasSibling: canonical[sibling],
			LegacyRowCount:    rows,
		})
	}
	return out, nil
}

// LegacyLatestReport describes one `<model>_latest/` dir for the
// cleanup-legacy-latest CLI command.
type LegacyLatestReport struct {
	LegacyPath          string // absolute path to the `<model>_latest/` dir
	CanonicalPath       string // absolute path to the expected `<model>/` canonical sibling
	CanonicalHasSibling bool   // true when CanonicalPath exists (safe to delete legacy)
	LegacyRowCount      int64  // rows in legacy vectors.db; -1 if unreadable
}

// countLegacyRows opens the legacy dir's vectors.db read-only and
// returns the row count from the `entries` table. Non-destructive.
// Returns -1 on any error (dir missing, db missing, schema different).
func countLegacyRows(legacyDir string) int64 {
	dbPath := filepath.Join(legacyDir, "vectors.db")
	if _, err := os.Stat(dbPath); err != nil {
		return -1
	}
	db, err := sql.Open("sqlite", dbPath+"?mode=ro&_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return -1
	}
	defer db.Close()
	var n int64
	err = db.QueryRow(`SELECT COUNT(*) FROM entries`).Scan(&n)
	if err != nil {
		return -1
	}
	return n
}

// RemoveLegacyLatestDir deletes `<baseDir>/<name>_latest/` (where name
// is derived from the model) recursively. Fails loudly if the
// canonical sibling does not exist — that's a user-data-loss red flag.
// Callers must have confirmed via FindLegacyLatestDirs that the dir is
// safe to remove.
func RemoveLegacyLatestDir(legacyPath string) error {
	if !strings.HasSuffix(legacyPath, "_latest") {
		return &removeSafetyError{reason: "path does not have the _latest suffix; refusing to remove for safety", path: legacyPath}
	}
	// Require that the canonical sibling exist, otherwise we'd be
	// dropping user data with no canonical replacement.
	canonical := strings.TrimSuffix(legacyPath, "_latest")
	if _, err := os.Stat(canonical); err != nil {
		return &removeSafetyError{reason: "no canonical sibling exists; removing would delete the only copy of the data", path: legacyPath}
	}
	return os.RemoveAll(legacyPath)
}

type removeSafetyError struct {
	reason string
	path   string
}

func (e *removeSafetyError) Error() string {
	return "refusing to remove " + e.path + ": " + e.reason
}

// readEmbeddingModel opens a vectors.db read-only and reads the embedding_model
// from the store_metadata table. Returns empty string if unavailable.
func readEmbeddingModel(dbPath string) string {
	db, err := sql.Open("sqlite", dbPath+"?mode=ro&_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return ""
	}
	defer db.Close()

	var model string
	err = db.QueryRow(`SELECT value FROM store_metadata WHERE key = 'embedding_model'`).Scan(&model)
	if err != nil {
		return ""
	}
	return model
}
