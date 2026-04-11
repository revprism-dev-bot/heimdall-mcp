package heimdall

import (
	"os"
	"path/filepath"
	"strings"
)

// ModelDBDir returns the model-specific subdirectory within a base .heimdall_db path.
// Sanitizes the model name for use as a directory name (replaces : and / with _).
func ModelDBDir(baseDir, model string) string {
	sanitized := strings.ReplaceAll(model, ":", "_")
	sanitized = strings.ReplaceAll(sanitized, "/", "_")
	return filepath.Join(baseDir, sanitized)
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
