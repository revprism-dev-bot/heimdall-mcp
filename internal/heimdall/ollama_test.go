package heimdall

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newBatchServer stands up a fake Ollama endpoint that returns a
// user-supplied response body and status code for /api/embed.
func newBatchServer(t *testing.T, status int, body any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body != nil {
			_ = json.NewEncoder(w).Encode(body)
		}
	}))
}

// captureEmbedServer records the last /api/embed POST body so tests can
// assert on presence / absence of the keep_alive field.
func captureEmbedServer(t *testing.T, status int, resp any) (*httptest.Server, *[]byte) {
	t.Helper()
	var lastBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		lastBody = b
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if resp != nil {
			_ = json.NewEncoder(w).Encode(resp)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &lastBody
}

func TestOllamaClient_EmbedBatch_HappyPath(t *testing.T) {
	srv := newBatchServer(t, http.StatusOK, EmbedResponse{
		Embeddings: [][]float32{{1, 0}, {0, 1}, {0.5, 0.5}},
	})
	defer srv.Close()

	client := NewOllamaClient(srv.URL)
	vecs, err := client.EmbedBatch(context.Background(), "m", []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vecs) != 3 {
		t.Fatalf("expected 3 vectors, got %d", len(vecs))
	}
	if vecs[0][0] != 1 || vecs[1][1] != 1 {
		t.Errorf("vectors not returned in order: %v", vecs)
	}
}

func TestOllamaClient_EmbedBatch_Empty(t *testing.T) {
	// No server needed — the empty-batch path returns before any HTTP call.
	client := NewOllamaClient("http://127.0.0.1:0")
	vecs, err := client.EmbedBatch(context.Background(), "m", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vecs != nil {
		t.Errorf("expected nil vectors for empty batch, got %v", vecs)
	}
}

func TestOllamaClient_EmbedBatch_SingleTextUsesSingleEmbed(t *testing.T) {
	// The single-text fast path delegates to Embed, which returns
	// Embeddings[0]. Use the same server so both code paths hit /api/embed.
	srv := newBatchServer(t, http.StatusOK, EmbedResponse{
		Embeddings: [][]float32{{0.25, 0.75}},
	})
	defer srv.Close()

	client := NewOllamaClient(srv.URL)
	vecs, err := client.EmbedBatch(context.Background(), "m", []string{"only"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vecs) != 1 || vecs[0][1] != 0.75 {
		t.Errorf("single-text path returned unexpected vectors: %v", vecs)
	}
}

func TestOllamaClient_EmbedBatch_HTTPError(t *testing.T) {
	srv := newBatchServer(t, http.StatusInternalServerError, map[string]string{"error": "boom"})
	defer srv.Close()

	client := NewOllamaClient(srv.URL)
	_, err := client.EmbedBatch(context.Background(), "m", []string{"a", "b"})
	if err == nil {
		t.Fatal("expected error on 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status code, got: %v", err)
	}
}

func TestOllamaClient_EmbedBatch_CountMismatch(t *testing.T) {
	srv := newBatchServer(t, http.StatusOK, EmbedResponse{
		Embeddings: [][]float32{{1, 0}}, // asked for 3, got 1
	})
	defer srv.Close()

	client := NewOllamaClient(srv.URL)
	_, err := client.EmbedBatch(context.Background(), "m", []string{"a", "b", "c"})
	if err == nil {
		t.Fatal("expected error on count mismatch")
	}
	if !strings.Contains(err.Error(), "expected 3") {
		t.Errorf("error should name expected count, got: %v", err)
	}
}

func TestOllamaClient_EmbedBatch_RejectsOversizedBatch(t *testing.T) {
	// Should fail before making any HTTP call, so an unreachable endpoint is fine.
	client := NewOllamaClient("http://127.0.0.1:0")
	texts := make([]string, EmbedBatchSize+1)
	for i := range texts {
		texts[i] = "t"
	}
	_, err := client.EmbedBatch(context.Background(), "m", texts)
	if err == nil {
		t.Fatal("expected error for oversized batch")
	}
	if !strings.Contains(err.Error(), "exceeds max") {
		t.Errorf("error should mention max size, got: %v", err)
	}
}

func TestOllamaEmbed_OmitsKeepAliveOnInteractivePath(t *testing.T) {
	srv, bodyPtr := captureEmbedServer(t, http.StatusOK, EmbedResponse{
		Embeddings: [][]float32{{1, 0, 0}},
	})

	client := NewOllamaClient(srv.URL)
	vec, err := client.Embed(context.Background(), "nomic-embed-text", "hello")
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(vec) != 3 {
		t.Errorf("unexpected vec: %v", vec)
	}

	var parsed map[string]any
	if err := json.Unmarshal(*bodyPtr, &parsed); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if _, ok := parsed["keep_alive"]; ok {
		t.Errorf("Embed must not send keep_alive, body=%s", string(*bodyPtr))
	}
}

func TestOllamaEmbed_SendsKeepAliveOnHookPath(t *testing.T) {
	srv, bodyPtr := captureEmbedServer(t, http.StatusOK, EmbedResponse{
		Embeddings: [][]float32{{1, 0, 0}},
	})

	client := NewOllamaClient(srv.URL)
	_, err := client.EmbedForHook(context.Background(), "nomic-embed-text", "hello")
	if err != nil {
		t.Fatalf("embed-for-hook: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(*bodyPtr, &parsed); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	ka, ok := parsed["keep_alive"]
	if !ok {
		t.Fatalf("EmbedForHook must send keep_alive, body=%s", string(*bodyPtr))
	}
	if ka != HookKeepAlive {
		t.Errorf("keep_alive: got %v want %q", ka, HookKeepAlive)
	}
}

// fakeModelLister implements heimdall.modelLister (via interface duck-typing
// within the package) so ResolveUsableModelDB can run without real Ollama.
type fakeModelLister struct {
	models []ModelInfo
	err    error
}

func (f *fakeModelLister) ListModels(_ context.Context) ([]ModelInfo, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.models, nil
}

func TestResolveUsableModelDB(t *testing.T) {
	type setup struct {
		dirs   []string // subdirs to create with a vectors.db sentinel inside
		models []string // models "pulled" in fake Ollama
	}
	cases := []struct {
		name       string
		setup      setup
		preferred  string
		ollamaDown bool
		wantModel  string
		wantEmpty  bool
	}{
		{
			name:       "ollama down returns empty",
			setup:      setup{dirs: []string{"nomic-embed-text"}, models: []string{"nomic-embed-text:latest"}},
			preferred:  "nomic-embed-text",
			ollamaDown: true,
			wantEmpty:  true,
		},
		{
			name:      "preferred pulled and indexed",
			setup:     setup{dirs: []string{"nomic-embed-text", "mxbai-embed-large"}, models: []string{"nomic-embed-text:latest", "mxbai-embed-large:latest"}},
			preferred: "nomic-embed-text",
			wantModel: "nomic-embed-text",
		},
		{
			name:      "preferred not pulled, falls back to any available",
			setup:     setup{dirs: []string{"nomic-embed-text", "mxbai-embed-large"}, models: []string{"mxbai-embed-large:latest"}},
			preferred: "nomic-embed-text",
			wantModel: "mxbai-embed-large",
		},
		{
			name:      "legacy _latest suffix is resolved",
			setup:     setup{dirs: []string{"nomic-embed-text_latest"}, models: []string{"nomic-embed-text:latest"}},
			preferred: "nomic-embed-text",
			wantModel: "nomic-embed-text",
		},
		{
			name:      "no indexes at all",
			setup:     setup{},
			preferred: "nomic-embed-text",
			wantEmpty: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			for _, d := range tc.setup.dirs {
				dir := filepath.Join(base, d)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "vectors.db"), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var lister modelLister = &fakeModelLister{
				models: toModelInfos(tc.setup.models),
			}
			if tc.ollamaDown {
				lister = &fakeModelLister{err: errBoom}
			}

			gotDir, gotModel := ResolveUsableModelDB(context.Background(), lister, base, tc.preferred)
			if tc.wantEmpty {
				if gotDir != "" || gotModel != "" {
					t.Errorf("expected empty result, got dir=%q model=%q", gotDir, gotModel)
				}
				return
			}
			if gotModel != tc.wantModel {
				t.Errorf("model: got %q want %q", gotModel, tc.wantModel)
			}
			if gotDir == "" {
				t.Error("expected non-empty dir")
			}
		})
	}
}

func toModelInfos(names []string) []ModelInfo {
	out := make([]ModelInfo, len(names))
	for i, n := range names {
		out[i] = ModelInfo{Name: n}
	}
	return out
}

var errBoom = &stringError{"ollama down"}

type stringError struct{ s string }

func (e *stringError) Error() string { return e.s }
