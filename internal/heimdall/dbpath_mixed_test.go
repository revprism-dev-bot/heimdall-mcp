package heimdall

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// These tests cover the mixed-state logic added to ModelDBDir in the
// legacy-_latest migration work (PR1). The canonical <model>/ directory and
// the legacy <model>_latest/ directory may both exist on disk during the
// transition. ModelDBDir must deterministically prefer whichever dir holds
// the more recent *data* — we use MAX(mod_time) from the entries table as
// the content-aware tiebreak, with file mtime only as a fallback when
// neither DB can be opened (A-L-4 / D-19).
//
// Tests here use os.Chtimes with explicit time.Unix(100,0) / time.Unix(200,0)
// timestamps (no time.Sleep) per plan D-04. This is the first os.Chtimes
// usage in the repo — keep the idiom deterministic.

// seedModelDBWithRow opens a vector store at baseDir/<modelDirName>, writes
// one entry with the given mod_time, then closes the store so files are
// flushed. Returns the directory path so callers can os.Chtimes it.
//
// The legacy-_latest auto-migration is disabled for the duration of the
// helper so that seeding a `<model>_latest/` followed by a `<model>/` (or
// vice versa) does NOT collapse via migration-001 into a single dir. The
// reader-tiebreak tests in this file deliberately construct mixed states
// to exercise ModelDBDir's mixed-state logic; the schema framework's
// migration-001 would otherwise pre-empt that setup at the second OpenStore.
func seedModelDBWithRow(t *testing.T, baseDir, modelDirName string, rowModTime int64) string {
	t.Helper()
	t.Setenv(DisableLegacyMigrationEnv, "1")
	dir := filepath.Join(baseDir, modelDirName)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("open %s: %v", dir, err)
	}
	if err := store.Upsert([]VectorRecord{{
		ID:        "seed:" + modelDirName,
		FilePath:  "seed.go",
		Content:   "seed",
		Embedding: []float32{1, 0, 0, 0},
		ModTime:   rowModTime,
	}}); err != nil {
		store.Close()
		t.Fatalf("upsert %s: %v", dir, err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close %s: %v", dir, err)
	}
	return dir
}

// makeEmptyModelDir creates the directory with a vectors.db file but no rows.
// Used to simulate the observed bug where an empty bare directory
// shadowed a populated _latest dir. Like seedModelDBWithRow, disables the
// auto-migration so a sibling `_latest/` is preserved for the test.
func makeEmptyModelDir(t *testing.T, baseDir, modelDirName string) string {
	t.Helper()
	t.Setenv(DisableLegacyMigrationEnv, "1")
	dir := filepath.Join(baseDir, modelDirName)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("open %s: %v", dir, err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close %s: %v", dir, err)
	}
	return dir
}

// TestModelDBDir_MixedLegacyAndCurrent_PrefersRecent — when both dirs
// exist and both have rows, the dir whose entries table has the higher
// MAX(mod_time) wins. This is the content-aware tiebreak from the v2
// plan D-19 / A-L-4.
//
// Scenario: legacy `_latest` DB has a row at mod_time=200; canonical `<model>`
// DB has a row at mod_time=100. ModelDBDir must return the legacy path
// because it holds newer data.
func TestModelDBDir_MixedLegacyAndCurrent_PrefersRecent(t *testing.T) {
	baseDir := t.TempDir()
	model := "nomic-embed-text"

	// Newer data in the legacy dir (mod_time=200)
	legacyDir := seedModelDBWithRow(t, baseDir, model+"_latest", 200)
	// Older data in the canonical dir (mod_time=100)
	canonicalDir := seedModelDBWithRow(t, baseDir, model, 100)

	// File mtimes reversed relative to row mod_time to prove we're using
	// row content, not file mtime: canonical file is "newer" on disk but
	// holds older data. Per A-L-4 the correct answer is still the legacy
	// dir (higher MAX(mod_time)).
	oldT := time.Unix(100, 0)
	newT := time.Unix(200, 0)
	if err := os.Chtimes(filepath.Join(legacyDir, "vectors.db"), oldT, oldT); err != nil {
		t.Fatalf("chtimes legacy: %v", err)
	}
	if err := os.Chtimes(filepath.Join(canonicalDir, "vectors.db"), newT, newT); err != nil {
		t.Fatalf("chtimes canonical: %v", err)
	}

	got := ModelDBDir(baseDir, model)
	if got != legacyDir {
		t.Fatalf("ModelDBDir(mixed-state, legacy-newer) = %q, want %q (legacy holds newer data)", got, legacyDir)
	}
}

// TestModelDBDir_MixedState_CurrentNewer verifies the inverse: when the
// canonical dir holds newer data, it wins even if the legacy dir's file
// mtime is more recent on disk.
func TestModelDBDir_MixedState_CurrentNewer(t *testing.T) {
	baseDir := t.TempDir()
	model := "bge-m3"

	// Older data in legacy
	legacyDir := seedModelDBWithRow(t, baseDir, model+"_latest", 100)
	// Newer data in canonical
	canonicalDir := seedModelDBWithRow(t, baseDir, model, 200)

	// File mtimes reversed.
	newT := time.Unix(200, 0)
	oldT := time.Unix(100, 0)
	if err := os.Chtimes(filepath.Join(legacyDir, "vectors.db"), newT, newT); err != nil {
		t.Fatalf("chtimes legacy: %v", err)
	}
	if err := os.Chtimes(filepath.Join(canonicalDir, "vectors.db"), oldT, oldT); err != nil {
		t.Fatalf("chtimes canonical: %v", err)
	}

	got := ModelDBDir(baseDir, model)
	if got != canonicalDir {
		t.Fatalf("ModelDBDir(mixed-state, canonical-newer) = %q, want %q (canonical holds newer data)", got, canonicalDir)
	}
}

// TestModelDBDir_MixedState_EmptyCanonical_PrefersLegacy — the observed
// failure mode (B-C2). Canonical dir exists but is empty (zero rows);
// legacy dir has data. Empty canonical must NOT shadow populated legacy.
func TestModelDBDir_MixedState_EmptyCanonical_PrefersLegacy(t *testing.T) {
	baseDir := t.TempDir()
	model := "nomic-embed-text"

	legacyDir := seedModelDBWithRow(t, baseDir, model+"_latest", 100)
	// Empty canonical — just mkdir; no rows.
	_ = makeEmptyModelDir(t, baseDir, model)

	got := ModelDBDir(baseDir, model)
	if got != legacyDir {
		t.Fatalf("ModelDBDir(empty-canonical, populated-legacy) = %q, want %q", got, legacyDir)
	}
}

// TestModelDBDir_OnlyLegacyExists returns the legacy path (existing
// behaviour — preserved by v2).
func TestModelDBDir_OnlyLegacyExists(t *testing.T) {
	baseDir := t.TempDir()
	model := "nomic-embed-text"
	legacyDir := seedModelDBWithRow(t, baseDir, model+"_latest", 100)

	got := ModelDBDir(baseDir, model)
	if got != legacyDir {
		t.Fatalf("ModelDBDir(only-legacy) = %q, want %q", got, legacyDir)
	}
}

// TestModelDBDir_OnlyCanonicalExists returns canonical (the default path).
func TestModelDBDir_OnlyCanonicalExists(t *testing.T) {
	baseDir := t.TempDir()
	model := "nomic-embed-text"
	canonicalDir := seedModelDBWithRow(t, baseDir, model, 100)

	got := ModelDBDir(baseDir, model)
	if got != canonicalDir {
		t.Fatalf("ModelDBDir(only-canonical) = %q, want %q", got, canonicalDir)
	}
}

// TestModelDBDir_NeitherExists returns the canonical path (new-store
// default behaviour; creating the dir is the caller's responsibility).
func TestModelDBDir_NeitherExists(t *testing.T) {
	baseDir := t.TempDir()
	model := "nomic-embed-text"

	got := ModelDBDir(baseDir, model)
	want := filepath.Join(baseDir, model)
	if got != want {
		t.Fatalf("ModelDBDir(neither) = %q, want %q", got, want)
	}
}

// TestModelDBDir_MixedState_BothUnopenable_FallsBackToFileMtime — last-ditch
// fallback (v2 §4 Problem #1, D-19 "fallback to file mtime only if DB
// unopenable"). Create two dirs with corrupted vectors.db files, set
// different file mtimes, and assert the newer file mtime wins.
func TestModelDBDir_MixedState_BothUnopenable_FallsBackToFileMtime(t *testing.T) {
	baseDir := t.TempDir()
	model := "nomic-embed-text"

	canonicalDir := filepath.Join(baseDir, model)
	legacyDir := filepath.Join(baseDir, model+"_latest")
	if err := os.MkdirAll(canonicalDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(legacyDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Write garbage to both vectors.db files so sql.Open/PRAGMA queries fail
	// immediately and we fall through to the file-mtime branch.
	if err := os.WriteFile(filepath.Join(canonicalDir, "vectors.db"), []byte("corrupt"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "vectors.db"), []byte("corrupt"), 0644); err != nil {
		t.Fatal(err)
	}
	// legacy file is NEWER on disk.
	oldT := time.Unix(100, 0)
	newT := time.Unix(200, 0)
	if err := os.Chtimes(filepath.Join(canonicalDir, "vectors.db"), oldT, oldT); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(legacyDir, "vectors.db"), newT, newT); err != nil {
		t.Fatal(err)
	}

	got := ModelDBDir(baseDir, model)
	if got != legacyDir {
		t.Fatalf("ModelDBDir(both-corrupt, legacy-newer-mtime) = %q, want %q (file-mtime fallback)", got, legacyDir)
	}
}

// TestResolveUsableModelDB_SkipsEmptyModelDir — A-L-6. If a model dir
// exists on disk but contains no vectors.db, ResolveUsableModelDB must
// treat it as unusable. Today's implementation checks os.Stat on the
// dir, which would falsely pick an empty dir.
func TestResolveUsableModelDB_SkipsEmptyModelDir(t *testing.T) {
	baseDir := t.TempDir()

	// Create an empty model dir (no vectors.db)
	emptyDir := filepath.Join(baseDir, "nomic-embed-text")
	if err := os.MkdirAll(emptyDir, 0755); err != nil {
		t.Fatal(err)
	}

	client := stubModelLister{models: []ModelInfo{{Name: "nomic-embed-text"}}}
	dbDir, model := ResolveUsableModelDB(context.Background(), client, baseDir, "nomic-embed-text")
	if dbDir != "" || model != "" {
		t.Fatalf("ResolveUsableModelDB(empty-dir) = (%q, %q), want (\"\", \"\")", dbDir, model)
	}
}

// stubModelLister is a tiny test helper — avoids wiring a real OllamaClient.
type stubModelLister struct{ models []ModelInfo }

func (s stubModelLister) ListModels(_ context.Context) ([]ModelInfo, error) {
	return s.models, nil
}

// TestMaxModTimeFromDB_ReadsWALWrites — concurrency review CRITICAL finding.
// When a writer has un-checkpointed rows in the WAL, maxModTimeFromDB must
// still see them. Previously the function opened with `?mode=ro&immutable=1`,
// which tells SQLite to bypass the WAL and read the main DB file directly
// — under a live writer, this returns a stale snapshot.
//
// Scenario: open a store, upsert a row with mod_time=42, DO NOT close (WAL
// frames remain uncheckpointed). Call maxModTimeFromDB from a separate
// connection. With immutable=1 this returned 0/false (or a stale value);
// with the fix (plain WAL reader) it returns (42, true).
func TestMaxModTimeFromDB_ReadsWALWrites(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Hold the writer open for the whole test so any WAL pages remain
	// uncheckpointed on disk.
	t.Cleanup(func() { _ = store.Close() })

	const want int64 = 42
	if err := store.Upsert([]VectorRecord{{
		ID:        "wal:seed",
		FilePath:  "seed.go",
		Content:   "seed",
		Embedding: []float32{1, 0, 0, 0},
		ModTime:   want,
	}}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, ok := maxModTimeFromDB(filepath.Join(dir, "vectors.db"))
	if !ok {
		t.Fatalf("maxModTimeFromDB not ok — open with immutable=1 can bypass the WAL")
	}
	if got != want {
		t.Fatalf("maxModTimeFromDB = %d, want %d (reader did not see un-checkpointed WAL write)", got, want)
	}
}
