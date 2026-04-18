package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// TestMarkerLifecycle_DroppedModelDirStaysOnDisk verifies the contract that
// when a resume marker recorded models [A, B] and the user re-runs the
// resolver and picks only [A], the on-disk subdirectory for B — a partial
// .heimdall_db/B/ left over from the interrupted run — is NOT deleted.
// The user can still resume the dropped model later by re-running and
// re-selecting it.
//
// This is a filesystem-level regression: the fix introduces no cleanup
// codepaths, so the test just asserts the pre-existing subdir survives
// a marker rewrite.
func TestMarkerLifecycle_DroppedModelDirStaysOnDisk(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), ".heimdall_db")
	// Simulate an interrupted run: marker recording two models, plus both
	// their subdirectories populated with a placeholder file each.
	if err := heimdall.WriteResumeMarker(baseDir, []string{"alpha", "beta"}, time.Now()); err != nil {
		t.Fatalf("WriteResumeMarker: %v", err)
	}
	for _, m := range []string{"alpha", "beta"} {
		dir := filepath.Join(baseDir, m)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "vectors.db"), []byte("partial"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// User re-runs; resolver picks only alpha. cliIndex updates the marker
	// to reflect the new selection. We simulate just that step (the real
	// indexWithModel call writes into alpha/ only).
	if err := heimdall.WriteResumeMarker(baseDir, []string{"alpha"}, time.Now()); err != nil {
		t.Fatalf("WriteResumeMarker (update): %v", err)
	}

	// beta/ must still be on disk — we do NOT prune dropped models.
	betaDB := filepath.Join(baseDir, "beta", "vectors.db")
	if _, err := os.Stat(betaDB); err != nil {
		t.Errorf("partial beta DB was removed: %v (should be preserved for later resume)", err)
	}
	// alpha/ should still exist too.
	alphaDB := filepath.Join(baseDir, "alpha", "vectors.db")
	if _, err := os.Stat(alphaDB); err != nil {
		t.Errorf("alpha DB missing: %v", err)
	}
}

// TestSubRepoOpts_PrintsStartLine asserts the CLI-owned SubRepoOpts
// factory emits an "Indexing sub-repo i/N: <name>" line on start so users
// see progress during the sub-repo pass (regression for the silent-minutes
// bug in PR #67).
func TestSubRepoOpts_PrintsStartLine(t *testing.T) {
	var buf bytes.Buffer
	opts := newSubRepoCLIOpts(&buf, time.Now())
	opts.OnSubRepoStart("payments-analyzer-app", 1, 3)
	got := buf.String()
	if !strings.Contains(got, "Indexing sub-repo 1/3: payments-analyzer-app") {
		t.Errorf("OnSubRepoStart output missing expected line.\nGot: %q", got)
	}
}

// TestSubRepoOpts_PrintsDoneSummary asserts that after each sub-repo the
// factory emits a summary line (files, chunks, elapsed).
func TestSubRepoOpts_PrintsDoneSummary(t *testing.T) {
	var buf bytes.Buffer
	opts := newSubRepoCLIOpts(&buf, time.Now())
	opts.OnSubRepoStart("svc", 1, 1)
	opts.OnSubRepoDone(heimdall.SubRepoResult{
		Name: "svc",
		Result: &heimdall.IndexResult{
			FilesIndexed:  12,
			ChunksCreated: 84,
			Duration:      3 * time.Second,
		},
	})
	got := buf.String()
	for _, want := range []string{"Indexing sub-repo 1/1: svc", "12 files", "84 chunks"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q.\nGot:\n%s", want, got)
		}
	}
}

// TestSubRepoOpts_PrintsFailureSummary asserts failed sub-repos emit a
// FAILED line instead of a success summary.
func TestSubRepoOpts_PrintsFailureSummary(t *testing.T) {
	var buf bytes.Buffer
	opts := newSubRepoCLIOpts(&buf, time.Now())
	opts.OnSubRepoStart("bad", 1, 1)
	opts.OnSubRepoDone(heimdall.SubRepoResult{
		Name: "bad",
		Err:  errors.New("boom"),
	})
	got := buf.String()
	if !strings.Contains(got, "FAILED") {
		t.Errorf("expected FAILED marker for error sub-repo, got: %s", got)
	}
}

// TestRenderIndexSummary_CategorizedSkips asserts the new summary format
// splits FilesSkipped into category-labelled indent lines and shows no
// category line when its counter is 0 (plan §G6).
func TestRenderIndexSummary_CategorizedSkips(t *testing.T) {
	r := &heimdall.IndexResult{
		FilesScanned:  100,
		FilesIndexed:  80,
		FilesSkipped:  20,
		ChunksCreated: 400,
		Skip: heimdall.SkipBreakdown{
			UserExcluded: 4,
			SubRepo:      2,
			Binary:       2,
			Unchanged:    12,
		},
	}
	var buf bytes.Buffer
	renderIndexSummary(&buf, "nomic-embed-text", 12*time.Second, "/tmp/db", r, nil)
	out := buf.String()
	for _, want := range []string{
		"Skipped:  20 files",
		"12 unchanged (incremental)",
		"4 excluded by pattern",
		"2 binary",
		"2 sub-repo directories (indexed separately below)",
		"Indexed:  80 files",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q\n---full output---\n%s", want, out)
		}
	}
}

// TestRenderIndexSummary_PinnedAndFirstTime verifies the pinned-model
// bracket notation and the first-time hint emit together when the
// conditions hold.
func TestRenderIndexSummary_PinnedAndFirstTime(t *testing.T) {
	r := &heimdall.IndexResult{FilesScanned: 3, FilesIndexed: 3, ChunksCreated: 9}
	subs := []heimdall.SubRepoResult{
		{
			Path: "/abs/outer/sub-infra", Name: "sub-infra",
			DBPath: "/abs/outer/sub-infra/.heimdall_db",
			Model:  "bge-m3", PinnedModel: true,
			Result: &heimdall.IndexResult{FilesIndexed: 7, ChunksCreated: 32},
		},
		{
			Path: "/abs/outer/sub-new", Name: "sub-new",
			DBPath: "/abs/outer/sub-new/.heimdall_db",
			Model:  "nomic-embed-text", PinnedModel: false,
			Result: &heimdall.IndexResult{FilesIndexed: 12, ChunksCreated: 84},
		},
	}
	var buf bytes.Buffer
	renderIndexSummary(&buf, "nomic-embed-text", 10*time.Second, "/abs/outer/.heimdall_db", r, subs)
	out := buf.String()
	for _, want := range []string{
		"Sub-repos: 2 indexed separately",
		"[bge-m3 — pinned]",
		"[nomic-embed-text]",
		"first indexing of sub-repos may take longer",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q\n---full output---\n%s", want, out)
		}
	}
}

// TestRenderIndexSummary_SubRepoFailure renders a failing sub-repo without
// panicking on the nil Result and surfaces the FAILED tag.
func TestRenderIndexSummary_SubRepoFailure(t *testing.T) {
	r := &heimdall.IndexResult{FilesScanned: 1, FilesIndexed: 1, ChunksCreated: 2}
	subs := []heimdall.SubRepoResult{
		{
			Path: "/abs/outer/sub-broken", Name: "sub-broken",
			DBPath: "/abs/outer/sub-broken/.heimdall_db",
			Err:    errFake("disk full"),
		},
	}
	var buf bytes.Buffer
	renderIndexSummary(&buf, "nomic-embed-text", 5*time.Second, "/abs/outer/.heimdall_db", r, subs)
	out := buf.String()
	if !strings.Contains(out, "sub-broken  FAILED: disk full") {
		t.Errorf("expected FAILED line, got:\n%s", out)
	}
	if !strings.Contains(out, "1 sub-repo(s) failed") {
		t.Errorf("expected failure footer, got:\n%s", out)
	}
}

// errFake is a stringly-typed error used only to keep these unit tests free
// of cross-package dependencies on error-wrapping helpers.
type errFake string

func (e errFake) Error() string { return string(e) }
