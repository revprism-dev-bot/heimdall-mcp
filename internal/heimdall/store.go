package heimdall

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/heimdall/schema"

	_ "modernc.org/sqlite"
)

// SubProjectRoot is the reserved sentinel that callers pass as sub_project
// to restrict a search to outer/wrapper chunks only (rows whose sub_project
// is NULL or ”). Empty string continues to mean "no filter". Per locked
// OQ-c / R-v2-1 the registry rejects sub-repo names matching ^__[a-z]+__$
// to prevent collision with this sentinel.
const SubProjectRoot = "__root__"

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
	Summary       string    `json:"summary"`       // one-line heuristic summary for L0 tiered retrieval
	ContextPath   string    `json:"contextPath"`   // hierarchical path for scoped retrieval (e.g. "src/api/auth")
	LastAccessed  int64     `json:"lastAccessed"`  // unix timestamp of last search hit
	SubProject    string    `json:"subProject"`    // sub-repo directory name (empty = root project)
}

// SearchResult is a record with its similarity score.
type SearchResult struct {
	Record     VectorRecord
	Similarity float64
}

// SearchOption is a functional option for SearchFiltered.
type SearchOption func(*searchConfig)

type searchConfig struct {
	scopePath string // context_path prefix filter (empty = no filter)
	detail    string // "summary", "snippet", "full" (empty = "full")
}

// WithScope restricts results to entries whose context_path starts with the given prefix.
func WithScope(path string) SearchOption {
	return func(c *searchConfig) { c.scopePath = path }
}

// WithDetail controls the level of content returned: "summary" (one-line), "snippet" (first 200 chars), "full".
func WithDetail(level string) SearchOption {
	return func(c *searchConfig) { c.detail = level }
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

	// hookCacheRows tracks the current number of rows in hook_cache so that
	// HookCachePut can decide whether to evict without running SELECT COUNT(*)
	// on every insert. The value is loaded lazily on first HookCachePut (when
	// it is -1) and maintained incrementally thereafter. Protected by mu.
	// PERF-003.
	hookCacheRows int64
}

// OpenStore loads or creates a vector store at the given directory.
// The store file is dbDir/vectors.db.
//
// As part of opening, the per-project versioned migration framework runs:
//   - PreOpen migrations (e.g. the legacy `<model>_latest/` rename) fire
//     BEFORE MkdirAll so a freshly-created bare dir does not block a
//     pending rename.
//   - Post-open SQL migrations and the recording of completed PreOpen
//     versions happen after the DB is opened and the bootstrap tables are
//     present.
//
// If any migration fails, OpenStore closes the partially-opened store and
// returns the error — callers MUST NOT receive a half-migrated handle.
func OpenStore(dbDir string) (*VectorStore, error) {
	// Run PreOpen migrations BEFORE creating the dir. Migration-001 wants
	// to rename `<model>_latest/` → `<model>/` which is impossible if we
	// MkdirAll the canonical dir first.
	//
	// We derive (BaseDir, Model) from dbDir using the convention
	// dbDir = <baseDir>/<sanitizedModel>. Callers that pass a non-conventional
	// dbDir (e.g. raw t.TempDir() in unit tests) just see migration-001
	// no-op — the rename precondition is "<baseDir>/<model>_latest exists",
	// which fails and the migration returns nil cleanly.
	sctx := &schema.Context{
		BaseDir: filepath.Dir(dbDir),
		Model:   filepath.Base(dbDir),
	}
	migrations := Migrations()
	preOpenCompleted, err := schema.ApplyPreOpen(sctx, migrations)
	if err != nil {
		return nil, err
	}

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

	// Migrate: add summary column for tiered retrieval (L0 one-line summary).
	db.Exec(`ALTER TABLE entries ADD COLUMN summary TEXT DEFAULT ''`)

	// Migrate: add context_path column for path-based scoping.
	db.Exec(`ALTER TABLE entries ADD COLUMN context_path TEXT DEFAULT ''`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_context_path ON entries(context_path)`)

	// Migrate: add last_accessed column for content lifecycle tracking.
	db.Exec(`ALTER TABLE entries ADD COLUMN last_accessed INTEGER DEFAULT 0`)

	// Migrate: add sub_project column for sub-repo tagging.
	db.Exec(`ALTER TABLE entries ADD COLUMN sub_project TEXT DEFAULT ''`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_sub_project ON entries(sub_project)`)

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

	// Run versioned schema migrations (records any PreOpen versions that
	// just completed; runs pending KindSQL migrations in order). On
	// failure we close the DB and propagate the error so callers never
	// receive a partially-migrated store.
	sctx.DB = db
	if err := schema.Apply(sctx, migrations, preOpenCompleted); err != nil {
		db.Close()
		return nil, err
	}

	return &VectorStore{db: db, hookCacheRows: -1}, nil
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
func (s *VectorStore) Search(ctx context.Context, query []float32, topK int) []SearchResult {
	return s.SearchFiltered(ctx, query, topK, "", "", nil)
}

// SearchFiltered is the single search implementation. Supports optional source_type
// and sub_project pre-filters (SQL WHERE) and metadata post-filter (JSON comparison).
//
// ctx is honored at three points: the SQL QueryContext call (lets the driver
// cancel a mid-flight statement), a pre-loop check, and per-row in the scan
// loop (large indexes can take meaningful time in Scan+DecodeFloat32Vec+
// CosineSimilarity). If ctx fires mid-loop the partial result set is returned
// rather than nil — the caller can still surface whatever we managed to score
// before the deadline. A nil ctx is treated as context.Background() so this
// stays callable from test helpers that don't care about cancellation.
func (s *VectorStore) SearchFiltered(ctx context.Context, query []float32, topK int, sourceType string, subProject string, metadataFilter map[string]any, opts ...SearchOption) []SearchResult {
	if ctx == nil {
		ctx = context.Background()
	}

	var cfg searchConfig
	for _, o := range opts {
		o(&cfg)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	// SAFETY: All filter values are passed as parameterized arguments (?),
	// never interpolated into the SQL string. The dynamic WHERE clause
	// only appends fixed column-name conditions.
	sqlQuery := `SELECT id, file_path, start_line, end_line, content, kind, identifier, vector, mod_time, source_type, metadata, relationships, summary, context_path, last_accessed, sub_project FROM entries`
	var conditions []string
	var args []any
	if sourceType != "" {
		conditions = append(conditions, `source_type = ?`)
		args = append(args, sourceType)
	}
	if subProject != "" {
		// SubProjectRoot sentinel: "__root__" filters to outer/wrapper
		// chunks only (rows whose sub_project is empty OR NULL). This is
		// the locked OQ-c semantic — empty string continues to mean "no
		// filter" (back-compat), but callers who want wrapper-only hits
		// can now express that intent. Reserved sentinel names matching
		// ^__[a-z]+__$ are rejected at registry.Register time to avoid
		// collision with a sub-repo literally named "__root__". See
		// handoff Problem #2 / plan OQ-c / R-v2-1.
		if subProject == SubProjectRoot {
			conditions = append(conditions, `(sub_project IS NULL OR sub_project = '')`)
		} else {
			conditions = append(conditions, `sub_project = ?`)
			args = append(args, subProject)
		}
	}
	if cfg.scopePath != "" {
		conditions = append(conditions, `context_path LIKE ? || '%'`)
		args = append(args, cfg.scopePath)
	}
	if len(conditions) > 0 {
		sqlQuery += ` WHERE ` + strings.Join(conditions, ` AND `)
	}

	rows, err := s.db.QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		if ctx.Err() != nil {
			break
		}
		var rec VectorRecord
		var vecBlob []byte
		var kind, identifier, srcType, meta, rels, summary, ctxPath, subProj sql.NullString
		var lastAccessed sql.NullInt64
		if err := rows.Scan(&rec.ID, &rec.FilePath, &rec.StartLine, &rec.EndLine, &rec.Content,
			&kind, &identifier, &vecBlob, &rec.ModTime, &srcType, &meta, &rels, &summary, &ctxPath, &lastAccessed, &subProj); err != nil {
			continue
		}
		rec.Kind = kind.String
		rec.Identifier = identifier.String
		rec.SourceType = srcType.String
		rec.Metadata = meta.String
		rec.Relationships = rels.String
		rec.Summary = summary.String
		rec.ContextPath = ctxPath.String
		rec.LastAccessed = lastAccessed.Int64
		rec.SubProject = subProj.String
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

	// Apply detail level to trim content for token savings.
	switch cfg.detail {
	case "summary":
		for i := range results {
			if results[i].Record.Summary != "" {
				results[i].Record.Content = results[i].Record.Summary
			} else if len(results[i].Record.Content) > 120 {
				results[i].Record.Content = results[i].Record.Content[:120] + "..."
			}
		}
	case "snippet":
		for i := range results {
			if len(results[i].Record.Content) > 200 {
				results[i].Record.Content = results[i].Record.Content[:200] + "..."
			}
		}
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
		INSERT OR REPLACE INTO entries (id, file_path, start_line, end_line, content, kind, identifier, vector, mod_time, content_hash, source_type, metadata, relationships, summary, context_path, sub_project)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
		summary := r.Summary
		if summary == "" {
			summary = GenerateSummary(r.Content, r.Kind, r.Identifier)
		}
		contextPath := r.ContextPath
		if contextPath == "" {
			contextPath = deriveContextPath(r.FilePath)
		}
		if _, err := stmt.Exec(r.ID, r.FilePath, r.StartLine, r.EndLine, content, r.Kind, r.Identifier, vecBlob, r.ModTime, r.ContentHash, sourceType, metadata, relationships, summary, contextPath, r.SubProject); err != nil {
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

// VectorByID returns the embedding vector for the given chunk id. Used by
// the semantic-drift compute path to cosine-compare an assistant's
// heimdall_search query vector against the vectors of hits the hook
// injected earlier in the same turn (see
// docs/plans/hooks/12-semantic-drift-metric.md §3.1).
//
// Returns sql.ErrNoRows when the id is not present — the caller treats
// that as an F3 fail-open skip (the index was reindexed between hook fire
// and report time). Unlike ExpandByID this SELECT only pulls the vector
// blob: drift lookups happen per-hit and would dominate SQLite wall time
// if we fetched every column.
func (s *VectorStore) VectorByID(chunkID string) ([]float32, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var vecBlob []byte
	err := s.db.QueryRow(`SELECT vector FROM entries WHERE id = ?`, chunkID).Scan(&vecBlob)
	if err != nil {
		return nil, err
	}
	return DecodeFloat32Vec(vecBlob), nil
}

// ExpandByID returns the full content for a given chunk ID.
// Used by the heimdall_expand tool for tiered retrieval drill-down.
func (s *VectorStore) ExpandByID(chunkID string) (*VectorRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	row := s.db.QueryRow(`SELECT id, file_path, start_line, end_line, content, kind, identifier, source_type, metadata, relationships, summary, context_path, sub_project FROM entries WHERE id = ?`, chunkID)

	var rec VectorRecord
	var kind, identifier, srcType, meta, rels, summary, ctxPath, subProj sql.NullString
	err := row.Scan(&rec.ID, &rec.FilePath, &rec.StartLine, &rec.EndLine, &rec.Content,
		&kind, &identifier, &srcType, &meta, &rels, &summary, &ctxPath, &subProj)
	if err != nil {
		return nil, err
	}
	rec.Kind = kind.String
	rec.Identifier = identifier.String
	rec.SourceType = srcType.String
	rec.Metadata = meta.String
	rec.Relationships = rels.String
	rec.Summary = summary.String
	rec.ContextPath = ctxPath.String
	rec.SubProject = subProj.String
	return &rec, nil
}

// ListByContextPath returns distinct context_path prefixes at the given depth.
// Used by the heimdall_ls tool for filesystem-style navigation.
func (s *VectorStore) ListByContextPath(prefix string) []PathEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var entries []PathEntry

	// Count direct entries at this prefix
	sqlQ := `SELECT context_path, COUNT(*) as cnt FROM entries WHERE context_path LIKE ? || '%' GROUP BY context_path`
	rows, err := s.db.Query(sqlQ, prefix)
	if err != nil {
		return nil
	}
	defer rows.Close()

	children := make(map[string]int)
	prefixLen := len(prefix)
	for rows.Next() {
		var path string
		var count int
		if rows.Scan(&path, &count) != nil {
			continue
		}
		// Find the next path segment after the prefix
		rest := path[prefixLen:]
		rest = strings.TrimPrefix(rest, "/")
		if rest == "" {
			continue
		}
		seg := rest
		if idx := strings.Index(rest, "/"); idx >= 0 {
			seg = rest[:idx]
		}
		children[seg] += count
	}

	for name, count := range children {
		childPath := prefix + name
		if prefix != "" && !strings.HasSuffix(prefix, "/") {
			childPath = prefix + "/" + name
		}
		entries = append(entries, PathEntry{Name: name, Path: childPath, ChunkCount: count})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries
}

// PathEntry represents one child in a context path listing.
type PathEntry struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	ChunkCount int    `json:"chunkCount"`
}

// GenerateSummary produces a heuristic one-line summary for tiered retrieval.
func GenerateSummary(content, kind, identifier string) string {
	if identifier != "" {
		switch kind {
		case "function":
			return kind + " " + identifier
		case "type":
			return kind + " " + identifier
		default:
			if kind != "" {
				return kind + " " + identifier
			}
			return identifier
		}
	}
	// For content without an identifier, take the first non-empty line
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "/*") {
			continue
		}
		if len(line) > 120 {
			line = line[:120] + "..."
		}
		return line
	}
	if len(content) > 120 {
		return content[:120] + "..."
	}
	return content
}

// deriveContextPath computes a hierarchical context_path from a file path.
// Strips the file extension and filename, keeping the directory hierarchy.
func deriveContextPath(filePath string) string {
	dir := filepath.Dir(filePath)
	if dir == "." || dir == "" {
		return ""
	}
	return filepath.ToSlash(dir)
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

// BackfillSubProject sets the sub_project column to name for every row in
// the store whose sub_project is currently NULL or ” (the default for
// stores indexed before Problem #2 was fixed). It is safe to call on a
// store that already has fully-populated sub_project values — it is a
// no-op (idempotent).
//
// The helper exists specifically for legacy sub-repo stores written by
// pre-fix binaries: those rows carry ” (or NULL) and the post-fix search
// filter (`sub_project = ?`) returns zero hits. Running this helper once
// at the top of a sub-repo indexing pass promotes the existing rows into
// the post-fix tagging regime without a full reindex.
//
// name must be non-empty. An empty name is rejected rather than silently
// doing nothing, because every legitimate backfill caller has a concrete
// sub-repo name to stamp (IndexSubRepos passes filepath.Base(subPath)).
//
// Returns the number of rows updated. See TestBackfillSubProject_*.
func (s *VectorStore) BackfillSubProject(name string) (int64, error) {
	if name == "" {
		return 0, os.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(
		`UPDATE entries SET sub_project = ? WHERE sub_project IS NULL OR sub_project = ''`,
		name,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
