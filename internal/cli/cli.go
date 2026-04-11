// Package cli implements the command-line interface for heimdall-mcp.
package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
	"github.com/caio-silva/heimdall-mcp/internal/registry"
)

// RunCLI dispatches the CLI subcommand.
func RunCLI(cfg config.Config, args []string) {
	cmd := args[0]
	// Parse --out flag from anywhere in args
	outPath := ""
	var cleanArgs []string
	for i := 1; i < len(args); i++ {
		if (args[i] == "--out" || args[i] == "-o") && i+1 < len(args) {
			outPath = args[i+1]
			i++
		} else {
			cleanArgs = append(cleanArgs, args[i])
		}
	}

	switch cmd {
	case "index":
		if len(cleanArgs) < 1 {
			fmt.Fprintf(os.Stderr, "Usage: heimdall-mcp index <path> [--out /path/to/output/dir]\n")
			os.Exit(1)
		}
		cliIndex(cfg, cleanArgs[0], outPath)
	case "status":
		cliStatus(cfg, outPath)
	case "search":
		if len(cleanArgs) < 1 {
			fmt.Fprintf(os.Stderr, "Usage: heimdall-mcp search <query> [--out /path/to/db/dir]\n")
			os.Exit(1)
		}
		cliSearch(cfg, cleanArgs[0], outPath)
	case "projects":
		cliProjects()
	case "help", "--help", "-h":
		fmt.Println("heimdall-mcp — local semantic code search")
		fmt.Println()
		fmt.Println("CLI usage:")
		fmt.Println("  heimdall-mcp index <path> [--out <dir>]   Index a directory")
		fmt.Println("  heimdall-mcp status [--out <dir>]         Show index stats")
		fmt.Println("  heimdall-mcp search <query> [--out <dir>] Search indexed files")
		fmt.Println("  heimdall-mcp projects                     List registered projects")
		fmt.Println()
		fmt.Println("Options:")
		fmt.Println("  --out, -o <dir>  Where to store the database (default: <path>/.heimdall_db/)")
		fmt.Println("                   The DB is a single file: <dir>/vectors.db")
		fmt.Println()
		fmt.Println("MCP usage (no args):")
		fmt.Println("  claude mcp add heimdall /path/to/heimdall-mcp")
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\nRun: heimdall-mcp help\n", cmd)
		os.Exit(1)
	}
}

func cliIndex(cfg config.Config, path string, dbPath string) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid path: %v\n", err)
		os.Exit(1)
	}
	info, err := os.Stat(absPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Path does not exist: %s\n", absPath)
		os.Exit(1)
	}
	if !info.IsDir() {
		fmt.Fprintf(os.Stderr, "Not a directory: %s\n", absPath)
		os.Exit(1)
	}

	// Migrate legacy .viking_db directory to .heimdall_db
	heimdall.MigrateDBDir(absPath)

	ctx := context.Background()
	client := heimdall.NewOllamaClient(cfg.OllamaEndpoint)
	fmt.Printf("Connecting to Ollama at %s...\n", cfg.OllamaEndpoint)
	if err := client.Ping(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Ollama not reachable: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Ollama: connected")

	dbDir := dbPath
	if dbDir == "" {
		dbDir = filepath.Join(absPath, ".heimdall_db")
	}
	store, err := heimdall.OpenStore(dbDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Store error: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	embedder := heimdall.NewOllamaEmbedder(client, cfg.Model)
	indexer := heimdall.NewIndexer(absPath, embedder, store, heimdall.ChunkerOpts{
		MaxChunkSize: 1500,
		ContextDepth: cfg.ContextDepth,
		ExcludeGlobs: cfg.ExcludePatterns,
	})

	startTime := time.Now()
	fmt.Printf("Indexing %s...\n", absPath)
	fmt.Printf("Started:  %s\n", startTime.Format("2006-01-02 15:04:05"))
	fmt.Printf("Database: %s\n\n", dbDir)

	// Use async with progress so we get live updates.
	// Incremental: skips files already indexed with same modtime.
	// If you stop and restart, it picks up where it left off.
	progressCh := make(chan heimdall.IndexProgress, 64)
	indexer.IndexProjectAsync(ctx, progressCh)

	lastPrint := time.Now()
	for p := range progressCh {
		if p.Done {
			if p.Err != nil {
				fmt.Fprintf(os.Stderr, "\nIndexing failed: %v\n", p.Err)
				os.Exit(1)
			}
			r := p.Result
			elapsed := time.Since(startTime).Round(time.Second)
			fmt.Printf("\n\nDone.\n")
			fmt.Printf("  Started:  %s\n", startTime.Format("2006-01-02 15:04:05"))
			fmt.Printf("  Finished: %s\n", time.Now().Format("2006-01-02 15:04:05"))
			fmt.Printf("  Elapsed:  %s\n", elapsed)
			fmt.Printf("  Scanned:  %d files\n", r.FilesScanned)
			fmt.Printf("  Indexed:  %d files\n", r.FilesIndexed)
			fmt.Printf("  Skipped:  %d files (unchanged)\n", r.FilesSkipped)
			fmt.Printf("  Chunks:   %d\n", r.ChunksCreated)
			fmt.Printf("  Database: %s\n", dbDir)

			// Register project in registry
			name := filepath.Base(absPath)
			reg := registry.LoadRegistry()
			reg.Register(name, absPath, dbDir)
			if err := reg.Save(); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to save project registry: %v\n", err)
			}
			return
		}

		now := time.Now()
		if now.Sub(lastPrint) > 500*time.Millisecond || p.Current == p.Total {
			elapsed := now.Sub(startTime).Round(time.Second)
			eta := ""
			if p.BytesDone > 0 && p.BytesTotal > 0 {
				// ETA based on bytes processed — accounts for file size differences
				bytesPerSec := float64(p.BytesDone) / now.Sub(startTime).Seconds()
				if bytesPerSec > 0 {
					remainingBytes := p.BytesTotal - p.BytesDone
					remainingSecs := float64(remainingBytes) / bytesPerSec
					eta = fmt.Sprintf(" | ETA: %s", (time.Duration(remainingSecs) * time.Second).Round(time.Second))
				}
			}
			pct := ""
			if p.BytesTotal > 0 {
				pct = fmt.Sprintf(" %d%%", p.BytesDone*100/p.BytesTotal)
			}
			fmt.Printf("\r  [%s] %d/%d files%s (%d chunks)%s — %s\033[K",
				elapsed, p.Current, p.Total, pct, p.ChunksSoFar, eta, p.FilePath)
			lastPrint = now
		}
	}
}

func cliStatus(cfg config.Config, dbPath string) {
	ctx := context.Background()
	client := heimdall.NewOllamaClient(cfg.OllamaEndpoint)

	fmt.Printf("Ollama: %s\n", cfg.OllamaEndpoint)
	if err := client.Ping(ctx); err != nil {
		fmt.Println("  Status: offline")
	} else {
		fmt.Println("  Status: running")
		models, err := client.ListModels(ctx)
		if err == nil {
			hasModel := false
			for _, m := range models {
				if m.Name == cfg.Model || (len(m.Name) > len(cfg.Model) && m.Name[:len(cfg.Model)] == cfg.Model) {
					hasModel = true
					break
				}
			}
			if hasModel {
				fmt.Printf("  Model: %s (available)\n", cfg.Model)
			} else {
				fmt.Printf("  Model: %s (not pulled)\n", cfg.Model)
			}
		}
	}

	statusDbDir := dbPath
	if statusDbDir == "" {
		cwd, _ := os.Getwd()
		statusDbDir = filepath.Join(cwd, ".heimdall_db")
	}
	if _, err := os.Stat(statusDbDir); err == nil {
		store, err := heimdall.OpenStore(statusDbDir)
		if err == nil {
			defer store.Close()
			stats := store.Stats()
			fmt.Printf("\nIndex: %s\n", statusDbDir)
			fmt.Printf("  Files: %d\n", stats.TotalFiles)
			fmt.Printf("  Chunks: %d\n", stats.TotalRecords)
			if stats.LastModified > 0 {
				fmt.Printf("  Last indexed: %s\n", time.Unix(stats.LastModified, 0).Format("2006-01-02 15:04:05"))
			}
		}
	} else {
		fmt.Printf("\nNo index found at %s\n", statusDbDir)
	}

	// Show registered projects
	reg := registry.LoadRegistry()
	projects := reg.All()
	if len(projects) > 0 {
		fmt.Printf("\nRegistered projects:\n")
		cwd, _ := os.Getwd()
		for _, p := range projects {
			marker := " "
			if registry.IsSubpath(cwd, p.Path) {
				marker = "*"
			}
			fmt.Printf("  %s %-20s %s\n", marker, p.Name, p.Path)
		}
	}
}

func cliSearch(cfg config.Config, query string, dbPath string) {
	ctx := context.Background()
	client := heimdall.NewOllamaClient(cfg.OllamaEndpoint)
	if err := client.Ping(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Ollama not reachable: %v\n", err)
		os.Exit(1)
	}

	searchDbDir := dbPath
	if searchDbDir == "" {
		cwd, _ := os.Getwd()
		searchDbDir = filepath.Join(cwd, ".heimdall_db")
	}
	store, err := heimdall.OpenStore(searchDbDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Store error: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	embedder := heimdall.NewOllamaEmbedder(client, cfg.Model)
	retriever := heimdall.NewRetriever(embedder, store, 5, cfg.MaxContextTokens)

	blocks, err := retriever.Retrieve(ctx, query)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Search error: %v\n", err)
		os.Exit(1)
	}

	if len(blocks) == 0 {
		fmt.Println("No results found.")
		return
	}

	for i, b := range blocks {
		fmt.Printf("\n--- %s (L%d-%d, score: %.2f) ---\n", b.FilePath, b.StartLine, b.EndLine, b.Score)
		fmt.Println(b.Content)
		if i < len(blocks)-1 {
			fmt.Println()
		}
	}
}

func cliProjects() {
	reg := registry.LoadRegistry()
	projects := reg.All()
	if len(projects) == 0 {
		fmt.Println("No projects registered. Index a project first.")
		return
	}

	cwd, _ := os.Getwd()
	fmt.Printf("Registered projects (%d):\n\n", len(projects))
	for _, p := range projects {
		marker := " "
		if registry.IsSubpath(cwd, p.Path) {
			marker = "*"
		}
		fmt.Printf("  %s %s\n", marker, p.Name)
		fmt.Printf("    Path: %s\n", p.Path)
		fmt.Printf("    DB:   %s\n", p.DBPath)

		// Try to show stats
		if _, err := os.Stat(p.DBPath); err == nil {
			store, err := heimdall.OpenStore(p.DBPath)
			if err == nil {
				stats := store.Stats()
				fmt.Printf("    Files: %d  Chunks: %d", stats.TotalFiles, stats.TotalRecords)
				if stats.LastModified > 0 {
					fmt.Printf("  Last indexed: %s", time.Unix(stats.LastModified, 0).Format("2006-01-02 15:04:05"))
				}
				fmt.Println()
				store.Close()
			}
		}
	}
}
