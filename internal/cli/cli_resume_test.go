package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// TestResolveResume_NoMarker_ReturnsFresh asserts that without a marker
// the resolver returns "fresh" (go through normal model-selection) and
// prints nothing — the marker path is the exception, not the rule.
func TestResolveResume_NoMarker_ReturnsFresh(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), ".heimdall_db")
	var out bytes.Buffer
	decision, models, err := resolveResume(baseDir, "/fake/path", strings.NewReader(""), &out, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decision != resumeProceedFresh {
		t.Errorf("decision = %v, want resumeProceedFresh", decision)
	}
	if models != nil {
		t.Errorf("models = %v, want nil", models)
	}
	if out.Len() != 0 {
		t.Errorf("output should be empty, got %q", out.String())
	}
}

// TestResolveResume_PromptContinueUsesMarkerModels simulates the user
// pressing "C" + Enter — the resolver returns the marker's recorded
// models so cliIndex can skip its model-selection step entirely.
func TestResolveResume_PromptContinueUsesMarkerModels(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), ".heimdall_db")
	markerModels := []string{"nomic-embed-text", "bge-m3"}
	if err := heimdall.WriteResumeMarker(baseDir, markerModels, time.Now()); err != nil {
		t.Fatalf("WriteResumeMarker: %v", err)
	}
	var out bytes.Buffer
	decision, models, err := resolveResume(baseDir, "/some/path", strings.NewReader("C\n"), &out, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decision != resumeContinue {
		t.Errorf("decision = %v, want resumeContinue", decision)
	}
	if len(models) != 2 || models[0] != "nomic-embed-text" || models[1] != "bge-m3" {
		t.Errorf("models = %v, want %v", models, markerModels)
	}
	// Output should include the warning and the recorded model list
	text := out.String()
	for _, want := range []string{"interrupted", "nomic-embed-text", "bge-m3"} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q.\nGot:\n%s", want, text)
		}
	}
	// Marker must still exist after Continue (indexing proceeds; deletion
	// happens on clean completion, not at prompt time).
	if _, err := os.Stat(heimdall.ResumeMarkerPath(baseDir)); err != nil {
		t.Errorf("marker should still exist after Continue: %v", err)
	}
}

// TestResolveResume_PromptRestartDeletesMarker simulates the user
// pressing "R" + Enter — the resolver deletes the marker and returns
// "fresh" so cliIndex falls through to the regular model-selection flow.
func TestResolveResume_PromptRestartDeletesMarker(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), ".heimdall_db")
	if err := heimdall.WriteResumeMarker(baseDir, []string{"m"}, time.Now()); err != nil {
		t.Fatalf("WriteResumeMarker: %v", err)
	}
	var out bytes.Buffer
	decision, models, err := resolveResume(baseDir, "/x", strings.NewReader("R\n"), &out, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decision != resumeRestart {
		t.Errorf("decision = %v, want resumeRestart", decision)
	}
	if models != nil {
		t.Errorf("models = %v, want nil (caller re-selects)", models)
	}
	if _, err := os.Stat(heimdall.ResumeMarkerPath(baseDir)); !os.IsNotExist(err) {
		t.Errorf("marker should be deleted after Restart: %v", err)
	}
}

// TestResolveResume_PromptCancelExitsCleanly simulates "Q" + Enter —
// the resolver returns resumeCancel. The caller is expected to print a
// "Cancelled" message and exit(0). Marker is preserved so the next run
// picks up where the user left off.
func TestResolveResume_PromptCancelExitsCleanly(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), ".heimdall_db")
	if err := heimdall.WriteResumeMarker(baseDir, []string{"m"}, time.Now()); err != nil {
		t.Fatalf("WriteResumeMarker: %v", err)
	}
	var out bytes.Buffer
	decision, _, err := resolveResume(baseDir, "/x", strings.NewReader("Q\n"), &out, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decision != resumeCancel {
		t.Errorf("decision = %v, want resumeCancel", decision)
	}
	if _, err := os.Stat(heimdall.ResumeMarkerPath(baseDir)); err != nil {
		t.Errorf("marker should be preserved on cancel: %v", err)
	}
}

// TestResolveResume_NonTTYAutoContinues asserts that with isTTY=false
// the resolver auto-chooses "Continue" (best-effort: keep indexing
// progressing rather than blocking on a prompt that nobody can answer).
// A notice line is printed so users tailing the log see what happened.
func TestResolveResume_NonTTYAutoContinues(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), ".heimdall_db")
	models := []string{"nomic-embed-text"}
	if err := heimdall.WriteResumeMarker(baseDir, models, time.Now()); err != nil {
		t.Fatalf("WriteResumeMarker: %v", err)
	}
	var out bytes.Buffer
	decision, got, err := resolveResume(baseDir, "/x", strings.NewReader(""), &out, false /* non-TTY */)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decision != resumeContinue {
		t.Errorf("decision = %v, want resumeContinue", decision)
	}
	if len(got) != 1 || got[0] != "nomic-embed-text" {
		t.Errorf("models = %v, want %v", got, models)
	}
	text := out.String()
	if !strings.Contains(text, "non-TTY") && !strings.Contains(text, "non-interactive") {
		t.Errorf("expected non-TTY notice in output, got:\n%s", text)
	}
}

// TestResolveResume_PromptInvalidInputDefaultsToCancel asserts that
// unrecognised input (not C/R/Q) is treated as Cancel — safer than
// guessing. This avoids "user hit a random key and we blew away their
// models" surprises.
func TestResolveResume_PromptInvalidInputDefaultsToCancel(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), ".heimdall_db")
	if err := heimdall.WriteResumeMarker(baseDir, []string{"m"}, time.Now()); err != nil {
		t.Fatalf("WriteResumeMarker: %v", err)
	}
	var out bytes.Buffer
	decision, _, err := resolveResume(baseDir, "/x", strings.NewReader("xyz\n"), &out, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decision != resumeCancel {
		t.Errorf("decision = %v, want resumeCancel (unknown input should not delete marker)", decision)
	}
	if _, err := os.Stat(heimdall.ResumeMarkerPath(baseDir)); err != nil {
		t.Errorf("marker should be preserved on unknown input: %v", err)
	}
}
