package heimdall

import (
	"database/sql"
	"errors"
	"strconv"
	"time"
)

// MaxHookCacheStdoutBytes is the hard refusal ceiling for a single hook_cache
// row's stdout payload. Callers asking to cache anything larger receive
// ErrHookCacheOversize and the row is not inserted. Sized so one "## Heimdall
// suggests" block stays within a Tier B Claude Code budget.
const MaxHookCacheStdoutBytes = 32 * 1024

// ErrHookCacheMiss is returned from HookCacheGet when no fresh row matches
// the key (either nothing stored or the row is older than the requested TTL).
var ErrHookCacheMiss = errors.New("heimdall: hook cache miss")

// ErrHookCacheOversize is returned from HookCachePut when the stdout payload
// exceeds MaxHookCacheStdoutBytes. The row is not written.
var ErrHookCacheOversize = errors.New("heimdall: hook cache payload exceeds 32KB")

// indexVersionKey is the store_metadata key that holds a monotonic counter
// bumped exactly once per successful Upsert / RemoveByFile transaction. Hook
// cache keys include this value so a write to the index auto-invalidates
// every affected cache row on next read (the next key computation will
// differ).
const indexVersionKey = "index_version"

// bumpIndexVersionTx atomically increments the index_version metadata key
// inside an open transaction using a single SQL statement. Must be called
// from Upsert / RemoveByFile so the bump and the data write share one commit
// boundary — otherwise readers can observe stale-key cache hits against
// freshly-written data.
//
// PERF-002: Uses INSERT ... ON CONFLICT to do an atomic read-modify-write in
// one round-trip instead of the previous SELECT-then-INSERT OR REPLACE pair.
func bumpIndexVersionTx(tx *sql.Tx) error {
	_, err := tx.Exec(`
		INSERT INTO store_metadata (key, value) VALUES (?, '1')
		ON CONFLICT(key) DO UPDATE SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT)
	`, indexVersionKey)
	return err
}

// GetIndexVersion returns the current monotonic index_version counter. A
// fresh store returns 0. Safe for concurrent callers.
func (s *VectorStore) GetIndexVersion() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var raw string
	s.db.QueryRow(`SELECT value FROM store_metadata WHERE key = ?`, indexVersionKey).Scan(&raw)
	if raw == "" {
		return 0
	}
	v, _ := strconv.ParseInt(raw, 10, 64)
	return v
}

// HookCachePut inserts or replaces a hook_cache row. Payloads larger than
// MaxHookCacheStdoutBytes are refused. After insert, if the row count
// exceeds rowCap, oldest-by-created_at rows are evicted until the count
// is back within cap. rowCap <= 0 disables the cap.
//
// ttl is accepted for symmetry with HookCacheGet but is not stored —
// freshness is enforced on read by the ttl parameter of HookCacheGet.
//
// PERF-003: Row count is tracked incrementally in s.hookCacheRows instead
// of running SELECT COUNT(*) on every insert. The counter is lazily
// initialized on first call (hookCacheRows == -1) and adjusted for
// inserts, replacements, and evictions.
func (s *VectorStore) HookCachePut(key string, stdout []byte, ttl time.Duration, rowCap int) error {
	if len(stdout) > MaxHookCacheStdoutBytes {
		return ErrHookCacheOversize
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Lazy-initialize the row counter on first call.
	if s.hookCacheRows < 0 {
		var count int64
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM hook_cache`).Scan(&count); err != nil {
			return err
		}
		s.hookCacheRows = count
	}

	// Check whether this key already exists so we know whether the INSERT OR
	// REPLACE will be a net-new row (increment counter) or a replacement
	// (counter stays the same).
	var exists int64
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM hook_cache WHERE key = ?`, key,
	).Scan(&exists); err != nil {
		return err
	}

	now := time.Now().Unix()
	// hit_count resets on replace — this row's stdout has changed, so old
	// accounting no longer applies to the new payload.
	if _, err := s.db.Exec(
		`INSERT OR REPLACE INTO hook_cache (key, stdout, created_at, hit_count) VALUES (?, ?, ?, 0)`,
		key, stdout, now,
	); err != nil {
		return err
	}
	_ = ttl

	if exists == 0 {
		s.hookCacheRows++
	}

	if rowCap > 0 && s.hookCacheRows > int64(rowCap) {
		excess := s.hookCacheRows - int64(rowCap)
		res, err := s.db.Exec(
			`DELETE FROM hook_cache WHERE key IN (
				SELECT key FROM hook_cache ORDER BY created_at ASC LIMIT ?
			)`, excess,
		)
		if err != nil {
			return err
		}
		if evicted, err := res.RowsAffected(); err == nil {
			s.hookCacheRows -= evicted
		}
	}
	return nil
}

// HookCacheGet returns the cached stdout for the given key, or
// ErrHookCacheMiss if nothing fresh is stored. A hit increments hit_count
// on a best-effort basis. A stored row older than ttl is reported as a
// miss (the row is left in place — eviction happens via rowCap or
// explicit clear).
//
// The lookup runs under RLock so concurrent cache-hit readers don't
// serialize behind each other; the hit_count bump takes a brief write
// lock afterward and its error is intentionally swallowed so a failed
// bump cannot mask a successful read.
func (s *VectorStore) HookCacheGet(key string, ttl time.Duration) ([]byte, error) {
	s.mu.RLock()
	var stdout []byte
	var createdAt int64
	err := s.db.QueryRow(
		`SELECT stdout, created_at FROM hook_cache WHERE key = ?`, key,
	).Scan(&stdout, &createdAt)
	s.mu.RUnlock()

	if err == sql.ErrNoRows {
		return nil, ErrHookCacheMiss
	}
	if err != nil {
		return nil, err
	}

	if ttl > 0 {
		age := time.Since(time.Unix(createdAt, 0))
		if age > ttl {
			return nil, ErrHookCacheMiss
		}
	}

	s.mu.Lock()
	_, _ = s.db.Exec(
		`UPDATE hook_cache SET hit_count = hit_count + 1 WHERE key = ?`, key,
	)
	s.mu.Unlock()

	return stdout, nil
}

// HookCacheClear removes every row from the hook_cache table. Returns an
// error only on driver-level failure. Resets the incremental row counter.
func (s *VectorStore) HookCacheClear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM hook_cache`)
	if err == nil {
		s.hookCacheRows = 0
	}
	return err
}

// HookCacheStats reports row count, total stdout bytes, and aggregate hit
// count for the hook_cache table. Used by `heimdall-mcp hooks cache-stats`.
func (s *VectorStore) HookCacheStats() (count int64, totalBytes int64, hitCount int64, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	err = s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(LENGTH(stdout)), 0), COALESCE(SUM(hit_count), 0) FROM hook_cache`,
	).Scan(&count, &totalBytes, &hitCount)
	return
}
