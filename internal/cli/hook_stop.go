package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// stopPayload is the JSON payload sent by Claude Code on the Stop hook event.
type stopPayload struct {
	SessionID            string `json:"session_id"`
	TranscriptPath       string `json:"transcript_path"`
	CWD                  string `json:"cwd"`
	LastAssistantMessage string `json:"last_assistant_message"`
	StopHookActive       bool   `json:"stop_hook_active"`
	HookEventName        string `json:"hook_event_name"`
}

// sessionEndPayload is the JSON payload sent on the SessionEnd hook event.
type sessionEndPayload struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	CWD            string `json:"cwd"`
	HookEventName  string `json:"hook_event_name"`
	Reason         string `json:"reason"`
}

// HookStop handles the Stop hook event. Appends the last assistant message
// to a rolling buffer file keyed by session_id. Always exits 0 (OQ-5).
func HookStop(_ config.Config, stdin io.Reader, stdout, _ io.Writer, env map[string]string, args []string) int {
	projectRoot := ""
	if heimdall.HooksDisabled(projectRoot, env) {
		return 0
	}

	data, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil || len(data) == 0 {
		heimdall.LogHookEvent("WARN", "stop", map[string]any{"err": "stdin_read_failed"})
		return 0
	}

	var payload stopPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		heimdall.LogHookEvent("WARN", "stop", map[string]any{"err": "json_parse_failed"})
		return 0
	}

	if payload.SessionID == "" || payload.LastAssistantMessage == "" {
		heimdall.LogHookEvent("DEBUG", "stop", map[string]any{"msg": "empty_session_or_message"})
		return 0
	}

	projectDir := payload.CWD
	if projectDir == "" {
		projectDir, _ = os.Getwd()
	}

	bufferDir := resolveBufferDir(projectDir)
	if err := os.MkdirAll(bufferDir, 0700); err != nil {
		heimdall.LogHookEvent("WARN", "stop", map[string]any{"err": "mkdir_failed"})
		return 0
	}

	bufferPath := filepath.Join(bufferDir, payload.SessionID+".jsonl")
	entry := map[string]any{
		"ts":      time.Now().Unix(),
		"message": payload.LastAssistantMessage,
	}
	line, _ := json.Marshal(entry)
	line = append(line, '\n')

	f, err := os.OpenFile(bufferPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		heimdall.LogHookEvent("WARN", "stop", map[string]any{"err": "buffer_open_failed"})
		return 0
	}
	defer f.Close()

	info, _ := f.Stat()
	if info != nil && info.Size()+int64(len(line)) > 2*1024*1024 {
		heimdall.LogHookEvent("INFO", "stop", map[string]any{"msg": "buffer_capped", "session": payload.SessionID})
		return 0
	}

	f.Write(line)

	heimdall.LogHookEvent("INFO", "stop", map[string]any{
		"session": payload.SessionID,
		"msg":     "buffer_appended",
		"bytes":   len(line),
	})

	return 0
}

// HookSessionEnd handles the SessionEnd hook event. Triggers ingest-session
// from the transcript path when the session ends. Always exits 0 (OQ-5).
func HookSessionEnd(cfg config.Config, stdin io.Reader, stdout, _ io.Writer, env map[string]string, args []string) int {
	projectRoot := ""
	if heimdall.HooksDisabled(projectRoot, env) {
		return 0
	}

	data, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil || len(data) == 0 {
		heimdall.LogHookEvent("WARN", "session-end", map[string]any{"err": "stdin_read_failed"})
		return 0
	}

	var payload sessionEndPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		heimdall.LogHookEvent("WARN", "session-end", map[string]any{"err": "json_parse_failed"})
		return 0
	}

	if payload.SessionID == "" {
		heimdall.LogHookEvent("DEBUG", "session-end", map[string]any{"msg": "empty_session"})
		return 0
	}

	projectDir := payload.CWD
	if projectDir == "" {
		projectDir, _ = os.Getwd()
	}

	bufferDir := resolveBufferDir(projectDir)
	bufferPath := filepath.Join(bufferDir, payload.SessionID+".jsonl")

	if payload.TranscriptPath != "" {
		if _, err := os.Stat(payload.TranscriptPath); err == nil {
			transcript, err := os.ReadFile(payload.TranscriptPath)
			if err == nil && len(transcript) > 0 {
				summary, candidates := extractTranscriptSummary(transcript)
				writes := countHeimdallWrites(transcript)
				analysis := AnalyzeTranscript(transcript)

				if summary != "" {
					memDBPath := config.ResolveMemoryDBPath()
					memStore, err := heimdall.OpenMemoryStore(memDBPath)
					if err == nil {
						defer memStore.Close()
						ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
						defer cancel()
						client := newOllamaClient(cfg)
						embedder := heimdall.NewOllamaEmbedder(client, cfg.Model)
						result, err := heimdall.IngestSessionSummary(ctx, summary, "", embedder, memStore)
						if err == nil {
							heimdall.LogHookEvent("INFO", "session-end", map[string]any{
								"session":  payload.SessionID,
								"reason":   payload.Reason,
								"msg":      "ingest_ok",
								"memories": result.MemoriesCreated + result.MemoriesUpdated,
							})
						}
					}
				}

				if err := writeLastSessionReview(projectDir, payload.SessionID, time.Now(), candidates, writes, analysis.Misses, analysis); err != nil {
					heimdall.LogHookEvent("WARN", "session-end", map[string]any{
						"err": "review_write_failed",
						"msg": err.Error(),
					})
				}
			}
		}
	}

	os.Remove(bufferPath)

	heimdall.LogHookEvent("INFO", "session-end", map[string]any{
		"session": payload.SessionID,
		"reason":  payload.Reason,
		"msg":     "session_ended",
	})

	return 0
}

func resolveBufferDir(projectDir string) string {
	return filepath.Join(projectDir, ".heimdall_db", "hooks", "sessions")
}

// CandidateEvent is one remember-worthy moment detected in a transcript scan.
// Excerpt is capped at 200 chars; Marker is one of: correction, frustration,
// teaching, workaround.
type CandidateEvent struct {
	Marker  string
	Excerpt string
}

// Marker priority (highest first) for review-record ranking.
var candidateMarkerPriority = []string{"correction", "workaround", "teaching", "frustration"}

// markerPatterns maps marker class → compiled regex. Regexes scan user
// messages only. Precompiled at package init.
var markerPatterns = map[string]*regexp.Regexp{
	"correction":  regexp.MustCompile(`(?i)\bactually\b|\bno,?\s|\bdon['']?t\b|\bstop\b|\binstead\b|\bwrong\b`),
	"frustration": regexp.MustCompile(`(?i)\bfuck\b|\bwhy\b|\bbroken\b`),
	"teaching":    regexp.MustCompile(`(?i)\bturns out\b|\bfyi\b|\bheads up\b|\bfor reference\b`),
	"workaround":  regexp.MustCompile(`(?i)\bworkaround\b|\bhack\b|\btrick\b|\bgotcha\b`),
}

const (
	extractorMaxOutput  = 20 * 1024
	extractorExcerptMax = 200
	extractorTailCount  = 3
	extractorTailLen    = 800
)

// extractTranscriptSummary scans the full JSONL transcript for user-side
// marker hits and returns:
//   - summary: concatenated context windows around each hit. Each window
//     contains the user message plus the preceding assistant message, is
//     deduplicated, joined with "\n\n---\n\n", and capped at 20 KB. On
//     zero hits, falls back to the last 3 assistant messages trimmed to
//     800 chars each.
//   - candidates: one entry per hit with marker class plus a ≤200-char
//     excerpt of the user message.
//
// Single byte-scan pass; O(len(data)) + O(hits × regex_cost).
// roleFromLine extracts the "role" field value from a JSONL line without
// full JSON decoding. Returns "" if the pattern is not found.
func roleFromLine(line []byte) string {
	const prefix = `"role":"`
	idx := bytes.Index(line, []byte(prefix))
	if idx < 0 {
		return ""
	}
	rest := line[idx+len(prefix):]
	end := bytes.IndexByte(rest, '"')
	if end < 0 {
		return ""
	}
	return string(rest[:end])
}

// contentRawFromLine extracts the raw JSON value of the "content" key from
// a JSONL line via lightweight struct decode (avoids map[string]any allocs).
func contentRawFromLine(line []byte) json.RawMessage {
	var e struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(line, &e) != nil {
		return nil
	}
	return e.Content
}

func extractTranscriptSummary(data []byte) (string, []CandidateEvent) {
	// prevAssistantRaw stores the raw JSONL line of the most recent assistant
	// message, for lazy content decoding when a user marker hit is found.
	// prevAssistantTail stores the trimmed content for tail-fallback tracking.
	// We decode full content only for user messages (always small) and for
	// the single preceding assistant message when a marker hits.

	var candidates []CandidateEvent
	var windows []string
	seen := map[string]bool{}

	// Rolling tail: last extractorTailCount assistant message raw lines.
	type rawLine []byte
	tailRaw := make([]rawLine, 0, extractorTailCount)
	var prevRole string
	var prevAssistantRaw []byte

	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 2*1024*1024), 2*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		role := roleFromLine(line)
		if role == "" {
			continue
		}

		if role == "assistant" {
			// Store raw line for lazy decode; update tail ring.
			lineCopy := append([]byte(nil), line...)
			prevAssistantRaw = lineCopy
			if len(tailRaw) < extractorTailCount {
				tailRaw = append(tailRaw, lineCopy)
			} else {
				copy(tailRaw, tailRaw[1:])
				tailRaw[extractorTailCount-1] = lineCopy
			}
			prevRole = "assistant"
		} else if role == "user" {
			// Decode user content (user messages are always small).
			raw := contentRawFromLine(line)
			if raw == nil {
				prevRole = "user"
				prevAssistantRaw = nil
				continue
			}
			content := extractContentTextRaw(raw)
			if content == "" {
				prevRole = "user"
				prevAssistantRaw = nil
				continue
			}

			for _, marker := range candidateMarkerPriority {
				if markerPatterns[marker].MatchString(content) {
					excerpt := content
					if len(excerpt) > extractorExcerptMax {
						excerpt = excerpt[:extractorExcerptMax]
					}
					candidates = append(candidates, CandidateEvent{Marker: marker, Excerpt: excerpt})

					var win strings.Builder
					if prevRole == "assistant" && prevAssistantRaw != nil {
						// Lazy-decode the preceding assistant message now.
						if prevRaw := contentRawFromLine(prevAssistantRaw); prevRaw != nil {
							prevContent := extractContentTextRaw(prevRaw)
							if prevContent != "" {
								win.WriteString("assistant: ")
								win.WriteString(trimTo(prevContent, extractorTailLen))
								win.WriteString("\n\n")
							}
						}
					}
					win.WriteString("user: ")
					win.WriteString(trimTo(content, extractorTailLen))
					s := win.String()
					if !seen[s] {
						seen[s] = true
						windows = append(windows, s)
					}
					break
				}
			}
			prevRole = "user"
			prevAssistantRaw = nil
		} else {
			prevRole = role
			prevAssistantRaw = nil
		}
	}

	if len(windows) == 0 {
		// Decode tail assistant messages for fallback summary.
		decoded := make([]string, 0, len(tailRaw))
		for _, raw := range tailRaw {
			if cr := contentRawFromLine(raw); cr != nil {
				if c := extractContentTextRaw(cr); c != "" {
					decoded = append(decoded, trimTo(c, extractorTailLen))
				}
			}
		}
		windows = decoded
	}

	summary := strings.Join(windows, "\n\n---\n\n")
	if len(summary) > extractorMaxOutput {
		summary = summary[:extractorMaxOutput]
	}
	return summary, candidates
}

// extractContentTextRaw is like extractContentText but operates on
// json.RawMessage to avoid double-parsing the content field.
func extractContentTextRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Quick heuristic: if it starts with '"' it's a string.
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		return ""
	}
	// Otherwise treat as array of content blocks.
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		var m struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(b, &m) == nil && m.Type == "text" && m.Text != "" {
			parts = append(parts, m.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func trimTo(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// countHeimdallWrites returns the number of tool_use blocks in the
// transcript whose name is heimdall_remember or heimdall_index_text.
func countHeimdallWrites(data []byte) int {
	n := 0
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 2*1024*1024), 2*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var entry map[string]any
		if json.Unmarshal(line, &entry) != nil {
			continue
		}
		raw, ok := entry["content"].([]any)
		if !ok {
			continue
		}
		for _, block := range raw {
			m, ok := block.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := m["type"].(string); t != "tool_use" {
				continue
			}
			name, _ := m["name"].(string)
			if name == "heimdall_remember" || name == "heimdall_index_text" {
				n++
			}
		}
	}
	return n
}

// writeLastSessionReview persists a review record when the session warrants
// nagging the next SessionStart. Suppression: skip when candidates <= 2 AND
// writes >= 1 AND len(misses) == 0 (well-behaved session). Only 3 highest-
// priority excerpts; up to 5 missed-call entries.
func writeLastSessionReview(projectDir, sessionID string, endedAt time.Time, candidates []CandidateEvent, writes int, misses []MissedCall, analysis AnalysisResult) error {
	if len(candidates) <= 2 && writes >= 1 && len(misses) == 0 {
		return nil
	}

	priorityIdx := map[string]int{}
	for i, m := range candidateMarkerPriority {
		priorityIdx[m] = i
	}
	ranked := make([]CandidateEvent, len(candidates))
	copy(ranked, candidates)
	sort.SliceStable(ranked, func(i, j int) bool {
		return priorityIdx[ranked[i].Marker] < priorityIdx[ranked[j].Marker]
	})

	topMarkers := make([]string, 0, 3)
	excerpts := make([]string, 0, 3)
	for i, c := range ranked {
		if i >= 3 {
			break
		}
		topMarkers = append(topMarkers, c.Marker)
		excerpts = append(excerpts, c.Excerpt)
	}

	// Serialize up to 5 miss entries for the review record.
	type missRecord struct {
		Rule    string `json:"rule"`
		ExpTool string `json:"expected_tool"`
		Trigger string `json:"trigger"`
		Turn    int    `json:"turn"`
	}
	missRecords := make([]missRecord, 0, len(misses))
	for i, mc := range misses {
		if i >= 5 {
			break
		}
		missRecords = append(missRecords, missRecord{
			Rule:    mc.Rule,
			ExpTool: mc.ExpectedTool,
			Trigger: mc.TriggerExcerpt,
			Turn:    mc.TurnIndex,
		})
	}

	record := map[string]any{
		"session_id":  sessionID,
		"ended_at":    endedAt.Unix(),
		"candidates":  len(candidates),
		"writes":      writes,
		"top_markers": topMarkers,
		"excerpts":    excerpts,
		"misses":      missRecords,
		"triggers":    analysis.Triggers,
		"followed":    analysis.Followed,
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Join(projectDir, ".heimdall_db", "hooks")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "last-session-review.json"), data, 0600)
}
