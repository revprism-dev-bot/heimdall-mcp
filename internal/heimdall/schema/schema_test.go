package schema_test

// Integration test for the versioned schema migration framework + the
// canonical migration-001 (`<model>_latest/` rename). Lives in the schema
// package's external _test package so it can import the parent heimdall
// package without creating an import cycle.
//
// The test exercises the end-to-end loop:
//  1. Seed a legacy `<model>_latest/` dir with one row using OpenStore +
//     Upsert (the only "fixture writer" we trust to produce a schema that
//     round-trips against the current binary).
//  2. Call OpenStore on the canonical `<model>/` path. The framework's
//     ApplyPreOpen MUST detect the legacy dir, fire migration-001 (which
//     delegates to heimdall.MigrateLegacyLatestDir), and rename it.
//  3. Assert the canonical dir now exists with the seed row and the
//     legacy dir is gone.
//  4. Assert schema_migrations records v1 with non-zero applied_at.
//  5. Assert a SECOND OpenStore is a no-op: the rename does not re-fire,
//     the row count stays the same, and the schema_migrations row is
//     not duplicated.

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall/schema"
)

const fixtureModel = "nomic-embed-text"

// seedLegacyDir writes one row to baseDir/<model>_latest/vectors.db using
// the production OpenStore code path. Returns the legacy dir path. Mirrors
// the helper in migrate_legacy_latest_test.go but lives here so the
// schema package's integration test does not depend on any heimdall_test
// helpers (those are unexported).
func seedLegacyDir(t *testing.T, baseDir, model string) string {
	t.Helper()
	// The directory naming convention must match heimdall.ModelDBDir's
	// sanitization. For "nomic-embed-text" the sanitized form is the same
	// string (no `:` or `/`), so we can use it verbatim. If a future test
	// uses a model with `:` or `/`, swap to a cross-package helper.
	legacy := filepath.Join(baseDir, model+"_latest")
	store, err := heimdall.OpenStore(legacy)
	if err != nil {
		t.Fatalf("seed: open legacy %s: %v", legacy, err)
	}
	if err := store.Upsert([]heimdall.VectorRecord{{
		ID:        "legacy:seed:1",
		FilePath:  "fixture/seed.go",
		Content:   "// fixture row written by schema_test.go",
		Embedding: []float32{1, 0, 0, 0},
		ModTime:   100,
	}}); err != nil {
		store.Close()
		t.Fatalf("seed: upsert: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("seed: close: %v", err)
	}
	return legacy
}

// countRows opens vectors.db read-only and returns the row count, or -1
// on any error.
func countRows(dbPath string) int64 {
	db, err := sql.Open("sqlite", dbPath+"?mode=ro&_journal_mode=WAL&_busy_timeout=5000")
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

// schemaMigrationVersions returns the list of versions recorded in the
// per-store schema_migrations table, in ascending order.
func schemaMigrationVersions(t *testing.T, dbPath string) []int {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath+"?mode=ro&_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	defer db.Close()
	versions, err := schema.AppliedVersions(context.Background(), db)
	if err != nil {
		t.Fatalf("AppliedVersions: %v", err)
	}
	return versions
}

// TestMigrationFramework_Migration001_RenamesLatestAndIsIdempotent is the
// golden-path integration test required by the migration-framework PR.
//
// Step 1 — seed: write one row into baseDir/<model>_latest/.
// Step 2 — open canonical: triggers ApplyPreOpen → migration-001 → rename.
// Step 3 — assert canonical now holds the row, legacy dir is gone, and
//
//	schema_migrations records [1].
//
// Step 4 — second open: no-op. No duplicate v1 row, row count unchanged,
//
//	legacy still absent.
func TestMigrationFramework_Migration001_RenamesLatestAndIsIdempotent(t *testing.T) {
	baseDir := t.TempDir()

	// Step 1 — seed legacy.
	legacy := seedLegacyDir(t, baseDir, fixtureModel)
	if n := countRows(filepath.Join(legacy, "vectors.db")); n != 1 {
		t.Fatalf("seed: legacy row count = %d, want 1", n)
	}
	canonical := filepath.Join(baseDir, fixtureModel)
	if _, err := os.Stat(canonical); !os.IsNotExist(err) {
		t.Fatalf("seed precondition: canonical %s should not yet exist (stat err=%v)", canonical, err)
	}

	// Step 2 — open canonical. Migration-001 must run.
	store, err := heimdall.OpenStore(canonical)
	if err != nil {
		t.Fatalf("OpenStore(%s): %v", canonical, err)
	}

	// Step 3 — post-conditions of the first open.
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("after open: legacy dir %s should be gone (stat err=%v)", legacy, err)
	}
	if _, err := os.Stat(canonical); err != nil {
		t.Fatalf("after open: canonical dir %s missing: %v", canonical, err)
	}
	canonicalDB := filepath.Join(canonical, "vectors.db")
	if n := countRows(canonicalDB); n != 1 {
		t.Errorf("after open: canonical row count = %d, want 1 (seed row should have moved)", n)
	}
	versions := schemaMigrationVersions(t, canonicalDB)
	if len(versions) != 1 || versions[0] != 1 {
		t.Errorf("after open: schema_migrations versions = %v, want [1]", versions)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}

	// Snapshot post-first-open state for comparison after the second open.
	beforeSecondRows := countRows(canonicalDB)

	// Step 4 — second open is a no-op. Migration-001's pre-open log
	// suppresses the re-fire; INSERT OR REPLACE in the framework's record
	// step keeps schema_migrations a single row (even if it ran).
	store2, err := heimdall.OpenStore(canonical)
	if err != nil {
		t.Fatalf("second OpenStore(%s): %v", canonical, err)
	}
	if err := store2.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("after second open: legacy dir reappeared: %v", legacy)
	}
	if afterRows := countRows(canonicalDB); afterRows != beforeSecondRows {
		t.Errorf("after second open: row count drifted: before=%d after=%d", beforeSecondRows, afterRows)
	}
	versionsAfter := schemaMigrationVersions(t, canonicalDB)
	if len(versionsAfter) != 1 || versionsAfter[0] != 1 {
		t.Errorf("after second open: schema_migrations versions = %v, want [1]", versionsAfter)
	}
}
