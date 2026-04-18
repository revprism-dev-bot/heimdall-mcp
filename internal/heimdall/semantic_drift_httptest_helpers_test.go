package heimdall

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// httptestServerWithBodyHandler lets drift tests inspect the batch request
// body (so they can echo back exactly len(Input) vectors) without
// reinventing the newBatchServer / captureEmbedServer pattern in
// ollama_test.go. The body handler returns a response payload that's
// JSON-encoded into the response.
func httptestServerWithBodyHandler(t *testing.T, bodyFn func([]byte) EmbedResponse) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		resp := bodyFn(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// httptestServerSlow stalls for `delay` before returning a minimal success
// response. Used by cancellation propagation tests — cancel the ctx before
// the delay elapses so the embedder receives a context.Canceled.
func httptestServerSlow(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			http.NotFound(w, r)
			return
		}
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_ = json.NewEncoder(w).Encode(EmbedResponse{
			Embeddings: [][]float32{{1, 0, 0}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}
