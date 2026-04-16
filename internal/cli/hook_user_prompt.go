package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// userPromptBudgetDefault is the soft wall-clock target the hook aims to hit
// on the uncached path (plan §3.2: p95 ≤ 250 ms). --budget-ms overrides.
const userPromptBudgetDefault = 250

// userPromptHardTimeout is the hard internal ceiling. If a user passes
// --budget-ms higher than this we raise the hard cap to at least match —
// the budget flag is a soft aim, not a true contract.
const userPromptHardTimeout = 500

// userPromptMinPromptLen is the skip threshold (in runes). Prompts shorter
// than this bypass the whole hook — no embed, no search, no cache. Trivial
// "ok", "yes", "nvm" turns aren't worth the latency.
const userPromptMinPromptLen = 8

// userPromptCacheTTL bounds how long a cache row survives before being
// considered stale on read. The index_version fingerprint in the cache key
// already invalidates on any indexing event; this TTL is a secondary guard
// for very long-lived sessions.
const userPromptCacheTTL = 24 * time.Hour

// userPromptCacheRowCap limits the hook_cache table to ~1000 rows, matching
// the Wave 1 T10 sizing. Eviction is oldest-first inside HookCachePut.
const userPromptCacheRowCap = 1000

// userPromptSuppressionWindow is the 5-minute Tier B cooldown per
// (project, failure_code) key, same as session-start.
const userPromptSuppressionWindow = 5 * time.Minute

// userPromptMaxRunes caps rendered markdown at ~1500 tokens (6000 runes).
const userPromptMaxRunes = 6000

// userPromptCacheKeyPromptCap bounds the raw prompt bytes folded into the
// SHA-256 cache key. Long prompts are truncated before hashing so the cache
// layout is predictable regardless of user input size.
const userPromptCacheKeyPromptCap = 2048

// HookUserPromptDeps mirrors HookSessionStartDeps: nil-valued fields fall
// back to production implementations, tests inject fakes.
type HookUserPromptDeps struct {
	NewOllamaClient func(endpoint string) *heimdall.OllamaClient
	OpenStore       func(dbDir string) (*heimdall.VectorStore, error)
	NewHookEmbedder func(client *heimdall.OllamaClient, model string) heimdall.Embedder
	// OpenMemoryStore opens the global memory store for skill surfacing.
	// Nil → real one at config.ResolveMemoryDBPath().
	OpenMemoryStore func() (*heimdall.MemoryStore, error)
	Suppress        func(project, failureCode string, window time.Duration) bool
	Now             func() time.Time
}

// HookUserPrompt is the Claude Code `UserPromptSubmit` hook entry point.
//
// Stdin: UserPromptSubmit event JSON; `prompt` and `cwd` fields are consumed.
// Stdout: a `## Heimdall context` markdown block (happy path), empty on skip
// or degraded path, or a one-line Tier B banner on suppressed failure.
// Stderr: never written to. Errors go through LogHookEvent.
// Exit code: always 0 per OQ-5.
func HookUserPrompt(cfg config.Config, stdin io.Reader, stdout, stderr io.Writer, env map[string]string, args []string, deps HookUserPromptDeps) int {
	_ = stderr

	fs := flag.NewFlagSet("hook user-prompt", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var (
		sourceFlag  string
		versionFlag int
		budgetMs    int
		projectFlag string
		promptFlag  string
	)
	fs.StringVar(&sourceFlag, "source", "", "install marker fallback (ignored)")
	fs.IntVar(&versionFlag, "version", 0, "install marker fallback (ignored)")
	fs.IntVar(&budgetMs, "budget-ms", userPromptBudgetDefault, "soft budget in ms")
	fs.StringVar(&projectFlag, "project", "", "project root override")
	fs.StringVar(&promptFlag, "prompt", "", "prompt override (debug; normally from stdin)")

	if err := fs.Parse(args); err != nil {
		heimdall.LogHookEvent("ERROR", "user-prompt", map[string]any{
			"stage": "flag_parse",
			"err":   err.Error(),
		})
		return 0
	}
	_ = sourceFlag
	_ = versionFlag

	newClient := deps.NewOllamaClient
	if newClient == nil {
		newClient = heimdall.NewOllamaClient
	}
	openStore := deps.OpenStore
	if openStore == nil {
		openStore = heimdall.OpenStore
	}
	newHookEmbedder := deps.NewHookEmbedder
	if newHookEmbedder == nil {
		newHookEmbedder = func(client *heimdall.OllamaClient, model string) heimdall.Embedder {
			return &hookEmbedder{client: client, model: model}
		}
	}
	openMemoryStore := deps.OpenMemoryStore
	if openMemoryStore == nil {
		openMemoryStore = func() (*heimdall.MemoryStore, error) {
			return heimdall.OpenMemoryStore(config.ResolveMemoryDBPath())
		}
	}
	suppress := deps.Suppress
	if suppress == nil {
		suppress = heimdall.ShouldEmitTierB
	}

	// --- read prompt + project root from stdin JSON -------------------------
	prompt, stdinProject := readUserPromptStdin(stdin, promptFlag)

	projectRoot := ""
	if projectFlag != "" {
		if abs, err := filepath.Abs(projectFlag); err == nil {
			projectRoot = abs
		} else {
			projectRoot = projectFlag
		}
	} else if stdinProject != "" {
		projectRoot = stdinProject
	} else if cwd, err := os.Getwd(); err == nil {
		projectRoot = cwd
	}
	if projectRoot == "" {
		heimdall.LogHookEvent("INFO", "user-prompt", map[string]any{
			"stage":  "resolve_root",
			"reason": "no cwd resolvable",
		})
		return 0
	}

	// --- skip heuristic: trivial prompt ------------------------------------
	trimmed := strings.TrimSpace(prompt)
	if len([]rune(trimmed)) < userPromptMinPromptLen {
		heimdall.LogHookEvent("INFO", "user-prompt", map[string]any{
			"stage":  "skip",
			"reason": "prompt_too_short",
		})
		return 0
	}

	// --- disable gate (§5.7) ------------------------------------------------
	if heimdall.HooksDisabled(projectRoot, env) {
		heimdall.LogHookEvent("INFO", "user-prompt", map[string]any{"stage": "disabled"})
		return 0
	}

	// --- budget -------------------------------------------------------------
	// --budget-ms is the wall-clock deadline for the whole hook, the same
	// shape session-start uses. Plan §3.2 targets p95 ≤ 250 ms on the
	// uncached path; userPromptHardTimeout documents that the plan's "hard
	// ceiling" is 500 ms but we don't enforce a separate tier — anyone who
	// wants more can raise --budget-ms explicitly.
	if budgetMs <= 0 {
		budgetMs = userPromptBudgetDefault
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(budgetMs)*time.Millisecond)
	defer cancel()

	baseDir := filepath.Join(projectRoot, ".heimdall_db")

	// --- Ollama ping --------------------------------------------------------
	client := newClient(cfg.OllamaEndpoint)
	pingCtx, pingCancel := context.WithTimeout(ctx, 300*time.Millisecond)
	pingErr := client.Ping(pingCtx)
	pingCancel()
	if pingErr != nil {
		heimdall.LogHookEvent("INFO", "user-prompt", map[string]any{
			"stage": "ollama_ping",
			"err":   pingErr.Error(),
		})
		emitTierBNote(stdout, suppress, projectRoot, "ollama_down", "ollama unreachable")
		return 0
	}

	// --- model resolution ---------------------------------------------------
	dbDir, resolvedModel := heimdall.ResolveUsableModelDB(ctx, client, baseDir, cfg.Model)
	if dbDir == "" || resolvedModel == "" {
		heimdall.LogHookEvent("INFO", "user-prompt", map[string]any{
			"stage":  "resolve_model",
			"reason": "no usable index",
		})
		return 0
	}

	store, err := openStore(dbDir)
	if err != nil {
		heimdall.LogHookEvent("ERROR", "user-prompt", map[string]any{
			"stage": "open_store",
			"err":   err.Error(),
		})
		return 0
	}
	defer store.Close()

	// --- VerifyHookIndex gate ----------------------------------------------
	if verr := heimdall.VerifyHookIndex(store, resolvedModel); verr != nil {
		code := classifyVerifyErr(verr)
		heimdall.LogHookEvent("WARN", "user-prompt", map[string]any{
			"stage": "verify_hook_index",
			"code":  code,
			"err":   verr.Error(),
		})
		emitTierBNote(stdout, suppress, projectRoot, code, "index model mismatch")
		return 0
	}

	// --- cache lookup -------------------------------------------------------
	cacheKey := userPromptCacheKey(trimmed, store.GetIndexVersion(), projectRoot)
	if cached, cerr := store.HookCacheGet(cacheKey, userPromptCacheTTL); cerr == nil {
		_, _ = stdout.Write(cached)
		heimdall.LogHookEvent("INFO", "user-prompt", map[string]any{
			"stage": "cache_hit",
			"bytes": len(cached),
			"model": resolvedModel,
		})
		return 0
	} else if !errors.Is(cerr, heimdall.ErrHookCacheMiss) {
		heimdall.LogHookEvent("WARN", "user-prompt", map[string]any{
			"stage": "cache_get",
			"err":   cerr.Error(),
		})
		// fall through to recompute
	}

	// --- embed prompt -------------------------------------------------------
	embedder := newHookEmbedder(client, resolvedModel)
	queryVec, eerr := embedder.Embed(ctx, trimmed)
	if eerr != nil {
		heimdall.LogHookEvent("WARN", "user-prompt", map[string]any{
			"stage": "embed",
			"err":   eerr.Error(),
		})
		return 0
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		heimdall.LogHookEvent("WARN", "user-prompt", map[string]any{
			"stage": "budget_after_embed",
			"err":   ctxErr.Error(),
		})
		return 0
	}

	// --- search -------------------------------------------------------------
	results := store.SearchFiltered(ctx, queryVec, 5, "", "", nil)

	if ctxErr := ctx.Err(); ctxErr != nil {
		heimdall.LogHookEvent("WARN", "user-prompt", map[string]any{
			"stage": "budget_after_search",
			"err":   ctxErr.Error(),
		})
		return 0
	}

	// --- surface relevant skills (additive) --------------------------------
	// Best-effort: memory store missing / empty / slow returns nil bullets
	// and the "### Relevant skills" section is simply omitted. Runs inside
	// the same ctx deadline as the search — if the prompt was embedded and
	// searched successfully there's usually ample slack to do a cheap
	// memory-DB scan keyed by type=skill.
	var skillBullets []string
	if memStore, merr := openMemoryStore(); merr == nil && memStore != nil {
		skillBullets = surfaceRelevantSkills(ctx, trimmed, skillsTopNDefault, embedder, memStore)
		memStore.Close()
	} else if merr != nil {
		heimdall.LogHookEvent("INFO", "user-prompt", map[string]any{
			"stage": "open_memory",
			"err":   merr.Error(),
		})
	}

	// --- render + cache store ----------------------------------------------
	body := formatSearchHookMD(trimmed, resolvedModel, filepath.Base(projectRoot), results)
	body = insertSkillsSection(body, skillBullets)
	body = capRunes(body, userPromptMaxRunes)

	if perr := store.HookCachePut(cacheKey, []byte(body), userPromptCacheTTL, userPromptCacheRowCap); perr != nil {
		heimdall.LogHookEvent("WARN", "user-prompt", map[string]any{
			"stage": "cache_put",
			"err":   perr.Error(),
		})
	}
	_, _ = io.WriteString(stdout, body)

	heimdall.LogHookEvent("INFO", "user-prompt", map[string]any{
		"stage":  "ok",
		"hits":   len(results),
		"skills": len(skillBullets),
		"bytes":  len(body),
		"model":  resolvedModel,
	})
	return 0
}

// readUserPromptStdin extracts (prompt, absProjectFromCWD) from a
// UserPromptSubmit event JSON on stdin. Empty stdin, malformed JSON, or
// missing fields all degrade gracefully — the caller decides what to do
// with empty returns. If promptOverride is non-empty it is used verbatim
// and stdin is not consulted (debug path).
func readUserPromptStdin(stdin io.Reader, promptOverride string) (prompt, projectFromCWD string) {
	if promptOverride != "" {
		return promptOverride, ""
	}
	if stdin == nil {
		return "", ""
	}
	limited := io.LimitReader(stdin, 256*1024)
	data, _ := io.ReadAll(limited)
	if len(data) == 0 {
		return "", ""
	}
	var evt struct {
		Prompt string `json:"prompt"`
		CWD    string `json:"cwd"`
	}
	if err := json.Unmarshal(data, &evt); err != nil {
		return "", ""
	}
	if evt.CWD != "" {
		if abs, err := filepath.Abs(evt.CWD); err == nil {
			projectFromCWD = abs
		} else {
			projectFromCWD = evt.CWD
		}
	}
	return evt.Prompt, projectFromCWD
}

// userPromptCacheKey builds the stable hex SHA-256 key that indexes a
// cache row. Components:
//   - normalized prompt (lowercased, whitespace collapsed, truncated)
//   - index_version — any Upsert/RemoveByFile bumps this, instantly
//     invalidating every cached row from the previous index state
//   - project scope — keeps prompts from different repos isolated even
//     if the same store file is shared (sub-project future-proofing)
//
// The normalization is deliberately aggressive so "What does X do?" and
// "what does x do?   " collide on the same cache row.
func userPromptCacheKey(prompt string, indexVersion int64, scope string) string {
	normalized := normalizePromptForCache(prompt)
	if len(normalized) > userPromptCacheKeyPromptCap {
		normalized = normalized[:userPromptCacheKeyPromptCap]
	}
	h := sha256.New()
	h.Write([]byte(normalized))
	h.Write([]byte{0})
	h.Write([]byte(scope))
	h.Write([]byte{0})
	_, _ = fmt.Fprintf(h, "v%d", indexVersion)
	return hex.EncodeToString(h.Sum(nil))
}

// normalizePromptForCache collapses whitespace to single spaces and
// lowercases. Used only for the cache key — the original prompt is still
// what gets embedded.
func normalizePromptForCache(prompt string) string {
	prompt = strings.ToLower(strings.TrimSpace(prompt))
	if prompt == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(prompt))
	prevSpace := false
	for _, r := range prompt {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	return b.String()
}
