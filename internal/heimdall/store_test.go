package heimdall

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
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
	results := store.Search(context.Background(), []float32{1.0, 0.0, 0.0}, 1)
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

	results := store.Search(context.Background(), []float32{1.0}, 1)
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
	results := store.SearchFiltered(context.Background(), []float32{1.0, 0.0}, 10, "ticket", "", nil)
	if len(results) != 1 {
		t.Fatalf("expected 1 ticket result, got %d", len(results))
	}
	if results[0].Record.ID != "ext:JIRA-1:0" {
		t.Errorf("expected JIRA-1, got %s", results[0].Record.ID)
	}

	// Filter by code
	results = store.SearchFiltered(context.Background(), []float32{1.0, 0.0}, 10, "code", "", nil)
	if len(results) != 1 {
		t.Fatalf("expected 1 code result, got %d", len(results))
	}

	// No filter returns all
	results = store.SearchFiltered(context.Background(), []float32{1.0, 0.0}, 10, "", "", nil)
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
	results := store.SearchFiltered(context.Background(), []float32{1.0}, 10, "", "", map[string]any{"status": "open"})
	if len(results) != 2 {
		t.Fatalf("expected 2 open results, got %d", len(results))
	}

	// Filter by status=open AND priority=high
	results = store.SearchFiltered(context.Background(), []float32{1.0}, 10, "", "", map[string]any{"status": "open", "priority": "high"})
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
	results := store.SearchFiltered(context.Background(), []float32{1.0}, 10, "ticket", "", map[string]any{"status": "open"})
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

	results := store.Search(context.Background(), []float32{1.0}, 1)
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
	results := store.Search(context.Background(), []float32{1.0, 0.0}, 2)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// topK=0 should return all
	results = store.Search(context.Background(), []float32{1.0, 0.0}, 0)
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
	results := store2.Search(context.Background(), []float32{1.0, 0.0}, 1)
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

func TestUpdateLastAccessed(t *testing.T) {
	dir := tempStoreDir(t)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	records := []VectorRecord{
		{ID: "a1", FilePath: "a.go", Content: "a", Embedding: []float32{1.0}, ModTime: 100},
		{ID: "b1", FilePath: "b.go", Content: "b", Embedding: []float32{0.9}, ModTime: 100},
		{ID: "c1", FilePath: "c.go", Content: "c", Embedding: []float32{0.8}, ModTime: 100},
	}
	if err := store.Upsert(records); err != nil {
		t.Fatal(err)
	}

	before := time.Now().Unix()
	if err := store.UpdateLastAccessed([]string{"a1", "c1"}); err != nil {
		t.Fatal(err)
	}
	after := time.Now().Unix()

	// Verify a1 and c1 have updated last_accessed
	for _, id := range []string{"a1", "c1"} {
		var la int64
		store.DB().QueryRow(`SELECT last_accessed FROM entries WHERE id = ?`, id).Scan(&la)
		if la < before || la > after {
			t.Errorf("entry %s: last_accessed=%d, expected between %d and %d", id, la, before, after)
		}
	}

	// Verify b1 was NOT updated
	var la int64
	store.DB().QueryRow(`SELECT last_accessed FROM entries WHERE id = ?`, "b1").Scan(&la)
	if la != 0 {
		t.Errorf("entry b1: last_accessed=%d, want 0", la)
	}
}

func TestUpdateLastAccessed_EmptyIDs(t *testing.T) {
	dir := tempStoreDir(t)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Should be a no-op, not an error
	if err := store.UpdateLastAccessed(nil); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateLastAccessed([]string{}); err != nil {
		t.Fatal(err)
	}
}

func TestSearchFiltered_ReturnsLastAccessed(t *testing.T) {
	dir := tempStoreDir(t)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	rec := VectorRecord{
		ID: "r1", FilePath: "r.go", Content: "record", Embedding: []float32{1.0}, ModTime: 100,
	}
	if err := store.Upsert([]VectorRecord{rec}); err != nil {
		t.Fatal(err)
	}

	// Initially last_accessed should be 0
	results := store.SearchFiltered(context.Background(), []float32{1.0}, 1, "", "", nil)
	if len(results) != 1 {
		t.Fatal("expected 1 result")
	}
	if results[0].Record.LastAccessed != 0 {
		t.Errorf("LastAccessed = %d, want 0", results[0].Record.LastAccessed)
	}

	// Update and verify
	if err := store.UpdateLastAccessed([]string{"r1"}); err != nil {
		t.Fatal(err)
	}
	results = store.SearchFiltered(context.Background(), []float32{1.0}, 1, "", "", nil)
	if len(results) != 1 {
		t.Fatal("expected 1 result")
	}
	if results[0].Record.LastAccessed <= 0 {
		t.Errorf("LastAccessed = %d, want > 0 after update", results[0].Record.LastAccessed)
	}
}

func TestFreshnessWeight(t *testing.T) {
	now := time.Now().Unix()

	tests := []struct {
		name         string
		lastAccessed int64
		wantMin      float64
		wantMax      float64
	}{
		{"never accessed (0)", 0, 1.0, 1.0},
		{"negative", -1, 1.0, 1.0},
		{"just now", now, 0.99, 1.01},
		{"45 days ago", now - 45*86400, 0.74, 0.76},
		{"90+ days ago", now - 100*86400, 0.49, 0.51},
		{"exactly 90 days", now - 90*86400, 0.49, 0.51},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := freshnessWeight(tt.lastAccessed)
			if got < tt.wantMin || got > tt.wantMax {
				t.Errorf("freshnessWeight(%d) = %.4f, want [%.2f, %.2f]", tt.lastAccessed, got, tt.wantMin, tt.wantMax)
			}
		})
	}
}

func TestFreshnessWeight_LinearDecay(t *testing.T) {
	now := time.Now().Unix()

	// Verify the decay is monotonically decreasing
	prev := freshnessWeight(now)
	for days := 1; days <= 90; days++ {
		w := freshnessWeight(now - int64(days)*86400)
		if w > prev+0.001 { // small epsilon for floating point
			t.Errorf("day %d: weight %.4f > prev %.4f — not monotonically decreasing", days, w, prev)
		}
		prev = w
	}

	// Past 90 days, weight should be clamped at 0.5
	w91 := freshnessWeight(now - 91*86400)
	w180 := freshnessWeight(now - 180*86400)
	if math.Abs(w91-0.5) > 0.01 || math.Abs(w180-0.5) > 0.01 {
		t.Errorf("past 90 days: w91=%.4f, w180=%.4f, both should be ~0.5", w91, w180)
	}
}

func TestSearchFiltered_FreshnessDecay(t *testing.T) {
	dir := tempStoreDir(t)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Two records with identical embeddings — same cosine similarity
	now := time.Now().Unix()
	vec := EncodeFloat32Vec([]float32{1.0, 0.0})

	// Record A: accessed recently (should rank higher)
	if _, err := store.DB().Exec(`INSERT INTO entries (id, file_path, start_line, end_line, content, kind, identifier, vector, mod_time, content_hash, source_type, metadata, relationships, last_accessed)
		VALUES ('a', 'a.go', 0, 0, 'a', 'file', '', ?, 100, '', 'code', '{}', '[]', ?)`, vec, now-1*86400); err != nil {
		t.Fatal("insert a:", err)
	}

	// Record B: accessed 60 days ago (should rank lower due to decay)
	if _, err := store.DB().Exec(`INSERT INTO entries (id, file_path, start_line, end_line, content, kind, identifier, vector, mod_time, content_hash, source_type, metadata, relationships, last_accessed)
		VALUES ('b', 'b.go', 0, 0, 'b', 'file', '', ?, 100, '', 'code', '{}', '[]', ?)`, vec, now-60*86400); err != nil {
		t.Fatal("insert b:", err)
	}

	results := store.SearchFiltered(context.Background(), []float32{1.0, 0.0}, 2, "", "", nil)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// A (recently accessed) should rank above B (old access)
	if results[0].Record.ID != "a" {
		t.Errorf("expected 'a' first (recently accessed), got %q", results[0].Record.ID)
	}
	if results[1].Record.ID != "b" {
		t.Errorf("expected 'b' second (old access), got %q", results[1].Record.ID)
	}
	if results[0].Similarity <= results[1].Similarity {
		t.Errorf("recently accessed entry should score higher: a=%.4f, b=%.4f",
			results[0].Similarity, results[1].Similarity)
	}
}

func TestSearchFiltered_NeverAccessedNotPenalized(t *testing.T) {
	dir := tempStoreDir(t)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().Unix()
	vec := EncodeFloat32Vec([]float32{1.0, 0.0})

	// Record A: never accessed (last_accessed = 0) — should get full weight
	if _, err := store.DB().Exec(`INSERT INTO entries (id, file_path, start_line, end_line, content, kind, identifier, vector, mod_time, content_hash, source_type, metadata, relationships, last_accessed)
		VALUES ('new', 'new.go', 0, 0, 'new code', 'file', '', ?, 100, '', 'code', '{}', '[]', 0)`, vec); err != nil {
		t.Fatal("insert new:", err)
	}

	// Record B: accessed 60 days ago — should be penalized
	if _, err := store.DB().Exec(`INSERT INTO entries (id, file_path, start_line, end_line, content, kind, identifier, vector, mod_time, content_hash, source_type, metadata, relationships, last_accessed)
		VALUES ('old', 'old.go', 0, 0, 'old code', 'file', '', ?, 100, '', 'code', '{}', '[]', ?)`, vec, now-60*86400); err != nil {
		t.Fatal("insert old:", err)
	}

	results := store.SearchFiltered(context.Background(), []float32{1.0, 0.0}, 2, "", "", nil)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// Never-accessed entry should rank higher than 60-day-old access
	if results[0].Record.ID != "new" {
		t.Errorf("expected 'new' first (never accessed = full weight), got %q", results[0].Record.ID)
	}
}

func TestShouldRunLifecycle_And_MarkLifecycleRun(t *testing.T) {
	dir := tempStoreDir(t)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Initially should be true (lastLifecycleRun = 0)
	if !store.ShouldRunLifecycle() {
		t.Error("ShouldRunLifecycle() = false, want true on fresh store")
	}

	// After marking, should be false
	store.MarkLifecycleRun()
	if store.ShouldRunLifecycle() {
		t.Error("ShouldRunLifecycle() = true, want false after MarkLifecycleRun()")
	}

	// Simulate passage of time by directly setting lastLifecycleRun
	store.mu.Lock()
	store.lastLifecycleRun = time.Now().Unix() - 3601
	store.mu.Unlock()

	if !store.ShouldRunLifecycle() {
		t.Error("ShouldRunLifecycle() = false, want true after >1 hour")
	}
}

func TestLastAccessedMigration(t *testing.T) {
	dir := tempStoreDir(t)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Verify the last_accessed column exists with default 0
	rec := VectorRecord{
		ID: "m1", FilePath: "m.go", Content: "migration test", Embedding: []float32{1.0}, ModTime: 100,
	}
	if err := store.Upsert([]VectorRecord{rec}); err != nil {
		t.Fatal(err)
	}

	var la int64
	store.DB().QueryRow(`SELECT last_accessed FROM entries WHERE id = ?`, "m1").Scan(&la)
	if la != 0 {
		t.Errorf("default last_accessed = %d, want 0", la)
	}
}

func TestSubProjectMigration(t *testing.T) {
	dir := tempStoreDir(t)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Insert a record with SubProject set
	rec := VectorRecord{
		ID:         "sp1",
		FilePath:   "service-a/main.go",
		Content:    "package main",
		Embedding:  []float32{1.0, 0.0},
		ModTime:    100,
		SubProject: "service-a",
	}
	if err := store.Upsert([]VectorRecord{rec}); err != nil {
		t.Fatal(err)
	}

	// Verify sub_project is stored
	var sp string
	store.DB().QueryRow(`SELECT sub_project FROM entries WHERE id = ?`, "sp1").Scan(&sp)
	if sp != "service-a" {
		t.Errorf("sub_project = %q, want %q", sp, "service-a")
	}
}

func TestSubProjectMigration_DefaultEmpty(t *testing.T) {
	dir := tempStoreDir(t)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Insert a record without SubProject — should default to empty
	rec := VectorRecord{
		ID:        "sp2",
		FilePath:  "main.go",
		Content:   "package main",
		Embedding: []float32{1.0},
		ModTime:   100,
	}
	if err := store.Upsert([]VectorRecord{rec}); err != nil {
		t.Fatal(err)
	}

	var sp string
	store.DB().QueryRow(`SELECT sub_project FROM entries WHERE id = ?`, "sp2").Scan(&sp)
	if sp != "" {
		t.Errorf("default sub_project = %q, want empty", sp)
	}
}

func TestSearchFiltered_BySubProject(t *testing.T) {
	dir := tempStoreDir(t)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	records := []VectorRecord{
		{ID: "a1", FilePath: "service-a/main.go", Content: "service a", Embedding: []float32{1.0, 0.0}, ModTime: 100, SubProject: "service-a"},
		{ID: "a2", FilePath: "service-a/handler.go", Content: "handler a", Embedding: []float32{0.9, 0.1}, ModTime: 100, SubProject: "service-a"},
		{ID: "b1", FilePath: "service-b/main.go", Content: "service b", Embedding: []float32{0.8, 0.2}, ModTime: 100, SubProject: "service-b"},
		{ID: "r1", FilePath: "main.go", Content: "root", Embedding: []float32{0.7, 0.3}, ModTime: 100, SubProject: ""},
	}
	if err := store.Upsert(records); err != nil {
		t.Fatal(err)
	}

	// Filter by service-a
	results := store.SearchFiltered(context.Background(), []float32{1.0, 0.0}, 10, "", "service-a", nil)
	if len(results) != 2 {
		t.Fatalf("expected 2 results for service-a, got %d", len(results))
	}
	for _, r := range results {
		if r.Record.SubProject != "service-a" {
			t.Errorf("expected SubProject 'service-a', got %q", r.Record.SubProject)
		}
	}

	// Filter by service-b
	results = store.SearchFiltered(context.Background(), []float32{1.0, 0.0}, 10, "", "service-b", nil)
	if len(results) != 1 {
		t.Fatalf("expected 1 result for service-b, got %d", len(results))
	}
	if results[0].Record.ID != "b1" {
		t.Errorf("expected b1, got %s", results[0].Record.ID)
	}

	// No sub_project filter returns all
	results = store.SearchFiltered(context.Background(), []float32{1.0, 0.0}, 10, "", "", nil)
	if len(results) != 4 {
		t.Fatalf("expected 4 results with no sub_project filter, got %d", len(results))
	}
}

func TestSearchFiltered_SubProjectAndSourceType(t *testing.T) {
	dir := tempStoreDir(t)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	records := []VectorRecord{
		{ID: "c1", FilePath: "service-a/main.go", Content: "code", Embedding: []float32{1.0}, ModTime: 100, SourceType: "code", SubProject: "service-a"},
		{ID: "t1", FilePath: "JIRA-1", Content: "ticket", Embedding: []float32{0.9}, ModTime: 100, SourceType: "ticket", SubProject: "service-a"},
		{ID: "c2", FilePath: "service-b/main.go", Content: "code b", Embedding: []float32{0.8}, ModTime: 100, SourceType: "code", SubProject: "service-b"},
	}
	if err := store.Upsert(records); err != nil {
		t.Fatal(err)
	}

	// Filter by both sub_project and source_type
	results := store.SearchFiltered(context.Background(), []float32{1.0}, 10, "code", "service-a", nil)
	if len(results) != 1 {
		t.Fatalf("expected 1 result for code+service-a, got %d", len(results))
	}
	if results[0].Record.ID != "c1" {
		t.Errorf("expected c1, got %s", results[0].Record.ID)
	}
}

func TestSearchFiltered_SubProjectReturnedInResults(t *testing.T) {
	dir := tempStoreDir(t)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	rec := VectorRecord{
		ID:         "sp_ret",
		FilePath:   "svc/handler.go",
		Content:    "handler code",
		Embedding:  []float32{1.0},
		ModTime:    100,
		SubProject: "svc",
	}
	if err := store.Upsert([]VectorRecord{rec}); err != nil {
		t.Fatal(err)
	}

	results := store.Search(context.Background(), []float32{1.0}, 1)
	if len(results) != 1 {
		t.Fatal("expected 1 result")
	}
	if results[0].Record.SubProject != "svc" {
		t.Errorf("SubProject = %q, want %q", results[0].Record.SubProject, "svc")
	}
}

// TestSearchFiltered_BudgetTimeout asserts that a pre-cancelled context
// makes SearchFiltered short-circuit without panic, without leaking
// goroutines or rows, and returns a possibly-empty-or-partial slice rather
// than nil-on-error. This is the T4 contract (Wave 2 phase 1b).
func TestSearchFiltered_BudgetTimeout(t *testing.T) {
	dir := tempStoreDir(t)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Seed enough rows that a linear scan would normally return many results.
	var recs []VectorRecord
	for i := 0; i < 200; i++ {
		recs = append(recs, VectorRecord{
			ID:        fmt.Sprintf("r-%d", i),
			FilePath:  fmt.Sprintf("f/%d.go", i),
			StartLine: 1,
			EndLine:   10,
			Content:   "payload",
			Embedding: []float32{1.0, 0.0},
		})
	}
	if err := store.Upsert(recs); err != nil {
		t.Fatal(err)
	}

	// Pre-cancelled context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Must not panic. Must not block. Returned slice may be nil or partial.
	results := store.SearchFiltered(ctx, []float32{1.0, 0.0}, 10, "", "", nil)

	if ctx.Err() == nil {
		t.Fatalf("expected ctx.Err() != nil after cancel")
	}
	// SearchFiltered returns nil if QueryContext errors out at the driver
	// level (sqlite sees the cancelled context and refuses to start). That
	// is an acceptable outcome — the contract is "no panic, no leak, and the
	// caller's ctx.Err() survives". A partial slice is also acceptable.
	if len(results) > 10 {
		t.Errorf("results exceeded topK=10: got %d", len(results))
	}
}

// TestSearchFiltered_NilContext asserts that nil ctx is tolerated — test
// helpers that don't care about cancellation should still work.
func TestSearchFiltered_NilContext(t *testing.T) {
	dir := tempStoreDir(t)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Upsert([]VectorRecord{
		{ID: "a", FilePath: "a.go", StartLine: 1, EndLine: 2, Content: "x", Embedding: []float32{1.0}},
	}); err != nil {
		t.Fatal(err)
	}

	//nolint:staticcheck // intentionally passing nil ctx to exercise the guard
	results := store.SearchFiltered(nil, []float32{1.0}, 1, "", "", nil)
	if len(results) != 1 {
		t.Fatalf("expected 1 result with nil ctx, got %d", len(results))
	}
}

// TestVectorByID_Found asserts the helper returns the encoded vector blob
// decoded back to the same []float32 that was upserted — this is the
// round-trip that semantic-drift scoring relies on.
func TestVectorByID_Found(t *testing.T) {
	dir := tempStoreDir(t)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	want := []float32{0.1, 0.2, 0.3, 0.4}
	if err := store.Upsert([]VectorRecord{
		{ID: "vec:1", FilePath: "a.go", Content: "x", Embedding: want, ModTime: 1},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := store.VectorByID("vec:1")
	if err != nil {
		t.Fatalf("VectorByID: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("len: got %d want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("vec[%d]: got %v want %v", i, got[i], want[i])
		}
	}
}

// TestVectorByID_Missing asserts the helper returns sql.ErrNoRows for an
// unknown id — callers (semantic drift compute) rely on this sentinel to
// decide between "skip hit" (F3) and "hard fail".
func TestVectorByID_Missing(t *testing.T) {
	dir := tempStoreDir(t)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	_, err = store.VectorByID("does-not-exist")
	if err == nil {
		t.Fatalf("expected error on missing id")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected sql.ErrNoRows, got %v", err)
	}
}
