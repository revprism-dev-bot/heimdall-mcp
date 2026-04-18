// llm_classifier_test.go — Layer 2 integration tests for the LLM
// classifier fallback. Plan 11 §7.2 table (7 tests). Uses net/http/httptest
// to stand up a fake Ollama server; exercises the real OllamaClient.Chat
// code path without any external dependency.
package heimdall

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newChatServer stands up a fake /api/chat endpoint that echoes the
// configured response. asserter runs on the captured request body before
// the response is sent — lets tests assert on keep_alive/format/options
// shape without polluting the main happy-path assertion.
type chatServerSpec struct {
	status   int
	respBody any
	sleep    time.Duration
	// asserter is called with the raw request body. Returning a non-nil
	// error fails the server-side check loudly so tests see the
	// mismatch at server-response time rather than silently succeeding.
	asserter func(t *testing.T, body []byte)
	// rawBodyText, if non-empty, is written instead of JSON-encoding
	// respBody. Lets tests simulate malformed server output (e.g. "not
	// json") without fighting encoding/json.
	rawBodyText string
}

func newChatServer(t *testing.T, spec chatServerSpec) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		if spec.asserter != nil {
			spec.asserter(t, b)
		}
		if spec.sleep > 0 {
			// Honor client-side cancellation so the timeout test doesn't
			// hang Go's test timeout.
			select {
			case <-time.After(spec.sleep):
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(spec.status)
		if spec.rawBodyText != "" {
			_, _ = w.Write([]byte(spec.rawBodyText))
			return
		}
		if spec.respBody != nil {
			_ = json.NewEncoder(w).Encode(spec.respBody)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// chatResponseBody is the minimal envelope tests use for the mock.
func chatResponseBody(classToken, reason string) ChatResponse {
	// content is the JSON string the model is instructed to emit. The
	// parser unmarshals content → {class, reason}.
	content, _ := json.Marshal(map[string]string{"class": classToken, "reason": reason})
	return ChatResponse{
		Model:   "test-model",
		Message: ChatMessage{Role: "assistant", Content: string(content)},
		Done:    true,
	}
}

// -----------------------------------------------------------------------
// L2-1 — Happy path: fake returns {"class":"warn",...} → ClassWarn with reason.
// -----------------------------------------------------------------------

func TestLLM_L2_1_HappyPath(t *testing.T) {
	srv := newChatServer(t, chatServerSpec{
		status:   http.StatusOK,
		respBody: chatResponseBody("warn", "destroys build artifacts"),
		asserter: func(t *testing.T, body []byte) {
			// Assert the request body carries the knobs plan 11 §7.2
			// asks us to validate: keep_alive, format, options.
			var raw map[string]any
			if err := json.Unmarshal(body, &raw); err != nil {
				t.Errorf("request body not valid JSON: %v\nbody=%s", err, string(body))
				return
			}
			if raw["keep_alive"] != "10m" {
				t.Errorf("keep_alive=%v, want 10m", raw["keep_alive"])
			}
			if raw["format"] == nil {
				t.Errorf("format field missing in request body")
			}
			if raw["stream"] != false {
				t.Errorf("stream=%v, want false", raw["stream"])
			}
			opts, _ := raw["options"].(map[string]any)
			if opts == nil {
				t.Fatal("options missing")
			}
			if opts["temperature"] != float64(0) {
				t.Errorf("temperature=%v, want 0", opts["temperature"])
			}
			if opts["seed"] != float64(42) {
				t.Errorf("seed=%v, want 42", opts["seed"])
			}
			if opts["num_predict"] != float64(LLMClassifierNumPredict) {
				t.Errorf("num_predict=%v, want %d", opts["num_predict"], LLMClassifierNumPredict)
			}
			// Sandwich fencing: user message must wrap the command.
			msgs, _ := raw["messages"].([]any)
			if len(msgs) < 2 {
				t.Fatalf("messages missing: %v", raw["messages"])
			}
			user, _ := msgs[1].(map[string]any)
			if got, _ := user["content"].(string); !strings.Contains(got, "<cmd>") || !strings.Contains(got, "</cmd>") {
				t.Errorf("user content must be fenced with <cmd>…</cmd>: %q", got)
			}
		},
	})

	classifier := NewOllamaLLMClassifier(NewOllamaClient(srv.URL), "test-model")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	class, reason, err := classifier.ClassifyBash(ctx, "make build")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if class != ClassWarn {
		t.Errorf("class = %v, want ClassWarn", class)
	}
	if reason != "destroys build artifacts" {
		t.Errorf("reason = %q", reason)
	}
}

// -----------------------------------------------------------------------
// L2-2 — Timeout: server sleeps past the client ctx deadline.
// -----------------------------------------------------------------------

func TestLLM_L2_2_Timeout(t *testing.T) {
	srv := newChatServer(t, chatServerSpec{
		status:   http.StatusOK,
		respBody: chatResponseBody("allow", "slow"),
		sleep:    500 * time.Millisecond,
	})
	classifier := NewOllamaLLMClassifier(NewOllamaClient(srv.URL), "test-model")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	class, _, err := classifier.ClassifyBash(ctx, "make build")
	if err == nil {
		t.Fatalf("expected timeout error, got nil")
	}
	if class != ClassAllow {
		t.Errorf("fail-open class = %v, want ClassAllow", class)
	}
	// Canonical timeout signal — the hook handler errors.Is-matches this.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded in chain", err)
	}
}

// -----------------------------------------------------------------------
// L2-3 — Server HTTP 500 → wrapped error, fail-open class.
// -----------------------------------------------------------------------

func TestLLM_L2_3_Server500(t *testing.T) {
	srv := newChatServer(t, chatServerSpec{
		status:      http.StatusInternalServerError,
		rawBodyText: `internal error`,
	})
	classifier := NewOllamaLLMClassifier(NewOllamaClient(srv.URL), "test-model")
	class, _, err := classifier.ClassifyBash(context.Background(), "make build")
	if err == nil {
		t.Fatalf("expected HTTP-500 error, got nil")
	}
	if class != ClassAllow {
		t.Errorf("fail-open class = %v, want ClassAllow", class)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("err should mention status 500: %v", err)
	}
}

// -----------------------------------------------------------------------
// L2-4 — Bad JSON envelope: server returns malformed outer JSON.
// -----------------------------------------------------------------------

func TestLLM_L2_4_BadEnvelopeJSON(t *testing.T) {
	srv := newChatServer(t, chatServerSpec{
		status:      http.StatusOK,
		rawBodyText: `not json at all`,
	})
	classifier := NewOllamaLLMClassifier(NewOllamaClient(srv.URL), "test-model")
	class, _, err := classifier.ClassifyBash(context.Background(), "make build")
	if err == nil {
		t.Fatalf("expected decode error, got nil")
	}
	if class != ClassAllow {
		t.Errorf("fail-open class = %v, want ClassAllow", class)
	}
}

// -----------------------------------------------------------------------
// L2-5 — Unknown class token inside a valid envelope.
// -----------------------------------------------------------------------

func TestLLM_L2_5_UnknownClassToken(t *testing.T) {
	srv := newChatServer(t, chatServerSpec{
		status:   http.StatusOK,
		respBody: chatResponseBody("mebbe", "unsure"),
	})
	classifier := NewOllamaLLMClassifier(NewOllamaClient(srv.URL), "test-model")
	class, _, err := classifier.ClassifyBash(context.Background(), "make build")
	if err == nil {
		t.Fatalf("expected ErrLLMBadResponse, got nil")
	}
	if !errors.Is(err, ErrLLMBadResponse) {
		t.Errorf("err must wrap ErrLLMBadResponse, got %v", err)
	}
	if class != ClassAllow {
		t.Errorf("fail-open class = %v, want ClassAllow", class)
	}
}

// -----------------------------------------------------------------------
// L2-6 — Reason truncation: 5 KB reason gets trimmed to 200 chars + ...
// -----------------------------------------------------------------------

func TestLLM_L2_6_ReasonTruncation(t *testing.T) {
	longReason := strings.Repeat("a", 5000)
	srv := newChatServer(t, chatServerSpec{
		status:   http.StatusOK,
		respBody: chatResponseBody("warn", longReason),
	})
	classifier := NewOllamaLLMClassifier(NewOllamaClient(srv.URL), "test-model")
	class, reason, err := classifier.ClassifyBash(context.Background(), "make build")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if class != ClassWarn {
		t.Errorf("class = %v, want ClassWarn", class)
	}
	// The classifier itself returns the raw (large) reason; truncation
	// happens in the hook-handler layer via TruncateReason. Layer 2's
	// contract is "we received the reason"; layer 1 covers the
	// truncation log event. Assert the helper, too, for belt-and-
	// suspenders.
	trimmed, didTrunc := TruncateReason(reason)
	if !didTrunc {
		t.Errorf("TruncateReason on %d-char input should flag truncation", len(reason))
	}
	if len(trimmed) != LLMClassifierReasonMaxChars {
		t.Errorf("trimmed length = %d, want %d", len(trimmed), LLMClassifierReasonMaxChars)
	}
	if !strings.HasSuffix(trimmed, "...") {
		t.Errorf("trimmed must end in ellipsis: %q", trimmed)
	}
}

// -----------------------------------------------------------------------
// L2-7 — Cancellation mid-call: caller cancels ctx, HTTP request drops
// and the classifier returns ctx.Err() without leaking the goroutine.
// -----------------------------------------------------------------------

func TestLLM_L2_7_Cancellation(t *testing.T) {
	// Server sleeps a long time — we cancel before it responds.
	srv := newChatServer(t, chatServerSpec{
		status:   http.StatusOK,
		respBody: chatResponseBody("allow", "never returns"),
		sleep:    2 * time.Second,
	})
	classifier := NewOllamaLLMClassifier(NewOllamaClient(srv.URL), "test-model")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var (
		gotClass Classification
		gotErr   error
	)
	go func() {
		gotClass, _, gotErr = classifier.ClassifyBash(ctx, "make build")
		close(done)
	}()

	// Give the request time to hit the server handler.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("classifier did not return after ctx cancel")
	}

	if gotErr == nil {
		t.Fatalf("expected cancellation error, got nil")
	}
	if gotClass != ClassAllow {
		t.Errorf("fail-open class = %v, want ClassAllow", gotClass)
	}
	if !errors.Is(gotErr, context.Canceled) && !errors.Is(gotErr, context.DeadlineExceeded) {
		t.Errorf("err should wrap context.Canceled, got %v", gotErr)
	}
}
