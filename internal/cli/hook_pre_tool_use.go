// hook_pre_tool_use.go — Phase 3 `PreToolUse` guardrail hook handler.
//
// Design: docs/plans/hooks/08-destructive-op-primitive.md.
//
// This is the ONLY heimdall hook that may exit non-zero (exit 2) or write
// to stderr, and it only does so in `block` mode on a `block` classification.
// All other modes (shadow/warn/off) return exit 0 with empty stderr,
// matching the OQ-5 retrieval-hook contract surface.
package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// guardrailMode is the three-state rollout knob driven by HEIMDALL_GUARDRAILS.
// See design §8 "Rollout plan".
type guardrailMode int

const (
	// guardrailShadow (default) classifies silently into the hook log but
	// never emits stdout or blocks the tool. Intended for the first
	// release to collect telemetry on which rules fire in the wild.
	guardrailShadow guardrailMode = iota

	// guardrailWarn emits the `## Heimdall guardrail` block on stdout for
	// warn+block classes, but still exits 0 — no tool is blocked.
	guardrailWarn

	// guardrailBlock is the fully-enforced mode. Block classifications
	// exit 2 with stderr reason; warn emits on stdout.
	guardrailBlock
)

// preToolUseEvent is the slice of the Claude Code PreToolUse payload we
// actually consume. Unknown fields are ignored.
type preToolUseEvent struct {
	SessionID string `json:"session_id"`
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		Command string `json:"command"`
	} `json:"tool_input"`
	CWD string `json:"cwd"`
}

// HookPreToolUse is the Claude Code `PreToolUse` hook entry point.
//
// Stdin: PreToolUse event JSON. Only `tool_name`, `tool_input.command`, and
// `cwd` are consumed.
//
// Contract:
//   - `tool_name != "Bash"` → exit 0 silently (we only classify Bash).
//   - HEIMDALL_HOOKS=0 or `.heimdall/hooks.disabled` → exit 0, no log.
//   - HEIMDALL_GUARDRAILS:
//   - "off"               → exit 0 immediately, skip classifier entirely.
//   - "" or "shadow"      → classify, log only, exit 0.
//   - "warn"              → classify + log; warn/block → `## Heimdall guardrail` on stdout; exit 0.
//   - "block" or "1"      → classify + log; warn → stdout; block → exit 2 + stderr reason.
//   - Internal errors (malformed JSON, classifier panic) → exit 0 fail-open.
//
// Exit codes:
//   - 0 always EXCEPT when mode=block and class=block (then 2).
func HookPreToolUse(cfg config.Config, stdin io.Reader, stdout, stderr io.Writer, env map[string]string, args []string, deps HookPreToolUseDeps) int {
	_ = cfg

	// Panic guard: design §5 F3 — a regex panic must collapse to allow/exit 0.
	// Extra defense even though the compiled regexes should never panic.
	defer func() {
		if r := recover(); r != nil {
			heimdall.LogHookEvent("ERROR", "pre-tool-use", map[string]any{
				"stage": "panic",
				"err":   fmt.Sprintf("%v", r),
			})
		}
	}()

	// --- flag parsing -------------------------------------------------------

	fs := flag.NewFlagSet("hook pre-tool-use", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		sourceFlag  string
		versionFlag int
	)
	fs.StringVar(&sourceFlag, "source", "", "install marker fallback (ignored)")
	fs.IntVar(&versionFlag, "version", 0, "install marker fallback (ignored)")
	if err := fs.Parse(args); err != nil {
		heimdall.LogHookEvent("ERROR", "pre-tool-use", map[string]any{
			"stage": "flag_parse",
			"err":   err.Error(),
		})
		return 0
	}
	_ = sourceFlag
	_ = versionFlag

	// --- fast-path disable checks ------------------------------------------

	mode := parseGuardrailMode(env["HEIMDALL_GUARDRAILS"])

	// "off" — hard short-circuit with NO classifier work. The design explicitly
	// wants this fast so users can disable guardrails at zero cost.
	if isOffValue(env["HEIMDALL_GUARDRAILS"]) {
		return 0
	}

	// HEIMDALL_HOOKS=0 kills all hooks including this one (design §5 F4).
	if env != nil {
		if v, ok := env["HEIMDALL_HOOKS"]; ok && v == "0" {
			return 0
		}
	}

	// --- read event payload -------------------------------------------------

	classifier := deps.Classify
	if classifier == nil {
		classifier = heimdall.ClassifyBashCommand
	}

	// Read the event JSON. Size-bound to keep a pathological stdin from
	// eating memory.
	var data []byte
	if stdin != nil {
		limited := io.LimitReader(stdin, 256*1024)
		data, _ = io.ReadAll(limited)
	}
	if len(data) == 0 {
		// No payload means we can't classify anything. Fail open.
		heimdall.LogHookEvent("INFO", "pre-tool-use", map[string]any{
			"stage":  "skip",
			"reason": "empty_stdin",
		})
		return 0
	}

	var evt preToolUseEvent
	if err := json.Unmarshal(data, &evt); err != nil {
		// Malformed JSON → fail-open (design §5 F2). Log and exit 0.
		logHookEventWithSession("WARN", "pre-tool-use", evt.SessionID, map[string]any{
			"stage": "bad_stdin",
			"err":   err.Error(),
		})
		return 0
	}

	// Only act on Bash tool calls. For every other tool name, exit 0 silently
	// and don't even log — the PreToolUse hook fires for every tool and we
	// don't want to spam the log.
	if evt.ToolName != "Bash" {
		return 0
	}

	// Per-project marker — checked AFTER we know it's a Bash call to avoid
	// a filesystem stat on every non-Bash PreToolUse fire.
	if evt.CWD != "" && heimdall.HooksDisabled(evt.CWD, env) {
		return 0
	}

	cmd := evt.ToolInput.Command
	if cmd == "" {
		logHookEventWithSession("INFO", "pre-tool-use", evt.SessionID, map[string]any{
			"stage":  "skip",
			"reason": "empty_command",
			"mode":   modeString(mode),
		})
		return 0
	}

	// --- classify ----------------------------------------------------------

	class, reason, ruleID := classifier(cmd)

	// Always log exactly one structured event per fire (per task spec).
	logLevel := "INFO"
	if class == ClassBlockAlias {
		logLevel = "WARN"
	}
	logHookEventWithSession(logLevel, "pre-tool-use", evt.SessionID, map[string]any{
		"stage":   "classify",
		"mode":    modeString(mode),
		"class":   class.String(),
		"rule_id": ruleID,
		"reason":  reason,
	})

	// --- act on classification based on mode -------------------------------

	switch mode {
	case guardrailShadow:
		// Never emit stdout/stderr, never block. Just log (done above).
		return 0

	case guardrailWarn:
		// Warn + block both emit the `## Heimdall guardrail` block on stdout.
		// Hook still exits 0 — this mode is "preview what block mode would
		// do" without actually stopping tools.
		if class == ClassWarnAlias || class == ClassBlockAlias {
			writeGuardrailBlock(stdout, class, ruleID, reason, cmd)
		}
		return 0

	case guardrailBlock:
		switch class {
		case ClassBlockAlias:
			// THE one and only non-zero-exit / stderr code path in any
			// heimdall hook. Claude Code reads stderr on exit 2 and shows
			// it to the model as actionable feedback.
			fmt.Fprintf(stderr, "heimdall guardrail: %s (%s)\n", reason, ruleID)
			return 2
		case ClassWarnAlias:
			writeGuardrailBlock(stdout, class, ruleID, reason, cmd)
			return 0
		}
		return 0
	}
	return 0
}

// HookPreToolUseDeps lets tests inject a stub classifier. A zero-value deps
// uses heimdall.ClassifyBashCommand.
type HookPreToolUseDeps struct {
	Classify func(cmd string) (heimdall.Classification, string, string)
}

// Typed aliases — they compile down to the same values as
// heimdall.ClassAllow/Warn/Block, but keep the handler file from needing
// a bare reference to every token from the heimdall package.
const (
	ClassAllowAlias = heimdall.ClassAllow
	ClassWarnAlias  = heimdall.ClassWarn
	ClassBlockAlias = heimdall.ClassBlock
)

// writeGuardrailBlock renders the one-line `## Heimdall guardrail` block
// Claude sees in its context when warn/block classifications surface on
// stdout. Kept short: Claude only needs class, rule id, and reason.
func writeGuardrailBlock(w io.Writer, class heimdall.Classification, ruleID, reason, cmd string) {
	// Truncate the echoed command to keep the block tight. Claude already
	// has the full command in its tool-call context.
	short := cmd
	if len(short) > 120 {
		short = short[:117] + "..."
	}
	fmt.Fprintf(w,
		"## Heimdall guardrail\n\n"+
			"**Class:** %s  •  **Rule:** %s\n"+
			"**Reason:** %s\n"+
			"**Command:** `%s`\n",
		class.String(), ruleID, reason, short)
}

// parseGuardrailMode maps HEIMDALL_GUARDRAILS values onto the internal
// mode enum. Anything unrecognized falls back to shadow (the safe default).
func parseGuardrailMode(raw string) guardrailMode {
	switch raw {
	case "block", "1", "on", "enforce":
		return guardrailBlock
	case "warn":
		return guardrailWarn
	case "shadow", "":
		return guardrailShadow
	case "off", "0":
		// `off` is treated as shadow here for mode-string logging; the
		// actual short-circuit happens earlier via isOffValue. Classifying
		// won't run so this branch shouldn't be reached.
		return guardrailShadow
	default:
		// Unknown mode strings are forgiving: fall back to shadow so a
		// typo like `HEIMDALL_GUARDRAIL=block` (missing trailing S) does
		// not accidentally turn on enforcement.
		return guardrailShadow
	}
}

// isOffValue reports whether the env value should short-circuit the hook
// before doing any classification work.
func isOffValue(raw string) bool {
	return raw == "off" || raw == "0"
}

// modeString renders the mode for log payloads.
func modeString(m guardrailMode) string {
	switch m {
	case guardrailShadow:
		return "shadow"
	case guardrailWarn:
		return "warn"
	case guardrailBlock:
		return "block"
	default:
		return "unknown"
	}
}
