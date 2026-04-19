// skills.go — implements `heimdall-mcp skills <subcommand>` for the
// disk → heimdall sync direction (TODO section 4 follow-up).
//
// Today only `skills import` is supported. It walks the configured Claude
// Code skills directory (default `~/.claude/skills`, overridable via the
// HEIMDALL_CLAUDE_SKILLS_DIR env var or the `--dir <path>` flag), parses
// every SKILL.md it finds, and upserts a `MemoryType=skill` row keyed by
// a deterministic slug ID so re-runs are idempotent.
//
// The outbound (heimdall → disk) direction is exposed only via the MCP
// `heimdall_remember` tool's `write_file` flag (see internal/mcp/memory_tools.go).
// We deliberately do NOT expose a CLI write-back command — outbound is
// opt-in per memory and the CLI surface for that would just duplicate the
// MCP one without adding value.
//
// Exit codes: 0 happy, 1 runtime error, 2 usage error.
package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// SkillsDeps lets tests inject fakes for the embedder and store openers
// without going through real Ollama or the global memory DB path.
type SkillsDeps struct {
	// LoadConfig defaults to config.LoadConfig.
	LoadConfig func() config.Config
	// OpenMemoryStore defaults to opening config.ResolveMemoryDBPath.
	OpenMemoryStore func() (*heimdall.MemoryStore, error)
	// NewEmbedder defaults to a real Ollama embedder. The returned
	// closure is invoked once per skill body that needs embedding.
	NewEmbedder func(ctx context.Context, endpoint, model string) (func(string) ([]float32, error), error)
}

// CLISkills dispatches `heimdall-mcp skills <subcommand>`.
func CLISkills(cfg config.Config, stdin io.Reader, stdout, stderr io.Writer, env map[string]string, args []string, deps SkillsDeps) int {
	_ = stdin
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage: heimdall-mcp skills <import>")
		fmt.Fprintln(stderr, "  import  Import SKILL.md files from ~/.claude/skills/ as type=skill memories")
		return 2
	}
	switch args[0] {
	case "import":
		return cliSkillsImport(cfg, stdout, stderr, env, args[1:], deps)
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, "heimdall-mcp skills — sync between Claude Code skill files and Heimdall memories")
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "  skills import [--dir <path>] [--dry-run] [--format text|json]")
		fmt.Fprintln(stdout, "    Walk the skills directory and upsert one type=skill memory per SKILL.md.")
		fmt.Fprintln(stdout, "    Default --dir resolves from $HEIMDALL_CLAUDE_SKILLS_DIR, then ~/.claude/skills.")
		fmt.Fprintln(stdout, "    Idempotent — re-runs only update memories whose content has changed.")
		return 0
	default:
		fmt.Fprintf(stderr, "Unknown skills subcommand: %s\n", args[0])
		return 2
	}
}

// cliSkillsImport handles `heimdall-mcp skills import`. Returns the shell
// exit code. Tests inject deps; production passes empty SkillsDeps which is
// then hydrated via fillSkillsDeps below.
func cliSkillsImport(cfg config.Config, stdout, stderr io.Writer, env map[string]string, args []string, deps SkillsDeps) int {
	fs := flag.NewFlagSet("skills import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		dirFlag string
		dryRun  bool
		format  string
	)
	fs.StringVar(&dirFlag, "dir", "", "skills directory to scan (default: $HEIMDALL_CLAUDE_SKILLS_DIR or ~/.claude/skills)")
	fs.BoolVar(&dryRun, "dry-run", false, "report what would change without writing to the memory store")
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

	skillsDir := heimdall.ResolveClaudeSkillsDir(dirFlag, env)
	if skillsDir == "" {
		fmt.Fprintln(stderr, "error: could not resolve skills directory (set --dir or HEIMDALL_CLAUDE_SKILLS_DIR)")
		return 1
	}

	deps = fillSkillsDeps(deps)

	openStore := deps.OpenMemoryStore
	store, err := openStore()
	if err != nil {
		fmt.Fprintf(stderr, "error: open memory store: %v\n", err)
		return 1
	}
	defer store.Close()

	var embedFn func(string) ([]float32, error)
	if !dryRun {
		ctx := context.Background()
		embedFn, err = deps.NewEmbedder(ctx, cfg.OllamaEndpoint, cfg.Model)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
	}

	res, err := heimdall.ImportSkillsFromDir(store, heimdall.SkillImportOpts{
		Dir:    skillsDir,
		Embed:  embedFn,
		DryRun: dryRun,
	})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	return writeSkillsImport(stdout, stderr, format, skillsDir, dryRun, res)
}

// writeSkillsImport emits the text or JSON summary of an import run. Errors
// during import are logged to stderr but do not change the exit code unless
// the entire run failed (handled by the caller).
func writeSkillsImport(stdout, stderr io.Writer, format, dir string, dryRun bool, res heimdall.SkillImportResult) int {
	for _, e := range res.Errors {
		fmt.Fprintf(stderr, "skill import warning: %s: %v\n", e.Path, e.Err)
	}
	switch format {
	case "json":
		payload := map[string]any{
			"dir":         dir,
			"dryRun":      dryRun,
			"scannedDirs": res.ScannedDirs,
			"filesFound":  res.FilesFound,
			"created":     res.Created,
			"updated":     res.Updated,
			"unchanged":   res.Unchanged,
			"skipped":     res.Skipped,
			"errorCount":  len(res.Errors),
		}
		if errs := errorPathStrings(res.Errors); len(errs) > 0 {
			payload["errors"] = errs
		}
		data, _ := json.MarshalIndent(payload, "", "  ")
		fmt.Fprintln(stdout, string(data))
	default:
		marker := ""
		if dryRun {
			marker = " (dry-run)"
		}
		fmt.Fprintf(stdout, "Imported skills from %s%s\n", dir, marker)
		fmt.Fprintf(stdout, "  scanned:   %d\n", res.ScannedDirs)
		fmt.Fprintf(stdout, "  files:     %d\n", res.FilesFound)
		fmt.Fprintf(stdout, "  created:   %d\n", res.Created)
		fmt.Fprintf(stdout, "  updated:   %d\n", res.Updated)
		fmt.Fprintf(stdout, "  unchanged: %d\n", res.Unchanged)
		fmt.Fprintf(stdout, "  skipped:   %d\n", res.Skipped)
		if len(res.Errors) > 0 {
			fmt.Fprintf(stdout, "  errors:    %d (see stderr)\n", len(res.Errors))
		}
	}
	return 0
}

func errorPathStrings(errs []heimdall.SkillImportError) []string {
	if len(errs) == 0 {
		return nil
	}
	out := make([]string, 0, len(errs))
	for _, e := range errs {
		out = append(out, fmt.Sprintf("%s: %s", e.Path, e.Err))
	}
	return out
}

// fillSkillsDeps hydrates any nil function fields in deps with their real
// production defaults. Test callers leave fields nil to use the real impl
// or set them explicitly to inject a fake.
func fillSkillsDeps(deps SkillsDeps) SkillsDeps {
	if deps.LoadConfig == nil {
		deps.LoadConfig = config.LoadConfig
	}
	if deps.OpenMemoryStore == nil {
		deps.OpenMemoryStore = func() (*heimdall.MemoryStore, error) {
			return heimdall.OpenMemoryStore(config.ResolveMemoryDBPath())
		}
	}
	if deps.NewEmbedder == nil {
		deps.NewEmbedder = func(ctx context.Context, endpoint, model string) (func(string) ([]float32, error), error) {
			client := newOllamaClientForEndpoint(endpoint)
			if err := client.Ping(ctx); err != nil {
				return nil, fmt.Errorf("ollama not reachable at %s: %w", endpoint, err)
			}
			emb := heimdall.NewOllamaEmbedder(client, model)
			return func(content string) ([]float32, error) {
				return emb.Embed(ctx, content)
			}, nil
		}
	}
	return deps
}

