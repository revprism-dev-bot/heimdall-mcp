package heimdall

import (
	"database/sql"
	"log"
	"os"
	"path/filepath"
)

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
