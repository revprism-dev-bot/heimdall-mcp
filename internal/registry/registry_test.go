package registry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestRegister_DedupesByTuple verifies the NEW contract: dedupe key is the
// (Name, Path) tuple. A second Register call with the same tuple
// overwrites in-place; a different tuple appends.
func TestRegister_DedupesByTuple(t *testing.T) {
	r := &Registry{}
	r.Register("frontend", "/proj-a/frontend", "/proj-a/frontend/.heimdall_db")
	r.Register("frontend", "/proj-a/frontend", "/proj-a/frontend/.heimdall_db-v2")
	if got := len(r.All()); got != 1 {
		t.Fatalf("len = %d, want 1 (dedupe on (name,path) tuple match)", got)
	}
	if r.All()[0].DBPath != "/proj-a/frontend/.heimdall_db-v2" {
		t.Errorf("DBPath not updated on re-register: %s", r.All()[0].DBPath)
	}
}

// TestRegistry_TupleDedupe_AllowsSharedNamesAtDistinctPaths (D-03 / A-L-3).
// Two sub-repos in different outer projects that share a basename must
// end up as two distinct entries under the new (Name, Path) tuple
// dedupe. The old Name OR Path rule collapsed them into one entry and
// clobbered the first.
func TestRegistry_TupleDedupe_AllowsSharedNamesAtDistinctPaths(t *testing.T) {
	r := &Registry{}
	r.Register("frontend", "/a/frontend", "/a/frontend/.heimdall_db")
	r.Register("frontend", "/b/frontend", "/b/frontend/.heimdall_db")
	all := r.All()
	if len(all) != 2 {
		t.Fatalf("len = %d, want 2 (distinct paths → distinct entries); got %+v", len(all), all)
	}
	paths := map[string]bool{}
	for _, e := range all {
		paths[e.Path] = true
	}
	if !paths["/a/frontend"] || !paths["/b/frontend"] {
		t.Errorf("expected both /a/frontend and /b/frontend, got %v", paths)
	}
}

// TestRegister_SamePathDifferentName appends because the tuple differs.
// The old contract collapsed on Path-match and silently renamed — callers
// that switched names now get a fresh entry (safer default).
func TestRegister_SamePathDifferentName(t *testing.T) {
	r := &Registry{}
	r.Register("original-name", "/a/frontend", "/a/frontend/.heimdall_db")
	r.Register("new-name", "/a/frontend", "/a/frontend/.heimdall_db")
	if got := len(r.All()); got != 2 {
		t.Fatalf("len = %d, want 2 (distinct names → distinct entries)", got)
	}
}

// TestRegistry_Register_RejectsReservedSentinelName (R-v2-1). Sub-repo names
// matching ^__[a-z]+__$ (e.g. __root__) are reserved for filter sentinels
// and are silently rejected at Register time.
func TestRegistry_Register_RejectsReservedSentinelName(t *testing.T) {
	r := &Registry{}
	r.Register("__root__", "/a/root", "/a/root/.heimdall_db")
	if got := len(r.All()); got != 0 {
		t.Errorf("registered reserved name __root__; got %d entries, want 0", got)
	}

	// Non-sentinel double-underscore names are allowed.
	r.Register("__CamelCase__", "/a/case", "/a/case/.heimdall_db") // uppercase → allowed
	if got := len(r.All()); got != 1 {
		t.Errorf("non-sentinel __CamelCase__ rejected; got %d entries, want 1", got)
	}

	// Regular names are allowed.
	r.Register("my-repo", "/a/repo", "/a/repo/.heimdall_db")
	if got := len(r.All()); got != 2 {
		t.Errorf("ordinary name rejected; got %d entries, want 2", got)
	}
}

// TestRegistry_RepairStaleDbPathWhenCanonicalExists — D-02 (A-L-5). Load a
// registry with an entry whose DBPath != canonical <path>/.heimdall_db
// AND the canonical location exists on disk. LoadRegistry must rewrite
// DBPath to canonical without deleting the entry.
func TestRegistry_RepairStaleDbPathWhenCanonicalExists(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	// Fake two project dirs, each with their own canonical .heimdall_db.
	projA := filepath.Join(tmp, "proj-a")
	canonicalA := filepath.Join(projA, ".heimdall_db")
	if err := os.MkdirAll(canonicalA, 0755); err != nil {
		t.Fatal(err)
	}
	projB := filepath.Join(tmp, "proj-b")
	canonicalB := filepath.Join(projB, ".heimdall_db")
	if err := os.MkdirAll(canonicalB, 0755); err != nil {
		t.Fatal(err)
	}

	// Write a registry with proj-b's DBPath pointing at proj-a's
	// .heimdall_db (the observed stale-fingerprint pattern) and proj-a
	// pointing at its own canonical (valid).
	regFile := filepath.Join(tmp, "heimdall-mcp", "projects.json")
	if err := os.MkdirAll(filepath.Dir(regFile), 0755); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{
		"projects": []map[string]string{
			{"name": "proj-a", "path": projA, "dbPath": canonicalA},
			{"name": "proj-b", "path": projB, "dbPath": canonicalA}, // STALE
		},
	}
	data, _ := json.Marshal(payload)
	if err := os.WriteFile(regFile, data, 0644); err != nil {
		t.Fatal(err)
	}

	r := LoadRegistry()
	all := r.All()
	if len(all) != 2 {
		t.Fatalf("len = %d, want 2 (repair MUST NOT delete entries)", len(all))
	}
	found := false
	for _, e := range all {
		if e.Name == "proj-b" {
			found = true
			if e.DBPath != canonicalB {
				t.Errorf("proj-b DBPath = %q, want %q (canonical) after repair", e.DBPath, canonicalB)
			}
		}
	}
	if !found {
		t.Errorf("proj-b entry disappeared after repair")
	}
}

// TestRegistry_Repair_LeavesOffPathDbPathAloneWhenCanonicalAbsent (A-R-CON-2).
// If DBPath is non-canonical BUT the canonical location does NOT exist,
// the repair must leave the entry as-is (no silent rewrite of a dbPath the
// user may have configured intentionally on a different filesystem).
func TestRegistry_Repair_LeavesOffPathDbPathAloneWhenCanonicalAbsent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	proj := filepath.Join(tmp, "proj")
	if err := os.MkdirAll(proj, 0755); err != nil {
		t.Fatal(err)
	}
	// Do NOT create <proj>/.heimdall_db — canonical absent.

	custom := filepath.Join(tmp, "custom-store")
	if err := os.MkdirAll(custom, 0755); err != nil {
		t.Fatal(err)
	}

	regFile := filepath.Join(tmp, "heimdall-mcp", "projects.json")
	if err := os.MkdirAll(filepath.Dir(regFile), 0755); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{
		"projects": []map[string]string{
			{"name": "proj", "path": proj, "dbPath": custom},
		},
	}
	data, _ := json.Marshal(payload)
	if err := os.WriteFile(regFile, data, 0644); err != nil {
		t.Fatal(err)
	}

	r := LoadRegistry()
	all := r.All()
	if len(all) != 1 {
		t.Fatalf("entry count = %d, want 1", len(all))
	}
	if all[0].DBPath != custom {
		t.Errorf("DBPath rewritten to %q (canonical absent — must leave %q alone)", all[0].DBPath, custom)
	}
}

// TestRegistry_RepairNeverRemovesEntries — D-08 I12. Repair may rewrite
// DBPath, never remove entries.
func TestRegistry_RepairNeverRemovesEntries(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	regFile := filepath.Join(tmp, "heimdall-mcp", "projects.json")
	if err := os.MkdirAll(filepath.Dir(regFile), 0755); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{
		"projects": []map[string]string{
			{"name": "gone-proj", "path": "/definitely/not/here", "dbPath": "/definitely/not/here/.heimdall_db"},
		},
	}
	data, _ := json.Marshal(payload)
	if err := os.WriteFile(regFile, data, 0644); err != nil {
		t.Fatal(err)
	}

	r := LoadRegistry()
	if len(r.All()) != 1 {
		t.Errorf("entry count = %d, want 1 (repair must not delete)", len(r.All()))
	}
}
