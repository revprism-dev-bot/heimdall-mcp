// Package cli implements the command-line interface for heimdall-mcp.
package cli

import (
	"context"
	"encoding/json"
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
	case "configure", "config":
		cliConfigure(cleanArgs)
	case "paths":
		cliManagePaths(cfg, cleanArgs)
	case "models":
		cliModels(cfg)
	case "help", "--help", "-h":
		fmt.Println("heimdall-mcp — local semantic code search + memory")
		fmt.Println()
		fmt.Println("CLI usage:")
		fmt.Println("  heimdall-mcp index <path> [--out <dir>]    Index a directory")
		fmt.Println("  heimdall-mcp status [--out <dir>]          Show index stats")
		fmt.Println("  heimdall-mcp search <query> [--out <dir>]  Search indexed files")
		fmt.Println("  heimdall-mcp projects                      List registered projects")
		fmt.Println("  heimdall-mcp config get [key]              Get config (full or key)")
		fmt.Println("  heimdall-mcp config set <key> <value>      Set a config key")
		fmt.Println("  heimdall-mcp paths list                    List indexed paths")
		fmt.Println("  heimdall-mcp paths add <path>              Add a path to index")
		fmt.Println("  heimdall-mcp paths remove <path>           Remove a path")
		fmt.Println("  heimdall-mcp models                        List available embedding models")
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

func cliConfigure(args []string) {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  heimdall-mcp config get [key]        Get config (full or specific key)\n")
		fmt.Fprintf(os.Stderr, "  heimdall-mcp config set <key> <val>  Set a config key\n")
		fmt.Fprintf(os.Stderr, "\nKeys: model, git.enabled, git.depth, git.include_diffs, git.branches,\n")
		fmt.Fprintf(os.Stderr, "      stale_timeout_minutes, lifecycle.active_days,\n")
		fmt.Fprintf(os.Stderr, "      lifecycle.archive_days, max_chunks_per_project\n")
		os.Exit(1)
	}

	switch args[0] {
	case "get":
		cfg := config.LoadConfig()
		if len(args) > 1 {
			key := args[1]
			val, ok := getConfigKey(&cfg, key)
			if !ok {
				fmt.Fprintf(os.Stderr, "Unknown config key: %s\n", key)
				os.Exit(1)
			}
			out, _ := json.MarshalIndent(map[string]any{"key": key, "value": val}, "", "  ")
			fmt.Println(string(out))
		} else {
			out, _ := json.MarshalIndent(cfg, "", "  ")
			fmt.Println(string(out))
		}
	case "set":
		if len(args) < 3 {
			fmt.Fprintf(os.Stderr, "Usage: heimdall-mcp config set <key> <value>\n")
			os.Exit(1)
		}
		cfg := config.LoadConfig()
		key, rawVal := args[1], args[2]

		// Validate model before saving — verify it exists and can embed
		if key == "model" {
			fmt.Printf("Verifying model %q with Ollama...\n", rawVal)
			client := heimdall.NewOllamaClient(cfg.OllamaEndpoint)
			ctx := context.Background()
			if err := client.VerifyModel(ctx, rawVal); err != nil {
				fmt.Fprintf(os.Stderr, "Model verification failed: %v\n", err)
				fmt.Fprintf(os.Stderr, "\nThe model must be pulled in Ollama and support embeddings.\n")
				fmt.Fprintf(os.Stderr, "Run: ollama pull %s\n", rawVal)
				fmt.Fprintf(os.Stderr, "Run: heimdall-mcp models   (to see available models)\n")
				os.Exit(1)
			}
			fmt.Println("Model verified: produces embeddings.")
			if !heimdall.IsKnownEmbeddingModel(rawVal) {
				fmt.Println("Note: this model is not in our curated list but it works for embeddings.")
			}
		}

		if err := setConfigKey(&cfg, key, rawVal); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		if err := config.SaveConfig(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to save config: %v\n", err)
			os.Exit(1)
		}
		val, _ := getConfigKey(&cfg, key)
		fmt.Printf("%s = %v (saved)\n", key, val)
		if key == "model" {
			fmt.Println("Important: re-index your projects for the new model to take effect.")
		}
	default:
		fmt.Fprintf(os.Stderr, "Unknown config action: %s (use 'get' or 'set')\n", args[0])
		os.Exit(1)
	}
}

func getConfigKey(cfg *config.Config, key string) (any, bool) {
	switch key {
	case "model":
		return cfg.Model, true
	case "git.enabled":
		return cfg.GitEnabled, true
	case "git.depth":
		return cfg.GitDepth, true
	case "git.include_diffs":
		return cfg.GitIncludeDiffs, true
	case "git.branches":
		return cfg.GitBranches, true
	case "stale_timeout_minutes":
		return cfg.StaleTimeoutMin, true
	case "lifecycle.active_days":
		return cfg.LifecycleActiveDays, true
	case "lifecycle.archive_days":
		return cfg.LifecycleArchiveDays, true
	case "max_chunks_per_project":
		return cfg.MaxChunksPerProject, true
	default:
		return nil, false
	}
}

func setConfigKey(cfg *config.Config, key, rawVal string) error {
	switch key {
	case "model":
		cfg.Model = rawVal
	case "git.enabled":
		cfg.GitEnabled = rawVal == "true"
	case "git.depth":
		var n int
		if _, err := fmt.Sscanf(rawVal, "%d", &n); err != nil {
			return fmt.Errorf("git.depth requires an integer: %w", err)
		}
		cfg.GitDepth = n
	case "git.include_diffs":
		cfg.GitIncludeDiffs = rawVal == "true"
	case "git.branches":
		var branches []string
		if err := json.Unmarshal([]byte(rawVal), &branches); err != nil {
			return fmt.Errorf("git.branches requires a JSON array: %w", err)
		}
		cfg.GitBranches = branches
	case "stale_timeout_minutes":
		var n int
		if _, err := fmt.Sscanf(rawVal, "%d", &n); err != nil {
			return fmt.Errorf("stale_timeout_minutes requires an integer: %w", err)
		}
		cfg.StaleTimeoutMin = n
	case "lifecycle.active_days":
		var n int
		if _, err := fmt.Sscanf(rawVal, "%d", &n); err != nil {
			return fmt.Errorf("lifecycle.active_days requires an integer: %w", err)
		}
		cfg.LifecycleActiveDays = n
	case "lifecycle.archive_days":
		var n int
		if _, err := fmt.Sscanf(rawVal, "%d", &n); err != nil {
			return fmt.Errorf("lifecycle.archive_days requires an integer: %w", err)
		}
		cfg.LifecycleArchiveDays = n
	case "max_chunks_per_project":
		var n int
		if _, err := fmt.Sscanf(rawVal, "%d", &n); err != nil {
			return fmt.Errorf("max_chunks_per_project requires an integer: %w", err)
		}
		cfg.MaxChunksPerProject = n
	default:
		return fmt.Errorf("unknown config key: %s", key)
	}
	return nil
}

func cliManagePaths(cfg config.Config, args []string) {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  heimdall-mcp paths list              List indexed paths\n")
		fmt.Fprintf(os.Stderr, "  heimdall-mcp paths add <path>        Add a directory to index\n")
		fmt.Fprintf(os.Stderr, "  heimdall-mcp paths remove <path>     Remove a directory\n")
		os.Exit(1)
	}

	switch args[0] {
	case "list":
		cfg := config.LoadConfig()
		if len(cfg.IndexedPaths) == 0 {
			fmt.Println("No indexed paths configured.")
			return
		}
		fmt.Printf("Indexed paths (%d):\n", len(cfg.IndexedPaths))
		for _, p := range cfg.IndexedPaths {
			fmt.Printf("  %s\n", p)
		}
	case "add":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "Usage: heimdall-mcp paths add <path>\n")
			os.Exit(1)
		}
		path := args[1]
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

		cfg := config.LoadConfig()
		for _, existing := range cfg.IndexedPaths {
			if existing == absPath {
				fmt.Printf("Path already indexed: %s\n", absPath)
				return
			}
		}
		cfg.IndexedPaths = append(cfg.IndexedPaths, absPath)
		if err := config.SaveConfig(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to save config: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Added: %s (%d total paths)\n", absPath, len(cfg.IndexedPaths))
	case "remove":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "Usage: heimdall-mcp paths remove <path>\n")
			os.Exit(1)
		}
		path := args[1]
		absPath, _ := filepath.Abs(path)

		cfg := config.LoadConfig()
		idx := -1
		for i, p := range cfg.IndexedPaths {
			if p == path || p == absPath {
				idx = i
				break
			}
		}
		if idx == -1 {
			fmt.Fprintf(os.Stderr, "Path not found: %s\n", path)
			os.Exit(1)
		}
		cfg.IndexedPaths = append(cfg.IndexedPaths[:idx], cfg.IndexedPaths[idx+1:]...)
		if err := config.SaveConfig(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to save config: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Removed: %s (%d remaining paths)\n", path, len(cfg.IndexedPaths))
	default:
		fmt.Fprintf(os.Stderr, "Unknown paths action: %s (use 'list', 'add', or 'remove')\n", args[0])
		os.Exit(1)
	}
}

func cliModels(cfg config.Config) {
	ctx := context.Background()
	client := heimdall.NewOllamaClient(cfg.OllamaEndpoint)

	fmt.Printf("Current model: %s\n", cfg.Model)
	fmt.Println()

	// Show curated list
	fmt.Println("Recommended embedding models:")
	fmt.Println()
	fmt.Printf("  %-28s %-18s %-6s %-5s %s\n", "MODEL", "ORIGIN", "SIZE", "DIM", "NOTES")
	fmt.Printf("  %-28s %-18s %-6s %-5s %s\n", "-----", "------", "----", "---", "-----")
	for _, m := range heimdall.KnownEmbeddingModels {
		marker := " "
		if m.Name == cfg.Model {
			marker = "*"
		}
		fmt.Printf(" %s%-28s %-18s %-6s %-5d %s\n", marker, m.Name, m.Origin, m.Params, m.Dimensions, m.Notes)
	}
	fmt.Println()

	// Check what's actually pulled in Ollama
	if err := client.Ping(ctx); err != nil {
		fmt.Println("Ollama: offline — cannot check local models")
		fmt.Println("Run: ollama serve")
		return
	}

	models, err := client.ListModels(ctx)
	if err != nil {
		fmt.Printf("Could not list Ollama models: %v\n", err)
		return
	}

	fmt.Println("Local Ollama models:")
	fmt.Println()
	if len(models) == 0 {
		fmt.Println("  (none pulled)")
		return
	}

	for _, m := range models {
		status := ""
		if heimdall.IsKnownEmbeddingModel(m.Name) {
			info := heimdall.LookupEmbeddingModel(m.Name)
			if info != nil {
				status = fmt.Sprintf("embedding (%s, %dd)", info.Params, info.Dimensions)
			} else {
				status = "embedding (known)"
			}
		} else {
			status = "unknown — use 'config set model' to test"
		}
		marker := " "
		if m.Name == cfg.Model || (len(m.Name) > len(cfg.Model) && m.Name[:len(cfg.Model)] == cfg.Model) {
			marker = "*"
		}
		fmt.Printf(" %s%-35s %s\n", marker, m.Name, status)
	}
	fmt.Println()
	fmt.Println("To use a model: heimdall-mcp config set model <name>")
	fmt.Println("To pull a model: ollama pull <name>")
}
