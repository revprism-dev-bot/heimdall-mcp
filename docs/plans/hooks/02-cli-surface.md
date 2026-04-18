# 02 — CLI Surface for Claude Code Hooks

**Owner:** cli-surface
**Status:** Revised to align with `01-architecture.md` phase 1 scope.
**Scope:** design only. No code. Companion to `01-architecture.md`.

## Goal

Every hook in `01-architecture.md` is a shell script invoked by Claude Code. Those scripts need a scriptable, fast, non-interactive CLI on the `heimdall-mcp` binary. MCP (JSON-RPC over stdio) is unusable from a hook because hooks get event JSON on stdin and must emit free-form stdout. This doc specifies every new subcommand, flag, and stdin/stdout/stderr contract for phase 1.

Phase 1 scope per architect: **SessionStart, UserPromptSubmit, PostToolUse(Edit|Write), Stop.** Only these four hook commands ship; `post-commit`, `pre-destructive`, `session-end` are deferred (architect: `post-commit` is already covered by `runIndex`'s built-in git indexing + stale check; `pre-destructive` defers to phase 3 when an `agent`-type hook is available — see `08-destructive-op-primitive.md`; `SessionEnd` is less reliable than `Stop`).

The existing CLI (`index`, `search`, `status`, `projects`, `configure`/`config`, `paths`, `models`, `help`) is untouched in its current behavior. Hooks require **additive** surface: new top-level `hook` subcommand for the four hook entry points, plus new top-level `recall` / `ingest-session` subcommands (currently MCP-only), plus a new `--format=hook-md` output mode on `search` and `recall`, plus `--format=json` on `status`.

## Command inventory

| Command | Purpose | Hook event |
|---|---|---|
| `heimdall-mcp hook session-start` | Compose project status + top memories as markdown; fires `cache-warm` inline | `SessionStart` |
| `heimdall-mcp hook cache-warm` | Warm-embed Ollama model (no DB) — invoked by `session-start`, not registered as its own event | (internal) |
| `heimdall-mcp hook drain-reindex-dlq` | Drain `reindex.deadletter.jsonl` through the incremental indexer — spawned detached by `session-start` on probe=ok, not registered as its own event | (internal) |
| `heimdall-mcp hook user-prompt` | Skip-heuristic + semantic search, emit markdown | `UserPromptSubmit` |
| `heimdall-mcp hook post-edit` | Fork detached re-index of the edited file; exit 0 immediately | `PostToolUse(Edit\|Write)` |
| `heimdall-mcp hook stop` | Append turn to rolling buffer; fork ingest-session when full | `Stop` |
| `heimdall-mcp recall` | Top-level recall wrapper over `MemoryStore.SearchMemories` | used by `session-start` |
| `heimdall-mcp ingest-session` | Top-level wrapper over `heimdall.IngestSession` | forked from `hook stop` |
| `heimdall-mcp search --format=hook-md` | New output mode on existing `search` | used by `hook user-prompt` |
| `heimdall-mcp recall --format=hook-md` | New output mode on new `recall` | used by `hook session-start` |
| `heimdall-mcp status --format=json` | New output mode on existing `status` | used by `hook session-start` composer |
| `heimdall-mcp install-hooks` | Non-destructive merge into `~/.claude/settings.json` | — |
| `heimdall-mcp uninstall-hooks` | Remove heimdall-owned hook entries | — |
| `heimdall-mcp hooks doctor` | Diagnose install + dry-fire each hook (per 04 §6) | — |
| `heimdall-mcp hooks tail` | Tail the hook log with filters (per 04 §6) | — |
| `heimdall-mcp hooks cache-clear` | Drop the `hook_cache` table contents (per 03 §3) | — |
| `heimdall-mcp hooks cache-stats` | Emit cache hit/miss/size stats as JSON or human text | — |

Namespacing rationale: `hook <event>` mirrors Claude Code event names with dashes, so reading `settings.json` makes it obvious which binary runs at which event. The four hook commands plus `install-hooks` live in one cohesive namespace; `recall` / `ingest-session` are promoted to top-level because they are generally useful CLI primitives (not hook-specific) that happen to have been MCP-only until now.

---

## Command signatures

Shared conventions for all four `hook` subcommands (confirmed with failure-modes 04 §2):
- Read full event JSON from stdin. Never rely on argv for user-provided data (prompt text, file paths). Parse with `encoding/json`, unknown fields ignored.
- Always non-interactive. Architect confirmed: `cliIndex`'s `promptModelSelection` interactive path is fine because hooks never call `index`. All `hook *` subcommands must never prompt.
- **Exit code taxonomy (confirmed with failure-modes 04 §2):** `0` = proceed-with-stdout; `2` = block-with-stderr (reserved, never used by phase-1 retrieval hooks); any other non-zero = Claude Code ignores stdout and proceeds. **All four phase-1 hook commands always exit 0 to Claude Code**, even on internal error — retrieval failures degrade silently (Tier A) or emit a single-line Tier B note per 04's failure taxonomy. Only `install-hooks` is allowed to exit non-zero (Tier C).
- **Internal classification enum (architect round-2, for diagnostics only — never exits to Claude Code):** the retrieval pipeline classifies its own completion state into a richer enum that is always logged to `hooks.log` (parsed by `hooks tail --level`) and, under one escape hatch, returned as the process exit code.
  ```go
  const (
    HookOK            = 0
    HookNoIndex       = 10   // ResolveStrictModelDB -> ErrHookModelDBMissing
    HookOllamaDown    = 20   // pre-embed Ollama probe failure
    HookEmbedTimeout  = 30   // embed request exceeded --budget-ms mid-call
    HookModelMismatch = 40   // VerifyHookIndex rejected the index
    HookSkipped       = 50   // skip heuristic fired (length/slash/punctuation); NOT a failure state — hook ran and deliberately exited early. `hooks tail --level` log parsers must not alarm on this code.
    HookInternalErr   = 60   // unclassified panic/error path
  )
  ```
  At every `hook *` command boundary the Go main first computes the internal code, writes one line to `hooks.log` in the form `INFO|WARN hook.<event> code=<name> latency_ms=<N> [key=value …]`, **then** unconditionally `os.Exit(0)` — Claude Code only ever sees zero. The one exception: the `HEIMDALL_HOOK_DEBUG=1` environment variable flips the final exit to `os.Exit(code)` so `hooks doctor`'s dry-fire check #12 and test-rollout's golden assertions can read the real classification. Production hooks **must never** set this env var; `install-hooks` refuses to write `HEIMDALL_HOOK_DEBUG=1` into any hook command string. This preserves 04 §A.3's deterministic-empty invariant for Claude Code while giving diagnostics machine-readable classification.
- Stderr is silent by default. The hook wrapper redirects to `/dev/null`. Real errors go to a log file at **`${XDG_STATE_HOME:-~/.local/state}/heimdall/hooks.log`** (failure-modes 04 §6) with a 10 MB size cap (bumped from 5 MB in HD-5, 2026-04-18) and one rotation generation (`hooks.log.1`). **Stderr must never leak file paths or stack traces** — enforced in `docs/reviews/code-review-context.md`.
- Before any embed call, every retrieval hook invokes `heimdall.VerifyHookIndex(store, requestedModel)` (failure-modes 04 §3) to refuse wrong-but-confident cross-model search. Mismatch emits the Tier B note and skips retrieval.
- **Strict DB resolution (failure-modes 04 §3, confirmed).** Hook subcommands must NOT call `resolveAnyLocalModelDB` (`internal/cli/cli.go:325-336`, CLI counterpart of `server.go:35 resolveAnyModelDB`). That helper fuzzy-matches any indexed model whose Ollama model is pulled — precisely the "wrong-but-confident" cross-model path §3 exists to prevent. Hooks call a new `heimdall.ResolveStrictModelDB(cfg, requestedModel)` helper that: (1) reads the configured model from `.heimdall/hooks.toml` → flag → `cfg.Model`; (2) returns only the `NormalizeModelName(model)` subdirectory; (3) returns a distinct sentinel error (`ErrHookModelDBMissing`) when that subdir is absent, so the hook emits Tier B row #6 (`heimdall: index models [<have>] don't match configured model <want> — run: heimdall index . --model=<want>`); (4) never falls back to another indexed model, ever. The split is physical — `internal/cli/cli.go`'s `hook *` dispatch calls `ResolveStrictModelDB` from a different code path than `cliSearch`/`cliStatus`, never via a `--strict` bool flag on the existing resolver. Non-hook CLI commands keep the fuzzy resolver for back-compat.
- Per-project disable (failure-modes 04 row #15): presence of `<project>/.heimdall/hooks.disabled` (empty marker file) → every hook exits 0 silently within <1 ms. Chosen over a TOML key because stat-ing a marker file is the cheapest presence check available and survives corrupt TOML. `hooks.toml` keys like `[hooks] enabled = false` also work but are secondary.
- `--format` is unused inside `hook` commands (they always emit their own fixed shape) but is critical on the underlying `search`/`recall`/`status` that `hook` composes.
- **Latency/caching flags (latency-eng 03 §3-§7), accepted on every `hook *` subcommand and forwarded to the underlying search/embed path:**
  - `--cache-key <hex>` — override the computed cache key (test-only; normal callers never set this).
  - `--cache-ttl <duration>` — TTL for `hook_cache` rows (default `24h`, per 03 §3).
  - `--no-cache` — force a miss path (debug).
  - `--timeout <duration>` — hard wall-clock kill (default `500ms` on `user-prompt`, `3s` on `session-start`, `5s` on `stop`, `200ms` foreground on `post-edit`; matches 03 §2).
  - `--debounce-window <duration>` — coalesce window for `post-edit` only (default `1s`, per 03 §6).
  - `--batch <path>` (repeatable) — accept multiple paths for a single `post-edit` re-index call. When supplied, the hook bypasses stdin path parsing and re-indexes the given list as one transaction.
  - `--keepalive <duration>` — forwarded as Ollama `keep_alive` in every embed request (default `10m`, per 03 §5). Set to `0` to disable.
  - Each flag is also honored under a `[hooks.latency]` section in `hooks.toml`; flags override TOML, TOML overrides built-in defaults.

### `heimdall-mcp hook session-start`

**Flags:** none required. Optional: `--project <path>` (override CWD), `--max <N>` (default 5), `--budget-ms <N>` (default 1500, architect's p50).

**Stdin:** `SessionStart` event JSON.
```json
{"event": "SessionStart", "session_id": "…", "cwd": "/path", "claude_version": "…"}
```
Fields used: `cwd`. Unknown fields ignored. If stdin is empty, fall back to process CWD.

**Behavior:** internally shells the composer: `heimdall-mcp status --format=json` (machine read) followed by `heimdall-mcp recall --query "<cwd-basename> project context" --limit 5 --format=hook-md`. Composes into the architect's fixed shape. **Also fires `hook cache-warm` inline** (see below) so the embed model is resident before the first `user-prompt` hook runs — latency-eng 03 §5's replacement for goroutine keepalive. **Finally, if `VerifyHookIndex` + `ResolveStrictModelDB` both succeed AND the `ollama_status` probe shows `ok`**, session-start spawns `heimdall-mcp hook drain-reindex-dlq --project <cwd>` **detached** (double-fork, stdin/stdout/stderr to `/dev/null`) so the post-outage DLQ catches up in the background without blocking the 3 s session-start budget. Drain spawn failure is logged and swallowed — it's fire-and-forget.

### `heimdall-mcp hook cache-warm`

Dedicated sub-command invoked **only** from inside `hook session-start` (not registered with Claude Code as its own event). Issues one `POST /api/embed` with `keep_alive: "10m"` and a dummy input so the configured model is resident when the first `user-prompt` hook fires. No DB reads, no cache writes, no `VerifyHookIndex` call (it runs before any store query).

**Flags:** `--keepalive <duration>` (default `10m`), `--timeout <duration>` (default `400ms`).

**Stdin:** none.
**Stdout:** empty.
**Stderr:** silent.
**Exit code:** 0 always — a failed warm-up degrades to cold-start on the first `user-prompt`, which is survivable.
**Target:** ≤50 ms warm, ≤400 ms cold (per 03 §5). Folds into the 3 s SessionStart hard budget as one of the composer's parallel steps.

### `heimdall-mcp hook drain-reindex-dlq`

Dedicated internal sub-command invoked **only** by `hook session-start` (detached double-fork). Not registered with Claude Code as its own event. Walks `<project>/.heimdall_db/hooks/reindex.deadletter.jsonl`, deduplicates by path (keeping newest `ts`), runs the incremental indexer across the unique set, and truncates the file on success. Records whose `attempts >= 5` are dropped and logged `WARN deadletter.abandoned` per the give-up policy.

**Flags:** `--project <path>` (required), `--max-entries <N>` (default `1000`, matching architect's phase-1 scope cap), `--timeout <duration>` (no wall-clock budget by default — the drain is background).

**Stdin:** none.
**Stdout:** empty.
**Stderr:** silent. All progress lines go to `hooks.log` as `INFO dlq.drain paths=<N> queued_from=<earliest_ts>`.
**Exit code:** 0 success (including "nothing to drain"), non-zero is never visible to Claude Code because the parent session-start never reaps the detached grandchild.
**Cap:** if `reindex.deadletter.jsonl` holds >1 000 unique paths, the drain processes the newest 1 000, leaves the rest in the file, and emits a single `WARN dlq.over_cap pending=<N>` log line. The next session-start session continues from there. Session-start itself additionally emits a Tier B "index is stale, run `heimdall-mcp index .`" note (04 §A.2 wording) on its own stdout when the cap is hit, so the user sees it.

**Stdout (markdown, the exact contract from `01-architecture.md` §1):**
```
## Heimdall context

**Project:** <name> · <N> chunks · last indexed <when>
**Model:** <model-name>

- <type>: <content> _(tags: a, b)_
- <type>: <content> _(tags: c)_
- …

_heimdall-mcp — run `heimdall_search` for more context_
```
Hard rules:
1. Single `## Heimdall context` top-level header so Claude recognizes the injection boundary.
2. Capped at ~1.5K tokens (architect's budget).
3. **Empty-results branch split (architect callback + 04 §A.2):** two distinct states with distinct output:
   - *Indexed but nothing recalled for this query:* `## Heimdall context\n\n_no memories yet for this project — run `heimdall-mcp index .` to build an index._`
   - *No index at all (DB dir missing or `ResolveStrictModelDB` returns `ErrHookModelDBMissing`):* 04 §A.2 Tier B form — `## Heimdall context\n\n> heimdall: no index for this project — run: heimdall-mcp index .`
4. No ANSI, no emoji, no trailing whitespace.
5. Degraded (Ollama down, specifically): emit `## Heimdall context\n\n> heimdall: ollama unreachable at <endpoint> — retrieval disabled this session. Start with: ollama serve` (04 row #1). Short reason must not include absolute paths.

**Stderr:** silent.
**Exit code:** 0 always.
**Timeout:** 3 s hard (architect). On internal timeout, heimdall self-aborts and emits the degraded-state note.

### `heimdall-mcp hook user-prompt`

**Hot path.** Architect budget: **300 ms p50, 500 ms hard**. Latency-eng 03 §2 splits the p95 budget by cache state: ≤20 ms on cache hit, ≤250 ms on cache miss + warm Ollama, ≤500 ms hard on cache miss + cold Ollama (first-turn-after-idle). `--budget-ms 300` is the internal deadline; `--timeout 500ms` is the hard kill.

**Flags:**
- `--budget-ms <N>` (default 300) — **wall-clock budget, honored internally**. Required flag per architect.
- `--max <N>` (default 5)
- `--min-prompt-chars <N>` (default 8, latency-eng 03 §4 — keeps short imperatives like "fix bug.", "add test", "refactor foo" in the retrieval path while still catching "hi", "ok", "yes", "thx", "done" acks)
- `--project <path>` (default CWD)

**Stdin:** `UserPromptSubmit` event JSON.
```json
{"event": "UserPromptSubmit", "session_id": "…", "cwd": "…", "prompt": "fix the git commit indexing batch path"}
```
Field used: `prompt`. **Never read prompt from argv** — shell quoting would destroy it.

**Skip heuristic (mandatory per architect §2):** before touching Ollama, exit 0 with empty stdout if any holds:
- `len(strings.TrimSpace(prompt)) < --min-prompt-chars` (default 8, matching 03 §4).
- Prompt starts with `/` (pure slash command).
- Prompt matches `^[\s\p{P}\p{S}]+$` (all punctuation/symbols/emoji).

If not skipped, internally runs `search <prompt> --format=hook-md --limit <max> --budget-ms <budget>`.

**Stdout (markdown, architect's §2 shape):**
```
## Heimdall suggests

### <file>:<Lstart>-<Lend> (score 0.NN)
```<lang>
<snippet truncated to ~400 chars>
```

### <file>:<Lstart>-<Lend> (score 0.NN)
```<lang>
<snippet>
```

_retrieved via heimdall — use heimdall_search to fetch more or full content_
```
Hard rules:
1. Single `## Heimdall suggests` header.
2. Per hit: `###` subheader with file:line-range and score, then a fenced code block.
3. Snippet truncated to ~400 chars (architect's cap). Total output ≤ ~2K tokens.
4. Zero hits → emit nothing, exit 0. **Never** inject a "no results" note (wastes context).
5. Budget exceeded → emit nothing, exit 0. The harness-side timeout is a fallback; we hit the budget internally first so stdout is always clean.

**Stderr:** silent.
**Exit code:** 0 always in phase 1.
**Timeout:** 500 ms hard (architect). Internal `--budget-ms` cuts off work at 300 ms.

### `heimdall-mcp hook post-edit`

**Flags:**
- `--debounce-window <duration>` (default `1s`, confirmed by architect against 04 §A.4)
- `--state-dir <path>` (default `<project>/.heimdall_db/hooks/`, matching 04 §A.4). Resolves all state files — `reindex.lock`, `reindex.pending`, `reindex.last_run`, `reindex.inflight.pid`, `reindex.deadletter.jsonl`, `ollama_status` — as siblings under this directory.
- `--lockfile <path>` (default `<state-dir>/reindex.lock`) — override individual file, rarely needed
- `--pending-file <path>` (default `<state-dir>/reindex.pending`) — override individual file, rarely needed
- `--batch <path>` (repeatable) — re-index a supplied list in one transaction, bypassing stdin path extraction

**Stdin:** `PostToolUse` event JSON.
```json
{"event": "PostToolUse", "tool_name": "Edit", "tool_input": {"file_path": "/abs/path/foo.go"}, "tool_result": {"success": true}}
```
Fields used: `tool_name` (for matcher confirmation), `tool_input.file_path`. Only matched by Claude Code when `tool_name ∈ {Edit, Write}` — the hook can trust the matcher but must still parse `file_path` defensively.

**Behavior:** architect §3 + latency 03 §6 coalesce protocol — read JSON, extract `file_path`, append to `--pending-file`, then try `flock(--lockfile, non-blocking)`. If lock-acquired → probe Ollama (see below), then sleep `--debounce-window`, drain pending file, fork+detach `heimdall-mcp hook post-edit --batch <paths...>` scoped to the enclosing registered project, release lock. If locked → exit 0 (another hook already queued it). Foreground always exits 0 within 200 ms. Pending-file is capped at 500 lines (drop oldest on overflow, per 03 §6).

**Ollama status probe + outage queue (architect round-2):** before the detached actor spends real work, probe Ollama once per debounce cycle via a cached `<state-dir>/ollama_status` file.
- **Probe:** cheap `GET /api/tags` with `Transport.ResponseHeaderTimeout=1s`. Success → write `ok <unix_ts>` to `ollama_status`. Failure → write `down <unix_ts>` to `ollama_status`.
- **Cache TTL:** 30 s. If the file is younger than 30 s, skip the probe and reuse its verdict. 50 rapid edits through the lock-acquire branch therefore cost 1 probe, not 50.
- **On probe=ok:** proceed with the coalesce drain normally. Any per-file failures that happen *inside* the actor (embed rejected, embed timeout mid-call) still append to `reindex.deadletter.jsonl` with the appropriate `err_code`, per the failure-modes 04 taxonomy.
- **On probe=down:** the detached actor is not forked. The current pending batch is drained directly into `reindex.deadletter.jsonl` with `err_code="ollama_unreachable"` and `attempts=0`, then the pending file is emptied. Foreground exits 0 silent. Internal log line `INFO post-edit.dlq.queued paths=<N> reason=ollama_down` goes to hooks.log for `hooks tail` visibility.
- **Drain trigger (architect):** the next `hook session-start` run, upon successful `VerifyHookIndex` + strict resolver + probe=ok, spawns a **detached** `heimdall-mcp hook drain-reindex-dlq --project <cwd>` subprocess. The drain reads `reindex.deadletter.jsonl`, deduplicates paths (keeping the newest), runs the incremental indexer over the unique set, and empties the file on success. Cap: 1 000 entries per drain; beyond that, session-start emits the 04 Tier B "index is stale" note and stops consuming the DLQ until the user runs `heimdall-mcp index .` manually.
- **One unified DLQ file.** The earlier "Dead-letter queue" block and this Ollama-outage path both write `reindex.deadletter.jsonl` — `err_code` distinguishes the cases (`ollama_unreachable` for the pre-flight probe path, `embed_timeout`/`embed_rejected`/etc. for mid-actor failures). There is **no** separate `reindex_dlq.jsonl`.

**Dead-letter queue (shared design with failure-modes 04 A.4; cli-surface owns the file format, failure-modes owns the taxonomy of what-goes-in):**
- **Path:** `<project>/.heimdall_db/hooks/reindex.deadletter.jsonl`. Co-located with `reindex.lock`, `reindex.pending`, `ollama_status` so `uninstall-hooks` wipes one directory.
- **Distinct from `reindex.pending`:** pending = not-yet-tried, evicted on successful drain; deadletter = tried-and-failed, persists across actor runs.
- **Record format:** one JSON line per failed work item: `{"ts": <unix>, "path": "<abs>", "err_code": "<code>", "attempts": <N>}`. `err_code` comes from failure-modes 04 §6 log taxonomy (`ollama_unreachable`, `embed_timeout`, `embed_rejected`, etc.).
- **Drain trigger:** every successful actor run (post-edit coalesce drain with Ollama up + model pulled) flushes matching deadletter rows before taking new pending work. `heimdall-mcp index` and `heimdall-mcp hooks doctor` are additional manual drain points but hooks do not rely on user action.
- **Size cap:** 10 MB or 10,000 lines whichever first; head-truncate on overflow and log `WARN deadletter.truncated dropped=<N>` to hooks.log.
- **Give-up policy:** rows with `attempts >= 5` are dropped on next drain and logged `WARN deadletter.abandoned path=<path> err=<code>`.
- **Retrieval hooks never use the deadletter** — `user-prompt` and `session-start` are ephemeral; only `post-edit` (and phase-2 stop-ingest writes) persist failures here.

**Stdout:** empty. This hook must add zero noise.
**Stderr:** silent.
**Exit code:** 0 always, even on fork failure.
**Timeout:** 200 ms **foreground only** (architect + latency-eng 03 §6). This budget covers the stdin-parse + append-to-pending + flock-attempt + fork-or-exit path. The actual reindex work runs in the forked detached child with no wall budget and may take many seconds on cold Ollama or large files. Reading "`post-edit` timeout = 200 ms" as "reindex within 200 ms" is wrong — the child owns its own context.

### `heimdall-mcp hook stop`

**Flags:**
- `--buffer-dir <path>` (default `${XDG_STATE_HOME:-~/.local/state}/heimdall/sessions/`, confirmed by architect as the closure of 01 OQ #4 via 04 §A.8)
- `--flush-every <N>` (default 5 turns)
- `--flush-kb <N>` (default 64)

**Buffer file format (04 §A.8):** length-prefixed JSON — each turn record is written as `<4-byte BE length><payload-bytes>\n`. The read side in `heimdall-mcp ingest-session --buffer <path>` must honor this length-prefix protocol, not newline-splitting; payloads may themselves contain newlines.

**Stdin:** `Stop` event JSON. Architect explicitly flagged the exact shape as open question §1.
```json
{"event": "Stop", "session_id": "…", "turn_transcript": "…", "turn_index": 7}
```
Implementation must be **defensive about field names** — try `turn_transcript`, fall back to `transcript`, fall back to `message`, else write the whole event JSON to the buffer and let ingest-session handle it.

**Behavior:** architect §4 — append turn to `<buffer-dir>/<session_id>.log`. If turn count ≥ `--flush-every` OR file size ≥ `--flush-kb` KB → fork `heimdall-mcp ingest-session --session-id <id> --buffer <path>` in background and truncate buffer. Foreground exits 0 immediately.

**Stdout:** empty.
**Stderr:** silent.
**Exit code:** 0 always.
**Timeout:** 200 ms foreground.

---

### `heimdall-mcp recall` (new top-level)

Currently MCP-only (`toolRecall` at `internal/mcp/memory_tools.go:137`). Architect §7 requires a CLI equivalent.

**Flags:**
- `--query <string>` (required)
- `--limit <N>` (default 5, max 100 — matches MCP handler)
- `--type <preference|decision|fact|context>` (optional)
- `--tags <csv>` (optional)
- `--project <name>` (optional)
- `--format <text|json|hook-md>` (default `text`)

**Stdin:** none.

**Stdout (text, default, human):**
```
<score> [<type>] <content>
  tags: a, b, c
  project: <name>
```
**Stdout (json):** same shape as the MCP `memoryResult` array in `memory_tools.go:187-208`.

**Stdout (hook-md):** markdown bullets, one per memory, matching the shape composed into `hook session-start`:
```
- <type>: <content> _(tags: a, b)_
```
No header, no footer — pure bullet lines. The composer in `hook session-start` wraps them.

**Stderr:** stays silent when run from a hook. When run interactively (no `HEIMDALL_HOOK=1` env), may print short errors.
**Exit codes:** 0 success, 1 error (for interactive use).

### `heimdall-mcp ingest-session` (new top-level)

Currently MCP-only (`toolIngestSession` at `internal/mcp/memory_tools.go:213`). Forked by `hook stop`.

**Flags:**
- `--summary-stdin` (read transcript summary from stdin) OR `--buffer <path>` (read and delete file) — exactly one required.
- `--project <name>` (optional)
- `--session-id <id>` (optional, for logging)

**Stdin:** raw transcript text if `--summary-stdin`.

**Stdout:** JSON matching `toolIngestSession` response (chunksProcessed, memoriesCreated, etc.). Machine-readable.
**Stderr:** silent when hook-invoked.
**Exit codes:** 0 success, 1 error.

### `heimdall-mcp search --format=hook-md`

Additive format on existing `search`. All existing flags preserved. New flags required by architect §2:
- `--format=hook-md` — emits the user-prompt injection shape (header, per-hit `###` + fenced snippet, footer).
- `--budget-ms <N>` — wall-clock budget honored internally. On timeout, return partial results or empty.
- `--limit <N>` — already exists via the retriever N; needs flag exposure.

Existing human output (`internal/cli/cli.go:360-366`) is preserved as the default format. `--format=text` maps to current human output; `--format=json` emits the MCP-style enriched-result array; `--format=hook-md` emits the architect's shape.

### `heimdall-mcp recall --format=hook-md`

Covered above.

### `heimdall-mcp status --format=json`

Additive format on existing `status`. Default human output (`internal/cli/cli.go:235-309`) is preserved. JSON shape mirrors `toolStatus` response in `internal/mcp/tools.go:485-599` — project name, chunk count, last-indexed timestamp, Ollama state, registered projects.

Used by `hook session-start` as its data source. No behavioral change to human-facing `status`.

### `heimdall-mcp install-hooks`

**Flags (confirmed with failure-modes 04 §4):**
- `--scope <user|project>` (default `user`)
- `--dry-run` — print the proposed diff, write nothing, exit 0
- `--print-diff` — alias of `--dry-run` (explicit name for scripts that want the intent obvious)
- `--force` — **no-op safety valve.** Per 04 §5, install never conflicts with other hooks (composition strategy = append, never replace except on upgrade of our own older version). `--force` is accepted for scripting ergonomics but does nothing destructive.
- `--enable <csv>` (default all four phase-1 hooks)

**Behavior:** atomically (write temp + fsync + rename) parse `~/.claude/settings.json` (or `./.claude/settings.json` for project scope) as JSON, merge the four hook entries under a heimdall-marked block, write. Never touch unrelated keys. Implementation uses `encoding/json`, not regex.

**Marker convention — command-string tag is PRIMARY (architect callback flipped 04 §5 default).** Every hook entry we write appends `--source=heimdall --version=1` to the command invocation, e.g. `heimdall-mcp hook user-prompt --source=heimdall --version=1`. The two flags are silently accepted by every `hook *` subcommand as self-identification no-ops (they land in the same ignore-list as `--tag` would have). Command-string arguments are opaque to Claude Code by construction (exec argv), so the marker is guaranteed safe regardless of `settings.json` schema tolerance. Upgrade/uninstall/`hooks doctor` parse the command string to find heimdall entries; this is the always-on path.

**Marker secondary (additive, if probe succeeds):** if a one-time `install-hooks` round-trip probe shows Claude Code preserves unknown JSON fields, we additionally write `"source": "heimdall"` + `"version": 1` on the hook entry object. This makes JSON-level tooling (jq queries, Claude Code's own hook introspection if it ever ships one) aware of our entries without changing the command-string primary path. If the probe fails or is inconclusive, we skip the JSON fields entirely. The command-string tag is never optional.

Matcher strings and binary paths come from `01-architecture.md` (single source of truth), read from a baked-in manifest so `install-hooks` and the hook contracts can't drift.

**Stdout:** human diff (always, in `--dry-run` and real runs).
**Stderr:** Tier C one-line errors per 04 §4 (`settings.json is not valid JSON at line N — fix or delete and retry`, `cannot write settings.json: permission denied`, etc.).
**Exit:** 0 ok / 1 any write or permission failure. **Tier C is the only phase-1 non-zero exit allowed.**

Test fixture: existing settings.json with three unrelated hooks → after install, those three survive untouched.

### `heimdall-mcp uninstall-hooks`

Symmetric. Removes any hook entry whose command string contains `--source=heimdall` (the primary marker). If the probe also wrote JSON fields, those come off with the entry automatically. If nothing to remove, exit 0 silently (per 04 §4).

### `heimdall-mcp hooks doctor`

Promoted to a `hooks` sub-subcommand per failure-modes 04 §6. Runs every check a hook would run, reports pass/fail for each, prints the last 20 log lines, **does not modify state**.

**Checks (from 04 §6):**
1. `heimdall-mcp` binary on PATH — yes/no + absolute path
2. Settings file exists + valid JSON + hooks block present
3. Heimdall hook entries present (by command-string `--source=heimdall` marker; additionally checks `"source": "heimdall"` JSON field if probe recorded it)
4. Ollama reachable at configured endpoint
5. Configured model pulled in Ollama
6. `.heimdall_db/` present in CWD
7. Index `embedding_model` matches configured model (runs `VerifyHookIndex`)
8. Index `embedding_dim` matches current model's test-embed dim
9. Log file at `${XDG_STATE_HOME:-~/.local/state}/heimdall/hooks.log` writable
10. Config file `.heimdall/hooks.toml` (if present) parseable
11. Foreign `UserPromptSubmit` hooks from other tools — listed, not flagged (observation, not error)
12. Dry-fire each of the four hook commands under `HEIMDALL_HOOK_DEBUG=1` (architect round-2) with a **retrieval-triggering** synthetic event JSON (e.g. a 40-character prompt like `show me the indexer auto index function` that defeats the skip heuristic), so the check exercises `VerifyHookIndex` + Ollama embed + store scan end-to-end AND reads the real internal classification code (the debug env var flips `os.Exit(0)` to `os.Exit(code)`). Assert the internal code is `HookOK=0`; if it's `HookNoIndex=10` / `HookOllamaDown=20` / `HookEmbedTimeout=30` / `HookModelMismatch=40`, emit the matching FAIL line with remediation. A skip-heuristic-only prompt would false-OK on a broken Ollama connection.

Each check emits one line: `OK|WARN|FAIL description [remediation]`.

**Exit codes:** `0` if all checks OK or WARN only, non-zero if any FAIL. Safe to run in CI — test-rollout uses this as the primary fire-and-assert tool.

### `heimdall-mcp hooks cache-clear`

Per latency 03 §3. Drops the `hook_cache` table contents (not the schema) for the current project DB.

**Flags:**
- `--project <path>` (default CWD) — target project DB
- `--all-models` — clear cache across every per-model subdirectory under the project's DB dir (default: current model only)

**Stdout:** one line per DB cleared: `cleared <path>: <N> rows`.
**Stderr:** one-line error per failure; never leaks absolute paths inside the repo.
**Exit:** 0 ok / 1 if any DB failed to open.

### `heimdall-mcp hooks cache-stats`

Per latency 03. Emits hit/miss/size counters for `hook_cache`.

**Flags:**
- `--project <path>` (default CWD)
- `--format <text|json>` (default `text`)
- `--all-models` — aggregate across per-model DBs

**Stdout (text):** one block per model with `rows`, `total_hits`, `oldest`, `newest`, `bytes`.
**Stdout (json):** same data, machine-readable.
**Exit:** 0 ok / 1 on DB open failure.

### `heimdall-mcp hooks tail`

Thin wrapper around `tail -F` on the log file (failure-modes 04 §6). One-shot helper, does not daemonize.

**Flags:**
- `--level <info|warn|error>` — filter by log level
- `--event <session-start|user-prompt|post-edit|stop>` — filter by hook
- `--since <duration>` (e.g. `5m`, `1h`) — tail only recent entries
- `--project <path>` — filter by project root in the log key=value pairs
- `--follow` (default on — `-F`-style follow)

**Stdout:** log lines matching filters.
**Stderr:** errors opening log.
**Exit:** 0 normal / 1 log file not found or unreadable.

Handy for "why did my prompt feel weird just now" — test-rollout and failure-modes both use this for after-the-fact inspection.

---

## Config file design

**Decision: `.heimdall/hooks.toml` in-project, with fallback to `~/.config/heimdall/hooks.toml` globally.**

Per-repo opt-out is a first-class need; in-project config is git-versioned so teams stay consistent; global fallback covers "on everywhere" setups. Matches existing `.heimdall_db/` directory convention.

```toml
[hooks]
enabled = true

[hooks.session_start]
enabled = true
max = 5
budget_ms = 1500

[hooks.user_prompt]
enabled = true
max = 5
budget_ms = 300
min_prompt_chars = 8

[hooks.post_edit]
enabled = true
debounce_sec = 2

[hooks.stop]
enabled = true
flush_every = 5
flush_kb = 64
buffer_dir = "${XDG_STATE_HOME:-~/.local/state}/heimdall/sessions"

[hooks.latency]
cache_ttl = "24h"
keepalive = "10m"
debounce_window = "1s"
# per-hook timeout overrides
user_prompt_timeout = "500ms"
session_start_timeout = "3s"
stop_timeout = "5s"
post_edit_foreground_timeout = "200ms"
```

Flags override TOML; TOML overrides built-in defaults. Read once at process start.

---

## Backward-compat check (verified against current code)

- `index`, `search`, `status`, `projects`, `configure`/`config`, `paths`, `models`, `help` dispatch in `internal/cli/cli.go:32-80` — all unchanged in behavior. New formats are additive via `--format`.
- Global `--out`/`-o` parse loop at `internal/cli/cli.go:20-31` stays. **`hook *` commands do not accept `--out`** and **must not call `resolveAnyLocalModelDB`** at `internal/cli/cli.go:325-336` — they call the new `heimdall.ResolveStrictModelDB` helper instead (see Shared Conventions, per failure-modes 04 §3). `cliSearch` / `cliStatus` / `cliIndex` keep the existing fuzzy resolver for back-compat. The split is physical — two distinct call sites — so no `hook` code path can accidentally reach the fuzzy matcher.
- `cliSearch` default output (`internal/cli/cli.go:360-366`) stays the default. `--format=hook-md` is opt-in.
- `cliStatus` default output stays. `--format=json` is opt-in.
- `cliIndex`'s interactive `promptModelSelection` at `internal/cli/cli.go:122` stays interactive. Architect §7 confirms hooks never call `index`, so this is fine. All `hook *` subcommands are strictly non-interactive (enforced: no stdin read except event JSON, no `prompt*` calls, `HEIMDALL_HOOK=1` env var sets a global "no TTY" bit).
- MCP server (`internal/mcp/`) untouched. Both CLI and MCP wrap the same `heimdall` package functions. The new top-level `recall` / `ingest-session` CLI commands reuse `heimdall.Memory*` and `heimdall.IngestSession` the same way `toolRecall` and `toolIngestSession` do.
- Exit-code semantics: existing commands use 0/1. `hook *` commands always exit 0 in phase 1 — no clash.

---

## Coordination notes

- **hook-architect (review round confirmed):** 02 approved end-to-end against 01. Delta fixes folded: (a) empty-results branch split — "indexed but no recall" vs "no index at all" now emit distinct text, the latter using 04 §A.2 Tier B form. (b) `--budget-ms` confirmed as internally-enforced `context.WithTimeout` deadline, checked before any stdout write (not a harness-side timeout). (c) Ollama-down-pre-embed → Tier B visible note (04 row #1) with rate-limit once-per-session; embed-timeout-mid-call → silent empty (04 §A.3). (d) Marker strategy **flipped**: command-string `--source=heimdall --version=1` is primary (guaranteed-safe, exec argv is opaque to Claude Code); JSON fields are additive only if the `install-hooks` round-trip probe shows preservation. (e) `--buffer-dir` default fixed to `${XDG_STATE_HOME:-~/.local/state}/heimdall/sessions/`, length-prefixed JSON protocol documented on `hook stop` and referenced by `ingest-session`. (f) `--state-dir` default moved to `<project>/.heimdall_db/hooks/` matching 04 §A.4 scheme. (g) `hooks doctor` check #12 now fires a retrieval-triggering prompt to actually exercise Ollama, not a skip-heuristic prompt that would false-OK. (h) Architect's rulings close 02 OQs #1-#4, #8, #9 — only #5 (confirmed), #6 (product call), and #7 (bikeshed) remain.
- **latency-eng round 2 (folded):** Added `hook cache-warm` as an internal composer sub-command called inline from `session-start` (not a separately-registered Claude Code event). Flags `--keepalive` (10m default) and `--timeout` (400ms default); exits 0 always; no DB writes. Budget split noted on `hook user-prompt`: ≤20ms cache hit / ≤250ms miss warm / ≤500ms miss cold. Correction on your review: the current doc already had `--timeout 500ms`, `--cache-key <hex>`, 8→`--min-prompt-chars 12` (I adopted 12 not 8 — happy to drop to 8 if you insist), `--cache-ttl 24h`, `--no-cache`, `--keepalive 10m`, `--debounce-window 1s`, `--batch`, and top-level `hooks cache-clear` / `hooks cache-stats` — you may have been reading a stale version. The only real deltas from your review are cache-warm and the split budget; both are in now.
- **latency-eng (round 1, folded 03 requests):** all seven flags you listed (`--cache-key`, `--cache-ttl`, `--no-cache`, `--timeout`, `--debounce-window`, `--batch`, `--keepalive`) are exposed on the `hook *` subcommand tree in the Shared Conventions block above and wired into `post-edit`'s per-command section. `hooks cache-clear` and `hooks cache-stats` are added to the inventory with full signatures. `--budget-ms` stays on `hook user-prompt` and `search --format=hook-md` as an internally-enforced wall clock so stdout is always clean-empty on deadline rather than truncated. **Answers to your two open questions:** (1) **Multi-model — hooks must always use `cfg.Model`**, not smallest-dim. Closing OQ#1 against you: failure-modes 04 §3 requires `VerifyHookIndex` to bail Tier B when the store's `embedding_model` doesn't match the caller's configured model, and 03 §7 OQ#4 already marks this closed under the same reasoning. If the user wants the bge-m3 penalty hidden, they configure `Model = nomic-embed-text` at the project level — that is the single knob. "Smallest-dim wins" would silently cross-pollute embedding spaces via `ResolveUsableModelDB`'s fuzzy matcher, which is a correctness failure, not a latency trade. (2) **Cache key filter set — yes, include `sub_project` and `source_type`**. 03 §3 already encodes them; the filter set surfaced on the hook CLI is exactly those two plus `canonical_json(metadata_filter)` (empty object in phase 1 since we don't expose raw metadata filters from hook subcommands). The `hook user-prompt` command itself has **no** `--sub-project` / `--source-type` flags in phase 1 — both are inherited implicitly from the resolved project context. If we ever add them, the cache-key composition in 03 §3 already handles them with no schema change.
- **failure-modes (architect round-2 delta):** please update 04 row #2 (Ollama-down-during-edit) to reference the unified `reindex.deadletter.jsonl` DLQ as the degradation mechanism rather than "silently dropped". Post-edit now probes Ollama before forking the actor and writes affected paths to the DLQ with `err_code="ollama_unreachable"` when probe=down. The next successful `hook session-start` spawns `hook drain-reindex-dlq` detached to catch up. Single DLQ file — no separate `reindex_dlq.jsonl` path. The internal classification enum (`HookOllamaDown=20`, etc.) is logged to `hooks.log` in a form compatible with `hooks tail --level` filters; please confirm your 04 §6 log taxonomy row wording matches the `INFO|WARN hook.<event> code=<name> latency_ms=<N>` shape I've specified.
- **failure-modes (confirmed):** exit code taxonomy (`0`/`2`/other-ignored) — ✓ matches 04 §1-2. Install flags `--dry-run` / `--print-diff` / `--force`-as-no-op — ✓ adopted verbatim. `hooks doctor` and `hooks tail` under the `hooks` sub-subcommand — ✓. Log file at `${XDG_STATE_HOME:-~/.local/state}/heimdall/hooks.log` with 5 MB cap + one rotation — ✓. Marker fields `"source": "heimdall"` + `"version": N` with command-string fallback (`--tag=heimdall-v1`) — ✓, both strategies specified. Per-project disable via `.heimdall/hooks.disabled` marker file — ✓. One question back: for `user-prompt` Ollama-down-pre-embed I'm using Tier B visible note (matching 04 row #1); embed-timeout-mid-call stays silent-empty (matching 04 §A.3). Confirm the split.
- **test-rollout:** `hooks doctor` is your CI harness. Every hook command's stdout is byte-stable and assertable. `--format=json` on `search`, `recall`, `status`, `ingest-session` gives machine-readable assertions. Golden test fixtures per 04 coordination: one row per failure in the 04 taxonomy table, with exit code + stdout + log-line assertions.

---

## Open questions for human decision

Post-architect review round. Previously-open questions #1-#4, #8, #9 are now closed; remaining items require only the human product call on #6.

1. ~~**Binary name.**~~ **Closed by architect:** `heimdall-mcp` for v1; ship a `heimdall` symlink later.
2. ~~**`hook` namespace vs flat.**~~ **Closed by architect:** keep `hook <event>`, discoverability wins.
3. ~~**TOML vs JSON for `hooks.toml`.**~~ **Closed by architect:** TOML is fine, no new dep concerns (Go stdlib-only parser).
4. ~~**`recall` / `ingest-session` as top-level vs namespaced.**~~ **Closed by architect:** top-level is correct, they're generally useful primitives.
5. **`user-prompt` on Ollama-down vs embed-timeout.** **Confirmed by architect** against 04 row #1 + §A.3. Ollama-down-pre-embed → Tier B visible note, rate-limited once per session. Embed-timeout-mid-call → silent empty, deterministic no partial flush.
6. **Opt-in vs opt-out `install-hooks`.** Still open — architect marked as product call for the human. Recommendation from cli-surface + architect: **opt-in** via explicit `heimdall-mcp install-hooks` run, not auto-fired on first `index`. Rollout is reversible this way.
7. **Output-format name bikeshed.** `hook-md` retained. No blocker.
8. ~~**Marker field strategy choice.**~~ **Closed by architect:** flipped — command-string `--source=heimdall --version=1` is always-on primary (guaranteed safe — exec argv is opaque to Claude Code). JSON `"source"`/`"version"` fields become additive secondaries only if the `install-hooks` first-run probe confirms Claude Code round-trips unknown fields.
9. ~~**Per-project disable mechanism.**~~ **Closed by architect:** marker file `.heimdall/hooks.disabled` is correct — fastest presence check.
