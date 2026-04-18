package heimdall

import (
	"context"
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
// Also checks for the "_latest" suffixed variant on disk for backward compat.
func ModelDBDir(baseDir, model string) string {
	sanitized := sanitizeModelDirName(NormalizeModelName(model))
	dir := filepath.Join(baseDir, sanitized)

	// Check if the directory exists; if not, check the _latest variant
	// (created when Ollama reports model as "name:latest")
	if _, err := os.Stat(dir); err != nil {
		legacyDir := filepath.Join(baseDir, sanitized+"_latest")
		if _, err := os.Stat(legacyDir); err == nil {
			return legacyDir
		}
	}
	return dir
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
