package registry

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
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

// withRegistryDir points XDG_CONFIG_HOME at a temp dir for the duration
// of t so registryPath() is sandboxed. Returns the directory that will
// hold projects.json.
func withRegistryDir(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	return filepath.Join(tmp, "heimdall-mcp")
}

// TestLoadRegistry_LogsOnCorruptJSON pins the log-surface contract added
// for the PR #73 re-review "nice-to-have" gap. A corrupt projects.json
// must (1) log a diagnostic, (2) reset Projects to nil so downstream
// resolvers don't see partial garbage, and (3) not panic.
//
// Before this fix the reader path silently returned an empty registry,
// which masked real state loss.
func TestLoadRegistry_LogsOnCorruptJSON(t *testing.T) {
	dir := withRegistryDir(t)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	regPath := filepath.Join(dir, "projects.json")
	// Malformed JSON — not a valid object.
	if err := os.WriteFile(regPath, []byte("{not-json"), 0644); err != nil {
		t.Fatalf("seed corrupt registry: %v", err)
	}

	// Capture log output.
	var buf bytes.Buffer
	origOut := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(origOut)
		log.SetFlags(origFlags)
	}()

	r := LoadRegistry()
	if r == nil {
		t.Fatalf("LoadRegistry returned nil on corrupt file (must return an empty registry, not nil)")
	}
	if len(r.Projects) != 0 {
		t.Errorf("Projects len = %d, want 0 (must reset after parse failure); got %+v", len(r.Projects), r.Projects)
	}
	got := buf.String()
	if !strings.Contains(got, "corrupt") {
		t.Errorf("log output did not mention corruption: %q", got)
	}
	if !strings.Contains(got, regPath) {
		t.Errorf("log output did not include registry path %q: %q", regPath, got)
	}
}

// TestRegistry_Save_Atomic_NoTempFileLeftOnSuccess — after a successful
// Save, the only file in the registry directory must be projects.json.
// No temp-file leaks.
func TestRegistry_Save_Atomic_NoTempFileLeftOnSuccess(t *testing.T) {
	dir := withRegistryDir(t)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	r := LoadRegistry()
	r.Register("alpha", "/tmp/alpha", "/tmp/alpha/.heimdall_db")
	if err := r.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if name == "projects.json" {
			continue
		}
		if strings.HasSuffix(name, ".tmp") || strings.Contains(name, "projects-") {
			t.Errorf("temp file leaked after successful Save: %s", name)
		}
	}

	// Content round-trip check — file is valid JSON.
	data, err := os.ReadFile(filepath.Join(dir, "projects.json"))
	if err != nil {
		t.Fatalf("read projects.json: %v", err)
	}
	var parsed Registry
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("projects.json not valid JSON after Save: %v (%q)", err, data)
	}
	if len(parsed.Projects) != 1 || parsed.Projects[0].Name != "alpha" {
		t.Errorf("Save did not round-trip: %+v", parsed.Projects)
	}
}

// TestRegistry_Save_Atomic_NoTempLeakOnRenameFailure — simulates a
// rename failure by making the final path be an existing directory
// (os.Rename into a directory fails). The temp file must be cleaned
// up regardless.
//
// This exercises the deferred-cleanup branch in Save() and pins the
// invariant that failure paths don't leak temps.
func TestRegistry_Save_Atomic_NoTempLeakOnRenameFailure(t *testing.T) {
	dir := withRegistryDir(t)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	r := LoadRegistry()
	r.Register("alpha", "/tmp/alpha", "/tmp/alpha/.heimdall_db")

	// Block the target path: create a directory where projects.json
	// should live. os.Rename of a regular file onto a non-empty dir
	// fails on Linux.
	blocker := filepath.Join(dir, "projects.json")
	if err := os.MkdirAll(blocker, 0755); err != nil {
		t.Fatalf("mkdir blocker: %v", err)
	}
	// Put a file in it so rename to the directory name fails on all
	// POSIX filesystems (empty dir could swap on some).
	if err := os.WriteFile(filepath.Join(blocker, "sentinel"), []byte("x"), 0644); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}

	if err := r.Save(); err == nil {
		t.Fatalf("Save succeeded unexpectedly — blocker directory should have made rename fail")
	}

	// Assert no temp files remain in the registry dir.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		// The blocker dir we created is expected; anything else that
		// looks temp is a leak.
		if name == "projects.json" {
			continue
		}
		if strings.HasPrefix(name, "projects-") && strings.HasSuffix(name, ".tmp") {
			t.Errorf("temp file leaked after failed Save: %s", name)
		}
	}
}
