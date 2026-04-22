package schema

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
)

// SeedFixtureDB creates a SQLite DB at dbPath with the legacy pre-schema-N
// entries table (no schema_migrations, no sub_project column etc.). It is
// a deliberately minimal fixture writer used by integration tests to
// simulate a store from before migration-N shipped.
//
// kind controls which shape to seed:
//   - "pre-v1": no schema_migrations table; entries table present; rows
//     have empty sub_project (pre-migration-003 state).
//   - "empty": fresh schema, no entries. Useful for idempotency checks.
//
// Callers own the resulting DB and are responsible for removing the
// parent dir (typically t.TempDir()).
//
// The helper intentionally does NOT create `<model>_latest/` variants
// here — migration-001 fixture directories are set up by the test itself
// since they concern directory layout, not DB rows. See
// migrate_framework_integration_test.go in the parent package for the
// canonical example.
func SeedFixtureDB(dbPath, kind string, rows []FixtureEntry) error {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return err
	}
	defer db.Close()

	switch kind {
	case "pre-v1":
		// Create the same entries schema used by production OpenStore,
		// minus the schema_migrations table. sub_project column is
		// present but all rows seeded with ''.
		if _, err := db.Exec(`
			CREATE TABLE IF NOT EXISTS entries (
				id TEXT PRIMARY KEY,
				file_path TEXT NOT NULL,
				start_line INTEGER,
				end_line INTEGER,
				content TEXT NOT NULL,
				kind TEXT,
				identifier TEXT,
				vector BLOB NOT NULL,
				mod_time INTEGER NOT NULL,
				content_hash TEXT NOT NULL DEFAULT '',
				source_type TEXT DEFAULT 'code',
				metadata TEXT DEFAULT '{}',
				relationships TEXT DEFAULT '[]',
				summary TEXT DEFAULT '',
				context_path TEXT DEFAULT '',
				last_accessed INTEGER DEFAULT 0,
				sub_project TEXT DEFAULT ''
			);
			CREATE INDEX IF NOT EXISTS idx_file_path ON entries(file_path);
			CREATE INDEX IF NOT EXISTS idx_source_type ON entries(source_type);
			CREATE INDEX IF NOT EXISTS idx_context_path ON entries(context_path);
			CREATE INDEX IF NOT EXISTS idx_sub_project ON entries(sub_project);
			CREATE TABLE IF NOT EXISTS store_metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL);
			CREATE TABLE IF NOT EXISTS hook_cache (
				key        TEXT PRIMARY KEY,
				stdout     BLOB NOT NULL,
				created_at INTEGER NOT NULL,
				hit_count  INTEGER NOT NULL DEFAULT 0
			);
			CREATE INDEX IF NOT EXISTS idx_hook_cache_created ON hook_cache(created_at);
		`); err != nil {
			return err
		}
	case "empty":
		// Fresh schema — seeded via the production OpenStore path in the
		// caller. Helper just creates the file.
	default:
		return fmt.Errorf("schema.SeedFixtureDB: unknown kind %q", kind)
	}

	// Seed rows (if any).
	for _, r := range rows {
		if _, err := db.Exec(
			`INSERT INTO entries (id, file_path, start_line, end_line, content, vector, mod_time, sub_project)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ID, r.FilePath, r.StartLine, r.EndLine, r.Content, r.Vector, r.ModTime, r.SubProject,
		); err != nil {
			return err
		}
	}
	return nil
}

// FixtureEntry is a minimal row shape for pre-migration seeding. It
// intentionally doesn't cover every column — migrations care about
// specific columns, and a test that cares about more can SeedFixtureDB
// then UPDATE additional columns directly.
type FixtureEntry struct {
	ID         string
	FilePath   string
	StartLine  int
	EndLine    int
	Content    string
	Vector     []byte // encoded float32 vector; tests typically use a zero-length []byte
	ModTime    int64
	SubProject string
}

// StaticRegistry is a fake RegistryAdapter that stores an in-memory list
// of projects and counts repair calls. Useful for asserting that a
// migration invoked Repair() without touching the real registry file.
type StaticRegistry struct {
	entries    []RegistryProject
	Calls      int
	RepairedBy int
	RepairErr  error
}

func NewStaticRegistry(entries []RegistryProject) *StaticRegistry {
	return &StaticRegistry{entries: entries}
}

func (r *StaticRegistry) Projects() []RegistryProject {
	return r.entries
}

func (r *StaticRegistry) Repair() (int, error) {
	r.Calls++
	return r.RepairedBy, r.RepairErr
}
