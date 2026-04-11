package heimdall

import (
	"time"
)

// LifecycleConfig controls the three-stage content lifecycle.
type LifecycleConfig struct {
	ActiveDays  int // entries accessed within this window stay active (default 30)
	ArchiveDays int // entries older than this are pruned (default 90)
	MaxChunks   int // hard cap on total entries (default 10000)
}

// LifecycleResult summarises what a lifecycle run changed.
type LifecycleResult struct {
	Archived int // entries whose content was truncated
	Pruned   int // entries deleted because they exceeded ArchiveDays
	Capped   int // entries deleted to enforce MaxChunks
}

// RunLifecycle executes the three-stage content lifecycle against the store.
//
//  1. Archive — entries past ActiveDays but within ArchiveDays have their content
//     compressed to the first 200 characters + "... [archived]".
//  2. Prune — entries past ArchiveDays are deleted.
//  3. Size cap — if total entries exceed MaxChunks, the oldest non-exempt entries
//     are deleted to bring the count back to MaxChunks.
//
// Code entries (source_type = 'code') and memory entries (kind = 'memory') are
// exempt from archiving and pruning.
func RunLifecycle(store *VectorStore, cfg LifecycleConfig) (*LifecycleResult, error) {
	store.Mu().Lock()
	defer store.Mu().Unlock()

	now := time.Now().Unix()
	archiveThreshold := now - int64(cfg.ActiveDays)*86400
	pruneThreshold := now - int64(cfg.ArchiveDays)*86400

	result := &LifecycleResult{}

	// Stage 1: Archive — compress content for stale but not yet ancient entries.
	res, err := store.DB().Exec(`
		UPDATE entries SET content = SUBSTR(content, 1, 200) || '... [archived]'
		WHERE last_accessed > 0
		  AND last_accessed < ?
		  AND last_accessed >= ?
		  AND source_type NOT IN ('code')
		  AND kind != 'memory'
		  AND LENGTH(content) > 200`,
		archiveThreshold, pruneThreshold)
	if err != nil {
		return nil, err
	}
	archived, _ := res.RowsAffected()
	result.Archived = int(archived)

	// Stage 2: Prune — delete entries that are beyond the archive window.
	res, err = store.DB().Exec(`
		DELETE FROM entries
		WHERE last_accessed > 0
		  AND last_accessed < ?
		  AND source_type NOT IN ('code')
		  AND kind != 'memory'`,
		pruneThreshold)
	if err != nil {
		return nil, err
	}
	pruned, _ := res.RowsAffected()
	result.Pruned = int(pruned)

	// Stage 3: Size cap — if entry count exceeds MaxChunks, delete oldest
	// non-exempt entries.
	if cfg.MaxChunks > 0 {
		var totalCount int
		if err := store.DB().QueryRow(`SELECT COUNT(*) FROM entries`).Scan(&totalCount); err != nil {
			return nil, err
		}

		excess := totalCount - cfg.MaxChunks
		if excess > 0 {
			res, err = store.DB().Exec(`
				DELETE FROM entries WHERE id IN (
					SELECT id FROM entries
					WHERE source_type NOT IN ('code') AND kind != 'memory'
					ORDER BY last_accessed ASC
					LIMIT ?
				)`, excess)
			if err != nil {
				return nil, err
			}
			capped, _ := res.RowsAffected()
			result.Capped = int(capped)
		}
	}

	return result, nil
}
