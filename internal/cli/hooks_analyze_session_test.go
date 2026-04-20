package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caio-silva/heimdall-mcp/internal/config"
)

func TestHooksAnalyzeSession_MissedCorrection(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := HooksAnalyzeSession(
		config.DefaultConfig(),
		strings.NewReader(""),
		&stdout, &stderr,
		map[string]string{},
		[]string{"--transcript", "testdata/transcript_missed_correction.jsonl"},
	)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Triggers:") {
		t.Errorf("missing Triggers line; got:\n%s", out)
	}
	if !strings.Contains(out, "user_correction") {
		t.Errorf("expected user_correction miss in output; got:\n%s", out)
	}
	if !strings.Contains(out, "heimdall_remember") {
		t.Errorf("expected heimdall_remember mention in output; got:\n%s", out)
	}
}

func TestHooksAnalyzeSession_AllCompliant(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := HooksAnalyzeSession(
		config.DefaultConfig(),
		strings.NewReader(""),
		&stdout, &stderr,
		map[string]string{},
		[]string{"--transcript", "testdata/transcript_all_compliant.jsonl"},
	)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Misses: none") {
		t.Errorf("expected 'Misses: none' for compliant transcript; got:\n%s", out)
	}
}

func TestHooksAnalyzeSession_JSONFormat(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := HooksAnalyzeSession(
		config.DefaultConfig(),
		strings.NewReader(""),
		&stdout, &stderr,
		map[string]string{},
		[]string{"--transcript", "testdata/transcript_missed_correction.jsonl", "--format", "json"},
	)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; stderr: %s", code, stderr.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %s", err, stdout.String())
	}
	if _, ok := payload["triggers"]; !ok {
		t.Errorf("missing 'triggers' field in JSON output: %v", payload)
	}
	if _, ok := payload["misses"]; !ok {
		t.Errorf("missing 'misses' field in JSON output: %v", payload)
	}
}

func TestHooksAnalyzeSession_MissingTranscriptFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := HooksAnalyzeSession(
		config.DefaultConfig(),
		strings.NewReader(""),
		&stdout, &stderr,
		map[string]string{},
		[]string{},
	)
	if code != 1 {
		t.Errorf("expected exit 1 when --transcript missing, got %d", code)
	}
}

func TestHooksAnalyzeSession_NonexistentFile(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := HooksAnalyzeSession(
		config.DefaultConfig(),
		strings.NewReader(""),
		&stdout, &stderr,
		map[string]string{},
		[]string{"--transcript", "/nonexistent/path/transcript.jsonl"},
	)
	if code != 1 {
		t.Errorf("expected exit 1 for nonexistent file, got %d", code)
	}
}

func TestHooksAnalyzeSession_WritesHookLog(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HEIMDALL_HOOK_LOG", filepath.Join(tmp, "hooks.log"))

	var stdout, stderr bytes.Buffer
	code := HooksAnalyzeSession(
		config.DefaultConfig(),
		strings.NewReader(""),
		&stdout, &stderr,
		map[string]string{},
		[]string{"--transcript", "testdata/transcript_missed_correction.jsonl"},
	)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; stderr: %s", code, stderr.String())
	}

	logBytes, err := os.ReadFile(filepath.Join(tmp, "hooks.log"))
	if err != nil {
		t.Fatalf("hooks.log not written: %v", err)
	}
	logStr := string(logBytes)
	if !strings.Contains(logStr, "event=mcp.compliance") {
		t.Errorf("expected event=mcp.compliance in hooks.log; got:\n%s", logStr)
	}
	if !strings.Contains(logStr, "transcript=") {
		t.Errorf("expected transcript= field in hooks.log; got:\n%s", logStr)
	}
}

func TestHooksDispatch_AnalyzeSession(t *testing.T) {
	// Verify the dispatcher routes analyze-session correctly.
	var stdout, stderr bytes.Buffer
	code := DispatchHooks(
		config.DefaultConfig(),
		strings.NewReader(""),
		&stdout, &stderr,
		map[string]string{},
		[]string{"analyze-session", "--transcript", "testdata/transcript_missed_read.jsonl"},
	)
	if code != 0 {
		t.Fatalf("expected exit 0 from dispatcher, got %d; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Transcript:") {
		t.Errorf("expected output from analyze-session, got:\n%s", stdout.String())
	}
}
