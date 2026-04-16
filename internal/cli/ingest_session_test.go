package cli

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

func testIngestDeps(store *heimdall.MemoryStore) IngestDeps {
	return IngestDeps{
		LoadConfig: func() config.Config {
			return config.Config{OllamaEndpoint: "http://127.0.0.1:0", Model: "test-model"}
		},
		OpenMemoryStore: func() (*heimdall.MemoryStore, error) { return store, nil },
		NewEmbedder: func(context.Context, string, string) (heimdall.Embedder, error) {
			return &heimdall.StubEmbedder{Dimension: 3}, nil
		},
	}
}

func openIngestStore(t *testing.T) *heimdall.MemoryStore {
	t.Helper()
	dir := t.TempDir()
	store, err := heimdall.OpenMemoryStore(filepath.Join(dir, "memories.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestCLIIngestSession_StdinHappy(t *testing.T) {
	store := openIngestStore(t)
	deps := testIngestDeps(store)

	var stdout, stderr strings.Builder
	code := CLIIngestSession(
		strings.NewReader("We decided to use TDD for all new features."),
		&stdout, &stderr, nil,
		[]string{"--summary-stdin", "--session-id", "sess-42"},
		deps,
	)
	if code != 0 {
		t.Fatalf("exit %d, stderr=%q", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("expected silent stderr, got %q", stderr.String())
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(stdout.String()), &got); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, stdout.String())
	}
	if got["ok"] != true {
		t.Errorf("ok field: %v", got["ok"])
	}
	if got["session_id"] != "sess-42" {
		t.Errorf("session_id: %v", got["session_id"])
	}
}

func TestCLIIngestSession_BufferFile(t *testing.T) {
	store := openIngestStore(t)
	deps := testIngestDeps(store)

	// Build a length-prefixed buffer on disk.
	dir := t.TempDir()
	bufPath := filepath.Join(dir, "buf.log")
	f, err := os.Create(bufPath)
	if err != nil {
		t.Fatal(err)
	}
	payloads := []string{"first turn summary", "second turn summary"}
	for _, p := range payloads {
		var lb [4]byte
		binary.BigEndian.PutUint32(lb[:], uint32(len(p)))
		f.Write(lb[:])
		f.WriteString(p)
	}
	f.Close()

	var stdout, stderr strings.Builder
	code := CLIIngestSession(strings.NewReader(""), &stdout, &stderr, nil,
		[]string{"--buffer", bufPath, "--session-id", "sess"}, deps)
	if code != 0 {
		t.Fatalf("exit %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"ok":true`) {
		t.Errorf("stdout missing ok: %q", stdout.String())
	}
}

func TestCLIIngestSession_UsageErrors(t *testing.T) {
	store := openIngestStore(t)
	deps := testIngestDeps(store)

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"neither", []string{}, "one of --summary-stdin or --buffer"},
		{"both", []string{"--summary-stdin", "--buffer", "/tmp/x"}, "mutually exclusive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr strings.Builder
			code := CLIIngestSession(strings.NewReader(""), &stdout, &stderr, nil, tc.args, deps)
			if code != 2 {
				t.Errorf("expected exit 2, got %d", code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("expected stderr to contain %q, got %q", tc.want, stderr.String())
			}
		})
	}
}

func TestCLIIngestSession_EmptySummary(t *testing.T) {
	store := openIngestStore(t)
	deps := testIngestDeps(store)

	var stdout, stderr strings.Builder
	code := CLIIngestSession(strings.NewReader(""), &stdout, &stderr, nil,
		[]string{"--summary-stdin"}, deps)
	if code != 1 {
		t.Errorf("expected exit 1 for empty summary, got %d", code)
	}
	if !strings.Contains(stderr.String(), "empty summary") {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}
}

func TestCLIIngestSession_MissingBufferFile(t *testing.T) {
	store := openIngestStore(t)
	deps := testIngestDeps(store)

	var stdout, stderr strings.Builder
	code := CLIIngestSession(strings.NewReader(""), &stdout, &stderr, nil,
		[]string{"--buffer", "/nonexistent/path"}, deps)
	if code != 1 {
		t.Errorf("expected exit 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "open buffer") {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}
}

// TEST-A-002: malformed / oversized / zero-length buffer records.
//
// These drive CLIIngestSession through --buffer <path> so we cover the CLI
// wiring in addition to the heimdall.ReadLengthPrefixedBuffer unit tests.
// The expectation in all three cases is no panic.

func TestCLIIngestSession_BufferTruncatedPrefix(t *testing.T) {
	store := openIngestStore(t)
	deps := testIngestDeps(store)

	// Write only 2 of the 4 prefix bytes — hard error expected.
	dir := t.TempDir()
	bufPath := filepath.Join(dir, "buf.log")
	if err := os.WriteFile(bufPath, []byte{0x00, 0x01}, 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr strings.Builder
	code := CLIIngestSession(strings.NewReader(""), &stdout, &stderr, nil,
		[]string{"--buffer", bufPath, "--session-id", "sess"}, deps)
	if code != 1 {
		t.Errorf("expected exit 1 for truncated prefix, got %d", code)
	}
	if !strings.Contains(stderr.String(), "parse buffer") {
		t.Errorf("expected parse error in stderr, got %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("expected silent stdout on error, got %q", stdout.String())
	}
}

func TestCLIIngestSession_BufferOversizedLength(t *testing.T) {
	store := openIngestStore(t)
	deps := testIngestDeps(store)

	// 4-byte length prefix declaring 3 MB (over the 2 MB cap).
	dir := t.TempDir()
	bufPath := filepath.Join(dir, "buf.log")
	var lb [4]byte
	binary.BigEndian.PutUint32(lb[:], 3*1024*1024)
	if err := os.WriteFile(bufPath, lb[:], 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr strings.Builder
	code := CLIIngestSession(strings.NewReader(""), &stdout, &stderr, nil,
		[]string{"--buffer", bufPath, "--session-id", "sess"}, deps)
	if code != 1 {
		t.Errorf("expected exit 1 for oversized length, got %d", code)
	}
	if !strings.Contains(stderr.String(), "parse buffer") {
		t.Errorf("expected parse error in stderr, got %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "too large") {
		t.Errorf("expected 'too large' detail in stderr, got %q", stderr.String())
	}
}

func TestCLIIngestSession_BufferZeroLengthRecordsSkipped(t *testing.T) {
	store := openIngestStore(t)
	deps := testIngestDeps(store)

	// Record 1: length 0 (skipped). Record 2: length 20 payload.
	dir := t.TempDir()
	bufPath := filepath.Join(dir, "buf.log")
	f, err := os.Create(bufPath)
	if err != nil {
		t.Fatal(err)
	}
	var lb [4]byte
	binary.BigEndian.PutUint32(lb[:], 0)
	f.Write(lb[:])
	payload := "kept payload content"
	binary.BigEndian.PutUint32(lb[:], uint32(len(payload)))
	f.Write(lb[:])
	f.WriteString(payload)
	f.Close()

	var stdout, stderr strings.Builder
	code := CLIIngestSession(strings.NewReader(""), &stdout, &stderr, nil,
		[]string{"--buffer", bufPath, "--session-id", "sess"}, deps)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d stderr=%q", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("expected silent stderr, got %q", stderr.String())
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout.String()), &got); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, stdout.String())
	}
	if got["ok"] != true {
		t.Errorf("expected ok=true, got %v", got["ok"])
	}
}

func TestCLIIngestSession_OllamaUnavailable(t *testing.T) {
	store := openIngestStore(t)
	deps := testIngestDeps(store)
	deps.NewEmbedder = func(context.Context, string, string) (heimdall.Embedder, error) {
		return nil, &fakeErr{"ollama not reachable"}
	}

	var stdout, stderr strings.Builder
	code := CLIIngestSession(
		strings.NewReader("summary text"),
		&stdout, &stderr, nil,
		[]string{"--summary-stdin"}, deps,
	)
	if code != 1 {
		t.Errorf("expected exit 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "ollama not reachable") {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}
}
