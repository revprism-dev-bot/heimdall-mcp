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
func NewOllamaClient(endpoint string) *OllamaClient {
	return &OllamaClient{
		endpoint: endpoint,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// EmbedRequest is the POST body for /api/embed.
type EmbedRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

// EmbedResponse is the response from /api/embed.
type EmbedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

// Embed generates an embedding vector for the given text.
func (c *OllamaClient) Embed(ctx context.Context, model, text string) ([]float32, error) {
	body, _ := json.Marshal(EmbedRequest{Model: model, Input: text})
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
