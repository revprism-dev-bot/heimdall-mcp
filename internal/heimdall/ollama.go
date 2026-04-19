package heimdall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// DefaultEmbedMaxConcurrent caps concurrent /api/embed requests issued by a
// single OllamaClient. Two was picked empirically — burst parallelism of 4+
// against a single-GPU Ollama instance was observed to deadline-exceed during
// indexing (see docs/handoffs/2026-04-19-legacy-db-paths-and-filters.md
// Problem #5). Raise via NewOllamaClientWithLimit / NewOllamaClientWithOptions
// or config `embedMaxConcurrent`.
const DefaultEmbedMaxConcurrent = 2

// DefaultEmbedTimeout is the per-request deadline applied when the caller's
// context carries no deadline. Matches the historical inline default.
const DefaultEmbedTimeout = 30 * time.Second

// DefaultEmbedMaxRetries is the number of retries applied to an embed call
// when the failure is `context.DeadlineExceeded` against the PER-REQUEST
// deadline (not the caller's ctx). Set to 0 to disable retries.
const DefaultEmbedMaxRetries = 2

// defaultEmbedRetryBackoff is the sequence of sleeps between retries.
// Callers can override via OllamaOptions.RetryBackoff. If the configured
// MaxRetries exceeds len(backoff), the last entry is reused for the
// remaining attempts.
var defaultEmbedRetryBackoff = []time.Duration{
	250 * time.Millisecond,
	500 * time.Millisecond,
}

// OllamaOptions groups the tunables for constructing an OllamaClient.
// Zero values mean "use the package default".
type OllamaOptions struct {
	// MaxConcurrent bounds in-flight /api/embed requests. 0 = unbounded
	// (caller manages concurrency upstream). Negative coerces to
	// DefaultEmbedMaxConcurrent with a one-line warn log.
	MaxConcurrent int
	// EmbedTimeout is applied to embed requests whose caller ctx has no
	// deadline. 0 → DefaultEmbedTimeout.
	EmbedTimeout time.Duration
	// MaxRetries controls retries on PER-REQUEST context.DeadlineExceeded
	// (only). The caller's own ctx cancellation is NEVER retried. Default
	// DefaultEmbedMaxRetries.
	MaxRetries int
	// RetryBackoff is consumed in order. If shorter than MaxRetries the
	// last entry is repeated. nil → defaultEmbedRetryBackoff.
	RetryBackoff []time.Duration
}

// OllamaClient talks to the Ollama REST API.
//
// Concurrency: EmbedForHook / Embed / EmbedBatch share a counting
// semaphore (cap = MaxConcurrent) so burst parallelism from the indexer
// does not deadline-exceed a single-GPU Ollama instance. The semaphore
// acquire respects the caller's context. Chat/Ping/ListModels/PullModel
// are NOT gated — they carry their own distinct deadlines and are not
// the hot bursty path.
type OllamaClient struct {
	endpoint   string
	httpClient *http.Client
	// sem is a counting semaphore: send struct{}{} to acquire, receive
	// to release. A nil sem means "unbounded" (MaxConcurrent == 0).
	sem          chan struct{}
	embedTimeout time.Duration
	maxRetries   int
	retryBackoff []time.Duration
}

// NewOllamaClient creates a client for the given endpoint (e.g. "http://localhost:11434").
// The shared http.Client has no hard timeout — per-call deadlines are expected
// to come from the caller's context, which lets batch embeds use a longer
// deadline than single-text embeds while still sharing one connection pool.
//
// This constructor preserves backward compatibility: callers get the safe
// concurrency default (DefaultEmbedMaxConcurrent). Use
// NewOllamaClientWithLimit or NewOllamaClientWithOptions to override.
func NewOllamaClient(endpoint string) *OllamaClient {
	return NewOllamaClientWithLimit(endpoint, DefaultEmbedMaxConcurrent)
}

// NewOllamaClientWithLimit constructs an OllamaClient with an explicit
// concurrency cap. `maxConcurrent == 0` disables the semaphore (unbounded);
// a negative value coerces to DefaultEmbedMaxConcurrent with a WARN log
// (constructor signature cannot return an error without breaking every
// existing caller).
func NewOllamaClientWithLimit(endpoint string, maxConcurrent int) *OllamaClient {
	return NewOllamaClientWithOptions(endpoint, OllamaOptions{MaxConcurrent: maxConcurrent})
}

// NewOllamaClientFromConfig is a convenience constructor that maps the
// `EmbedMaxConcurrent` / `EmbedTimeoutMs` / `EmbedMaxRetries` fields from
// the top-level Config (see internal/config) onto OllamaOptions. Zero
// values fall through to package defaults — callers do not need to know
// the defaults. Kept in the heimdall package so the heimdall ↔ config
// cycle stays one-way.
func NewOllamaClientFromConfig(endpoint string, maxConcurrent, timeoutMs, maxRetries int) *OllamaClient {
	opts := OllamaOptions{
		MaxConcurrent: maxConcurrent, // 0 → unbounded (explicit opt-out),
		MaxRetries:    DefaultEmbedMaxRetries,
	}
	// Zero in the config means "unset → default". Only non-zero overrides.
	if maxConcurrent == 0 {
		opts.MaxConcurrent = DefaultEmbedMaxConcurrent
	}
	if timeoutMs > 0 {
		opts.EmbedTimeout = time.Duration(timeoutMs) * time.Millisecond
	}
	if maxRetries > 0 {
		opts.MaxRetries = maxRetries
	}
	// Allow explicit "0 retries" only via negative sentinel — zero from
	// JSON defaults is ambiguous. Callers who want zero retries set the
	// config field explicitly to -1 (treated as 0 after clamping below).
	if maxRetries < 0 {
		opts.MaxRetries = 0
	}
	return NewOllamaClientWithOptions(endpoint, opts)
}

// NewOllamaClientWithOptions is the full-fidelity constructor. Options fields
// left at the zero value fall back to package defaults.
func NewOllamaClientWithOptions(endpoint string, opts OllamaOptions) *OllamaClient {
	maxConcurrent := opts.MaxConcurrent
	if maxConcurrent < 0 {
		log.Printf("heimdall: NewOllamaClientWithOptions: invalid MaxConcurrent=%d; coercing to default %d", maxConcurrent, DefaultEmbedMaxConcurrent)
		maxConcurrent = DefaultEmbedMaxConcurrent
	}

	timeout := opts.EmbedTimeout
	if timeout <= 0 {
		timeout = DefaultEmbedTimeout
	}

	retries := opts.MaxRetries
	if retries < 0 {
		retries = 0
	}

	backoff := opts.RetryBackoff
	if len(backoff) == 0 {
		backoff = defaultEmbedRetryBackoff
	}

	var sem chan struct{}
	if maxConcurrent > 0 {
		sem = make(chan struct{}, maxConcurrent)
	}

	return &OllamaClient{
		endpoint:     endpoint,
		httpClient:   &http.Client{},
		sem:          sem,
		embedTimeout: timeout,
		maxRetries:   retries,
		retryBackoff: backoff,
	}
}

// acquireEmbedSlot blocks until the semaphore admits this call, or ctx is
// cancelled. Returns a release func (always non-nil; safe to call when
// acquire returned an error — it's a no-op in that case).
func (c *OllamaClient) acquireEmbedSlot(ctx context.Context) (release func(), err error) {
	if c.sem == nil {
		return func() {}, nil
	}
	select {
	case c.sem <- struct{}{}:
		return func() { <-c.sem }, nil
	case <-ctx.Done():
		return func() {}, ctx.Err()
	}
}

// backoffFor returns the sleep duration to apply AFTER attempt `attemptIdx`
// (0-indexed). When the configured backoff slice is shorter than needed the
// last entry is reused for remaining retries.
func (c *OllamaClient) backoffFor(attemptIdx int) time.Duration {
	if len(c.retryBackoff) == 0 {
		return 0
	}
	if attemptIdx >= len(c.retryBackoff) {
		return c.retryBackoff[len(c.retryBackoff)-1]
	}
	return c.retryBackoff[attemptIdx]
}

// isPerRequestDeadlineExceeded returns true when `err` is
// context.DeadlineExceeded from our PER-REQUEST timeout. The caller's ctx is
// passed separately so we can distinguish "caller cancelled us" (do not
// retry) from "our internal timeout fired" (retry is safe).
func isPerRequestDeadlineExceeded(err error, callerCtx context.Context) bool {
	if err == nil {
		return false
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// If the caller's context has been cancelled or timed out, attribute
	// the deadline to them — do NOT retry.
	if callerCtx.Err() != nil {
		return false
	}
	return true
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
//
// Concurrency + retry contract:
//   - Acquires a semaphore slot (respects caller ctx).
//   - Each attempt gets its own context.WithTimeout(callerCtx, embedTimeout)
//     UNLESS the caller already set a deadline — then we honor theirs.
//   - On per-request context.DeadlineExceeded, retries up to maxRetries
//     times with configurable backoff. Caller ctx cancellation is NEVER
//     retried (the caller has told us to stop).
//   - Non-timeout errors (HTTP 4xx/5xx, decode errors, transport errors
//     other than deadline-exceeded) are returned immediately.
func (c *OllamaClient) embed(ctx context.Context, payload EmbedRequest) ([]float32, error) {
	release, err := c.acquireEmbedSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	body, _ := json.Marshal(payload)

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		// Honor caller cancellation between attempts.
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, err
		}

		vec, err := c.doEmbedOnce(ctx, body)
		if err == nil {
			return vec, nil
		}
		lastErr = err
		if !isPerRequestDeadlineExceeded(err, ctx) {
			return nil, err
		}
		if attempt == c.maxRetries {
			break
		}
		// Sleep, but don't exceed caller ctx.
		sleep := c.backoffFor(attempt)
		if sleep > 0 {
			select {
			case <-time.After(sleep):
			case <-ctx.Done():
				return nil, lastErr
			}
		}
	}
	return nil, lastErr
}

// doEmbedOnce executes a single /api/embed round-trip. Applies the default
// per-request deadline when the caller ctx has none. Returns the first
// embedding vector or an error wrapping the HTTP / decode failure.
func (c *OllamaClient) doEmbedOnce(ctx context.Context, body []byte) ([]float32, error) {
	reqCtx, cancel := c.perRequestCtx(ctx)
	if cancel != nil {
		defer cancel()
	}

	req, err := http.NewRequestWithContext(reqCtx, "POST", c.endpoint+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Preserve errors.Is(err, context.DeadlineExceeded) — the retry
		// gate relies on it. http.Client already wraps with url.Error
		// whose Unwrap chain exposes the ctx error.
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
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

// applyBatchDeadline mirrors perRequestCtx but with a caller-supplied budget
// (the batch-scaled timeout). Returns a nil cancel when no new context is
// needed (caller-deadline shorter, or budget non-positive).
func (c *OllamaClient) applyBatchDeadline(ctx context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	if budget <= 0 {
		return ctx, nil
	}
	callerDeadline, hasCallerDeadline := ctx.Deadline()
	if hasCallerDeadline {
		if time.Until(callerDeadline) < budget {
			return ctx, nil
		}
	}
	return context.WithTimeout(ctx, budget)
}

// perRequestCtx returns a child context with a per-request deadline bounded
// by min(caller_deadline, c.embedTimeout). This is the knob the retry loop
// relies on: per-request context.DeadlineExceeded must fire BEFORE the
// caller's deadline so we can distinguish "our budget blew" (retry) from
// "caller cancelled us" (abort).
//
// If neither caller deadline nor c.embedTimeout apply, returns ctx as-is.
// Always returns a non-nil cancel func (defensive — callers `defer cancel`
// unconditionally) unless ctx is passed through unchanged.
func (c *OllamaClient) perRequestCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	// If embedTimeout is zero/negative, fall back to caller ctx unchanged.
	if c.embedTimeout <= 0 {
		return ctx, nil
	}
	callerDeadline, hasCallerDeadline := ctx.Deadline()
	budget := c.embedTimeout
	if hasCallerDeadline {
		remaining := time.Until(callerDeadline)
		if remaining < budget {
			// Caller has less headroom — honor theirs directly (no new
			// child needed; the caller ctx already fires in time).
			return ctx, nil
		}
	}
	return context.WithTimeout(ctx, budget)
}

// EmbedBatch generates embeddings for multiple texts in one API call.
// Rejects batches larger than EmbedBatchSize so that callers cannot bypass
// the split path and force Ollama to load an unbounded request into memory.
//
// Concurrency: shares the same semaphore as Embed / EmbedForHook so a mix
// of single and batch callers cannot collectively exceed MaxConcurrent.
// Retries follow the same rules as embed() — per-request DeadlineExceeded
// only, never on caller ctx cancel.
func (c *OllamaClient) EmbedBatch(ctx context.Context, model string, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if len(texts) > EmbedBatchSize {
		return nil, fmt.Errorf("ollama embed batch: size %d exceeds max %d", len(texts), EmbedBatchSize)
	}
	// Single text: reuse the single-embed path for simplicity. The
	// semaphore acquire inside Embed handles concurrency bounding.
	if len(texts) == 1 {
		vec, err := c.Embed(ctx, model, texts[0])
		if err != nil {
			return nil, err
		}
		return [][]float32{vec}, nil
	}

	release, err := c.acquireEmbedSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	body, _ := json.Marshal(EmbedBatchRequest{Model: model, Input: texts})

	// Scale the deadline with batch size so larger batches get proportionally
	// more time. Baseline is the configured per-request timeout (no longer
	// a hardcoded 30s — honors OllamaOptions.EmbedTimeout).
	batchTimeout := c.embedTimeout + time.Duration(len(texts))*2*time.Second

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, err
		}
		vecs, err := c.doEmbedBatchOnce(ctx, body, batchTimeout, len(texts))
		if err == nil {
			return vecs, nil
		}
		lastErr = err
		if !isPerRequestDeadlineExceeded(err, ctx) {
			return nil, err
		}
		if attempt == c.maxRetries {
			break
		}
		sleep := c.backoffFor(attempt)
		if sleep > 0 {
			select {
			case <-time.After(sleep):
			case <-ctx.Done():
				return nil, lastErr
			}
		}
	}
	return nil, lastErr
}

// doEmbedBatchOnce executes a single batch /api/embed round-trip. Applies
// a batch-scaled per-request deadline bounded by min(caller_deadline,
// batchTimeout) so the retry loop can distinguish our timeout from the
// caller's cancellation.
func (c *OllamaClient) doEmbedBatchOnce(ctx context.Context, body []byte, batchTimeout time.Duration, wantCount int) ([][]float32, error) {
	reqCtx, cancel := c.applyBatchDeadline(ctx, batchTimeout)
	if cancel != nil {
		defer cancel()
	}

	req, err := http.NewRequestWithContext(reqCtx, "POST", c.endpoint+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
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
	if len(result.Embeddings) != wantCount {
		return nil, fmt.Errorf("ollama embed batch: expected %d embeddings, got %d", wantCount, len(result.Embeddings))
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
