package mcp

// MCP-layer integration tests for the sub-repo indexing pass.
//
// These are plan §10.6 Test 1/2/3 — delivering the regression coverage
// the plan review H1 finding blocked on. They exercise the full
// runIndex path against a fake Ollama httptest server (no network, no
// real embeddings) and assert on the on-disk state after indexing:
//
//   - Registry contains the outer project plus one entry per sub-repo.
//   - Sub-repo git commits land in the SUB-REPO's own store, never in
//     the outer store (regression for the tools.go:362 bug fixed in
//     b70546f).
//   - heimdall_search — scoped to a specific sub-repo — returns content
//     from that sub-repo's own store.
//
// The fake Ollama pattern mirrors the one in internal/cli/integration_test.go
// but lives in-package so we can call s.runIndex directly rather than
// exec-ing the full binary — runIndex is the smallest code path that
// covers the sub-repo fan-out we care about.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
	"github.com/caio-silva/heimdall-mcp/internal/registry"
)

// startFakeOllamaForMCP spins up an httptest server that mimics the
// subset of Ollama the MCP index/search paths hit: /api/tags,
// /api/embed. Returns deterministic fixed-dim vectors so repeated
// queries are stable.
func startFakeOllamaForMCP(t *testing.T, model string, dim int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{{"name": model}},
		})
	})

	// Non-zero deterministic vector keyed by input text so different
	// chunks distinguishably but embeds for the same text match. This
	// avoids the "empty vector" guard in the Ollama client.
	var counter atomic.Int64
	makeVec := func(text string) []float32 {
		v := make([]float32, dim)
		// Base noise so no vector is ever all-zeros.
		for i := range v {
			v[i] = 1.0 / float32(dim)
		}
		// Mix in a text-derived bump on one coordinate so distinct
		// texts get distinguishable vectors.
		h := int64(0)
		for _, c := range text {
			h = h*31 + int64(c)
		}
		slot := int(h) % dim
		if slot < 0 {
			slot += dim
		}
		v[slot] += 1.0
		counter.Add(1)
		return v
	}

	mux.HandleFunc("/api/embed", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input any `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		var vectors [][]float32
		switch in := body.Input.(type) {
		case string:
			vectors = [][]float32{makeVec(in)}
		case []any:
			for _, v := range in {
				if s, ok := v.(string); ok {
					vectors = append(vectors, makeVec(s))
				} else {
					vectors = append(vectors, makeVec(""))
				}
			}
		default:
			vectors = [][]float32{makeVec("")}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": vectors})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// gitInit initialises a git repo at path and makes one commit.
// Sub-repo git indexing requires at least one commit; without it the
// git-indexer returns an error and exercises a less-interesting path.
func gitInit(t *testing.T, path string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=Test",
		"GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@test.com",
	)
	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = path
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v at %s: %v\n%s", args, path, err, out)
		}
	}
	runGit("init")
	runGit("checkout", "-b", "main")
	// Every git repo needs a seed commit so git log --all succeeds. The
	// content is tagged with the repo basename so we can distinguish
	// which repo's commits landed in which store.
	seedFile := filepath.Join(path, "seed.txt")
	if err := os.WriteFile(seedFile, []byte(filepath.Base(path)+" seed"), 0644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	runGit("add", "seed.txt")
	runGit("commit", "-m", fmt.Sprintf("seed: %s initial commit", filepath.Base(path)))
}

// newMCPIntegrationServer builds an in-process Server wired to a fake
// Ollama and an isolated XDG_CONFIG_HOME so registry.Save does not
// clobber the developer's real projects.json.
func newMCPIntegrationServer(t *testing.T, model string, dim int) (*Server, string) {
	t.Helper()
	fake := startFakeOllamaForMCP(t, model, dim)

	// Isolate XDG_CONFIG_HOME so registry writes land in t.TempDir().
	// config.SaveConfig also honours XDG_CONFIG_HOME — we avoid touching
	// the developer's real config.json.
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)

	cfg := config.DefaultConfig()
	cfg.OllamaEndpoint = fake.URL
	cfg.Model = model

	srv := &Server{
		Cfg:      cfg,
		Registry: registry.LoadRegistry(), // empty, will Save under $XDG_CONFIG_HOME
	}
	return srv, fake.URL
}

// seedOuterWithSubRepos prepares an outer project at base/outer with
// two immediate sub-repos — 'alpha' and 'beta' — each with its own
// git history. Returns the outer project's absolute path.
func seedOuterWithSubRepos(t *testing.T, base string) (outer, alpha, beta string) {
	t.Helper()
	outer = filepath.Join(base, "outer")
	if err := os.MkdirAll(outer, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outer, "outer.go"), []byte("package outer\n// outer-unique-token\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitInit(t, outer)

	alpha = filepath.Join(outer, "alpha")
	if err := os.MkdirAll(alpha, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(alpha, "alpha.go"), []byte("package alpha\n// alpha-unique-token\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitInit(t, alpha)

	beta = filepath.Join(outer, "beta")
	if err := os.MkdirAll(beta, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beta, "beta.go"), []byte("package beta\n// beta-unique-token\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitInit(t, beta)
	return outer, alpha, beta
}

// runIndexSync drives s.runIndex synchronously (in the calling goroutine)
// after seeding s.Index the way toolIndex would. This makes the test
// deterministic — no goroutine polling, no time-dependent assertions.
func runIndexSync(t *testing.T, s *Server, absPath string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	s.Index.Mu.Lock()
	s.Index.Running = true
	s.Index.Path = absPath
	s.Index.Current = 0
	s.Index.Total = 0
	s.Index.StartedAt = time.Now()
	s.Index.LastUpdate = time.Now()
	s.Index.Result = nil
	s.Index.Err = nil
	s.Index.Cancel = cancel
	s.Index.Mu.Unlock()

	s.runIndex(ctx, absPath)

	s.Index.Mu.Lock()
	defer s.Index.Mu.Unlock()
	if s.Index.Err != nil {
		t.Fatalf("runIndex returned error: %v", s.Index.Err)
	}
	if s.Index.Result == nil {
		t.Fatalf("runIndex returned nil result")
	}
}

// TestMCPIndex_RegistersSubRepos — plan §10.6 Test 1 — asserts that
// after heimdall_index on a parent with immediate sub-repos, the
// registry ends up with ONE entry per project (outer + each sub).
func TestMCPIndex_RegistersSubRepos(t *testing.T) {
	const model = "nomic-embed-text"
	const dim = 4

	base := t.TempDir()
	outer, alpha, beta := seedOuterWithSubRepos(t, base)

	srv, _ := newMCPIntegrationServer(t, model, dim)
	t.Chdir(outer)

	runIndexSync(t, srv, outer)

	projects := srv.Registry.All()
	if len(projects) != 3 {
		var names []string
		for _, p := range projects {
			names = append(names, p.Name)
		}
		t.Fatalf("Registry entries = %d (%v), want 3 (outer + alpha + beta)", len(projects), names)
	}

	foundPaths := map[string]bool{}
	for _, p := range projects {
		foundPaths[p.Path] = true
	}
	for _, want := range []string{outer, alpha, beta} {
		if !foundPaths[want] {
			t.Errorf("Registry missing entry for %s (have: %v)", want, foundPaths)
		}
	}
}

// TestMCPIndex_SubRepoGitCommitsInSubRepoStore — plan §10.6 Test 2 —
// is the regression test for the tools.go:362 bug (pre-PR #67, sub-repo
// git commits were being written to the OUTER store). The fix opens a
// per-sub-repo store for the git-commit indexing pass. Here we verify:
//
//   - sub-repo commits are present in the sub-repo's own store
//   - those same commits are NOT present in the outer store
func TestMCPIndex_SubRepoGitCommitsInSubRepoStore(t *testing.T) {
	const model = "nomic-embed-text"
	const dim = 4

	base := t.TempDir()
	outer, alpha, _ := seedOuterWithSubRepos(t, base)

	srv, _ := newMCPIntegrationServer(t, model, dim)
	t.Chdir(outer)

	runIndexSync(t, srv, outer)

	// Helper: count records with FilePath == "git:commit" in a store.
	countCommits := func(t *testing.T, dbDir string) int {
		t.Helper()
		store, err := heimdall.OpenStore(dbDir)
		if err != nil {
			t.Fatalf("open store %s: %v", dbDir, err)
		}
		defer store.Close()
		// SearchFiltered with sourceType="commit" returns every commit chunk.
		results := store.SearchFiltered(context.Background(), make([]float32, dim), 0, "commit", "", nil)
		return len(results)
	}

	outerDB := heimdall.ModelDBDir(filepath.Join(outer, ".heimdall_db"), model)
	alphaDB := heimdall.ModelDBDir(filepath.Join(alpha, ".heimdall_db"), model)

	// The outer store should contain exactly the OUTER repo's commits —
	// one seed commit per gitInit(). If the fix regresses, this number
	// will be 3 (outer + alpha + beta) instead of 1.
	outerCommits := countCommits(t, outerDB)
	if outerCommits != 1 {
		t.Errorf("outer store commit count = %d, want 1 (regression: sub-repo commits leaked into outer store)", outerCommits)
	}

	// The sub-repo store should contain its OWN commit.
	alphaCommits := countCommits(t, alphaDB)
	if alphaCommits != 1 {
		t.Errorf("alpha sub-repo store commit count = %d, want 1 (sub-repo commit not indexed in its own store)", alphaCommits)
	}
}

// TestMCPIndex_SearchFindsSubRepoContent — plan §10.6 Test 3 —
// end-to-end confirmation that after heimdall_index creates the
// separate sub-repo databases, heimdall_search (scoped via the
// project argument to the sub-repo) can surface content from the
// sub-repo's own store. We use the filtered-search path directly to
// avoid the Ollama client dependency in toolSearch's standard path.
func TestMCPIndex_SearchFindsSubRepoContent(t *testing.T) {
	const model = "nomic-embed-text"
	const dim = 4

	base := t.TempDir()
	outer, alpha, _ := seedOuterWithSubRepos(t, base)

	srv, _ := newMCPIntegrationServer(t, model, dim)
	t.Chdir(outer)

	runIndexSync(t, srv, outer)

	// Open the sub-repo's store directly (what toolSearch would do
	// after resolveAnyModelDB(alpha)) and search for a token that
	// appears ONLY in the alpha sub-repo's files. A successful hit
	// proves the sub-repo's content was indexed into its own store
	// and is discoverable via a query.
	alphaDB := heimdall.ModelDBDir(filepath.Join(alpha, ".heimdall_db"), model)
	store, err := heimdall.OpenStore(alphaDB)
	if err != nil {
		t.Fatalf("open alpha sub-repo store: %v", err)
	}
	defer store.Close()

	// Any query vector works — we filter by file path on the results.
	queryVec := make([]float32, dim)
	for i := range queryVec {
		queryVec[i] = 1.0 / float32(dim)
	}
	results := store.Search(context.Background(), queryVec, 0)

	var found bool
	for _, r := range results {
		if r.Record.FilePath == "alpha.go" {
			found = true
			break
		}
	}
	if !found {
		var paths []string
		for _, r := range results {
			paths = append(paths, r.Record.FilePath)
		}
		t.Errorf("alpha sub-repo store did not contain alpha.go (have: %v)", paths)
	}

	// Negative check: alpha.go MUST NOT appear in the outer store —
	// that would mean sub-repo files leaked.
	outerDB := heimdall.ModelDBDir(filepath.Join(outer, ".heimdall_db"), model)
	outerStore, err := heimdall.OpenStore(outerDB)
	if err != nil {
		t.Fatalf("open outer store: %v", err)
	}
	defer outerStore.Close()
	if outerStore.HasFile("alpha/alpha.go") || outerStore.HasFile("alpha.go") {
		t.Error("outer store contains alpha/alpha.go — sub-repo files leaked into outer")
	}
}
