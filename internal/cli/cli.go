// Package cli implements the command-line interface for heimdall-mcp.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
	"github.com/caio-silva/heimdall-mcp/internal/registry"
)

// computeETA returns a human-readable ETA string given indexing progress.
func computeETA(chunksSoFar, filesCurrent, filesTotal int, elapsed time.Duration) string {
	if chunksSoFar <= 0 || filesCurrent <= 0 || filesTotal <= 0 {
		return ""
	}
	elapsedSecs := elapsed.Seconds()
	if elapsedSecs <= 0 {
		return ""
	}
	chunksPerSec := float64(chunksSoFar) / elapsedSecs
	if chunksPerSec <= 0 {
		return ""
	}
	remainingFiles := filesTotal - filesCurrent
	if remainingFiles <= 0 {
		return ""
	}
	avgChunksPerFile := float64(chunksSoFar) / float64(filesCurrent)
	remainingChunks := float64(remainingFiles) * avgChunksPerFile
	remainingSecs := remainingChunks / chunksPerSec
	return fmt.Sprintf(" | ETA: %s", (time.Duration(remainingSecs) * time.Second).Round(time.Second))
}

// buildVersion returns a short version string suitable for `--version` output.
// Reads the module version and vcs.revision from the Go build info so local
// `go build` and `go install` both produce something meaningful without needing
// -ldflags injection. Falls back to "wave2-phase3" (matching the installed hook
// envelope's heimdall_version) when build info is unavailable.
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "heimdall-mcp wave2-phase3"
	}
	version := info.Main.Version
	if version == "" || version == "(devel)" {
		version = "wave2-phase3"
	}
	var rev string
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 7 {
			rev = s.Value[:7]
			break
		}
	}
	if rev != "" {
		return fmt.Sprintf("heimdall-mcp %s (%s)", version, rev)
	}
	return fmt.Sprintf("heimdall-mcp %s", version)
}

// envToMap converts os.Environ-style KEY=VALUE slices to a map.
// Shared by every CLI handler that accepts the §5.4 env parameter.
func envToMap(environ []string) map[string]string {
	m := make(map[string]string, len(environ))
	for _, e := range environ {
		for i := 0; i < len(e); i++ {
			if e[i] == '=' {
				m[e[:i]] = e[i+1:]
				break
			}
		}
	}
	return m
}

// RunCLI dispatches the CLI subcommand.
func RunCLI(cfg config.Config, args []string) {
	cmd := args[0]
	// Parse --out, --model (repeatable, CSV-friendly), --all-models, and
	// repeatable --exclude flags from anywhere in args. Keep the hand-rolled
	// loop (rest of cli.go relies on the same shape) but collect --model
	// into a slice so both `--model a,b` and `--model a --model b` reach
	// the resolver as a flat list.
	outPath := ""
	var modelFlags []string
	allModels := false
	var excludeFlags []string
	var cleanArgs []string
	for i := 1; i < len(args); i++ {
		if (args[i] == "--out" || args[i] == "-o") && i+1 < len(args) {
			outPath = args[i+1]
			i++
		} else if args[i] == "--model" && i+1 < len(args) {
			modelFlags = append(modelFlags, args[i+1])
			i++
		} else if args[i] == "--all-models" {
			allModels = true
		} else if args[i] == "--exclude" && i+1 < len(args) {
			excludeFlags = append(excludeFlags, args[i+1])
			i++
		} else {
			cleanArgs = append(cleanArgs, args[i])
		}
	}

	switch cmd {
	case "index":
		if len(cleanArgs) < 1 {
			fmt.Fprintf(os.Stderr, "Usage: heimdall-mcp index <path> [--out /path/to/output/dir] [--model <name>[,<name>...]]... [--all-models] [--exclude <pattern>]...\n")
			os.Exit(1)
		}
		if err := config.ValidateExcludePatterns(excludeFlags); err != nil {
			fmt.Fprintf(os.Stderr, "Invalid --exclude: %v\n", err)
			os.Exit(1)
		}
		cliIndex(cfg, cleanArgs[0], outPath, modelFlags, allModels, excludeFlags)
	case "status":
		// Status handler parses its own flags (including --out/-o and --format).
		os.Exit(CLIStatus(os.Stdin, os.Stdout, os.Stderr, envToMap(os.Environ()), args[1:], StatusDeps{}))
	case "recall":
		os.Exit(CLIRecall(os.Stdin, os.Stdout, os.Stderr, envToMap(os.Environ()), args[1:], RecallDeps{}))
	case "ingest-session":
		os.Exit(CLIIngestSession(os.Stdin, os.Stdout, os.Stderr, envToMap(os.Environ()), args[1:], IngestDeps{}))
	case "search":
		// Search parses its own flags (including --out, --format, --budget-ms,
		// --limit) because T4 added --format=hook-md and --budget-ms for the
		// user-prompt hook path. The cleanArgs top-level parser strips --out
		// already, but we pass the full args[1:] so --format etc are visible
		// to the nested flag.FlagSet.
		os.Exit(cliSearch(cfg, args[1:]))
	case "projects":
		cliProjects()
	case "configure", "config":
		cliConfigure(cleanArgs)
	case "paths":
		cliManagePaths(cfg, cleanArgs)
	case "models":
		cliModels(cfg)
	case "hooks":
		env := envMap()
		code := DispatchHooks(cfg, os.Stdin, os.Stdout, os.Stderr, env, cleanArgs)
		if code != 0 {
			os.Exit(code)
		}
	case "hook":
		env := envMap()
		code := DispatchHook(cfg, os.Stdin, os.Stdout, os.Stderr, env, cleanArgs)
		if code != 0 {
			os.Exit(code)
		}
	case "sessions":
		env := envMap()
		code := DispatchSessions(cfg, os.Stdin, os.Stdout, os.Stderr, env, cleanArgs)
		if code != 0 {
			os.Exit(code)
		}
	case "install-hooks":
		os.Exit(CLIInstallHooks(cfg, os.Stdin, os.Stdout, os.Stderr, envMap(), args[1:]))
	case "uninstall-hooks":
		os.Exit(CLIUninstallHooks(cfg, os.Stdin, os.Stdout, os.Stderr, envMap(), args[1:]))
	case "skills":
		os.Exit(CLISkills(cfg, os.Stdin, os.Stdout, os.Stderr, envMap(), cleanArgs, SkillsDeps{}))
	case "version", "--version", "-V":
		fmt.Println(buildVersion())
	case "help", "--help", "-h":
		fmt.Println("heimdall-mcp — local semantic code search + memory")
		fmt.Println()
		fmt.Println("CLI usage:")
		fmt.Println("  heimdall-mcp index <path> [--out <dir>] [--model <name>[,<name>...]]... [--all-models] [--exclude <pat>]...")
		fmt.Println("                                                   Index a directory.")
		fmt.Println("                                                   Nested git repos are indexed")
		fmt.Println("                                                   as separate projects, each")
		fmt.Println("                                                   with its own .heimdall_db/.")
		fmt.Println("                                                   --model accepts a comma-separated")
		fmt.Println("                                                   list and can be repeated; runs")
		fmt.Println("                                                   one pass per selected model.")
		fmt.Println("                                                   --all-models picks every")
		fmt.Println("                                                   embedding-capable Ollama model.")
		fmt.Println("                                                   --exclude is repeatable; it")
		fmt.Println("                                                   takes a glob or relative path,")
		fmt.Println("                                                   not an absolute path.")
		fmt.Println("  heimdall-mcp status [--out <dir>] [--format]     Show index stats")
		fmt.Println("  heimdall-mcp search <query> [--out <dir>]        Search indexed files")
		fmt.Println("  heimdall-mcp recall --query <text> [--format]    Recall memories")
		fmt.Println("  heimdall-mcp ingest-session --summary-stdin      Ingest a session summary")
		fmt.Println("  heimdall-mcp projects                            List registered projects")
		fmt.Println("  heimdall-mcp config get [key]              Get config (full or key)")
		fmt.Println("  heimdall-mcp config set <key> <value>      Set a config key")
		fmt.Println("  heimdall-mcp paths list                    List indexed paths")
		fmt.Println("  heimdall-mcp paths add <path>              Add a path to index")
		fmt.Println("  heimdall-mcp paths remove <path>           Remove a path")
		fmt.Println("  heimdall-mcp models                        List available embedding models")
		fmt.Println("  heimdall-mcp skills import [flags]         Import ~/.claude/skills/ into memory")
		fmt.Println("  heimdall-mcp hook session-start [flags]    Claude Code SessionStart retrieval hook")
		fmt.Println("  heimdall-mcp hooks tail [flags]            Tail the hook log with filters")
		fmt.Println("  heimdall-mcp hooks cache-clear [flags]     Drop hook_cache contents")
		fmt.Println("  heimdall-mcp hooks cache-stats [flags]     Show hook_cache stats")
		fmt.Println("  heimdall-mcp sessions list|report          Per-session savings metrics")
		fmt.Println("  heimdall-mcp version                       Print the binary version")
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

func cliIndex(cfg config.Config, path string, dbPath string, modelFlags []string, allModels bool, excludeFlags []string) {
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

	baseDir := dbPath
	if baseDir == "" {
		baseDir = filepath.Join(absPath, ".heimdall_db")
	}

	// Consult the on-disk resume marker BEFORE the Ollama ping so a
	// flag-only run doesn't flap the user through an unnecessary network
	// probe. The marker's models feed resolveIndexModels as pre-selection
	// (TTY) or auto-continue list (non-TTY).
	var markerModels []string
	if marker, err := heimdall.ReadResumeMarker(baseDir); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: unreadable resume marker at %s: %v (ignoring)\n",
			heimdall.ResumeMarkerPath(baseDir), err)
		_ = heimdall.DeleteResumeMarker(baseDir)
	} else if marker != nil {
		markerModels = append([]string{}, marker.Models...)
		fmt.Printf("\nPrevious indexing of %s with models %v was interrupted on %s.\n",
			absPath, marker.Models, marker.StartTime.Format("2006-01-02 15:04:05 UTC"))
	}

	ctx := context.Background()
	client := newOllamaClient(cfg)
	fmt.Printf("Connecting to Ollama at %s...\n", cfg.OllamaEndpoint)
	if err := client.Ping(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Ollama not reachable: %v\n", err)
		fmt.Fprintf(os.Stderr, "\nInstall: https://ollama.ai\nStart:   ollama serve\n")
		os.Exit(1)
	}
	fmt.Println("Ollama: connected")

	fmt.Println("\nDiscovering embedding models...")
	discovered, err := discoverEmbeddingModels(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Model discovery failed: %v\n", err)
		os.Exit(1)
	}
	formatDiscoveryResults(discovered)
	fmt.Println()

	// Resolve models via the unified selector. TTY → multi-select with
	// marker + cfg pre-checked; non-TTY → silent auto-pick from marker /
	// config / flags.
	selectedModels, fromMarker, err := resolveIndexModels(resolveOpts{
		Discovered:   discovered,
		ModelFlags:   modelFlags,
		AllModels:    allModels,
		ConfigModel:  cfg.Model,
		MarkerModels: markerModels,
		IsTTY:        isStdinTTY(),
	})
	if err != nil {
		if errors.Is(err, errNoModelsSelected) {
			// Clean exit: user opened the prompt and submitted empty.
			// Leave any marker on disk so a later run can still resume.
			fmt.Println("No models selected. Nothing to index.")
			return
		}
		fmt.Fprintf(os.Stderr, "\n%v\n", err)
		os.Exit(1)
	}

	// Write the resume marker BEFORE the first indexWithModel call so an
	// interruption mid-indexing leaves a marker for the next run. Writing
	// after model selection means restarts pick up exactly the set the
	// user picked this time. If writing fails we surface the error but
	// continue — missing a marker is worse than a silent-write bug, not
	// fatal to indexing itself. (If marker write ever fails, the next run
	// will just look like a fresh start.)
	//
	// Skip the re-write on the non-TTY "resume" path where the marker on
	// disk already describes exactly this run — re-writing would be a
	// harmless no-op but avoiding the extra syscall keeps the log tidy.
	_ = fromMarker
	if err := heimdall.WriteResumeMarker(baseDir, selectedModels, time.Now()); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to write resume marker: %v\n", err)
	}

	// Effective exclude list: global config + this invocation's --exclude
	// flags. Per-invocation flags are additive, never persisted — users who
	// want global excludes run `heimdall-mcp config set exclude_patterns ...`.
	effectiveExcludes := append([]string{}, cfg.ExcludePatterns...)
	effectiveExcludes = append(effectiveExcludes, excludeFlags...)

	reg := registry.LoadRegistry()
	name := filepath.Base(absPath)

	// Index with each selected model. For each model, we ALSO run the
	// sub-repo pass so every sub-repo gets a store under that same model.
	// The sub-repo pass uses caller-wins semantics — existing subdirs for
	// OTHER models coexist untouched (see heimdall.IndexSubRepos doc).
	for i, modelName := range selectedModels {
		if i > 0 {
			fmt.Println()
		}
		// Pass the registry so indexWithModel can register each sub-repo
		// unconditionally via IndexSubRepos.OnSubRepoDiscovered — that
		// fires after successful store-open but BEFORE the incremental
		// short-circuit, so no-op runs still refresh the registry. The
		// downstream loop below is the idempotent safety net.
		subResults := indexWithModel(ctx, cfg, client, absPath, baseDir, modelName, effectiveExcludes, reg)

		// Idempotent safety net: re-register each sub-repo that got as far
		// as producing a Result. Registry.Register tuple-dedupes, so this
		// is a no-op for entries already registered during discovery.
		for _, sr := range subResults {
			if sr.Err != nil || sr.Result == nil {
				continue
			}
			reg.Register(sr.Name, sr.Path, sr.DBPath)
		}
	}

	// Register outer project in registry (uses base dir)
	reg.Register(name, absPath, baseDir)
	if err := reg.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to save project registry: %v\n", err)
	}

	// Clean completion: remove the resume marker. Any future run starts
	// fresh. If the delete fails (permissions, fs race) we surface a
	// warning but do not exit non-zero — indexing succeeded, the stale
	// marker will just prompt on the next run.
	if err := heimdall.DeleteResumeMarker(baseDir); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to remove resume marker: %v\n", err)
	}

	if len(selectedModels) > 1 {
		fmt.Printf("\nDone. %d indexes created.\n", len(selectedModels))
	}

	// OQ-2 follow-up: hint about install-hooks if not already installed.
	if !hooksDetected(envMap()) {
		fmt.Fprintln(os.Stderr, "Tip: run `heimdall-mcp install-hooks` to have Claude Code use heimdall automatically.")
	}
}

// indexWithModel runs the outer indexing pass followed by a sub-repo pass
// and prints the categorized summary (plan §G6). Returns the sub-repo
// results so the caller can register them. reg (nullable) is wired into
// IndexSubRepos.OnSubRepoDiscovered so each sub-repo is registered at
// discovery time (before the incremental short-circuit) — the caller's
// post-loop Register call remains as an idempotent safety net.
func indexWithModel(ctx context.Context, cfg config.Config, client *heimdall.OllamaClient, absPath, baseDir, modelName string, excludeGlobs []string, reg *registry.Registry) []heimdall.SubRepoResult {
	heimdall.MigrateToModelDir(baseDir, modelName)
	if _, err := heimdall.MigrateLegacyLatestDir(baseDir, modelName); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: legacy-latest migration for %s: %v\n", baseDir, err)
	}
	dbDir := heimdall.ModelDBDir(baseDir, modelName)
	store, err := heimdall.OpenStore(dbDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Store error: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	embedder := heimdall.NewOllamaEmbedder(client, modelName)
	chunkerOpts := heimdall.ChunkerOpts{
		MaxChunkSize: 1500,
		ContextDepth: cfg.ContextDepth,
		ExcludeGlobs: excludeGlobs,
	}
	indexer := heimdall.NewIndexer(absPath, embedder, store, chunkerOpts)

	startTime := time.Now()
	fmt.Printf("Indexing %s...\n", absPath)
	fmt.Printf("Model:    %s\n", modelName)
	fmt.Printf("Started:  %s\n", startTime.Format("2006-01-02 15:04:05"))
	fmt.Printf("Database: %s\n\n", dbDir)

	progressCh := make(chan heimdall.IndexProgress, 64)
	indexer.IndexProjectAsync(ctx, progressCh)

	var outerResult *heimdall.IndexResult
	lastPrint := time.Now()
	for p := range progressCh {
		if p.Done {
			if p.Err != nil {
				fmt.Fprintf(os.Stderr, "\nIndexing failed: %v\n", p.Err)
				return nil
			}
			outerResult = p.Result
			// Stamp model metadata
			store.SetMetadata("embedding_model", modelName)
			if vec, err := embedder.Embed(ctx, "test"); err == nil {
				store.SetMetadata("embedding_dim", fmt.Sprintf("%d", len(vec)))
			}
			break
		}

		now := time.Now()
		if now.Sub(lastPrint) > 500*time.Millisecond || p.Current == p.Total {
			elapsed := now.Sub(startTime).Round(time.Second)
			eta := computeETA(p.ChunksSoFar, p.Current, p.Total, now.Sub(startTime))
			pct := ""
			if p.BytesTotal > 0 {
				pct = fmt.Sprintf(" %d%%", p.BytesDone*100/p.BytesTotal)
			}
			fmt.Printf("\r  [%s] %d/%d files%s (%d chunks)%s — %s\033[K",
				elapsed, p.Current, p.Total, pct, p.ChunksSoFar, eta, p.FilePath)
			lastPrint = now
		}
	}
	if outerResult == nil {
		return nil
	}

	// Sub-repo pass: index each discovered sub-repo as its own project.
	// Wire CLI callbacks so the user sees per-sub-repo progress instead of
	// minutes of silence while 60k+ files quietly embed (PR #67 regression).
	subOpts := newSubRepoCLIOpts(os.Stdout, startTime)
	if reg != nil {
		subOpts.OnSubRepoDiscovered = func(path, name, dbPath string) {
			reg.Register(name, path, dbPath)
		}
	}
	subResults, subErr := indexer.IndexSubRepos(ctx, modelName, subOpts)
	if subErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: sub-repo discovery failed: %v\n", subErr)
	}

	elapsed := time.Since(startTime).Round(time.Second)
	renderIndexSummary(os.Stdout, modelName, elapsed, dbDir, outerResult, subResults)
	return subResults
}

// renderIndexSummary prints the categorized outer-pass summary followed by
// the sub-repo list (plan §G6). The writer is abstracted so tests can
// capture the output without stubbing os.Stdout.
func renderIndexSummary(w io.Writer, model string, elapsed time.Duration, dbDir string, r *heimdall.IndexResult, subs []heimdall.SubRepoResult) {
	fmt.Fprintln(w)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Done.")
	fmt.Fprintf(w, "  Model:    %s\n", model)
	fmt.Fprintf(w, "  Elapsed:  %s\n", elapsed)
	fmt.Fprintf(w, "  Scanned:  %d files\n", r.FilesScanned)
	fmt.Fprintf(w, "  Indexed:  %d files\n", r.FilesIndexed)
	fmt.Fprintf(w, "  Skipped:  %d files\n", r.FilesSkipped)
	if r.Skip.Unchanged > 0 {
		fmt.Fprintf(w, "    └─ %d unchanged (incremental)\n", r.Skip.Unchanged)
	}
	if r.Skip.UserExcluded > 0 {
		fmt.Fprintf(w, "    └─ %d excluded by pattern\n", r.Skip.UserExcluded)
	}
	if r.Skip.Binary > 0 {
		fmt.Fprintf(w, "    └─ %d binary\n", r.Skip.Binary)
	}
	if r.Skip.SubRepo > 0 {
		// Sub-repo skip counts only reflect that a dir was NOT walked into
		// the outer store; the sub-repo pass indexes them separately.
		fmt.Fprintf(w, "    └─ %d sub-repo directories (indexed separately below)\n", r.Skip.SubRepo)
	}

	// Count successes, failures, and detect first-time sub-repo indexing so
	// we can emit the "may take longer" hint from plan §G6.
	var succeeded, failed, firstTime int
	for _, s := range subs {
		if s.Err != nil || s.Result == nil {
			failed++
			continue
		}
		succeeded++
		// "First time" heuristic: if the sub-repo had no pre-existing pinned
		// model, its store didn't exist before this run.
		if !s.PinnedModel {
			// If the caller's model equals the outer model, this run just
			// created the store — approximate "first time" as the store
			// having exactly one indexed file count equal to FilesIndexed
			// with no Unchanged hits (no prior run could produce Unchanged
			// records). Conservative: treat any non-pinned sub-repo with
			// Skip.Unchanged == 0 as newly created.
			if s.Result.Skip.Unchanged == 0 {
				firstTime++
			}
		}
	}
	if len(subs) > 0 {
		fmt.Fprintf(w, "  Sub-repos: %d indexed separately\n", succeeded)
		for _, s := range subs {
			if s.Err != nil || s.Result == nil {
				fmt.Fprintf(w, "    └─ %s  FAILED: %v\n", s.Name, s.Err)
				continue
			}
			pinnedSuffix := ""
			if s.PinnedModel {
				pinnedSuffix = " — pinned"
			}
			fmt.Fprintf(w, "    └─ %s  [%s%s]  (%d files, %d chunks)  → %s\n",
				s.Name, s.Model, pinnedSuffix, s.Result.FilesIndexed, s.Result.ChunksCreated, s.DBPath)
		}
		if failed > 0 {
			fmt.Fprintf(w, "    (note: %d sub-repo(s) failed — see entries above)\n", failed)
		}
		if firstTime > 0 {
			fmt.Fprintln(w, "  Note: first indexing of sub-repos may take longer; subsequent runs are incremental.")
		}
	}

	fmt.Fprintf(w, "  Chunks:   %d\n", r.ChunksCreated)
	fmt.Fprintf(w, "  Database: %s\n", dbDir)
}

// newSubRepoCLIOpts builds a heimdall.SubRepoOpts wired to write per-
// sub-repo progress to w. Used by indexWithModel so users see
// "Indexing sub-repo i/N: <name>" lines between the outer summary and
// each sub-repo's own summary, plus periodic heartbeats while each
// sub-repo runs (fixes the minutes-of-silence regression from PR #67).
//
// The outerStart parameter is accepted for future use (e.g. showing a
// cumulative wall-clock in the start line). Currently unused — per-sub-
// repo elapsed is measured from OnSubRepoStart → OnSubRepoDone.
//
// OnProgress emits at most one line every ~2 seconds so even a 60k-file
// sub-repo shows activity without flooding stdout. The line is
// overwritten in-place with \r just like the outer pass to keep output
// tidy — the \r style only works on a real TTY but degrades gracefully
// to repeated lines when w is a buffer or pipe (acceptable for tests).
func newSubRepoCLIOpts(w io.Writer, outerStart time.Time) heimdall.SubRepoOpts {
	_ = outerStart // reserved for future cumulative-elapsed display
	var (
		subStart  time.Time
		lastPrint time.Time
	)
	return heimdall.SubRepoOpts{
		OnSubRepoStart: func(name string, i, total int) {
			subStart = time.Now()
			lastPrint = time.Time{} // reset so first progress event prints
			fmt.Fprintf(w, "\nIndexing sub-repo %d/%d: %s\n", i, total, name)
		},
		OnProgress: func(p heimdall.IndexProgress) {
			now := time.Now()
			// Throttle to ~2s cadence; always print the final event so the
			// trailing state is visible before OnSubRepoDone's summary.
			if !lastPrint.IsZero() && now.Sub(lastPrint) < 2*time.Second && p.Current != p.Total {
				return
			}
			lastPrint = now
			elapsed := now.Sub(subStart).Round(time.Second)
			pct := ""
			if p.BytesTotal > 0 {
				pct = fmt.Sprintf(" %d%%", p.BytesDone*100/p.BytesTotal)
			}
			fmt.Fprintf(w, "\r  [%s] %d/%d files%s (%d chunks) — %s\033[K",
				elapsed, p.Current, p.Total, pct, p.ChunksSoFar, p.FilePath)
		},
		OnSubRepoDone: func(res heimdall.SubRepoResult) {
			elapsed := time.Since(subStart).Round(time.Second)
			fmt.Fprintln(w) // end the \r progress line
			if res.Err != nil || res.Result == nil {
				fmt.Fprintf(w, "  FAILED after %s: %v\n", elapsed, res.Err)
				return
			}
			fmt.Fprintf(w, "  Done: %d files, %d chunks, %s elapsed\n",
				res.Result.FilesIndexed, res.Result.ChunksCreated, elapsed)
		},
	}
}

// cliSearch parses `search <query> [--out <dir>] [--format text|hook-md]
// [--budget-ms N] [--limit N]` and runs the search. Returns a shell exit code.
//
// --format=hook-md emits the same `## Heimdall context` markdown shape as
// `hook session-start` + `hook user-prompt`, but with a "Relevant code"
// section built from search hits rather than memory bullets. That lets T6
// (`hook user-prompt`) call this path with a tight budget.
//
// --budget-ms wraps the whole run in a context.WithTimeout. On expiry the
// partial hits (if any) are still rendered — better to return truncated
// context than nothing.
func cliSearch(cfg config.Config, args []string) int {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var (
		outPath  string
		format   string
		budgetMs int
		limit    int
	)
	fs.StringVar(&outPath, "out", "", "base directory for the index (default: <cwd>/.heimdall_db)")
	fs.StringVar(&outPath, "o", "", "alias of --out")
	fs.StringVar(&format, "format", "text", "output format: text, hook-md")
	fs.IntVar(&budgetMs, "budget-ms", 0, "hard wall-clock deadline in ms (0 = no budget)")
	fs.IntVar(&limit, "limit", 5, "max number of hits to return")

	// Go's flag package stops parsing at the first non-flag arg, so
	// `search <query> --format=hook-md` would miss the format flag. Pre-sort
	// args into "flags first, positionals last" using the flag set as the
	// source of truth for which names are flags. The `--flag=value` form and
	// the bare-bool form are both handled.
	reordered := reorderFlagsFirst(fs, args)
	if err := fs.Parse(reordered); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: heimdall-mcp search <query> [--out <dir>] [--format text|hook-md] [--budget-ms N] [--limit N]\n")
		return 2
	}
	query := strings.Join(rest, " ")

	switch format {
	case "text", "hook-md":
	default:
		fmt.Fprintf(os.Stderr, "error: invalid --format %q (want text|hook-md)\n", format)
		return 2
	}
	if limit <= 0 {
		limit = 5
	}

	ctx := context.Background()
	if budgetMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(budgetMs)*time.Millisecond)
		defer cancel()
	}

	client := newOllamaClient(cfg)
	pingCtx, pingCancel := context.WithTimeout(ctx, 1*time.Second)
	pingErr := client.Ping(pingCtx)
	pingCancel()
	if pingErr != nil {
		if format == "hook-md" {
			return 0 // retrieval contract — stay quiet
		}
		fmt.Fprintf(os.Stderr, "Ollama not reachable: %v\n", pingErr)
		return 1
	}

	searchBaseDir := outPath
	if searchBaseDir == "" {
		cwd, _ := os.Getwd()
		searchBaseDir = filepath.Join(cwd, ".heimdall_db")
	}

	searchDbDir, resolvedModel := resolveAnyLocalModelDB(cfg, searchBaseDir)
	if searchDbDir == "" {
		if format == "hook-md" {
			return 0
		}
		available := heimdall.ListAvailableModels(searchBaseDir)
		if len(available) > 0 {
			fmt.Fprintf(os.Stderr, "Indexes exist for %v but none of those models are pulled in Ollama.\n", available)
			fmt.Fprintf(os.Stderr, "Run: ollama pull <model>\n")
		} else {
			fmt.Fprintf(os.Stderr, "No index found. Run: heimdall-mcp index <path>\n")
		}
		return 1
	}
	if format == "text" {
		fmt.Printf("Using model: %s\n", resolvedModel)
	}

	store, err := heimdall.OpenStore(searchDbDir)
	if err != nil {
		if format == "hook-md" {
			return 0
		}
		fmt.Fprintf(os.Stderr, "Store error: %v\n", err)
		return 1
	}
	defer store.Close()

	embedder := heimdall.NewOllamaEmbedder(client, resolvedModel)
	queryVec, err := embedder.Embed(ctx, query)
	if err != nil {
		if format == "hook-md" {
			return 0
		}
		fmt.Fprintf(os.Stderr, "Embed error: %v\n", err)
		return 1
	}

	results := store.SearchFiltered(ctx, queryVec, limit, "", "", nil)

	if format == "hook-md" {
		// searchBaseDir is always .../.heimdall_db; the project name is its parent's base.
		projectName := filepath.Base(filepath.Dir(searchBaseDir))
		body := formatSearchHookMD(query, resolvedModel, projectName, results)
		body = capRunes(body, sessionStartMaxRunes)
		fmt.Print(body)
		return 0
	}

	if len(results) == 0 {
		fmt.Println("No results found.")
		return 0
	}
	for i, r := range results {
		fmt.Printf("\n--- %s (L%d-%d, score: %.2f) ---\n", r.Record.FilePath, r.Record.StartLine, r.Record.EndLine, r.Similarity)
		fmt.Println(r.Record.Content)
		if i < len(results)-1 {
			fmt.Println()
		}
	}
	return 0
}

// reorderFlagsFirst partitions args into [flags..., positionals...] so
// stdlib flag.Parse — which stops at the first non-flag token — sees every
// flag regardless of where the user typed it. Uses `fs` as the source of
// truth for which names are known flags (so arbitrary positional tokens that
// happen to start with `-` don't get misclassified).
//
// Supported forms per token:
//   - `--flag=value` or `-flag=value` → single token, routed to flags
//   - `--flag value` or `-flag value` → two tokens, both routed to flags
//   - anything else                   → positional
func reorderFlagsFirst(fs *flag.FlagSet, args []string) []string {
	var flags, positionals []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if len(a) < 2 || a[0] != '-' || a == "-" || a == "--" {
			positionals = append(positionals, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		value := ""
		hasValue := false
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			value = name[eq+1:]
			name = name[:eq]
			hasValue = true
		}
		f := fs.Lookup(name)
		if f == nil {
			// Not a known flag — treat as positional so `search -foo` doesn't
			// silently eat the next token.
			positionals = append(positionals, a)
			continue
		}
		flags = append(flags, a)
		if !hasValue && !isBoolFlag(f) && i+1 < len(args) {
			flags = append(flags, args[i+1])
			i++
		}
		_ = value
	}
	return append(flags, positionals...)
}

// isBoolFlag returns true if the flag's underlying Value reports itself as a
// bool (implements the `IsBoolFlag() bool` interface per flag package docs).
// Bool flags don't consume a following token.
func isBoolFlag(f *flag.Flag) bool {
	type boolFlag interface {
		IsBoolFlag() bool
	}
	if bf, ok := f.Value.(boolFlag); ok {
		return bf.IsBoolFlag()
	}
	return false
}

// formatSearchHookMD renders search hits as a `## Heimdall context` block
// suitable for injection via a retrieval hook. Matches the shape of
// formatSessionStartBlock so Claude sees one consistent banner shape
// regardless of which hook emitted it.
func formatSearchHookMD(query, model, projectName string, results []heimdall.SearchResult) string {
	var b strings.Builder
	b.Grow(256 + 120*len(results))

	b.WriteString("## Heimdall context\n\n")
	fmt.Fprintf(&b, "**Project:** %s  •  **Model:** %s  •  **Hits:** %d\n", projectName, model, len(results))
	fmt.Fprintf(&b, "**Query:** %s\n", singleLine(query))

	if len(results) > 0 {
		b.WriteString("\n### Relevant code\n")
		for _, r := range results {
			first := singleLine(r.Record.Content)
			if len(first) > 120 {
				first = first[:117] + "..."
			}
			fmt.Fprintf(&b, "- `%s:%d-%d` — %s\n", r.Record.FilePath, r.Record.StartLine, r.Record.EndLine, first)
		}
	}

	b.WriteString("\n_retrieved via heimdall-mcp_\n")
	return b.String()
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

		// Show available models and stats per model
		models := heimdall.ListAvailableModels(p.DBPath)
		if len(models) > 0 {
			for _, m := range models {
				modelDir := filepath.Join(p.DBPath, m)
				store, err := heimdall.OpenStore(modelDir)
				if err == nil {
					stats := store.Stats()
					fmt.Printf("    [%s] Files: %d  Chunks: %d", m, stats.TotalFiles, stats.TotalRecords)
					if stats.LastModified > 0 {
						fmt.Printf("  Last indexed: %s", time.Unix(stats.LastModified, 0).Format("2006-01-02 15:04:05"))
					}
					fmt.Println()
					store.Close()
				}
			}
		}
	}
}

func cliConfigure(args []string) {
	// Plan 11 §5.4 / 11a §5.4 item 7: support
	//   heimdall-mcp configure --llm-classifier-model=<model>
	// as a one-shot shortcut. The `config set llm_classifier_model <v>`
	// surface still works (see setConfigKey) and is the preferred
	// programmatic entry; this flag exists for copy-paste from the
	// plan 11 docs and the `hooks doctor` hint.
	for _, a := range args {
		const pfx = "--llm-classifier-model="
		if strings.HasPrefix(a, pfx) {
			model := strings.TrimPrefix(a, pfx)
			cfg := config.LoadConfig()
			cfg.LLMClassifierModel = model
			if err := config.SaveConfig(cfg); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to save config: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("llm_classifier_model = %q (saved)\n", model)
			if model != "" {
				fmt.Printf("Hint: ollama pull %s\n", model)
				fmt.Println("      set HEIMDALL_LLM_CLASSIFIER=1 to enable the fallback")
			}
			return
		}
	}

	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  heimdall-mcp config get [key]                   Get config (full or specific key)\n")
		fmt.Fprintf(os.Stderr, "  heimdall-mcp config set <key> <val>             Set a config key\n")
		fmt.Fprintf(os.Stderr, "  heimdall-mcp configure --llm-classifier-model=<model>\n")
		fmt.Fprintf(os.Stderr, "                                                  One-shot setter for the LLM classifier fallback\n")
		fmt.Fprintf(os.Stderr, "\nKeys: model, git.enabled, git.depth, git.include_diffs, git.branches,\n")
		fmt.Fprintf(os.Stderr, "      stale_timeout_minutes, lifecycle.active_days,\n")
		fmt.Fprintf(os.Stderr, "      lifecycle.archive_days, max_chunks_per_project,\n")
		fmt.Fprintf(os.Stderr, "      llm_classifier_model, exclude_patterns\n")
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
			client := newOllamaClient(cfg)
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
	case "llm_classifier_model":
		return cfg.LLMClassifierModel, true
	case "exclude_patterns":
		return cfg.ExcludePatterns, true
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
	case "llm_classifier_model":
		cfg.LLMClassifierModel = rawVal
	case "exclude_patterns":
		var patterns []string
		if err := json.Unmarshal([]byte(rawVal), &patterns); err != nil {
			return fmt.Errorf("exclude_patterns requires a JSON array: %w", err)
		}
		if err := config.ValidateExcludePatterns(patterns); err != nil {
			return err
		}
		cfg.ExcludePatterns = patterns
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
	client := newOllamaClient(cfg)

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
	fmt.Println("To pull a model: ollama pull <name>")
}

// resolveAnyLocalModelDB finds any usable model index in a base dir.
// Thin CLI-side wrapper over heimdall.ResolveUsableModelDB.
func resolveAnyLocalModelDB(cfg config.Config, baseDir string) (string, string) {
	client := newOllamaClient(cfg)
	return heimdall.ResolveUsableModelDB(context.Background(), client, baseDir, cfg.Model)
}
