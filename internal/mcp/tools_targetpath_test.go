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

// TestToolIndexText_WritesToTargetNotCWD — behavioral test (per PR #73
// tests-reviewer finding): the original test exercised resolveDBDir in
// isolation and never invoked toolIndexText. A future refactor that
// inlined resolveDBDir elsewhere would pass that test while regressing
// the invariant. This replacement drives toolIndexText through a fake
// Ollama server and asserts the indexed content lands inside the
// registered project's DB — NOT cwd.
func TestToolIndexText_WritesToTargetNotCWD(t *testing.T) {
	const model = "nomic-embed-text"
	const dim = 4

	srv, _ := newMCPIntegrationServer(t, model, dim)

	// Register a project at its own tempdir. Use the canonical DBPath
	// shape (<projectRoot>/.heimdall_db) so the resolver returns it
	// verbatim for input.Project.
	projectRoot := t.TempDir()
	baseDir := filepath.Join(projectRoot, ".heimdall_db")
	srv.Registry.Register("remoteproj", projectRoot, baseDir)

	// Chdir away so os.Getwd is NOT projectRoot. A regression that
	// routed through cwd instead of the registered path would write
	// .heimdall_db under elsewhere.
	elsewhere := t.TempDir()
	t.Chdir(elsewhere)

	body, _ := json.Marshal(indexTextInput{
		Content: "indexed content for targeting test",
		Source:  "unit-test",
		Project: "remoteproj",
		Type:    "note",
	})
	res := srv.toolIndexText(body)
	if res.IsError {
		t.Fatalf("toolIndexText returned error: %s", resultText(res))
	}

	// Assert the model-specific dir under the REGISTERED baseDir was
	// created (toolIndexText writes through heimdall.OpenStore →
	// ModelDBDir(baseDir, model)).
	registeredModelDir := heimdall.ModelDBDir(baseDir, model)
	if _, err := os.Stat(filepath.Join(registeredModelDir, "vectors.db")); err != nil {
		t.Errorf("vectors.db missing under registered project %q: %v", registeredModelDir, err)
	}

	// Assert cwd was NOT polluted. The regression signature is a
	// .heimdall_db dir appearing beneath the unrelated cwd.
	if _, err := os.Stat(filepath.Join(elsewhere, ".heimdall_db")); err == nil {
		t.Errorf("toolIndexText wrote .heimdall_db into cwd %q — target-path invariant regressed", elsewhere)
	}
}

// TestToolStatus_DoesNotSubstringMatchRegistry — PR #73 review HIGH
// finding. toolStatus previously routed through resolveDBDir, which
// uses Registry.Find, which does case-insensitive substring matching.
// Asking for path=/srv/auth when a project named `auth` is registered
// at `/srv/auth-service` would return the wrong project's stats.
//
// The fix: when input.Path is an absolute filesystem directory, short
// circuit to <path>/.heimdall_db (matching the resolveRunIndexBaseDir
// exact-match contract). Only fall through to resolveDBDir when the
// input looks like a project name, not a path.
func TestToolStatus_DoesNotSubstringMatchRegistry(t *testing.T) {
	s := helperServer(t)

	// Register a project whose name coincidentally is a substring of
	// the absolute path we will ask about. Registry.Find does
	// case-insensitive substring matching on name, so a naive lookup
	// of path=/srv/auth-service would return this project when the
	// requested path is the unrelated `/srv/auth`.
	//
	// We simulate the inverse: register project `auth` at `realProject`,
	// then ask status for a DIFFERENT absolute path `requestedPath`
	// whose basename happens to contain "auth". toolStatus must return
	// stats for requestedPath's own store, not the registered project's.
	realProject := t.TempDir()
	realBase := filepath.Join(realProject, ".heimdall_db")
	realModelDir := heimdall.ModelDBDir(realBase, s.Cfg.Model)
	realStore, err := heimdall.OpenStore(realModelDir)
	if err != nil {
		t.Fatalf("open real store: %v", err)
	}
	if err := realStore.Upsert([]heimdall.VectorRecord{{
		ID: "real:1", FilePath: "r.go", Content: "real", Embedding: []float32{1, 0, 0, 0}, ModTime: 100,
	}}); err != nil {
		realStore.Close()
		t.Fatalf("upsert real: %v", err)
	}
	realStore.Close()
	s.Registry.Register("auth", realProject, realBase)

	// Build a separate, unrelated absolute directory whose path contains
	// the substring "auth" (mimicking `/srv/auth-v2` when `auth` is
	// registered at `/srv/auth-service`). Its own DB has TWO entries so
	// we can distinguish it from the registered project's store.
	requestedRoot := filepath.Join(t.TempDir(), "auth-v2-experimental")
	if err := os.MkdirAll(requestedRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	requestedBase := filepath.Join(requestedRoot, ".heimdall_db")
	requestedModelDir := heimdall.ModelDBDir(requestedBase, s.Cfg.Model)
	requestedStore, err := heimdall.OpenStore(requestedModelDir)
	if err != nil {
		t.Fatalf("open requested store: %v", err)
	}
	if err := requestedStore.Upsert([]heimdall.VectorRecord{
		{ID: "req:1", FilePath: "a.go", Content: "x", Embedding: []float32{1, 0, 0, 0}, ModTime: 200},
		{ID: "req:2", FilePath: "b.go", Content: "y", Embedding: []float32{0, 1, 0, 0}, ModTime: 200},
	}); err != nil {
		requestedStore.Close()
		t.Fatalf("upsert requested: %v", err)
	}
	requestedStore.Close()

	// Chdir somewhere else so cwd-fallback would miss both.
	t.Chdir(t.TempDir())

	args, _ := json.Marshal(statusInput{Path: requestedRoot})
	res := s.toolStatus(args)
	if res.IsError {
		t.Fatalf("toolStatus error: %s", resultText(res))
	}
	body := resultText(res)
	// The status body must point at requestedBase (the abs-path short
	// circuit), not at realBase (the substring match result).
	if !strings.Contains(body, requestedBase) {
		t.Errorf("expected dbPath %q in status body:\n%s", requestedBase, body)
	}
	if strings.Contains(body, realBase) && !strings.Contains(body, requestedBase) {
		t.Errorf("status body resolved to wrong registered project:\n%s", body)
	}
	// indexedFiles should reflect requestedRoot's store (2 entries → 2
	// files), NOT the registered project's (1 entry → 1 file).
	if !strings.Contains(body, `"indexedFiles": 2`) {
		t.Errorf("expected indexedFiles=2 (requestedRoot's store), got:\n%s", body)
	}
}

// TestToolStatus_AbsolutePathShortCircuits — when input.Path is an
// absolute path to an existing directory, toolStatus must return
// <path>/.heimdall_db WITHOUT consulting Registry.Find. Covers the
// registry-bypass guarantee.
func TestToolStatus_AbsolutePathShortCircuits(t *testing.T) {
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

	// NOT registered. cwd elsewhere.
	t.Chdir(t.TempDir())

	args, _ := json.Marshal(statusInput{Path: projectRoot})
	res := s.toolStatus(args)
	if res.IsError {
		t.Fatalf("toolStatus error: %s", resultText(res))
	}
	body := resultText(res)
	if !strings.Contains(body, baseDir) {
		t.Errorf("expected abs-path short circuit to dbPath=%q; body:\n%s", baseDir, body)
	}
	if !strings.Contains(body, `"indexedFiles": 1`) {
		t.Errorf("expected indexedFiles=1 (unregistered abs-path short circuit); body:\n%s", body)
	}
}

// TestToolStatus_UnregisteredAbsPath_ShortCircuits — covers PR #73
// tests-reviewer finding #8: unregistered absolute paths previously
// silently fell through to `<cwd>/.heimdall_db`, producing misleading
// status. With the fix, an unregistered absolute path that points at
// an existing directory resolves to that path's own `.heimdall_db`
// (even if the DB doesn't yet exist — the status call simply reports
// an empty index at the correct location).
func TestToolStatus_UnregisteredAbsPath_ShortCircuits(t *testing.T) {
	s := helperServer(t)

	// Absolute path exists but has no .heimdall_db yet. cwd elsewhere.
	unregistered := t.TempDir()
	cwd := t.TempDir()
	t.Chdir(cwd)

	args, _ := json.Marshal(statusInput{Path: unregistered})
	res := s.toolStatus(args)
	if res.IsError {
		t.Fatalf("toolStatus error: %s", resultText(res))
	}
	body := resultText(res)

	wantDBPath := filepath.Join(unregistered, ".heimdall_db")
	if !strings.Contains(body, wantDBPath) {
		t.Errorf("expected dbPath=%q (short-circuit from abs path), got:\n%s", wantDBPath, body)
	}
	// Must NOT have routed to cwd-based fallback.
	cwdDBPath := filepath.Join(cwd, ".heimdall_db")
	if strings.Contains(body, cwdDBPath) {
		t.Errorf("status routed to cwd %q instead of short-circuiting to requested path %q:\n%s", cwdDBPath, unregistered, body)
	}
}

// TestBaseDir_CLIAndMCPAgree_ForSameTarget — plan v2 invariant I9 (§8
// merge gate, D-08). The cross-layer contract: indexing the same target
// project through CLI (cli.go:378) and MCP (tools.go:runIndex) must
// produce the SAME baseDir. Both paths must anchor at
// `<target>/.heimdall_db`, not at their respective cwds.
//
// We drive the MCP layer directly (runIndex), and compute the CLI path
// via the same heimdall.ModelDBDir call the CLI uses. Assert equality.
func TestBaseDir_CLIAndMCPAgree_ForSameTarget(t *testing.T) {
	const model = "nomic-embed-text"
	const dim = 4

	base := t.TempDir()
	target := filepath.Join(base, "target-project")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "main.go"),
		[]byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// CLI and MCP must both resolve baseDir from the target path, not
	// from their current working directory.
	elsewhere := t.TempDir()
	t.Chdir(elsewhere)

	srv, _ := newMCPIntegrationServer(t, model, dim)
	runIndexSync(t, srv, target)

	// MCP's resolved baseDir — read from the registry after runIndex.
	p := srv.Registry.Find("target-project")
	if p == nil {
		t.Fatalf("registry has no 'target-project' entry after runIndex")
	}
	mcpBaseDir := p.DBPath

	// What the CLI would compute for the same target: the CLI path also
	// uses <target>/.heimdall_db (cli.go:indexCmd → filepath.Join(target, ".heimdall_db")).
	cliBaseDir := filepath.Join(target, ".heimdall_db")

	if mcpBaseDir != cliBaseDir {
		t.Fatalf("MCP baseDir %q != CLI baseDir %q — cross-layer invariant I9 violated", mcpBaseDir, cliBaseDir)
	}

	// Tighten further: both layers must resolve ModelDBDir to the same
	// model-specific path. Any divergence between the MCP and CLI
	// convention would slip past the basic equality check.
	mcpModelDir := heimdall.ModelDBDir(mcpBaseDir, model)
	cliModelDir := heimdall.ModelDBDir(cliBaseDir, model)
	if mcpModelDir != cliModelDir {
		t.Errorf("MCP modelDir %q != CLI modelDir %q", mcpModelDir, cliModelDir)
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

// TestResolveStatusBaseDir_NormalizesPath — PR #73 re-review N-1.
// Cosmetic variants of an absolute path (`/foo/./`, `/foo/../foo`,
// trailing slash) must reach the same registered entry as the
// canonical cleaned form. Otherwise the exact-match registry-bypass
// contract is soft: a registered project could silently fall through
// to `<input>/.heimdall_db` just because the caller typed `./`.
func TestResolveStatusBaseDir_NormalizesPath(t *testing.T) {
	s := helperServer(t)

	real := t.TempDir()
	base := filepath.Join(real, ".heimdall_db")
	s.Registry.Register("proj", real, base)

	// Each variant must resolve to the registered DBPath — not to
	// `<variant>/.heimdall_db`.
	variants := []string{
		real,                          // canonical (control)
		real + "/",                    // trailing slash
		real + "/.",                   // embedded current-dir
		filepath.Join(real, "sub", ".."), // via ..
	}
	for _, v := range variants {
		got := s.resolveStatusBaseDir(v)
		if got != base {
			t.Errorf("resolveStatusBaseDir(%q) = %q, want %q (should normalize)", v, got, base)
		}
	}
}

// TestResolveRunIndexBaseDir_NormalizesPath — same as the status
// sibling above but for the write-path resolver. Writes MUST land in
// the registered location regardless of cosmetic path variants.
func TestResolveRunIndexBaseDir_NormalizesPath(t *testing.T) {
	s := helperServer(t)

	real := t.TempDir()
	base := filepath.Join(real, ".heimdall_db")
	s.Registry.Register("proj", real, base)

	variants := []string{
		real,
		real + "/",
		real + "/.",
		filepath.Join(real, "sub", ".."),
	}
	for _, v := range variants {
		got := s.resolveRunIndexBaseDir(v)
		if got != base {
			t.Errorf("resolveRunIndexBaseDir(%q) = %q, want %q (should normalize)", v, got, base)
		}
	}
}

// TestResolveStatusBaseDir_NormalizesUnregisteredPath — for an
// unregistered absolute path, the helper returns <Clean(input)>/.heimdall_db
// (not `<raw>/.heimdall_db`). Locks the cleaned-fallback shape so a
// trailing slash in `path` doesn't produce a trailing-slash DBPath that
// would downstream mismatch a later registered entry.
func TestResolveStatusBaseDir_NormalizesUnregisteredPath(t *testing.T) {
	s := helperServer(t)

	dir := t.TempDir()
	got := s.resolveStatusBaseDir(dir + "/")
	want := filepath.Join(dir, ".heimdall_db")
	if got != want {
		t.Errorf("resolveStatusBaseDir(%q) = %q, want %q", dir+"/", got, want)
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
