package heimdall

import (
	"database/sql"

	"github.com/caio-silva/heimdall-mcp/internal/heimdall/schema"
)

// Migrations returns the canonical list of declared schema migrations for
// the per-project vector store. The slice is the source of truth for the
// version ladder; the framework's validateMigrations() asserts strict
// ascending order at every Apply call.
//
// This is a function rather than a package-level var because the migration
// closures call back into the heimdall package (notably MigrateLegacyLatestDir,
// which itself calls OpenStore). A package-level var would create an
// initialization cycle: var Migrations refers to MigrateLegacyLatestDir,
// which refers to OpenStore, which refers to Migrations. As a function, the
// closures are constructed lazily when Migrations() is called from inside
// OpenStore — by which time all package init has completed.
//
// Add a new migration by appending an entry with the next free version.
// PreOpen migrations operate on the filesystem before sql.Open;
// SQL migrations operate on a *sql.Tx after the DB is opened.
//
// Every migration MUST be idempotent. The framework will skip versions
// already recorded in schema_migrations, but a misbehaving migration that
// crashes after partially mutating disk state must be safe to re-run.
//
// Per the Wave 2 dispatch decision (decisions.md Q2c), this PR ships only
// migration-001 (the `_latest` rename). Migrations 002 (stale registry
// repair) and 003 (sub_project backfill) are scoped to follow-up PRs.
func Migrations() []schema.Migration {
	return []schema.Migration{
		{
			Version: 1,
			Name:    "rename-model-latest-to-bare",
			Kind:    schema.KindPreOpen,
			Apply: func(c *schema.Context, _ *sql.Tx) error {
				// Skip silently when context lacks the filesystem fields the
				// migration needs (in-memory stores, tests that pass
				// non-standard dbDir into OpenStore). Forward-compatible
				// with the centralized-DB direction: the framework will
				// still record this version as applied, but the rename is
				// a no-op when there is nothing to rename.
				if c == nil || c.BaseDir == "" || c.Model == "" {
					return nil
				}
				// Delegate to the existing implementation in migrate.go
				// which already handles: env-var opt-out, symlink safety,
				// lock protection, stale-lock recovery, hook_cache
				// clearing on the legacy store, and the both-exist
				// mixed-state case (which it leaves to the reader-side
				// tiebreak in ModelDBDir).
				//
				// We intentionally ignore the (migrated bool) return
				// value: the framework only cares about (err) for
				// ordering, and a "no migration needed" outcome is still
				// a successful apply.
				_, err := MigrateLegacyLatestDir(c.BaseDir, c.Model)
				return err
			},
		},
	}
}
