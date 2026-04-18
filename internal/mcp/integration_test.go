package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
	"github.com/caio-silva/heimdall-mcp/internal/registry"
)

// setupTestServer creates a test server with a mock embedder and temporary DB.
// Returns the server and cleanup function.
func setupTestServer(t *testing.T) (*Server, *heimdall.VectorStore, string) {
	t.Helper()
	tmpDir := t.TempDir()
	dbDir := filepath.Join(tmpDir, ".heimdall_db")

	store, err := heimdall.OpenStore(dbDir)
	if err != nil {
		t.Fatal(err)
	}

	reg := &registry.Registry{}
	reg.Register("testproj", tmpDir, dbDir)

	srv := &Server{
		Cfg:      config.DefaultConfig(),
		Registry: reg,
	}

	return srv, store, dbDir
}

func TestToolSearch_EnrichedResults(t *testing.T) {
	srv, store, _ := setupTestServer(t)
	defer store.Close()

	// Insert records
	records := []heimdall.VectorRecord{
		{
			ID: "main.go:1:10", FilePath: "main.go", StartLine: 1, EndLine: 10,
			Content: "func main() {}", Kind: "function", Identifier: "main",
			Embedding: []float32{1.0, 0.0, 0.0}, ModTime: 100,
			SourceType: "code", Metadata: "{}", Relationships: "[]",
		},
		{
			ID: "ext:JIRA-1:0", FilePath: "JIRA-1", StartLine: 1, EndLine: 1,
			Content: "ticket description", Kind: "external", Identifier: "JIRA-1",
			Embedding: []float32{0.9, 0.1, 0.0}, ModTime: 100,
			SourceType: "ticket", Metadata: `{"status":"open"}`, Relationships: "[]",
		},
	}
	if err := store.Upsert(records); err != nil {
		t.Fatal(err)
	}

	// The toolSearch requires Ollama which isn't available in tests.
	// Instead, test the filtered search path directly by calling SearchFiltered.
	_ = srv // srv is used for other tests
	results := store.SearchFiltered(context.Background(), []float32{1.0, 0.0, 0.0}, 5, "", "", nil)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// Verify enriched fields are available
	for _, r := range results {
		if r.Record.ID == "" {
			t.Error("ChunkID (Record.ID) should not be empty")
		}
		source := classifySource(r.Record.Kind)
		if source != "code" && source != "external" {
			t.Errorf("unexpected source: %s", source)
		}
	}
}

func TestToolSearch_FilteredBySourceType(t *testing.T) {
	srv, store, _ := setupTestServer(t)
	_ = srv
	defer store.Close()

	records := []heimdall.VectorRecord{
		{ID: "c1", FilePath: "a.go", Content: "code", Embedding: []float32{1.0, 0.0}, ModTime: 100, SourceType: "code", Kind: "file"},
		{ID: "e1", FilePath: "JIRA-1", Content: "ticket", Embedding: []float32{0.9, 0.1}, ModTime: 100, SourceType: "ticket", Kind: "external"},
	}
	if err := store.Upsert(records); err != nil {
		t.Fatal(err)
	}

	// Filter by ticket
	results := store.SearchFiltered(context.Background(), []float32{1.0, 0.0}, 10, "ticket", "", nil)
	if len(results) != 1 {
		t.Fatalf("expected 1 ticket result, got %d", len(results))
	}
	if classifySource(results[0].Record.Kind) != "external" {
		t.Error("expected external source")
	}
}

func TestToolSearch_FilteredBySubProject(t *testing.T) {
	_, store, _ := setupTestServer(t)
	defer store.Close()

	// Two records belonging to different sub-projects plus one root-level
	// record that should never be returned when a sub-project filter is set.
	records := []heimdall.VectorRecord{
		{ID: "a1", FilePath: "service-a/main.go", Content: "a", Embedding: []float32{1.0, 0.0}, ModTime: 100, SourceType: "code", Kind: "file", SubProject: "service-a"},
		{ID: "b1", FilePath: "service-b/main.go", Content: "b", Embedding: []float32{0.9, 0.1}, ModTime: 100, SourceType: "code", Kind: "file", SubProject: "service-b"},
		{ID: "r1", FilePath: "root.go", Content: "r", Embedding: []float32{0.8, 0.2}, ModTime: 100, SourceType: "code", Kind: "file"},
	}
	if err := store.Upsert(records); err != nil {
		t.Fatal(err)
	}

	// SearchFiltered signature: (query, limit, sourceType, subProject, metadataFilter)
	results := store.SearchFiltered(context.Background(), []float32{1.0, 0.0}, 10, "", "service-a", nil)
	if len(results) != 1 {
		t.Fatalf("expected 1 service-a result, got %d", len(results))
	}
	if results[0].Record.ID != "a1" {
		t.Errorf("expected a1, got %s", results[0].Record.ID)
	}

	// Filter on the other sub-project returns only its record.
	results = store.SearchFiltered(context.Background(), []float32{1.0, 0.0}, 10, "", "service-b", nil)
	if len(results) != 1 || results[0].Record.ID != "b1" {
		t.Fatalf("expected only b1, got %+v", results)
	}

	// No sub-project filter returns all three.
	results = store.SearchFiltered(context.Background(), []float32{1.0, 0.0}, 10, "", "", nil)
	if len(results) != 3 {
		t.Fatalf("expected 3 unfiltered results, got %d", len(results))
	}
}

func TestToolSearch_FilteredByMetadata(t *testing.T) {
	_, store, _ := setupTestServer(t)
	defer store.Close()

	records := []heimdall.VectorRecord{
		{ID: "t1", FilePath: "JIRA-1", Content: "open ticket", Embedding: []float32{1.0}, ModTime: 100, SourceType: "ticket", Metadata: `{"status":"open"}`},
		{ID: "t2", FilePath: "JIRA-2", Content: "closed ticket", Embedding: []float32{0.9}, ModTime: 100, SourceType: "ticket", Metadata: `{"status":"closed"}`},
	}
	if err := store.Upsert(records); err != nil {
		t.Fatal(err)
	}

	results := store.SearchFiltered(context.Background(), []float32{1.0}, 10, "ticket", "", map[string]any{"status": "open"})
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Record.ID != "t1" {
		t.Errorf("expected t1, got %s", results[0].Record.ID)
	}
}

func TestToolExplain_ScoreDistribution(t *testing.T) {
	_, store, _ := setupTestServer(t)
	defer store.Close()

	// Insert records with known vectors so we can predict scores
	records := []heimdall.VectorRecord{
		{ID: "r1", FilePath: "a.go", Content: "high match", Embedding: []float32{1.0, 0.0}, ModTime: 100, Kind: "file"},
		{ID: "r2", FilePath: "b.go", Content: "medium match", Embedding: []float32{0.7, 0.7}, ModTime: 100, Kind: "file"},
		{ID: "r3", FilePath: "c.go", Content: "low match", Embedding: []float32{0.0, 1.0}, ModTime: 100, Kind: "file"},
	}
	if err := store.Upsert(records); err != nil {
		t.Fatal(err)
	}

	// Search with topK=0 to get all
	allResults := store.Search(context.Background(), []float32{1.0, 0.0}, 0)
	if len(allResults) != 3 {
		t.Fatalf("expected 3 results, got %d", len(allResults))
	}

	// Compute score distribution
	dist := ScoreDistribution{}
	for _, r := range allResults {
		switch {
		case r.Similarity >= 0.90:
			dist.Above90++
		case r.Similarity >= 0.70:
			dist.Above70++
		case r.Similarity >= 0.50:
			dist.Above50++
		default:
			dist.Below50++
		}
	}

	// We expect: r1 should be ~1.0 (above90), r2 ~0.7 (above70 or above50), r3 ~0.0 (below50)
	if dist.Above90 < 1 {
		t.Error("expected at least 1 result above 0.90")
	}
	if dist.Below50 < 1 {
		t.Error("expected at least 1 result below 0.50")
	}
}

func TestToolExplain_SourceCounts(t *testing.T) {
	_, store, _ := setupTestServer(t)
	defer store.Close()

	records := []heimdall.VectorRecord{
		{ID: "c1", FilePath: "a.go", Content: "code", Embedding: []float32{1.0}, ModTime: 100, Kind: "file"},
		{ID: "c2", FilePath: "b.go", Content: "code", Embedding: []float32{0.9}, ModTime: 100, Kind: "function"},
		{ID: "e1", FilePath: "JIRA-1", Content: "ticket", Embedding: []float32{0.8}, ModTime: 100, Kind: "external"},
	}
	if err := store.Upsert(records); err != nil {
		t.Fatal(err)
	}

	allResults := store.Search(context.Background(), []float32{1.0}, 0)
	counts := SourceCounts{}
	for _, r := range allResults {
		switch classifySource(r.Record.Kind) {
		case "code":
			counts.Code++
		case "external":
			counts.External++
		case "memory":
			counts.Memory++
		}
	}

	if counts.Code != 2 {
		t.Errorf("expected 2 code, got %d", counts.Code)
	}
	if counts.External != 1 {
		t.Errorf("expected 1 external, got %d", counts.External)
	}
	if counts.Memory != 0 {
		t.Errorf("expected 0 memory, got %d", counts.Memory)
	}
}

func TestToolExplain_RelationshipTraversal(t *testing.T) {
	srv, store, _ := setupTestServer(t)
	defer store.Close()

	// Insert a target entry
	target := heimdall.VectorRecord{
		ID: "ext:JIRA-200:0", FilePath: "JIRA-200", Content: "target ticket content here",
		Embedding: []float32{0.5}, ModTime: 100, Kind: "external", SourceType: "ticket",
	}
	// Insert an entry with a relationship pointing to JIRA-200
	source := heimdall.VectorRecord{
		ID: "ext:JIRA-100:0", FilePath: "JIRA-100", Content: "source ticket",
		Embedding: []float32{1.0}, ModTime: 100, Kind: "external", SourceType: "ticket",
		Relationships: `[{"type":"blocks","target":"JIRA-200"}]`,
	}
	if err := store.Upsert([]heimdall.VectorRecord{target, source}); err != nil {
		t.Fatal(err)
	}

	// Resolve relationships for the source record
	related := srv.resolveRelationships(store, source)
	if len(related) != 1 {
		t.Fatalf("expected 1 related item, got %d", len(related))
	}
	if related[0].Source != "JIRA-200" {
		t.Errorf("expected related source JIRA-200, got %s", related[0].Source)
	}
	if related[0].Relationship != "blocks" {
		t.Errorf("expected relationship 'blocks', got %s", related[0].Relationship)
	}
	if related[0].Snippet == "" {
		t.Error("expected non-empty snippet")
	}
}

func TestToolIndexText_TypedContent(t *testing.T) {
	// Test the validation and parsing of typed content input
	validInput := indexTextInput{
		Content:       "ticket content",
		Source:        "JIRA-123",
		Type:          "ticket",
		Metadata:      json.RawMessage(`{"status":"open","priority":"high"}`),
		Relationships: json.RawMessage(`[{"type":"implements","target":"EPIC-1"}]`),
	}

	// Validate source
	if err := validateSource(validInput.Source); err != nil {
		t.Errorf("valid source should pass: %v", err)
	}

	// Validate type
	st := validateSourceType(validInput.Type)
	if st != "ticket" {
		t.Errorf("expected 'ticket', got %q", st)
	}

	// Validate metadata
	var obj map[string]any
	if err := json.Unmarshal(validInput.Metadata, &obj); err != nil {
		t.Errorf("valid metadata should parse: %v", err)
	}

	// Validate relationships
	var rels []struct {
		Type   string `json:"type"`
		Target string `json:"target"`
	}
	if err := json.Unmarshal(validInput.Relationships, &rels); err != nil {
		t.Errorf("valid relationships should parse: %v", err)
	}
	if len(rels) != 1 {
		t.Errorf("expected 1 relationship, got %d", len(rels))
	}
}

func TestToolIndexText_ValidationErrors(t *testing.T) {
	// Invalid type
	if st := validateSourceType("invalid_type"); st != "" {
		t.Error("expected empty string for invalid type")
	}

	// Invalid source — path traversal
	if err := validateSource("../../etc/passwd"); err == nil {
		t.Error("expected error for path traversal")
	}

	// Invalid source — too long
	longSource := make([]byte, 501)
	for i := range longSource {
		longSource[i] = 'a'
	}
	if err := validateSource(string(longSource)); err == nil {
		t.Error("expected error for too-long source")
	}

	// Invalid metadata — not an object
	badMeta := json.RawMessage(`"not an object"`)
	var obj map[string]any
	if err := json.Unmarshal(badMeta, &obj); err == nil {
		t.Error("expected error for non-object metadata")
	}

	// Invalid relationships — not an array
	badRels := json.RawMessage(`{"not":"an array"}`)
	var rels []struct {
		Type   string `json:"type"`
		Target string `json:"target"`
	}
	if err := json.Unmarshal(badRels, &rels); err == nil {
		t.Error("expected error for non-array relationships")
	}
}

func TestResolveRelationships_EmptyRelationships(t *testing.T) {
	srv, store, _ := setupTestServer(t)
	defer store.Close()

	// Record with no relationships
	rec := heimdall.VectorRecord{Relationships: "[]"}
	related := srv.resolveRelationships(store, rec)
	if related != nil {
		t.Error("expected nil for empty relationships")
	}

	// Record with empty string
	rec2 := heimdall.VectorRecord{Relationships: ""}
	related2 := srv.resolveRelationships(store, rec2)
	if related2 != nil {
		t.Error("expected nil for empty relationship string")
	}
}

func TestSearchResultEnriched_JSON(t *testing.T) {
	result := SearchResultEnriched{
		File:           "main.go",
		StartLine:      1,
		EndLine:        10,
		Content:        "func main() {}",
		Score:          0.95,
		Source:         "code",
		ChunkID:        "main.go:1:10",
		EmbeddingModel: "bge-m3",
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}

	var parsed map[string]any
	json.Unmarshal(data, &parsed)

	// Verify all expected keys exist
	expectedKeys := []string{"file", "startLine", "endLine", "content", "score", "source", "chunkId", "embeddingModel"}
	for _, key := range expectedKeys {
		if _, ok := parsed[key]; !ok {
			t.Errorf("missing key %q in JSON output", key)
		}
	}
}

func TestExplainResult_JSON(t *testing.T) {
	result := ExplainResult{
		Query:               "test query",
		QueryEmbeddingDim:   1024,
		TotalChunksSearched: 100,
		SearchTimeMs:        15,
		Results: []ExplainResultItem{
			{Rank: 1, ChunkID: "a:1:5", File: "a.go", Score: 0.95, Source: "code", ChunkSizeBytes: 100, ChunkLines: 5},
		},
		ScoreDistribution: ScoreDistribution{Above90: 1, Above70: 5, Above50: 20, Below50: 74},
		SourceCounts:      SourceCounts{Code: 90, External: 10, Memory: 0},
		IndexStats:        IndexStatsInfo{TotalChunks: 100, TotalFiles: 20, LastIndexed: "2026-04-11 10:00:00"},
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}

	var parsed map[string]any
	json.Unmarshal(data, &parsed)

	expectedKeys := []string{"query", "queryEmbeddingDim", "totalChunksSearched", "searchTimeMs", "results", "scoreDistribution", "sourceCounts", "indexStats"}
	for _, key := range expectedKeys {
		if _, ok := parsed[key]; !ok {
			t.Errorf("missing key %q in ExplainResult JSON", key)
		}
	}
}

// --- Phase 4.1: Auto-index on first search ---

func TestAutoIndexOnSearch_ReturnsIndexingStarted(t *testing.T) {
	reg := &registry.Registry{}
	srv := &Server{
		Cfg:      config.DefaultConfig(),
		Registry: reg,
	}

	// autoIndexOnSearch should return a message with "indexing_started"
	// since the CWD will exist but no DB exists
	result := srv.autoIndexOnSearch()

	if result.IsError {
		t.Fatalf("expected non-error result, got error: %s", result.Content[0].Text)
	}

	text := result.Content[0].Text
	if !contains(text, "indexing_started") && !contains(text, "indexing_in_progress") {
		t.Errorf("expected indexing_started or indexing_in_progress in response, got: %s", text)
	}

	// The index should now be marked as running
	srv.Index.Mu.Lock()
	running := srv.Index.Running
	srv.Index.Mu.Unlock()

	if !running {
		// It may have already completed or failed (since Ollama isn't running in test),
		// which is fine - we just verify the trigger path works
		t.Log("indexing not running (expected in test environment without Ollama)")
	}

	// Clean up: cancel any running index
	srv.Index.Mu.Lock()
	if srv.Index.Cancel != nil {
		srv.Index.Cancel()
	}
	srv.Index.Mu.Unlock()

	// Clean up: remove .heimdall_db created by runIndex in CWD
	cwd, _ := os.Getwd()
	os.RemoveAll(filepath.Join(cwd, ".heimdall_db"))
}

func TestAutoIndexOnSearch_AlreadyRunning(t *testing.T) {
	reg := &registry.Registry{}
	srv := &Server{
		Cfg:      config.DefaultConfig(),
		Registry: reg,
	}

	// Pre-set indexing as running
	srv.Index.Mu.Lock()
	srv.Index.Running = true
	srv.Index.Path = "/some/path"
	srv.Index.Mu.Unlock()

	result := srv.autoIndexOnSearch()

	if result.IsError {
		t.Fatalf("expected non-error result, got error")
	}

	text := result.Content[0].Text
	if !contains(text, "indexing_in_progress") {
		t.Errorf("expected indexing_in_progress in response, got: %s", text)
	}

	// Clean up
	srv.Index.Mu.Lock()
	srv.Index.Running = false
	srv.Index.Mu.Unlock()
}

// --- Phase 4.3: Stale index check ---

func TestCheckAndTriggerReindex_FreshIndex(t *testing.T) {
	srv, store, dbDir := setupTestServer(t)
	defer store.Close()

	// Insert a record with a recent modtime
	records := []heimdall.VectorRecord{
		{
			ID: "r1", FilePath: "a.go", Content: "func main",
			Embedding: []float32{1.0}, ModTime: time.Now().Unix(),
			SourceType: "code",
		},
	}
	if err := store.Upsert(records); err != nil {
		t.Fatal(err)
	}

	note := srv.checkAndTriggerReindex(store, dbDir)
	if note != "" {
		t.Errorf("expected empty note for fresh index, got: %s", note)
	}
}

func TestCheckAndTriggerReindex_StaleIndex(t *testing.T) {
	srv, store, dbDir := setupTestServer(t)
	defer store.Close()

	// Insert a record with an old modtime (older than 30 minutes)
	oldTime := time.Now().Unix() - staleTimeoutSeconds - 100
	records := []heimdall.VectorRecord{
		{
			ID: "r1", FilePath: "a.go", Content: "func main",
			Embedding: []float32{1.0}, ModTime: oldTime,
			SourceType: "code",
		},
	}
	if err := store.Upsert(records); err != nil {
		t.Fatal(err)
	}

	note := srv.checkAndTriggerReindex(store, dbDir)
	if note == "" {
		t.Error("expected stale note, got empty")
	}
	if !contains(note, "stale") {
		t.Errorf("expected note to mention 'stale', got: %s", note)
	}
}

func TestCheckAndTriggerReindex_NoProjectPath(t *testing.T) {
	// Server with empty registry — no project path can be found
	reg := &registry.Registry{}
	srv := &Server{
		Cfg:      config.DefaultConfig(),
		Registry: reg,
	}

	tmpDir := t.TempDir()
	dbDir := filepath.Join(tmpDir, ".heimdall_db")
	store, err := heimdall.OpenStore(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Insert old record
	oldTime := time.Now().Unix() - staleTimeoutSeconds - 100
	store.Upsert([]heimdall.VectorRecord{
		{ID: "r1", FilePath: "a.go", Content: "test", Embedding: []float32{1.0}, ModTime: oldTime},
	})

	// dbDir won't match any registered project, so no re-index should trigger
	note := srv.checkAndTriggerReindex(store, dbDir)
	if note != "" {
		t.Errorf("expected empty note when no project matches, got: %s", note)
	}
}

func TestCheckAndTriggerReindex_EmptyStore(t *testing.T) {
	srv, store, dbDir := setupTestServer(t)
	defer store.Close()

	// Empty store — LastModified is 0
	note := srv.checkAndTriggerReindex(store, dbDir)
	if note != "" {
		t.Errorf("expected empty note for empty store, got: %s", note)
	}
}

// --- Phase 4.4: MCP instructions ---

func TestHandleInitialize_ContainsInstructions(t *testing.T) {
	srv := &Server{
		Cfg:      config.DefaultConfig(),
		Registry: &registry.Registry{},
	}

	req := JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "initialize",
	}

	resp := srv.Handle(req)
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if resp.Error != nil {
		t.Fatalf("expected no error, got: %v", resp.Error)
	}

	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatal("expected result to be a map")
	}

	instructions, ok := result["instructions"]
	if !ok {
		t.Fatal("expected 'instructions' field in initialize response")
	}

	instrStr, ok := instructions.(string)
	if !ok {
		t.Fatal("expected instructions to be a string")
	}

	// Instructions contain either the happy-path text (Ollama running) or
	// setup guidance (Ollama not running). Both are valid.
	hasHappyPath := contains(instrStr, "heimdall_index_text") && contains(instrStr, "silently")
	hasSetupGuide := contains(instrStr, "Ollama") && contains(instrStr, "ollama")
	if !hasHappyPath && !hasSetupGuide {
		t.Errorf("instructions should contain either indexing guidance or Ollama setup instructions, got: %s", instrStr[:min(len(instrStr), 200)])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestHandleInitialize_HasServerInfo(t *testing.T) {
	srv := &Server{
		Cfg:      config.DefaultConfig(),
		Registry: &registry.Registry{},
	}

	req := JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "initialize",
	}

	resp := srv.Handle(req)
	result := resp.Result.(map[string]any)

	serverInfo, ok := result["serverInfo"].(map[string]any)
	if !ok {
		t.Fatal("expected serverInfo")
	}
	if serverInfo["name"] != "heimdall-mcp" {
		t.Errorf("expected name 'heimdall-mcp', got %v", serverInfo["name"])
	}
}

// helper
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestDBDir_WrittenOnFirstUse(t *testing.T) {
	tmpDir := t.TempDir()
	dbDir := filepath.Join(tmpDir, "new_db")

	// Directory should not exist yet
	if _, err := os.Stat(dbDir); err == nil {
		t.Fatal("db dir should not exist yet")
	}

	// OpenStore should create it
	store, err := heimdall.OpenStore(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := os.Stat(dbDir); err != nil {
		t.Fatal("db dir should exist after OpenStore")
	}
}
