// Package cli — Ollama client construction.
//
// newOllamaClient centralizes config-aware OllamaClient construction for
// every CLI entry point (indexer, hooks, doctor, skills, sessions, etc.).
// The legacy heimdall.NewOllamaClient(endpoint) constructor drops all of
// the user's embed tuning (EmbedMaxConcurrent / EmbedTimeoutMs /
// EmbedMaxRetries) so the CLI — the primary burst-parallelism surface —
// silently ignored HEIMDALL_EMBED_* env vars and config.json values.
//
// This helper resolves env-var overrides onto a TRANSIENT copy of the
// config (see config.ResolveEmbedConfig) so env tuning never
// accidentally round-trips to disk via a later SaveConfig.
//
// Closes PR74 review F1 (CLI path bypassing config) and F2 (env
// persistence hazard).
package cli

import (
	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// newOllamaClient constructs an OllamaClient from the given cfg. Prefers
// this helper over heimdall.NewOllamaClient at every CLI site so the
// user's HEIMDALL_EMBED_* env vars and embed-tuning config fields take
// effect globally. The caller's cfg is NOT mutated.
//
// Contract notes:
//   - Endpoint comes from the resolved config (normally == cfg.OllamaEndpoint;
//     ResolveEmbedConfig does not override the endpoint, this is just
//     shape parity with the MCP server path).
//   - The sentinel conventions for EmbedMaxConcurrent / EmbedMaxRetries
//     (see config.Config docs) flow through to the client.
func newOllamaClient(cfg config.Config) *heimdall.OllamaClient {
	effective := config.ResolveEmbedConfig(cfg)
	return heimdall.NewOllamaClientFromConfig(
		effective.OllamaEndpoint,
		effective.EmbedMaxConcurrent,
		effective.EmbedTimeoutMs,
		effective.EmbedMaxRetries,
	)
}

// newOllamaClientForEndpoint is the "the endpoint doesn't live on a cfg
// struct" variant used by deps-injected hook test harnesses that hand
// the endpoint in as a bare string. Applies env-var resolution against a
// synthesized minimal Config so HEIMDALL_EMBED_* still take effect even
// on the hook fast paths.
func newOllamaClientForEndpoint(endpoint string) *heimdall.OllamaClient {
	return newOllamaClient(config.Config{OllamaEndpoint: endpoint})
}
