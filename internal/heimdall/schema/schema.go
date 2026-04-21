// Package schema provides a versioned migration framework for heimdall's
// per-project vector store.
//
// # Contract
//
// Migrations are declared in Go code as a numbered slice (see migrations.go).
// The slice order IS the canonical order — the Version field is redundant
// but enforced at init() to catch accidental reordering. Every migration
// must be idempotent: running it twice on an already-migrated store is a
// no-op.
//
// Applied versions are persisted in the per-store schema_migrations table.
// On every OpenStore we call Apply(ctx), which scans the declared list,
// skips versions already recorded, and runs the rest in order. Each
// migration runs inside its own SQL transaction; a failure rolls back and
// Apply returns the error to the caller. The caller (OpenStore) must NOT
// return a partially-migrated store — if Apply fails, the store is closed
// and the error propagates up so the user sees a clean failure rather than
// a silent half-broken state.
//
// # Migration kinds
//
// Some migrations operate on filesystem state BEFORE the DB is opened (e.g.
// migration-001's `<model>_latest/` → `<model>/` rename). Those are
// declared as PreOpen migrations and invoked via ApplyPreOpen(dbDir, ...)
// at OpenStore's entry — after the target dbDir has been computed but
// before sql.Open. Their completion is recorded the moment the DB is
// opened and the schema_migrations table is present.
//
// SQL migrations (the common case) run post-open against a *sql.Tx and
// operate on the store's own tables.
//
// Cross-store migrations (e.g. registry repair) are SQL migrations that
// carry a RegistryAdapter in the Context; they are idempotent across
// stores, so running once per store is cheap even for the 99% case where
// there's nothing to repair.
//
// # Test helpers
//
// See testhelpers.go. The golden-path integration test (in the parent
// heimdall package) must pass before shipping any new migration; every new
// migration must ship with an entry in testdata/ demonstrating the
// pre-migration state and the expected post-migration result.
//
// # Centralized-DB forward compatibility
//
// The framework intentionally does NOT hardcode filesystem-specific
// assumptions deep in the migration API. The Context struct carries a DB
// handle and a small bundle of ambient state (dbDir, baseDir, model) that
// current migrations need. Future migrations that run server-side against
// a central DB will see the same Context minus the filesystem fields (or
// with them pointing at null). Clients of a centralized DB read the
// schema_migrations table on connect to decide whether to refuse (client
// older than server) — the version numbers carry the semantics.
package schema

import (
	"context"
	"database/sql"
	"fmt"
	"log"
)

// Context is the per-migration handle. Fields are populated by the framework
// entry points; migrations use only what they need.
type Context struct {
	// DB is the opened store DB. Always non-nil when passed to post-open
	// migrations.
	DB *sql.DB

	// DBDir is the filesystem path to the model-specific store directory
	// (e.g. ".heimdall_db/nomic-embed-text/"). Empty for in-memory stores
	// and for callers that don't provide filesystem context (tests).
	DBDir string

	// BaseDir is the parent of DBDir — the `.heimdall_db/` directory.
	// Empty when DBDir is empty.
	BaseDir string

	// Model is the embedding model name this store was built for (e.g.
	// "nomic-embed-text"). Empty when unknown.
	Model string

	// Registry is an optional adapter onto the project registry. Used by
	// migration-002 to scan/repair stale dbPaths. Nil in unit tests that
	// don't exercise registry behavior.
	Registry RegistryAdapter

	// Ctx is the Go context for cancellation.
	Ctx context.Context
}

// RegistryAdapter is the minimal surface the framework needs from the
// project registry. Kept as an interface so tests can inject fakes without
// pulling registry's on-disk format into schema's test dependencies.
type RegistryAdapter interface {
	// Projects returns a snapshot of registered projects. Never nil.
	Projects() []RegistryProject
	// Repair rewrites stale dbPaths in-place per the migration-002 contract
	// and persists. Returns the number of entries rewritten.
	Repair() (int, error)
}

// RegistryProject mirrors registry.ProjectEntry without pulling the
// registry package into schema's dependency graph.
type RegistryProject struct {
	Name   string
	Path   string
	DBPath string
}

// Kind distinguishes filesystem migrations (run before the DB is opened)
// from SQL migrations (run post-open against a transaction).
type Kind int

const (
	KindSQL     Kind = iota // runs against *sql.Tx post-open; the common case
	KindPreOpen             // runs against filesystem before sql.Open; version recorded after
)

// Migration is one declared step in the version ladder.
//
// Apply is called with a fresh Context and — for KindSQL migrations —
// a *sql.Tx that wraps the framework's per-migration transaction.
// PreOpen migrations receive a nil Tx.
type Migration struct {
	Version int
	Name    string
	Kind    Kind
	Apply   func(ctx *Context, tx *sql.Tx) error
}

// schemaMigrationsTableDDL is the one piece of DDL this package owns. Created
// on first Apply. Kept as a named constant so tests can reference it.
const schemaMigrationsTableDDL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
	version     INTEGER PRIMARY KEY,
	name        TEXT NOT NULL,
	applied_at  INTEGER NOT NULL
)`

// ApplyPreOpen runs all KindPreOpen migrations from the given list that
// are not yet recorded in the per-model pre-open log. It returns the list
// of versions that completed so the caller (OpenStore) can record them
// once the DB is opened and the schema_migrations table exists.
//
// We track pre-open state on disk (a tiny text file) rather than in the DB
// because by definition these migrations run before the DB is openable.
// On the very first run, the file is absent → every pre-open migration
// runs. Completed versions are appended. Re-reads are tolerant of
// whitespace and ordering.
//
// If any pre-open migration fails, subsequent migrations (SQL or otherwise)
// are skipped and the error propagates — do not open the store.
func ApplyPreOpen(c *Context, migrations []Migration) ([]int, error) {
	if c == nil {
		return nil, fmt.Errorf("schema: nil Context")
	}
	if err := validateMigrations(migrations); err != nil {
		return nil, err
	}
	done, err := readPreOpenLog(c.BaseDir, c.Model)
	if err != nil {
		return nil, fmt.Errorf("schema: read pre-open log: %w", err)
	}
	completed := make([]int, 0, 2)
	for _, m := range migrations {
		if m.Kind != KindPreOpen {
			continue
		}
		if done[m.Version] {
			continue
		}
		log.Printf("heimdall/schema: applying pre-open migration %d (%s)", m.Version, m.Name)
		if err := m.Apply(c, nil); err != nil {
			return completed, fmt.Errorf("schema: pre-open migration %d (%s) failed: %w", m.Version, m.Name, err)
		}
		if err := writePreOpenLog(c.BaseDir, c.Model, m.Version); err != nil {
			// The migration happened, but we can't persist the mark. Log
			// loudly — it will re-run next time (idempotent), but operators
			// deserve to see the degradation.
			log.Printf("heimdall/schema: WARN — migration %d (%s) ran but could not be recorded: %v (will re-run next open; migrations are idempotent)", m.Version, m.Name, err)
		}
		completed = append(completed, m.Version)
	}
	return completed, nil
}

// Apply runs all pending KindSQL migrations inside per-migration
// transactions, plus records any PreOpen versions passed in from
// ApplyPreOpen. Each migration runs in its own transaction. On failure,
// the transaction rolls back and Apply returns the error; callers MUST
// close the store and propagate the error to the user.
func Apply(c *Context, migrations []Migration, preOpenCompleted []int) error {
	if c == nil || c.DB == nil {
		return fmt.Errorf("schema: Apply requires Context.DB")
	}
	if err := validateMigrations(migrations); err != nil {
		return err
	}

	// Bootstrap the schema_migrations table. This is safe to run on every
	// open — CREATE TABLE IF NOT EXISTS.
	if _, err := c.DB.ExecContext(c.ctx(), schemaMigrationsTableDDL); err != nil {
		return fmt.Errorf("schema: create schema_migrations: %w", err)
	}

	// Record any pre-open completions the caller already performed.
	for _, v := range preOpenCompleted {
		name := versionName(migrations, v)
		if err := recordVersion(c.ctx(), c.DB, v, name); err != nil {
			return fmt.Errorf("schema: record pre-open v%d: %w", v, err)
		}
	}

	applied, err := readAppliedVersions(c.ctx(), c.DB)
	if err != nil {
		return fmt.Errorf("schema: read applied: %w", err)
	}

	for _, m := range migrations {
		if m.Kind != KindSQL {
			continue
		}
		if applied[m.Version] {
			continue
		}
		log.Printf("heimdall/schema: applying SQL migration %d (%s)", m.Version, m.Name)
		if err := runInTx(c, m); err != nil {
			return fmt.Errorf("schema: migration %d (%s) failed: %w", m.Version, m.Name, err)
		}
	}
	return nil
}

// runInTx runs one migration in its own transaction and records completion
// on success. A failure in the migration body rolls back the tx;
// a failure in recordVersion also rolls back (we refuse to leave the DB
// with the migration applied but unrecorded — the next open would re-run
// it, which is safe since migrations are idempotent, but the schema_migrations
// table would stay out of sync with reality).
func runInTx(c *Context, m Migration) error {
	tx, err := c.DB.BeginTx(c.ctx(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // safe no-op after Commit

	if err := m.Apply(c, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(c.ctx(),
		`INSERT OR REPLACE INTO schema_migrations (version, name, applied_at) VALUES (?, ?, strftime('%s','now'))`,
		m.Version, m.Name,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// validateMigrations checks that the declared list is strictly ascending
// and that every version is positive and unique. Catches accidental
// reordering / duplication / 0-based numbering at the earliest possible
// point (framework entry) rather than deep in a migration run.
func validateMigrations(ms []Migration) error {
	last := 0
	seen := map[int]bool{}
	for i, m := range ms {
		if m.Version <= 0 {
			return fmt.Errorf("schema: migrations[%d].Version=%d must be positive", i, m.Version)
		}
		if seen[m.Version] {
			return fmt.Errorf("schema: duplicate migration version %d", m.Version)
		}
		if m.Version <= last {
			return fmt.Errorf("schema: migration %d (%s) out of order (previous=%d)", m.Version, m.Name, last)
		}
		if m.Apply == nil {
			return fmt.Errorf("schema: migration %d (%s) has nil Apply", m.Version, m.Name)
		}
		if m.Name == "" {
			return fmt.Errorf("schema: migration %d has empty Name", m.Version)
		}
		seen[m.Version] = true
		last = m.Version
	}
	return nil
}

// ctx returns the Context's Go ctx, defaulting to context.Background.
func (c *Context) ctx() context.Context {
	if c.Ctx == nil {
		return context.Background()
	}
	return c.Ctx
}

// readAppliedVersions returns the set of migration versions currently
// recorded in schema_migrations. Missing table → empty set.
func readAppliedVersions(ctx context.Context, db *sql.DB) (map[int]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}

func recordVersion(ctx context.Context, db *sql.DB, version int, name string) error {
	_, err := db.ExecContext(ctx,
		`INSERT OR REPLACE INTO schema_migrations (version, name, applied_at) VALUES (?, ?, strftime('%s','now'))`,
		version, name,
	)
	return err
}

// versionName looks up the declared name for a version within the given
// list. Returns fmt.Sprintf("v%d", version) if not found — safe default
// for forward compatibility (an older binary may not know the name of a
// newer version, but it can still stamp the row).
func versionName(ms []Migration, version int) string {
	for _, m := range ms {
		if m.Version == version {
			return m.Name
		}
	}
	return fmt.Sprintf("v%d", version)
}

// AppliedVersions is a diagnostics helper. Returns the applied versions in
// ascending order. Exposed for status tools + tests.
func AppliedVersions(ctx context.Context, db *sql.DB) ([]int, error) {
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// DeclaredVersions returns the declared migration versions in order from
// the given list. Used by tests to assert the framework sees the expected
// migrations.
func DeclaredVersions(ms []Migration) []int {
	out := make([]int, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Version)
	}
	return out
}
