package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// fakeOllamaServer serves the two endpoints the harness touches:
// /api/tags (Ping) and /api/embed. Returns deterministic vectors keyed
// on input text so the Layer-2 test matches the hand-worked expected
// matrix in TestCalibrate_FixtureMatrix.
func fakeOllamaServer(t *testing.T, vectors map[string][]float32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"nomic-embed-text"}]}`))
	})
	mux.HandleFunc("/api/embed", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string      `json:"model"`
			Input interface{} `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var texts []string
		switch v := req.Input.(type) {
		case string:
			texts = []string{v}
		case []interface{}:
			for _, x := range v {
				texts = append(texts, fmt.Sprint(x))
			}
		}
		out := make([][]float32, 0, len(texts))
		for _, text := range texts {
			vec, ok := vectors[text]
			if !ok {
				// Default to zero vector to exercise the below-threshold path
				// on unexpected inputs — mirrors plan 12 §7 F4 degradation
				// without actually tripping it (len matches).
				vec = make([]float32, 3)
			}
			out = append(out, vec)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"embeddings": out})
	})
	return httptest.NewServer(mux)
}

// setupStore populates a VectorStore with the three test chunks the
// fixture relies on. Vectors are hand-chosen so cosine hits exact
// rational values (1.0, 0.0) — see TestCalibrate_FixtureMatrix body
// for the expected-matrix derivation.
func setupStore(t *testing.T) (string, *heimdall.VectorStore) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".heimdall_db")
	store, err := heimdall.OpenStore(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	records := []heimdall.VectorRecord{
		{
			ID: "hit1", FilePath: "x/a.go", StartLine: 1, EndLine: 10,
			Content: "hit1 content", Embedding: []float32{1, 0, 0},
			Kind: "function", ModTime: 1,
		},
		{
			ID: "hit2", FilePath: "x/b.go", StartLine: 1, EndLine: 10,
			Content: "hit2 content", Embedding: []float32{0, 1, 0},
			Kind: "function", ModTime: 1,
		},
		{
			ID: "chunkZ", FilePath: "x/c.go", StartLine: 1, EndLine: 10,
			Content: "chunkZ content", Embedding: []float32{0, 1, 0},
			Kind: "function", ModTime: 1,
		},
	}
	if err := store.Upsert(records); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	return dir, store
}

// writeHooksLogWithPromptVectors synthesizes a hooks.log with
// `event=user-prompt stage=ok` lines carrying `prompt_embed_b64` for
// each (session, turn) pair. This is what Stage 1 of plan 12 will
// produce in production; generating it here lets the harness exercise
// its real hooks.log path without depending on Stage 1 having landed.
func writeHooksLogWithPromptVectors(t *testing.T, dir string, entries []hookLogEntry) string {
	t.Helper()
	var buf bytes.Buffer
	for _, e := range entries {
		pvecB64 := base64.StdEncoding.EncodeToString(heimdall.EncodeFloat32Vec(e.PromptVec))
		line := fmt.Sprintf("%s INFO event=user-prompt session=%s stage=ok prompt_embed_b64=%s\n",
			e.Timestamp, e.SessionID, pvecB64)
		buf.WriteString(line)
	}
	path := filepath.Join(dir, "hooks.log")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write hooks.log: %v", err)
	}
	return path
}

type hookLogEntry struct {
	Timestamp string
	SessionID string
	PromptVec []float32
}

// stdFixtureVectors is the single source of truth for every test
// vector used by the harness layer-2 tests. Keyed so the fake Ollama
// and the fixture hooks.log stay in sync.
var stdFixtureVectors = map[string][]float32{
	"qClose":     {1, 0, 0},
	"qUnrelated": {0, 0, 1},
	"qNotClose":  {0, 0, 1},
}

// stdPromptEntries is the per-turn prompt-vector set the hooks.log
// fixture needs. Position in the slice == turn_idx for that session.
var stdPromptEntries = []hookLogEntry{
	{Timestamp: "2026-04-18T10:00:00Z", SessionID: "sA", PromptVec: []float32{1, 0, 0}}, // pA
	{Timestamp: "2026-04-18T10:01:00Z", SessionID: "sA", PromptVec: []float32{0, 1, 0}}, // pB
	{Timestamp: "2026-04-18T10:02:00Z", SessionID: "sA", PromptVec: []float32{0, 0, 1}}, // pC
	{Timestamp: "2026-04-18T10:03:00Z", SessionID: "sB", PromptVec: []float32{1, 0, 0}}, // pD
	{Timestamp: "2026-04-18T10:04:00Z", SessionID: "sB", PromptVec: []float32{1, 0, 0}}, // pE
}

// TestCalibrate_FixtureMatrix is the headline Layer-2 test. Runs the
// harness end-to-end over the committed fixture sample with a real
// VectorStore, a httptest-backed fake Ollama, and an assembled hooks.log
// — then asserts the exact confusion matrix at T1=0.55 / T2=0.45. If
// anything about vector lookup, query re-embed, or the scoring rules
// drifts, this test breaks.
func TestCalibrate_FixtureMatrix(t *testing.T) {
	dir, store := setupStore(t)
	defer store.Close()

	srv := fakeOllamaServer(t, stdFixtureVectors)
	defer srv.Close()

	tmpDir := t.TempDir()
	hooksLog := writeHooksLogWithPromptVectors(t, tmpDir, stdPromptEntries)

	cfg := runConfig{
		LabelsPath:   "fixtures/small_sample.jsonl",
		HooksLogPath: hooksLog,
		DBDir:        dir,
		Model:        "nomic-embed-text",
		Ollama:       srv.URL,
		T1Min:        0.55, T1Max: 0.55, T1Step: 0.05,
		T2Min: 0.45, T2Max: 0.45, T2Step: 0.05,
		Format: "json",
	}

	client := heimdall.NewOllamaClient(srv.URL)
	embedder := heimdall.NewOllamaEmbedder(client, "nomic-embed-text")

	var buf bytes.Buffer
	if err := runCalibrate(context.Background(), cfg, store, embedder, &buf); err != nil {
		t.Fatalf("runCalibrate: %v", err)
	}

	var report GridReport
	if err := json.Unmarshal(buf.Bytes(), &report); err != nil {
		t.Fatalf("json unmarshal: %v\n%s", err, buf.String())
	}
	if got, want := len(report.Grid), 1; got != want {
		t.Fatalf("grid len = %d, want %d", got, want)
	}
	pt := report.Grid[0]
	// Redundant bucket: turn 0 is truly_redundant + detected; turn 4 is
	// truly_redundant + missed by detector. Turns 1,2,3 are not-
	// redundant and correctly not detected.
	if pt.TPRedundant != 1 || pt.FPRedundant != 0 || pt.FNRedundant != 1 || pt.TNRedundant != 3 {
		t.Errorf("redundant matrix TP/FP/FN/TN = %d/%d/%d/%d, want 1/0/1/3",
			pt.TPRedundant, pt.FPRedundant, pt.FNRedundant, pt.TNRedundant)
	}
	// Missed bucket: turn 1 is truly_missed and detected; rest are
	// correctly not detected.
	if pt.TPMissed != 1 || pt.FPMissed != 0 || pt.FNMissed != 0 || pt.TNMissed != 4 {
		t.Errorf("missed matrix TP/FP/FN/TN = %d/%d/%d/%d, want 1/0/0/4",
			pt.TPMissed, pt.FPMissed, pt.FNMissed, pt.TNMissed)
	}
	if pt.NTurnsScored != 5 {
		t.Errorf("NTurnsScored = %d, want 5", pt.NTurnsScored)
	}
	if pt.PrecisionRedundant != 1.0 {
		t.Errorf("PrecisionRedundant = %f, want 1.0", pt.PrecisionRedundant)
	}
	if pt.PrecisionMissed != 1.0 {
		t.Errorf("PrecisionMissed = %f, want 1.0", pt.PrecisionMissed)
	}
	if got, want := report.Metadata.NTurnsTotal, 5; got != want {
		t.Errorf("NTurnsTotal = %d, want %d", got, want)
	}
	if report.Metadata.NTurnsSkipped != 0 {
		t.Errorf("NTurnsSkipped = %d, want 0", report.Metadata.NTurnsSkipped)
	}
}

// TestCalibrate_Deterministic is the byte-identical-output guarantee
// from the harness brief. Two runs over the same inputs must produce
// the same CSV. The fixture lives under fixtures/ so a `diff` between
// the fresh run and fixtures/expected_grid.csv is the same check a
// follow-up `make calibrate-drift-smoke` target will use.
func TestCalibrate_Deterministic(t *testing.T) {
	dir, store := setupStore(t)
	defer store.Close()

	srv := fakeOllamaServer(t, stdFixtureVectors)
	defer srv.Close()

	tmpDir := t.TempDir()
	hooksLog := writeHooksLogWithPromptVectors(t, tmpDir, stdPromptEntries)

	cfg := runConfig{
		LabelsPath:   "fixtures/small_sample.jsonl",
		HooksLogPath: hooksLog,
		DBDir:        dir,
		Model:        "nomic-embed-text",
		Ollama:       srv.URL,
		T1Min:        0.40, T1Max: 0.70, T1Step: 0.05,
		T2Min: 0.35, T2Max: 0.55, T2Step: 0.05,
		Format: "csv",
	}

	client := heimdall.NewOllamaClient(srv.URL)
	embedder := heimdall.NewOllamaEmbedder(client, "nomic-embed-text")

	var buf1, buf2 bytes.Buffer
	if err := runCalibrate(context.Background(), cfg, store, embedder, &buf1); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if err := runCalibrate(context.Background(), cfg, store, embedder, &buf2); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if !bytes.Equal(buf1.Bytes(), buf2.Bytes()) {
		t.Fatalf("determinism failure: byte diff between runs\nrun1:\n%s\nrun2:\n%s",
			buf1.String(), buf2.String())
	}

	// Golden file check: the committed fixtures/expected_grid.csv
	// must match byte-for-byte. If the derivation changes intentionally
	// the operator regenerates the golden via
	// `go test ./cmd/calibrate-drift -run Deterministic -update` —
	// support for that flag is a two-line follow-up if needed.
	golden, err := os.ReadFile("fixtures/expected_grid.csv")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(golden, buf1.Bytes()) {
		t.Fatalf("golden mismatch:\n--- golden ---\n%s\n--- got ---\n%s",
			string(golden), buf1.String())
	}
}

// TestCalibrate_FallbackReEmbed verifies that when hooks.log lacks
// `prompt_embed_b64` for a turn, the harness falls back to reading the
// prompt text — but only for turns that the fake Ollama can fulfill.
// Today's implementation drops prompt-less turns from NTurnsScored
// rather than re-embedding (simpler + no silent data fabrication). The
// test asserts that drop happens instead of a crash. If a future
// revision adds re-embed fallback the test should flip its expectation.
func TestCalibrate_FallbackNoPromptVec(t *testing.T) {
	dir, store := setupStore(t)
	defer store.Close()

	srv := fakeOllamaServer(t, stdFixtureVectors)
	defer srv.Close()

	// Empty hooks.log → no pvec for any turn.
	tmpDir := t.TempDir()
	emptyLog := filepath.Join(tmpDir, "hooks.log")
	if err := os.WriteFile(emptyLog, nil, 0o600); err != nil {
		t.Fatalf("write empty log: %v", err)
	}

	cfg := runConfig{
		LabelsPath:   "fixtures/small_sample.jsonl",
		HooksLogPath: emptyLog,
		DBDir:        dir,
		Model:        "nomic-embed-text",
		Ollama:       srv.URL,
		T1Min:        0.55, T1Max: 0.55, T1Step: 0.05,
		T2Min: 0.45, T2Max: 0.45, T2Step: 0.05,
		Format: "json",
	}
	client := heimdall.NewOllamaClient(srv.URL)
	embedder := heimdall.NewOllamaEmbedder(client, "nomic-embed-text")

	var buf bytes.Buffer
	if err := runCalibrate(context.Background(), cfg, store, embedder, &buf); err != nil {
		t.Fatalf("runCalibrate: %v", err)
	}
	var report GridReport
	if err := json.Unmarshal(buf.Bytes(), &report); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if report.Metadata.NTurnsSkipped != 5 {
		t.Errorf("NTurnsSkipped = %d, want 5 (all turns skipped with no pvec)",
			report.Metadata.NTurnsSkipped)
	}
	if got := report.Grid[0].NTurnsScored; got != 0 {
		t.Errorf("NTurnsScored = %d, want 0", got)
	}
}

// TestCalibrate_CSVHeaderPresent confirms the CSV emitter writes the
// fixed column header used downstream by spreadsheets + the Stage 2→3
// artifact pipeline. Regressing column order or renaming a field is a
// break that needs a deliberate bump.
func TestCalibrate_CSVHeaderPresent(t *testing.T) {
	dir, store := setupStore(t)
	defer store.Close()
	srv := fakeOllamaServer(t, stdFixtureVectors)
	defer srv.Close()
	tmpDir := t.TempDir()
	hooksLog := writeHooksLogWithPromptVectors(t, tmpDir, stdPromptEntries)

	cfg := runConfig{
		LabelsPath:   "fixtures/small_sample.jsonl",
		HooksLogPath: hooksLog,
		DBDir:        dir,
		Model:        "nomic-embed-text",
		Ollama:       srv.URL,
		T1Min:        0.55, T1Max: 0.55, T1Step: 0.05,
		T2Min: 0.45, T2Max: 0.45, T2Step: 0.05,
		Format: "csv",
	}
	client := heimdall.NewOllamaClient(srv.URL)
	embedder := heimdall.NewOllamaEmbedder(client, "nomic-embed-text")
	var buf bytes.Buffer
	if err := runCalibrate(context.Background(), cfg, store, embedder, &buf); err != nil {
		t.Fatalf("runCalibrate: %v", err)
	}
	header := strings.SplitN(buf.String(), "\n", 2)[0]
	wantCols := []string{"t1", "t2", "precision_redundant", "recall_redundant", "f1_redundant", "precision_missed", "recall_missed", "f1_missed"}
	for _, col := range wantCols {
		if !strings.Contains(header, col) {
			t.Errorf("CSV header missing %q; got %q", col, header)
		}
	}
}
