package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
	"github.com/caio-silva/heimdall-mcp/internal/registry"
)

// StatusDeps exposes injection seams for CLIStatus tests.
type StatusDeps struct {
	LoadConfig   func() config.Config
	LoadRegistry func() *registry.Registry
	NewClient    func(endpoint string) *heimdall.OllamaClient
	// BaseDir overrides the default CWD/.heimdall_db base. Empty means use
	// --out flag (if present) else CWD/.heimdall_db.
	BaseDirOverride string
	// Now lets tests freeze time if they need deterministic "last indexed"
	// formatting. Unused today; reserved.
	Now func() time.Time
}

// CLIStatus is the testable handler for `heimdall-mcp status`.
// Preserves the existing human-friendly text output byte-for-byte when
// --format is absent or "text". Adds --format=json which emits the shape
// from heimdall.StatusInfo (mirrors MCP toolStatus).
func CLIStatus(stdin io.Reader, stdout, stderr io.Writer, env map[string]string, args []string, deps StatusDeps) int {
	_ = stdin
	_ = env

	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		outPath string
		format  string
	)
	fs.StringVar(&outPath, "out", "", "base directory for the index (default: <cwd>/.heimdall_db)")
	fs.StringVar(&outPath, "o", "", "alias of --out")
	fs.StringVar(&format, "format", "text", "output format: text, json")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	switch format {
	case "text", "json":
	default:
		fmt.Fprintf(stderr, "error: invalid --format %q (want text|json)\n", format)
		return 2
	}

	loadCfg := deps.LoadConfig
	if loadCfg == nil {
		loadCfg = config.LoadConfig
	}
	cfg := loadCfg()

	loadReg := deps.LoadRegistry
	if loadReg == nil {
		loadReg = registry.LoadRegistry
	}
	reg := loadReg()

	newClient := deps.NewClient
	if newClient == nil {
		newClient = heimdall.NewOllamaClient
	}
	client := newClient(cfg.OllamaEndpoint)

	baseDir := deps.BaseDirOverride
	if baseDir == "" {
		baseDir = outPath
	}
	if baseDir == "" {
		cwd, _ := os.Getwd()
		baseDir = filepath.Join(cwd, ".heimdall_db")
	}

	if format == "json" {
		return writeStatusJSON(stdout, stderr, cfg, reg, client, baseDir)
	}
	return writeStatusText(stdout, cfg, reg, client, baseDir)
}

func writeStatusJSON(stdout, stderr io.Writer, cfg config.Config, reg *registry.Registry, client *heimdall.OllamaClient, baseDir string) int {
	ctx := context.Background()
	info := heimdall.GatherStatus(ctx, cfg.OllamaEndpoint, cfg.Model, baseDir, cfg.ExcludePatterns, client)

	cwd, _ := os.Getwd()
	for _, p := range reg.All() {
		info.Projects = append(info.Projects, heimdall.StatusProject{
			Name:    p.Name,
			Path:    p.Path,
			DBPath:  p.DBPath,
			Current: registry.IsSubpath(cwd, p.Path),
		})
	}

	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "error: marshal json: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, string(data))
	return 0
}

// writeStatusText reproduces the byte-for-byte output of the original cliStatus
// so existing callers and docs are unaffected.
func writeStatusText(stdout io.Writer, cfg config.Config, reg *registry.Registry, client *heimdall.OllamaClient, baseDir string) int {
	ctx := context.Background()
	fmt.Fprintf(stdout, "Ollama: %s\n", cfg.OllamaEndpoint)
	fmt.Fprintf(stdout, "Model:  %s\n", cfg.Model)
	if err := client.Ping(ctx); err != nil {
		fmt.Fprintln(stdout, "  Status: offline")
	} else {
		fmt.Fprintln(stdout, "  Status: running")
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
				fmt.Fprintf(stdout, "  Model: %s (available)\n", cfg.Model)
			} else {
				fmt.Fprintf(stdout, "  Model: %s (not pulled)\n", cfg.Model)
			}
		}
	}

	available := heimdall.ListAvailableModels(baseDir)
	if len(available) > 0 {
		fmt.Fprintf(stdout, "\nAvailable models: %v\n", available)
	}

	modelDir := heimdall.ModelDBDir(baseDir, cfg.Model)
	if _, err := os.Stat(modelDir); err == nil {
		store, err := heimdall.OpenStore(modelDir)
		if err == nil {
			stats := store.Stats()
			fmt.Fprintf(stdout, "\nIndex (%s): %s\n", cfg.Model, modelDir)
			fmt.Fprintf(stdout, "  Files: %d\n", stats.TotalFiles)
			fmt.Fprintf(stdout, "  Chunks: %d\n", stats.TotalRecords)
			if stats.LastModified > 0 {
				fmt.Fprintf(stdout, "  Last indexed: %s\n", time.Unix(stats.LastModified, 0).Format("2006-01-02 15:04:05"))
			}
			store.Close()
		}
	} else {
		fmt.Fprintf(stdout, "\nNo index found for model %q at %s\n", cfg.Model, modelDir)
		if len(available) > 0 {
			fmt.Fprintf(stdout, "  Available models: %v\n", available)
		}
	}

	projects := reg.All()
	if len(projects) > 0 {
		fmt.Fprintf(stdout, "\nRegistered projects:\n")
		cwd, _ := os.Getwd()
		for _, p := range projects {
			marker := " "
			if registry.IsSubpath(cwd, p.Path) {
				marker = "*"
			}
			fmt.Fprintf(stdout, "  %s %-20s %s\n", marker, p.Name, p.Path)
		}
	}
	return 0
}
