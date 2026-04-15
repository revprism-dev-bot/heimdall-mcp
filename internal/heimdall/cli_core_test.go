package heimdall

import (
	"bytes"
	"context"
	"encoding/binary"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestMemoryStore(t *testing.T) *MemoryStore {
	t.Helper()
	dir := t.TempDir()
	store, err := OpenMemoryStore(filepath.Join(dir, "memories.db"))
	if err != nil {
		t.Fatalf("open memory store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func seedMemory(t *testing.T, store *MemoryStore, id, content string, vec []float32, tags []string, project string) {
	t.Helper()
	now := time.Now().Unix()
	if err := store.UpsertMemory(Memory{
		ID:          id,
		Content:     content,
		Type:        MemoryTypeFact,
		Tags:        tags,
		Project:     project,
		Vector:      vec,
		CreatedAt:   now,
		UpdatedAt:   now,
		Source:      MemorySourceExplicit,
		ContentHash: ContentHash(content),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestRunRecall_HappyPath(t *testing.T) {
	store := newTestMemoryStore(t)
	seedMemory(t, store, "mem:a", "use TDD", []float32{1, 0, 0}, []string{"testing"}, "")
	seedMemory(t, store, "mem:b", "use Go", []float32{0, 1, 0}, nil, "")

	mock := &MockEmbedder{
		Vectors:   map[string][]float32{"prefers testing": {1, 0, 0}},
		Dimension: 3,
	}
	hits, err := RunRecall(context.Background(), RecallParams{
		Query: "prefers testing",
		Limit: 5,
	}, mock, store)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(hits))
	}
	if hits[0].ID != "mem:a" {
		t.Errorf("expected mem:a first (exact vector match), got %q", hits[0].ID)
	}
	if hits[0].Score <= hits[1].Score {
		t.Errorf("expected descending score, got %.3f vs %.3f", hits[0].Score, hits[1].Score)
	}
}

func TestRunRecall_EmptyStore(t *testing.T) {
	store := newTestMemoryStore(t)
	mock := &MockEmbedder{Dimension: 3}
	hits, err := RunRecall(context.Background(), RecallParams{
		Query: "anything",
		Limit: 5,
	}, mock, store)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("expected 0 hits, got %d", len(hits))
	}
}

func TestRunRecall_MissingQuery(t *testing.T) {
	store := newTestMemoryStore(t)
	mock := &MockEmbedder{Dimension: 3}
	if _, err := RunRecall(context.Background(), RecallParams{Query: "  "}, mock, store); err == nil {
		t.Fatal("expected error for empty query")
	}
}

func TestRunRecall_LimitClamped(t *testing.T) {
	store := newTestMemoryStore(t)
	mock := &MockEmbedder{Dimension: 3}
	// Should not panic; limit negative clamps to default.
	if _, err := RunRecall(context.Background(), RecallParams{Query: "q", Limit: -1}, mock, store); err != nil {
		t.Errorf("unexpected error on negative limit: %v", err)
	}
	// Limit above 100 clamps silently.
	if _, err := RunRecall(context.Background(), RecallParams{Query: "q", Limit: 500}, mock, store); err != nil {
		t.Errorf("unexpected error on large limit: %v", err)
	}
}

func TestRunRecall_ProjectFilter(t *testing.T) {
	store := newTestMemoryStore(t)
	seedMemory(t, store, "mem:a", "alpha", []float32{1, 0, 0}, nil, "p1")
	seedMemory(t, store, "mem:b", "beta", []float32{1, 0, 0}, nil, "p2")

	mock := &MockEmbedder{Vectors: map[string][]float32{"q": {1, 0, 0}}, Dimension: 3}
	hits, err := RunRecall(context.Background(), RecallParams{
		Query: "q", Limit: 5, Project: "p1",
	}, mock, store)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != "mem:a" {
		t.Errorf("project filter failed: got %+v", hits)
	}
}

func TestReadLengthPrefixedBuffer_Roundtrip(t *testing.T) {
	var buf bytes.Buffer
	payloads := []string{`{"turn":1}`, "multi\nline turn", `{"turn":3}`}
	for _, p := range payloads {
		var lenBuf [4]byte
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(p)))
		buf.Write(lenBuf[:])
		buf.WriteString(p)
	}

	got, err := ReadLengthPrefixedBuffer(&buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := strings.Join(payloads, "\n")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestReadLengthPrefixedBuffer_EmptyInput(t *testing.T) {
	got, err := ReadLengthPrefixedBuffer(bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestReadLengthPrefixedBuffer_TruncatedPrefix(t *testing.T) {
	_, err := ReadLengthPrefixedBuffer(bytes.NewReader([]byte{0, 1}))
	if err == nil {
		t.Fatal("expected error on truncated prefix")
	}
}

func TestReadLengthPrefixedBuffer_TruncatedPayload(t *testing.T) {
	var buf bytes.Buffer
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], 10)
	buf.Write(lenBuf[:])
	buf.WriteString("short") // only 5 bytes, need 10
	if _, err := ReadLengthPrefixedBuffer(&buf); err == nil {
		t.Fatal("expected error on truncated payload")
	}
}

func TestReadLengthPrefixedBuffer_ZeroLengthRecordsSkipped(t *testing.T) {
	var buf bytes.Buffer
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], 0)
	buf.Write(lenBuf[:])
	binary.BigEndian.PutUint32(lenBuf[:], 4)
	buf.Write(lenBuf[:])
	buf.WriteString("abcd")

	got, err := ReadLengthPrefixedBuffer(&buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "abcd" {
		t.Errorf("got %q, want %q", got, "abcd")
	}
}

func TestIngestSessionSummary_Empty(t *testing.T) {
	store := newTestMemoryStore(t)
	mock := &MockEmbedder{Dimension: 3}
	if _, err := IngestSessionSummary(context.Background(), "  ", "", mock, store); err == nil {
		t.Fatal("expected error for empty summary")
	}
}

func TestGatherStatus_NoIndex(t *testing.T) {
	baseDir := t.TempDir()
	info := GatherStatus(context.Background(), "http://127.0.0.1:0", "nomic", baseDir, []string{".git"}, nil)
	if info.OllamaRunning {
		t.Error("expected OllamaRunning false when client nil")
	}
	if info.LastIndexed != "no index" {
		t.Errorf("expected 'no index', got %q", info.LastIndexed)
	}
	if info.Model != "nomic" {
		t.Errorf("model not propagated: %q", info.Model)
	}
}
