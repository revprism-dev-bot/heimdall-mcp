// hook_pre_tool_use_llm_test.go — Layer 1 (stub) tests for the LLM
// classifier fallback. Plan 11 §7.1 table (17 tests). Each injects a
// stub LLMClassifier via HookPreToolUseDeps.LLM; no network is touched.
//
// Shared invariant: with HEIMDALL_LLM_CLASSIFIER unset or deps.LLM nil,
// the LLM stub must NEVER be called. This is the default-off contract
// (plan 11 §10.3 / 11a §5).
package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// -----------------------------------------------------------------------
// Stub LLMClassifier.
// -----------------------------------------------------------------------

type stubLLM struct {
	calls    int
	classRet heimdall.Classification
	reason   string
	err      error
	// If onCall is set, it runs before returning — useful for cancellation
	// tests that want to assert ctx is honored.
	onCall func(ctx context.Context)
}

func (s *stubLLM) ClassifyBash(ctx context.Context, cmd string) (heimdall.Classification, string, error) {
	s.calls++
	if s.onCall != nil {
		s.onCall(ctx)
	}
	return s.classRet, s.reason, s.err
}

// runLLM is the LLM-aware harness. Mirrors runHookPreToolUse from the
// base test file but takes a LLMClassifier + Config so tests can drive
// the LLM branch deterministically. Also resets the package-level
// first-call and no-model singletons so each test is independent.
func runLLM(t *testing.T, opts runPTUOpts, llm heimdall.LLMClassifier, cfg config.Config) (stdout, stderr string, code int) {
	t.Helper()
	resetLLMFirstCallForTest()
	resetLLMNoModelLogForTest()

	var out, errBuf bytes.Buffer
	deps := HookPreToolUseDeps{Classify: opts.classify, LLM: llm}
	code = HookPreToolUse(cfg, strings.NewReader(opts.stdin), &out, &errBuf, opts.env, opts.args, deps)
	return out.String(), errBuf.String(), code
}

// enabledEnv returns a ready-to-use env map with the LLM toggle on plus
// any additional entries provided via overrides. Keeps test bodies short
// and makes the default-off tests easy to spot (env that does NOT call
// enabledEnv).
func enabledEnv(guardrails string, overrides map[string]string) map[string]string {
	env := map[string]string{
		"HEIMDALL_LLM_CLASSIFIER": "1",
	}
	if guardrails != "" {
		env["HEIMDALL_GUARDRAILS"] = guardrails
	}
	for k, v := range overrides {
		env[k] = v
	}
	return env
}

// hookLogTempEnv points HEIMDALL_HOOK_LOG at a fresh tempdir file so
// log-content assertions are deterministic across parallel tests.
func hookLogTempEnv(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	p := filepath.Join(d, "hooks.log")
	t.Setenv("HEIMDALL_HOOK_LOG", p)
	return p
}

// testCfg returns a minimal config with the LLM model set so the fallback
// can run. The empty-model path has its own test.
func testCfg() config.Config {
	return config.Config{LLMClassifierModel: "llama3.2:3b"}
}

// -----------------------------------------------------------------------
// L1-1 — Regression guard on the ClassUnknown default fall-through.
// -----------------------------------------------------------------------

func TestLLM_L1_1_UnknownDefaultStatic(t *testing.T) {
	class, reason, id := heimdall.ClassifyBashCommand("make build")
	if class != heimdall.ClassUnknown {
		t.Errorf("ClassifyBashCommand(make build) = %v, want ClassUnknown", class)
	}
	if reason != "" || id != "" {
		t.Errorf("ClassUnknown must have empty reason/id, got reason=%q id=%q", reason, id)
	}
}

// -----------------------------------------------------------------------
// L1-2..L1-4 — Static beats LLM: LLM never consulted when static is
// already Allow, Warn, or Block.
// -----------------------------------------------------------------------

func TestLLM_L1_2_StaticAllowSkipsLLM(t *testing.T) {
	stub := &stubLLM{classRet: heimdall.ClassBlock, reason: "would block"}
	_, _, code := runLLM(t, runPTUOpts{
		stdin:    bashEvent("ls -la"),
		env:      enabledEnv("shadow", nil),
		classify: stubClassifier(heimdall.ClassAllow, "", ""),
	}, stub, testCfg())
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if stub.calls != 0 {
		t.Errorf("LLM must not be consulted on static Allow; calls=%d", stub.calls)
	}
}

func TestLLM_L1_3_StaticWarnSkipsLLM(t *testing.T) {
	stub := &stubLLM{classRet: heimdall.ClassAllow}
	_, _, _ = runLLM(t, runPTUOpts{
		stdin:    bashEvent("rm -rf node_modules"),
		env:      enabledEnv("warn", nil),
		classify: stubClassifier(heimdall.ClassWarn, "r", "RM_WARN"),
	}, stub, testCfg())
	if stub.calls != 0 {
		t.Errorf("LLM must not be consulted on static Warn; calls=%d", stub.calls)
	}
}

func TestLLM_L1_4_StaticBlockSkipsLLM(t *testing.T) {
	stub := &stubLLM{classRet: heimdall.ClassAllow}
	_, errOut, code := runLLM(t, runPTUOpts{
		stdin:    bashEvent("rm -rf /"),
		env:      enabledEnv("block", nil),
		classify: stubClassifier(heimdall.ClassBlock, "root", "RM_RF_ROOT"),
	}, stub, testCfg())
	if stub.calls != 0 {
		t.Errorf("LLM must not be consulted on static Block; calls=%d", stub.calls)
	}
	if code != 2 {
		t.Errorf("static block in block mode must exit 2; got %d", code)
	}
	if !strings.Contains(errOut, "RM_RF_ROOT") {
		t.Errorf("stderr must surface the static rule id: %q", errOut)
	}
}

// -----------------------------------------------------------------------
// L1-5 — Unknown triggers LLM (exactly one call, with the command).
// -----------------------------------------------------------------------

func TestLLM_L1_5_UnknownTriggersLLM(t *testing.T) {
	var seen string
	stub := &stubLLM{
		classRet: heimdall.ClassAllow,
		onCall: func(ctx context.Context) {
			// Capture nothing from ctx; we just want to confirm invocation.
		},
	}
	// Wrap to capture the actual cmd passed in.
	stubCapture := &stubCmdLLM{inner: stub, capture: &seen}
	runLLM(t, runPTUOpts{
		stdin:    bashEvent("terraform destroy -auto-approve"),
		env:      enabledEnv("shadow", nil),
		classify: stubClassifier(heimdall.ClassUnknown, "", ""),
	}, stubCapture, testCfg())
	if stub.calls != 1 {
		t.Errorf("expected 1 LLM call, got %d", stub.calls)
	}
	if seen != "terraform destroy -auto-approve" {
		t.Errorf("LLM got cmd=%q, want %q", seen, "terraform destroy -auto-approve")
	}
}

type stubCmdLLM struct {
	inner   *stubLLM
	capture *string
}

func (s *stubCmdLLM) ClassifyBash(ctx context.Context, cmd string) (heimdall.Classification, string, error) {
	*s.capture = cmd
	return s.inner.ClassifyBash(ctx, cmd)
}

// -----------------------------------------------------------------------
// L1-6..L1-8 — LLM verdict shape: Allow/Warn/Block → class+ruleID forwarded.
// -----------------------------------------------------------------------

func TestLLM_L1_6_LLMAllow(t *testing.T) {
	logPath := hookLogTempEnv(t)
	stub := &stubLLM{classRet: heimdall.ClassAllow, reason: "looks benign"}
	_, _, code := runLLM(t, runPTUOpts{
		stdin:    bashEvent("make build"),
		env:      enabledEnv("block", nil),
		classify: stubClassifier(heimdall.ClassUnknown, "", ""),
	}, stub, testCfg())
	if code != 0 {
		t.Errorf("Allow in block mode must exit 0; got %d", code)
	}
	logTxt := readLog(t, logPath)
	if !strings.Contains(logTxt, "class=allow") {
		t.Errorf("log must record class=allow after LLM upgrade: %s", logTxt)
	}
	if !strings.Contains(logTxt, "rule_id=llm:llama3.2:3b") {
		t.Errorf("log must record rule_id=llm:<model>: %s", logTxt)
	}
}

func TestLLM_L1_7_LLMWarn(t *testing.T) {
	stub := &stubLLM{classRet: heimdall.ClassWarn, reason: "recoverable-destructive"}
	out, _, code := runLLM(t, runPTUOpts{
		stdin:    bashEvent("rm -rf build/"),
		env:      enabledEnv("warn", nil),
		classify: stubClassifier(heimdall.ClassUnknown, "", ""),
	}, stub, testCfg())
	if code != 0 {
		t.Errorf("Warn must exit 0; got %d", code)
	}
	if !strings.Contains(out, "## Heimdall guardrail") {
		t.Errorf("warn mode must render guardrail block on LLM warn: %q", out)
	}
	if !strings.Contains(out, "llm:llama3.2:3b") {
		t.Errorf("warn block must surface llm:<model> rule id: %q", out)
	}
	if !strings.Contains(out, "recoverable-destructive") {
		t.Errorf("reason must be forwarded: %q", out)
	}
}

func TestLLM_L1_8_LLMBlockExit2(t *testing.T) {
	stub := &stubLLM{classRet: heimdall.ClassBlock, reason: "drops prod"}
	_, errOut, code := runLLM(t, runPTUOpts{
		stdin:    bashEvent("psql prod -c 'DROP TABLE users'"),
		env:      enabledEnv("block", nil),
		classify: stubClassifier(heimdall.ClassUnknown, "", ""),
	}, stub, testCfg())
	if code != 2 {
		t.Errorf("Block in block mode must exit 2; got %d", code)
	}
	if !strings.Contains(errOut, "llm:llama3.2:3b") {
		t.Errorf("stderr must carry llm:<model> prefix: %q", errOut)
	}
	if !strings.Contains(errOut, "drops prod") {
		t.Errorf("stderr must forward reason: %q", errOut)
	}
}

// -----------------------------------------------------------------------
// L1-9 — Timeout: DeadlineExceeded → log llm.classifier.timeout, exit 0.
// -----------------------------------------------------------------------

func TestLLM_L1_9_Timeout(t *testing.T) {
	logPath := hookLogTempEnv(t)
	stub := &stubLLM{err: context.DeadlineExceeded}
	_, errOut, code := runLLM(t, runPTUOpts{
		stdin:    bashEvent("make build"),
		env:      enabledEnv("block", nil),
		classify: stubClassifier(heimdall.ClassUnknown, "", ""),
	}, stub, testCfg())
	if code != 0 {
		t.Errorf("Timeout fail-open → exit 0; got %d", code)
	}
	if errOut != "" {
		t.Errorf("stderr must stay empty on timeout (OQ-5); got %q", errOut)
	}
	logTxt := readLog(t, logPath)
	if !strings.Contains(logTxt, "stage=llm.classifier.timeout") {
		t.Errorf("expected timeout log event: %s", logTxt)
	}
}

// -----------------------------------------------------------------------
// L1-10 — Unreachable: conn-refused → log llm.classifier.unreachable.
// -----------------------------------------------------------------------

func TestLLM_L1_10_Unreachable(t *testing.T) {
	logPath := hookLogTempEnv(t)
	// Point Tier-B suppression at a unique store so we don't pick up state
	// from earlier test runs.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stub := &stubLLM{err: errors.New("ollama chat: connection refused")}
	_, _, code := runLLM(t, runPTUOpts{
		stdin:    bashEvent("make build"),
		env:      enabledEnv("shadow", nil),
		classify: stubClassifier(heimdall.ClassUnknown, "", ""),
	}, stub, testCfg())
	if code != 0 {
		t.Errorf("Unreachable fail-open → exit 0; got %d", code)
	}
	logTxt := readLog(t, logPath)
	if !strings.Contains(logTxt, "stage=llm.classifier.unreachable") {
		t.Errorf("expected unreachable log event: %s", logTxt)
	}
}

// -----------------------------------------------------------------------
// L1-11 — Bad JSON → log llm.classifier.bad_response.
// -----------------------------------------------------------------------

func TestLLM_L1_11_BadResponse(t *testing.T) {
	logPath := hookLogTempEnv(t)
	stub := &stubLLM{err: heimdall.ErrLLMBadResponse}
	_, _, code := runLLM(t, runPTUOpts{
		stdin:    bashEvent("make build"),
		env:      enabledEnv("block", nil),
		classify: stubClassifier(heimdall.ClassUnknown, "", ""),
	}, stub, testCfg())
	if code != 0 {
		t.Errorf("Bad response fail-open → exit 0; got %d", code)
	}
	logTxt := readLog(t, logPath)
	if !strings.Contains(logTxt, "stage=llm.classifier.bad_response") {
		t.Errorf("expected bad_response log event: %s", logTxt)
	}
}

// -----------------------------------------------------------------------
// L1-12 — Bad class (maps to bad_response in our wire contract).
// -----------------------------------------------------------------------

func TestLLM_L1_12_BadClass(t *testing.T) {
	// Simulate a "maybe" response via the parse helper. This exercises the
	// layer 1 expectation that ErrLLMBadResponse is the bucket for any
	// schema-violation, including "valid JSON but unknown class value."
	_, _, err := heimdall.TestHelperParseLLMContent(`{"class":"maybe","reason":"x"}`)
	if err == nil || !errors.Is(err, heimdall.ErrLLMBadResponse) {
		t.Fatalf("parseLLMChatContent must flag bad class; got err=%v", err)
	}
}

// -----------------------------------------------------------------------
// L1-13 — Disabled by env: HEIMDALL_LLM_CLASSIFIER unset/0 → LLM never called.
// -----------------------------------------------------------------------

func TestLLM_L1_13_DisabledByEnv(t *testing.T) {
	stub := &stubLLM{classRet: heimdall.ClassBlock, reason: "wrong!"}
	// No HEIMDALL_LLM_CLASSIFIER at all:
	_, _, _ = runLLM(t, runPTUOpts{
		stdin:    bashEvent("make build"),
		env:      map[string]string{"HEIMDALL_GUARDRAILS": "block"},
		classify: stubClassifier(heimdall.ClassUnknown, "", ""),
	}, stub, testCfg())
	if stub.calls != 0 {
		t.Errorf("LLM must not run when env var unset; calls=%d", stub.calls)
	}

	// HEIMDALL_LLM_CLASSIFIER=0 (explicit off):
	stub.calls = 0
	_, _, _ = runLLM(t, runPTUOpts{
		stdin:    bashEvent("make build"),
		env:      map[string]string{"HEIMDALL_GUARDRAILS": "block", "HEIMDALL_LLM_CLASSIFIER": "0"},
		classify: stubClassifier(heimdall.ClassUnknown, "", ""),
	}, stub, testCfg())
	if stub.calls != 0 {
		t.Errorf("LLM must not run with HEIMDALL_LLM_CLASSIFIER=0; calls=%d", stub.calls)
	}
}

// -----------------------------------------------------------------------
// L1-14 — Disabled by empty model + env on → log no_model_configured, skip.
// -----------------------------------------------------------------------

func TestLLM_L1_14_DisabledByEmptyModel(t *testing.T) {
	logPath := hookLogTempEnv(t)
	stub := &stubLLM{classRet: heimdall.ClassBlock, reason: "nope"}
	_, _, code := runLLM(t, runPTUOpts{
		stdin:    bashEvent("make build"),
		env:      enabledEnv("block", nil),
		classify: stubClassifier(heimdall.ClassUnknown, "", ""),
	}, stub, config.Config{LLMClassifierModel: ""}) // explicit empty model
	if code != 0 {
		t.Errorf("exit 0 expected when no model configured; got %d", code)
	}
	if stub.calls != 0 {
		t.Errorf("LLM must not run with empty model; calls=%d", stub.calls)
	}
	logTxt := readLog(t, logPath)
	if !strings.Contains(logTxt, "stage=llm.classifier.no_model_configured") {
		t.Errorf("expected no_model_configured log; got: %s", logTxt)
	}
}

// -----------------------------------------------------------------------
// L1-15 — Block mode + LLM block → exit 2 with stderr `llm:` prefix.
// (Already covered by L1-8 above; this test reiterates the OQ-5 contract
// so the stderr format is greppable in regression runs.)
// -----------------------------------------------------------------------

func TestLLM_L1_15_BlockModeLLMBlockExits2(t *testing.T) {
	stub := &stubLLM{classRet: heimdall.ClassBlock, reason: "wipes artifact cache"}
	_, errOut, code := runLLM(t, runPTUOpts{
		stdin:    bashEvent("make nuke-everything"),
		env:      enabledEnv("block", nil),
		classify: stubClassifier(heimdall.ClassUnknown, "", ""),
	}, stub, testCfg())
	if code != 2 {
		t.Fatalf("exit 2 expected on block+block; got %d", code)
	}
	if !strings.HasPrefix(strings.TrimSpace(errOut), "heimdall guardrail:") {
		t.Errorf("stderr must start with 'heimdall guardrail:': %q", errOut)
	}
	if !strings.Contains(errOut, "(llm:llama3.2:3b)") {
		t.Errorf("stderr must carry the llm: rule id suffix: %q", errOut)
	}
}

// -----------------------------------------------------------------------
// L1-16 — Shadow mode + LLM block → exit 0, log-only (shadow-first).
// -----------------------------------------------------------------------

func TestLLM_L1_16_ShadowModeLLMBlockExits0(t *testing.T) {
	logPath := hookLogTempEnv(t)
	stub := &stubLLM{classRet: heimdall.ClassBlock, reason: "shadow only"}
	out, errOut, code := runLLM(t, runPTUOpts{
		stdin:    bashEvent("make nuke-everything"),
		env:      enabledEnv("shadow", nil),
		classify: stubClassifier(heimdall.ClassUnknown, "", ""),
	}, stub, testCfg())
	if code != 0 {
		t.Errorf("shadow must exit 0 even on LLM block; got %d", code)
	}
	if out != "" || errOut != "" {
		t.Errorf("shadow must stay silent, got stdout=%q stderr=%q", out, errOut)
	}
	logTxt := readLog(t, logPath)
	if !strings.Contains(logTxt, "class=block") {
		t.Errorf("shadow log must record the LLM verdict: %s", logTxt)
	}
}

// -----------------------------------------------------------------------
// L1-17 — Warn mode + LLM block → exit 0, stdout guardrail block.
// -----------------------------------------------------------------------

func TestLLM_L1_17_WarnModeLLMBlockStdout(t *testing.T) {
	stub := &stubLLM{classRet: heimdall.ClassBlock, reason: "would destroy prod"}
	out, errOut, code := runLLM(t, runPTUOpts{
		stdin:    bashEvent("kubectl delete --all-namespaces pod"),
		env:      enabledEnv("warn", nil),
		classify: stubClassifier(heimdall.ClassUnknown, "", ""),
	}, stub, testCfg())
	if code != 0 {
		t.Errorf("warn mode must exit 0 even on LLM block; got %d", code)
	}
	if errOut != "" {
		t.Errorf("stderr must stay empty in warn mode; got %q", errOut)
	}
	if !strings.Contains(out, "## Heimdall guardrail") {
		t.Errorf("warn-mode LLM block must render guardrail block: %q", out)
	}
}

// -----------------------------------------------------------------------
// EXTRA — default-off invariant: with HEIMDALL_LLM_CLASSIFIER unset and
// deps.LLM nil, ZERO behavior change vs today's static-only path.
// -----------------------------------------------------------------------

func TestLLM_DefaultOff_ZeroBehaviorChange(t *testing.T) {
	// Real classifier + static block + block mode → exit 2. Same as the
	// pre-existing TestHookPreToolUse_EndToEnd_RealClassifier_Block, run
	// here to double-pin the invariant after the refactor.
	var out, errBuf bytes.Buffer
	cfg := config.Config{} // empty — no model configured
	code := HookPreToolUse(
		cfg,
		strings.NewReader(bashEvent("rm -rf /")),
		&out, &errBuf,
		map[string]string{"HEIMDALL_GUARDRAILS": "block"},
		nil,
		HookPreToolUseDeps{}, // zero — no LLM
	)
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "RM_RF_ROOT") {
		t.Errorf("stderr missing static rule id: %q", errBuf.String())
	}
}

// -----------------------------------------------------------------------
// EXTRA — Timeout override honors the env var and caps at 2000ms.
// -----------------------------------------------------------------------

func TestLLM_TimeoutOverrideCap(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", 1500 * time.Millisecond},
		{"500", 500 * time.Millisecond},
		{"3000", 2000 * time.Millisecond}, // capped
		{"not-a-number", 1500 * time.Millisecond},
		{"-1", 1500 * time.Millisecond},
	}
	for _, c := range cases {
		env := map[string]string{}
		if c.env != "" {
			env["HEIMDALL_LLM_CLASSIFIER_TIMEOUT_MS"] = c.env
		}
		got := llmTimeoutFromEnv(env)
		if got != c.want {
			t.Errorf("env=%q got %v want %v", c.env, got, c.want)
		}
	}
}

// -----------------------------------------------------------------------
// helpers.
// -----------------------------------------------------------------------

func readLog(t *testing.T, path string) string {
	t.Helper()
	data, _ := os.ReadFile(path)
	return string(data)
}
