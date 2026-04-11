package heimdall

import (
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
