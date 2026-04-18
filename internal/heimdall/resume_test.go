package heimdall

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestResumeMarker_WriteRead is the primary round-trip regression:
// WriteResumeMarker → ReadResumeMarker returns the same fields.
func TestResumeMarker_WriteRead(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), ".heimdall_db")
	start := time.Date(2026, 4, 18, 12, 34, 56, 0, time.UTC)
	models := []string{"nomic-embed-text", "bge-m3"}

	if err := WriteResumeMarker(baseDir, models, start); err != nil {
		t.Fatalf("WriteResumeMarker: %v", err)
	}

	got, err := ReadResumeMarker(baseDir)
	if err != nil {
		t.Fatalf("ReadResumeMarker: %v", err)
	}
	if got == nil {
		t.Fatal("ReadResumeMarker returned nil, want marker")
	}
	if !got.StartTime.Equal(start) {
		t.Errorf("StartTime = %v, want %v", got.StartTime, start)
	}
	if !reflect.DeepEqual(got.Models, models) {
		t.Errorf("Models = %v, want %v", got.Models, models)
	}
	if got.Status != "running" {
		t.Errorf("Status = %q, want %q", got.Status, "running")
	}
}

// TestResumeMarker_ReadMissingIsNoError asserts that a missing marker
// returns (nil, nil) rather than an error — "no interrupted run" is the
// happy path and must not break callers.
func TestResumeMarker_ReadMissingIsNoError(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), ".heimdall_db")
	got, err := ReadResumeMarker(baseDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("got marker %+v, want nil", got)
	}
}

// TestResumeMarker_Delete asserts DeleteResumeMarker removes the file
// and is idempotent (second call on missing marker succeeds).
func TestResumeMarker_Delete(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), ".heimdall_db")
	if err := WriteResumeMarker(baseDir, []string{"m"}, time.Now()); err != nil {
		t.Fatalf("WriteResumeMarker: %v", err)
	}

	if err := DeleteResumeMarker(baseDir); err != nil {
		t.Fatalf("DeleteResumeMarker (1st): %v", err)
	}
	if _, err := os.Stat(ResumeMarkerPath(baseDir)); !os.IsNotExist(err) {
		t.Errorf("marker still exists after delete: %v", err)
	}
	if err := DeleteResumeMarker(baseDir); err != nil {
		t.Errorf("DeleteResumeMarker (2nd, missing file) = %v, want nil", err)
	}
}

// TestResumeMarker_CorruptIsError asserts a corrupt marker surfaces an
// error — silently ignoring it would hide a filesystem bug and drop the
// user back into a confused "why is my index empty" state.
func TestResumeMarker_CorruptIsError(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), ".heimdall_db")
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ResumeMarkerPath(baseDir), []byte("not valid json{"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := ReadResumeMarker(baseDir)
	if err == nil {
		t.Error("expected error for corrupt marker, got nil")
	}
}

// TestResumeMarker_PersistsAfterInterruption simulates the interrupted-
// indexing lifecycle: Write the marker at start; DO NOT call Delete
// (simulating ctrl-C / crash mid-indexing). The marker must still be
// on disk so the next run's Read returns it and can prompt the user.
//
// This is the regression test for the primary stop/resume UX contract:
// partial SQLite writes are fine for incremental resume, but the USER
// needs a visible hint that something was interrupted.
func TestResumeMarker_PersistsAfterInterruption(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), ".heimdall_db")
	if err := WriteResumeMarker(baseDir, []string{"nomic-embed-text"}, time.Now()); err != nil {
		t.Fatalf("WriteResumeMarker: %v", err)
	}
	// NO DeleteResumeMarker call — simulating interruption.
	m, err := ReadResumeMarker(baseDir)
	if err != nil {
		t.Fatalf("ReadResumeMarker: %v", err)
	}
	if m == nil {
		t.Error("marker should still be on disk after interrupted run")
	}
}

// TestResumeMarker_CleanCompletionRemovesMarker simulates the happy-path
// lifecycle: Write at start, Delete at end, Read returns nil. Mirrors
// the final arc in cliIndex's happy path.
func TestResumeMarker_CleanCompletionRemovesMarker(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), ".heimdall_db")
	if err := WriteResumeMarker(baseDir, []string{"m"}, time.Now()); err != nil {
		t.Fatalf("WriteResumeMarker: %v", err)
	}
	if err := DeleteResumeMarker(baseDir); err != nil {
		t.Fatalf("DeleteResumeMarker: %v", err)
	}
	m, err := ReadResumeMarker(baseDir)
	if err != nil {
		t.Fatalf("ReadResumeMarker: %v", err)
	}
	if m != nil {
		t.Errorf("marker should be gone after clean completion, got %+v", m)
	}
}
