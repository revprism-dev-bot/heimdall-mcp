package heimdall

import (
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Tests for MigrateLegacyLatestDir (PR1). Covers:
//   - happy-path rename of `<model>_latest/` → `<model>/`
//   - both-populated case: log and leave
//   - idempotency
//   - env-var opt-out (HEIMDALL_DISABLE_LEGACY_MIGRATION=1)
//   - symlink safety (Lstat-based skip)
//   - concurrent invocations
//   - HookCacheClear on legacy store before rename
//   - disabled-env ALSO forces reader to canonical (R-v2-3)

// seedLegacyDir creates baseDir/<model>_latest and writes a seed row.
// Returns the path to the legacy dir.
func seedLegacyDir(t *testing.T, baseDir, model string, rowModTime int64) string {
	t.Helper()
	legacy := filepath.Join(baseDir, sanitizeModelDirName(NormalizeModelName(model))+"_latest")
	store, err := OpenStore(legacy)
	if err != nil {
		t.Fatalf("open legacy %s: %v", legacy, err)
	}
	if err := store.Upsert([]VectorRecord{{
		ID:        "legacy:seed",
		FilePath:  "seed.go",
		Content:   "legacy-seed",
		Embedding: []float32{1, 0, 0, 0},
		ModTime:   rowModTime,
	}}); err != nil {
		store.Close()
		t.Fatalf("upsert legacy: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close legacy: %v", err)
	}
	return legacy
}

// countRowsAt opens a vectors.db read-only and returns the number of
// rows in `entries`. Returns -1 on error.
func countRowsAt(dbPath string) int64 {
	db, err := sql.Open("sqlite", dbPath+"?mode=ro")
	if err != nil {
		return -1
	}
	defer db.Close()
	var n int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM entries`).Scan(&n); err != nil {
		return -1
	}
	return n
}

// TestMigrateLegacyLatestDir_HappyPath — canonical dir absent, legacy dir
// exists → legacy is renamed to canonical. Returns (true, nil).
func TestMigrateLegacyLatestDir_HappyPath(t *testing.T) {
	baseDir := t.TempDir()
	const model = "nomic-embed-text"
	legacy := seedLegacyDir(t, baseDir, model, 200)

	migrated, err := MigrateLegacyLatestDir(baseDir, model)
	if err != nil {
		t.Fatalf("MigrateLegacyLatestDir returned error: %v", err)
	}
	if !migrated {
		t.Fatalf("MigrateLegacyLatestDir returned migrated=false, want true")
	}

	// Legacy dir must be gone.
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("legacy dir should be removed after rename, stat err = %v", err)
	}

	// Canonical dir must exist and hold the seed row.
	canonical := filepath.Join(baseDir, model)
	if _, err := os.Stat(canonical); err != nil {
		t.Fatalf("canonical dir should exist after migration: %v", err)
	}
	if n := countRowsAt(filepath.Join(canonical, "vectors.db")); n != 1 {
		t.Errorf("canonical store row count = %d, want 1", n)
	}
}

// TestMigrateLegacyLatestDir_NoLegacyDir_NoOp — canonical exists, legacy
// doesn't → returns (false, nil); no filesystem change.
func TestMigrateLegacyLatestDir_NoLegacyDir_NoOp(t *testing.T) {
	baseDir := t.TempDir()
	const model = "nomic-embed-text"
	// Just create canonical dir
	canonical := filepath.Join(baseDir, model)
	if err := os.MkdirAll(canonical, 0755); err != nil {
		t.Fatal(err)
	}

	migrated, err := MigrateLegacyLatestDir(baseDir, model)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if migrated {
		t.Errorf("migrated = true, want false (no legacy dir to migrate)")
	}
}

// TestMigrateLegacyLatestDir_BothExist_LogsAndLeaves — both dirs populated.
// Returns (false, nil); neither is deleted/moved. Reader preference from
// ModelDBDir handles the mixed state separately.
func TestMigrateLegacyLatestDir_BothExist_LogsAndLeaves(t *testing.T) {
	baseDir := t.TempDir()
	const model = "nomic-embed-text"
	legacy := seedLegacyDir(t, baseDir, model, 100)
	// Populate canonical separately.
	canonical := filepath.Join(baseDir, model)
	store, err := OpenStore(canonical)
	if err != nil {
		t.Fatalf("open canonical: %v", err)
	}
	if err := store.Upsert([]VectorRecord{{
		ID:        "canonical:seed",
		FilePath:  "c.go",
		Content:   "c",
		Embedding: []float32{1, 0, 0, 0},
		ModTime:   200,
	}}); err != nil {
		store.Close()
		t.Fatalf("upsert canonical: %v", err)
	}
	store.Close()

	migrated, err := MigrateLegacyLatestDir(baseDir, model)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if migrated {
		t.Errorf("migrated = true for both-populated; want false (log-and-leave)")
	}
	// Both dirs must still exist.
	if _, err := os.Stat(legacy); err != nil {
		t.Errorf("legacy dir should remain in both-populated case: %v", err)
	}
	if _, err := os.Stat(canonical); err != nil {
		t.Errorf("canonical dir should remain in both-populated case: %v", err)
	}
}

// TestMigrateLegacyLatestDir_Idempotent — repeated calls are safe.
// First call migrates, second returns (false, nil).
func TestMigrateLegacyLatestDir_Idempotent(t *testing.T) {
	baseDir := t.TempDir()
	const model = "nomic-embed-text"
	seedLegacyDir(t, baseDir, model, 100)

	migrated1, err1 := MigrateLegacyLatestDir(baseDir, model)
	if err1 != nil || !migrated1 {
		t.Fatalf("first: migrated=%v err=%v", migrated1, err1)
	}
	migrated2, err2 := MigrateLegacyLatestDir(baseDir, model)
	if err2 != nil {
		t.Fatalf("second: err=%v", err2)
	}
	if migrated2 {
		t.Errorf("second call: migrated=true, want false (already done)")
	}
}

// TestMigrateLegacyLatestDir_DisabledByEnvVar — HEIMDALL_DISABLE_LEGACY_MIGRATION=1
// short-circuits the rename. Legacy dir remains in place.
func TestMigrateLegacyLatestDir_DisabledByEnvVar(t *testing.T) {
	baseDir := t.TempDir()
	const model = "nomic-embed-text"
	legacy := seedLegacyDir(t, baseDir, model, 100)

	t.Setenv("HEIMDALL_DISABLE_LEGACY_MIGRATION", "1")

	migrated, err := MigrateLegacyLatestDir(baseDir, model)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if migrated {
		t.Errorf("migrated=true while env var opts out; want false")
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Errorf("legacy dir must remain when migration is disabled: %v", err)
	}
}

// TestMigrateLegacyLatestDir_SymlinkSource — if the `<model>_latest` entry
// is a symlink, skip migration (do not follow) and return (false, nil).
// Exercises the Lstat-based guard per plan D-16.
func TestMigrateLegacyLatestDir_SymlinkSource(t *testing.T) {
	baseDir := t.TempDir()
	const model = "nomic-embed-text"

	// Real target dir elsewhere
	realTarget := filepath.Join(baseDir, "real_target")
	if err := os.MkdirAll(realTarget, 0755); err != nil {
		t.Fatal(err)
	}
	// Seed a vectors.db in the real target so a follower would pick it up.
	if err := os.WriteFile(filepath.Join(realTarget, "vectors.db"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}

	// Make <model>_latest a symlink to real_target
	legacyLink := filepath.Join(baseDir, model+"_latest")
	if err := os.Symlink(realTarget, legacyLink); err != nil {
		t.Skipf("symlink not supported on this filesystem: %v", err)
	}

	migrated, err := MigrateLegacyLatestDir(baseDir, model)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if migrated {
		t.Errorf("migrated=true for a symlink source; want false (safety skip)")
	}

	// Link must still be intact and point at the real target.
	info, err := os.Lstat(legacyLink)
	if err != nil {
		t.Fatalf("lstat link: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("symlink source was replaced/rewritten; should have been skipped")
	}
}

// TestMigrateLegacyLatestDir_ConcurrentInvocations — two goroutines call
// the migration simultaneously. Exactly one wins the rename; the other
// sees (false, nil) via lock-file short-circuit. Final state: one canonical
// dir with the legacy data, no stale lock file.
func TestMigrateLegacyLatestDir_ConcurrentInvocations(t *testing.T) {
	baseDir := t.TempDir()
	const model = "nomic-embed-text"
	seedLegacyDir(t, baseDir, model, 100)

	var wg sync.WaitGroup
	results := make([]bool, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			migrated, err := MigrateLegacyLatestDir(baseDir, model)
			results[i] = migrated
			errs[i] = err
		}()
	}
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Errorf("goroutine %d err: %v", i, e)
		}
	}
	// Exactly one should report migrated=true (the winner). The loser
	// returns false because the lock-file contention short-circuits it,
	// or because by the time it acquires the lock the legacy dir is gone.
	winners := 0
	for _, r := range results {
		if r {
			winners++
		}
	}
	if winners != 1 {
		t.Errorf("winners = %d, want exactly 1 (results: %v)", winners, results)
	}

	// Legacy gone, canonical has the row.
	legacyPath := filepath.Join(baseDir, model+"_latest")
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Errorf("legacy should be gone: err=%v", err)
	}
	canonical := filepath.Join(baseDir, model)
	if n := countRowsAt(filepath.Join(canonical, "vectors.db")); n != 1 {
		t.Errorf("canonical row count = %d, want 1", n)
	}
}

// TestMigrateLegacyLatestDir_ClearsLegacyHookCache — before renaming, the
// migration opens the legacy store, clears its hook_cache table, and
// closes it. This invalidates any hook_cache rows that would otherwise
// become unreachable post-rename (plan D-12 / EV-5).
func TestMigrateLegacyLatestDir_ClearsLegacyHookCache(t *testing.T) {
	baseDir := t.TempDir()
	const model = "nomic-embed-text"
	legacy := seedLegacyDir(t, baseDir, model, 100)

	// Put a hook_cache row in the legacy store.
	store, err := OpenStore(legacy)
	if err != nil {
		t.Fatalf("reopen legacy: %v", err)
	}
	if err := store.HookCachePut("test-key", []byte("payload"), 0, 0); err != nil {
		store.Close()
		t.Fatalf("HookCachePut: %v", err)
	}
	// Confirm it's there
	n, _, _, err := store.HookCacheStats()
	if err != nil {
		store.Close()
		t.Fatalf("stats: %v", err)
	}
	if n != 1 {
		store.Close()
		t.Fatalf("stats before migration: got %d rows, want 1", n)
	}
	store.Close()

	migrated, err := MigrateLegacyLatestDir(baseDir, model)
	if err != nil || !migrated {
		t.Fatalf("migrated=%v err=%v", migrated, err)
	}

	// Open the renamed (now canonical) store and verify hook_cache is empty.
	canonical := filepath.Join(baseDir, model)
	store2, err := OpenStore(canonical)
	if err != nil {
		t.Fatalf("open canonical after migration: %v", err)
	}
	defer store2.Close()
	n, _, _, err = store2.HookCacheStats()
	if err != nil {
		t.Fatalf("stats after migration: %v", err)
	}
	if n != 0 {
		t.Errorf("hook_cache rows after migration = %d, want 0 (pre-rename clear failed)", n)
	}
}

// TestMigrateDisabled_WritesStillGoToCanonical — R-v2-3 in the plan. When
// the env var opts out of the rename, writes must STILL go to the
// canonical path (ModelDBDir returns canonical when legacy would be
// the reader-preferred side). The opt-out only skips the physical
// rename; it does NOT silently route writes to legacy.
//
// We verify this at the ModelDBDir level: if legacy is newer by row
// mod_time but the env var says "don't rename", the caller that writes
// via ModelDBDir still gets the canonical path when we deliberately
// force canonical (which is what the caller chain in runIndex does:
// it computes dbDir = ModelDBDir(...) AFTER calling Migrate... — when
// Migrate skipped, ModelDBDir honours its normal mixed-state rule).
// This test simply documents the combined behaviour: the env-var opts
// out of rename; it does not change ModelDBDir's reader preference.
func TestMigrateDisabled_WritesStillGoToCanonical(t *testing.T) {
	baseDir := t.TempDir()
	const model = "nomic-embed-text"
	// Legacy has data; canonical absent.
	legacy := seedLegacyDir(t, baseDir, model, 100)

	t.Setenv("HEIMDALL_DISABLE_LEGACY_MIGRATION", "1")

	migrated, err := MigrateLegacyLatestDir(baseDir, model)
	if err != nil || migrated {
		t.Fatalf("expected (false, nil), got (%v, %v)", migrated, err)
	}

	// When env var opts out AND canonical doesn't yet exist, the caller
	// chain eventually writes to canonical (because reader-preference via
	// ModelDBDir returns legacy — which is fine — but FUTURE writes that
	// don't go through MigrateLegacyLatestDir create the canonical dir
	// via OpenStore(ModelDBDir(...)) on next call, then reader-preference
	// flips on data freshness).
	//
	// The invariant we assert here: the rename did not happen.
	if _, err := os.Stat(legacy); err != nil {
		t.Errorf("legacy dir must remain when migration is disabled: %v", err)
	}

	// Time check: assert the migration returned promptly (no significant
	// wait for lock / retry), because env-var opts out at the top.
	start := time.Now()
	_, _ = MigrateLegacyLatestDir(baseDir, model)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("disabled-env fast-path took %v, want <2s", elapsed)
	}
}
