package heimdall

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OllamaClient talks to the Ollama REST API.
type OllamaClient struct {
	endpoint   string
	httpClient *http.Client
}

// NewOllamaClient creates a client for the given endpoint (e.g. "http://localhost:11434").
// The shared http.Client has no hard timeout — per-call deadlines are expected
// to come from the caller's context, which lets batch embeds use a longer
// deadline than single-text embeds while still sharing one connection pool.
func NewOllamaClient(endpoint string) *OllamaClient {
	return &OllamaClient{
		endpoint:   endpoint,
		httpClient: &http.Client{},
	}
}

// EmbedBatchSize is the maximum number of texts to send in a single batch
// embedding request. Keeps Ollama memory usage manageable.
const EmbedBatchSize = 32

// HookKeepAlive is the model residency hint sent on every embed request
// issued from the hook path. Keeps the model hot between fast-fire hook
// invocations so cold starts don't dominate the 250 ms p95 budget.
// See docs/plans/hooks §5.6 for the rationale.
const HookKeepAlive = "10m"

// EmbedRequest is the POST body for /api/embed.
//
// KeepAlive maps to Ollama's optional `keep_alive` field; when non-empty it
// overrides Ollama's default model-residency TTL. The interactive MCP path
// leaves this empty so it doesn't force GPU memory to stay resident between
// calls; the hook path sets it via the EmbedForHook wrapper below.
type EmbedRequest struct {
	Model     string `json:"model"`
	Input     string `json:"input"`
	KeepAlive string `json:"keep_alive,omitempty"`
}

// EmbedBatchRequest is the POST body for /api/embed (multiple texts).
type EmbedBatchRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// EmbedResponse is the response from /api/embed.
type EmbedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

// Embed generates an embedding vector for the given text. The interactive
// path — no keep_alive, so Ollama applies its default residency.
func (c *OllamaClient) Embed(ctx context.Context, model, text string) ([]float32, error) {
	return c.embed(ctx, EmbedRequest{Model: model, Input: text})
}

// EmbedForHook is the hook-path embed entry point. It sets keep_alive: "10m"
// so the model stays resident between fast-fire hook invocations. Keeping
// this separate from Embed preserves the clean request shape for the
// interactive MCP server, which shouldn't pay the keep-alive cost on every
// call — see docs/plans/hooks §5.6.
func (c *OllamaClient) EmbedForHook(ctx context.Context, model, text string) ([]float32, error) {
	return c.embed(ctx, EmbedRequest{Model: model, Input: text, KeepAlive: HookKeepAlive})
}

// embed is the shared HTTP plumbing. Kept private so Embed and EmbedForHook
// are the only public shapes and their intent is obvious at each call site.
func (c *OllamaClient) embed(ctx context.Context, payload EmbedRequest) ([]float32, error) {
	// Apply a default per-call deadline if the caller hasn't already set one,
	// matching the historical 30s cap on the shared http.Client.
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}

	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", c.endpoint+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama embed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama embed: status %d: %s", resp.StatusCode, string(b))
	}

	var result EmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("ollama embed decode: %w", err)
	}
	if len(result.Embeddings) == 0 || len(result.Embeddings[0]) == 0 {
		return nil, fmt.Errorf("ollama returned empty embeddings")
	}
	return result.Embeddings[0], nil
}

// EmbedBatch generates embeddings for multiple texts in one API call.
// Rejects batches larger than EmbedBatchSize so that callers cannot bypass
// the split path and force Ollama to load an unbounded request into memory.
func (c *OllamaClient) EmbedBatch(ctx context.Context, model string, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if len(texts) > EmbedBatchSize {
		return nil, fmt.Errorf("ollama embed batch: size %d exceeds max %d", len(texts), EmbedBatchSize)
	}
	// Single text: reuse the single-embed path for simplicity.
	if len(texts) == 1 {
		vec, err := c.Embed(ctx, model, texts[0])
		if err != nil {
			return nil, err
		}
		return [][]float32{vec}, nil
	}

	body, _ := json.Marshal(EmbedBatchRequest{Model: model, Input: texts})

	// Scale the deadline with batch size so larger batches get proportionally
	// more time, while still reusing the shared http.Client (and its keep-alive
	// connection pool) rather than allocating a new one per call.
	batchTimeout := 30*time.Second + time.Duration(len(texts))*2*time.Second
	reqCtx, cancel := context.WithTimeout(ctx, batchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, "POST", c.endpoint+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama embed batch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama embed batch: status %d: %s", resp.StatusCode, string(b))
	}

	var result EmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("ollama embed batch decode: %w", err)
	}
	if len(result.Embeddings) != len(texts) {
		return nil, fmt.Errorf("ollama embed batch: expected %d embeddings, got %d", len(texts), len(result.Embeddings))
	}
	return result.Embeddings, nil
}

// ModelInfo holds basic model metadata from Ollama.
type ModelInfo struct {
	Name string `json:"name"`
}

// tagsResponse is the JSON envelope for GET /api/tags.
type tagsResponse struct {
	Models []ModelInfo `json:"models"`
}

// Ping checks if Ollama is reachable by hitting /api/tags with a short timeout.
func (c *OllamaClient) Ping(ctx context.Context) error {
	pingClient := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", c.endpoint+"/api/tags", nil)
	if err != nil {
		return err
	}
	resp, err := pingClient.Do(req)
	if err != nil {
		return fmt.Errorf("ollama ping: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama ping: status %d", resp.StatusCode)
	}
	return nil
}

// ListModels returns the locally available models from Ollama.
func (c *OllamaClient) ListModels(ctx context.Context) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.endpoint+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama list models: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama list models: status %d: %s", resp.StatusCode, string(b))
	}

	var result tagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("ollama list models decode: %w", err)
	}
	return result.Models, nil
}

// VerifyModel checks that the given model is listed in /api/tags and can
// produce an embedding. Returns nil on success or a descriptive error.
func (c *OllamaClient) VerifyModel(ctx context.Context, model string) error {
	// Step 1: Verify the model appears in the local model list.
	models, err := c.ListModels(ctx)
	if err != nil {
		return fmt.Errorf("verify model: failed to list models: %w", err)
	}
	found := false
	for _, m := range models {
		if m.Name == model || strings.HasPrefix(m.Name, model+":") {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("verify model: %s not found in local models", model)
	}

	// Step 2: Do a test embed to confirm the model is functional.
	vec, err := c.Embed(ctx, model, "test")
	if err != nil {
		return fmt.Errorf("verify model: test embed failed: %w", err)
	}
	if len(vec) == 0 {
		return fmt.Errorf("verify model: test embed returned empty vector")
	}

	return nil
}

// -----------------------------------------------------------------------
// /api/chat support — used by the optional LLM classifier fallback
// (internal/heimdall/llm_classifier.go). Mirrors the embed() plumbing:
// context-based deadline, HTTP status handling, wrapped errors.
// -----------------------------------------------------------------------

// ChatMessage is one entry in the /api/chat `messages` array.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatOptions maps to the Ollama /api/chat `options` object. Only the
// fields we actually set are modeled — leave others nil so the server
// applies its defaults. Pointer semantics so `omitempty` distinguishes
// "unset" from "explicit zero value" (seed=0 is valid, as is
// num_predict=0 meaning "no output").
type ChatOptions struct {
	Temperature *float64 `json:"temperature,omitempty"`
	Seed        *int     `json:"seed,omitempty"`
	NumPredict  *int     `json:"num_predict,omitempty"`
}

// ChatRequest is the POST body for /api/chat. Format accepts either the
// literal string "json" (Ollama's original loose mode) or a raw JSON
// Schema object (Ollama ≥0.5 structured output). The LLM classifier uses
// the latter; we expose both shapes via json.RawMessage so callers pass
// pre-marshalled JSON without us re-validating it here.
type ChatRequest struct {
	Model     string          `json:"model"`
	Messages  []ChatMessage   `json:"messages"`
	KeepAlive string          `json:"keep_alive,omitempty"`
	Format    json.RawMessage `json:"format,omitempty"`
	Options   *ChatOptions    `json:"options,omitempty"`
	Stream    bool            `json:"stream"`
}

// ChatResponse is the (non-streaming) response envelope from /api/chat.
type ChatResponse struct {
	Model   string      `json:"model"`
	Message ChatMessage `json:"message"`
	Done    bool        `json:"done"`
}

// Chat sends a chat-completion request to /api/chat and returns the full
// response. Mirrors embed() error handling: wrapped HTTP errors, status
// checks, decoded body. The caller's ctx governs the deadline — we do
// NOT apply a default timeout because the LLM classifier path wants an
// explicit context.WithTimeout set at the hook-handler layer.
//
// Safe for concurrent use; the shared http.Client handles connection
// pooling the same way Embed/EmbedBatch rely on it.
func (c *OllamaClient) Chat(ctx context.Context, payload ChatRequest) (*ChatResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("ollama chat marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.endpoint+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama chat: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama chat: status %d: %s", resp.StatusCode, string(b))
	}

	var result ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("ollama chat decode: %w", err)
	}
	return &result, nil
}

// PullModel pulls a model from the Ollama library. This is a blocking call
// that waits for the pull to complete.
func (c *OllamaClient) PullModel(ctx context.Context, model string) error {
	body, _ := json.Marshal(map[string]interface{}{
		"name":   model,
		"stream": false,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", c.endpoint+"/api/pull", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	// Use a client with a long timeout for model pulls
	pullClient := &http.Client{Timeout: 30 * time.Minute}
	resp, err := pullClient.Do(req)
	if err != nil {
		return fmt.Errorf("ollama pull: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ollama pull: status %d: %s", resp.StatusCode, string(b))
	}

	// Drain the response body (Ollama may send progress JSON lines even with stream=false)
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
