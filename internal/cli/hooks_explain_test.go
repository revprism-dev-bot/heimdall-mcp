package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/caio-silva/heimdall-mcp/internal/config"
)

// runExplain wraps a call to HooksExplainCommand via DispatchHooks so we
// exercise the dispatcher routing as well.
func runExplain(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errBuf bytes.Buffer
	fullArgs := append([]string{"explain-command"}, args...)
	code = DispatchHooks(config.Config{}, nil, &out, &errBuf, map[string]string{}, fullArgs)
	return out.String(), errBuf.String(), code
}

// 1 — Allow command (benign).
func TestHooksExplainCommand_Allow(t *testing.T) {
	out, _, code := runExplain(t, "ls", "-la")
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if !strings.Contains(out, "class=allow") {
		t.Errorf("stdout missing class=allow: %q", out)
	}
	if !strings.Contains(out, "rule=-") {
		t.Errorf("stdout should show empty rule as '-', got: %q", out)
	}
}

// 2 — Warn classification (recoverable rm -rf).
func TestHooksExplainCommand_Warn(t *testing.T) {
	out, _, code := runExplain(t, "rm", "-rf", "node_modules")
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if !strings.Contains(out, "class=warn") {
		t.Errorf("stdout missing class=warn: %q", out)
	}
	if !strings.Contains(out, "rule=RM_RF_RECURSIVE_GENERIC") {
		t.Errorf("stdout missing expected rule id: %q", out)
	}
}

// 3 — Block classification (catastrophic rm -rf /).
func TestHooksExplainCommand_Block(t *testing.T) {
	out, _, code := runExplain(t, "rm", "-rf", "/")
	// Design doc §4 exit code for the dry-run command is 0 unconditionally
	// per our task spec ("Exits 0 regardless"). The original §4 text had
	// 0/2, but the implementation spec is informational = always 0.
	if code != 0 {
		t.Errorf("code = %d, want 0 (explain-command is always 0 informational)", code)
	}
	if !strings.Contains(out, "class=block") {
		t.Errorf("stdout missing class=block: %q", out)
	}
	if !strings.Contains(out, "rule=RM_RF_ROOT") {
		t.Errorf("stdout missing RM_RF_ROOT: %q", out)
	}
	if !strings.Contains(out, "root filesystem") {
		t.Errorf("stdout missing reason text: %q", out)
	}
}

// 4 — Allowlist precedence: --force-with-lease beats generic --force.
func TestHooksExplainCommand_ForceWithLease(t *testing.T) {
	out, _, code := runExplain(t, "git", "push", "--force-with-lease", "origin", "main")
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if !strings.Contains(out, "class=allow") {
		t.Errorf("expected allow for --force-with-lease, got: %q", out)
	}
	if !strings.Contains(out, "rule=GIT_PUSH_FORCE_WITH_LEASE") {
		t.Errorf("missing allow rule id: %q", out)
	}
}

// 5 — Missing arg: usage message to stderr, exit 0.
func TestHooksExplainCommand_NoArg(t *testing.T) {
	out, errOut, code := runExplain(t)
	if code != 0 {
		t.Errorf("code = %d, want 0 even on missing arg", code)
	}
	if out != "" {
		t.Errorf("stdout should be empty on missing arg, got: %q", out)
	}
	if !strings.Contains(errOut, "Usage") {
		t.Errorf("stderr should contain usage, got: %q", errOut)
	}
}

// 6 — Single quoted string arg (common invocation shape).
func TestHooksExplainCommand_SingleQuotedArg(t *testing.T) {
	out, _, code := runExplain(t, "git push --force origin main")
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if !strings.Contains(out, "class=block") {
		t.Errorf("expected block for force-push to main, got: %q", out)
	}
	if !strings.Contains(out, "rule=GIT_PUSH_FORCE_PROTECTED") {
		t.Errorf("expected GIT_PUSH_FORCE_PROTECTED, got: %q", out)
	}
}

// 7 — Dispatcher routing: explain-command reaches the handler.
func TestDispatchHooks_RoutesExplainCommand(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := DispatchHooks(config.Config{}, nil, &out, &errBuf, map[string]string{},
		[]string{"explain-command", "echo", "hi"})
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "class=allow") {
		t.Errorf("expected allow result, got stdout: %q", out.String())
	}
}
