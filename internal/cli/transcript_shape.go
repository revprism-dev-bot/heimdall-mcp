package cli

import "strings"

// extractRoleAndContent normalizes a raw JSONL entry from a Claude Code
// transcript into (role, content, ok), handling both shapes:
//
//  1. Legacy / synthetic shape: top-level "role" and "content" fields.
//     {"role":"user","content":"..."}
//
//  2. Real Claude Code shape: top-level "type" discriminator ("user",
//     "assistant", "system") with role+content nested under "message".
//     {"type":"user","message":{"role":"user","content":"..."}, ...}
//
// The caller receives the normalised role string and the raw content value
// (string or []any block list) regardless of which shape the entry uses.
// ok is false when the entry should be skipped (e.g. non-message types such
// as "permission-mode" or "attachment", or when content is missing entirely).
func extractRoleAndContent(entry map[string]any) (role string, content any, ok bool) {
	// -- Shape 1: top-level role --
	if r, _ := entry["role"].(string); r != "" {
		c := entry["content"]
		return r, c, true
	}

	// -- Shape 2: type discriminator + nested message --
	topType, _ := entry["type"].(string)
	switch topType {
	case "user", "assistant", "system":
	default:
		// Not a message entry (permission-mode, attachment, etc.) — skip.
		return "", nil, false
	}

	// Skip meta entries. Claude Code sets isMeta=true (and often a
	// sourceToolUseID) on system-injected content that arrives in the
	// user-role channel — skill loader payloads, tool-invocation echoes,
	// and internal prompts. These aren't real human input and must not
	// trigger marker-based rules (correction, teaching, workaround,
	// frustration) — the skill bodies routinely contain words like
	// "stop", "instead", "turns out" in their documentation.
	if isMeta, _ := entry["isMeta"].(bool); isMeta {
		return "", nil, false
	}

	msg, _ := entry["message"].(map[string]any)
	if msg == nil {
		return "", nil, false
	}
	r, _ := msg["role"].(string)
	if r == "" {
		// Fall back to the top-level type as role for system messages that
		// sometimes omit the inner role field.
		r = topType
	}
	c := msg["content"]
	return r, c, true
}

// normalizeToolName strips the "mcp__<server>__" prefix that Claude Code
// prepends to MCP tool names in real transcripts, returning the bare tool
// name used internally.
//
//	"mcp__heimdall__heimdall_remember" → "heimdall_remember"
//	"mcp__other__tool"                → "tool"
//	"heimdall_remember"               → "heimdall_remember"   (no-op)
//	"Read"                            → "Read"                (no-op)
func normalizeToolName(name string) string {
	if !strings.HasPrefix(name, "mcp__") {
		return name
	}
	// Strip the first two "__"-separated segments ("mcp" and the server name).
	// e.g. "mcp__heimdall__heimdall_remember" → ["mcp", "heimdall", "heimdall_remember"]
	rest := name[len("mcp__"):]
	idx := strings.Index(rest, "__")
	if idx < 0 {
		// Malformed prefix with no second "__" — return as-is.
		return name
	}
	return rest[idx+2:]
}
