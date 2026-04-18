package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
		fmt.Fprintln(stdout, "                [--verbose] (include semantic_drift per-turn rows)")
		fmt.Fprintln(stdout, "                [--scope-aware] (reserved; OQ-5)")
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
	fs := flag.NewFlagSet("sessions report", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		sessionID  string
		format     string
		cwd        string
		home       string
		current    bool
		verbose    bool
		scopeAware bool
	)
	fs.StringVar(&sessionID, "session-id", "", "target Claude Code session id")
	fs.BoolVar(&current, "current", false, "auto-pick the most-recent session from hooks.log")
	fs.StringVar(&format, "format", "text", "output format: text|json")
	fs.StringVar(&cwd, "cwd", "", "project cwd for transcript lookup (default: os.Getwd)")
	fs.StringVar(&home, "home", "", "override home dir for transcript lookup (debug/tests)")
	// --verbose (OQ-10): expose per-turn semantic_drift rows alongside the
	// headline counts. Off by default to keep the report readable.
	fs.BoolVar(&verbose, "verbose", false, "include semantic_drift per-turn rows (OQ-10)")
	// --scope-aware (OQ-5): when set, the missed-bucket replay filters the
	// store by the hook-time scope instead of full-index. Not wired in
	// Stage 2 (requires per-turn scope from the hook log which F hasn't
	// added); accepted but ignored for now. Keeps the CLI forward-
	// compatible with the v2 expansion.
	fs.BoolVar(&scopeAware, "scope-aware", false, "filter missed-bucket replay by hook scope (OQ-5; not yet wired)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	_ = scopeAware // reserved for v2.x; §OQ-5
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
		Verbose:    verbose,
	}
	if transcriptErr != nil {
		report.TranscriptError = transcriptErr.Error()
	}

	// Semantic-drift v2 compute (plan 12 Stage 2). Fail-open: any F1-F9
	// path yields report.SemanticDrift=nil + a non-empty Diag, which the
	// renderers serialize as `semantic_drift: null` plus a diagnostic
	// field. Never aborts `sessions report`.
	driftRep, driftDiag := computeSemanticDriftForReport(cfg, cwd, tsum, entries, verbose)
	report.SemanticDrift = driftRep
	report.SemanticDriftDiag = driftDiag

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

// computeSemanticDriftForReport is the sessions-report-side glue: opens
// the project's vector store, constructs an Ollama-backed embedder, and
// calls heimdall.ComputeSemanticDrift. Every failure maps to a diagnostic
// string + nil report (fail-open, plan 12 §7). We never mutate the caller.
//
// The store + embedder are created here rather than injected because
// `sessions report` is a cold post-hoc command — latency isn't a concern
// and we want the happy path to Just Work without extra plumbing. Tests
// that exercise the compute directly bypass this helper and call
// ComputeSemanticDrift with fakes.
func computeSemanticDriftForReport(cfg config.Config, cwd string, tsum heimdall.TranscriptSummary, entries []heimdall.HookLogEntry, verbose bool) (*heimdall.SemanticDriftReport, string) {
	// Fast-exit: no transcript signal at all. Mirrors F8 which already
	// bails in the compute path, but we skip the store open entirely
	// so a missing store doesn't produce a misleading diagnostic.
	if tsum.UserMessages == 0 {
		return nil, "no_transcript"
	}

	baseDir := filepath.Join(cwd, ".heimdall_db")
	ctx, cancel := context.WithTimeout(context.Background(), heimdall.SemanticDriftEmbedBatchTimeout+5*time.Second)
	defer cancel()

	client := heimdall.NewOllamaClient(cfg.OllamaEndpoint)
	dbDir, model := heimdall.ResolveUsableModelDB(ctx, client, baseDir, cfg.Model)
	if dbDir == "" || model == "" {
		return nil, "no_usable_index"
	}

	store, err := heimdall.OpenStore(dbDir)
	if err != nil {
		return nil, "store_open_error"
	}
	defer store.Close()

	embedder := heimdall.NewOllamaEmbedder(client, model)

	rep, diag, cerr := heimdall.ComputeSemanticDrift(ctx, heimdall.ComputeSemanticDriftInputs{
		Transcript:     tsum,
		HookEntries:    entries,
		Store:          store,
		Embedder:       embedder,
		EmbeddingModel: model,
		IncludePerTurn: verbose,
	})
	if cerr != nil {
		return nil, "invariant_broken"
	}
	return rep, diag
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

	// SemanticDrift holds the v2 semantic-drift metric (plan 12 §6). Nil
	// when the compute path degrades (any F1-F9); SemanticDriftDiag carries
	// the reason ("ollama_unreachable", "no_hook_data", etc.). Additive
	// field — JSON schema_version stays "v1" per plan 12 §6.3.
	SemanticDrift     *heimdall.SemanticDriftReport
	SemanticDriftDiag string

	// Verbose mirrors the --verbose CLI flag. When true, the renderers
	// emit the per-turn semantic_drift rows (OQ-10). The compute layer
	// already gates PerTurn on a separate IncludePerTurn bool — this
	// field governs whether the renderers surface what's there.
	Verbose bool
}

func transcriptDuration(t heimdall.TranscriptSummary) time.Duration {
	if t.FirstTimestamp.IsZero() || t.LastTimestamp.IsZero() {
		return 0
	}
	return t.LastTimestamp.Sub(t.FirstTimestamp)
}

// renderSemanticDriftText emits the 2-line headline for the semantic_drift
// block (plan 12 §6.2). When r.Verbose is true, per-turn rows follow
// (OQ-10). On failure (r.SemanticDrift==nil), shows "n/a (<diag>)".
func renderSemanticDriftText(w io.Writer, r SessionReport) {
	if r.SemanticDrift == nil {
		diag := r.SemanticDriftDiag
		if diag == "" {
			diag = "unknown"
		}
		fmt.Fprintf(w, "\n  semantic_drift: n/a (%s)\n", diag)
		return
	}
	sd := r.SemanticDrift
	fmt.Fprintf(w, "\n  semantic_drift: redundant=%d missed=%d (T1=%.2f T2=%.2f, near: t1=%d t2=%d)\n",
		sd.SemanticRedundantCalls, sd.MissedCallOpportunities,
		sd.ThresholdT1, sd.ThresholdT2,
		sd.NearThresholdRedundant, sd.NearThresholdMissed)
	if sd.HeimdallNonSearchWhenHitsPresent > 0 {
		fmt.Fprintf(w, "  heimdall_non_search_when_hits_present=%d\n",
			sd.HeimdallNonSearchWhenHitsPresent)
	}
	if r.Verbose && len(sd.PerTurn) > 0 {
		fmt.Fprintf(w, "  per_turn:\n")
		for _, row := range sd.PerTurn {
			fmt.Fprintf(w, "    turn=%d verdict=%s best_sim=%.3f query=%q\n",
				row.TurnIdx, row.Verdict, row.BestSim, row.Query)
		}
	}
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
	renderSemanticDriftText(w, r)
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
	toolUse := map[string]interface{}{
		"total":                    r.Transcript.ToolUseCount,
		"heimdall":                 r.Transcript.HeimdallToolCalls,
		"redundant_heimdall_calls": r.Transcript.RedundantHeimdallCalls,
		"by_name":                  r.Transcript.ToolUseByName,
	}
	// Nested semantic_drift block (plan 12 §6.1). Always present under
	// tool_use; the compute path emits `null` on failure so consumers
	// can distinguish "we tried and couldn't" from "the CLI predates
	// the feature". A sibling semantic_drift_error carries the diag.
	toolUse["semantic_drift"] = semanticDriftToJSON(r.SemanticDrift)
	if r.SemanticDrift == nil && r.SemanticDriftDiag != "" {
		toolUse["semantic_drift_error"] = r.SemanticDriftDiag
	}

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
		"tool_use": toolUse,
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

// semanticDriftToJSON renders the SemanticDriftReport as a map with stable
// key order. Go's json encoder sorts map keys alphabetically by default,
// so building a plain map is sufficient for deterministic output — we do
// NOT need to hand-roll JSON.
//
// Returns nil when sd is nil, which serializes to `null` under the
// `tool_use.semantic_drift` key (plan 12 §6.1 fail-open path).
func semanticDriftToJSON(sd *heimdall.SemanticDriftReport) interface{} {
	if sd == nil {
		return nil
	}
	block := map[string]interface{}{
		"semantic_redundant_calls":             sd.SemanticRedundantCalls,
		"missed_call_opportunities":            sd.MissedCallOpportunities,
		"near_threshold_redundant":             sd.NearThresholdRedundant,
		"near_threshold_missed":                sd.NearThresholdMissed,
		"turns_total":                          sd.TurnsTotal,
		"turns_with_hits":                      sd.TurnsWithHits,
		"turns_skipped":                        sd.TurnsSkipped,
		"turns_skipped_by_reason":              sd.TurnsSkippedBy,
		"heimdall_non_search_when_hits_present": sd.HeimdallNonSearchWhenHitsPresent,
		"threshold_t1":                         sd.ThresholdT1,
		"threshold_t2":                         sd.ThresholdT2,
		"threshold_version":                    sd.ThresholdVersion,
		"embedding_model":                      sd.EmbeddingModel,
		"near_threshold_delta":                 heimdall.NearThresholdDelta,
	}
	// Per-turn rows only when --verbose was set. Absent entirely (not
	// empty array) when verbose is off, keeping the headline JSON small.
	if len(sd.PerTurn) > 0 {
		rows := make([]map[string]interface{}, 0, len(sd.PerTurn))
		for _, row := range sd.PerTurn {
			rows = append(rows, map[string]interface{}{
				"turn_idx": row.TurnIdx,
				"verdict":  row.Verdict,
				"best_sim": row.BestSim,
				"query":    row.Query,
			})
		}
		block["per_turn"] = rows
	}
	return block
}
