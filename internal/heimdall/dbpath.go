package heimdall

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// NormalizeModelName strips Ollama's implicit ":latest" tag so that model
// identifiers are comparable regardless of whether the caller or Ollama
// included the tag.
func NormalizeModelName(model string) string {
	return strings.TrimSuffix(model, ":latest")
}

// sanitizeModelDirName turns a normalized model name into a value usable as a
// directory name. Keeps ModelDBDir and friends in sync.
func sanitizeModelDirName(model string) string {
	s := strings.ReplaceAll(model, ":", "_")
	s = strings.ReplaceAll(s, "/", "_")
	return s
}

// ModelDBDir returns the model-specific subdirectory within a base .heimdall_db path.
// Strips the ":latest" tag (it's the default and config never includes it),
// then sanitizes for use as a directory name (replaces : and / with _).
//
// Mixed-state rule (v2 plan D-19 / A-L-4): when both the canonical `<model>/`
// dir and the legacy `<model>_latest/` dir exist, pick whichever holds
// the more recent *data* — using MAX(mod_time) from the entries table as
// a content-aware tiebreak. When neither DB can be opened (e.g. corrupt
// files), fall back to file mtime. This prevents an empty bare dir from
// shadowing a populated legacy dir.
//
// Callers that perform writes should route through MigrateLegacyLatestDir
// first (see migrate.go) so the mixed state becomes single-state after
// the first write on new binaries.
func ModelDBDir(baseDir, model string) string {
	sanitized := sanitizeModelDirName(NormalizeModelName(model))
	dir := filepath.Join(baseDir, sanitized)
	legacyDir := filepath.Join(baseDir, sanitized+"_latest")

	canonicalExists := false
	legacyExists := false
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		canonicalExists = true
	}
	if info, err := os.Stat(legacyDir); err == nil && info.IsDir() {
		legacyExists = true
	}

	switch {
	case canonicalExists && legacyExists:
		// Both exist — disambiguate by data recency.
		if pickLegacyOverCanonical(legacyDir, dir) {
			return legacyDir
		}
		return dir
	case legacyExists:
		return legacyDir
	default:
		// Either canonical exists or neither does; always return canonical.
		return dir
	}
}

// pickLegacyOverCanonical decides which of two existing model dirs
// (legacy `<model>_latest/` vs canonical `<model>/`) should be returned
// by ModelDBDir. Primary signal: MAX(mod_time) from the entries table
// (content-aware). Fallback: file mtime on vectors.db. Ties go to the
// canonical dir (so a fresh binary deterministically prefers the
// non-legacy location once data equalizes).
func pickLegacyOverCanonical(legacyDir, canonicalDir string) bool {
	legacyRows, legacyOK := maxModTimeFromDB(filepath.Join(legacyDir, "vectors.db"))
	canonicalRows, canonicalOK := maxModTimeFromDB(filepath.Join(canonicalDir, "vectors.db"))

	// If at least one DB is queryable, compare by row mod_time.
	if legacyOK || canonicalOK {
		// A zero-row DB counts as mod_time=0. If the other side has any
		// rows, the populated side wins (fixes the "empty canonical
		// shadows legacy" bug from handoff Problem #1).
		if legacyRows > canonicalRows {
			return true
		}
		return false
	}

	// Both unopenable — fall back to file mtime.
	return fileMtimeNewer(
		filepath.Join(legacyDir, "vectors.db"),
		filepath.Join(canonicalDir, "vectors.db"),
	)
}

// maxModTimeFromDB opens a vectors.db read-only, queries
// `SELECT COALESCE(MAX(mod_time), 0) FROM entries`, and returns the
// value. Returns (0, false) if the DB can't be opened or queried (e.g.
// missing file, corrupt, or table absent). A fresh store with no rows
// returns (0, true).
//
// DSN matches the writer side (`store.go:OpenStore`): `_journal_mode=WAL`
// + `_busy_timeout=5000`. DO NOT add `immutable=1` — per SQLite docs,
// immutable=1 instructs the driver that the file will not change and
// allows it to bypass the WAL, which would return a stale snapshot when
// a concurrent writer has un-checkpointed rows in the WAL. The reader
// acquires a shared lock on the WAL; writer contention is handled by
// SQLite's own busy-timeout.
func maxModTimeFromDB(dbPath string) (int64, bool) {
	if _, err := os.Stat(dbPath); err != nil {
		return 0, false
	}
	db, err := sql.Open("sqlite", dbPath+"?mode=ro&_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return 0, false
	}
	defer db.Close()

	var v int64
	if err := db.QueryRow(`SELECT COALESCE(MAX(mod_time), 0) FROM entries`).Scan(&v); err != nil {
		return 0, false
	}
	return v, true
}

// fileMtimeNewer reports whether `a` has a more recent mtime than `b`.
// Used as the last-resort tiebreak in pickLegacyOverCanonical when
// neither DB can be opened. Ties fall to canonical (returns false).
func fileMtimeNewer(a, b string) bool {
	ai, aerr := os.Stat(a)
	bi, berr := os.Stat(b)
	if aerr != nil && berr != nil {
		return false
	}
	if aerr != nil {
		return false
	}
	if berr != nil {
		return true
	}
	return ai.ModTime().After(bi.ModTime())
}

// ListAvailableModels returns the model names that have indexes in the base dir.
// Each entry is a directory name under baseDir that contains a vectors.db file.
func ListAvailableModels(baseDir string) []string {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return nil
	}
	var models []string
	for _, e := range entries {
		if e.IsDir() {
			dbFile := filepath.Join(baseDir, e.Name(), "vectors.db")
			if _, err := os.Stat(dbFile); err == nil {
				models = append(models, e.Name())
			}
		}
	}
	return models
}

// modelLister is the minimal subset of *OllamaClient needed by
// ResolveUsableModelDB — splitting it out keeps the helper testable without
// a live Ollama instance.
type modelLister interface {
	ListModels(ctx context.Context) ([]ModelInfo, error)
}

// ResolveUsableModelDB picks a model index under baseDir whose model is
// actually pulled in Ollama. It prefers preferredModel when both an index and
// a pulled model exist for it; otherwise it returns the first available index
// whose backing model is pulled. Returns ("", "") if Ollama is unreachable or
// no pulled model has an index.
func ResolveUsableModelDB(ctx context.Context, client modelLister, baseDir, preferredModel string) (string, string) {
	available := ListAvailableModels(baseDir)
	if len(available) == 0 {
		return "", ""
	}

	ollamaModels, err := client.ListModels(ctx)
	if err != nil {
		return "", ""
	}

	pulled := make(map[string]bool, len(ollamaModels)*2)
	for _, m := range ollamaModels {
		pulled[m.Name] = true
		pulled[NormalizeModelName(m.Name)] = true
	}

	// Prefer the configured model if it has an index and is pulled.
	if preferredModel != "" {
		configDir := ModelDBDir(baseDir, preferredModel)
		if _, err := os.Stat(configDir); err == nil && pulled[NormalizeModelName(preferredModel)] {
			return configDir, preferredModel
		}
	}

	// Otherwise find any available index whose model is pulled.
	for _, dirName := range available {
		dbDir := filepath.Join(baseDir, dirName)
		modelName := strings.TrimSuffix(dirName, "_latest")
		if pulled[modelName] {
			return dbDir, modelName
		}
	}

	return "", ""
}

// DiscoverSubReposAbs returns the absolute paths of immediate subdirectories
// of root that contain a `.git` entry (directory or file — `.git` may be a
// gitlink in worktrees).
//
// Unlike DiscoverSubRepos (which silently returns an empty map on a read
// failure), DiscoverSubReposAbs propagates the underlying ReadDir error so
// callers can distinguish "no sub-repos" from "couldn't scan". See L1 in the
// plan review.
//
// Symlinks are NOT followed — this matches the industry-standard default
// (git, fd, ripgrep) and prevents the double-indexing hazard where
// `outer-a/child` and `outer-b/child` both resolve to the same git repo
// via a symlink. Any entry whose Type includes os.ModeSymlink is ignored
// even if it would resolve to a directory with a `.git/` inside.
func DiscoverSubReposAbs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read sub-repo root %s: %w", root, err)
	}
	var out []string
	for _, entry := range entries {
		if !entry.IsDir() {
			// Skips regular files. On Unix entry.IsDir is lstat-based, so
			// symlinks already return false here — the explicit check below
			// is defense in depth for platforms that behave differently.
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		gitPath := filepath.Join(root, entry.Name(), ".git")
		if _, err := os.Stat(gitPath); err == nil {
			out = append(out, filepath.Join(root, entry.Name()))
		}
	}
	return out, nil
}

// DiscoverSubRepos scans immediate subdirectories of root and returns the set
// of directory names that contain their own .git/ directory. The result is a
// map keyed by directory name (not full path) so callers can do O(1) lookups.
// Returns an empty map if root cannot be read.
func DiscoverSubRepos(root string) map[string]bool {
	result := make(map[string]bool)
	entries, err := os.ReadDir(root)
	if err != nil {
		return result
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		gitDir := filepath.Join(root, entry.Name(), ".git")
		// Accept .git as either a directory (standard repo) OR a regular file
		// (gitlink used by worktrees). Matches hasRepoMarker in scope.go.
		if _, err := os.Stat(gitDir); err == nil {
			result[entry.Name()] = true
		}
	}
	return result
}
