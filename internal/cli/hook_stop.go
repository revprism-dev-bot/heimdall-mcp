package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
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
				summary := extractTranscriptSummary(transcript)
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

func extractTranscriptSummary(data []byte) string {
	var messages []string
	start := 0
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			line := data[start:i]
			start = i + 1
			if len(line) == 0 {
				continue
			}
			var entry map[string]any
			if json.Unmarshal(line, &entry) != nil {
				continue
			}
			if role, _ := entry["role"].(string); role == "assistant" {
				if content, _ := entry["content"].(string); content != "" {
					messages = append(messages, content)
				}
			}
		}
	}
	if start < len(data) {
		line := data[start:]
		if len(line) > 0 {
			var entry map[string]any
			if json.Unmarshal(line, &entry) == nil {
				if role, _ := entry["role"].(string); role == "assistant" {
					if content, _ := entry["content"].(string); content != "" {
						messages = append(messages, content)
					}
				}
			}
		}
	}

	if len(messages) > 5 {
		messages = messages[len(messages)-5:]
	}
	if len(messages) == 0 {
		return ""
	}
	result := ""
	for _, m := range messages {
		if len(m) > 500 {
			m = m[:500]
		}
		result += m + "\n\n"
	}
	if len(result) > 5000 {
		result = result[:5000]
	}
	return result
}
