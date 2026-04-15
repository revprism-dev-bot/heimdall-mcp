package heimdall

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
