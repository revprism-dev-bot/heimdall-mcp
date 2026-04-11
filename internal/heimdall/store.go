package heimdall

import (
	"database/sql"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"

	_ "modernc.org/sqlite"
)

// VectorRecord is a single indexed chunk with its embedding.
type VectorRecord struct {
	ID          string    `json:"id"`
	FilePath    string    `json:"filePath"`
	StartLine   int       `json:"startLine"`
	EndLine     int       `json:"endLine"`
	Content     string    `json:"content"`
	Kind        string    `json:"kind"`
	Identifier  string    `json:"identifier"`
	Embedding   []float32 `json:"embedding"`
	ModTime     int64     `json:"modTime"`
	ContentHash string    `json:"contentHash"`
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
	mu sync.RWMutex
	db *sql.DB
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

	return &VectorStore{db: db}, nil
}

// Close closes the underlying database connection.
func (s *VectorStore) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// Search returns the top-k most similar records to the query vector.
func (s *VectorStore) Search(query []float32, topK int) []SearchResult {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`SELECT id, file_path, start_line, end_line, content, kind, identifier, vector, mod_time FROM entries`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var rec VectorRecord
		var vecBlob []byte
		var kind, identifier sql.NullString
		if err := rows.Scan(&rec.ID, &rec.FilePath, &rec.StartLine, &rec.EndLine, &rec.Content, &kind, &identifier, &vecBlob, &rec.ModTime); err != nil {
			continue
		}
		rec.Kind = kind.String
		rec.Identifier = identifier.String
		rec.Embedding = decodeFloat32Vec(vecBlob)
		sim := cosineSimilarity(query, rec.Embedding)
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
		INSERT OR REPLACE INTO entries (id, file_path, start_line, end_line, content, kind, identifier, vector, mod_time, content_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, r := range records {
		vecBlob := encodeFloat32Vec(r.Embedding)
		if _, err := stmt.Exec(r.ID, r.FilePath, r.StartLine, r.EndLine, r.Content, r.Kind, r.Identifier, vecBlob, r.ModTime, r.ContentHash); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// RemoveByFile removes all records for the given file path.
func (s *VectorStore) RemoveByFile(filePath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`DELETE FROM entries WHERE file_path = ?`, filePath)
	return err
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

// encodeFloat32Vec serializes a []float32 to a compact binary blob.
func encodeFloat32Vec(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

// decodeFloat32Vec deserializes a binary blob back to []float32.
func decodeFloat32Vec(b []byte) []float32 {
	n := len(b) / 4
	v := make([]float32, n)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}

// cosineSimilarity computes the cosine similarity between two vectors.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}
