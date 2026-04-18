// Package heimdall — destructive_ops.go implements the Phase 3 guardrail
// classifier primitive. Given a raw Bash command string, it returns one of
// four classifications (allow / warn / block / unknown) plus a stable rule
// ID and a short human-readable reason.
//
// Design reference: docs/plans/hooks/08-destructive-op-primitive.md and
// the ClassUnknown prerequisite in
// docs/plans/hooks/11-llm-classification-fallback.md §2.1.
//
// Contract:
//   - Deterministic, pure function (no I/O, no RNG).
//   - Allowlist precedence: any allow rule short-circuits to ClassAllow.
//   - Block precedence: among deny rules, block wins over warn.
//   - p99 ≤ 1 ms on typical command strings (≤256 bytes). All regex is
//     compiled at init.
//   - Default classification when NOTHING matches any rule is ClassUnknown
//     (distinct from ClassAllow, which is reserved for commands that hit
//     an explicit allow-list rule). Callers that need today's "allow by
//     default" behavior collapse ClassUnknown to ClassAllow at their own
//     surface — see the hook handler in internal/cli/hook_pre_tool_use.go.
//
// This is a paper-cut protector, NOT a security boundary. A motivated human
// can trivially bypass any string match (rename binary, pipe through bash -c,
// use xargs). We do not pretend otherwise — §1.2 of the design doc is
// explicit.
package heimdall

import (
	"context"
	"regexp"
	"strings"
)

// Classification is a four-state label returned by ClassifyBashCommand.
type Classification int

const (
	// ClassAllow means the command was explicitly matched by an allowlist
	// rule (e.g. `git push --force-with-lease`). The PreToolUse hook lets
	// Claude Code run it without interference, and the match is treated
	// as authoritative — the LLM fallback (plan 11) never runs on it.
	ClassAllow Classification = iota

	// ClassWarn means the command is recoverable-destructive (e.g.
	// `rm -rf node_modules`). The hook logs it and, in warn/block mode,
	// writes a `## Heimdall guardrail` block to stdout. It never blocks.
	ClassWarn

	// ClassBlock means the command is catastrophically destructive
	// (e.g. `rm -rf /`, force-push to main). In block mode the hook exits
	// 2 with a one-line stderr reason so Claude Code cancels the tool call.
	ClassBlock

	// ClassUnknown means no rule matched — neither allowlist nor warn nor
	// block. This is the default fall-through and is deliberately distinct
	// from ClassAllow so callers can tell "I recognized this as safe" from
	// "no rule fired, presumed safe."
	//
	// Callers that want today's "allow by default" behavior collapse this
	// to ClassAllow at their own surface (see the hook handler in
	// internal/cli/hook_pre_tool_use.go — every mode treats ClassUnknown
	// identically to ClassAllow for exit-code purposes, and never exits 2
	// on Unknown regardless of mode). The extension point documented in
	// docs/plans/hooks/11-llm-classification-fallback.md consults an LLM
	// fallback if and only if the static classifier returned ClassUnknown.
	ClassUnknown
)

// String renders a Classification as the lowercase token used in log lines
// and `hooks explain-command` output.
func (c Classification) String() string {
	switch c {
	case ClassAllow:
		return "allow"
	case ClassWarn:
		return "warn"
	case ClassBlock:
		return "block"
	case ClassUnknown:
		return "unknown"
	default:
		return "unknown"
	}
}

// destructiveRule is one entry in the starter ruleset. Kept package-private
// because the external API is ClassifyBashCommand + Classification; exposing
// the rule table directly would lock us in to a particular shape.
type destructiveRule struct {
	id      string
	class   Classification
	pattern *regexp.Regexp
	reason  string
}

// destructiveRules is the starter ruleset (v1). Order does not matter within
// each class — precedence is allow > block > warn, enforced by
// ClassifyBashCommand. The table is bound at package init via compiled
// regex.
//
// IDs are stable: they appear in logs, tests, and the explain-command CLI.
// Changing an ID is a breaking change.
var destructiveRules = []destructiveRule{
	// -------------------------------------------------------------------
	// ALLOW rules — matched first, short-circuit to ClassAllow.
	// -------------------------------------------------------------------
	{
		id:      "GIT_PUSH_FORCE_WITH_LEASE",
		class:   ClassAllow,
		pattern: regexp.MustCompile(`(?i)^git\s+push\s+(?:.*\s+)?--force-with-lease\b`),
		reason:  "--force-with-lease is the safer form; explicitly allowed.",
	},

	// -------------------------------------------------------------------
	// BLOCK rules — catastrophic, irreversible operations.
	// -------------------------------------------------------------------
	{
		id:      "RM_RF_ROOT",
		class:   ClassBlock,
		pattern: regexp.MustCompile(`^rm\s+(?:-[a-zA-Z]*[rR][a-zA-Z]*[fF][a-zA-Z]*|-[a-zA-Z]*[fF][a-zA-Z]*[rR][a-zA-Z]*)\s+/+\s*$`),
		reason:  "rm -rf / would wipe the root filesystem.",
	},
	{
		id:      "RM_RF_STAR_ROOT",
		class:   ClassBlock,
		pattern: regexp.MustCompile(`^rm\s+(?:-[a-zA-Z]*[rR][a-zA-Z]*[fF][a-zA-Z]*|-[a-zA-Z]*[fF][a-zA-Z]*[rR][a-zA-Z]*)\s+/\*`),
		reason:  "rm -rf /* would wipe the root filesystem.",
	},
	{
		id:      "RM_RF_HOME",
		class:   ClassBlock,
		pattern: regexp.MustCompile(`^rm\s+(?:-[a-zA-Z]*[rR][a-zA-Z]*[fF][a-zA-Z]*|-[a-zA-Z]*[fF][a-zA-Z]*[rR][a-zA-Z]*)\s+(?:\$HOME|~)/?\s*$`),
		reason:  "rm -rf $HOME would wipe the user home directory.",
	},
	{
		id:      "GIT_PUSH_FORCE_PROTECTED",
		class:   ClassBlock,
		pattern: regexp.MustCompile(`(?i)^git\s+push\s+(?:.*\s+)?(?:--force|-f)\b.*\b(?:main|master|prod|production|release/\S+)\b`),
		reason:  "Force-push to a protected branch rewrites shared history.",
	},
	{
		id:      "GIT_RESET_HARD_PROTECTED",
		class:   ClassBlock,
		pattern: regexp.MustCompile(`(?i)^git\s+reset\s+--hard\s+(?:origin/)?(?:main|master|release/\S+|prod|production)\b`),
		reason:  "Hard reset against a protected branch rewrites history irreversibly.",
	},
	{
		id:      "DROP_DATABASE",
		class:   ClassBlock,
		pattern: regexp.MustCompile(`(?i)\bdrop\s+database\b`),
		reason:  "DROP DATABASE is irreversible.",
	},
	{
		id:      "DD_OF_DEV",
		class:   ClassBlock,
		pattern: regexp.MustCompile(`^(?:sudo\s+)?dd\b.*\bof=/dev/(?:sd|nvme|hd|vd|mmcblk|xvd)`),
		reason:  "dd to a raw block device is irreversible and usually wrong.",
	},
	{
		id:      "MKFS_ANY",
		class:   ClassBlock,
		pattern: regexp.MustCompile(`^(?:sudo\s+)?mkfs(?:\.[a-zA-Z0-9]+)?\s+/dev/`),
		reason:  "mkfs against a real device destroys the filesystem.",
	},
	{
		id:      "KUBECTL_DELETE_PROD",
		class:   ClassBlock,
		pattern: regexp.MustCompile(`^kubectl\s+delete\b.*(?:--namespace(?:=|\s+)prod(?:uction)?\b|-n\s+prod(?:uction)?\b|--context(?:=|\s+)\S*prod\S*)`),
		reason:  "Direct prod mutation via kubectl delete is out-of-process.",
	},

	// -------------------------------------------------------------------
	// WARN rules — recoverable or context-dependent.
	// -------------------------------------------------------------------
	{
		id:      "RM_RF_DOT_GIT",
		class:   ClassWarn,
		pattern: regexp.MustCompile(`^rm\s+(?:-[a-zA-Z]*[rR][a-zA-Z]*[fF][a-zA-Z]*|-[a-zA-Z]*[fF][a-zA-Z]*[rR][a-zA-Z]*)\s+\.git\b`),
		reason:  "Removing .git/ discards the entire repo history.",
	},
	{
		id:      "GIT_PUSH_FORCE_GENERIC",
		class:   ClassWarn,
		pattern: regexp.MustCompile(`(?i)^git\s+push\s+(?:.*\s+)?(?:--force|-f)\b`),
		reason:  "Force-push rewrites history on the remote.",
	},
	{
		id:      "GIT_RESET_HARD_GENERIC",
		class:   ClassWarn,
		pattern: regexp.MustCompile(`(?i)^git\s+reset\s+--hard\b`),
		reason:  "Hard reset discards uncommitted changes.",
	},
	{
		id:      "GIT_CLEAN_FDX",
		class:   ClassWarn,
		pattern: regexp.MustCompile(`(?i)^git\s+clean\s+-[a-zA-Z]*[fdx][a-zA-Z]*`),
		reason:  "git clean -fdx removes ignored files (often .env).",
	},
	{
		id:      "GIT_BRANCH_FORCE_DELETE",
		class:   ClassWarn,
		pattern: regexp.MustCompile(`(?i)^git\s+branch\s+-D\b`),
		reason:  "Force-deleting an unmerged branch loses unique commits.",
	},
	{
		id:      "KUBECTL_DELETE_GENERIC",
		class:   ClassWarn,
		pattern: regexp.MustCompile(`^kubectl\s+delete\b`),
		reason:  "Cluster-state mutation; confirm namespace/context.",
	},
	{
		id:      "CHMOD_777_RECURSIVE",
		class:   ClassWarn,
		pattern: regexp.MustCompile(`^chmod\s+-R\s+0?7{3}\b`),
		reason:  "Recursive chmod 777 is almost never what you want.",
	},
	{
		id:      "CURL_PIPE_BASH",
		class:   ClassWarn,
		pattern: regexp.MustCompile(`^(?:curl|wget)\b[^|]*\|\s*(?:sudo\s+)?(?:bash|sh|zsh)\b`),
		reason:  "Piping remote scripts to a shell is a common compromise vector.",
	},
	{
		id:      "RM_RF_RECURSIVE_GENERIC",
		class:   ClassWarn,
		pattern: regexp.MustCompile(`^rm\s+(?:-[a-zA-Z]*[rR][a-zA-Z]*[fF][a-zA-Z]*|-[a-zA-Z]*[fF][a-zA-Z]*[rR][a-zA-Z]*)\s+\S`),
		reason:  "Recursive deletion. Confirm path before running.",
	},
}

// strippablePrefix is a short whitelist of command prefixes we peel off
// before pattern matching. Everything on this list preserves the "real"
// command's argv semantics, so `time rm -rf /` still matches RM_RF_ROOT.
// Keep this intentionally tight — stripping arbitrary commands would open a
// bypass surface.
var strippablePrefixes = []string{
	"time ",
	"nice ",
	"ionice ",
	"nohup ",
	"exec ",
}

// normalizeCommand trims whitespace, collapses runs of horizontal whitespace
// to a single space, and strips a small whitelist of transparent prefixes
// (time/nice/etc.). It is the first step of classification — the rules are
// written against the normalized form.
//
// Explicit non-goals (design §3.5):
//   - DO NOT expand `$HOME`, `~`, globs, or subshells.
//   - DO NOT parse `bash -c "..."` payloads.
//   - DO NOT lowercase (classification rules use case-insensitive regex
//     where needed; blind lowercasing would mangle paths and args).
func normalizeCommand(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return ""
	}

	// Collapse runs of tabs/spaces to a single space. Avoid regex so this
	// stays cheap on the hot path.
	var b strings.Builder
	b.Grow(len(cmd))
	prevSpace := false
	for _, r := range cmd {
		if r == ' ' || r == '\t' {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	out := b.String()

	// Strip an `env VAR=val ...` prefix (any number of KEY=VAL pairs). This
	// lets `env FOO=bar rm -rf /` still match RM_RF_ROOT. We do NOT strip
	// arbitrary commands — only `env` with its characteristic KEY=VALUE
	// pattern.
	if strings.HasPrefix(out, "env ") {
		rest := strings.TrimPrefix(out, "env ")
		// Peel KEY=VAL tokens until we hit something without `=`.
		for {
			sp := strings.IndexByte(rest, ' ')
			if sp < 0 {
				break
			}
			tok := rest[:sp]
			if !isEnvAssignment(tok) {
				break
			}
			rest = strings.TrimLeft(rest[sp+1:], " ")
		}
		out = rest
	}

	// Strip whitelisted transparent prefixes, iteratively. Someone writing
	// `time nice rm -rf /` should still resolve to just `rm -rf /`.
	for {
		stripped := false
		for _, p := range strippablePrefixes {
			if strings.HasPrefix(out, p) {
				out = strings.TrimLeft(out[len(p):], " ")
				stripped = true
				break
			}
		}
		if !stripped {
			break
		}
	}
	return out
}

// isEnvAssignment reports whether tok looks like an `env VAR=value` pair
// (an identifier followed by `=` and anything). Used by normalizeCommand
// to walk past the `env` prefix.
func isEnvAssignment(tok string) bool {
	if tok == "" {
		return false
	}
	eq := strings.IndexByte(tok, '=')
	if eq <= 0 {
		return false
	}
	// Require the left-hand side to look like a shell identifier:
	// [A-Za-z_][A-Za-z0-9_]*
	name := tok[:eq]
	for i, r := range name {
		switch {
		case r == '_':
			// ok
		case r >= 'A' && r <= 'Z':
			// ok
		case r >= 'a' && r <= 'z':
			// ok
		case i > 0 && r >= '0' && r <= '9':
			// ok (digits allowed after first char)
		default:
			return false
		}
	}
	return true
}

// ClassifyBashCommand classifies a proposed Bash command as
// allow/warn/block/unknown.
//
// Precedence:
//  1. Allow rules win unconditionally (e.g., `git push --force-with-lease`
//     beats `--force`).
//  2. Block rules beat warn rules.
//  3. First match within a class wins (the table is authored so rules do
//     not overlap ambiguously, but when they do we take the earlier
//     definition).
//  4. Default when nothing matched = ClassUnknown with empty rule/reason.
//     This is deliberately distinct from ClassAllow so callers can tell
//     "I recognized this as safe" from "no rule fired." Today's
//     allow-by-default behavior is preserved at the hook-handler surface
//     (see internal/cli/hook_pre_tool_use.go — ClassUnknown is collapsed
//     to ClassAllow for exit-code purposes in every mode, including block
//     mode). The ClassUnknown value is the hook that a future LLM
//     fallback hangs off of (plan 11 §2).
//
// Returns: (class, reason, ruleID). ruleID and reason are the empty string
// on the default unknown fall-through, and also on the empty-command early
// return.
//
// This function is pure and allocation-light: it does a string trim, a
// single normalization pass, and a fixed-size regex sweep. It never touches
// disk, the network, or the clock.
//
// Refactor note (plan 11 §7.4 / 11a §5 item 13): this function delegates
// to classifyStatic. The determinism guard in destructive_ops_test.go
// lives on classifyStatic so that, once a Classifier with an LLM fallback
// is wired up at the hook-handler layer, the pure static-layer contract
// remains directly testable without stubbing the LLM out.
func ClassifyBashCommand(cmd string) (Classification, string, string) {
	return classifyStatic(cmd)
}

// classifyStatic is the pure, deterministic static-rule layer. No I/O,
// no goroutines, no randomness — safe to repeat-call and assert against.
// Callers that want the LLM overlay construct a Classifier and call
// Classify; callers that want today's static-only behavior call
// ClassifyBashCommand which delegates here.
func classifyStatic(cmd string) (Classification, string, string) {
	norm := normalizeCommand(cmd)
	if norm == "" {
		// An empty / whitespace-only command isn't really a command at
		// all. Report it as Unknown so the default fall-through is a
		// single well-defined state; the hook handler short-circuits on
		// an empty `tool_input.command` before reaching us anyway.
		return ClassUnknown, "", ""
	}

	// Pass 1: allow rules. Any match short-circuits with ClassAllow.
	for _, r := range destructiveRules {
		if r.class != ClassAllow {
			continue
		}
		if r.pattern.MatchString(norm) {
			return ClassAllow, r.reason, r.id
		}
	}

	// Pass 2: block rules.
	for _, r := range destructiveRules {
		if r.class != ClassBlock {
			continue
		}
		if r.pattern.MatchString(norm) {
			return ClassBlock, r.reason, r.id
		}
	}

	// Pass 3: warn rules.
	for _, r := range destructiveRules {
		if r.class != ClassWarn {
			continue
		}
		if r.pattern.MatchString(norm) {
			return ClassWarn, r.reason, r.id
		}
	}

	// Nothing matched: not recognized — neither safe nor unsafe.
	return ClassUnknown, "", ""
}

// -----------------------------------------------------------------------
// Classifier — static rules + optional LLM fallback overlay.
// -----------------------------------------------------------------------

// Classifier bundles the static rule table with an optional LLM fallback
// consulted ONLY when the static layer returns ClassUnknown. The zero
// value (LLM==nil) is static-only and matches today's behavior exactly —
// so a `Classifier{}` is a drop-in for code that previously called
// ClassifyBashCommand.
//
// Wiring for the PreToolUse hook happens in internal/cli/hook_pre_tool_use.go;
// the hook-handler layer owns (a) the env-var gate (HEIMDALL_LLM_CLASSIFIER),
// (b) the timeout context, and (c) the per-error WARN/ERROR log shapes.
// The Classifier itself is intentionally small: it runs the static layer
// and, if Unknown, forwards to LLM.ClassifyBash.
type Classifier struct {
	// LLM is the optional second-pass classifier. nil → static-only.
	LLM LLMClassifier
}

// Classify runs the static rules and, on ClassUnknown with a non-nil LLM,
// forwards to the LLM fallback. Returns (class, reason, ruleID) in the
// same shape as ClassifyBashCommand.
//
// On LLM error (timeout, unreachable, bad response), returns the
// underlying ClassUnknown with empty reason/ruleID — the caller is
// expected to fail open at its own surface (the hook handler collapses
// Unknown to exit-0 allow per plan 11 §2.1) and log the error.
//
// The caller MUST pass a ctx with a deadline when c.LLM is non-nil. We do
// NOT apply a default timeout here; the hook handler wraps ctx with
// context.WithTimeout so the timeout value is observable in one place
// (HEIMDALL_LLM_CLASSIFIER_TIMEOUT_MS) and capped at the handler budget.
//
// This method returns the LLM error as a 4th return value so the caller
// can discriminate success from ClassUnknown-fail-open — we do that via a
// package-private helper rather than changing the 3-tuple public shape
// (see ClassifyWithErr below).
func (c *Classifier) Classify(ctx context.Context, cmd string) (Classification, string, string) {
	class, reason, ruleID, _ := c.ClassifyWithErr(ctx, cmd)
	return class, reason, ruleID
}

// ClassifyWithErr is the error-exposing variant of Classify. The hook
// handler uses this so it can log per-failure-mode events (F1 timeout,
// F2 unreachable, F3 bad response) distinct from the ClassUnknown
// fall-through. On success err is nil; on LLM failure err is non-nil and
// the returned class is ClassUnknown (the static verdict that triggered
// the call) so fail-open semantics are preserved.
func (c *Classifier) ClassifyWithErr(ctx context.Context, cmd string) (Classification, string, string, error) {
	class, reason, ruleID := classifyStatic(cmd)
	if class != ClassUnknown || c == nil || c.LLM == nil {
		return class, reason, ruleID, nil
	}

	llmClass, llmReason, err := c.LLM.ClassifyBash(ctx, cmd)
	if err != nil {
		// Fail open: return the original Unknown so the caller's
		// existing Unknown-handling surface (exit 0 in every mode)
		// stays load-bearing for the error path.
		return ClassUnknown, "", "", err
	}
	return llmClass, llmReason, "", nil
}

// DestructiveRuleCount returns the number of rules currently shipped in the
// binary. Used by tests and the explain-command --rules listing to stay in
// sync with the rule table without leaking the table layout.
func DestructiveRuleCount() int {
	return len(destructiveRules)
}

// DestructiveRules returns a copy of the starter ruleset metadata for
// listing/reporting. Each entry exposes (id, class, reason). The pattern is
// intentionally omitted — callers should not try to duplicate the match
// logic outside this package.
type DestructiveRuleSummary struct {
	ID     string
	Class  Classification
	Reason string
}

// ListDestructiveRules returns a read-only summary of every rule. Order
// matches the internal table (allow rules first, then block, then warn) so
// explain-command --rules can render a stable listing.
func ListDestructiveRules() []DestructiveRuleSummary {
	out := make([]DestructiveRuleSummary, 0, len(destructiveRules))
	for _, r := range destructiveRules {
		out = append(out, DestructiveRuleSummary{
			ID:     r.id,
			Class:  r.class,
			Reason: r.reason,
		})
	}
	return out
}
