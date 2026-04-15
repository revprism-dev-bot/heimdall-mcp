package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
	"github.com/caio-silva/heimdall-mcp/internal/registry"
)

// fakeOllama stands up a minimal Ollama REST stub that answers /api/tags
// and the ping endpoint so Ping() + ListModels() succeed.
func fakeOllama(t *testing.T, models []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.Header().Set("Content-Type", "application/json")
			entries := make([]map[string]string, 0, len(models))
			for _, m := range models {
				entries = append(entries, map[string]string{"name": m})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"models": entries})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
}

func testStatusDeps(t *testing.T, srvURL, model, baseDir string) StatusDeps {
	t.Helper()
	return StatusDeps{
		LoadConfig: func() config.Config {
			return config.Config{
				OllamaEndpoint:  srvURL,
				Model:           model,
				ExcludePatterns: []string{".git"},
			}
		},
		LoadRegistry: func() *registry.Registry { return &registry.Registry{} },
		NewClient:    func(endpoint string) *heimdall.OllamaClient { return heimdall.NewOllamaClient(endpoint) },
		BaseDirOverride: baseDir,
	}
}

func TestCLIStatus_TextOfflineNoIndex(t *testing.T) {
	baseDir := t.TempDir()
	deps := StatusDeps{
		LoadConfig: func() config.Config {
			return config.Config{OllamaEndpoint: "http://127.0.0.1:0", Model: "nomic-embed-text"}
		},
		LoadRegistry:    func() *registry.Registry { return &registry.Registry{} },
		NewClient:       func(e string) *heimdall.OllamaClient { return heimdall.NewOllamaClient(e) },
		BaseDirOverride: baseDir,
	}

	var stdout, stderr strings.Builder
	code := CLIStatus(strings.NewReader(""), &stdout, &stderr, nil, nil, deps)
	if code != 0 {
		t.Fatalf("exit %d stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	// Byte-exact prefix lines verifying we preserved the original output.
	wantSubs := []string{
		"Ollama: http://127.0.0.1:0\n",
		"Model:  nomic-embed-text\n",
		"  Status: offline\n",
		"No index found for model",
	}
	for _, s := range wantSubs {
		if !strings.Contains(out, s) {
			t.Errorf("text output missing %q\nfull output:\n%s", s, out)
		}
	}
}

func TestCLIStatus_TextOnlineModelAvailable(t *testing.T) {
	srv := fakeOllama(t, []string{"nomic-embed-text:latest"})
	defer srv.Close()

	baseDir := t.TempDir()
	deps := testStatusDeps(t, srv.URL, "nomic-embed-text", baseDir)

	var stdout, stderr strings.Builder
	code := CLIStatus(strings.NewReader(""), &stdout, &stderr, nil, nil, deps)
	if code != 0 {
		t.Fatalf("exit %d stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "  Status: running\n") {
		t.Errorf("expected running status, got:\n%s", out)
	}
	if !strings.Contains(out, "(available)") {
		t.Errorf("expected model marked available, got:\n%s", out)
	}
}

// TEST-A-003: golden file for status JSON shape.
//
// Uses a deterministic offline configuration:
//   - endpoint pointing at a closed port (Ollama offline)
//   - fixed model name
//   - empty baseDir (no index yet)
//   - no registered projects
//
// Compares the full JSON document (canonicalized via re-marshal in sorted key
// order) against internal/cli/testdata/status.golden.json. The golden file
// uses {{BASE_DIR}} and {{MODEL_DB_DIR}} placeholders which the test fills in
// with the tempdir values before comparison.
func TestCLIStatus_JSONFormat(t *testing.T) {
	baseDir := t.TempDir()
	deps := StatusDeps{
		LoadConfig: func() config.Config {
			return config.Config{
				OllamaEndpoint: "http://127.0.0.1:0",
				Model:          "nomic-embed-text",
			}
		},
		LoadRegistry:    func() *registry.Registry { return &registry.Registry{} },
		NewClient:       func(e string) *heimdall.OllamaClient { return heimdall.NewOllamaClient(e) },
		BaseDirOverride: baseDir,
	}

	var stdout, stderr strings.Builder
	code := CLIStatus(strings.NewReader(""), &stdout, &stderr, nil,
		[]string{"--format", "json"}, deps)
	if code != 0 {
		t.Fatalf("exit %d stderr=%q", code, stderr.String())
	}

	// Parse actual output.
	var got heimdall.StatusInfo
	if err := json.Unmarshal([]byte(stdout.String()), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout.String())
	}

	// Load golden, substitute placeholders, parse expected.
	goldenPath := filepath.Join("testdata", "status.golden.json")
	goldenBytes, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	modelDBDir := heimdall.ModelDBDir(baseDir, "nomic-embed-text")
	goldenStr := string(goldenBytes)
	goldenStr = strings.ReplaceAll(goldenStr, "{{BASE_DIR}}", baseDir)
	goldenStr = strings.ReplaceAll(goldenStr, "{{MODEL_DB_DIR}}", modelDBDir)

	var want heimdall.StatusInfo
	if err := json.Unmarshal([]byte(goldenStr), &want); err != nil {
		t.Fatalf("invalid golden JSON after substitution: %v\n%s", err, goldenStr)
	}

	// Canonicalize both via Marshal with a stable struct — json.Marshal on a
	// struct emits fields in declaration order, so equal structs produce
	// byte-identical output.
	gotBytes, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("remarshal got: %v", err)
	}
	wantBytes, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("remarshal want: %v", err)
	}
	if !bytes.Equal(gotBytes, wantBytes) {
		t.Errorf("status JSON shape mismatch:\n got:  %s\n want: %s", gotBytes, wantBytes)
	}
}

func TestCLIStatus_InvalidFormat(t *testing.T) {
	deps := StatusDeps{
		LoadConfig:   func() config.Config { return config.Config{} },
		LoadRegistry: func() *registry.Registry { return &registry.Registry{} },
		NewClient:    func(e string) *heimdall.OllamaClient { return heimdall.NewOllamaClient(e) },
	}
	var stdout, stderr strings.Builder
	code := CLIStatus(strings.NewReader(""), &stdout, &stderr, nil,
		[]string{"--format", "xml"}, deps)
	if code != 2 {
		t.Errorf("expected exit 2, got %d", code)
	}
	if !strings.Contains(stderr.String(), "invalid --format") {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}
}

func TestCLIStatus_OutFlagRouted(t *testing.T) {
	baseDir := t.TempDir()
	deps := StatusDeps{
		LoadConfig: func() config.Config {
			return config.Config{OllamaEndpoint: "http://127.0.0.1:0", Model: "m"}
		},
		LoadRegistry: func() *registry.Registry { return &registry.Registry{} },
		NewClient:    func(e string) *heimdall.OllamaClient { return heimdall.NewOllamaClient(e) },
	}
	var stdout, stderr strings.Builder
	code := CLIStatus(strings.NewReader(""), &stdout, &stderr, nil,
		[]string{"--out", baseDir, "--format", "json"}, deps)
	if code != 0 {
		t.Fatalf("exit %d stderr=%q", code, stderr.String())
	}
	var info heimdall.StatusInfo
	if err := json.Unmarshal([]byte(stdout.String()), &info); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if info.BaseDir != baseDir {
		t.Errorf("baseDir=%q, want %q", info.BaseDir, baseDir)
	}
}
