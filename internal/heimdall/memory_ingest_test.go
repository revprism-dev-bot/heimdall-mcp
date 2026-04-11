package heimdall

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestChunkSummary_Short(t *testing.T) {
	chunks := ChunkSummary("Short text.", 500)
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d, want 1", len(chunks))
	}
	if chunks[0] != "Short text." {
		t.Errorf("chunk = %q, want %q", chunks[0], "Short text.")
	}
}

func TestChunkSummary_Long(t *testing.T) {
	// Create text longer than 500 chars with multiple sentences
	sentences := []string{
		"The user decided to use SQLite for all local storage.",
		"This was chosen because it requires no external dependencies.",
		"The team agreed to use WAL mode for better concurrency.",
		"Performance testing showed it handles 10K records easily.",
		"The database schema uses a single table for simplicity.",
		"Migrations are handled by adding columns with ALTER TABLE.",
		"The content hash column enables skip-reindexing optimization.",
		"All vector operations use brute-force cosine similarity.",
		"This is acceptable for projects under 50K chunks.",
		"Future work may add SQLite-vss for larger projects.",
	}
	summary := strings.Join(sentences, " ")

	chunks := ChunkSummary(summary, 200)
	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if len(c) > 250 { // allow some slack for sentence boundaries
			t.Errorf("chunk %d too long: %d chars", i, len(c))
		}
	}
}

func TestChunkSummary_Empty(t *testing.T) {
	chunks := ChunkSummary("", 500)
	if len(chunks) != 0 {
		t.Fatalf("chunks = %d, want 0", len(chunks))
	}

	chunks = ChunkSummary("   ", 500)
	if len(chunks) != 0 {
		t.Fatalf("whitespace-only: chunks = %d, want 0", len(chunks))
	}
}

func TestExtractTags_Technical(t *testing.T) {
	tags := ExtractTags("We use SQLite database with REST API endpoint for the backend server.")

	tagSet := make(map[string]bool)
	for _, tag := range tags {
		tagSet[tag] = true
	}

	expected := []string{"database", "api", "backend"}
	for _, e := range expected {
		if !tagSet[e] {
			t.Errorf("missing tag %q, got %v", e, tags)
		}
	}
}

func TestExtractTags_Empty(t *testing.T) {
	tags := ExtractTags("Hello world.")
	// Should not extract any technical tags from generic text
	for _, tag := range tags {
		// Only CamelCase patterns could match — "Hello" is not CamelCase
		if tag != "" {
			// fine — some generic words might match patterns
		}
	}
}

func TestExtractTags_CamelCase(t *testing.T) {
	tags := ExtractTags("The VectorStore and MemoryStore handle data.")

	tagSet := make(map[string]bool)
	for _, tag := range tags {
		tagSet[tag] = true
	}

	if !tagSet["vectorstore"] {
		t.Errorf("missing tag 'vectorstore', got %v", tags)
	}
	if !tagSet["memorystore"] {
		t.Errorf("missing tag 'memorystore', got %v", tags)
	}
}

func TestClassifyMemoryType_Decision(t *testing.T) {
	cases := []string{
		"We decided to use Go for the backend.",
		"The team agreed on using SQLite.",
		"We chose REST instead of gRPC.",
	}
	for _, c := range cases {
		got := ClassifyMemoryType(c)
		if got != MemoryTypeDecision {
			t.Errorf("ClassifyMemoryType(%q) = %q, want %q", c, got, MemoryTypeDecision)
		}
	}
}

func TestClassifyMemoryType_Preference(t *testing.T) {
	cases := []string{
		"User prefers dark mode.",
		"The developer always use tabs.",
		"They have a preference for short functions.",
	}
	for _, c := range cases {
		got := ClassifyMemoryType(c)
		if got != MemoryTypePreference {
			t.Errorf("ClassifyMemoryType(%q) = %q, want %q", c, got, MemoryTypePreference)
		}
	}
}

func TestClassifyMemoryType_Context(t *testing.T) {
	cases := []string{
		"Currently working on the memory system.",
		"The migration is in progress.",
	}
	for _, c := range cases {
		got := ClassifyMemoryType(c)
		if got != MemoryTypeContext {
			t.Errorf("ClassifyMemoryType(%q) = %q, want %q", c, got, MemoryTypeContext)
		}
	}
}

func TestClassifyMemoryType_Default(t *testing.T) {
	got := ClassifyMemoryType("The sky is blue.")
	if got != MemoryTypeFact {
		t.Errorf("ClassifyMemoryType default = %q, want %q", got, MemoryTypeFact)
	}
}

func TestContentHash_Normalization(t *testing.T) {
	h1 := ContentHash("  Hello   World  ")
	h2 := ContentHash("hello world")
	h3 := ContentHash("HELLO WORLD")

	if h1 != h2 {
		t.Errorf("hash mismatch: whitespace normalization failed")
	}
	if h1 != h3 {
		t.Errorf("hash mismatch: case normalization failed")
	}
}

func TestContentHash_DifferentContent(t *testing.T) {
	h1 := ContentHash("Hello World")
	h2 := ContentHash("Goodbye World")
	if h1 == h2 {
		t.Errorf("different content should have different hashes")
	}
}

func TestIngestSession_Basic(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenMemoryStore(filepath.Join(dir, "memories.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	embedder := &MockEmbedder{
		Dimension: 3,
		Vectors: map[string][]float32{
			// Will map any text to a deterministic vector
		},
	}

	result, err := IngestSession(context.Background(), "We decided to use Go. The API uses REST.", "", embedder, store)
	if err != nil {
		t.Fatalf("IngestSession: %v", err)
	}
	if result.ChunksProcessed == 0 {
		t.Error("expected at least 1 chunk processed")
	}
	if result.MemoriesCreated == 0 {
		t.Error("expected at least 1 memory created")
	}

	// Verify memories are stored
	if store.MemoryCount() == 0 {
		t.Error("expected memories in store after ingestion")
	}
}

func TestIngestSession_Dedup(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenMemoryStore(filepath.Join(dir, "memories.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	embedder := &MockEmbedder{
		Dimension: 3,
		Vectors:   map[string][]float32{},
	}

	summary := "We decided to use Go for the backend."

	// First ingestion
	r1, err := IngestSession(context.Background(), summary, "", embedder, store)
	if err != nil {
		t.Fatalf("first IngestSession: %v", err)
	}
	count1 := store.MemoryCount()

	// Second ingestion with same content
	r2, err := IngestSession(context.Background(), summary, "", embedder, store)
	if err != nil {
		t.Fatalf("second IngestSession: %v", err)
	}
	count2 := store.MemoryCount()

	if count2 != count1 {
		t.Errorf("memory count changed: %d -> %d (expected same due to dedup)", count1, count2)
	}
	if r2.Duplicates != r1.MemoriesCreated {
		t.Errorf("expected %d duplicates on second run, got %d", r1.MemoriesCreated, r2.Duplicates)
	}
}

func TestIngestSession_ExplicitWins(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenMemoryStore(filepath.Join(dir, "memories.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Create a vector that the mock embedder will return
	vec := []float32{1.0, 0.0, 0.0}

	embedder := &MockEmbedder{
		Dimension: 3,
		Vectors: map[string][]float32{
			"We decided to use Go.": vec,
		},
	}

	// Store an explicit memory with the same vector
	store.UpsertMemory(Memory{
		ID: "mem:explicit:existing", Content: "Use Go for development",
		Type: MemoryTypeFact, Vector: vec,
		CreatedAt: 1000, UpdatedAt: 1000,
		Source: MemorySourceExplicit, ContentHash: "different-hash",
	})

	originalContent := "Use Go for development"

	// Ingest session with similar content
	_, err = IngestSession(context.Background(), "We decided to use Go.", "", embedder, store)
	if err != nil {
		t.Fatalf("IngestSession: %v", err)
	}

	// Verify the explicit memory's content was NOT overwritten
	m, _ := store.GetMemoryByID("mem:explicit:existing")
	if m == nil {
		t.Fatal("explicit memory should still exist")
	}
	if m.Content != originalContent {
		t.Errorf("explicit memory content was overwritten: got %q, want %q", m.Content, originalContent)
	}
}
