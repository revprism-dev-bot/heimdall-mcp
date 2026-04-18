package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// HooksAuditSchemaVersion is the JSON schema version emitted by
// `hooks audit-guardrails --format=json`. Bump on breaking field changes
// only (rename, removal, type change). Additive changes are safe.
const HooksAuditSchemaVersion = "v1"

// promotionMinWindow is the minimum audit window required before the
// recommendation can be anything other than "true (window too short)".
// See the rollout section of docs/plans/hooks/08-destructive-op-primitive.md.
const promotionMinWindow = 24 * time.Hour

// topRulesCap bounds the "top rules by block count" section to keep the
// text report scannable on a terminal.
const topRulesCap = 10

// fpCandidatesCap caps the "false positive candidates" list. Operators who
// need the full dataset can re-run with --format=json or grep hooks.log
// directly — the report is meant to be skimmable, not exhaustive.
const fpCandidatesCap = 50

// auditFlags bundles the parsed CLI flags for `hooks audit-guardrails`.
type auditFlags struct {
	since    time.Duration
	hasSince bool
	format   string
}

// auditSummary is the in-memory shape both the text and JSON renderers
// consume. Produced by aggregateAudit from a filtered slice of hooks.log
// entries. Exported fields only — JSON marshalling needs them.
type auditSummary struct {
	SchemaVersion          string         `json:"schema_version"`
	WindowFrom             string         `json:"window_from,omitempty"`
	WindowTo               string         `json:"window_to"`
	TotalEvents            int            `json:"total_events"`
	CountsByClass          map[string]int `json:"counts_by_class"`
	CountsByMode           map[string]int `json:"counts_by_mode"`
	TopRules               []ruleStat     `json:"top_rules_by_block"`
	FalsePositiveCandidate []fpCandidate  `json:"false_positive_candidates"`
	// LLMVerdicts buckets the class a classify event landed on when it
	// came from the LLM fallback (rule_id starts with `llm:`). Plan 11a
	// §5.4 item 9: ops need the allow/warn/block split on LLM-handled
	// unknowns to evaluate stage-2 promotion criteria (FP rate, §8.2).
	LLMVerdicts map[string]int `json:"llm_verdicts,omitempty"`
	// LLMPromptVersions buckets the `prompt_version=N` field emitted on
	// llm.classifier.classify events. Plan 11a §OQ-5 — lets operators
	// split telemetry cleanly across prompt edits.
	LLMPromptVersions map[string]int `json:"llm_prompt_versions,omitempty"`
	PromoteToWarn     bool           `json:"promote_to_warn"`
	PromotionReason   string         `json:"promotion_reason"`
	Notes             []string       `json:"notes,omitempty"`
}

// ruleStat captures the per-rule aggregate used for the top-offenders
// ranking. SampleReason is the most recent reason string seen for this
// rule; it's informational only — the rule_id is the stable key.
type ruleStat struct {
	RuleID       string `json:"rule_id"`
	BlockCount   int    `json:"block_count"`
	SampleReason string `json:"sample_reason,omitempty"`
}

// fpCandidate represents a single `class=block mode=shadow` verdict — the
// exact dataset operators need to review before promoting the rollout
// knob. Timestamp is RFC3339 UTC; RuleID and Reason carry whatever the
// producer logged (may be empty / "-" for classifications without a rule).
type fpCandidate struct {
	Timestamp string `json:"timestamp"`
	RuleID    string `json:"rule_id,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Session   string `json:"session,omitempty"`
}

// HooksAuditGuardrails implements `heimdall-mcp hooks audit-guardrails`.
//
// Produces a structured report over `event=pre-tool-use` entries in
// hooks.log, aimed at operators deciding whether to promote the default
// `HEIMDALL_GUARDRAILS=shadow` to `warn` (and later `block`).
//
// Flags:
//   - --since=<Go duration>  Restrict the window to [now-duration, now]. Default: the full log.
//   - --format=text|json     Output format. Default: text.
//
// Exit codes:
//   - 0 — normal (including empty log / no events — the report still
//     renders so downstream scripts always see a parseable payload).
//   - 1 — hooks.log exists but is unreadable.
//   - 2 — bad flag or argument.
func HooksAuditGuardrails(stdin io.Reader, stdout, stderr io.Writer, env map[string]string, args []string) int {
	_ = stdin
	_ = env

	flags, err := parseAuditFlags(args)
	if err != nil {
		fmt.Fprintf(stderr, "hooks audit-guardrails: %v\n", err)
		return 2
	}

	now := time.Now().UTC()
	opts := heimdall.ReadHookLogOpts{Event: "pre-tool-use"}
	var windowFrom time.Time
	if flags.hasSince {
		windowFrom = now.Add(-flags.since)
		opts.Since = windowFrom
	}

	entries, err := heimdall.ReadHookLog(opts)
	if err != nil {
		// ReadHookLog already returns (nil, nil) on a missing path — any
		// error here means the file exists but can't be read (permissions,
		// unexpected I/O failure). Surface as exit 1, same as `hooks tail`.
		fmt.Fprintln(stderr, "hooks audit-guardrails: log unavailable")
		return 1
	}

	summary := aggregateAudit(entries, windowFrom, now, flags.hasSince)

	switch flags.format {
	case "json":
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(summary)
	default:
		renderAuditText(stdout, summary)
	}
	return 0
}

// parseAuditFlags is split out to keep the handler short and to make the
// flag surface trivially unit-testable without spinning up a fake log.
func parseAuditFlags(args []string) (auditFlags, error) {
	f := auditFlags{format: "text"}
	// Normalize `--flag=value` into `--flag value` so the stdlib flag
	// package (which only honors `=` for some forms) matches the rest of
	// the hooks CLI surface (hooks tail, sessions list, etc.).
	args = splitEqualsFlags(args)

	fs := flag.NewFlagSet("hooks audit-guardrails", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var sinceRaw, format string
	fs.StringVar(&sinceRaw, "since", "", "restrict the audit window (e.g. 24h, 7d)")
	fs.StringVar(&format, "format", "text", "output format: text|json")
	if err := fs.Parse(args); err != nil {
		return f, err
	}
	if n := fs.NArg(); n > 0 {
		return f, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	switch format {
	case "text", "json":
		f.format = format
	default:
		return f, fmt.Errorf("invalid --format %q (want text|json)", format)
	}

	if sinceRaw != "" {
		d, err := parseTailDuration(sinceRaw)
		if err != nil {
			return f, fmt.Errorf("invalid --since %q: %w", sinceRaw, err)
		}
		if d < 0 {
			return f, fmt.Errorf("invalid --since %q: must be non-negative", sinceRaw)
		}
		f.since = d
		f.hasSince = true
	}

	return f, nil
}

// knownClasses enumerates every classifier bucket that `hooks
// audit-guardrails` surfaces as a first-class count. Keeping these
// seeded (at zero) guarantees the text report and the JSON payload are
// symmetric across windows — a window with no `unknown` verdicts still
// renders `unknown=0` so operators don't have to wonder whether the key
// is missing because nothing matched or because the audit tool predates
// ClassUnknown. See plan 11 §2.1 / plan 08 §8 (rollout): Unknown is the
// LLM-fallback extension point and needs to be observable before
// promotion, not after.
var knownClasses = []string{"allow", "warn", "block", "unknown"}

// aggregateAudit folds pre-filtered `event=pre-tool-use` entries into the
// summary shape. Callers must pre-filter by event to keep this function
// event-agnostic (and trivially testable with canned inputs).
func aggregateAudit(entries []heimdall.HookLogEntry, windowFrom, windowTo time.Time, hasWindow bool) auditSummary {
	s := auditSummary{
		SchemaVersion:          HooksAuditSchemaVersion,
		WindowTo:               windowTo.Format(time.RFC3339),
		CountsByClass:          map[string]int{},
		CountsByMode:           map[string]int{},
		TopRules:               []ruleStat{},
		FalsePositiveCandidate: []fpCandidate{},
	}
	// Seed every known class at zero so the JSON payload always
	// includes the `unknown` key (and every other bucket) even on a
	// fresh log. Additive change — schema_version stays v1.
	for _, k := range knownClasses {
		s.CountsByClass[k] = 0
	}
	if hasWindow {
		s.WindowFrom = windowFrom.Format(time.RFC3339)
	}

	// Per-rule block aggregates. Captured ordered so the JSON output is
	// deterministic (equal block counts → alphabetical rule id).
	type ruleAgg struct {
		count        int
		sampleReason string
	}
	ruleCounts := map[string]*ruleAgg{}

	for _, e := range entries {
		// Defensive filter — callers should have already narrowed to
		// pre-tool-use, but a mixed log (tests, future events) shouldn't
		// corrupt the counts.
		if e.Event != "pre-tool-use" {
			continue
		}
		// Separate LLM-telemetry bucket: `stage=llm.classifier.classify`
		// carries a `prompt_version` field we surface for audit splits
		// across prompt edits (plan 11a §OQ-5). These rows do NOT count
		// toward CountsByClass because the classify-stage row already
		// logged the final verdict — we'd double-count.
		stage := e.Fields["stage"]
		if stage == "llm.classifier.classify" {
			if pv := e.Fields["prompt_version"]; pv != "" {
				if s.LLMPromptVersions == nil {
					s.LLMPromptVersions = map[string]int{}
				}
				s.LLMPromptVersions[pv]++
			}
			continue
		}
		class := e.Fields["class"]
		mode := e.Fields["mode"]
		// Skip bookkeeping lines that weren't a classify stage (e.g.
		// stage=skip, stage=panic, stage=flag_parse). A classify stage
		// always emits a non-empty class.
		if class == "" {
			continue
		}
		s.TotalEvents++
		s.CountsByClass[class]++
		if mode != "" {
			s.CountsByMode[mode]++
		}

		// LLM-verdict bucket: the hook handler sets rule_id=llm:<model>
		// after an LLM upgrade. Count the resulting class here so ops can
		// evaluate FP rate on LLM-produced blocks / warns.
		if ruleID := e.Fields["rule_id"]; strings.HasPrefix(ruleID, "llm:") {
			if s.LLMVerdicts == nil {
				s.LLMVerdicts = map[string]int{}
			}
			s.LLMVerdicts[class]++
		}

		if class == "block" {
			ruleID := e.Fields["rule_id"]
			if ruleID == "" {
				ruleID = "-"
			}
			a := ruleCounts[ruleID]
			if a == nil {
				a = &ruleAgg{}
				ruleCounts[ruleID] = a
			}
			a.count++
			if a.sampleReason == "" {
				a.sampleReason = e.Fields["reason"]
			}

			// Shadow-mode blocks are the actually-interesting dataset:
			// these are what a reviewer must eyeball before promoting.
			if mode == "shadow" {
				if len(s.FalsePositiveCandidate) < fpCandidatesCap {
					s.FalsePositiveCandidate = append(s.FalsePositiveCandidate, fpCandidate{
						Timestamp: e.Timestamp.UTC().Format(time.RFC3339),
						RuleID:    ruleID,
						Reason:    e.Fields["reason"],
						Session:   e.Session,
					})
				}
			}
		}
	}

	// Rank rules: block count DESC, rule_id ASC as a deterministic tiebreaker.
	ruleIDs := make([]string, 0, len(ruleCounts))
	for id := range ruleCounts {
		ruleIDs = append(ruleIDs, id)
	}
	sort.Slice(ruleIDs, func(i, j int) bool {
		a, b := ruleCounts[ruleIDs[i]], ruleCounts[ruleIDs[j]]
		if a.count != b.count {
			return a.count > b.count
		}
		return ruleIDs[i] < ruleIDs[j]
	})
	for _, id := range ruleIDs {
		if len(s.TopRules) >= topRulesCap {
			break
		}
		s.TopRules = append(s.TopRules, ruleStat{
			RuleID:       id,
			BlockCount:   ruleCounts[id].count,
			SampleReason: ruleCounts[id].sampleReason,
		})
	}

	// Promotion recommendation. The rule (see §8 of the destructive-op
	// design doc):
	//
	//   - No events at all → recommend promotion (nothing to prove).
	//   - Block count == 0 within the window → recommend promotion.
	//   - Block count > 0:
	//       - Window < 24h (or unbounded with a very tight stretch of
	//         data, treated here as "full log has <24h of events")
	//         → still recommend promotion, but annotate "window too short".
	//       - Otherwise → recommend AGAINST promotion and call out the
	//         top offending rule.
	//
	// The "unbounded" branch is rare in practice (you would always pass
	// --since=168h per the handoff doc), but covering it keeps the CLI
	// friendly when a user forgets the flag.
	blockCount := s.CountsByClass["block"]
	switch {
	case s.TotalEvents == 0:
		s.PromoteToWarn = true
		s.PromotionReason = "no pre-tool-use events in window"
		s.Notes = append(s.Notes, "hooks.log has no pre-tool-use verdicts yet; run Claude Code once the hook is installed to collect telemetry")
	case blockCount == 0:
		s.PromoteToWarn = true
		s.PromotionReason = "zero block verdicts in window"
	default:
		// Determine effective window size. When --since is unset, the
		// window spans the earliest → latest entry.
		effWindow := effectiveWindow(entries, windowFrom, windowTo, hasWindow)
		if effWindow < promotionMinWindow {
			s.PromoteToWarn = true
			s.PromotionReason = fmt.Sprintf("window too short (%s < %s)", effWindow.Round(time.Second), promotionMinWindow)
			s.Notes = append(s.Notes, "window covered less than one day of telemetry; rerun once ≥24h of real traffic has accumulated")
		} else {
			top := "-"
			if len(s.TopRules) > 0 {
				top = s.TopRules[0].RuleID
			}
			s.PromoteToWarn = false
			s.PromotionReason = fmt.Sprintf("%d block verdict(s) within window; top offender: %s", blockCount, top)
		}
	}

	return s
}

// effectiveWindow computes the interval the summary actually covers. When
// the caller passed --since, we trust that explicit window. Otherwise the
// window is the gap between the earliest and latest entry we saw; an
// empty or single-entry log collapses to zero.
func effectiveWindow(entries []heimdall.HookLogEntry, windowFrom, windowTo time.Time, hasWindow bool) time.Duration {
	if hasWindow {
		return windowTo.Sub(windowFrom)
	}
	if len(entries) == 0 {
		return 0
	}
	first := entries[0].Timestamp
	last := entries[0].Timestamp
	for _, e := range entries[1:] {
		if e.Timestamp.Before(first) {
			first = e.Timestamp
		}
		if e.Timestamp.After(last) {
			last = e.Timestamp
		}
	}
	return last.Sub(first)
}

// renderAuditText writes the text-format report to w. Deterministic order
// — callers (including tests) can assert against it directly.
func renderAuditText(w io.Writer, s auditSummary) {
	fmt.Fprintln(w, "# PreToolUse guardrail audit")
	if s.WindowFrom != "" {
		fmt.Fprintf(w, "window_from=%s\n", s.WindowFrom)
	} else {
		fmt.Fprintln(w, "window_from=<full log>")
	}
	fmt.Fprintf(w, "window_to=%s\n", s.WindowTo)
	fmt.Fprintf(w, "total_events=%d\n", s.TotalEvents)

	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "## Counts by class")
	// knownClasses includes `unknown` — the "no rule matched" bucket
	// introduced by plan 11 §2.1. Unknown is NOT a promotion blocker
	// (see the recommendation logic above) and NEVER appears in the FP
	// candidate list, but it IS reported here so reviewers can see how
	// much traffic is hitting the future LLM-fallback extension point.
	for _, k := range sortedKeysWithDefaults(s.CountsByClass, knownClasses) {
		fmt.Fprintf(w, "  %s=%d\n", k, s.CountsByClass[k])
	}

	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "## Counts by mode")
	for _, k := range sortedKeysWithDefaults(s.CountsByMode, []string{"shadow", "warn", "block", "off"}) {
		fmt.Fprintf(w, "  %s=%d\n", k, s.CountsByMode[k])
	}

	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "## Top rules by block verdict")
	if len(s.TopRules) == 0 {
		fmt.Fprintln(w, "  (none)")
	} else {
		for _, r := range s.TopRules {
			reason := r.SampleReason
			if reason == "" {
				reason = "-"
			}
			fmt.Fprintf(w, "  %-32s count=%d  sample_reason=%s\n", r.RuleID, r.BlockCount, reason)
		}
	}

	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "## False-positive candidates (class=block, mode=shadow)")
	if len(s.FalsePositiveCandidate) == 0 {
		fmt.Fprintln(w, "  (none — zero shadow-mode blocks in window)")
	} else {
		for _, fp := range s.FalsePositiveCandidate {
			rule := fp.RuleID
			if rule == "" {
				rule = "-"
			}
			reason := fp.Reason
			if reason == "" {
				reason = "-"
			}
			fmt.Fprintf(w, "  %s  rule=%-28s reason=%s\n", fp.Timestamp, rule, reason)
		}
	}

	// LLM-fallback verdict buckets (plan 11a §5.4 item 9). Only rendered
	// when the log actually contains LLM-upgraded entries so operators
	// with the fallback off see the same report they got before.
	if len(s.LLMVerdicts) > 0 || len(s.LLMPromptVersions) > 0 {
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "## LLM fallback (plan 11)")
		if len(s.LLMVerdicts) > 0 {
			fmt.Fprintln(w, "verdicts_by_class:")
			for _, k := range sortedKeysWithDefaults(s.LLMVerdicts, []string{"allow", "warn", "block"}) {
				if s.LLMVerdicts[k] == 0 {
					continue
				}
				fmt.Fprintf(w, "  %s=%d\n", k, s.LLMVerdicts[k])
			}
		}
		if len(s.LLMPromptVersions) > 0 {
			fmt.Fprintln(w, "by_prompt_version:")
			keys := make([]string, 0, len(s.LLMPromptVersions))
			for k := range s.LLMPromptVersions {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(w, "  v%s=%d\n", k, s.LLMPromptVersions[k])
			}
		}
	}

	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "## Promotion recommendation")
	fmt.Fprintf(w, "promote_to_warn=%t\n", s.PromoteToWarn)
	if s.PromotionReason != "" {
		fmt.Fprintf(w, "reason=%s\n", s.PromotionReason)
	}
	for _, n := range s.Notes {
		fmt.Fprintf(w, "note=%s\n", n)
	}
}

// sortedKeysWithDefaults returns the union of the map keys and the listed
// defaults in alphabetical order. Callers render `allow=0 warn=0 block=0`
// via `m[k]` — a key missing from the map naturally reads as zero because
// Go returns the zero value for absent map keys. This function does NOT
// mutate the input map.
func sortedKeysWithDefaults(m map[string]int, defaults []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(m)+len(defaults))
	for _, d := range defaults {
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	for k := range m {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Compile-time guard: the admin handler conforms to the HookHandler shape
// used by every other hooks-CLI entry. If someone changes the shape, the
// compiler catches it here before runtime.
var _ HookHandler = HooksAuditGuardrails
