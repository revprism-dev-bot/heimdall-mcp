package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/config"
)

// seedAuditLog writes the given lines to a temp hooks.log and points
// HEIMDALL_HOOK_LOG at it. Each line is emitted verbatim — callers are
// responsible for timestamp + key order (keys are sorted alphabetically by
// the real producer, and the reader tolerates any order, so test fixtures
// don't need to match production ordering exactly).
func seedAuditLog(t *testing.T, lines []string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.log")
	content := strings.Join(lines, "\n")
	if content != "" {
		content += "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HEIMDALL_HOOK_LOG", path)
	return path
}

func runAudit(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errBuf bytes.Buffer
	fullArgs := append([]string{"audit-guardrails"}, args...)
	code = DispatchHooks(config.Config{}, nil, &out, &errBuf, map[string]string{}, fullArgs)
	return out.String(), errBuf.String(), code
}

// ---- Behavior tests ----

func TestHooksAudit_EmptyLogCleanReport(t *testing.T) {
	seedAuditLog(t, nil)
	out, errBuf, code := runAudit(t)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errBuf)
	}
	if !strings.Contains(out, "total_events=0") {
		t.Errorf("expected total_events=0 in output, got:\n%s", out)
	}
	if !strings.Contains(out, "promote_to_warn=true") {
		t.Errorf("empty log must recommend promotion; got:\n%s", out)
	}
	if !strings.Contains(out, "no pre-tool-use events") {
		t.Errorf("empty log must note the no-events condition; got:\n%s", out)
	}
}

func TestHooksAudit_OnlyAllowVerdictsPromotes(t *testing.T) {
	// Old (>1 day) so the window-too-short condition doesn't override the
	// "zero blocks" signal.
	old := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339)
	seedAuditLog(t, []string{
		old + " INFO event=pre-tool-use class=allow mode=shadow rule_id=- session=S1 reason=-",
		old + " INFO event=pre-tool-use class=allow mode=shadow rule_id=- session=S1 reason=-",
		time.Now().UTC().Format(time.RFC3339) + " INFO event=pre-tool-use class=allow mode=shadow rule_id=- session=S2 reason=-",
	})
	out, errBuf, code := runAudit(t)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errBuf)
	}
	if !strings.Contains(out, "promote_to_warn=true") {
		t.Errorf("only-allow log must recommend promotion; got:\n%s", out)
	}
	if !strings.Contains(out, "allow=3") {
		t.Errorf("expected allow=3 in counts; got:\n%s", out)
	}
	if !strings.Contains(out, "block=0") {
		t.Errorf("expected block=0 in counts; got:\n%s", out)
	}
}

func TestHooksAudit_BlockInShadowSurfacesFalsePositive(t *testing.T) {
	// Put the bookends ≥24h apart so the effective window clears the
	// promotion-minimum threshold; otherwise the "window too short"
	// escape hatch would mask the block-count signal. Behaviour contract:
	// block count > 0 over a ≥1-day window => promote_to_warn=false.
	now := time.Now().UTC()
	first := now.Add(-72 * time.Hour).Format(time.RFC3339)
	mid := now.Add(-30 * time.Hour).Format(time.RFC3339)
	last := now.Add(-1 * time.Hour).Format(time.RFC3339)
	seedAuditLog(t, []string{
		first + " INFO event=pre-tool-use class=allow mode=shadow rule_id=- session=S1 reason=-",
		mid + " WARN event=pre-tool-use class=block mode=shadow rule_id=RM_RF_ROOT session=S1 reason=\"rm -rf at root\"",
		last + " INFO event=pre-tool-use class=allow mode=shadow rule_id=- session=S2 reason=-",
	})

	out, errBuf, code := runAudit(t)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errBuf)
	}
	if !strings.Contains(out, "promote_to_warn=false") {
		t.Errorf("a shadow-mode block MUST block promotion; got:\n%s", out)
	}
	if !strings.Contains(out, "RM_RF_ROOT") {
		t.Errorf("expected the offending rule id in the report; got:\n%s", out)
	}
	if !strings.Contains(out, "False-positive candidates") {
		t.Errorf("expected the FP candidates section; got:\n%s", out)
	}
	if !strings.Contains(out, "block=1") {
		t.Errorf("expected block=1 in counts; got:\n%s", out)
	}
	// Regression: the reader previously truncated quoted multi-word reasons
	// at the first inner space, so the top-rules and FP sections would show
	// `sample_reason="rm` instead of `rm -rf at root`. With the quote-aware
	// tokenizer the full phrase must survive.
	if !strings.Contains(out, "rm -rf at root") {
		t.Errorf("multi-word reason lost through the log round-trip; got:\n%s", out)
	}
}

func TestHooksAudit_SinceFilter(t *testing.T) {
	now := time.Now().UTC()
	oldTS := now.Add(-48 * time.Hour).Format(time.RFC3339)
	newTS := now.Add(-30 * time.Minute).Format(time.RFC3339)
	seedAuditLog(t, []string{
		oldTS + " WARN event=pre-tool-use class=block mode=shadow rule_id=RM_RF_ROOT session=S1 reason=old",
		newTS + " INFO event=pre-tool-use class=allow mode=shadow rule_id=- session=S2 reason=-",
	})

	// --since=1h keeps only the recent allow verdict. Block should not
	// appear → promote_to_warn=false must flip to true because the
	// window is (a) short (<1 day) OR (b) has zero blocks inside it.
	out, errBuf, code := runAudit(t, "--since=1h")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errBuf)
	}
	if strings.Contains(out, "RM_RF_ROOT") {
		t.Errorf("--since=1h should have excluded the 48h-old block; got:\n%s", out)
	}
	if !strings.Contains(out, "total_events=1") {
		t.Errorf("--since=1h must keep exactly 1 event; got:\n%s", out)
	}
	if !strings.Contains(out, "promote_to_warn=true") {
		t.Errorf("window with zero blocks must recommend promotion; got:\n%s", out)
	}
}

func TestHooksAudit_SinceInvalid(t *testing.T) {
	seedAuditLog(t, nil)
	_, errBuf, code := runAudit(t, "--since=not-a-duration")
	if code != 2 {
		t.Fatalf("expected rc=2 on bad --since, got %d (stderr=%s)", code, errBuf)
	}
	if !strings.Contains(errBuf, "invalid --since") {
		t.Errorf("expected 'invalid --since' in stderr, got: %q", errBuf)
	}
}

func TestHooksAudit_JSONSchemaVersion(t *testing.T) {
	now := time.Now().UTC()
	first := now.Add(-72 * time.Hour).Format(time.RFC3339)
	last := now.Add(-1 * time.Hour).Format(time.RFC3339)
	seedAuditLog(t, []string{
		first + " WARN event=pre-tool-use class=block mode=shadow rule_id=RM_RF_ROOT session=S1 reason=\"rm -rf at root\"",
		last + " INFO event=pre-tool-use class=allow mode=shadow rule_id=- session=S1 reason=-",
	})

	out, errBuf, code := runAudit(t, "--format=json")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errBuf)
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("json.Unmarshal: %v (raw output:\n%s)", err, out)
	}

	if v, ok := payload["schema_version"].(string); !ok || v != "v1" {
		t.Errorf("expected schema_version=\"v1\", got %v", payload["schema_version"])
	}

	// Promote-to-warn is explicitly false on this fixture.
	if v, ok := payload["promote_to_warn"].(bool); !ok || v {
		t.Errorf("expected promote_to_warn=false, got %v", payload["promote_to_warn"])
	}

	// Counts map must carry the block verdict.
	counts, ok := payload["counts_by_class"].(map[string]any)
	if !ok {
		t.Fatalf("expected counts_by_class map, got %T", payload["counts_by_class"])
	}
	if counts["block"] == nil {
		t.Errorf("expected counts_by_class.block present, got %v", counts)
	}
	if fp, ok := payload["false_positive_candidates"].([]any); !ok || len(fp) != 1 {
		t.Errorf("expected 1 FP candidate, got %v (raw=%s)", payload["false_positive_candidates"], out)
	}
}

func TestHooksAudit_FormatInvalid(t *testing.T) {
	seedAuditLog(t, nil)
	_, errBuf, code := runAudit(t, "--format=xml")
	if code != 2 {
		t.Fatalf("expected rc=2 on bad --format, got %d (stderr=%s)", code, errBuf)
	}
	if !strings.Contains(errBuf, "--format") {
		t.Errorf("expected --format error in stderr, got: %q", errBuf)
	}
}

func TestHooksAudit_IgnoresNonPreToolUseEvents(t *testing.T) {
	// Only non-matching events present. The handler must not count them.
	seedAuditLog(t, []string{
		"2026-04-16T20:00:00Z INFO event=user-prompt session=S1 stage=ok",
		"2026-04-16T20:00:01Z INFO event=session-start session=S1 stage=ok",
	})
	out, errBuf, code := runAudit(t)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errBuf)
	}
	if !strings.Contains(out, "total_events=0") {
		t.Errorf("non-pre-tool-use events must not be counted; got:\n%s", out)
	}
}

func TestHooksAudit_TopRulesByBlockCount(t *testing.T) {
	old := time.Now().UTC().Add(-36 * time.Hour).Format(time.RFC3339)
	lines := []string{
		old + " WARN event=pre-tool-use class=block mode=shadow rule_id=RULE_A session=S1 reason=ra",
		old + " WARN event=pre-tool-use class=block mode=shadow rule_id=RULE_A session=S1 reason=ra",
		old + " WARN event=pre-tool-use class=block mode=shadow rule_id=RULE_A session=S1 reason=ra",
		old + " WARN event=pre-tool-use class=block mode=shadow rule_id=RULE_B session=S1 reason=rb",
	}
	seedAuditLog(t, lines)
	out, errBuf, code := runAudit(t)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errBuf)
	}
	if !strings.Contains(out, "RULE_A") || !strings.Contains(out, "RULE_B") {
		t.Errorf("expected both rules in the top-rules section; got:\n%s", out)
	}
	// RULE_A must appear before RULE_B (higher block count).
	if idxA, idxB := strings.Index(out, "RULE_A"), strings.Index(out, "RULE_B"); idxA < 0 || idxB < 0 || idxA > idxB {
		t.Errorf("RULE_A should be ranked above RULE_B; got:\n%s", out)
	}
}

func TestHooksAudit_ModeCounts(t *testing.T) {
	old := time.Now().UTC().Add(-36 * time.Hour).Format(time.RFC3339)
	seedAuditLog(t, []string{
		old + " INFO event=pre-tool-use class=allow mode=shadow rule_id=- session=S1 reason=-",
		old + " INFO event=pre-tool-use class=allow mode=warn rule_id=- session=S1 reason=-",
		old + " INFO event=pre-tool-use class=allow mode=block rule_id=- session=S1 reason=-",
	})
	out, errBuf, code := runAudit(t)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errBuf)
	}
	for _, needle := range []string{"shadow=1", "warn=1", "block=1"} {
		if !strings.Contains(out, needle) {
			t.Errorf("expected mode count %q in output:\n%s", needle, out)
		}
	}
}

// TestHooksAudit_MixedClassCountsIncludeUnknown seeds a synthetic log
// with every classifier bucket (allow/warn/block/unknown) and verifies
// the audit report counts each bucket separately. Regression guard for
// the ClassUnknown prerequisite shipped in plan 11 §2.1 / PR #45 — the
// audit tool landed in PR #44 before Unknown existed, so we need
// explicit coverage that the fourth bucket is tracked end-to-end.
func TestHooksAudit_MixedClassCountsIncludeUnknown(t *testing.T) {
	// Old timestamps (≥24h ago) so the window-too-short escape hatch
	// doesn't mask the block-count signal and we can reason about the
	// promotion recommendation cleanly.
	old := time.Now().UTC().Add(-36 * time.Hour).Format(time.RFC3339)
	seedAuditLog(t, []string{
		old + " INFO event=pre-tool-use class=allow   mode=shadow rule_id=- session=S1 reason=-",
		old + " INFO event=pre-tool-use class=allow   mode=shadow rule_id=- session=S1 reason=-",
		old + " INFO event=pre-tool-use class=allow   mode=shadow rule_id=- session=S1 reason=-",
		old + " WARN event=pre-tool-use class=warn    mode=shadow rule_id=KUBECTL_DELETE_PROD session=S1 reason=warn",
		old + " INFO event=pre-tool-use class=unknown mode=shadow rule_id=- session=S1 reason=-",
		old + " INFO event=pre-tool-use class=unknown mode=shadow rule_id=- session=S1 reason=-",
	})

	out, errBuf, code := runAudit(t)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errBuf)
	}

	// Every bucket must appear in the text report — including unknown=N
	// (N > 0 here) and block=0 (seeded default, no entries).
	for _, needle := range []string{"allow=3", "warn=1", "block=0", "unknown=2"} {
		if !strings.Contains(out, needle) {
			t.Errorf("expected %q in counts-by-class; got:\n%s", needle, out)
		}
	}

	// Total should match — 3 + 1 + 0 + 2 = 6.
	if !strings.Contains(out, "total_events=6") {
		t.Errorf("expected total_events=6; got:\n%s", out)
	}

	// Unknown must NOT appear in the FP candidate list — that's
	// strictly `class=block mode=shadow` territory per the spec.
	if strings.Contains(out, "class=unknown") {
		// (The audit output doesn't echo `class=unknown` anywhere;
		// this is a belt-and-suspenders check against someone later
		// misclassifying unknown as an FP candidate.)
		t.Errorf("unknown must not leak into FP candidate lines; got:\n%s", out)
	}
}

// TestHooksAudit_UnknownDoesNotBlockPromotion pins down the semantic:
// Unknown verdicts are neutral. `promote_to_warn` still flips on
// (block_count == 0 OR window < 24h). A pile of unknowns is not a
// promotion blocker.
func TestHooksAudit_UnknownDoesNotBlockPromotion(t *testing.T) {
	// Window spans ≥24h so we're past the "window too short" escape
	// hatch — the recommendation must come from the block-count rule
	// alone (zero blocks → promote).
	now := time.Now().UTC()
	first := now.Add(-48 * time.Hour).Format(time.RFC3339)
	last := now.Add(-1 * time.Hour).Format(time.RFC3339)
	lines := []string{
		first + " INFO event=pre-tool-use class=allow   mode=shadow rule_id=- session=S1 reason=-",
		last + " INFO event=pre-tool-use class=allow   mode=shadow rule_id=- session=S1 reason=-",
	}
	// Pile on unknowns — they must not influence the recommendation.
	for i := 0; i < 50; i++ {
		lines = append(lines, first+" INFO event=pre-tool-use class=unknown mode=shadow rule_id=- session=S1 reason=-")
	}
	seedAuditLog(t, lines)

	out, errBuf, code := runAudit(t)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errBuf)
	}
	if !strings.Contains(out, "promote_to_warn=true") {
		t.Errorf("many unknowns + zero blocks must still recommend promotion; got:\n%s", out)
	}
	if !strings.Contains(out, "unknown=50") {
		t.Errorf("expected unknown=50 in counts; got:\n%s", out)
	}
	if !strings.Contains(out, "zero block verdicts") {
		t.Errorf("expected promotion reason to be the zero-blocks clause; got:\n%s", out)
	}
}

// TestHooksAudit_JSONCountsByClassIncludesUnknownKey asserts the JSON
// payload carries the `unknown` key explicitly — even on a fresh log
// with zero unknown verdicts. This guarantees downstream tooling
// (dashboards, promotion scripts) can always read
// `counts_by_class.unknown` without a presence check.
func TestHooksAudit_JSONCountsByClassIncludesUnknownKey(t *testing.T) {
	old := time.Now().UTC().Add(-36 * time.Hour).Format(time.RFC3339)
	seedAuditLog(t, []string{
		old + " INFO event=pre-tool-use class=allow mode=shadow rule_id=- session=S1 reason=-",
	})

	out, errBuf, code := runAudit(t, "--format=json")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errBuf)
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("json.Unmarshal: %v (raw output:\n%s)", err, out)
	}
	counts, ok := payload["counts_by_class"].(map[string]any)
	if !ok {
		t.Fatalf("counts_by_class not a map: %T", payload["counts_by_class"])
	}
	// All four buckets must be present — zero-valued keys included.
	for _, k := range []string{"allow", "warn", "block", "unknown"} {
		if _, present := counts[k]; !present {
			t.Errorf("counts_by_class[%q] missing from JSON payload; got keys=%v (raw=%s)", k, counts, out)
		}
	}
	// And schema_version stays v1 — additive change only.
	if v, _ := payload["schema_version"].(string); v != "v1" {
		t.Errorf("schema_version must stay v1 for additive changes; got %q", v)
	}
}

// TestHooksAudit_JSONCountsByClassUnknownValue seeds real unknown
// verdicts and verifies the JSON count matches. Companion to the
// zero-value presence test above.
func TestHooksAudit_JSONCountsByClassUnknownValue(t *testing.T) {
	now := time.Now().UTC()
	first := now.Add(-36 * time.Hour).Format(time.RFC3339)
	seedAuditLog(t, []string{
		first + " INFO event=pre-tool-use class=unknown mode=shadow rule_id=- session=S1 reason=-",
		first + " INFO event=pre-tool-use class=unknown mode=shadow rule_id=- session=S1 reason=-",
		first + " INFO event=pre-tool-use class=unknown mode=shadow rule_id=- session=S1 reason=-",
	})

	out, errBuf, code := runAudit(t, "--format=json")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errBuf)
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("json.Unmarshal: %v (raw output:\n%s)", err, out)
	}
	counts, ok := payload["counts_by_class"].(map[string]any)
	if !ok {
		t.Fatalf("counts_by_class not a map: %T", payload["counts_by_class"])
	}
	// JSON numbers decode as float64 through map[string]any.
	got, _ := counts["unknown"].(float64)
	if int(got) != 3 {
		t.Errorf("counts_by_class.unknown want 3, got %v (raw=%s)", counts["unknown"], out)
	}
	// FP candidate list must not include unknown entries even when
	// that's all the window holds.
	if fp, ok := payload["false_positive_candidates"].([]any); !ok || len(fp) != 0 {
		t.Errorf("unknown verdicts must not populate false_positive_candidates; got %v", payload["false_positive_candidates"])
	}
}

// Routing: DispatchHooks must register the new subcommand and its help text.
func TestDispatchHooks_RoutesAuditGuardrails(t *testing.T) {
	seedAuditLog(t, nil)
	out, errBuf, code := runAudit(t)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errBuf)
	}
	if !strings.Contains(out, "PreToolUse guardrail audit") {
		t.Errorf("expected audit report header in stdout; got:\n%s", out)
	}
}

func TestDispatchHooks_Help_MentionsAuditGuardrails(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := DispatchHooks(config.Config{}, nil, &out, &errBuf, nil, []string{"help"})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "audit-guardrails") {
		t.Errorf("help must list audit-guardrails; got:\n%s", out.String())
	}
}

// Plan 11a §5.4 item 9: audit-guardrails must bucket `class=unknown →
// llm:<class>` verdicts and `prompt_version=N` in separate sections.
func TestHooksAudit_LLMVerdictsAndPromptVersion(t *testing.T) {
	// Relative timestamps so the fixture doesn't bit-rot when the
	// wall-clock slides past the hard-coded date (the --since=24h
	// filter here was masking the seeded entries once UTC moved past
	// 2026-04-19).
	now := time.Now().UTC()
	fmtTS := func(offsetSec int) string {
		return now.Add(-time.Duration(offsetSec) * time.Second).Format(time.RFC3339)
	}
	seedAuditLog(t, []string{
		// Three classify rows whose rule_id is `llm:<model>`. The LLM
		// fallback upgraded ClassUnknown to allow/warn/block.
		fmtTS(5) + ` INFO event=pre-tool-use stage=classify mode=block class=allow rule_id=llm:llama3.2:3b reason=benign`,
		fmtTS(4) + ` INFO event=pre-tool-use stage=classify mode=block class=warn  rule_id=llm:llama3.2:3b reason=r`,
		fmtTS(3) + ` WARN event=pre-tool-use stage=classify mode=block class=block rule_id=llm:llama3.2:3b reason=p`,
		// Two llm.classifier.classify telemetry rows, one per prompt version.
		fmtTS(2) + ` INFO event=pre-tool-use stage=llm.classifier.classify model=llama3.2:3b prompt_version=1 elapsed_ms=120 class=allow reason_len=12`,
		fmtTS(1) + ` INFO event=pre-tool-use stage=llm.classifier.classify model=llama3.2:3b prompt_version=2 elapsed_ms=110 class=warn reason_len=14`,
	})
	out, errBuf, code := runAudit(t, "--format=json", "--since=24h")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errBuf)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, out)
	}
	verdicts, ok := payload["llm_verdicts"].(map[string]any)
	if !ok {
		t.Fatalf("expected llm_verdicts map; got %v", payload["llm_verdicts"])
	}
	want := map[string]float64{"allow": 1, "warn": 1, "block": 1}
	for k, v := range want {
		if verdicts[k] != v {
			t.Errorf("llm_verdicts[%q]=%v, want %v", k, verdicts[k], v)
		}
	}
	pv, ok := payload["llm_prompt_versions"].(map[string]any)
	if !ok {
		t.Fatalf("expected llm_prompt_versions map; got %v", payload["llm_prompt_versions"])
	}
	if pv["1"] != float64(1) || pv["2"] != float64(1) {
		t.Errorf("llm_prompt_versions split missing; got %v", pv)
	}
}
