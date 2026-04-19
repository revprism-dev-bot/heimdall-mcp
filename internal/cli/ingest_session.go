package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// IngestDeps mirrors RecallDeps for dependency injection in tests.
type IngestDeps struct {
	LoadConfig      func() config.Config
	NewEmbedder     func(ctx context.Context, endpoint, model string) (heimdall.Embedder, error)
	OpenMemoryStore func() (*heimdall.MemoryStore, error)
}

// CLIIngestSession is the testable handler for `heimdall-mcp ingest-session`.
// Flags: --summary-stdin OR --buffer <path> (exactly one), --project, --session-id.
// Output on success: single JSON object with ok/inserted/session_id to stdout.
// No output to stderr on success. Exit 0 on success, 1 runtime, 2 usage.
func CLIIngestSession(stdin io.Reader, stdout, stderr io.Writer, env map[string]string, args []string, deps IngestDeps) int {
	_ = env
	fs := flag.NewFlagSet("ingest-session", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		summaryStdin bool
		bufferPath   string
		project      string
		sessionID    string
	)
	fs.BoolVar(&summaryStdin, "summary-stdin", false, "read plain-text summary from stdin")
	fs.StringVar(&bufferPath, "buffer", "", "read length-prefixed JSON buffer from this file (see §3.4)")
	fs.StringVar(&project, "project", "", "project name")
	fs.StringVar(&sessionID, "session-id", "", "session id (for logging/output)")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Exactly one of --summary-stdin or --buffer must be set.
	switch {
	case summaryStdin && bufferPath != "":
		fmt.Fprintln(stderr, "error: --summary-stdin and --buffer are mutually exclusive")
		return 2
	case !summaryStdin && bufferPath == "":
		fmt.Fprintln(stderr, "error: one of --summary-stdin or --buffer is required")
		return 2
	}

	var summary string
	if summaryStdin {
		data, err := io.ReadAll(stdin)
		if err != nil {
			fmt.Fprintf(stderr, "error: read stdin: %v\n", err)
			return 1
		}
		summary = string(data)
	} else {
		f, err := os.Open(bufferPath) //nolint:gosec // path supplied by trusted caller (hook stop)
		if err != nil {
			fmt.Fprintf(stderr, "error: open buffer %s: %v\n", bufferPath, err)
			return 1
		}
		parsed, err := heimdall.ReadLengthPrefixedBuffer(f)
		f.Close()
		if err != nil {
			fmt.Fprintf(stderr, "error: parse buffer: %v\n", err)
			return 1
		}
		summary = parsed
	}

	if summary == "" {
		fmt.Fprintln(stderr, "error: empty summary")
		return 1
	}

	loadCfg := deps.LoadConfig
	if loadCfg == nil {
		loadCfg = config.LoadConfig
	}
	cfg := loadCfg()

	openStore := deps.OpenMemoryStore
	if openStore == nil {
		openStore = func() (*heimdall.MemoryStore, error) {
			return heimdall.OpenMemoryStore(config.ResolveMemoryDBPath())
		}
	}
	store, err := openStore()
	if err != nil {
		fmt.Fprintf(stderr, "error: open memory store: %v\n", err)
		return 1
	}
	defer store.Close()

	newEmbedder := deps.NewEmbedder
	if newEmbedder == nil {
		newEmbedder = func(ctx context.Context, endpoint, model string) (heimdall.Embedder, error) {
			client := newOllamaClientForEndpoint(endpoint)
			if err := client.Ping(ctx); err != nil {
				return nil, fmt.Errorf("ollama not reachable at %s: %w", endpoint, err)
			}
			return heimdall.NewOllamaEmbedder(client, model), nil
		}
	}

	ctx := context.Background()
	embedder, err := newEmbedder(ctx, cfg.OllamaEndpoint, cfg.Model)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	result, err := heimdall.IngestSessionSummary(ctx, summary, project, embedder, store)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	// Inserted = created + updated (both represent new or refreshed rows).
	inserted := result.MemoriesCreated + result.MemoriesUpdated
	out := map[string]any{
		"ok":               true,
		"inserted":         inserted,
		"session_id":       sessionID,
		"chunks_processed": result.ChunksProcessed,
		"memories_created": result.MemoriesCreated,
		"memories_updated": result.MemoriesUpdated,
		"duplicates":       result.Duplicates,
	}
	data, err := json.Marshal(out)
	if err != nil {
		fmt.Fprintf(stderr, "error: marshal response: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, string(data))
	return 0
}
