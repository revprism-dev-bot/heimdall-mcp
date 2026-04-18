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

// SessionsReportSchemaVersion is the JSON schema version emitted by
// `sessions list --format=json` and `sessions report --format=json`. Bump
// on any breaking field change (rename, removal, type change). Additive
// changes (new fields) do not require a bump. Consumers should accept
// unknown fields for forward compatibility.
const SessionsReportSchemaVersion = "v1"

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
		fmt.Fprintln(stdout, "  sessions list [--since=<dur>] [--format=text|json]")
		fmt.Fprintln(stdout, "                List recent sessions seen in hooks.log. --since")
		fmt.Fprintln(stdout, "                accepts a Go duration (24h, 30m, etc.) and keeps")
		fmt.Fprintln(stdout, "                only sessions whose last event falls inside the window.")
		fmt.Fprintln(stdout, "  sessions report (--session-id=<id> | --current) [--format=text|json]")
		fmt.Fprintln(stdout, "                [--cwd=<project-root>] (defaults to current dir)")
		fmt.Fprintln(stdout, "                --current auto-picks the most-recent session from hooks.log.")
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
	var (
		format   string
		sinceRaw string
	)
	fs.StringVar(&format, "format", "text", "output format: text|json")
	fs.StringVar(&sinceRaw, "since", "", "keep sessions whose last event is within this window (e.g. 24h, 30m)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	var cutoff time.Time
	if sinceRaw != "" {
		d, perr := time.ParseDuration(sinceRaw)
		if perr != nil {
			fmt.Fprintf(stderr, "invalid --since value %q: %v\n", sinceRaw, perr)
			return 2
		}
		if d < 0 {
			fmt.Fprintf(stderr, "invalid --since value %q: must be non-negative\n", sinceRaw)
			return 2
		}
		cutoff = time.Now().UTC().Add(-d)
	}

	entries, err := heimdall.ReadHookLog(heimdall.ReadHookLogOpts{})
	if err != nil {
		fmt.Fprintf(stderr, "read hooks.log: %v\n", err)
		return 1
	}
	agg := heimdall.AggregateHookLogBySession(entries)
	ids := heimdall.SortedSessionIDs(agg)

	if !cutoff.IsZero() {
		kept := ids[:0]
		for _, id := range ids {
			if agg[id].LastSeen.After(cutoff) || agg[id].LastSeen.Equal(cutoff) {
				kept = append(kept, id)
			}
		}
		ids = kept
	}

	if len(ids) == 0 {
		if format == "json" {
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(map[string]interface{}{
				"schema_version": SessionsReportSchemaVersion,
				"sessions":       []any{},
			})
			return 0
		}
		if !cutoff.IsZero() {
			fmt.Fprintf(stdout, "no sessions in hooks.log within --since=%s window\n", sinceRaw)
		} else {
			fmt.Fprintln(stdout, "no sessions in hooks.log (try starting a Claude Code session first)")
		}
		return 0
	}

	if format == "json" {
		type listRow struct {
			SessionID       string `json:"session_id"`
			FirstSeen       string `json:"first_seen,omitempty"`
			LastSeen        string `json:"last_seen,omitempty"`
			PromptEvents    int    `json:"user_prompt_events"`
			CacheHits       int    `json:"user_prompt_cache_hits"`
			GuardrailEvents int    `json:"pre_tool_use_events"`
		}
		rows := make([]listRow, 0, len(ids))
		for _, id := range ids {
			a := agg[id]
			row := listRow{
				SessionID:       id,
				PromptEvents:    a.UserPromptEvents,
				CacheHits:       a.UserPromptCacheHits,
				GuardrailEvents: a.PreToolUseEvents,
			}
			if !a.FirstSeen.IsZero() {
				row.FirstSeen = a.FirstSeen.UTC().Format(time.RFC3339)
			}
			if !a.LastSeen.IsZero() {
				row.LastSeen = a.LastSeen.UTC().Format(time.RFC3339)
			}
			rows = append(rows, row)
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]interface{}{
			"schema_version": SessionsReportSchemaVersion,
			"sessions":       rows,
		})
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
		current   bool
	)
	fs.StringVar(&sessionID, "session-id", "", "target Claude Code session id")
	fs.BoolVar(&current, "current", false, "auto-pick the most-recent session from hooks.log")
	fs.StringVar(&format, "format", "text", "output format: text|json")
	fs.StringVar(&cwd, "cwd", "", "project cwd for transcript lookup (default: os.Getwd)")
	fs.StringVar(&home, "home", "", "override home dir for transcript lookup (debug/tests)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if sessionID != "" && current {
		fmt.Fprintln(stderr, "--session-id and --current are mutually exclusive")
		return 2
	}
	if sessionID == "" && !current {
		fmt.Fprintln(stderr, "--session-id or --current is required")
		return 2
	}
	if current {
		allEntries, err := heimdall.ReadHookLog(heimdall.ReadHookLogOpts{})
		if err != nil {
			fmt.Fprintf(stderr, "read hooks.log: %v\n", err)
			return 1
		}
		sessionID = heimdall.MostRecentSessionID(heimdall.AggregateHookLogBySession(allEntries))
		if sessionID == "" {
			fmt.Fprintln(stderr, "--current: no sessions in hooks.log yet")
			return 1
		}
		fmt.Fprintf(stderr, "resolved --current to session %s\n", sessionID)
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
	fmt.Fprintf(w, "  total=%d heimdall=%d redundant_heimdall_calls=%d\n",
		r.Transcript.ToolUseCount, r.Transcript.HeimdallToolCalls, r.Transcript.RedundantHeimdallCalls)
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
	// Cache-hit ratio summary. Format is "cache_hits=<hits>/<total>" where
	// total = stage=ok + stage=cache_hit (skips and degraded stages
	// excluded). Zero-events renders "cache_hits=0/0" — consumers compute
	// the ratio and handle divide-by-zero themselves.
	fmt.Fprintf(w, "  cache_hits=%d/%d\n",
		r.Hooks.UserPromptCacheHits, r.Hooks.UserPromptCacheTotal)
	fmt.Fprintf(w, "\n## Guardrails\n")
	fmt.Fprintf(w, "  guardrail=%d verdicts=%v\n", r.Hooks.PreToolUseEvents, r.Hooks.GuardrailVerdicts)
	fmt.Fprintf(w, "\n## Reindexes\n")
	fmt.Fprintf(w, "  post_edit_events=%d reindex_ok=%d\n", r.Hooks.PostEditEvents, r.Hooks.PostEditReindexes)
	if r.TranscriptError != "" {
		fmt.Fprintf(w, "\n(transcript unavailable: %s)\n", r.TranscriptError)
	}
}

// toJSON renders a map matching the text sections with stable keys.
// The schema_version key lets downstream consumers detect incompatible
// changes. Bump SessionsReportSchemaVersion on breaking changes only.
func (r SessionReport) toJSON() map[string]interface{} {
	return map[string]interface{}{
		"schema_version": SessionsReportSchemaVersion,
		"session_id":     r.SessionID,
		"cwd":            r.CWD,
		"duration_sec":   int64(r.Duration.Seconds()),
		"tokens": map[string]int64{
			"input":          r.Transcript.TotalInputTokens,
			"cache_creation": r.Transcript.TotalCacheCreationTokens,
			"cache_read":     r.Transcript.TotalCacheReadTokens,
			"output":         r.Transcript.TotalOutputTokens,
		},
		"tool_use": map[string]interface{}{
			"total":                    r.Transcript.ToolUseCount,
			"heimdall":                 r.Transcript.HeimdallToolCalls,
			"redundant_heimdall_calls": r.Transcript.RedundantHeimdallCalls,
			"by_name":                  r.Transcript.ToolUseByName,
		},
		"heimdall_contribution": map[string]interface{}{
			"session_start_bytes":  r.Transcript.HookSuccessBytesByEvent["SessionStart"],
			"session_start_events": r.Transcript.HookSuccessCountByEvent["SessionStart"],
			"prompt_bytes":         r.Transcript.HookSuccessBytesByEvent["UserPromptSubmit"],
			"prompt_events":        r.Transcript.HookSuccessCountByEvent["UserPromptSubmit"],
			// Deprecated: heimdall_contribution.cache_hits is a back-compat
			// alias for the top-level user_prompt_cache_hits. Both read from
			// the same SessionHookAggregate.UserPromptCacheHits counter (the
			// count of hooks.log user-prompt events with stage=cache_hit for
			// the session). New consumers should read user_prompt_cache_hits
			// alongside user_prompt_cache_total so they have both numerator
			// and denominator for the hit-ratio. This alias predates PR #43
			// (shipped in PR #28) and is retained to avoid breaking existing
			// downstream readers without a schema_version bump. Safe to drop
			// on a future v2 schema revision.
			"cache_hits":     r.Hooks.UserPromptCacheHits,
			"skips":          r.Hooks.UserPromptSkips,
			"bytes_injected": r.Hooks.UserPromptBytesInjected,
		},
		"guardrails": map[string]interface{}{
			"events":   r.Hooks.PreToolUseEvents,
			"verdicts": r.Hooks.GuardrailVerdicts,
		},
		"reindexes": map[string]interface{}{
			"events":     r.Hooks.PostEditEvents,
			"reindex_ok": r.Hooks.PostEditReindexes,
		},
		// Canonical cache-hit counters: numerator + denominator at the top
		// level so consumers can compute the cache-hit ratio without
		// reaching into heimdall_contribution. user_prompt_cache_hits is
		// the count of hooks.log user-prompt events with stage=cache_hit;
		// user_prompt_cache_total is stage=ok + stage=cache_hit (the
		// "reached the cache layer" denominator). Degraded stages
		// (ollama_ping, embed errors, verify_hook_index, ...) and
		// stage=skip (prompt_too_short) are deliberately excluded from
		// both — the cache layer never observed them.
		//
		// Note: user_prompt_cache_hits has the same numeric value as the
		// deprecated heimdall_contribution.cache_hits alias above. The
		// alias is kept for back-compat only. Schema stays v1 (additive).
		"user_prompt_cache_hits":  r.Hooks.UserPromptCacheHits,
		"user_prompt_cache_total": r.Hooks.UserPromptCacheTotal,
		"transcript_error":        r.TranscriptError,
	}
}
