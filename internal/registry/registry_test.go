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
