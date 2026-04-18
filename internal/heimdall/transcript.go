package heimdall

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TranscriptSummary is the aggregate view of a single Claude Code session
// transcript. All counts are 0 when no matching events were seen.
type TranscriptSummary struct {
	SessionID string
	Path      string

	FirstTimestamp time.Time
	LastTimestamp  time.Time

	UserMessages      int
	AssistantMessages int

	TotalInputTokens         int64
	TotalCacheCreationTokens int64
	TotalCacheReadTokens     int64
	TotalOutputTokens        int64

	ToolUseCount      int
	ToolUseByName     map[string]int
	HeimdallToolCalls int // subset: any tool whose name starts with mcp__heimdall__

	// RedundantHeimdallCalls counts mcp__heimdall__* tool calls made by the
	// assistant in turns where the preceding UserPromptSubmit hook already
	// injected >=1 hit (i.e. a hook_success attachment with non-empty stdout).
	// A turn is defined as the span from one user message to the next. An
	// "attachment with stdout" flips the turn into "had-hits" mode; every
	// heimdall_* tool_use in an assistant message that follows while in
	// that mode counts as redundant. Reset on the next user message.
	RedundantHeimdallCalls int

	HookSuccessBytesByEvent map[string]int64 // e.g. "SessionStart" → 1240
	HookSuccessCountByEvent map[string]int

	ParseErrors int // non-fatal malformed lines
}

// TranscriptPathForSession returns the filesystem path Claude Code uses for a
// transcript file given the working directory and session id. home is injected
// so tests can exercise the slug rule without depending on os.UserHomeDir.
func TranscriptPathForSession(cwd, sessionID, home string) (string, error) {
	if sessionID == "" {
		return "", errors.New("empty session id")
	}
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("home dir: %w", err)
		}
		home = h
	}
	clean := filepath.Clean(cwd)
	slug := strings.ReplaceAll(clean, string(os.PathSeparator), "-")
	return filepath.Join(home, ".claude", "projects", slug, sessionID+".jsonl"), nil
}

// ParseTranscript streams the Claude Code transcript at path and returns a
// TranscriptSummary. Unknown event types are ignored. Malformed JSON lines
// increment ParseErrors but do not abort the parse — the transcript is only
// ever appended to, so a partial last line is treated as tolerable.
func ParseTranscript(path string) (TranscriptSummary, error) {
	sum := TranscriptSummary{
		Path:                    path,
		ToolUseByName:           map[string]int{},
		HookSuccessBytesByEvent: map[string]int64{},
		HookSuccessCountByEvent: map[string]int{},
	}

	f, err := os.Open(path)
	if err != nil {
		return sum, fmt.Errorf("open transcript: %w", err)
	}
	defer f.Close()

	// turnHasPromptHits is true once the current turn (the span since the
	// most-recent "user" message) has seen at least one UserPromptSubmit
	// hook_success attachment with non-empty stdout. Drives the redundant-
	// heimdall-calls accounting — see RedundantHeimdallCalls docstring.
	var turnHasPromptHits bool

	r := bufio.NewReaderSize(f, 128*1024)
	for {
		line, rerr := r.ReadBytes('\n')
		if len(line) > 0 {
			applyTranscriptLine(&sum, line, &turnHasPromptHits)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return sum, fmt.Errorf("read transcript: %w", rerr)
		}
	}
	return sum, nil
}

// transcriptEnvelope is the subset of a transcript line we care about.
type transcriptEnvelope struct {
	Type       string          `json:"type"`
	SessionID  string          `json:"sessionId"`
	Timestamp  string          `json:"timestamp"`
	Message    json.RawMessage `json:"message"`
	Attachment json.RawMessage `json:"attachment"`
}

func applyTranscriptLine(sum *TranscriptSummary, raw []byte, turnHasPromptHits *bool) {
	trimmed := bytesTrimSpaceTranscript(raw)
	if len(trimmed) == 0 {
		return
	}
	var env transcriptEnvelope
	if err := json.Unmarshal(trimmed, &env); err != nil {
		sum.ParseErrors++
		return
	}
	if sum.SessionID == "" && env.SessionID != "" {
		sum.SessionID = env.SessionID
	}
	if env.Timestamp != "" {
		if ts, err := time.Parse(time.RFC3339, env.Timestamp); err == nil {
			if sum.FirstTimestamp.IsZero() || ts.Before(sum.FirstTimestamp) {
				sum.FirstTimestamp = ts
			}
			if ts.After(sum.LastTimestamp) {
				sum.LastTimestamp = ts
			}
		}
	}

	switch env.Type {
	case "user":
		sum.UserMessages++
		// New turn starts — reset the had-prompt-hits flag.
		*turnHasPromptHits = false
	case "assistant":
		sum.AssistantMessages++
		applyAssistantMessage(sum, env.Message, *turnHasPromptHits)
	case "attachment":
		applyAttachment(sum, env.Attachment, turnHasPromptHits)
	}
}

type assistantMessage struct {
	Content []struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"content"`
	Usage struct {
		InputTokens              int64 `json:"input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
	} `json:"usage"`
}

func applyAssistantMessage(sum *TranscriptSummary, raw json.RawMessage, turnHadPromptHits bool) {
	if len(raw) == 0 {
		return
	}
	var msg assistantMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return
	}
	sum.TotalInputTokens += msg.Usage.InputTokens
	sum.TotalCacheCreationTokens += msg.Usage.CacheCreationInputTokens
	sum.TotalCacheReadTokens += msg.Usage.CacheReadInputTokens
	sum.TotalOutputTokens += msg.Usage.OutputTokens
	for _, c := range msg.Content {
		if c.Type != "tool_use" {
			continue
		}
		sum.ToolUseCount++
		name := c.Name
		if name == "" {
			name = "(unnamed)"
		}
		sum.ToolUseByName[name]++
		if strings.HasPrefix(name, "mcp__heimdall__") {
			sum.HeimdallToolCalls++
			if turnHadPromptHits {
				// Hook already injected context this turn; the model called
				// heimdall anyway. Mark the call as redundant (v1 definition;
				// no semantic-overlap scoring — that's item 3(A), deferred).
				sum.RedundantHeimdallCalls++
			}
		}
	}
}

type hookSuccessAttachment struct {
	Type      string `json:"type"`
	HookEvent string `json:"hookEvent"`
	Stdout    string `json:"stdout"`
}

func applyAttachment(sum *TranscriptSummary, raw json.RawMessage, turnHasPromptHits *bool) {
	if len(raw) == 0 {
		return
	}
	var a hookSuccessAttachment
	if err := json.Unmarshal(raw, &a); err != nil {
		return
	}
	if a.Type != "hook_success" || a.HookEvent == "" {
		return
	}
	sum.HookSuccessCountByEvent[a.HookEvent]++
	sum.HookSuccessBytesByEvent[a.HookEvent] += int64(len(a.Stdout))
	// A UserPromptSubmit hook fire with non-empty stdout means the hook
	// injected context (>=1 hit) for this turn. Empty stdout is the
	// skip/no-hits path and does not set the flag.
	if a.HookEvent == "UserPromptSubmit" && len(a.Stdout) > 0 {
		*turnHasPromptHits = true
	}
}

// bytesTrimSpaceTranscript is a tiny byte-level TrimSpace local to this file,
// so the transcript parser has no cross-package dependency on hook.go helpers.
func bytesTrimSpaceTranscript(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && isASCIISpace(b[start]) {
		start++
	}
	for end > start && isASCIISpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isASCIISpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
