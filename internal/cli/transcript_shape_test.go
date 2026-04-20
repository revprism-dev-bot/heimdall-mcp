package cli

import (
	"testing"
)

// TestNormalizeToolName_StripsMCPPrefix verifies that normalizeToolName
// correctly strips the "mcp__<server>__" prefix and leaves bare names untouched.
func TestNormalizeToolName_StripsMCPPrefix(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"mcp__heimdall__heimdall_remember", "heimdall_remember"},
		{"mcp__heimdall__heimdall_search", "heimdall_search"},
		{"mcp__heimdall__heimdall_recall", "heimdall_recall"},
		{"mcp__heimdall__heimdall_index_text", "heimdall_index_text"},
		{"mcp__other__tool", "tool"},
		{"Read", "Read"},
		{"heimdall_remember", "heimdall_remember"},
		{"WebFetch", "WebFetch"},
		{"mcp__", "mcp__"},       // malformed — no second "__", returned as-is
		{"mcp__a__b__c", "b__c"}, // extra __ preserved after stripping prefix
	}
	for _, tc := range cases {
		got := normalizeToolName(tc.input)
		if got != tc.want {
			t.Errorf("normalizeToolName(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// TestExtractRoleAndContent_BothShapes verifies that extractRoleAndContent
// correctly handles both the legacy top-level shape and the real Claude Code
// nested-message shape.
func TestExtractRoleAndContent_BothShapes(t *testing.T) {
	cases := []struct {
		name        string
		entry       map[string]any
		wantRole    string
		wantContent any
		wantOK      bool
	}{
		{
			name: "legacy_string_content",
			entry: map[string]any{
				"role":    "user",
				"content": "hello",
			},
			wantRole:    "user",
			wantContent: "hello",
			wantOK:      true,
		},
		{
			name: "legacy_block_content",
			entry: map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "text", "text": "hi"},
				},
			},
			wantRole: "assistant",
			wantOK:   true,
		},
		{
			name: "real_shape_user_string",
			entry: map[string]any{
				"type": "user",
				"uuid": "u1",
				"message": map[string]any{
					"role":    "user",
					"content": "actually no",
				},
			},
			wantRole:    "user",
			wantContent: "actually no",
			wantOK:      true,
		},
		{
			name: "real_shape_assistant_blocks",
			entry: map[string]any{
				"type": "assistant",
				"uuid": "a1",
				"message": map[string]any{
					"role": "assistant",
					"content": []any{
						map[string]any{"type": "text", "text": "ok"},
					},
				},
			},
			wantRole: "assistant",
			wantOK:   true,
		},
		{
			name: "permission_mode_skipped",
			entry: map[string]any{
				"type":           "permission-mode",
				"permissionMode": "default",
			},
			wantOK: false,
		},
		{
			name: "attachment_skipped",
			entry: map[string]any{
				"type": "attachment",
				"data": "...",
			},
			wantOK: false,
		},
		{
			name: "real_shape_system",
			entry: map[string]any{
				"type": "system",
				"message": map[string]any{
					"role":    "system",
					"content": "system prompt",
				},
			},
			wantRole:    "system",
			wantContent: "system prompt",
			wantOK:      true,
		},
		{
			// isMeta=true entries are Claude Code's skill-loader / tool-invocation
			// echoes that arrive in the user-role channel. They must be skipped —
			// skill bodies routinely contain marker words that would fire the
			// correction/teaching/workaround rules as false positives.
			name: "meta_user_skipped",
			entry: map[string]any{
				"type":   "user",
				"uuid":   "u2",
				"isMeta": true,
				"message": map[string]any{
					"role":    "user",
					"content": "Base directory for this skill: ... stop instead turns out",
				},
			},
			wantOK: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			role, content, ok := extractRoleAndContent(tc.entry)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if role != tc.wantRole {
				t.Errorf("role = %q, want %q", role, tc.wantRole)
			}
			// Content type check: if wantContent is a string, compare directly.
			if tc.wantContent != nil {
				if s, ok2 := tc.wantContent.(string); ok2 {
					if got, ok3 := content.(string); !ok3 || got != s {
						t.Errorf("content = %v (%T), want %q", content, content, s)
					}
				}
			}
		})
	}
}
