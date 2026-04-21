// install.go — implements T14 `install-hooks` and T15 `uninstall-hooks`.
//
// Both commands read/write Claude Code's `settings.json` using the stdlib
// `encoding/json` parser (per OQ-4). They MUST be neighborly: never clobber
// non-heimdall hooks, always write a timestamped backup before mutating, and
// never surprise the user in `--dry-run` mode.
//
// Marker detection is the union of two patterns (OQ-1 belt-and-braces):
//   1. the entry object has `"source": "heimdall"`
//   2. any `hooks[].command` string in the entry contains `--source=heimdall`
//
// Either is sufficient; either must be honored by uninstall and doctor.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// heimdallHookVersion is the integer version stamped onto every hook entry
// heimdall installs. Bump this when the command-string shape changes so that
// upgrades detect drift and reinstall.
const heimdallHookVersion = 1

// heimdallBinaryVersion is the string recorded in the `x-heimdall` metadata
// bag on installed hook entries. Distinct from heimdallHookVersion because the
// binary can churn without changing the hook shape. Bumped to "wave2-phase4"
// when the SessionEnd entry gained an explicit `timeout` field — Claude Code's
// default 1.5 s SessionEnd budget was killing the P0 ingest path mid-embed
// (see docs/plans/claude-heimdall-self-use/04-session-end-regression-handoff.md).
// autoUpgradeHooks uses this string on SessionStart to refresh stale heimdall
// entries in-place so shape drift heals without a manual reinstall.
const heimdallBinaryVersion = "wave2-phase4"

// sessionEndTimeoutSeconds overrides Claude Code's per-hook 1500 ms SessionEnd
// budget. Real sessions with ≥10 memories to ingest blow past 1.5 s on the
// embedder round-trips; the harness SIGTERMs the subprocess before any log
// line lands, leaving no evidence the hook fired. 30 s is a generous ceiling
// that still lets the outer `/exit` path return quickly on well-behaved runs.
const sessionEndTimeoutSeconds = 30

// phase1aHook describes one Claude Code hook entry heimdall owns. The list
// below is the canonical Phase 1a install set; `--only` filters over it by
// event name.
type phase1aHook struct {
	event          string // Claude Code hook event (e.g. "SessionStart")
	matcher        string // empty if the event has no matcher
	command        string // full command string including --source=heimdall
	timeoutSeconds int    // per-command timeout in seconds; 0 = harness default
}

// phase1aHooks is the canonical install set. The variable name is preserved
// for call-site stability; the list has grown across waves:
//   - Phase 1a: SessionStart, PostToolUse(Edit|Write)
//   - Phase 1b: +UserPromptSubmit
//   - Phase 2:  +Stop, +SessionEnd
//   - Phase 3:  +PreToolUse(Bash) guardrail — ships in shadow mode by default
//
// Command strings here are baked verbatim into `settings.json`; changes
// must be coordinated with the hook handlers in `hook.go`,
// `hook_post_edit.go`, `hook_user_prompt.go`, `hook_stop.go`, and
// `hook_pre_tool_use.go`.
var phase1aHooks = []phase1aHook{
	{
		event:   "SessionStart",
		matcher: "",
		command: "heimdall-mcp hook session-start --source=heimdall --version=1",
	},
	{
		event:   "PostToolUse",
		matcher: "Edit|Write",
		command: "heimdall-mcp hook post-edit --source=heimdall --version=1",
	},
	{
		event:   "UserPromptSubmit",
		matcher: "",
		command: "heimdall-mcp hook user-prompt --source=heimdall --version=1",
	},
	{
		event:   "Stop",
		matcher: "",
		command: "heimdall-mcp hook stop --source=heimdall --version=1",
	},
	{
		event:          "SessionEnd",
		matcher:        "",
		command:        "heimdall-mcp hook session-end --source=heimdall --version=1",
		timeoutSeconds: sessionEndTimeoutSeconds,
	},
	{
		// Phase 3 — destructive-op guardrails. Ships in shadow mode by
		// default: the hook always exits 0 and never emits unless the user
		// opts in via HEIMDALL_GUARDRAILS=warn|block.
		event:   "PreToolUse",
		matcher: "Bash",
		command: "heimdall-mcp hook pre-tool-use --source=heimdall --version=1",
	},
}

// installFlags captures the parsed flags for install-hooks.
type installFlags struct {
	scope          string // "user" | "project" | "" (auto-detect)
	dryRun         bool
	merge          bool
	force          bool
	only           []string // subset of phase1aHooks event names; empty = all
	hasOnly        bool
	noPrewarm      bool // --no-prewarm: skip the post-install Ollama warm-up
	noSkillsImport bool // --no-skills-import: skip the post-install skills import
}

// prewarmTimeout caps how long the post-install Ollama warm-up may run.
// Best-effort: if Ollama is cold or absent we do not want to keep the user
// waiting on `install-hooks`. Five seconds is plenty for a hot model and a
// soft ceiling for a cold one (model load can blow past this; that is fine,
// the prewarm just bails and the install still succeeds).
const prewarmTimeout = 5 * time.Second

// prewarmInput is the tiny fixed text we ask Ollama to embed during the
// install-time warm-up. The contents do not matter beyond being short and
// stable — we discard the resulting vector. Stable text means a hot model
// can serve it from internal caches with sub-100ms latency.
const prewarmInput = "heimdall prewarm"

// prewarmResult holds the outcome of a single prewarm attempt so the install
// handler can render a one-line status to the user.
type prewarmResult struct {
	model    string        // model that was warmed (echoed in the success line)
	duration time.Duration // wall-clock time the prewarm took
	err      error         // nil on success; non-nil descriptions are surfaced verbatim
}

// prewarmFunc is the dependency-injected shape used by CLIInstallHooks.
// Production wires it to runPrewarmOllama; tests swap it for a stub that
// records the call (or simulates timeouts/failures) without touching a real
// Ollama process. The function MUST NOT panic — it is best-effort and any
// error is logged and ignored.
type prewarmFunc func(ctx context.Context, endpoint, model string) prewarmResult

// prewarmFn is the live indirection point. Tests reset this via the small
// stubPrewarm test helper to inject stubs.
var prewarmFn prewarmFunc = runPrewarmOllama

// runPrewarmOllama is the production prewarm implementation: open an Ollama
// client, fire one EmbedForHook call against the given model with the tiny
// fixed input, discard the result. Caller is responsible for the timeout.
func runPrewarmOllama(ctx context.Context, endpoint, model string) prewarmResult {
	res := prewarmResult{model: model}
	start := time.Now()
	client := newOllamaClientForEndpoint(endpoint)
	_, err := client.EmbedForHook(ctx, model, prewarmInput)
	res.duration = time.Since(start)
	res.err = err
	return res
}

// skillsImportResult holds the outcome of a single best-effort skills import
// attempt so the install handler can render a one-line status. Mirrors the
// prewarm result shape: a one-line success/skip/failure summary is enough;
// any deeper diagnostics land in the hook log.
type skillsImportResult struct {
	dir      string              // resolved skills dir (echoed in the status line)
	result   heimdall.SkillImportResult
	duration time.Duration // wall-clock time the import took
	err      error         // nil on success; non-nil reasons are surfaced verbatim
}

// skillsImportFunc is the dependency-injected shape used by CLIInstallHooks.
// Production wires it to runSkillsImport; tests swap it for a stub that
// records the call (or simulates failures) without touching a real memory
// store or Ollama. The function MUST NOT panic — it is best-effort and any
// error is logged and ignored.
type skillsImportFunc func(ctx context.Context, cfg config.Config, env map[string]string) skillsImportResult

// skillsImportFn is the live indirection point. Tests reset this via the
// small stubSkillsImport test helper to inject stubs.
var skillsImportFn skillsImportFunc = runSkillsImport

// runSkillsImport is the production skills-import implementation: resolve
// the skills directory, open the global memory store, construct an Ollama
// embedder, and delegate to heimdall.ImportSkillsFromDir. This is the same
// code path as `heimdall-mcp skills import` (see cliSkillsImport) but with
// no flag parsing and no stdout emission — the install handler renders the
// status. Errors are surfaced in the returned result; caller decides.
//
// Named return value (`res`) lets the single trailing defer stamp duration
// onto every code path uniformly — assigning to a local then `return res`
// would copy-before-defer and leave duration at zero on the happy path.
func runSkillsImport(ctx context.Context, cfg config.Config, env map[string]string) (res skillsImportResult) {
	start := time.Now()
	defer func() {
		res.duration = time.Since(start)
	}()

	dir := heimdall.ResolveClaudeSkillsDir("", env)
	if dir == "" {
		res.err = errors.New("could not resolve skills directory (set HEIMDALL_CLAUDE_SKILLS_DIR or HOME)")
		return
	}
	res.dir = dir

	store, err := heimdall.OpenMemoryStore(config.ResolveMemoryDBPath())
	if err != nil {
		res.err = fmt.Errorf("open memory store: %w", err)
		return
	}
	defer store.Close()

	client := newOllamaClient(cfg)
	if err := client.Ping(ctx); err != nil {
		res.err = fmt.Errorf("ollama not reachable at %s: %w", cfg.OllamaEndpoint, err)
		return
	}
	emb := heimdall.NewOllamaEmbedder(client, cfg.Model)
	embedFn := func(content string) ([]float32, error) {
		return emb.Embed(ctx, content)
	}

	out, importErr := heimdall.ImportSkillsFromDir(store, heimdall.SkillImportOpts{
		Dir:   dir,
		Embed: embedFn,
	})
	res.result = out
	res.err = importErr
	return
}

// uninstallFlags captures the parsed flags for uninstall-hooks.
type uninstallFlags struct {
	scope  string
	dryRun bool
}

// CLIInstallHooks implements `heimdall-mcp install-hooks` per §5.4 testable
// handler shape. Never calls os.Exit; returns 0 on success, 1 on runtime
// error, 2 on usage error.
func CLIInstallHooks(cfg config.Config, stdin io.Reader, stdout, stderr io.Writer, env map[string]string, args []string) int {
	_ = stdin
	flags, err := parseInstallFlags(args)
	if err != nil {
		fmt.Fprintf(stderr, "install-hooks: %v\n", err)
		return 2
	}

	scope, settingsPath, err := resolveScope(flags.scope, env)
	if err != nil {
		fmt.Fprintf(stderr, "install-hooks: %v\n", err)
		return 1
	}

	current, originalBytes, err := readSettings(settingsPath)
	if err != nil {
		fmt.Fprintf(stderr, "install-hooks: %v\n", err)
		return 1
	}

	desired := pickHooks(phase1aHooks, flags.only)
	if len(desired) == 0 {
		fmt.Fprintln(stderr, "install-hooks: --only matched no known hooks")
		return 2
	}

	updated, report, err := applyInstall(current, desired, flags)
	if err != nil {
		fmt.Fprintf(stderr, "install-hooks: %v\n", err)
		return 1
	}

	// If applyInstall determined that there is nothing to do (same-version
	// entries already present and no forced overwrite), report and exit 0.
	if report.noop {
		fmt.Fprintln(stdout, "Already installed (same version). Nothing to do.")
		return 0
	}

	// Serialize the updated settings so we can render a diff and/or write.
	newBytes, err := marshalSettings(updated)
	if err != nil {
		fmt.Fprintf(stderr, "install-hooks: marshal failed: %v\n", err)
		return 1
	}

	if flags.dryRun {
		fmt.Fprintf(stdout, "scope=%s\nsettings=%s\n\n", scope, settingsPath)
		writeDiff(stdout, originalBytes, newBytes)
		if len(report.replaced) > 0 {
			fmt.Fprintf(stdout, "\nWould replace %d conflicting entries.\n", len(report.replaced))
		}
		if report.upgraded {
			fmt.Fprintln(stdout, "Would upgrade heimdall hooks to current version.")
		}
		// Mirror prewarm's contract: dry-run doesn't fire either post-install
		// step, but we DO announce the skills-import intent so the user can
		// preview it (unlike prewarm, skills import has a visible side-effect
		// on the global memory store and is worth surfacing). --no-skills-import
		// suppresses the announcement too.
		if !flags.noSkillsImport {
			dir := heimdall.ResolveClaudeSkillsDir("", env)
			if dir == "" {
				fmt.Fprintln(stdout, "Would run skills import (skills directory unresolved — set HEIMDALL_CLAUDE_SKILLS_DIR or HOME)")
			} else {
				fmt.Fprintf(stdout, "Would run skills import from %s\n", dir)
			}
		}
		return 0
	}

	// Non-dry-run: take a backup first, then atomically write.
	backup, err := writeBackup(settingsPath, originalBytes)
	if err != nil {
		fmt.Fprintf(stderr, "install-hooks: backup failed: %v\n", err)
		return 1
	}
	if backup != "" {
		fmt.Fprintf(stdout, "Backup: %s\n", backup)
	}
	if err := atomicWrite(settingsPath, newBytes); err != nil {
		fmt.Fprintf(stderr, "install-hooks: write failed: %v\n", err)
		return 1
	}
	if report.upgraded {
		fmt.Fprintln(stdout, "Upgraded heimdall hooks to current version.")
	}
	if len(report.replaced) > 0 {
		fmt.Fprintf(stdout, "Replaced %d conflicting entries.\n", len(report.replaced))
	}
	fmt.Fprintf(stdout, "Installed %d hooks (scope=%s, file=%s).\n", len(desired), scope, settingsPath)

	// Pre-warm Ollama so the very first SessionStart / PostToolUse the user
	// hits doesn't pay the cold-load tax (see docs/plans/hooks/07 caveat —
	// first real PostToolUse(Edit) measured at ~67s with a cold model).
	// Best-effort: timeout-bounded, gated by --no-prewarm, never fails the
	// install. --dry-run skips entirely (we never wrote settings.json).
	doPrewarm(stdout, cfg, flags.noPrewarm, "install-hooks")

	// Run `skills import` as a best-effort post-install step so that the
	// very first SessionStart / UserPromptSubmit hits can surface any
	// `type=skill` memories without the user having to remember to run
	// `heimdall-mcp skills import` manually. Mirrors the prewarm shape:
	// gated by --no-skills-import, never fails the install, --dry-run
	// skips entirely (we never wrote settings.json).
	doSkillsImport(stdout, cfg, env, flags.noSkillsImport, "install-hooks")

	fmt.Fprintln(stdout, "Run 'heimdall-mcp hooks doctor' to verify.")
	return 0
}

// doPrewarm renders the install-time Ollama warm-up. Single status line on
// stdout regardless of outcome:
//
//	Pre-warming Ollama...
//	Pre-warmed model=<name> in <ms>ms        (success)
//	Pre-warm skipped (<reason>)              (failure / disabled / no model)
//
// Caller passes `source` so the LogHookEvent payload distinguishes
// install-hooks vs auto-upgrade. Never returns an error — the install must
// succeed even if Ollama is down or the model is missing.
func doPrewarm(stdout io.Writer, cfg config.Config, disabled bool, source string) {
	if disabled {
		fmt.Fprintln(stdout, "Pre-warm skipped (--no-prewarm)")
		heimdall.LogHookEvent("INFO", source, map[string]any{
			"stage":  "prewarm",
			"reason": "disabled",
		})
		return
	}
	if cfg.Model == "" {
		// No configured model means we have nothing to warm. This is the
		// degraded case where the user runs install-hooks before configuring
		// or indexing — skip cleanly rather than guess at a default.
		fmt.Fprintln(stdout, "Pre-warm skipped (no model configured)")
		heimdall.LogHookEvent("INFO", source, map[string]any{
			"stage":  "prewarm",
			"reason": "no_model",
		})
		return
	}

	fmt.Fprintln(stdout, "Pre-warming Ollama...")
	ctx, cancel := context.WithTimeout(context.Background(), prewarmTimeout)
	defer cancel()
	res := prewarmFn(ctx, cfg.OllamaEndpoint, cfg.Model)
	if res.err != nil {
		// Trim any leading/trailing whitespace from the error so the one-line
		// status stays compact — Ollama errors are typically already tidy.
		reason := strings.TrimSpace(res.err.Error())
		if reason == "" {
			reason = "unknown"
		}
		fmt.Fprintf(stdout, "Pre-warm skipped (%s)\n", reason)
		heimdall.LogHookEvent("INFO", source, map[string]any{
			"stage":     "prewarm",
			"err":       res.err.Error(),
			"model":     res.model,
			"duration":  res.duration.String(),
			"timeoutMs": prewarmTimeout.Milliseconds(),
		})
		return
	}
	ms := res.duration.Milliseconds()
	fmt.Fprintf(stdout, "Pre-warmed model=%s in %dms\n", res.model, ms)
	heimdall.LogHookEvent("INFO", source, map[string]any{
		"stage":      "prewarm",
		"msg":        "ok",
		"model":      res.model,
		"durationMs": ms,
	})
}

// doSkillsImport renders the install-time skills-import step. Single status
// line on stdout regardless of outcome:
//
//	skills import: imported N skills in Xms (created=… updated=… unchanged=… errors=…)  (success; empty skills dir hits this too with zero counts)
//	skills import: <err> (non-fatal)                  (failure)
//	skills import: skipped (--no-skills-import)       (disabled)
//	skills import: skipped (no model configured)     (cfg.Model == "")
//
// `source` distinguishes install-hooks vs future auto-upgrade callers in
// the hook log. Never returns an error — the install must succeed even if
// the skills directory is empty, Ollama is down, or the memory store is
// unwritable. A failed skills import just means the user's first
// UserPromptSubmit won't have `type=skill` context injection on the very
// first turn; they can always run `heimdall-mcp skills import` manually.
func doSkillsImport(stdout io.Writer, cfg config.Config, env map[string]string, disabled bool, source string) {
	if disabled {
		fmt.Fprintln(stdout, "skills import: skipped (--no-skills-import)")
		heimdall.LogHookEvent("INFO", source, map[string]any{
			"stage":  "skills_import",
			"reason": "disabled",
		})
		return
	}
	if cfg.Model == "" {
		// No configured model means we cannot embed, and ImportSkillsFromDir
		// will refuse without an Embed func. Skip cleanly with the same
		// degraded-install message the prewarm uses.
		fmt.Fprintln(stdout, "skills import: skipped (no model configured)")
		heimdall.LogHookEvent("INFO", source, map[string]any{
			"stage":  "skills_import",
			"reason": "no_model",
		})
		return
	}

	// ImportSkillsFromDir has its own internal Ollama client with default
	// HTTP timeouts; we do NOT wrap it in a 5s context like prewarm because
	// a real skill corpus can legitimately take longer than that to embed
	// end-to-end (each SKILL.md is one Embed call, and chunked skills are
	// several). The outer `context.Background()` lets the embedder's per-
	// call timeout govern.
	ctx := context.Background()
	res := skillsImportFn(ctx, cfg, env)
	ms := res.duration.Milliseconds()
	if res.err != nil {
		reason := strings.TrimSpace(res.err.Error())
		if reason == "" {
			reason = "unknown"
		}
		fmt.Fprintf(stdout, "skills import: %s (non-fatal)\n", reason)
		heimdall.LogHookEvent("WARN", source, map[string]any{
			"stage":      "skills_import",
			"err":        res.err.Error(),
			"dir":        res.dir,
			"durationMs": ms,
		})
		return
	}

	// Success: summarize created + updated + unchanged in one line.
	imported := res.result.Created + res.result.Updated
	fmt.Fprintf(stdout, "skills import: imported %d skills in %dms (created=%d updated=%d unchanged=%d errors=%d)\n",
		imported, ms, res.result.Created, res.result.Updated, res.result.Unchanged, len(res.result.Errors))
	heimdall.LogHookEvent("INFO", source, map[string]any{
		"stage":      "skills_import",
		"msg":        "ok",
		"dir":        res.dir,
		"durationMs": ms,
		"created":    res.result.Created,
		"updated":    res.result.Updated,
		"unchanged":  res.result.Unchanged,
		"skipped":    res.result.Skipped,
		"errors":     len(res.result.Errors),
	})
}

// CLIUninstallHooks implements `heimdall-mcp uninstall-hooks` per §5.4.
func CLIUninstallHooks(cfg config.Config, stdin io.Reader, stdout, stderr io.Writer, env map[string]string, args []string) int {
	_ = cfg
	_ = stdin
	flags, err := parseUninstallFlags(args)
	if err != nil {
		fmt.Fprintf(stderr, "uninstall-hooks: %v\n", err)
		return 2
	}

	scope, settingsPath, err := resolveScope(flags.scope, env)
	if err != nil {
		fmt.Fprintf(stderr, "uninstall-hooks: %v\n", err)
		return 1
	}

	current, originalBytes, err := readSettings(settingsPath)
	if err != nil {
		// If settings.json simply doesn't exist, uninstall is a no-op. Same
		// for an effectively-empty file. Idempotent by design.
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(stdout, "No heimdall hooks found, nothing to do.")
			return 0
		}
		fmt.Fprintf(stderr, "uninstall-hooks: %v\n", err)
		return 1
	}

	updated, removed := applyUninstall(current)
	if removed == 0 {
		fmt.Fprintln(stdout, "No heimdall hooks found, nothing to do.")
		return 0
	}

	newBytes, err := marshalSettings(updated)
	if err != nil {
		fmt.Fprintf(stderr, "uninstall-hooks: marshal failed: %v\n", err)
		return 1
	}

	if flags.dryRun {
		fmt.Fprintf(stdout, "scope=%s\nsettings=%s\n\n", scope, settingsPath)
		writeDiff(stdout, originalBytes, newBytes)
		fmt.Fprintf(stdout, "\nWould remove %d heimdall hook entries.\n", removed)
		return 0
	}

	backup, err := writeBackup(settingsPath, originalBytes)
	if err != nil {
		fmt.Fprintf(stderr, "uninstall-hooks: backup failed: %v\n", err)
		return 1
	}
	if backup != "" {
		fmt.Fprintf(stdout, "Backup: %s\n", backup)
	}
	if err := atomicWrite(settingsPath, newBytes); err != nil {
		fmt.Fprintf(stderr, "uninstall-hooks: write failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Removed %d heimdall hook entries (scope=%s, file=%s).\n", removed, scope, settingsPath)
	return 0
}

// ----- flag parsing -----

func parseInstallFlags(args []string) (installFlags, error) {
	f := installFlags{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--dry-run":
			f.dryRun = true
		case a == "--merge":
			f.merge = true
		case a == "--force":
			f.force = true
		case a == "--scope":
			if i+1 >= len(args) {
				return f, errors.New("--scope requires a value")
			}
			i++
			f.scope = args[i]
		case strings.HasPrefix(a, "--scope="):
			f.scope = strings.TrimPrefix(a, "--scope=")
		case a == "--only":
			if i+1 >= len(args) {
				return f, errors.New("--only requires a value")
			}
			i++
			f.only = splitCSV(args[i])
			f.hasOnly = true
		case strings.HasPrefix(a, "--only="):
			f.only = splitCSV(strings.TrimPrefix(a, "--only="))
			f.hasOnly = true
		case a == "--no-prewarm":
			f.noPrewarm = true
		case a == "--no-skills-import":
			f.noSkillsImport = true
		case a == "-h" || a == "--help":
			return f, errors.New("usage: install-hooks [--scope=user|project] [--dry-run] [--merge] [--force] [--only=<events>] [--no-prewarm] [--no-skills-import]")
		default:
			return f, fmt.Errorf("unknown flag %q", a)
		}
	}
	if f.scope != "" && f.scope != "user" && f.scope != "project" {
		return f, fmt.Errorf("invalid --scope %q (want user|project)", f.scope)
	}
	if f.merge && f.force {
		return f, errors.New("--merge and --force are mutually exclusive")
	}
	return f, nil
}

func parseUninstallFlags(args []string) (uninstallFlags, error) {
	f := uninstallFlags{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--dry-run":
			f.dryRun = true
		case a == "--scope":
			if i+1 >= len(args) {
				return f, errors.New("--scope requires a value")
			}
			i++
			f.scope = args[i]
		case strings.HasPrefix(a, "--scope="):
			f.scope = strings.TrimPrefix(a, "--scope=")
		case a == "-h" || a == "--help":
			return f, errors.New("usage: uninstall-hooks [--scope=user|project] [--dry-run]")
		default:
			return f, fmt.Errorf("unknown flag %q", a)
		}
	}
	if f.scope != "" && f.scope != "user" && f.scope != "project" {
		return f, fmt.Errorf("invalid --scope %q (want user|project)", f.scope)
	}
	return f, nil
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// pickHooks filters phase1aHooks by --only event names.
func pickHooks(all []phase1aHook, only []string) []phase1aHook {
	if len(only) == 0 {
		return append([]phase1aHook(nil), all...)
	}
	set := make(map[string]bool, len(only))
	for _, name := range only {
		set[name] = true
	}
	out := make([]phase1aHook, 0, len(only))
	for _, h := range all {
		if set[h.event] {
			out = append(out, h)
		}
	}
	return out
}

// ----- scope resolution -----

// resolveScope picks the settings.json path for the given scope. Auto-detect
// rule: if CWD (or any ancestor) contains `.heimdall_db/`, treat it as a
// project scope and write to `<project>/.claude/settings.json`. Otherwise
// fall back to `$HOME/.claude/settings.json` (user scope).
//
// Test env hook: the `HEIMDALL_TEST_HOME` env var (if set) overrides
// `$HOME` for scope resolution; this lets unit tests hermetically control
// the user-scope path without touching the real `~/.claude`.
func resolveScope(explicit string, env map[string]string) (scope string, path string, err error) {
	chosen := explicit
	if chosen == "" {
		if root := findProjectRoot(env); root != "" {
			chosen = "project"
			return chosen, filepath.Join(root, ".claude", "settings.json"), nil
		}
		chosen = "user"
	}
	switch chosen {
	case "project":
		root := findProjectRoot(env)
		if root == "" {
			return "", "", errors.New("--scope=project requested but no .heimdall_db/ found in CWD or ancestors")
		}
		return "project", filepath.Join(root, ".claude", "settings.json"), nil
	case "user":
		home := env["HEIMDALL_TEST_HOME"]
		if home == "" {
			home = env["HOME"]
		}
		if home == "" {
			var herr error
			home, herr = os.UserHomeDir()
			if herr != nil || home == "" {
				return "", "", errors.New("could not resolve $HOME for user scope")
			}
		}
		return "user", filepath.Join(home, ".claude", "settings.json"), nil
	}
	return "", "", fmt.Errorf("invalid scope %q", chosen)
}

// findProjectRoot walks upward from the effective CWD looking for a
// `.heimdall_db/` directory. `HEIMDALL_TEST_CWD` in the env map overrides the
// real CWD so that tests can pin a fixture path.
func findProjectRoot(env map[string]string) string {
	cwd := env["HEIMDALL_TEST_CWD"]
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if cwd == "" {
		return ""
	}
	dir := cwd
	for i := 0; i < 32; i++ { // bounded — never walk forever
		if _, err := os.Stat(filepath.Join(dir, ".heimdall_db")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}

// ----- settings.json I/O -----

// readSettings loads and parses the settings file. On missing file it returns
// an empty map and a nil byte slice (the caller treats that as "new file").
// On parse error it returns a wrapped error — install must refuse rather than
// clobber unparseable JSON.
func readSettings(path string) (map[string]any, []byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{}, nil, nil
		}
		return nil, nil, fmt.Errorf("reading %s: %w", path, err)
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return map[string]any{}, raw, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, raw, fmt.Errorf("%s is not valid JSON — fix or delete and retry: %w", path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, raw, nil
}

// marshalSettings serializes a settings map with stable, indented output.
// Top-level key ordering is alphabetic (OQ-4 accepts this as a tradeoff for
// stdlib simplicity).
func marshalSettings(m map[string]any) ([]byte, error) {
	// encoding/json already sorts map keys alphabetically, so MarshalIndent
	// gives deterministic output out of the box.
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	b = append(b, '\n')
	return b, nil
}

// atomicWriteJSON marshals settings to indented JSON and atomically writes.
func atomicWriteJSON(path string, settings map[string]any) error {
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return atomicWrite(path, data)
}

// atomicWrite writes data to a temp file in the same directory then renames
// over the target. Parent dirs are created with 0o755.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".heimdall-settings-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Best-effort cleanup on any error after this point.
	defer func() {
		if _, statErr := os.Stat(tmpName); statErr == nil {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return nil
}

// writeBackup copies the original file bytes to a sibling
// `settings.json.heimdall-backup-<UTC>` path and returns the backup path.
// If originalBytes is nil (no prior file), returns ("", nil) with no error.
func writeBackup(path string, originalBytes []byte) (string, error) {
	if originalBytes == nil {
		return "", nil
	}
	ts := time.Now().UTC().Format("2006-01-02T15-04-05Z")
	backup := path + ".heimdall-backup-" + ts
	if err := os.WriteFile(backup, originalBytes, 0o600); err != nil {
		return "", err
	}
	return backup, nil
}

// writeDiff renders a simple unified-ish diff between old and new bytes onto
// w. This is not a full `diff -u` implementation — it walks the line lists
// and prints `- ` / `+ ` prefixes for lines that differ, with a context
// marker between hunks. Sufficient for --dry-run visibility.
func writeDiff(w io.Writer, oldBytes, newBytes []byte) {
	oldLines := splitLines(string(oldBytes))
	newLines := splitLines(string(newBytes))
	i, j := 0, 0
	for i < len(oldLines) || j < len(newLines) {
		switch {
		case i >= len(oldLines):
			fmt.Fprintf(w, "+ %s\n", newLines[j])
			j++
		case j >= len(newLines):
			fmt.Fprintf(w, "- %s\n", oldLines[i])
			i++
		case oldLines[i] == newLines[j]:
			fmt.Fprintf(w, "  %s\n", oldLines[i])
			i++
			j++
		default:
			fmt.Fprintf(w, "- %s\n", oldLines[i])
			fmt.Fprintf(w, "+ %s\n", newLines[j])
			i++
			j++
		}
	}
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	out := strings.Split(s, "\n")
	// Drop trailing empty line from trailing newline, which is noise in diffs.
	if len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

// ----- install core -----

// installReport records what applyInstall did so the top-level handler can
// render a human summary afterwards.
type installReport struct {
	noop     bool
	upgraded bool
	replaced []string // event:matcher strings that were replaced under --force
}

// applyInstall mutates a parsed settings map in place (copying as needed),
// ensuring the desired heimdall hooks are present and non-heimdall hooks are
// respected. Returns the updated map plus a report.
func applyInstall(settings map[string]any, desired []phase1aHook, flags installFlags) (map[string]any, installReport, error) {
	report := installReport{}
	out := cloneSettings(settings)

	hooksAny, ok := out["hooks"]
	if !ok || hooksAny == nil {
		hooksAny = map[string]any{}
	}
	hooksMap, ok := hooksAny.(map[string]any)
	if !ok {
		return nil, report, fmt.Errorf(`"hooks" must be an object`)
	}

	// Group desired hooks by event so we can append into the right bucket.
	byEvent := map[string][]phase1aHook{}
	for _, h := range desired {
		byEvent[h.event] = append(byEvent[h.event], h)
	}

	sameVersionCount := 0
	totalDesired := len(desired)

	eventKeys := make([]string, 0, len(byEvent))
	for k := range byEvent {
		eventKeys = append(eventKeys, k)
	}
	sort.Strings(eventKeys)

	for _, event := range eventKeys {
		hooks := byEvent[event]
		existingAny, has := hooksMap[event]
		var existingList []any
		if has && existingAny != nil {
			list, ok := existingAny.([]any)
			if !ok {
				return nil, report, fmt.Errorf(`"hooks.%s" must be an array`, event)
			}
			existingList = list
		}

		for _, h := range hooks {
			newEntry := buildHookEntry(h)

			// Classify existing entries at this event:
			//   matchIdx: index of an existing heimdall entry with the same
			//             matcher (if any) — triggers version compare.
			//   conflictIdx: indexes of non-heimdall entries with the same
			//             matcher (ignored if matcher differs).
			matchIdx := -1
			var conflictIdx []int
			for idx, raw := range existingList {
				obj, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				if !sameMatcher(obj, h.matcher) {
					continue
				}
				if isHeimdallEntry(obj) {
					matchIdx = idx
				} else {
					conflictIdx = append(conflictIdx, idx)
				}
			}

			switch {
			case matchIdx >= 0:
				// Existing heimdall entry. Compare version; upgrade if needed.
				oldObj := existingList[matchIdx].(map[string]any)
				if entryVersion(oldObj) == heimdallHookVersion && commandMatches(oldObj, h.command) {
					sameVersionCount++
					continue
				}
				existingList[matchIdx] = newEntry
				report.upgraded = true
			case len(conflictIdx) > 0:
				// Non-heimdall entry on the same event+matcher. Refuse unless
				// --merge (append alongside) or --force (replace).
				if flags.force {
					// Replace the first conflict with our entry, drop the rest.
					// Iterate in reverse so we can remove safely.
					sort.Sort(sort.Reverse(sort.IntSlice(conflictIdx)))
					for n, idx := range conflictIdx {
						if n == len(conflictIdx)-1 {
							existingList[idx] = newEntry
						} else {
							existingList = append(existingList[:idx], existingList[idx+1:]...)
						}
						report.replaced = append(report.replaced, fmt.Sprintf("%s:%s", event, h.matcher))
					}
				} else if flags.merge {
					existingList = append(existingList, newEntry)
				} else {
					return nil, report, fmt.Errorf(
						"conflict on event %q (matcher=%q): existing hook from unknown source. Re-run with --merge to install alongside, or --force to replace",
						event, h.matcher,
					)
				}
			default:
				// No conflict at all — append cleanly.
				existingList = append(existingList, newEntry)
			}
		}

		hooksMap[event] = existingList
	}

	if sameVersionCount == totalDesired && !report.upgraded && len(report.replaced) == 0 {
		report.noop = true
	}

	out["hooks"] = hooksMap
	return out, report, nil
}

// buildHookEntry renders a phase1aHook into the nested object shape Claude
// Code's settings.json uses.
func buildHookEntry(h phase1aHook) map[string]any {
	meta := map[string]any{
		"installed_at":     time.Now().UTC().Format(time.RFC3339),
		"heimdall_version": heimdallBinaryVersion,
	}
	inner := map[string]any{
		"type":    "command",
		"command": h.command,
	}
	// Claude Code reads `hooks[N].timeout` (seconds) as a per-command cap and
	// feeds it through getSessionEndHookTimeoutMs → AbortSignal.timeout(…).
	// Omit when zero so unrelated events stay bit-identical to the old shape.
	if h.timeoutSeconds > 0 {
		inner["timeout"] = h.timeoutSeconds
	}
	entry := map[string]any{
		"source":     "heimdall",
		"version":    heimdallHookVersion,
		"x-heimdall": meta,
		"hooks":      []any{inner},
	}
	if h.matcher != "" {
		entry["matcher"] = h.matcher
	}
	return entry
}

// isHeimdallEntry returns true if the given hook entry was written by us
// (either marker pattern per OQ-1).
func isHeimdallEntry(obj map[string]any) bool {
	if src, _ := obj["source"].(string); src == "heimdall" {
		return true
	}
	// Check any hooks[].command for --source=heimdall.
	raw, ok := obj["hooks"].([]any)
	if !ok {
		return false
	}
	for _, h := range raw {
		hmap, ok := h.(map[string]any)
		if !ok {
			continue
		}
		cmd, _ := hmap["command"].(string)
		if strings.Contains(cmd, "--source=heimdall") {
			return true
		}
	}
	return false
}

// entryMatchesCurrentVersion returns true if the entry's
// `x-heimdall.heimdall_version` equals the running binary's
// heimdallBinaryVersion. An entry missing the metadata bag is treated as stale
// — installs predating the bag need a refresh to gain timeout / matcher
// fields added in later waves.
func entryMatchesCurrentVersion(obj map[string]any) bool {
	meta, ok := obj["x-heimdall"].(map[string]any)
	if !ok {
		return false
	}
	v, _ := meta["heimdall_version"].(string)
	return v == heimdallBinaryVersion
}

// sameMatcher compares an existing entry's "matcher" field to the desired
// matcher string. An empty desired matcher matches an absent or empty field.
func sameMatcher(obj map[string]any, desired string) bool {
	got, _ := obj["matcher"].(string)
	return got == desired
}

// entryVersion reads the integer "version" field off an entry, defaulting to
// 0 if missing or of the wrong type.
func entryVersion(obj map[string]any) int {
	switch v := obj["version"].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

// commandMatches returns true if any hooks[].command on the entry equals the
// desired command string. Used to detect command-string drift even when the
// integer version has not been bumped.
func commandMatches(obj map[string]any, desired string) bool {
	raw, ok := obj["hooks"].([]any)
	if !ok {
		return false
	}
	for _, h := range raw {
		hmap, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if cmd, _ := hmap["command"].(string); cmd == desired {
			return true
		}
	}
	return false
}

// cloneSettings performs a deep-enough copy of a parsed settings map so that
// applyInstall can mutate without aliasing the caller's input.
func cloneSettings(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = cloneValue(v)
	}
	return out
}

func cloneValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return cloneSettings(x)
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = cloneValue(item)
		}
		return out
	default:
		return v
	}
}

// ----- uninstall core -----

// applyUninstall returns the settings map with every heimdall hook entry
// removed, plus a count of entries that were removed.
func applyUninstall(settings map[string]any) (map[string]any, int) {
	out := cloneSettings(settings)
	hooksAny, ok := out["hooks"]
	if !ok || hooksAny == nil {
		return out, 0
	}
	hooksMap, ok := hooksAny.(map[string]any)
	if !ok {
		return out, 0
	}

	removed := 0
	// Iterate over a snapshot of keys so we can delete during traversal.
	events := make([]string, 0, len(hooksMap))
	for k := range hooksMap {
		events = append(events, k)
	}
	for _, event := range events {
		list, ok := hooksMap[event].([]any)
		if !ok {
			continue
		}
		kept := make([]any, 0, len(list))
		for _, raw := range list {
			obj, ok := raw.(map[string]any)
			if !ok {
				kept = append(kept, raw)
				continue
			}
			if isHeimdallEntry(obj) {
				removed++
				continue
			}
			kept = append(kept, raw)
		}
		if len(kept) == 0 {
			delete(hooksMap, event)
		} else {
			hooksMap[event] = kept
		}
	}
	if len(hooksMap) == 0 {
		delete(out, "hooks")
	} else {
		out["hooks"] = hooksMap
	}
	return out, removed
}

// hooksDetected checks whether at least one heimdall hook entry exists in a
// Claude Code settings.json. It looks at both user-scope (~/.claude/settings.json)
// and project-scope (<project>/.claude/settings.json) paths. Returns true if
// any heimdall entry is found in either scope.
//
// The env map follows the same conventions as resolveScope (HEIMDALL_TEST_HOME,
// HEIMDALL_TEST_CWD, HOME) so callers from tests can pin paths.
func hooksDetected(env map[string]string) bool {
	// Check both user and project scopes. Either having heimdall hooks counts.
	for _, scope := range []string{"user", "project"} {
		_, path, err := resolveScope(scope, env)
		if err != nil {
			continue
		}
		settings, _, err := readSettings(path)
		if err != nil {
			continue
		}
		hooksAny, ok := settings["hooks"]
		if !ok || hooksAny == nil {
			continue
		}
		hooksMap, ok := hooksAny.(map[string]any)
		if !ok {
			continue
		}
		for _, eventAny := range hooksMap {
			list, ok := eventAny.([]any)
			if !ok {
				continue
			}
			for _, raw := range list {
				obj, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				if isHeimdallEntry(obj) {
					return true
				}
			}
		}
	}
	return false
}

// autoUpgradeHooks silently adds missing heimdall hook entries to an existing
// install. Called on SessionStart so users never need to manually reinstall
// after a binary upgrade that adds new hook events. Respects OQ-2: only
// upgrades if hooks were already installed — never auto-installs from scratch.
// Best-effort: all errors are logged and swallowed (OQ-5).
func autoUpgradeHooks(env map[string]string) {
	for _, scope := range []string{"project", "user"} {
		_, path, err := resolveScope(scope, env)
		if err != nil {
			continue
		}
		settings, _, err := readSettings(path)
		if err != nil {
			continue
		}
		hooksAny, ok := settings["hooks"]
		if !ok || hooksAny == nil {
			continue
		}
		hooksMap, ok := hooksAny.(map[string]any)
		if !ok {
			continue
		}

		// Check: does this scope have at least one heimdall entry?
		hasHeimdall := false
		for _, eventAny := range hooksMap {
			list, ok := eventAny.([]any)
			if !ok {
				continue
			}
			for _, raw := range list {
				obj, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				if isHeimdallEntry(obj) {
					hasHeimdall = true
					break
				}
			}
			if hasHeimdall {
				break
			}
		}
		if !hasHeimdall {
			continue
		}

		// Find which hooks from the template are missing, and which existing
		// heimdall entries are stale (heimdall_version != current).
		var missing []phase1aHook
		var refreshed []string // "event:matcher" tokens replaced in place
		for _, h := range phase1aHooks {
			list, _ := hooksMap[h.event].([]any)
			found := false
			for i, raw := range list {
				obj, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				if !isHeimdallEntry(obj) || !sameMatcher(obj, h.matcher) {
					continue
				}
				found = true
				if !entryMatchesCurrentVersion(obj) {
					list[i] = buildHookEntry(h)
					refreshed = append(refreshed, fmt.Sprintf("%s:%s", h.event, h.matcher))
				}
				break
			}
			if !found {
				missing = append(missing, h)
			} else {
				// Write the (possibly refreshed) list back so the slice
				// assignment above is picked up.
				hooksMap[h.event] = list
			}
		}

		if len(missing) == 0 && len(refreshed) == 0 {
			continue
		}

		// Add the missing entries.
		for _, h := range missing {
			entry := buildHookEntry(h)
			list, _ := hooksMap[h.event].([]any)
			list = append(list, entry)
			hooksMap[h.event] = list
		}
		settings["hooks"] = hooksMap

		// Atomic write with backup (same pattern as install-hooks).
		if err := atomicWriteJSON(path, settings); err != nil {
			heimdall.LogHookEvent("WARN", "auto-upgrade", map[string]any{
				"err":   "write_failed",
				"scope": scope,
			})
			continue
		}

		names := make([]string, len(missing))
		for i, h := range missing {
			names[i] = h.event
		}
		heimdall.LogHookEvent("INFO", "auto-upgrade", map[string]any{
			"msg":       "hooks_upgraded",
			"scope":     scope,
			"added":     names,
			"refreshed": refreshed,
			"total":     len(phase1aHooks),
			"path":      path,
		})

		// Prewarm note: install-hooks fires a 5s EmbedForHook to remove the
		// first-turn cold start (see docs/plans/hooks/07 caveat). Auto-upgrade
		// runs inside HookSessionStart, which already issues an EmbedForHook
		// against the same model for its recall step a few milliseconds later
		// (see hook.go HookSessionStart → newHookEmbedder.Embed). That call
		// is what actually warms Ollama; adding a second EmbedForHook here
		// would just double the latency on the already-tight 2s SessionStart
		// budget for zero benefit. So the auto-upgrade path intentionally
		// does NOT call doPrewarm — the next recall does the warm-up for us.
	}
}

// Compile-time sanity: both entry points match the HookHandler-ish shape we
// use for dispatch. We can't reuse HookHandler verbatim because these two
// also take cfg, but the internal shape is identical for test harness code.
var _ = CLIInstallHooks
var _ = CLIUninstallHooks
