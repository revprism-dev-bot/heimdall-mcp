package heimdall

import (
	"database/sql"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

// MaxMemoriesForSemanticDedup is the threshold above which semantic dedup
// is skipped to avoid loading too many vectors into memory.
const MaxMemoriesForSemanticDedup = 10000

// MemoryStore manages the SQLite-backed memory database.
// This is separate from VectorStore to avoid schema pollution.
type MemoryStore struct {
	db *sql.DB
	mu sync.RWMutex
}

// OpenMemoryStore opens or creates a memory store at the given path.
func OpenMemoryStore(dbPath string) (*MemoryStore, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS memories (
			id TEXT PRIMARY KEY,
			content TEXT NOT NULL,
			type TEXT NOT NULL DEFAULT 'fact',
			tags TEXT,
			project TEXT,
			vector BLOB NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			source TEXT DEFAULT 'explicit',
			content_hash TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_memories_type ON memories(type);
		CREATE INDEX IF NOT EXISTS idx_memories_project ON memories(project);
		CREATE INDEX IF NOT EXISTS idx_memories_source ON memories(source);
		CREATE INDEX IF NOT EXISTS idx_memories_content_hash ON memories(content_hash);
	`); err != nil {
		db.Close()
		return nil, err
	}

	return &MemoryStore{db: db}, nil
}

// Close closes the underlying database connection.
func (s *MemoryStore) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// UpsertMemory inserts or replaces a memory by ID.
func (s *MemoryStore) UpsertMemory(m Memory) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tagsJSON, _ := json.Marshal(m.Tags)
	vecBlob := EncodeFloat32Vec(m.Vector)

	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO memories (id, content, type, tags, project, vector, created_at, updated_at, source, content_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, m.ID, m.Content, string(m.Type), string(tagsJSON), m.Project, vecBlob, m.CreatedAt, m.UpdatedAt, string(m.Source), m.ContentHash)
	return err
}

// SearchMemories returns the top-k most similar memories to the query vector,
// with optional post-filtering.
func (s *MemoryStore) SearchMemories(query []float32, topK int, filters MemoryFilter) []MemorySearchResult {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// SAFETY: All filter values are passed as parameterized arguments (?),
	// never interpolated into the SQL string.
	sqlQuery := `SELECT id, content, type, tags, project, vector, created_at, updated_at, source, content_hash FROM memories`
	var conditions []string
	var args []any
	if filters.Type != "" {
		conditions = append(conditions, `type = ?`)
		args = append(args, string(filters.Type))
	}
	if filters.Project != "" {
		conditions = append(conditions, `project = ?`)
		args = append(args, filters.Project)
	}
	if filters.Source != "" {
		conditions = append(conditions, `source = ?`)
		args = append(args, string(filters.Source))
	}
	if len(conditions) > 0 {
		sqlQuery += ` WHERE ` + strings.Join(conditions, ` AND `)
	}

	rows, err := s.db.Query(sqlQuery, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var results []MemorySearchResult
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			continue
		}

		// Tags filter is post-scan (stored as JSON, not indexed)
		if len(filters.Tags) > 0 && !hasAnyTag(m.Tags, filters.Tags) {
			continue
		}

		sim := CosineSimilarity(query, m.Vector)
		results = append(results, MemorySearchResult{Memory: m, Similarity: sim})
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Similarity > results[j].Similarity
	})
	if topK > 0 && len(results) > topK {
		results = results[:topK]
	}
	return results
}

// FindSimilarMemory finds the most similar existing memory above the threshold.
// Returns nil if none found or if memory count exceeds the semantic dedup cap.
func (s *MemoryStore) FindSimilarMemory(vector []float32, threshold float64) (*Memory, float64, error) {
	if s.MemoryCount() > MaxMemoriesForSemanticDedup {
		return nil, 0, nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`SELECT id, content, type, tags, project, vector, created_at, updated_at, source, content_hash FROM memories`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var bestMemory *Memory
	bestSim := 0.0

	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			continue
		}
		sim := CosineSimilarity(vector, m.Vector)
		if sim > threshold && sim > bestSim {
			bestSim = sim
			mCopy := m
			bestMemory = &mCopy
		}
	}

	return bestMemory, bestSim, nil
}

// FindSimilarMemoryFromCache finds the most similar memory from a pre-loaded
// vector cache. Used during batch ingestion to avoid repeated DB queries.
func (s *MemoryStore) FindSimilarMemoryFromCache(vector []float32, threshold float64, cache []memoryVectorEntry) (string, float64) {
	bestID := ""
	bestSim := 0.0
	for _, entry := range cache {
		sim := CosineSimilarity(vector, entry.Vector)
		if sim > threshold && sim > bestSim {
			bestSim = sim
			bestID = entry.ID
		}
	}
	return bestID, bestSim
}

// GetMemoryByHash looks up a memory by its content hash.
func (s *MemoryStore) GetMemoryByHash(hash string) (*Memory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	row := s.db.QueryRow(`SELECT id, content, type, tags, project, vector, created_at, updated_at, source, content_hash FROM memories WHERE content_hash = ?`, hash)

	var m Memory
	var tagsJSON string
	var vecBlob []byte
	var memType, source string

	err := row.Scan(&m.ID, &m.Content, &memType, &tagsJSON, &m.Project, &vecBlob, &m.CreatedAt, &m.UpdatedAt, &source, &m.ContentHash)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	m.Type = MemoryType(memType)
	m.Source = MemorySource(source)
	m.Vector = DecodeFloat32Vec(vecBlob)
	if tagsJSON != "" {
		if err := json.Unmarshal([]byte(tagsJSON), &m.Tags); err != nil {
			log.Printf("heimdall: failed to unmarshal tags for memory %s: %v", m.ID, err)
		}
	}
	return &m, nil
}

// GetMemoryByID looks up a memory by its ID.
func (s *MemoryStore) GetMemoryByID(id string) (*Memory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	row := s.db.QueryRow(`SELECT id, content, type, tags, project, vector, created_at, updated_at, source, content_hash FROM memories WHERE id = ?`, id)

	var m Memory
	var tagsJSON string
	var vecBlob []byte
	var memType, source string

	err := row.Scan(&m.ID, &m.Content, &memType, &tagsJSON, &m.Project, &vecBlob, &m.CreatedAt, &m.UpdatedAt, &source, &m.ContentHash)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	m.Type = MemoryType(memType)
	m.Source = MemorySource(source)
	m.Vector = DecodeFloat32Vec(vecBlob)
	if tagsJSON != "" {
		if err := json.Unmarshal([]byte(tagsJSON), &m.Tags); err != nil {
			log.Printf("heimdall: failed to unmarshal tags for memory %s: %v", m.ID, err)
		}
	}
	return &m, nil
}

// MemoryStats returns counts of memories grouped by type, source, and project.
func (s *MemoryStore) MemoryStats() MemoryStoreStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	stats := MemoryStoreStats{
		ByType:    make(map[string]int),
		BySource:  make(map[string]int),
		ByProject: make(map[string]int),
	}

	s.db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&stats.TotalMemories)

	rows, err := s.db.Query(`SELECT type, COUNT(*) FROM memories GROUP BY type`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var t string
			var c int
			if rows.Scan(&t, &c) == nil {
				stats.ByType[t] = c
			}
		}
	}

	rows2, err := s.db.Query(`SELECT source, COUNT(*) FROM memories GROUP BY source`)
	if err == nil {
		defer rows2.Close()
		for rows2.Next() {
			var src string
			var c int
			if rows2.Scan(&src, &c) == nil {
				stats.BySource[src] = c
			}
		}
	}

	rows3, err := s.db.Query(`SELECT COALESCE(project, ''), COUNT(*) FROM memories GROUP BY project`)
	if err == nil {
		defer rows3.Close()
		for rows3.Next() {
			var p string
			var c int
			if rows3.Scan(&p, &c) == nil {
				if p == "" {
					p = "(global)"
				}
				stats.ByProject[p] = c
			}
		}
	}

	return stats
}

// LoadAllMemoryVectors loads all memory IDs and vectors for batch dedup.
func (s *MemoryStore) LoadAllMemoryVectors() ([]memoryVectorEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`SELECT id, vector FROM memories`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []memoryVectorEntry
	for rows.Next() {
		var id string
		var vecBlob []byte
		if err := rows.Scan(&id, &vecBlob); err != nil {
			continue
		}
		entries = append(entries, memoryVectorEntry{
			ID:     id,
			Vector: DecodeFloat32Vec(vecBlob),
		})
	}
	return entries, nil
}

// MemoryCount returns the total number of stored memories.
func (s *MemoryStore) MemoryCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var count int
	s.db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&count)
	return count
}

// scanMemory scans a single memory row from a *sql.Rows.
func scanMemory(rows *sql.Rows) (Memory, error) {
	var m Memory
	var tagsJSON string
	var vecBlob []byte
	var memType, source string

	err := rows.Scan(&m.ID, &m.Content, &memType, &tagsJSON, &m.Project, &vecBlob, &m.CreatedAt, &m.UpdatedAt, &source, &m.ContentHash)
	if err != nil {
		return m, err
	}

	m.Type = MemoryType(memType)
	m.Source = MemorySource(source)
	m.Vector = DecodeFloat32Vec(vecBlob)
	if tagsJSON != "" {
		if err := json.Unmarshal([]byte(tagsJSON), &m.Tags); err != nil {
			log.Printf("heimdall: failed to unmarshal tags for memory %s: %v", m.ID, err)
		}
	}
	return m, nil
}

// hasAnyTag returns true if memTags contains any of the filter tags.
func hasAnyTag(memTags, filterTags []string) bool {
	tagSet := make(map[string]bool, len(memTags))
	for _, t := range memTags {
		tagSet[t] = true
	}
	for _, t := range filterTags {
		if tagSet[t] {
			return true
		}
	}
	return false
}
