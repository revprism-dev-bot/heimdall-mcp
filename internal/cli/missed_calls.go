package cli

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

// MissedCall is one detected rule violation: a trigger fired in the
// transcript but the expected heimdall tool call did not happen within
// the rule's allowed window.
type MissedCall struct {
	Rule           string // "read_large_file" | "user_correction" | "external_content" | "task_start_recall"
	ExpectedTool   string // heimdall_search / heimdall_remember / heimdall_index_text / heimdall_recall
	TriggerExcerpt string // <= 200 chars describing what triggered the rule
	TurnIndex      int    // zero-based index in the transcript (parsedMsg.Index)
}

// AnalysisResult summarizes one transcript's compliance with the
// trigger->action rules. Triggers is the total number of rule firings;
// Followed is how many had the expected tool call within window.
// Misses = Triggers - Followed. The per-rule Misses list carries detail
// for display in the last-session-review block.
type AnalysisResult struct {
	Triggers int
	Followed int
	Misses   []MissedCall
}

// AnalyzeTranscript runs all four rules over a raw JSONL transcript and
// returns the AnalysisResult. Transcript shape: one JSON object per line;
// each object has "role" and "content" (either a string or a list of
// content blocks). Tool calls appear as content blocks of type "tool_use"
// with a "name" field; tool results as blocks of type "tool_result".
func AnalyzeTranscript(data []byte) AnalysisResult {
	msgs := parseTranscriptMessages(data)
	var out AnalysisResult
	for _, r := range rules {
		triggers, followed, misses := r.evaluate(msgs)
		out.Triggers += triggers
		out.Followed += followed
		out.Misses = append(out.Misses, misses...)
	}
	return out
}

// rule is an internal representation of one trigger->action pair. Each rule
// decides for itself what counts as a trigger and which tool name satisfies
// it within which window.
type rule struct {
	name         string
	expectedTool string
	evaluate     func([]parsedMsg) (triggers, followed int, misses []MissedCall)
}

// parsedMsg is the common representation used across the four rule
// evaluators. Content is already flattened to text for marker scanning;
// ContentBlocks is retained for tool_use / tool_result inspection.
type parsedMsg struct {
	Role          string
	Content       string
	ContentBlocks []map[string]any
	Index         int
}

// rules is the registry of four trigger->action pairs, mirroring the MCP
// instruction block. If you add a pair to the block, add a rule here.
var rules = []rule{
	{
		name:         "read_large_file",
		expectedTool: "heimdall_search",
		evaluate: func(msgs []parsedMsg) (int, int, []MissedCall) {
			return evaluateReadLargeFile(msgs)
		},
	},
	{
		name:         "user_correction",
		expectedTool: "heimdall_remember",
		evaluate: func(msgs []parsedMsg) (int, int, []MissedCall) {
			return evaluateUserCorrection(msgs)
		},
	},
	{
		name:         "external_content",
		expectedTool: "heimdall_index_text",
		evaluate: func(msgs []parsedMsg) (int, int, []MissedCall) {
			return evaluateExternalContent(msgs)
		},
	},
	{
		name:         "task_start_recall",
		expectedTool: "heimdall_recall",
		evaluate: func(msgs []parsedMsg) (int, int, []MissedCall) {
			return evaluateTaskStartRecall(msgs)
		},
	},
}

// correctionPatternAnalyzer matches user messages that are corrections.
// MUST stay aligned with hook_stop.go:markerPatterns["correction"].
var correctionPatternAnalyzer = regexp.MustCompile(`(?i)\bactually\b|\bno,?\s|\bdon['']?t\b|\bstop\b|\binstead\b|\bwrong\b`)

// taskStartPattern matches user messages that reference past decisions.
// Keep wording aligned with the MCP instruction bullet:
// "task that references past decisions, plans, or prior architecture".
var taskStartPattern = regexp.MustCompile(`(?i)\bpast decisions?\b|\bplan(s|ning)?\b|\barchitecture\b|\bprior(\s+issues?)?\b`)

// evaluateReadLargeFile: when an assistant turn contains a tool_use block
// for "Read", and the immediately following tool_result has more than 200
// newlines, that is a trigger. The expected follow-up is a heimdall_search
// tool_use in the SAME assistant turn OR the turn IMMEDIATELY AFTER the
// tool_result.
func evaluateReadLargeFile(msgs []parsedMsg) (int, int, []MissedCall) {
	var triggers, followed int
	var misses []MissedCall

	for i, msg := range msgs {
		if msg.Role != "assistant" {
			continue
		}
		// Find a tool_use block named "Read" in this assistant turn.
		readID := ""
		for _, block := range msg.ContentBlocks {
			if btype, _ := block["type"].(string); btype == "tool_use" {
				if name, _ := block["name"].(string); name == "Read" {
					readID, _ = block["id"].(string)
					break
				}
			}
		}
		if readID == "" {
			continue
		}

		// Check if this same assistant turn also has heimdall_search.
		if assistantHasTool(msg, "heimdall_search") {
			// heimdall_search already in the same turn — compliant regardless of result size.
			// We don't count this as a trigger since the file wasn't read yet (result not seen).
			// Continue: the result check happens below.
		}

		// Look for the tool_result in the next message (role user with tool_result blocks).
		if i+1 >= len(msgs) {
			continue
		}
		resultMsg := msgs[i+1]
		// Find the matching tool_result block for readID.
		resultContent := ""
		for _, block := range resultMsg.ContentBlocks {
			if btype, _ := block["type"].(string); btype == "tool_result" {
				toolUseID, _ := block["tool_use_id"].(string)
				if toolUseID != readID && readID != "" {
					continue
				}
				// Extract content text from the tool_result content field.
				resultContent = extractToolResultContent(block)
				break
			}
		}
		if resultContent == "" {
			continue
		}

		// Count newlines — trigger only if > 200.
		newlineCount := strings.Count(resultContent, "\n")
		if newlineCount <= 200 {
			continue
		}

		// Trigger fires.
		triggers++
		excerpt := "Read result: " + firstN(resultContent, 187)

		// Check for heimdall_search in the same assistant turn (pre-read)
		// OR the assistant turn IMMEDIATELY AFTER the tool_result.
		sameAssistantHasSearch := assistantHasTool(msg, "heimdall_search")
		nextAssistantHasSearch := false
		if i+2 < len(msgs) {
			nextMsg := msgs[i+2]
			if nextMsg.Role == "assistant" && assistantHasTool(nextMsg, "heimdall_search") {
				nextAssistantHasSearch = true
			}
		}

		if sameAssistantHasSearch || nextAssistantHasSearch {
			followed++
		} else {
			misses = append(misses, MissedCall{
				Rule:           "read_large_file",
				ExpectedTool:   "heimdall_search",
				TriggerExcerpt: excerpt,
				TurnIndex:      msg.Index,
			})
		}
	}

	return triggers, followed, misses
}

// evaluateUserCorrection: a user message matching the correction regex
// triggers the rule. Expected: heimdall_remember in the next assistant turn.
func evaluateUserCorrection(msgs []parsedMsg) (int, int, []MissedCall) {
	var triggers, followed int
	var misses []MissedCall

	for i, msg := range msgs {
		if msg.Role != "user" {
			continue
		}
		// Skip tool_result messages (they have no text Content).
		if msg.Content == "" {
			continue
		}
		if !correctionPatternAnalyzer.MatchString(msg.Content) {
			continue
		}

		// Trigger fires.
		triggers++
		excerpt := firstN(msg.Content, 200)

		// Look for heimdall_remember in the next assistant turn.
		found := false
		for j := i + 1; j < len(msgs) && j <= i+1; j++ {
			if msgs[j].Role == "assistant" && assistantHasTool(msgs[j], "heimdall_remember") {
				found = true
				break
			}
		}

		if found {
			followed++
		} else {
			misses = append(misses, MissedCall{
				Rule:           "user_correction",
				ExpectedTool:   "heimdall_remember",
				TriggerExcerpt: excerpt,
				TurnIndex:      msg.Index,
			})
		}
	}

	return triggers, followed, misses
}

// evaluateExternalContent: when a tool_result from WebFetch or a non-heimdall
// MCP tool has more than 500 chars of content, that is a trigger. Expected:
// heimdall_index_text in the next assistant turn.
func evaluateExternalContent(msgs []parsedMsg) (int, int, []MissedCall) {
	var triggers, followed int
	var misses []MissedCall

	for i, msg := range msgs {
		// Tool results are delivered as user messages with tool_result blocks.
		if msg.Role != "user" {
			continue
		}

		for _, block := range msg.ContentBlocks {
			btype, _ := block["type"].(string)
			if btype != "tool_result" {
				continue
			}

			// The tool_use_id tells us which tool produced this result.
			// We need to find the preceding assistant turn and check which tool
			// this result corresponds to.
			toolUseID, _ := block["tool_use_id"].(string)
			if toolUseID == "" {
				continue
			}

			// Find the tool_use block in a preceding message.
			toolName := findToolNameForID(msgs[:i], toolUseID)
			if !isExternalTool(toolName) {
				continue
			}

			// Extract the text content.
			content := extractToolResultContent(block)
			if len(content) <= 500 {
				continue
			}

			// Trigger fires.
			triggers++
			excerpt := "External content from " + toolName + ": " + firstN(content, 200-len("External content from "+toolName+": "))
			if len(excerpt) > 200 {
				excerpt = excerpt[:200]
			}

			// Look for heimdall_index_text in the next assistant turn.
			found := false
			for j := i + 1; j < len(msgs) && j <= i+1; j++ {
				if msgs[j].Role == "assistant" && assistantHasTool(msgs[j], "heimdall_index_text") {
					found = true
					break
				}
			}

			if found {
				followed++
			} else {
				misses = append(misses, MissedCall{
					Rule:           "external_content",
					ExpectedTool:   "heimdall_index_text",
					TriggerExcerpt: excerpt,
					TurnIndex:      msg.Index,
				})
			}
		}
	}

	return triggers, followed, misses
}

// evaluateTaskStartRecall: only the FIRST user message is checked. If it
// matches the taskStartPattern, that is a trigger. Expected: heimdall_recall
// in the first 3 assistant turns.
func evaluateTaskStartRecall(msgs []parsedMsg) (int, int, []MissedCall) {
	if len(msgs) == 0 {
		return 0, 0, nil
	}

	// Find the first user message.
	firstUserIdx := -1
	for i, msg := range msgs {
		if msg.Role == "user" && msg.Content != "" {
			firstUserIdx = i
			break
		}
	}
	if firstUserIdx < 0 {
		return 0, 0, nil
	}

	firstUserMsg := msgs[firstUserIdx]
	if !taskStartPattern.MatchString(firstUserMsg.Content) {
		return 0, 0, nil
	}

	// Trigger fires.
	excerpt := firstN(firstUserMsg.Content, 200)

	// Look for heimdall_recall in the first 3 assistant turns after the first user message.
	assistantTurns := 0
	found := false
	for j := firstUserIdx + 1; j < len(msgs) && assistantTurns < 3; j++ {
		if msgs[j].Role != "assistant" {
			continue
		}
		assistantTurns++
		if assistantHasTool(msgs[j], "heimdall_recall") {
			found = true
			break
		}
	}

	if found {
		return 1, 1, nil
	}
	return 1, 0, []MissedCall{
		{
			Rule:           "task_start_recall",
			ExpectedTool:   "heimdall_recall",
			TriggerExcerpt: excerpt,
			TurnIndex:      firstUserMsg.Index,
		},
	}
}

// parseTranscriptMessages walks the JSONL transcript and returns a slice of
// parsedMsg with role, flattened text content, and raw content blocks.
// The Index field reflects the line's 0-based position in the file.
// Handles both the legacy top-level shape and the real Claude Code shape
// via extractRoleAndContent.
func parseTranscriptMessages(data []byte) []parsedMsg {
	var msgs []parsedMsg
	i := 0
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(line) == 0 {
			i++
			continue
		}
		var entry map[string]any
		if json.Unmarshal(line, &entry) != nil {
			i++
			continue
		}
		role, rawContent, ok := extractRoleAndContent(entry)
		if !ok {
			i++
			continue
		}
		p := parsedMsg{Role: role, Index: i}
		switch v := rawContent.(type) {
		case string:
			p.Content = v
		case []any:
			var parts []string
			for _, block := range v {
				m, ok := block.(map[string]any)
				if !ok {
					continue
				}
				p.ContentBlocks = append(p.ContentBlocks, m)
				if t, _ := m["type"].(string); t == "text" {
					if s, _ := m["text"].(string); s != "" {
						parts = append(parts, s)
					}
				}
			}
			p.Content = strings.Join(parts, "\n")
		}
		msgs = append(msgs, p)
		i++
	}
	return msgs
}

// assistantHasTool returns true if the message has a tool_use block with the
// given tool name. The comparison is performed after normalising the stored
// name via normalizeToolName so that both bare ("heimdall_remember") and
// MCP-prefixed ("mcp__heimdall__heimdall_remember") names match.
func assistantHasTool(msg parsedMsg, toolName string) bool {
	for _, block := range msg.ContentBlocks {
		if btype, _ := block["type"].(string); btype == "tool_use" {
			if name, _ := block["name"].(string); normalizeToolName(name) == toolName {
				return true
			}
		}
	}
	return false
}

// extractToolResultContent returns the text content of a tool_result block.
// The "content" field may be a string or an array of content blocks.
func extractToolResultContent(block map[string]any) string {
	raw, ok := block["content"]
	if !ok {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := m["type"].(string); t == "text" {
				if s, _ := m["text"].(string); s != "" {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

// findToolNameForID scans earlier messages for a tool_use block with the
// given ID and returns its name. Returns "" if not found.
func findToolNameForID(msgs []parsedMsg, toolUseID string) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		for _, block := range msgs[i].ContentBlocks {
			if btype, _ := block["type"].(string); btype == "tool_use" {
				if id, _ := block["id"].(string); id == toolUseID {
					name, _ := block["name"].(string)
					return name
				}
			}
		}
	}
	return ""
}

// isExternalTool returns true if the tool name is WebFetch or any MCP tool
// outside the heimdall_* namespace (i.e., mcp__* but NOT mcp__heimdall__*).
func isExternalTool(toolName string) bool {
	if toolName == "WebFetch" {
		return true
	}
	if strings.HasPrefix(toolName, "mcp__") && !strings.HasPrefix(toolName, "mcp__heimdall__") {
		return true
	}
	return false
}

// firstN returns the first n bytes of s, or s itself if shorter.
func firstN(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	return s[:n]
}
