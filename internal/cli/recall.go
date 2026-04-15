package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// RecallDeps abstracts the external resources the recall handler needs,
// so tests can inject fakes without touching globals.
type RecallDeps struct {
	// Config source. If nil, config.LoadConfig is used.
	LoadConfig func() config.Config
	// NewEmbedder builds an embedder for a given endpoint+model.
	// If nil, defaults to a real Ollama embedder.
	NewEmbedder func(ctx context.Context, endpoint, model string) (heimdall.Embedder, error)
	// OpenMemoryStore opens the memory store. If nil, uses the real one at
	// config.ResolveMemoryDBPath().
	OpenMemoryStore func() (*heimdall.MemoryStore, error)
}

// CLIRecall is the testable handler for `heimdall-mcp recall`.
// It parses flags from args, performs the recall, and writes output in the
// requested format. Exit codes: 0 happy, 1 runtime error, 2 usage error.
//
// Contract per docs/plans/hooks/00-consolidated-plan.md §5.4:
//   - no os.Exit
//   - no read from os.Stdin/Stdout/Stderr (use stdin/stdout/stderr params)
//   - env passed as map for test injection
func CLIRecall(stdin io.Reader, stdout, stderr io.Writer, env map[string]string, args []string, deps RecallDeps) int {
	_ = stdin // recall does not read stdin; kept for uniform handler signature
	_ = env   // reserved for HEIMDALL_HOOK suppression and friends
	fs := flag.NewFlagSet("recall", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		query   string
		limit   int
		memType string
		tagsRaw string
		project string
		format  string
	)
	fs.StringVar(&query, "query", "", "query string (required)")
	fs.IntVar(&limit, "limit", 5, "max results (1-100)")
	fs.StringVar(&memType, "type", "", "memory type: preference, decision, fact, context")
	fs.StringVar(&tagsRaw, "tags", "", "comma-separated tag filter")
	fs.StringVar(&project, "project", "", "project name filter")
	fs.StringVar(&format, "format", "text", "output format: text, json, hook-md")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	if query == "" {
		fmt.Fprintln(stderr, "error: --query is required")
		return 2
	}

	switch format {
	case "text", "json", "hook-md":
	default:
		fmt.Fprintf(stderr, "error: invalid --format %q (want text|json|hook-md)\n", format)
		return 2
	}

	validTypes := map[string]bool{"": true, "preference": true, "decision": true, "fact": true, "context": true}
	if !validTypes[memType] {
		fmt.Fprintf(stderr, "error: invalid --type %q (want preference|decision|fact|context)\n", memType)
		return 2
	}

	var tags []string
	if tagsRaw != "" {
		for _, t := range strings.Split(tagsRaw, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				tags = append(tags, t)
			}
		}
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
			client := heimdall.NewOllamaClient(endpoint)
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

	hits, err := heimdall.RunRecall(ctx, heimdall.RecallParams{
		Query:   query,
		Type:    memType,
		Tags:    tags,
		Project: project,
		Limit:   limit,
	}, embedder, store)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	return writeRecall(stdout, format, hits)
}

func writeRecall(w io.Writer, format string, hits []heimdall.RecallHit) int {
	switch format {
	case "json":
		// Always produce a JSON array (never null) for a stable contract.
		if hits == nil {
			hits = []heimdall.RecallHit{}
		}
		data, err := json.MarshalIndent(hits, "", "  ")
		if err != nil {
			fmt.Fprintf(w, "error: marshal json: %v\n", err)
			return 1
		}
		fmt.Fprintln(w, string(data))
		return 0

	case "hook-md":
		// Bullet-per-memory, no header or footer. Composer wraps it.
		for _, h := range hits {
			line := fmt.Sprintf("- %s: %s", h.Type, singleLine(h.Content))
			if len(h.Tags) > 0 {
				line += fmt.Sprintf(" _(tags: %s)_", strings.Join(h.Tags, ", "))
			}
			fmt.Fprintln(w, line)
		}
		return 0

	default: // "text"
		if len(hits) == 0 {
			fmt.Fprintln(w, "No memories found.")
			return 0
		}
		for i, h := range hits {
			if i > 0 {
				fmt.Fprintln(w)
			}
			fmt.Fprintf(w, "%.2f [%s] %s\n", h.Score, h.Type, h.Content)
			if len(h.Tags) > 0 {
				fmt.Fprintf(w, "  tags: %s\n", strings.Join(h.Tags, ", "))
			}
			if h.Project != "" {
				fmt.Fprintf(w, "  project: %s\n", h.Project)
			}
		}
		return 0
	}
}

// singleLine collapses line breaks so a bullet stays on one line.
func singleLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

