package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// runPreToolUse is a tiny test harness mirroring other hook tests' style:
// build stdin/stdout/stderr buffers, run the hook, return the trio.
type runPTUOpts struct {
	stdin    string
	env      map[string]string
	args     []string
	classify func(string) (heimdall.Classification, string, string)
}

func runHookPreToolUse(t *testing.T, opts runPTUOpts) (stdout, stderr string, code int) {
	t.Helper()
	var out, errBuf bytes.Buffer
	deps := HookPreToolUseDeps{Classify: opts.classify}
	cfg := config.Config{}
	code = HookPreToolUse(cfg, strings.NewReader(opts.stdin), &out, &errBuf, opts.env, opts.args, deps)
	return out.String(), errBuf.String(), code
}

// stubClassifier returns a canned (class, reason, id) regardless of input.
func stubClassifier(class heimdall.Classification, reason, id string) func(string) (heimdall.Classification, string, string) {
	return func(cmd string) (heimdall.Classification, string, string) {
		return class, reason, id
	}
}

// -----------------------------------------------------------------------
// Bash payload. Tests use `tool_input.command` per Claude Code contract.
// -----------------------------------------------------------------------
const bashEventTmpl = `{"tool_name":"Bash","tool_input":{"command":"%s"}}`

func bashEvent(cmd string) string {
	return strings.Replace(bashEventTmpl, "%s", cmd, 1)
}

// 1 — Shadow mode (default) + ALLOW class: exit 0, empty stdout, empty stderr.
func TestHookPreToolUse_ShadowAllow(t *testing.T) {
	out, errOut, code := runHookPreToolUse(t, runPTUOpts{
		stdin:    bashEvent("ls -la"),
		env:      map[string]string{}, // no HEIMDALL_GUARDRAILS → shadow
		classify: stubClassifier(heimdall.ClassAllow, "", ""),
	})
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty", out)
	}
	if errOut != "" {
		t.Errorf("stderr = %q, want empty", errOut)
	}
}

// 2 — Shadow mode + WARN class: still exit 0, empty stdout/stderr (log-only).
func TestHookPreToolUse_ShadowWarn(t *testing.T) {
	out, errOut, code := runHookPreToolUse(t, runPTUOpts{
		stdin:    bashEvent("rm -rf node_modules"),
		env:      map[string]string{"HEIMDALL_GUARDRAILS": "shadow"},
		classify: stubClassifier(heimdall.ClassWarn, "recursive delete", "RM_RF_RECURSIVE_GENERIC"),
	})
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty in shadow", out)
	}
	if errOut != "" {
		t.Errorf("stderr = %q, want empty in shadow", errOut)
	}
}

// 3 — Shadow mode + BLOCK class: still exit 0, nothing emitted (shadow never blocks).
func TestHookPreToolUse_ShadowBlock(t *testing.T) {
	out, errOut, code := runHookPreToolUse(t, runPTUOpts{
		stdin:    bashEvent("rm -rf /"),
		env:      map[string]string{}, // default shadow
		classify: stubClassifier(heimdall.ClassBlock, "root fs", "RM_RF_ROOT"),
	})
	if code != 0 {
		t.Errorf("code = %d, want 0 in shadow even on block class", code)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty in shadow", out)
	}
	if errOut != "" {
		t.Errorf("stderr = %q, want empty in shadow", errOut)
	}
}

// 4 — Warn mode + WARN class: stdout gets the guardrail block, exit 0, no stderr.
func TestHookPreToolUse_WarnModeWarn(t *testing.T) {
	out, errOut, code := runHookPreToolUse(t, runPTUOpts{
		stdin:    bashEvent("rm -rf build/"),
		env:      map[string]string{"HEIMDALL_GUARDRAILS": "warn"},
		classify: stubClassifier(heimdall.ClassWarn, "Recursive deletion.", "RM_RF_RECURSIVE_GENERIC"),
	})
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if !strings.Contains(out, "## Heimdall guardrail") {
		t.Errorf("stdout missing guardrail block: %q", out)
	}
	if !strings.Contains(out, "RM_RF_RECURSIVE_GENERIC") {
		t.Errorf("stdout missing rule id: %q", out)
	}
	if errOut != "" {
		t.Errorf("stderr = %q, want empty in warn mode", errOut)
	}
}

// 5 — Warn mode + BLOCK class: stdout gets the guardrail block but we STILL
// exit 0 (block enforcement requires block mode).
func TestHookPreToolUse_WarnModeBlockClass(t *testing.T) {
	out, errOut, code := runHookPreToolUse(t, runPTUOpts{
		stdin:    bashEvent("rm -rf /"),
		env:      map[string]string{"HEIMDALL_GUARDRAILS": "warn"},
		classify: stubClassifier(heimdall.ClassBlock, "root fs", "RM_RF_ROOT"),
	})
	if code != 0 {
		t.Errorf("code = %d, want 0 in warn mode", code)
	}
	if !strings.Contains(out, "## Heimdall guardrail") {
		t.Errorf("stdout should surface block class in warn mode: %q", out)
	}
	if errOut != "" {
		t.Errorf("stderr = %q, want empty in warn mode", errOut)
	}
}

// 6 — Warn mode + ALLOW class: nothing emitted, exit 0.
func TestHookPreToolUse_WarnModeAllow(t *testing.T) {
	out, errOut, code := runHookPreToolUse(t, runPTUOpts{
		stdin:    bashEvent("ls -la"),
		env:      map[string]string{"HEIMDALL_GUARDRAILS": "warn"},
		classify: stubClassifier(heimdall.ClassAllow, "", ""),
	})
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty on allow", out)
	}
	if errOut != "" {
		t.Errorf("stderr = %q, want empty on allow", errOut)
	}
}

// 7 — Block mode + BLOCK class: exit 2, stderr reason, empty stdout.
func TestHookPreToolUse_BlockModeBlockExits2(t *testing.T) {
	out, errOut, code := runHookPreToolUse(t, runPTUOpts{
		stdin:    bashEvent("rm -rf /"),
		env:      map[string]string{"HEIMDALL_GUARDRAILS": "block"},
		classify: stubClassifier(heimdall.ClassBlock, "wipes root fs", "RM_RF_ROOT"),
	})
	if code != 2 {
		t.Errorf("code = %d, want 2 (block class + block mode)", code)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty on block", out)
	}
	if !strings.Contains(errOut, "wipes root fs") {
		t.Errorf("stderr missing reason: %q", errOut)
	}
	if !strings.Contains(errOut, "RM_RF_ROOT") {
		t.Errorf("stderr missing rule id: %q", errOut)
	}
}

// 8 — Block mode + WARN class: stdout block, exit 0 (warn never blocks).
func TestHookPreToolUse_BlockModeWarn(t *testing.T) {
	out, errOut, code := runHookPreToolUse(t, runPTUOpts{
		stdin:    bashEvent("rm -rf node_modules"),
		env:      map[string]string{"HEIMDALL_GUARDRAILS": "block"},
		classify: stubClassifier(heimdall.ClassWarn, "recursive", "RM_RF_RECURSIVE_GENERIC"),
	})
	if code != 0 {
		t.Errorf("code = %d, want 0 (warn class never blocks)", code)
	}
	if !strings.Contains(out, "## Heimdall guardrail") {
		t.Errorf("stdout missing block: %q", out)
	}
	if errOut != "" {
		t.Errorf("stderr = %q, want empty for warn class", errOut)
	}
}

// 9 — Block mode + ALLOW class: nothing, exit 0.
func TestHookPreToolUse_BlockModeAllow(t *testing.T) {
	out, errOut, code := runHookPreToolUse(t, runPTUOpts{
		stdin:    bashEvent("ls -la"),
		env:      map[string]string{"HEIMDALL_GUARDRAILS": "block"},
		classify: stubClassifier(heimdall.ClassAllow, "", ""),
	})
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if out != "" || errOut != "" {
		t.Errorf("want empty stdout+stderr for allow, got stdout=%q stderr=%q", out, errOut)
	}
}

// 10 — HEIMDALL_GUARDRAILS=off short-circuits (classifier must not run).
func TestHookPreToolUse_OffShortCircuits(t *testing.T) {
	classifierCalled := false
	out, errOut, code := runHookPreToolUse(t, runPTUOpts{
		stdin: bashEvent("rm -rf /"),
		env:   map[string]string{"HEIMDALL_GUARDRAILS": "off"},
		classify: func(cmd string) (heimdall.Classification, string, string) {
			classifierCalled = true
			return heimdall.ClassBlock, "should not fire", "RM_RF_ROOT"
		},
	})
	if classifierCalled {
		t.Errorf("classifier should not run when guardrails=off")
	}
	if code != 0 {
		t.Errorf("code = %d, want 0 with off", code)
	}
	if out != "" || errOut != "" {
		t.Errorf("off mode must emit nothing, got stdout=%q stderr=%q", out, errOut)
	}
}

// 11 — Non-Bash tool is ignored (no classify, no stdout, exit 0).
func TestHookPreToolUse_NonBashIgnored(t *testing.T) {
	classifierCalled := false
	out, errOut, code := runHookPreToolUse(t, runPTUOpts{
		stdin: `{"tool_name":"Edit","tool_input":{"file_path":"/tmp/foo.txt","new_string":"x"}}`,
		env:   map[string]string{"HEIMDALL_GUARDRAILS": "block"},
		classify: func(cmd string) (heimdall.Classification, string, string) {
			classifierCalled = true
			return heimdall.ClassBlock, "should not run", "WRONG"
		},
	})
	if classifierCalled {
		t.Errorf("classifier should not run for non-Bash tools")
	}
	if code != 0 {
		t.Errorf("code = %d, want 0 for non-Bash tool", code)
	}
	if out != "" || errOut != "" {
		t.Errorf("non-Bash must be silent, got stdout=%q stderr=%q", out, errOut)
	}
}

// 12 — Malformed JSON fails open (exit 0, empty stdout/stderr).
func TestHookPreToolUse_MalformedJSONFailsOpen(t *testing.T) {
	out, errOut, code := runHookPreToolUse(t, runPTUOpts{
		stdin:    `{not json}`,
		env:      map[string]string{"HEIMDALL_GUARDRAILS": "block"},
		classify: stubClassifier(heimdall.ClassBlock, "nope", "WRONG"),
	})
	if code != 0 {
		t.Errorf("code = %d, want 0 on bad JSON (fail-open)", code)
	}
	if out != "" || errOut != "" {
		t.Errorf("bad JSON must stay silent, got stdout=%q stderr=%q", out, errOut)
	}
}

// 13 — HEIMDALL_HOOKS=0 short-circuits (classifier must not run).
func TestHookPreToolUse_HooksDisabledEnv(t *testing.T) {
	classifierCalled := false
	_, errOut, code := runHookPreToolUse(t, runPTUOpts{
		stdin: bashEvent("rm -rf /"),
		env:   map[string]string{"HEIMDALL_HOOKS": "0", "HEIMDALL_GUARDRAILS": "block"},
		classify: func(cmd string) (heimdall.Classification, string, string) {
			classifierCalled = true
			return heimdall.ClassBlock, "nope", "RM_RF_ROOT"
		},
	})
	if classifierCalled {
		t.Errorf("classifier should not run when HEIMDALL_HOOKS=0")
	}
	if code != 0 {
		t.Errorf("code = %d, want 0 with HEIMDALL_HOOKS=0", code)
	}
	if errOut != "" {
		t.Errorf("stderr should be empty when disabled, got %q", errOut)
	}
}

// 14 — .heimdall/hooks.disabled marker short-circuits.
func TestHookPreToolUse_HooksDisabledMarker(t *testing.T) {
	project := t.TempDir()
	// Create the marker.
	if err := os.MkdirAll(filepath.Join(project, ".heimdall"), 0o755); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(project, ".heimdall", "hooks.disabled")
	if err := os.WriteFile(markerPath, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}

	// CWD pointing at a project with the marker should disable hooks even
	// though HEIMDALL_GUARDRAILS=block.
	classifierCalled := false
	stdin := `{"tool_name":"Bash","tool_input":{"command":"rm -rf /"},"cwd":"` + project + `"}`
	out, errOut, code := runHookPreToolUse(t, runPTUOpts{
		stdin: stdin,
		env:   map[string]string{"HEIMDALL_GUARDRAILS": "block"},
		classify: func(cmd string) (heimdall.Classification, string, string) {
			classifierCalled = true
			return heimdall.ClassBlock, "nope", "RM_RF_ROOT"
		},
	})
	if classifierCalled {
		t.Errorf("classifier should not run with hooks.disabled marker present")
	}
	if code != 0 {
		t.Errorf("code = %d, want 0 when marker present", code)
	}
	if out != "" || errOut != "" {
		t.Errorf("disabled hook must be silent, got stdout=%q stderr=%q", out, errOut)
	}
}

// 15 — Empty command in payload: log-only, exit 0.
func TestHookPreToolUse_EmptyCommand(t *testing.T) {
	out, errOut, code := runHookPreToolUse(t, runPTUOpts{
		stdin:    `{"tool_name":"Bash","tool_input":{"command":""}}`,
		env:      map[string]string{"HEIMDALL_GUARDRAILS": "block"},
		classify: stubClassifier(heimdall.ClassBlock, "x", "y"),
	})
	if code != 0 {
		t.Errorf("code = %d, want 0 on empty command", code)
	}
	if out != "" || errOut != "" {
		t.Errorf("empty command must be silent, got stdout=%q stderr=%q", out, errOut)
	}
}

// 16 — Empty stdin: exit 0, no classify.
func TestHookPreToolUse_EmptyStdin(t *testing.T) {
	classifierCalled := false
	out, errOut, code := runHookPreToolUse(t, runPTUOpts{
		stdin: "",
		env:   map[string]string{"HEIMDALL_GUARDRAILS": "block"},
		classify: func(cmd string) (heimdall.Classification, string, string) {
			classifierCalled = true
			return heimdall.ClassBlock, "", ""
		},
	})
	if classifierCalled {
		t.Errorf("classifier should not run on empty stdin")
	}
	if code != 0 {
		t.Errorf("code = %d, want 0 on empty stdin", code)
	}
	if out != "" || errOut != "" {
		t.Errorf("empty stdin must be silent, got stdout=%q stderr=%q", out, errOut)
	}
}

// 17 — Dispatcher routes "pre-tool-use" to HookPreToolUse (default classifier).
// With shadow mode (default) and the real classifier, `rm -rf /` classifies
// as block but shadow mode exits 0 with no emission.
func TestDispatchHook_RoutesPreToolUse(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := DispatchHook(
		config.Config{},
		strings.NewReader(bashEvent("rm -rf /")),
		&out, &errBuf,
		map[string]string{}, // default shadow
		[]string{"pre-tool-use"},
	)
	if code != 0 {
		t.Errorf("code = %d, want 0 in shadow mode", code)
	}
	if out.String() != "" {
		t.Errorf("stdout = %q, want empty in shadow", out.String())
	}
	if errBuf.String() != "" {
		t.Errorf("stderr = %q, want empty in shadow", errBuf.String())
	}
}

// 18 — Real classifier end-to-end: block mode + rm -rf / → exit 2, stderr.
// Uses the real heimdall.ClassifyBashCommand (not a stub) to prove the
// whole pipeline wires correctly.
func TestHookPreToolUse_EndToEnd_RealClassifier_Block(t *testing.T) {
	out, errOut, code := runHookPreToolUse(t, runPTUOpts{
		stdin: bashEvent("rm -rf /"),
		env:   map[string]string{"HEIMDALL_GUARDRAILS": "block"},
		// classify: nil — use the real classifier.
	})
	if code != 2 {
		t.Errorf("code = %d, want 2 for real rm -rf / in block mode", code)
	}
	if out != "" {
		t.Errorf("stdout should be empty on block, got %q", out)
	}
	if !strings.Contains(errOut, "RM_RF_ROOT") {
		t.Errorf("stderr missing RM_RF_ROOT rule id, got %q", errOut)
	}
}

// ---------------------------------------------------------------------------
// 19 — Session-id threading (Wave A).
// ---------------------------------------------------------------------------

func TestHookPreToolUse_LogsSessionID(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HEIMDALL_HOOK_LOG", filepath.Join(tmp, "hooks.log"))

	payload := `{"session_id":"sess-abc","tool_name":"Bash","tool_input":{"command":"ls -la"},"cwd":"/tmp"}`
	stdin := strings.NewReader(payload)
	var out, errBuf bytes.Buffer

	rc := HookPreToolUse(config.Config{}, stdin, &out, &errBuf, map[string]string{"HEIMDALL_GUARDRAILS": "shadow"}, nil, HookPreToolUseDeps{})
	if rc != 0 {
		t.Fatalf("expected exit 0 in shadow mode, got %d", rc)
	}

	data, _ := os.ReadFile(filepath.Join(tmp, "hooks.log"))
	if !strings.Contains(string(data), "session=sess-abc") {
		t.Fatalf("expected session=sess-abc:\n%s", string(data))
	}
	if !strings.Contains(string(data), "class=allow") {
		t.Fatalf("expected class=allow:\n%s", string(data))
	}
}
