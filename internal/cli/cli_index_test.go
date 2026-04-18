package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

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
