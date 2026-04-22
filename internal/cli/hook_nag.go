package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// nagDefaultTurns is the default value of N: after this many UserPromptSubmit
// turns without a direct heimdall_search / heimdall_recall / heimdall_remember
// / heimdall_index_text MCP call, the next turn appends a nudge to the
// auto-injected context block. Override via env HEIMDALL_NAG_TURNS.
//
// 5 was chosen over the original 10 (suggested in 00-problem-and-fixes.md §P1
// item 4) because (a) the cost of a false-positive nag is one extra line of
// context, while the cost of a missed nag is the very drift the P1 work is
// trying to suppress, and (b) 5 turns is roughly one "task chunk" — short
// enough to fire inside a single line of work, not just at task boundaries.
const nagDefaultTurns = 5

// nagDirectTools is the set of MCP tool names whose invocation resets the nag
// counter. We deliberately include only the four direct, model-driven action
// tools: passive observability tools (heimdall_status, heimdall_projects)
// don't count because the user often calls those reflexively when checking
// setup, which would mask drift on the action tools we actually care about.
var nagDirectTools = map[string]bool{
	"heimdall_search":     true,
	"heimdall_recall":     true,
	"heimdall_remember":   true,
	"heimdall_index_text": true,
}

// nagState is persisted per-session to track the per-turn nag counter.
//
// Centralized-DB caveat: today this state is stored on the local filesystem
// keyed by session id (see project_centralized_db_roadmap.md). The schema
// here intentionally avoids any path-shaped fields so a future server-side
// implementation can persist the same record under a project + session id
// without breaking on-disk compatibility.
type nagState struct {
	// Turn is the count of UserPromptSubmit invocations this session has
	// processed since the last reset (or session start).
	Turn int `json:"turn"`
	// LastNagTurn records the Turn value at which we last emitted a nag, so
	// we don't re-fire on every subsequent prompt — only every N turns.
	LastNagTurn int `json:"last_nag_turn"`
	// LastResetUnix records the wall-clock time of the most recent counter
	// reset (or session creation), in unix seconds. Used as the lower bound
	// when scanning hooks.log for direct-call resets so we never re-credit
	// an already-credited tool call.
	LastResetUnix int64 `json:"last_reset_unix"`
}

// nagStatePath returns the on-disk path for a session's nag state file.
// Co-located with the session-buffer rolling jsonl in
// .heimdall_db/hooks/sessions/ so the existing 48-hour orphan sweep
// (sweepOrphanBuffers) eventually evicts it too — no separate GC needed.
func nagStatePath(projectDir, sessionID string) string {
	return filepath.Join(projectDir, ".heimdall_db", "hooks", "sessions", sessionID+".nag.json")
}

// loadNagState reads the per-session nag state from disk. Missing or
// malformed file → returns a zero-valued state and no error: the caller
// treats first-ever load as turn 0, which is the same behavior we want when
// state is corrupted (don't keep nagging on a broken counter; reset cleanly).
func loadNagState(projectDir, sessionID string) nagState {
	if sessionID == "" {
		return nagState{}
	}
	data, err := os.ReadFile(nagStatePath(projectDir, sessionID))
	if err != nil {
		return nagState{}
	}
	var s nagState
	if json.Unmarshal(data, &s) != nil {
		return nagState{}
	}
	return s
}

// saveNagState persists nag state. Errors are logged-but-not-returned: a
// failed save just means the next turn starts from a stale counter, which
// is the same Tier-B-style "no nag" outcome we want on storage failure.
func saveNagState(projectDir, sessionID string, s nagState) {
	if sessionID == "" {
		return
	}
	dir := filepath.Join(projectDir, ".heimdall_db", "hooks", "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		heimdall.LogHookEvent("WARN", "user-prompt", map[string]any{
			"stage": "nag_save_mkdir",
			"err":   err.Error(),
		})
		return
	}
	data, err := json.Marshal(s)
	if err != nil {
		return
	}
	tmp := nagStatePath(projectDir, sessionID) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		heimdall.LogHookEvent("WARN", "user-prompt", map[string]any{
			"stage": "nag_save_write",
			"err":   err.Error(),
		})
		return
	}
	_ = os.Rename(tmp, nagStatePath(projectDir, sessionID))
}

// nagThresholdFromEnv returns the configured turn threshold, falling back to
// nagDefaultTurns. Negative or non-integer values fall back to the default
// rather than disabling the nag — to disable, set HEIMDALL_HOOKS=0 or just
// use heimdall regularly.
func nagThresholdFromEnv(env map[string]string) int {
	raw := env["HEIMDALL_NAG_TURNS"]
	if raw == "" {
		return nagDefaultTurns
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return nagDefaultTurns
	}
	return n
}

// hookLogReaderFunc abstracts ReadHookLog so tests can inject a synthetic
// log without writing to the real ~/.local/state/heimdall/hooks.log path.
type hookLogReaderFunc func(opts heimdall.ReadHookLogOpts) ([]heimdall.HookLogEntry, error)

// hadDirectHeimdallCallSince returns true if hooks.log contains at least one
// event=mcp.tool_call line for one of nagDirectTools whose timestamp is
// strictly after `since`.
//
// Cross-session caveat: today the MCP server does not thread the Claude Code
// session_id into its mcp.tool_call entries (see comment in server.go above
// mcpServerKey), so this scan attributes ALL direct calls anywhere on the
// host to the calling session's counter. On a single-developer single-machine
// setup that's effectively per-session because one Claude Code process owns
// its own MCP subprocess at a time. When session-id threading lands upstream
// (or when this moves server-side per the centralized-DB roadmap) this can
// be tightened with an extra filter.
func hadDirectHeimdallCallSince(reader hookLogReaderFunc, since time.Time) bool {
	if reader == nil {
		reader = heimdall.ReadHookLog
	}
	entries, err := reader(heimdall.ReadHookLogOpts{
		Event: "mcp.tool_call",
		Since: since,
	})
	if err != nil {
		// Treat read failure as "we don't know" — better to skip the nag
		// than to nag on a stale/corrupt log read. Same Tier-B principle.
		return false
	}
	for _, e := range entries {
		if nagDirectTools[e.Fields["tool"]] {
			return true
		}
	}
	return false
}

// nagDecision is the result of evaluating whether to emit a nag this turn.
// Returned separately from rendering so the same decision can be unit-tested
// without producing markdown.
type nagDecision struct {
	// Emit reports whether the caller should append the nag block.
	Emit bool
	// State is the post-decision state to persist (counter incremented,
	// LastNagTurn updated if Emit is true, LastResetUnix updated on reset).
	State nagState
	// Reason is a short tag for logging (turn count, "direct_call_reset",
	// "below_threshold", "fired"). Never user-visible.
	Reason string
	// TurnsSinceLast is included for the rendered nag string.
	TurnsSinceLast int
}

// evaluateNag is the pure decision function. Inputs:
//   - prev:     the previously persisted state (zero if first turn)
//   - threshold: N turns
//   - now:      injectable clock for tests
//   - hadCall:  whether a direct heimdall_* call landed since prev.LastResetUnix
//
// Behavior:
//
//  1. If hadCall is true → reset counter to 0, advance LastResetUnix to now,
//     do not emit. (User is using heimdall directly — no nudge needed.)
//  2. Else increment Turn. If (Turn - LastNagTurn) >= threshold → emit and
//     update LastNagTurn = Turn so we don't double-fire on the next prompt.
//  3. Otherwise → do nothing visible; just persist the bumped counter.
func evaluateNag(prev nagState, threshold int, now time.Time, hadCall bool) nagDecision {
	if hadCall {
		return nagDecision{
			Emit:           false,
			State:          nagState{Turn: 0, LastNagTurn: 0, LastResetUnix: now.Unix()},
			Reason:         "direct_call_reset",
			TurnsSinceLast: 0,
		}
	}
	next := prev
	if next.LastResetUnix == 0 {
		next.LastResetUnix = now.Unix()
	}
	next.Turn++
	if next.Turn-next.LastNagTurn >= threshold {
		next.LastNagTurn = next.Turn
		return nagDecision{
			Emit:           true,
			State:          next,
			Reason:         "fired",
			TurnsSinceLast: next.Turn,
		}
	}
	return nagDecision{
		Emit:           false,
		State:          next,
		Reason:         "below_threshold",
		TurnsSinceLast: next.Turn,
	}
}

// renderNagSuffix returns the markdown the hook splices into the auto-inject
// body when evaluateNag.Emit is true. Phrased as a concrete trigger→action
// reminder so the model has a clear next step rather than an abstract scold.
func renderNagSuffix(threshold int) string {
	return fmt.Sprintf(
		"\n### Heimdall nudge\n"+
			"No direct heimdall_search / heimdall_recall / heimdall_remember / heimdall_index_text calls in the last %d turns. "+
			"If you're exploring unfamiliar code, try heimdall_search first. "+
			"If you've learned something non-obvious or got corrected this turn, heimdall_remember it.\n",
		threshold,
	)
}

// spliceNagBeforeFooter inserts the nag suffix into a rendered Heimdall
// context body, just before the trailing "_retrieved via heimdall-mcp_"
// footer (matching insertSkillsSection's placement). Falls back to appending
// at end if the footer is missing.
//
// Critically: this runs AFTER cache write, so the cache layer never sees the
// nag. Each prompt re-evaluates nag state on its own session/turn counter.
func spliceNagBeforeFooter(body, suffix string) string {
	if suffix == "" {
		return body
	}
	const footer = "\n_retrieved via heimdall-mcp_\n"
	idx := strings.LastIndex(body, footer)
	if idx < 0 {
		return body + suffix
	}
	var b strings.Builder
	b.Grow(len(body) + len(suffix))
	b.WriteString(body[:idx])
	b.WriteString(suffix)
	b.WriteString(body[idx:])
	return b.String()
}

// purgeNagStateIfPresent removes the per-session nag state file. Called on
// SessionEnd so the file doesn't outlive the session. Errors other than
// "missing" are silently ignored — same Tier-B principle.
func purgeNagStateIfPresent(projectDir, sessionID string) {
	if sessionID == "" {
		return
	}
	if err := os.Remove(nagStatePath(projectDir, sessionID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		// Best-effort cleanup; don't escalate.
		_ = err
	}
}

// evaluateAndSpliceNag is the single integration point for the
// nag-after-N-turns logic in HookUserPrompt. Given a fully-rendered body
// (cache hit or fresh), it:
//
//  1. Loads per-session nag state (best-effort; missing → zero).
//  2. Scans hooks.log for any direct-tool mcp.tool_call since the last reset.
//  3. Calls evaluateNag for the decision + next state.
//  4. Persists next state (best-effort).
//  5. If the decision says emit, splices the nag block into body before the
//     "_retrieved via heimdall-mcp_" footer.
//
// Tier-B safety: only invoked from happy-path cache-hit and stage=ok branches
// in HookUserPrompt — never from the error-emitting paths (Ollama down, model
// mismatch). So a broken heimdall never produces a nag.
//
// Empty session ID short-circuits with no work and returns body unchanged
// (defensive — sessionID is always present in real Claude Code dispatches
// but absent in some doctor / dry-fire paths).
func evaluateAndSpliceNag(body, projectDir, sessionID string, env map[string]string, reader hookLogReaderFunc, nowFn func() time.Time, stageTag string) string {
	if sessionID == "" {
		return body
	}
	if env != nil && env["HEIMDALL_HOOKS"] == "0" {
		// Hooks disabled at the env level — also disable nag rendering.
		// HookUserPrompt's earlier disable gate would have already returned,
		// but defense-in-depth: never emit when the user opted out.
		return body
	}

	now := time.Now
	if nowFn != nil {
		now = nowFn
	}
	threshold := nagThresholdFromEnv(env)
	prev := loadNagState(projectDir, sessionID)
	since := time.Unix(prev.LastResetUnix, 0)
	if prev.LastResetUnix == 0 {
		// First-ever turn: no point scanning all of history.
		since = now()
	}
	hadCall := hadDirectHeimdallCallSince(reader, since)

	decision := evaluateNag(prev, threshold, now(), hadCall)
	saveNagState(projectDir, sessionID, decision.State)

	heimdall.LogHookEvent("INFO", "user-prompt", map[string]any{
		"stage":     "nag_eval",
		"sub_stage": stageTag,
		"session":   sessionID,
		"reason":    decision.Reason,
		"turn":      decision.State.Turn,
		"threshold": threshold,
		"emitted":   decision.Emit,
	})

	if !decision.Emit {
		return body
	}
	return spliceNagBeforeFooter(body, renderNagSuffix(threshold))
}
