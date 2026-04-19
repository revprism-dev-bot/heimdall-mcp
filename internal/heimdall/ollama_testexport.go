package heimdall

import "time"

// Test-only accessors for ollama unexported fields. Intra-package
// access is already available, but cross-package tests (e.g.
// internal/mcp server tests that verify configuration wiring) need a
// narrow, explicit surface. Named Test* so dead-code tools flag them if
// ever unreferenced.
//
// These are preferred over exposing the fields directly because the
// caller intent is obvious at the call site ("I'm reading a field for
// test assertion") and the unexported field can rename freely.

// TestEmbedTimeout returns the client's configured per-request embed
// timeout (after FromConfig sentinel resolution).
func (c *OllamaClient) TestEmbedTimeout() time.Duration { return c.embedTimeout }

// TestMaxRetries returns the client's configured maxRetries (post
// FromConfig sentinel resolution).
func (c *OllamaClient) TestMaxRetries() int { return c.maxRetries }
