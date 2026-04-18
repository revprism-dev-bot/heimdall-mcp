package heimdall

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// resumeMarkerFilename is the on-disk name of the indexing-in-progress
// sentinel. It lives directly under a project's .heimdall_db/ so one
// marker tracks an entire invocation (outer + every sub-repo pass the
// outer kicks off). Sub-repo interruptions are handled by incremental
// resume on the next run and do not get their own markers.
const resumeMarkerFilename = ".indexing-in-progress"

// ResumeMarker describes an interrupted `heimdall-mcp index` run. When
// present on disk, the next invocation can prompt the user to continue
// with the same model list, restart with a fresh model selection, or
// cancel outright.
//
// Status is always "running" at write time; the field is reserved for
// future states (e.g. "interrupted" after a clean signal handler, or
// "corrupt" to mark an unusable partial-write that auto-deletes on
// read). Parsers MUST tolerate unknown values.
type ResumeMarker struct {
	// StartTime is when the CLI started the indexing run (UTC). Displayed
	// back to the user so they can reason about how old the interrupted
	// run is.
	StartTime time.Time `json:"start_time"`
	// Models is the list of models the interrupted run was supposed to
	// iterate through. If the user picks "continue", we skip model
	// selection and use this list verbatim.
	Models []string `json:"models"`
	// Status is always "running" for markers written by WriteResumeMarker.
	// Future-proofing hook — consumers must treat unknown values as
	// equivalent to "running" (i.e. still prompt).
	Status string `json:"status"`
}

// ResumeMarkerPath returns the absolute path to the marker file for a
// given base .heimdall_db directory. Callers typically pass the outer
// project's baseDir; sub-repos do not get their own markers.
func ResumeMarkerPath(baseDir string) string {
	return filepath.Join(baseDir, resumeMarkerFilename)
}

// WriteResumeMarker writes a new marker at <baseDir>/.indexing-in-progress
// with status="running". It creates baseDir if needed (so the very first
// `heimdall-mcp index` on a fresh project still gets a marker without the
// caller having to pre-stamp the directory).
//
// StartTime is normalised to UTC so the file is portable between
// machines with different local clocks — the user only sees the
// formatted timestamp, so UTC vs local doesn't matter for them, but it
// keeps the JSON deterministic for tests.
func WriteResumeMarker(baseDir string, models []string, startTime time.Time) error {
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return fmt.Errorf("create marker dir: %w", err)
	}
	m := ResumeMarker{
		StartTime: startTime.UTC(),
		Models:    append([]string{}, models...), // defensive copy
		Status:    "running",
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal marker: %w", err)
	}
	// Best-effort atomic write: write to a tempfile in the same dir, then
	// rename. os.Rename is atomic on POSIX within a single filesystem. If
	// the rename fails we fall back to the temp file's contents being
	// orphaned — acceptable, the marker path itself is unchanged.
	tmp := filepath.Join(baseDir, resumeMarkerFilename+".tmp")
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("write marker tmp: %w", err)
	}
	if err := os.Rename(tmp, ResumeMarkerPath(baseDir)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename marker: %w", err)
	}
	return nil
}

// ReadResumeMarker reads and parses the marker at
// <baseDir>/.indexing-in-progress. Returns (nil, nil) if the file does
// not exist — "no marker" is not an error. Corrupt markers return a
// non-nil error so callers can surface them (e.g. the CLI prompt will
// fall through to the normal flow and let the user pick a model).
func ReadResumeMarker(baseDir string) (*ResumeMarker, error) {
	data, err := os.ReadFile(ResumeMarkerPath(baseDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read marker: %w", err)
	}
	var m ResumeMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse marker: %w", err)
	}
	return &m, nil
}

// DeleteResumeMarker removes the marker. Returns nil if the file does
// not exist (i.e. we already succeeded in clearing it) so callers don't
// need to double-check.
func DeleteResumeMarker(baseDir string) error {
	err := os.Remove(ResumeMarkerPath(baseDir))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete marker: %w", err)
	}
	return nil
}
