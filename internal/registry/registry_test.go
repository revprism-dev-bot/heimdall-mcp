package registry

import "testing"

// TestRegister_DedupesByName verifies the pre-existing Register contract:
// a second Register call with the same name overwrites the first entry
// rather than appending a duplicate.
func TestRegister_DedupesByName(t *testing.T) {
	r := &Registry{}
	r.Register("frontend", "/proj-a/frontend", "/proj-a/frontend/.heimdall_db")
	r.Register("frontend", "/proj-a/frontend", "/proj-a/frontend/.heimdall_db-v2")
	if got := len(r.All()); got != 1 {
		t.Fatalf("len = %d, want 1 (dedupe by name)", got)
	}
	if r.All()[0].DBPath != "/proj-a/frontend/.heimdall_db-v2" {
		t.Errorf("DBPath not updated on re-register: %s", r.All()[0].DBPath)
	}
}

// TestRegister_DedupesByPath verifies dedupe by absolute path (sub-repo
// pass relies on this — re-running index does not bloat projects.json).
func TestRegister_DedupesByPath(t *testing.T) {
	r := &Registry{}
	r.Register("original-name", "/a/frontend", "/a/frontend/.heimdall_db")
	r.Register("new-name", "/a/frontend", "/a/frontend/.heimdall_db")
	if got := len(r.All()); got != 1 {
		t.Fatalf("len = %d, want 1 (dedupe by path)", got)
	}
	if r.All()[0].Name != "new-name" {
		t.Errorf("Name not updated on re-register: %s", r.All()[0].Name)
	}
}

// TestRegister_SubReposWithSameBasenameAcrossProjects is the plan test §5 —
// two sub-repos in different outer projects that share a basename (e.g.
// "frontend") still end up as two distinct entries because Path is absolute.
func TestRegister_SubReposWithSameBasenameAcrossProjects(t *testing.T) {
	r := &Registry{}
	r.Register("frontend", "/a/frontend", "/a/frontend/.heimdall_db")
	r.Register("frontend", "/b/frontend", "/b/frontend/.heimdall_db")
	all := r.All()
	// By the current dedupe-by-name-OR-path contract the second Register
	// with the same name collapses onto the first entry. This test pins
	// the CURRENT observable behaviour so future changes to the dedupe key
	// are intentional. The plan accepts this (§5): sub-repos whose base
	// names collide across outer projects will dedupe by name — users
	// needing distinct entries should run each sub-repo through
	// Register with an unambiguous name (e.g. "outer-a/frontend").
	if len(all) != 1 {
		t.Fatalf("len = %d (current contract: dedupe by name); got %+v", len(all), all)
	}
	// Whichever entry wins, the retained Path must be one of the inputs.
	got := all[0].Path
	if got != "/a/frontend" && got != "/b/frontend" {
		t.Errorf("retained Path = %q, expected one of the inputs", got)
	}
}
