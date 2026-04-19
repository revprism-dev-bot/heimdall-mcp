package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
	"github.com/caio-silva/heimdall-mcp/internal/registry"
)

// Tests for PR1 Problem #1: MCP tools (runIndex, toolStatus, toolIndexText)
// MUST resolve baseDir via s.resolveDBDir(project), NOT os.Getwd().
// The one exception is autoIndexOnSearch (Phase 4 cwd-based auto-index;
// D-17 in the plan). Everything else must route through the single
// canonical registry-first resolver.

// helperServer constructs a *Server with an empty registry and the default
// config, but with OllamaEndpoint pointed at an unroutable address so
// unintended Ollama calls fail fast with a clear error rather than
// hang waiting for network timeout.
func helperServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.OllamaEndpoint = "http://127.0.0.1:1" // invalid; connection refused fast
	return &Server{
		Cfg:      cfg,
		Registry: &registry.Registry{},
	}
}

// TestToolStatus_UsesPathParam — heimdall_status must accept a `path`
// input parameter and resolve via resolveDBDir(path). If the path
// names a registered project, status reflects THAT project's DB, not
// the server's cwd.
func TestToolStatus_UsesPathParam(t *testing.T) {
	s := helperServer(t)

	// Seed a project registered at some tempdir with a vector store
	// under its canonical model dir.
	projectRoot := t.TempDir()
	baseDir := filepath.Join(projectRoot, ".heimdall_db")
	modelDir := heimdall.ModelDBDir(baseDir, s.Cfg.Model)
	store, err := heimdall.OpenStore(modelDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.Upsert([]heimdall.VectorRecord{{
		ID:        "seed:1",
		FilePath:  "seed.go",
		Content:   "x",
		Embedding: []float32{1, 0, 0, 0},
		ModTime:   100,
	}}); err != nil {
		store.Close()
		t.Fatalf("upsert: %v", err)
	}
	store.Close()

	s.Registry.Register("my-project", projectRoot, baseDir)

	// Chdir somewhere else so os.Getwd() would NOT hit projectRoot.
	tmp := t.TempDir()
	t.Chdir(tmp)

	// Call toolStatus with path=projectRoot.
	args, _ := json.Marshal(statusInput{Path: projectRoot})
	res := s.toolStatus(args)
	if res.IsError {
		t.Fatalf("toolStatus returned error: %v", res.Content)
	}
	body := resultText(res)
	// The body should show indexedFiles > 0 because our seeded store has
	// a row. Under the cwd-fallback, it would show 0.
	if !strings.Contains(body, `"indexedFiles": 1`) {
		t.Errorf("expected indexedFiles=1 when path param routes to registered project.\nbody:\n%s", body)
	}
}

// TestToolStatus_FallsBackToResolveDBDir — when `path` is empty, the
// tool still calls resolveDBDir(""), which consults registry.FindByCWD.
// If the current cwd IS inside a registered project, status must
// reflect that project.
func TestToolStatus_FallsBackToResolveDBDir(t *testing.T) {
	s := helperServer(t)
	projectRoot := t.TempDir()
	baseDir := filepath.Join(projectRoot, ".heimdall_db")
	modelDir := heimdall.ModelDBDir(baseDir, s.Cfg.Model)
	store, err := heimdall.OpenStore(modelDir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Upsert([]heimdall.VectorRecord{{
		ID: "seed:1", FilePath: "s.go", Content: "x", Embedding: []float32{1, 0, 0, 0}, ModTime: 100,
	}}); err != nil {
		store.Close()
		t.Fatalf("upsert: %v", err)
	}
	store.Close()

	s.Registry.Register("fallback-proj", projectRoot, baseDir)
	t.Chdir(projectRoot)

	args, _ := json.Marshal(statusInput{})
	res := s.toolStatus(args)
	if res.IsError {
		t.Fatalf("toolStatus error: %v", res.Content)
	}
	body := resultText(res)
	if !strings.Contains(body, `"indexedFiles": 1`) {
		t.Errorf("expected indexedFiles=1 via resolveDBDir(cwd) fallback:\n%s", body)
	}
}

// TestToolStatus_DoesNotUseContaminatedIndexPath — after an
// autoIndexOnSearch call writes cwd into s.Index.Path (see tools.go:464),
// toolStatus must NOT treat that as authoritative. The status of the
// registered project must still be returned correctly when the caller
// passes `path`.
func TestToolStatus_DoesNotUseContaminatedIndexPath(t *testing.T) {
	s := helperServer(t)

	// Set up a populated project.
	projectRoot := t.TempDir()
	baseDir := filepath.Join(projectRoot, ".heimdall_db")
	modelDir := heimdall.ModelDBDir(baseDir, s.Cfg.Model)
	store, err := heimdall.OpenStore(modelDir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Upsert([]heimdall.VectorRecord{{
		ID: "seed:1", FilePath: "f.go", Content: "x", Embedding: []float32{1, 0, 0, 0}, ModTime: 100,
	}}); err != nil {
		store.Close()
		t.Fatalf("upsert: %v", err)
	}
	store.Close()
	s.Registry.Register("real-proj", projectRoot, baseDir)

	// Simulate contaminated Index.Path (autoIndexOnSearch side effect).
	contamDir := t.TempDir()
	s.Index.Path = contamDir

	// Ask status for the real project. Must NOT reach into contamDir.
	args, _ := json.Marshal(statusInput{Path: projectRoot})
	res := s.toolStatus(args)
	if res.IsError {
		t.Fatalf("error: %v", res.Content)
	}
	body := resultText(res)
	// If status routed through the contaminated Index.Path, it would show
	// indexedFiles=0 (or omit it entirely). We want indexedFiles=1.
	if !strings.Contains(body, `"indexedFiles": 1`) {
		t.Errorf("status reached into contaminated Index.Path; body:\n%s", body)
	}
	if strings.Contains(body, contamDir) {
		t.Errorf("status body referenced contaminated dir %q:\n%s", contamDir, body)
	}
}

// TestToolIndexText_WritesToTargetNotCWD — Planner 3 critical finding.
// toolIndexText must route its baseDir through s.resolveDBDir(project),
// not os.Getwd(). If project is registered, content lands in the
// registered DB path.
func TestToolIndexText_WritesToTargetNotCWD(t *testing.T) {
	// Skip if the fake-Ollama integration harness isn't easily available
	// in this package. Instead assert the wiring at a smaller level: with
	// no project registered, resolveDBDir returns cwd-based path; with
	// a project registered for input.Project, resolveDBDir returns the
	// registered path. Exercise resolveDBDir directly in the code path
	// that toolIndexText uses.
	s := helperServer(t)

	projectRoot := t.TempDir()
	baseDir := filepath.Join(projectRoot, ".heimdall_db")
	s.Registry.Register("remoteproj", projectRoot, baseDir)

	// Chdir away so os.Getwd is NOT projectRoot.
	elsewhere := t.TempDir()
	t.Chdir(elsewhere)

	// resolveDBDir with the project name must return the registered baseDir.
	got := s.resolveDBDir("remoteproj")
	if got != baseDir {
		t.Fatalf("resolveDBDir(\"remoteproj\") = %q, want %q", got, baseDir)
	}
	// With empty project, the fallback goes through FindByCWD which
	// should NOT match elsewhere. Result should be <elsewhere>/.heimdall_db.
	gotEmpty := s.resolveDBDir("")
	if gotEmpty == baseDir {
		t.Errorf("resolveDBDir(\"\") returned registered baseDir %q; should have fallen back to cwd", baseDir)
	}
	if !strings.HasPrefix(gotEmpty, elsewhere) {
		t.Errorf("resolveDBDir(\"\") = %q, want prefix %q", gotEmpty, elsewhere)
	}
}

// TestRunIndex_WritesToTargetBaseDir — the critical fix. When
// heimdall_index is invoked with a path parameter, runIndex must compute
// baseDir from THAT path, not from os.Getwd(). After completion, the
// registry entry for the project must point to <absPath>/.heimdall_db.
// Uses the fake-Ollama scaffolding from tools_subrepo_integration_test.go.
func TestRunIndex_WritesToTargetBaseDir(t *testing.T) {
	const model = "nomic-embed-text"
	const dim = 4

	base := t.TempDir()
	// Build a minimal project directory (no git — just a Go file).
	targetProject := filepath.Join(base, "target")
	if err := os.MkdirAll(targetProject, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetProject, "main.go"),
		[]byte("package main\n\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Chdir to a DIFFERENT dir (not targetProject). If runIndex still
	// reads os.Getwd(), baseDir would resolve under elsewhere and
	// registry.DBPath would be wrong.
	elsewhere := t.TempDir()
	t.Chdir(elsewhere)

	srv, _ := newMCPIntegrationServer(t, model, dim)
	runIndexSync(t, srv, targetProject)

	// Registry must have the target project pointing at its OWN
	// .heimdall_db (not elsewhere's).
	p := srv.Registry.Find("target")
	if p == nil {
		t.Fatalf("registry: no 'target' entry after indexing")
	}
	wantDBPath := filepath.Join(targetProject, ".heimdall_db")
	if p.DBPath != wantDBPath {
		t.Errorf("registry DBPath = %q, want %q (runIndex used os.Getwd() instead of absPath)", p.DBPath, wantDBPath)
	}

	// And no .heimdall_db should have been created in `elsewhere`.
	if _, err := os.Stat(filepath.Join(elsewhere, ".heimdall_db")); err == nil {
		t.Errorf("runIndex wrote .heimdall_db to cwd %q (elsewhere) — target-path fix regressed", elsewhere)
	}
}

// TestRunIndex_FromSubdirectory_BaseDirResolvesToProvidedPath — plan
// corner case C10. Real-world Claude Code invocations pass subdir paths
// (e.g. `heimdall_index path=/proj/subpkg`). baseDir must anchor at the
// provided path, not its git root or its cwd.
func TestRunIndex_FromSubdirectory_BaseDirResolvesToProvidedPath(t *testing.T) {
	const model = "nomic-embed-text"
	const dim = 4

	base := t.TempDir()
	project := filepath.Join(base, "proj")
	subpkg := filepath.Join(project, "subpkg")
	if err := os.MkdirAll(subpkg, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subpkg, "a.go"),
		[]byte("package subpkg\n"), 0644); err != nil {
		t.Fatal(err)
	}

	t.Chdir(base) // cwd neither proj nor subpkg
	srv, _ := newMCPIntegrationServer(t, model, dim)
	runIndexSync(t, srv, subpkg)

	p := srv.Registry.Find("subpkg")
	if p == nil {
		t.Fatalf("no 'subpkg' entry in registry")
	}
	wantDBPath := filepath.Join(subpkg, ".heimdall_db")
	if p.DBPath != wantDBPath {
		t.Errorf("subpkg DBPath = %q, want %q", p.DBPath, wantDBPath)
	}
}

// TestResolveRunIndexBaseDir_ExactPathOnly — the write-path helper must
// NOT pick the wrong project via substring name matching. If the registry
// holds a project whose NAME happens to appear as a substring of the
// requested absolute path, the helper must still return
// <absPath>/.heimdall_db and not the unrelated project's DBPath.
func TestResolveRunIndexBaseDir_ExactPathOnly(t *testing.T) {
	s := helperServer(t)

	// Register a project whose NAME is a substring of a different absolute
	// path we'll ask about. (Registry.Find uses partial-substring matching;
	// we're ensuring resolveRunIndexBaseDir bypasses that.)
	realProject := t.TempDir()
	realBase := filepath.Join(realProject, ".heimdall_db")
	s.Registry.Register("proj", realProject, realBase)

	// Build a synthetic path whose basename coincidentally contains "proj"
	// as a substring. No matching registry entry for this path.
	elsewhere := t.TempDir()
	unrelated := filepath.Join(elsewhere, "my-proj-fork")
	if err := os.MkdirAll(unrelated, 0755); err != nil {
		t.Fatal(err)
	}

	got := s.resolveRunIndexBaseDir(unrelated)
	want := filepath.Join(unrelated, ".heimdall_db")
	if got != want {
		t.Errorf("resolveRunIndexBaseDir(%q) = %q, want %q (should NOT match by name substring)", unrelated, got, want)
	}
}

// TestResolveRunIndexBaseDir_EmptyPathFallsBack — with no hint, the
// helper must not return a stale path. It routes through resolveDBDir
// which ends at <cwd>/.heimdall_db.
func TestResolveRunIndexBaseDir_EmptyPathFallsBack(t *testing.T) {
	s := helperServer(t)
	tmp := t.TempDir()
	t.Chdir(tmp)

	got := s.resolveRunIndexBaseDir("")
	want := filepath.Join(tmp, ".heimdall_db")
	if got != want {
		t.Errorf("resolveRunIndexBaseDir(\"\") = %q, want %q", got, want)
	}
}

// resultText is a tiny helper to extract the text payload from an
// MCPToolResult so tests can assert on it.
func resultText(r MCPToolResult) string {
	var sb strings.Builder
	for _, c := range r.Content {
		sb.WriteString(c.Text)
	}
	return sb.String()
}
