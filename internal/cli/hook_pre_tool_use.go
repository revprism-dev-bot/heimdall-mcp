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
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// llmUnreachableSuppressWindow rate-limits WARN llm.classifier.unreachable
// events so a down Ollama doesn't spam the hook log on every Bash call.
// Mirrors plan 04's Tier-B suppressor (see internal/heimdall/suppress.go).
const llmUnreachableSuppressWindow = 10 * time.Minute

// llmNoModelLogged ensures INFO llm.classifier.no_model_configured is
// emitted at most once per process (plan 11 §6 F6). Package-level so the
// singleton survives repeat hook invocations in the same long-running
// process (e.g. integration tests); hook fires in shell-driven production
// are one-shot per process, so this is effectively a no-op there.
var (
	llmNoModelLogged   bool
	llmNoModelLoggedMu sync.Mutex
)

// consumeNoModelLog reports whether this is the first no-model-configured
// event of the process and marks it as seen. Drop-in replacement for the
// sync.Once shape that supports reset-in-tests via resetLLMNoModelLogForTest.
func consumeNoModelLog() bool {
	llmNoModelLoggedMu.Lock()
	defer llmNoModelLoggedMu.Unlock()
	if llmNoModelLogged {
		return false
	}
	llmNoModelLogged = true
	return true
}

// resetLLMNoModelLogForTest clears the once-per-process no-model gate so
// tests can assert independently. Test-only.
func resetLLMNoModelLogForTest() {
	llmNoModelLoggedMu.Lock()
	llmNoModelLogged = false
	llmNoModelLoggedMu.Unlock()
}

// llmFirstCallOnce emits a one-shot log annotation on the very first
// classify call of the process so 11a §6 risk #2 is greppable: a timeout
// on the first call after idle is expected, not a broken install. Tests
// reset this via resetLLMFirstCallForTest.
var (
	llmFirstCallDone bool
	llmFirstCallMu   sync.Mutex
)

func consumeFirstCall() bool {
	llmFirstCallMu.Lock()
	defer llmFirstCallMu.Unlock()
	if llmFirstCallDone {
		return false
	}
	llmFirstCallDone = true
	return true
}

// resetLLMFirstCallForTest resets the first-call marker so tests can
// assert first_call=true behavior deterministically.
func resetLLMFirstCallForTest() {
	llmFirstCallMu.Lock()
	llmFirstCallDone = false
	llmFirstCallMu.Unlock()
}

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
	// cfg now carries LLMClassifierModel, consulted downstream when the
	// env-var opt-in is set and deps.LLM is non-nil. With the env var
	// unset (the default), cfg is unused and the original static-only
	// code path is bit-for-bit unchanged — verified by
	// TestHookPreToolUse_LLM_DefaultOff_ZeroBehaviorChange.

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

	// --- LLM fallback (plan 11 §2.2 / §6; 11a §5.3) ------------------------
	//
	// Opt-in, default off. Consulted ONLY on ClassUnknown — the static rules
	// always win for Allow/Warn/Block (plan 11 §2.3). The branch is a pure
	// overlay: it can UPGRADE Unknown → allow/warn/block, but every failure
	// mode (F1-F7) collapses BACK to Unknown so the non-LLM code path below
	// is the only exit-code source of truth.
	if class == ClassUnknownAlias && deps.LLM != nil && env["HEIMDALL_LLM_CLASSIFIER"] == "1" {
		if cfg.LLMClassifierModel == "" {
			// F6 — toggle on, config missing. INFO once per process, skip.
			if consumeNoModelLog() {
				logHookEventWithSession("INFO", "pre-tool-use", evt.SessionID, map[string]any{
					"stage": "llm.classifier.no_model_configured",
				})
			}
		} else {
			llmClass, llmReason, llmRuleID := runLLMFallback(
				deps.LLM, cfg.LLMClassifierModel, cmd, env, evt,
			)
			if llmRuleID != "" {
				// LLM succeeded — overwrite the verdict. ruleID follows the
				// `llm:<model>` convention from plan 11 §5.3 so users who
				// see a stderr block know the verdict came from the
				// probabilistic layer and can retry with
				// HEIMDALL_LLM_CLASSIFIER=0 to bypass it.
				class, reason, ruleID = llmClass, llmReason, llmRuleID
			}
			// On failure: class stays ClassUnknown, reason/ruleID stay
			// empty. The runLLMFallback helper already logged the per-
			// failure-mode event (F1-F7). The mode switch below handles
			// Unknown as allow-equivalent in every mode — OQ-5 preserved.
		}
	}

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
	//
	// ClassUnknown is treated identically to ClassAllow for exit-code
	// purposes in EVERY mode (including block). This is the plan 11 §2.1
	// "no user-visible change in default deployments" contract: the only
	// hook that may exit 2 is mode=block AND class=ClassBlock (OQ-5 in
	// docs/plans/hooks/06-decisions.md). Unknown never exits 2, never
	// writes stdout, never writes stderr — it's logged as `class=unknown`
	// so the audit tooling can count it separately from real Allow hits.

	switch mode {
	case guardrailShadow:
		// Never emit stdout/stderr, never block. Just log (done above).
		return 0

	case guardrailWarn:
		// Warn + block both emit the `## Heimdall guardrail` block on stdout.
		// Hook still exits 0 — this mode is "preview what block mode would
		// do" without actually stopping tools. Allow and Unknown fall
		// through silently.
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
		// ClassAllow + ClassUnknown both fall here — exit 0, silent. OQ-5
		// compliance: a class=unknown fall-through must NEVER exit 2, even
		// in block mode.
		return 0
	}
	return 0
}

// HookPreToolUseDeps lets tests inject a stub classifier. A zero-value deps
// uses heimdall.ClassifyBashCommand for static classification and leaves
// the LLM fallback off (LLM==nil → nil-is-off invariant, plan 11 §10.3).
type HookPreToolUseDeps struct {
	// Classify is the static classifier. nil → heimdall.ClassifyBashCommand.
	Classify func(cmd string) (heimdall.Classification, string, string)

	// LLM is the optional LLM fallback consulted when Classify returns
	// ClassUnknown AND HEIMDALL_LLM_CLASSIFIER=1 AND cfg.LLMClassifierModel
	// is set. Zero value (nil) means "LLM off" — the static-only code path
	// is preserved exactly as today. Plan 11 §10.3 / 11a §5.1 item 5.
	LLM heimdall.LLMClassifier
}

// runLLMFallback invokes deps.LLM.ClassifyBash with a bounded deadline and
// emits one of the plan 11 §6 F1-F7 log events on success or failure.
// Returns (class, reason, ruleID) on success, where ruleID follows the
// `llm:<model>` convention from plan 11 §5.3. On any failure, returns
// (ClassUnknown, "", "") so the caller keeps its existing Unknown-
// handling surface (exit 0 in every mode).
//
// The 1500ms default is overridable via HEIMDALL_LLM_CLASSIFIER_TIMEOUT_MS,
// capped at 2000ms so the env var cannot push above the hook-handler budget
// (plan 11 §4.3).
func runLLMFallback(llm heimdall.LLMClassifier, model, cmd string, env map[string]string, evt preToolUseEvent) (heimdall.Classification, string, string) {
	timeout := llmTimeoutFromEnv(env)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	firstCall := consumeFirstCall()

	// Panic guard — plan 11 §6 F7. Even a defensive panic in the
	// classifier shouldn't bring down the hook.
	var (
		llmClass  heimdall.Classification
		llmReason string
		llmErr    error
	)
	func() {
		defer func() {
			if r := recover(); r != nil {
				llmErr = fmt.Errorf("panic: %v", r)
				logHookEventWithSession("ERROR", "pre-tool-use", evt.SessionID, map[string]any{
					"stage":          "llm.classifier.panic",
					"model":          model,
					"prompt_version": heimdall.LLMClassifierPromptVersion,
					"err":            fmt.Sprintf("%v", r),
				})
			}
		}()
		started := time.Now()
		llmClass, llmReason, llmErr = llm.ClassifyBash(ctx, cmd)
		elapsed := time.Since(started)

		if llmErr == nil {
			// Success — log INFO llm.classifier.classify.
			reason := llmReason
			truncated := false
			if r, didTrunc := heimdall.TruncateReason(reason); didTrunc {
				reason = r
				truncated = true
			}
			// F4 — reason_truncated WARN (separate event so it's countable).
			if truncated {
				logHookEventWithSession("WARN", "pre-tool-use", evt.SessionID, map[string]any{
					"stage":          "llm.classifier.reason_truncated",
					"model":          model,
					"prompt_version": heimdall.LLMClassifierPromptVersion,
				})
			}
			fields := map[string]any{
				"stage":          "llm.classifier.classify",
				"elapsed_ms":     int(elapsed / time.Millisecond),
				"model":          model,
				"prompt_version": heimdall.LLMClassifierPromptVersion,
				"class":          llmClass.String(),
				"reason_len":     len(reason),
			}
			if firstCall {
				fields["first_call"] = true
			}
			logHookEventWithSession("INFO", "pre-tool-use", evt.SessionID, fields)
			// Mutate the returned reason so the caller sees the truncated shape.
			llmReason = reason
		}
	}()

	if llmErr == nil {
		return llmClass, llmReason, fmt.Sprintf("llm:%s", model)
	}

	// Failure — bucket into F1 (timeout), F2 (unreachable), F3 (bad
	// response), or generic. F7 (panic) already logged inside the recover.
	stage := llmFailureStage(llmErr)

	// F2 unreachable gets rate-limited via plan 04's Tier-B suppressor so
	// a down Ollama doesn't spam the log on every Bash call. We gate on a
	// (project_root, "llm-classifier-unreachable") key.
	if stage == "llm.classifier.unreachable" {
		projectRoot := evt.CWD
		if !heimdall.ShouldEmitTierB(projectRoot, "llm-classifier-unreachable", llmUnreachableSuppressWindow) {
			return heimdall.ClassUnknown, "", ""
		}
	}

	fields := map[string]any{
		"stage":          stage,
		"model":          model,
		"prompt_version": heimdall.LLMClassifierPromptVersion,
		"err":            llmErr.Error(),
	}
	if firstCall {
		fields["first_call"] = true
	}
	level := "WARN"
	if stage == "llm.classifier.panic" {
		// Already logged inside the recover; don't double-emit.
		return heimdall.ClassUnknown, "", ""
	}
	logHookEventWithSession(level, "pre-tool-use", evt.SessionID, fields)

	return heimdall.ClassUnknown, "", ""
}

// llmFailureStage buckets an LLM-classifier error into one of the plan 11
// §6 F1/F2/F3 stage names. F7 (panic) is handled separately via recover.
func llmFailureStage(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "llm.classifier.timeout"
	}
	if errors.Is(err, heimdall.ErrLLMBadResponse) {
		return "llm.classifier.bad_response"
	}
	// Connection-refused, DNS fail, etc. surface as wrapped errors from
	// OllamaClient.Chat. A substring match is cheaper than unwrapping
	// net.OpError chains and is stable across Go versions.
	msg := err.Error()
	if strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "dial tcp") ||
		strings.Contains(msg, "EOF") {
		return "llm.classifier.unreachable"
	}
	// Non-200 HTTP status falls here too. Log as bad_response; the raw
	// error carries the status code for ops triage.
	return "llm.classifier.bad_response"
}

// llmTimeoutFromEnv resolves HEIMDALL_LLM_CLASSIFIER_TIMEOUT_MS with a
// default of 1500ms and a 2000ms cap (plan 11 §4.3). Unparseable or
// negative values fall back to the default.
func llmTimeoutFromEnv(env map[string]string) time.Duration {
	const (
		def = 1500 * time.Millisecond
		cap = 2000 * time.Millisecond
	)
	raw := env["HEIMDALL_LLM_CLASSIFIER_TIMEOUT_MS"]
	if raw == "" {
		return def
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms <= 0 {
		return def
	}
	d := time.Duration(ms) * time.Millisecond
	if d > cap {
		return cap
	}
	return d
}

// Typed aliases — they compile down to the same values as
// heimdall.ClassAllow/Warn/Block/Unknown, but keep the handler file from
// needing a bare reference to every token from the heimdall package.
const (
	ClassAllowAlias   = heimdall.ClassAllow
	ClassWarnAlias    = heimdall.ClassWarn
	ClassBlockAlias   = heimdall.ClassBlock
	ClassUnknownAlias = heimdall.ClassUnknown
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
