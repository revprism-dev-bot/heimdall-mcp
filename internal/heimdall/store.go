package heimdall

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// VectorRecord is a single indexed chunk with its embedding.
type VectorRecord struct {
	ID            string    `json:"id"`
	FilePath      string    `json:"filePath"`
	StartLine     int       `json:"startLine"`
	EndLine       int       `json:"endLine"`
	Content       string    `json:"content"`
	Kind          string    `json:"kind"`
	Identifier    string    `json:"identifier"`
	Embedding     []float32 `json:"embedding"`
	ModTime       int64     `json:"modTime"`
	ContentHash   string    `json:"contentHash"`
	SourceType    string    `json:"sourceType"`    // "code", "ticket", "doc", "pr", etc.
	Metadata      string    `json:"metadata"`      // JSON object string
	Relationships string    `json:"relationships"` // JSON array string
	LastAccessed  int64     `json:"lastAccessed"`  // unix timestamp of last search hit
}

// SearchResult is a record with its similarity score.
type SearchResult struct {
	Record     VectorRecord
	Similarity float64
}

// StoreStats holds index statistics.
type StoreStats struct {
	TotalRecords int
	TotalFiles   int
	LastModified int64
}

// VectorStore manages the SQLite-backed vector database.
type VectorStore struct {
	mu               sync.RWMutex
	db               *sql.DB
	lastLifecycleRun int64 // unix timestamp, protected by mu
}

// OpenStore loads or creates a vector store at the given directory.
// The store file is dbDir/vectors.db.
func OpenStore(dbDir string) (*VectorStore, error) {
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(dbDir, "vectors.db")

	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}

	// Create tables if they don't exist.
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
			content_hash TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_file_path ON entries(file_path);
	`); err != nil {
		db.Close()
		return nil, err
	}

	// Migrate: add content_hash column if missing (existing DBs).
	db.Exec(`ALTER TABLE entries ADD COLUMN content_hash TEXT NOT NULL DEFAULT ''`)

	// Migrate: add typed context columns if missing (existing DBs).
	db.Exec(`ALTER TABLE entries ADD COLUMN source_type TEXT DEFAULT 'code'`)
	db.Exec(`ALTER TABLE entries ADD COLUMN metadata TEXT DEFAULT '{}'`)
	db.Exec(`ALTER TABLE entries ADD COLUMN relationships TEXT DEFAULT '[]'`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_source_type ON entries(source_type)`)

	// Migrate: add last_accessed column for content lifecycle tracking.
	db.Exec(`ALTER TABLE entries ADD COLUMN last_accessed INTEGER DEFAULT 0`)

	// Metadata table for store-level properties (model, dimensions, etc.)
	db.Exec(`CREATE TABLE IF NOT EXISTS store_metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL)`)

	// hook_cache: BLOB-valued short-TTL cache keyed by normalized prompt +
	// index version + filter scope. Co-located with the per-model store so
	// every row is implicitly scoped to one embedding space. See
	// docs/plans/hooks §5.3 for the rationale.
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS hook_cache (
			key        TEXT PRIMARY KEY,
			stdout     BLOB NOT NULL,
			created_at INTEGER NOT NULL,
			hit_count  INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS idx_hook_cache_created ON hook_cache(created_at);
	`); err != nil {
		db.Close()
		return nil, err
	}

	return &VectorStore{db: db}, nil
}

// SetMetadata stores a key-value pair in the store metadata table.
func (s *VectorStore) SetMetadata(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT OR REPLACE INTO store_metadata (key, value) VALUES (?, ?)`, key, value)
	return err
}

// GetMetadata retrieves a value from the store metadata table.
// Returns empty string if the key doesn't exist.
func (s *VectorStore) GetMetadata(key string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var value string
	s.db.QueryRow(`SELECT value FROM store_metadata WHERE key = ?`, key).Scan(&value)
	return value
}

// Close closes the underlying database connection.
func (s *VectorStore) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// Search is a convenience wrapper — delegates to SearchFiltered with no filters.
func (s *VectorStore) Search(query []float32, topK int) []SearchResult {
	return s.SearchFiltered(query, topK, "", nil)
}

// SearchFiltered is the single search implementation. Supports optional source_type
// pre-filter (SQL WHERE) and metadata post-filter (JSON comparison).
func (s *VectorStore) SearchFiltered(query []float32, topK int, sourceType string, metadataFilter map[string]any) []SearchResult {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sqlQuery := `SELECT id, file_path, start_line, end_line, content, kind, identifier, vector, mod_time, source_type, metadata, relationships, last_accessed FROM entries`
	var args []any
	if sourceType != "" {
		sqlQuery += ` WHERE source_type = ?`
		args = append(args, sourceType)
	}

	rows, err := s.db.Query(sqlQuery, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var rec VectorRecord
		var vecBlob []byte
		var kind, identifier, srcType, meta, rels sql.NullString
		var lastAccessed sql.NullInt64
		if err := rows.Scan(&rec.ID, &rec.FilePath, &rec.StartLine, &rec.EndLine, &rec.Content,
			&kind, &identifier, &vecBlob, &rec.ModTime, &srcType, &meta, &rels, &lastAccessed); err != nil {
			continue
		}
		rec.Kind = kind.String
		rec.Identifier = identifier.String
		rec.SourceType = srcType.String
		rec.Metadata = meta.String
		rec.Relationships = rels.String
		rec.LastAccessed = lastAccessed.Int64
		rec.Embedding = DecodeFloat32Vec(vecBlob)

		// Post-filter: metadata match
		if len(metadataFilter) > 0 && !matchesMetadata(rec.Metadata, metadataFilter) {
			continue
		}

		sim := CosineSimilarity(query, rec.Embedding) * freshnessWeight(rec.LastAccessed)
		results = append(results, SearchResult{Record: rec, Similarity: sim})
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Similarity > results[j].Similarity
	})
	if topK > 0 && len(results) > topK {
		results = results[:topK]
	}
	return results
}

// matchesMetadata checks if the record's JSON metadata contains all key-value pairs in the filter.
// Uses JSON serialization for type-safe comparison (no fmt.Sprintf coercion).
func matchesMetadata(metadataJSON string, filter map[string]any) bool {
	if metadataJSON == "" || metadataJSON == "{}" {
		return false
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(metadataJSON), &meta); err != nil {
		return false
	}
	for k, v := range filter {
		val, ok := meta[k]
		if !ok {
			return false
		}
		expected, _ := json.Marshal(v)
		actual, _ := json.Marshal(val)
		if !bytes.Equal(expected, actual) {
			return false
		}
	}
	return true
}

// Upsert adds or updates records. Records with matching IDs are replaced.
func (s *VectorStore) Upsert(records []VectorRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT OR REPLACE INTO entries (id, file_path, start_line, end_line, content, kind, identifier, vector, mod_time, content_hash, source_type, metadata, relationships)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, r := range records {
		// Sanitize content: strip null bytes (SEC-10)
		content := strings.ReplaceAll(r.Content, "\x00", "")
		vecBlob := EncodeFloat32Vec(r.Embedding)
		sourceType := r.SourceType
		if sourceType == "" {
			sourceType = "code"
		}
		metadata := r.Metadata
		if metadata == "" {
			metadata = "{}"
		}
		relationships := r.Relationships
		if relationships == "" {
			relationships = "[]"
		}
		if _, err := stmt.Exec(r.ID, r.FilePath, r.StartLine, r.EndLine, content, r.Kind, r.Identifier, vecBlob, r.ModTime, r.ContentHash, sourceType, metadata, relationships); err != nil {
			return err
		}
	}

	if err := bumpIndexVersionTx(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveByFile removes all records for the given file path.
func (s *VectorStore) RemoveByFile(filePath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM entries WHERE file_path = ?`, filePath); err != nil {
		return err
	}
	if err := bumpIndexVersionTx(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Save is a no-op for SQLite (auto-persists). Kept for API compatibility.
func (s *VectorStore) Save() error {
	return nil
}

// Stats returns index statistics.
func (s *VectorStore) Stats() StoreStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var stats StoreStats

	s.db.QueryRow(`SELECT COUNT(*) FROM entries`).Scan(&stats.TotalRecords)
	s.db.QueryRow(`SELECT COUNT(DISTINCT file_path) FROM entries`).Scan(&stats.TotalFiles)
	s.db.QueryRow(`SELECT COALESCE(MAX(mod_time), 0) FROM entries`).Scan(&stats.LastModified)

	return stats
}

// HasFile returns true if the store has any entries for the given file path.
func (s *VectorStore) HasFile(filePath string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var count int
	s.db.QueryRow(`SELECT COUNT(*) FROM entries WHERE file_path = ? LIMIT 1`, filePath).Scan(&count)
	return count > 0
}

// ContentHashForFile returns the content_hash stored for a file, or "" if
// no records exist or the hash was never populated.
func (s *VectorStore) ContentHashForFile(filePath string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var hash string
	s.db.QueryRow(`SELECT content_hash FROM entries WHERE file_path = ? AND content_hash != '' LIMIT 1`, filePath).Scan(&hash)
	return hash
}

// MaxModTimeForFile returns the maximum mod_time for records matching the
// given file path, or 0 if no records exist for that file.
func (s *VectorStore) MaxModTimeForFile(filePath string) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var modTime int64
	s.db.QueryRow(`SELECT COALESCE(MAX(mod_time), 0) FROM entries WHERE file_path = ?`, filePath).Scan(&modTime)
	return modTime
}

// SnippetBySource returns the first 200 characters of content for entries
// matching the given file_path (source identifier).
func (s *VectorStore) SnippetBySource(source string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var content string
	s.db.QueryRow(`SELECT content FROM entries WHERE file_path = ? LIMIT 1`, source).Scan(&content)
	if len(content) > 200 {
		content = content[:200] + "..."
	}
	return content
}

// UpdateLastAccessed batch-updates the last_accessed timestamp to the current
// unix time for the given entry IDs.
func (s *VectorStore) UpdateLastAccessed(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().Unix()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1] // trim trailing comma

	args := make([]any, 0, len(ids)+1)
	args = append(args, now)
	for _, id := range ids {
		args = append(args, id)
	}

	_, err = tx.Exec(`UPDATE entries SET last_accessed = ? WHERE id IN (`+placeholders+`)`, args...)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// freshnessWeight applies a linear decay from 1.0 to 0.5 over 90 days based
// on the last_accessed timestamp. Entries that have never been accessed
// (last_accessed <= 0) are treated as fresh and receive no penalty.
func freshnessWeight(lastAccessed int64) float64 {
	if lastAccessed <= 0 {
		return 1.0 // never accessed = treat as fresh (new content)
	}
	age := float64(time.Now().Unix() - lastAccessed)
	if age <= 0 {
		return 1.0
	}
	const maxAge = float64(90 * 24 * 3600) // 90 days in seconds
	if age >= maxAge {
		return 0.5
	}
	// Linear decay from 1.0 to 0.5 over 90 days
	return 1.0 - 0.5*(age/maxAge)
}

// ShouldRunLifecycle returns true if at least one hour has elapsed since the
// last lifecycle run. Safe for concurrent use.
func (s *VectorStore) ShouldRunLifecycle() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return time.Now().Unix()-s.lastLifecycleRun > 3600
}

// MarkLifecycleRun records that a lifecycle run just completed.
func (s *VectorStore) MarkLifecycleRun() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastLifecycleRun = time.Now().Unix()
}

// DB exposes the underlying *sql.DB for lifecycle operations.
// This is intentionally package-internal — only lifecycle.go uses it.
func (s *VectorStore) DB() *sql.DB {
	return s.db
}

// Mu exposes the store mutex for lifecycle operations that need
// write-level coordination with the store.
func (s *VectorStore) Mu() *sync.RWMutex {
	return &s.mu
}
