package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// DispatchSessions routes `heimdall-mcp sessions <subcommand>`.
func DispatchSessions(cfg config.Config, stdin io.Reader, stdout, stderr io.Writer, env map[string]string, args []string) int {
	_ = stdin
	_ = env
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage: heimdall-mcp sessions <list|report> [flags]")
		return 2
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "list":
		return sessionsList(cfg, stdout, stderr, rest)
	case "report":
		return sessionsReport(cfg, stdout, stderr, rest)
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, "heimdall-mcp sessions — per-session savings metrics")
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "  sessions list                       List recent sessions seen in hooks.log")
		fmt.Fprintln(stdout, "  sessions report --session-id=<id>   Full report for one session")
		fmt.Fprintln(stdout, "                [--format=text|json]")
		fmt.Fprintln(stdout, "                [--cwd=<project-root>] (defaults to current dir)")
		return 0
	default:
		fmt.Fprintf(stderr, "Unknown sessions subcommand: %s\n", sub)
		return 2
	}
}

func sessionsList(cfg config.Config, stdout, stderr io.Writer, args []string) int {
	_ = cfg

	fs := flag.NewFlagSet("sessions list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var format string
	fs.StringVar(&format, "format", "text", "output format: text|json")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	entries, err := heimdall.ReadHookLog(heimdall.ReadHookLogOpts{})
	if err != nil {
		fmt.Fprintf(stderr, "read hooks.log: %v\n", err)
		return 1
	}
	agg := heimdall.AggregateHookLogBySession(entries)
	ids := heimdall.SortedSessionIDs(agg)
	if len(ids) == 0 {
		fmt.Fprintln(stdout, "no sessions in hooks.log (try starting a Claude Code session first)")
		return 0
	}

	if format == "json" {
		type listRow struct {
			SessionID       string `json:"session_id"`
			PromptEvents    int    `json:"user_prompt_events"`
			CacheHits       int    `json:"user_prompt_cache_hits"`
			GuardrailEvents int    `json:"pre_tool_use_events"`
		}
		rows := make([]listRow, 0, len(ids))
		for _, id := range ids {
			a := agg[id]
			rows = append(rows, listRow{
				SessionID:       id,
				PromptEvents:    a.UserPromptEvents,
				CacheHits:       a.UserPromptCacheHits,
				GuardrailEvents: a.PreToolUseEvents,
			})
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rows)
		return 0
	}

	fmt.Fprintln(stdout, "SESSION                              PROMPTS  CACHE-HITS  GUARDRAIL")
	for _, id := range ids {
		a := agg[id]
		fmt.Fprintf(stdout, "%-36s  prompts=%d  cache_hits=%d  guardrail=%d\n",
			id, a.UserPromptEvents, a.UserPromptCacheHits, a.PreToolUseEvents)
	}
	return 0
}

func sessionsReport(cfg config.Config, stdout, stderr io.Writer, args []string) int {
	_ = cfg
	fs := flag.NewFlagSet("sessions report", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		sessionID string
		format    string
		cwd       string
		home      string
	)
	fs.StringVar(&sessionID, "session-id", "", "target Claude Code session id (required)")
	fs.StringVar(&format, "format", "text", "output format: text|json")
	fs.StringVar(&cwd, "cwd", "", "project cwd for transcript lookup (default: os.Getwd)")
	fs.StringVar(&home, "home", "", "override home dir for transcript lookup (debug/tests)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if sessionID == "" {
		fmt.Fprintln(stderr, "--session-id is required")
		return 2
	}
	if cwd == "" {
		if c, err := os.Getwd(); err == nil {
			cwd = c
		}
	}

	entries, err := heimdall.ReadHookLog(heimdall.ReadHookLogOpts{Session: sessionID})
	if err != nil {
		fmt.Fprintf(stderr, "read hooks.log: %v\n", err)
		return 1
	}
	aggMap := heimdall.AggregateHookLogBySession(entries)
	hookAgg := aggMap[sessionID]

	transcriptPath, perr := heimdall.TranscriptPathForSession(cwd, sessionID, home)
	var tsum heimdall.TranscriptSummary
	var transcriptErr error
	if perr == nil {
		tsum, transcriptErr = heimdall.ParseTranscript(transcriptPath)
	} else {
		transcriptErr = perr
	}

	report := SessionReport{
		SessionID:  sessionID,
		CWD:        cwd,
		Transcript: tsum,
		Hooks:      hookAgg,
		Duration:   transcriptDuration(tsum),
		Now:        time.Now().UTC(),
	}
	if transcriptErr != nil {
		report.TranscriptError = transcriptErr.Error()
	}

	switch format {
	case "json":
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report.toJSON())
		return 0
	case "text":
		renderSessionReportText(stdout, report)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown format: %s\n", format)
		return 2
	}
}

// SessionReport is the joined per-session view used by both renderers.
type SessionReport struct {
	SessionID       string
	CWD             string
	Transcript      heimdall.TranscriptSummary
	Hooks           heimdall.SessionHookAggregate
	Duration        time.Duration
	Now             time.Time
	TranscriptError string
}

func transcriptDuration(t heimdall.TranscriptSummary) time.Duration {
	if t.FirstTimestamp.IsZero() || t.LastTimestamp.IsZero() {
		return 0
	}
	return t.LastTimestamp.Sub(t.FirstTimestamp)
}

func renderSessionReportText(w io.Writer, r SessionReport) {
	fmt.Fprintf(w, "# Session %s\n\n", r.SessionID)
	fmt.Fprintf(w, "CWD:           %s\n", r.CWD)
	if r.Duration > 0 {
		fmt.Fprintf(w, "Duration:      %s\n", r.Duration.Round(time.Second))
	}
	fmt.Fprintf(w, "\n## Transcript tokens\n")
	fmt.Fprintf(w, "  input_tokens=%d\n", r.Transcript.TotalInputTokens)
	fmt.Fprintf(w, "  cache_creation=%d\n", r.Transcript.TotalCacheCreationTokens)
	fmt.Fprintf(w, "  cache_read=%d\n", r.Transcript.TotalCacheReadTokens)
	fmt.Fprintf(w, "  output_tokens=%d\n", r.Transcript.TotalOutputTokens)
	fmt.Fprintf(w, "\n## Tool use\n")
	fmt.Fprintf(w, "  total=%d heimdall=%d\n", r.Transcript.ToolUseCount, r.Transcript.HeimdallToolCalls)
	names := make([]string, 0, len(r.Transcript.ToolUseByName))
	for n := range r.Transcript.ToolUseByName {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(w, "  - %s: %d\n", n, r.Transcript.ToolUseByName[n])
	}
	fmt.Fprintf(w, "\n## Heimdall contribution\n")
	fmt.Fprintf(w, "  SessionStart bytes=%d events=%d\n",
		r.Transcript.HookSuccessBytesByEvent["SessionStart"], r.Transcript.HookSuccessCountByEvent["SessionStart"])
	fmt.Fprintf(w, "  UserPromptSubmit bytes=%d events=%d\n",
		r.Transcript.HookSuccessBytesByEvent["UserPromptSubmit"], r.Transcript.HookSuccessCountByEvent["UserPromptSubmit"])
	fmt.Fprintf(w, "  prompts=%d cache_hits=%d skips=%d bytes_injected=%d\n",
		r.Hooks.UserPromptEvents, r.Hooks.UserPromptCacheHits, r.Hooks.UserPromptSkips, r.Hooks.UserPromptBytesInjected)
	fmt.Fprintf(w, "\n## Guardrails\n")
	fmt.Fprintf(w, "  guardrail=%d verdicts=%v\n", r.Hooks.PreToolUseEvents, r.Hooks.GuardrailVerdicts)
	fmt.Fprintf(w, "\n## Reindexes\n")
	fmt.Fprintf(w, "  post_edit_events=%d reindex_ok=%d\n", r.Hooks.PostEditEvents, r.Hooks.PostEditReindexes)
	if r.TranscriptError != "" {
		fmt.Fprintf(w, "\n(transcript unavailable: %s)\n", r.TranscriptError)
	}
}

// toJSON renders a map matching the text sections with stable keys.
func (r SessionReport) toJSON() map[string]interface{} {
	return map[string]interface{}{
		"session_id":   r.SessionID,
		"cwd":          r.CWD,
		"duration_sec": int64(r.Duration.Seconds()),
		"tokens": map[string]int64{
			"input":          r.Transcript.TotalInputTokens,
			"cache_creation": r.Transcript.TotalCacheCreationTokens,
			"cache_read":     r.Transcript.TotalCacheReadTokens,
			"output":         r.Transcript.TotalOutputTokens,
		},
		"tool_use": map[string]interface{}{
			"total":    r.Transcript.ToolUseCount,
			"heimdall": r.Transcript.HeimdallToolCalls,
			"by_name":  r.Transcript.ToolUseByName,
		},
		"heimdall_contribution": map[string]interface{}{
			"session_start_bytes":  r.Transcript.HookSuccessBytesByEvent["SessionStart"],
			"session_start_events": r.Transcript.HookSuccessCountByEvent["SessionStart"],
			"prompt_bytes":         r.Transcript.HookSuccessBytesByEvent["UserPromptSubmit"],
			"prompt_events":        r.Transcript.HookSuccessCountByEvent["UserPromptSubmit"],
			"cache_hits":           r.Hooks.UserPromptCacheHits,
			"skips":                r.Hooks.UserPromptSkips,
			"bytes_injected":       r.Hooks.UserPromptBytesInjected,
		},
		"guardrails": map[string]interface{}{
			"events":   r.Hooks.PreToolUseEvents,
			"verdicts": r.Hooks.GuardrailVerdicts,
		},
		"reindexes": map[string]interface{}{
			"events":     r.Hooks.PostEditEvents,
			"reindex_ok": r.Hooks.PostEditReindexes,
		},
		"transcript_error": r.TranscriptError,
	}
}
