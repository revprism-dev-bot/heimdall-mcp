package heimdall

import (
	"os"
	"path/filepath"
	"testing"
)

func tempStoreDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".heimdall_db")
	return dir
}

func TestOpenStore_MigrationCreatesColumns(t *testing.T) {
	dir := tempStoreDir(t)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Verify new columns exist by inserting a record with all fields
	rec := VectorRecord{
		ID:            "test:1:5",
		FilePath:      "test.go",
		StartLine:     1,
		EndLine:       5,
		Content:       "test content",
		Kind:          "file",
		Embedding:     []float32{1.0, 0.0, 0.0},
		ModTime:       100,
		SourceType:    "ticket",
		Metadata:      `{"status":"open"}`,
		Relationships: `[{"type":"blocks","target":"JIRA-200"}]`,
	}
	if err := store.Upsert([]VectorRecord{rec}); err != nil {
		t.Fatal("upsert with new fields should work:", err)
	}

	// Read back and verify
	results := store.Search([]float32{1.0, 0.0, 0.0}, 1)
	if len(results) != 1 {
		t.Fatal("expected 1 result, got", len(results))
	}
	r := results[0].Record
	if r.SourceType != "ticket" {
		t.Errorf("SourceType = %q, want %q", r.SourceType, "ticket")
	}
	if r.Metadata != `{"status":"open"}` {
		t.Errorf("Metadata = %q, want %q", r.Metadata, `{"status":"open"}`)
	}
	if r.Relationships != `[{"type":"blocks","target":"JIRA-200"}]` {
		t.Errorf("Relationships = %q, want expected JSON", r.Relationships)
	}
}

func TestOpenStore_DefaultValues(t *testing.T) {
	dir := tempStoreDir(t)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Insert record with empty new fields — should get defaults
	rec := VectorRecord{
		ID:        "test:1:1",
		FilePath:  "test.go",
		Content:   "test",
		Embedding: []float32{1.0},
		ModTime:   100,
	}
	if err := store.Upsert([]VectorRecord{rec}); err != nil {
		t.Fatal(err)
	}

	results := store.Search([]float32{1.0}, 1)
	if len(results) != 1 {
		t.Fatal("expected 1 result")
	}
	r := results[0].Record
	if r.SourceType != "code" {
		t.Errorf("default SourceType = %q, want %q", r.SourceType, "code")
	}
	if r.Metadata != "{}" {
		t.Errorf("default Metadata = %q, want %q", r.Metadata, "{}")
	}
	if r.Relationships != "[]" {
		t.Errorf("default Relationships = %q, want %q", r.Relationships, "[]")
	}
}

func TestSearchFiltered_BySourceType(t *testing.T) {
	dir := tempStoreDir(t)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	records := []VectorRecord{
		{ID: "code:1", FilePath: "main.go", Content: "func main", Embedding: []float32{1.0, 0.0}, ModTime: 100, SourceType: "code"},
		{ID: "ext:JIRA-1:0", FilePath: "JIRA-1", Content: "ticket content", Embedding: []float32{0.9, 0.1}, ModTime: 100, SourceType: "ticket"},
		{ID: "ext:DOC-1:0", FilePath: "DOC-1", Content: "doc content", Embedding: []float32{0.8, 0.2}, ModTime: 100, SourceType: "doc"},
	}
	if err := store.Upsert(records); err != nil {
		t.Fatal(err)
	}

	// Filter by ticket
	results := store.SearchFiltered([]float32{1.0, 0.0}, 10, "ticket", nil)
	if len(results) != 1 {
		t.Fatalf("expected 1 ticket result, got %d", len(results))
	}
	if results[0].Record.ID != "ext:JIRA-1:0" {
		t.Errorf("expected JIRA-1, got %s", results[0].Record.ID)
	}

	// Filter by code
	results = store.SearchFiltered([]float32{1.0, 0.0}, 10, "code", nil)
	if len(results) != 1 {
		t.Fatalf("expected 1 code result, got %d", len(results))
	}

	// No filter returns all
	results = store.SearchFiltered([]float32{1.0, 0.0}, 10, "", nil)
	if len(results) != 3 {
		t.Fatalf("expected 3 results with no filter, got %d", len(results))
	}
}

func TestSearchFiltered_ByMetadata(t *testing.T) {
	dir := tempStoreDir(t)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	records := []VectorRecord{
		{ID: "t1", FilePath: "JIRA-1", Content: "open ticket", Embedding: []float32{1.0}, ModTime: 100, SourceType: "ticket", Metadata: `{"status":"open","priority":"high"}`},
		{ID: "t2", FilePath: "JIRA-2", Content: "closed ticket", Embedding: []float32{0.9}, ModTime: 100, SourceType: "ticket", Metadata: `{"status":"closed","priority":"low"}`},
		{ID: "t3", FilePath: "JIRA-3", Content: "open low", Embedding: []float32{0.8}, ModTime: 100, SourceType: "ticket", Metadata: `{"status":"open","priority":"low"}`},
	}
	if err := store.Upsert(records); err != nil {
		t.Fatal(err)
	}

	// Filter by status=open
	results := store.SearchFiltered([]float32{1.0}, 10, "", map[string]any{"status": "open"})
	if len(results) != 2 {
		t.Fatalf("expected 2 open results, got %d", len(results))
	}

	// Filter by status=open AND priority=high
	results = store.SearchFiltered([]float32{1.0}, 10, "", map[string]any{"status": "open", "priority": "high"})
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Record.ID != "t1" {
		t.Errorf("expected t1, got %s", results[0].Record.ID)
	}
}

func TestSearchFiltered_Combined(t *testing.T) {
	dir := tempStoreDir(t)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	records := []VectorRecord{
		{ID: "t1", FilePath: "JIRA-1", Content: "ticket", Embedding: []float32{1.0}, ModTime: 100, SourceType: "ticket", Metadata: `{"status":"open"}`},
		{ID: "d1", FilePath: "DOC-1", Content: "doc", Embedding: []float32{0.9}, ModTime: 100, SourceType: "doc", Metadata: `{"status":"open"}`},
	}
	if err := store.Upsert(records); err != nil {
		t.Fatal(err)
	}

	// Filter by source_type=ticket AND status=open
	results := store.SearchFiltered([]float32{1.0}, 10, "ticket", map[string]any{"status": "open"})
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Record.SourceType != "ticket" {
		t.Errorf("expected ticket, got %s", results[0].Record.SourceType)
	}
}

func TestMatchesMetadata(t *testing.T) {
	tests := []struct {
		name     string
		metadata string
		filter   map[string]any
		want     bool
	}{
		{"empty metadata", "", map[string]any{"k": "v"}, false},
		{"empty object", "{}", map[string]any{"k": "v"}, false},
		{"invalid JSON", "not json", map[string]any{"k": "v"}, false},
		{"exact match", `{"status":"open"}`, map[string]any{"status": "open"}, true},
		{"missing key", `{"status":"open"}`, map[string]any{"priority": "high"}, false},
		{"partial match", `{"status":"open","priority":"high"}`, map[string]any{"status": "open"}, true},
		{"type safe - string vs number", `{"count":"1"}`, map[string]any{"count": float64(1)}, false},
		{"type safe - bool vs string", `{"active":"true"}`, map[string]any{"active": true}, false},
		{"number match", `{"count":1}`, map[string]any{"count": float64(1)}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesMetadata(tt.metadata, tt.filter)
			if got != tt.want {
				t.Errorf("matchesMetadata(%q, %v) = %v, want %v", tt.metadata, tt.filter, got, tt.want)
			}
		})
	}
}

func TestSnippetBySource(t *testing.T) {
	dir := tempStoreDir(t)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Short content — no truncation
	short := VectorRecord{
		ID: "s1", FilePath: "JIRA-100", Content: "short snippet",
		Embedding: []float32{1.0}, ModTime: 100,
	}
	if err := store.Upsert([]VectorRecord{short}); err != nil {
		t.Fatal(err)
	}
	snippet := store.SnippetBySource("JIRA-100")
	if snippet != "short snippet" {
		t.Errorf("expected 'short snippet', got %q", snippet)
	}

	// Long content — truncated to 200 + "..."
	longContent := ""
	for i := 0; i < 250; i++ {
		longContent += "x"
	}
	long := VectorRecord{
		ID: "l1", FilePath: "JIRA-200", Content: longContent,
		Embedding: []float32{1.0}, ModTime: 100,
	}
	if err := store.Upsert([]VectorRecord{long}); err != nil {
		t.Fatal(err)
	}
	snippet = store.SnippetBySource("JIRA-200")
	if len(snippet) != 203 { // 200 + "..."
		t.Errorf("expected length 203, got %d", len(snippet))
	}

	// Non-existent source
	snippet = store.SnippetBySource("DOES-NOT-EXIST")
	if snippet != "" {
		t.Errorf("expected empty string for non-existent source, got %q", snippet)
	}
}

func TestUpsert_NullByteSanitization(t *testing.T) {
	dir := tempStoreDir(t)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	rec := VectorRecord{
		ID: "n1", FilePath: "test.go", Content: "hello\x00world",
		Embedding: []float32{1.0}, ModTime: 100,
	}
	if err := store.Upsert([]VectorRecord{rec}); err != nil {
		t.Fatal(err)
	}

	results := store.Search([]float32{1.0}, 1)
	if len(results) != 1 {
		t.Fatal("expected 1 result")
	}
	if results[0].Record.Content != "helloworld" {
		t.Errorf("expected null bytes stripped, got %q", results[0].Record.Content)
	}
}

func TestSearchFiltered_TopK(t *testing.T) {
	dir := tempStoreDir(t)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	records := []VectorRecord{
		{ID: "a", FilePath: "a.go", Content: "a", Embedding: []float32{1.0, 0.0}, ModTime: 100},
		{ID: "b", FilePath: "b.go", Content: "b", Embedding: []float32{0.9, 0.1}, ModTime: 100},
		{ID: "c", FilePath: "c.go", Content: "c", Embedding: []float32{0.8, 0.2}, ModTime: 100},
	}
	if err := store.Upsert(records); err != nil {
		t.Fatal(err)
	}

	// topK=2 should return 2
	results := store.Search([]float32{1.0, 0.0}, 2)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// topK=0 should return all
	results = store.Search([]float32{1.0, 0.0}, 0)
	if len(results) != 3 {
		t.Fatalf("expected 3 results with topK=0, got %d", len(results))
	}
}

func TestOpenStore_ExistingDB_Migration(t *testing.T) {
	dir := tempStoreDir(t)

	// Create a store (first open creates the schema)
	store1, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Insert a record without new fields
	rec := VectorRecord{
		ID: "old:1:1", FilePath: "old.go", Content: "old code",
		Embedding: []float32{1.0, 0.0}, ModTime: 50,
	}
	if err := store1.Upsert([]VectorRecord{rec}); err != nil {
		t.Fatal(err)
	}
	store1.Close()

	// Re-open (migration runs again, should be idempotent)
	store2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()

	// Old record should still be readable with default values
	results := store2.Search([]float32{1.0, 0.0}, 1)
	if len(results) != 1 {
		t.Fatal("expected 1 result after re-open")
	}
	r := results[0].Record
	if r.Content != "old code" {
		t.Errorf("content = %q, want %q", r.Content, "old code")
	}
	// Defaults should apply
	if r.SourceType != "code" {
		t.Errorf("SourceType = %q, want %q", r.SourceType, "code")
	}

	// Should be able to store new records with new fields
	newRec := VectorRecord{
		ID: "new:1:1", FilePath: "new.go", Content: "new code",
		Embedding: []float32{0.5, 0.5}, ModTime: 100,
		SourceType: "doc", Metadata: `{"v":1}`, Relationships: `[]`,
	}
	if err := store2.Upsert([]VectorRecord{newRec}); err != nil {
		t.Fatal(err)
	}
}

func TestOpenStore_CleanupTempDir(t *testing.T) {
	// Ensure temp dirs are cleaned
	dir := tempStoreDir(t)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()

	if _, err := os.Stat(dir); err != nil {
		t.Fatal("store directory should exist after open")
	}
}
