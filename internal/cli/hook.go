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
	"strings"
	"time"
	"unicode/utf8"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// DispatchHook routes `heimdall-mcp hook <event>` subcommands. This is the
// retrieval-hook dispatcher (singular `hook`), distinct from the admin
// `hooks` dispatcher (plural) for tail/cache-clear/cache-stats.
//
// Retrieval-hook handlers follow the OQ-5 rule: return 0 regardless of
// internal errors. Never block Claude Code. Errors are logged via
// LogHookEvent; nothing is ever written to stderr on the retrieval path.
//
// DispatchHook itself may still return a non-zero code on developer CLI
// usage errors (empty args, unknown subcommand) because those paths are
// hit from a shell prompt, not from Claude Code — Claude Code always
// passes a known subcommand. The retrieval-path OQ-5 rule is enforced
// inside each case handler (HookSessionStart, HookPostEdit, …), not at
// the dispatcher boundary.
func DispatchHook(cfg config.Config, stdin io.Reader, stdout, stderr io.Writer, env map[string]string, args []string) int {
	if len(args) == 0 {
		// Empty args: a misconfigured install could bake a truncated command
		// into settings.json. Log for developers, print a short usage line to
		// stderr for interactive shells, but still return 0 so Claude Code
		// never sees a non-zero exit from a retrieval hook. (OQ-5.)
		heimdall.LogHookEvent("WARN", "hook", map[string]any{"err": "no_subcommand"})
		fmt.Fprintln(stderr, "Usage: heimdall-mcp hook <session-start|post-edit|post-edit-actor|user-prompt|stop> [flags]")
		return 0
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "session-start":
		return HookSessionStart(cfg, stdin, stdout, stderr, env, rest, HookSessionStartDeps{})
	case "post-edit":
		return HookPostEdit(cfg, stdin, stdout, stderr, env, rest)
	case "post-edit-actor":
		return HookPostEditActor(cfg, stdin, stdout, stderr, env, rest)
	case "user-prompt":
		return HookUserPrompt(cfg, stdin, stdout, stderr, env, rest, HookUserPromptDeps{})
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, "heimdall-mcp hook — Claude Code retrieval hook entry points")
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "  hook session-start [--budget-ms N] [--project <path>]")
		fmt.Fprintln(stdout, "  hook post-edit [--project <path>]")
		fmt.Fprintln(stdout, "  hook user-prompt [--budget-ms N] [--project <path>]")
		return 0
	default:
		// Unknown subcommand: same reasoning as empty-args. Log, short stderr
		// hint for developers, exit 0 so Claude Code continues normally.
		heimdall.LogHookEvent("WARN", "hook", map[string]any{"err": "unknown_subcommand", "sub": sub})
		fmt.Fprintf(stderr, "Unknown hook subcommand: %s\n", sub)
		return 0
	}
}

// HookSessionStartDeps abstracts external resources so tests can inject
// fakes. A zero-value struct uses the real implementations.
type HookSessionStartDeps struct {
	// NewOllamaClient builds an Ollama client for the given endpoint.
	NewOllamaClient func(endpoint string) *heimdall.OllamaClient
	// OpenMemoryStore opens the memory store. Nil → real one at
	// config.ResolveMemoryDBPath().
	OpenMemoryStore func() (*heimdall.MemoryStore, error)
	// NewHookEmbedder wraps an Ollama client into an Embedder that uses
	// EmbedForHook (keep_alive). Nil → real one.
	NewHookEmbedder func(client *heimdall.OllamaClient, model string) heimdall.Embedder
	// Suppress returns true if the caller should emit a Tier B note for
	// (project, failureCode) in the given window. Nil → real suppression
	// store at XDG_STATE_HOME/heimdall/suppress.db.
	Suppress func(project, failureCode string, window time.Duration) bool
	// Now returns the current time. Nil → time.Now.
	Now func() time.Time
}

// sessionStartBudgetDefault is the wall-clock deadline for the whole retrieval
// (plan §3.1: 2 s p95, 3 s hard internal). --budget-ms overrides.
const sessionStartBudgetDefault = 2000

// sessionStartSuppressionWindow is the 5-min window for Tier B note emission
// per (project, failure_code) key.
const sessionStartSuppressionWindow = 5 * time.Minute

// sessionStartMaxRunes caps the emitted markdown at roughly 1500 tokens. We
// use 6000 runes as a proxy (≈4 runes/token on prose).
const sessionStartMaxRunes = 6000

// HookSessionStart is the Claude Code `SessionStart` hook entry point.
//
// Stdin: Claude Code's SessionStart event JSON (unknown fields ignored,
// only `cwd` is consumed). Empty stdin is tolerated — falls back to the
// process CWD.
//
// Stdout: a single `## Heimdall context` markdown block injected into
// Claude's initial context. Empty stdout is valid and means "no context to
// contribute" (degraded / disabled / suppressed).
//
// Stderr: always empty on the retrieval path. Errors go through
// LogHookEvent.
//
// Exit code: always 0 per OQ-5. Never blocks Claude Code.
func HookSessionStart(cfg config.Config, stdin io.Reader, stdout, stderr io.Writer, env map[string]string, args []string, deps HookSessionStartDeps) int {
	_ = stderr // never written to; kept for uniform §5.4 signature

	// --- flag parsing -------------------------------------------------------

	fs := flag.NewFlagSet("hook session-start", flag.ContinueOnError)
	// Silence flag errors; they still abort the parse, which we handle by
	// exiting 0 (no context) rather than failing the hook.
	fs.SetOutput(io.Discard)

	var (
		sourceFlag  string
		versionFlag int
		budgetMS    int
		projectFlag string
	)
	// --source and --version are OQ-1 fallback markers: accepted and ignored.
	// Their sole purpose is so install-hooks can bake them into settings.json
	// and marker detection works even if Claude Code strips unknown JSON keys.
	fs.StringVar(&sourceFlag, "source", "", "install marker fallback (ignored)")
	fs.IntVar(&versionFlag, "version", 0, "install marker fallback (ignored)")
	fs.IntVar(&budgetMS, "budget-ms", sessionStartBudgetDefault, "hard wall-clock deadline in ms")
	fs.StringVar(&projectFlag, "project", "", "project root override (default: stdin cwd → os.Getwd)")

	if err := fs.Parse(args); err != nil {
		heimdall.LogHookEvent("ERROR", "session-start", map[string]any{
			"stage": "flag_parse",
			"err":   err.Error(),
		})
		return 0
	}
	_ = sourceFlag
	_ = versionFlag

	// --- resolve deps --------------------------------------------------------

	newClient := deps.NewOllamaClient
	if newClient == nil {
		newClient = heimdall.NewOllamaClient
	}
	openMemoryStore := deps.OpenMemoryStore
	if openMemoryStore == nil {
		openMemoryStore = func() (*heimdall.MemoryStore, error) {
			return heimdall.OpenMemoryStore(config.ResolveMemoryDBPath())
		}
	}
	newHookEmbedder := deps.NewHookEmbedder
	if newHookEmbedder == nil {
		newHookEmbedder = func(client *heimdall.OllamaClient, model string) heimdall.Embedder {
			return &hookEmbedder{client: client, model: model}
		}
	}
	suppress := deps.Suppress
	if suppress == nil {
		suppress = heimdall.ShouldEmitTierB
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}

	// --- resolve project root -----------------------------------------------

	projectRoot := resolveProjectRoot(stdin, projectFlag)
	if projectRoot == "" {
		heimdall.LogHookEvent("INFO", "session-start", map[string]any{
			"stage":  "resolve_root",
			"reason": "no cwd resolvable",
		})
		return 0
	}

	// --- disable gate (§5.7) -------------------------------------------------

	if heimdall.HooksDisabled(projectRoot, env) {
		heimdall.LogHookEvent("INFO", "session-start", map[string]any{
			"stage": "disabled",
		})
		return 0
	}

	// --- budget / deadline ---------------------------------------------------

	if budgetMS <= 0 {
		budgetMS = sessionStartBudgetDefault
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(budgetMS)*time.Millisecond)
	defer cancel()

	baseDir := filepath.Join(projectRoot, ".heimdall_db")

	// --- Ollama ping ---------------------------------------------------------

	client := newClient(cfg.OllamaEndpoint)
	pingCtx, pingCancel := context.WithTimeout(ctx, 1*time.Second)
	pingErr := client.Ping(pingCtx)
	pingCancel()
	if pingErr != nil {
		heimdall.LogHookEvent("INFO", "session-start", map[string]any{
			"stage": "ollama_ping",
			"err":   pingErr.Error(),
		})
		emitTierBNote(stdout, suppress, projectRoot, "ollama_down",
			"ollama unreachable")
		return 0
	}

	// --- model resolution ----------------------------------------------------

	// ResolveUsableModelDB refuses to fuzzy-match for the hook path? No — it
	// is allowed to pick a different indexed model than cfg.Model. But plan
	// §5.1 is explicit: on the hook path, after we pick a model we must run
	// VerifyHookIndex against that same model. If cfg.Model has no index,
	// ResolveUsableModelDB falls back to the first available — that is the
	// correct behavior (it's still an index we built ourselves, just under
	// a different model). The forbidden path is "ask Ollama to embed with
	// model X against an index built with model Y", and VerifyHookIndex
	// blocks exactly that.
	dbDir, resolvedModel := heimdall.ResolveUsableModelDB(ctx, client, baseDir, cfg.Model)
	if dbDir == "" || resolvedModel == "" {
		// No index yet, or the model isn't pulled in Ollama.
		heimdall.LogHookEvent("INFO", "session-start", map[string]any{
			"stage":  "resolve_model",
			"reason": "no usable index",
		})
		emitEmpty(stdout)
		return 0
	}

	store, err := heimdall.OpenStore(dbDir)
	if err != nil {
		heimdall.LogHookEvent("ERROR", "session-start", map[string]any{
			"stage": "open_store",
			"err":   err.Error(),
		})
		return 0
	}
	defer store.Close()

	// --- VerifyHookIndex gate ------------------------------------------------

	if verr := heimdall.VerifyHookIndex(store, resolvedModel); verr != nil {
		code := classifyVerifyErr(verr)
		heimdall.LogHookEvent("WARN", "session-start", map[string]any{
			"stage": "verify_hook_index",
			"code":  code,
			"err":   verr.Error(),
		})
		emitTierBNote(stdout, suppress, projectRoot, code,
			"index model mismatch")
		return 0
	}

	// --- gather status + recall ---------------------------------------------

	status := heimdall.GatherStatus(ctx, cfg.OllamaEndpoint, resolvedModel, baseDir, cfg.ExcludePatterns, client)

	var bullets []string
	if memStore, err := openMemoryStore(); err == nil && memStore != nil {
		// Defensive: caller might reuse the store across calls (tests do),
		// so we only Close if we opened it ourselves. The simplest heuristic
		// is "always let the caller manage its own store in tests" — which
		// the test deps enforce by returning a store whose Close is a no-op
		// via t.Cleanup. In production the real factory opens a fresh one
		// and we'd want to close it; but the MemoryStore Close is idempotent
		// and hookpath cold-start cost of leaving it to GC is negligible.
		// For determinism, we explicitly Close() here and rely on the test
		// fixture to tolerate double-close (OpenMemoryStore's sql.DB does).
		defer memStore.Close()

		embedder := newHookEmbedder(client, resolvedModel)
		query := fmt.Sprintf("%s project context", filepath.Base(projectRoot))
		hits, rerr := heimdall.RunRecall(ctx, heimdall.RecallParams{
			Query: query,
			Limit: 5,
		}, embedder, memStore)
		if rerr != nil {
			heimdall.LogHookEvent("WARN", "session-start", map[string]any{
				"stage": "recall",
				"err":   rerr.Error(),
			})
		} else {
			for _, h := range hits {
				bullets = append(bullets, singleLine(h.Content))
			}
		}
	} else if err != nil {
		heimdall.LogHookEvent("INFO", "session-start", map[string]any{
			"stage": "open_memory",
			"err":   err.Error(),
		})
	}

	// --- deadline check before emission --------------------------------------

	if ctxErr := ctx.Err(); ctxErr != nil {
		// Budget fired. Emit empty stdout — Claude gets no context this
		// turn, which is strictly better than a half-rendered block.
		heimdall.LogHookEvent("WARN", "session-start", map[string]any{
			"stage": "budget",
			"err":   ctxErr.Error(),
		})
		return 0
	}

	// --- format + write ------------------------------------------------------

	body := formatSessionStartBlock(status, resolvedModel, projectRoot, bullets, now())
	body = capRunes(body, sessionStartMaxRunes)
	_, _ = io.WriteString(stdout, body)

	heimdall.LogHookEvent("INFO", "session-start", map[string]any{
		"stage":      "ok",
		"bullets":    len(bullets),
		"chunks":     status.TotalChunks,
		"model":      resolvedModel,
	})
	return 0
}

// classifyVerifyErr maps a VerifyHookIndex sentinel to the short failure code
// used as the suppression key.
func classifyVerifyErr(err error) string {
	switch {
	case errors.Is(err, heimdall.ErrIndexModelMissing):
		return "index_model_missing"
	case errors.Is(err, heimdall.ErrIndexModelMismatch):
		return "index_model_mismatch"
	case errors.Is(err, heimdall.ErrIndexDimMismatch):
		return "index_dim_mismatch"
	default:
		return "index_verify_unknown"
	}
}

// emitTierBNote writes a one-line degraded-state note to stdout if and only
// if the suppression store says this failure code is out of its cooldown
// window. Otherwise writes nothing.
func emitTierBNote(w io.Writer, suppress func(string, string, time.Duration) bool, project, failureCode, humanReason string) {
	if !suppress(project, failureCode, sessionStartSuppressionWindow) {
		return
	}
	// Single-line degraded banner — matches plan §3.1 wording.
	fmt.Fprintf(w, "## Heimdall context\n\n> heimdall: unavailable (%s)\n", humanReason)
}

// emitEmpty writes the "no index yet" one-liner.
func emitEmpty(w io.Writer) {
	fmt.Fprint(w, "## Heimdall context\n\n> heimdall: no index yet. Run `heimdall-mcp index .` to create one.\n")
}

// formatSessionStartBlock builds the happy-path markdown block. An empty
// bullets slice still renders the header (with no "Recent memories"
// subsection) so the user sees the project is indexed.
func formatSessionStartBlock(status heimdall.StatusInfo, model, projectRoot string, bullets []string, now time.Time) string {
	var b strings.Builder
	b.Grow(256 + 80*len(bullets))

	projectName := filepath.Base(projectRoot)
	lastIndexed := status.LastIndexed
	if lastIndexed == "" {
		lastIndexed = "unknown"
	}

	b.WriteString("## Heimdall context\n\n")
	fmt.Fprintf(&b, "**Project:** %s  •  **Model:** %s  •  **Chunks:** %d  •  **Last indexed:** %s\n",
		projectName, model, status.TotalChunks, lastIndexed)

	if len(bullets) > 0 {
		b.WriteString("\n### Recent memories\n")
		for _, m := range bullets {
			if m == "" {
				continue
			}
			fmt.Fprintf(&b, "- %s\n", m)
		}
	}

	b.WriteString("\n_retrieved via heimdall-mcp_\n")
	return b.String()
}

// capRunes truncates s to at most n runes, appending an ellipsis if it had to
// cut. Used as a crude token ceiling — plan §3.1 says ≤1500 tokens; we use
// 6000 runes as a safe proxy on prose.
func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	if n > 3 {
		return string(runes[:n-3]) + "..."
	}
	return string(runes[:n])
}

// resolveProjectRoot picks the project root from, in order:
//  1. --project flag (absolutized)
//  2. stdin JSON `cwd` field (absolutized)
//  3. os.Getwd()
//
// Returns empty string on total failure. Unknown JSON fields are ignored.
// Empty stdin is tolerated (io.EOF).
func resolveProjectRoot(stdin io.Reader, projectFlag string) string {
	if projectFlag != "" {
		if abs, err := filepath.Abs(projectFlag); err == nil {
			return abs
		}
		return projectFlag
	}
	if stdin != nil {
		var evt struct {
			CWD string `json:"cwd"`
		}
		// Use a size-bounded reader so a pathological stdin can't balloon
		// memory. SessionStart event JSON is tiny — 64 KB is generous.
		limited := io.LimitReader(stdin, 64*1024)
		data, _ := io.ReadAll(limited)
		if len(bytesTrimSpace(data)) > 0 {
			// Best-effort decode; malformed JSON → fall through.
			_ = json.Unmarshal(data, &evt)
			if evt.CWD != "" {
				if abs, err := filepath.Abs(evt.CWD); err == nil {
					return abs
				}
				return evt.CWD
			}
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return cwd
}

// bytesTrimSpace is a cheap byte-level TrimSpace — avoids importing
// strings/bytes for one call site on the cold path.
func bytesTrimSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end {
		c := b[start]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			start++
			continue
		}
		break
	}
	for end > start {
		c := b[end-1]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			end--
			continue
		}
		break
	}
	return b[start:end]
}

// hookEmbedder is an Embedder that uses EmbedForHook (keep_alive: "10m"),
// per plan §5.6. Kept private to internal/cli — the retrieval path is the
// only caller.
type hookEmbedder struct {
	client *heimdall.OllamaClient
	model  string
}

func (h *hookEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	return h.client.EmbedForHook(ctx, h.model, text)
}
