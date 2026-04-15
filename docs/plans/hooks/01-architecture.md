# Hook Architecture & Data-Flow Contract

Plan 01 of the Claude Code hooks integration. Owns the question *which hooks fire, how they shape stdin/stdout, and what Claude sees after each one*. Planning only — no code changes.

## Goal

heimdall-mcp already exposes 11 MCP tools, but in practice Claude rarely calls them — tool exposure does not guarantee tool use. Claude Code's native hooks system lets us shift from "Claude might call heimdall" to "heimdall runs deterministically at specific lifecycle points, and its output lands in Claude's context before Claude decides what to do." This document fixes the contract between Claude Code and heimdall at each hook point: which events we subscribe to, what shape the event payload takes on stdin, what heimdall writes to stdout, and exactly what Claude will end up seeing. Everything downstream — the CLI surface, latency budget, degradation behavior, and test harness — depends on this contract being precise.

## Hook Event Decisions

Assessment is grounded in what heimdall can do *today* (per `internal/mcp/server.go`, `internal/mcp/memory_tools.go`, `internal/cli/cli.go`): semantic code search, memory recall/remember, session ingestion, git-commit indexing (runs automatically inside `runIndex`), text indexing, and status. The CLI binary today exposes `index`, `status`, `search`, `projects`, `config`, `paths`, `models` only — `recall`, `remember`, `ingest-session`, and `index-text` are MCP-only. That gap drives several coordination asks to `cli-surface`.

| Event | Decision | Hook type | Rationale |
|---|---|---|---|
| `SessionStart` | **USE (phase 1)** | `command` | Highest leverage for lowest risk. Fires once, stdout is injected into initial context. `heimdall_recall` + top project memories are exactly the shape it wants. Not latency-sensitive. |
| `UserPromptSubmit` | **USE (phase 1)**, gated | `command` with internal skip heuristic (phase 1); reconsider `prompt` in phase 2 | Highest leverage for quality. Fires every turn — hot path, budget ≤300 ms p50. `prompt`-type hook (mini-model judgement) is appealing but adds a model round-trip which may exceed the budget we want; defer it. A `command` with a cheap keyword/length skip heuristic is the phase-1 pick. |
| `PostToolUse(Edit\|Write)` | **USE (phase 1)**, fire-and-forget | `command`, background | Keeps the index fresh as Claude edits. Must never block — spawn detached process, always exit 0, stdout empty. Indexer must handle concurrent invocations safely (coordination ask to `failure-modes`). |
| `PostToolUse(Bash(git commit *))` | **SKIP — already covered** | — | `runIndex` in `internal/mcp/server.go:328` already re-indexes git commits automatically after any `heimdall_index` run, and the stale-index check at `:435` triggers re-indexing on search. The marginal value of a dedicated commit hook is tiny; the incremental indexer + stale check already cover it. Revisit if the stale-check window proves too coarse. |
| `Stop` / `SessionEnd` | **USE (phase 1)** on `Stop`, not `SessionEnd` | `command`, background | `heimdall_ingest_session` exists and wants a conversation summary. `Stop` fires at end of each assistant turn and gives us natural checkpoints. `SessionEnd` is less reliable (may not fire on crash). Fire-and-forget, never block. |
| `PreToolUse(Bash(rm *\|git push --force*))` | **DEFER to phase 2** | `agent` (when adopted) | Valuable but complex: needs judgement, not retrieval. Today heimdall has no "is this destructive given prior memories?" primitive, and the `agent` hook type requires spawning a subagent — best done once the phase-1 pipeline is stable and `heimdall_recall` has meaningful data. |

Phase 1 scope: **SessionStart, UserPromptSubmit, PostToolUse(Edit|Write), Stop.**

## Per-Hook Contracts

### 1. `SessionStart`

- **Stdin payload (sketch):**
  ```json
  {
    "event": "SessionStart",
    "session_id": "…",
    "cwd": "/home/noname/Code/heimdall-mcp",
    "claude_version": "…"
  }
  ```
- **Hook command:** `heimdall-mcp hook session-start` (new subcommand — ask to `cli-surface`).
- **Stdout format:** Markdown. A single fenced block titled `## Heimdall context` containing: (a) the project name and indexed-chunk count from `heimdall status`, (b) up to N=5 top memories from `heimdall recall --query "<cwd basename> project context" --limit 5`, each rendered as a bullet with `type`, `content`, and `tags`. Keep under ~1.5K tokens — Claude sees this as system context and we don't want to burn budget here.
- **Exit codes:** `0` always. `SessionStart` should never block the session. If heimdall fails internally, print a single-line `> heimdall: unavailable (<reason>)` note to stdout and exit 0 — Claude sees the degraded state but the session proceeds.
- **Timeout:** 2 s p95 target, 5 s hard (per 03 §2). Beyond that the harness kills the hook and proceeds.
- **Stderr policy:** silent (`>/dev/null` in the hook wrapper). Real errors go to heimdall's own log file, not the user's terminal.

### 2. `UserPromptSubmit`

- **Stdin payload (sketch):**
  ```json
  {
    "event": "UserPromptSubmit",
    "session_id": "…",
    "cwd": "…",
    "prompt": "fix the git commit indexing batch path"
  }
  ```
- **Hook command:** `heimdall-mcp hook user-prompt` reading the JSON from stdin.
- **Skip heuristic (inside the command):** read prompt, skip (exit 0 with empty stdout) if: prompt length < 12 chars, prompt is a pure slash command (`/…`), or prompt is all punctuation/emoji. Everything else runs retrieval.
- **Stdout format:** Markdown, pre-formatted for direct injection. Header `## Heimdall suggests`, then up to 5 results from `heimdall search "<prompt>" --format=hook`. Each result is a 3-line block: `### file:line-range (score 0.NN)` then a fenced snippet truncated to ~400 chars. A trailing line `_retrieved via heimdall — use heimdall_search to fetch more or full content_` prompts Claude to fetch more if needed. Total output capped at ~2K tokens.
- **Exit codes:** `0` always in phase 1 (never block the turn). Reserve exit 2 for the phase-2 destructive-op hook only.
- **Timeout:** **250 ms p95 target, 500 ms hard** (per 03 §2, backed by measured 180–380 ms end-to-end on a 10k-chunk nomic index). On timeout the harness kills, turn proceeds with no injection. Empty stdout on timeout is strictly better than blocking.
- **Stderr policy:** silent.

### 3. `PostToolUse(Edit|Write)`

- **Matcher:** `Edit|Write` tool names.
- **Stdin payload (sketch):**
  ```json
  {
    "event": "PostToolUse",
    "tool_name": "Edit",
    "tool_input": {"file_path": "/abs/path/foo.go", "...": "..."},
    "tool_result": {"success": true, "...": "..."}
  }
  ```
- **Hook command:** `heimdall-mcp hook post-edit` reading JSON from stdin, extracting `tool_input.file_path`.
- **Behavior:** fork+detach a background re-index of the single file (or the enclosing directory if the file belongs to a registered project). The foreground process exits 0 immediately with empty stdout.
- **Stdout format:** empty. This hook must not add noise to Claude's context.
- **Exit codes:** `0` always, even on backgrounding failure. A lockfile-based debouncer in heimdall coalesces rapid edits.
- **Timeout:** 200 ms for the foreground call (the background indexer has its own context).
- **Stderr policy:** silent.

### 4. `Stop`

- **Stdin payload (sketch):**
  ```json
  {
    "event": "Stop",
    "session_id": "…",
    "turn_transcript": "… full assistant turn text …",
    "turn_index": 7
  }
  ```
  (Exact shape to be confirmed against Claude Code docs — open question below.)
- **Hook command:** `heimdall-mcp hook stop`.
- **Behavior:** writes the turn transcript into a per-session rolling buffer file. Every N turns (default 5) or when the buffer exceeds M KB, forks a background `heimdall ingest-session` on the buffer and clears it. Foreground exits 0 immediately.
- **Stdout format:** empty.
- **Exit codes:** `0` always.
- **Timeout:** 200 ms.
- **Stderr policy:** silent.

## Data Flow: Worked Examples

### Scenario A — Fresh session in `heimdall-mcp` repo

1. User runs `claude` in `/home/noname/Code/heimdall-mcp`.
2. Claude Code dispatches `SessionStart` → JSON on stdin → `heimdall-mcp hook session-start`.
3. Hook shells `heimdall-mcp status --format=json` → sees project is registered, 12K chunks indexed, Ollama reachable.
4. **`VerifyHookIndex(store, cfg.Model)` runs** (per plan 04 §3). If the indexed `embedding_model` metadata does not match the configured model, the hook emits *only* a Tier B warning (`heimdall: index models [...] don't match configured model X — run: heimdall index . --model=X`) and skips recall entirely. No fuzzy resolution via `ResolveUsableModelDB` is allowed in the hook path — cosine across embedding spaces produces plausible-looking noise, which is worse than no retrieval.
5. Hook shells `heimdall-mcp recall --query "heimdall-mcp project context" --limit 5 --format=hook-md`. Output is composed into a markdown block (status line + 5 bullets). Written to stdout in <1.5 s.
6. Hook exits 0. Claude Code appends stdout to initial context.
7. Claude sees: project is heimdall-mcp, 12K chunks, "decision: use SQLite+sqlite-vec for vector store", "preference: no --no-verify", etc. — before the user types anything.

### Scenario B — User types "fix the git commit indexing batch path"

1. User hits Enter. Claude Code dispatches `UserPromptSubmit` with the prompt → stdin → `heimdall-mcp hook user-prompt`.
2. Skip heuristic: prompt length 42, not a slash command → run retrieval.
3. Hook shells `heimdall-mcp search "fix the git commit indexing batch path" --format=hook-md --limit 5 --budget-ms 280`.
4. **`VerifyHookIndex(store, cfg.Model)` runs** (per plan 04 §3). Must use the configured model exactly — no fuzzy resolve. On mismatch: emit nothing (after the first surfaced warning, the 5-min rate-limit in 04 §2 collapses subsequent turns to silent Tier A) and skip the embed. On match, proceed. Note: cache hits are automatically model-scoped because `hook_cache` lives inside `<dbdir>/<model>/vectors.db` (per 03 §3) — a model swap opens a different physical file with a different cache, so there is no cross-model contamination path even if VerifyHookIndex were somehow bypassed.
5. Internal path: `retriever.Retrieve` embeds the query via Ollama (~50 ms warm) → queries sqlite-vec (~30 ms) → ranks and returns 5 enriched blocks. Formatter serializes them into the hook markdown shape.
6. Hook stdout = the markdown block. Exit 0.
7. Claude Code injects the block as context for this turn.
8. Claude now sees `git_indexer.go:42 IndexGitCommits(…)` + snippet *before* it decides what to do, and proceeds with the edit using real context instead of guessing.
9. After Claude finishes responding, `Stop` fires → transcript written to rolling buffer → maybe triggers ingest-session in background.

## Dependencies on Teammates

- **`cli-surface`** — I need the following added to the CLI binary (phase 1, blocking):
  1. `heimdall-mcp hook session-start` — reads SessionStart JSON, writes markdown to stdout, exit 0 on any error.
  2. `heimdall-mcp hook user-prompt` — reads UserPromptSubmit JSON, applies skip heuristic, writes markdown, honors `--budget-ms`.
  3. `heimdall-mcp hook post-edit` — reads PostToolUse JSON, spawns detached re-index.
  4. `heimdall-mcp hook stop` — reads Stop JSON, appends to rolling buffer, triggers `ingest-session` when full.
  5. `heimdall-mcp recall --format=hook-md` and `heimdall-mcp search --format=hook-md` — JSON is wrong here; we need the markdown shape defined above.
  6. `heimdall-mcp status --format=json` — machine-readable for the session-start composer.
  7. Confirm the CLI shape is stable enough to be called from a hook (exit codes, no interactive prompts — `cliIndex` currently calls `promptModelSelection`, which is fine because hooks never call index, but confirm).
- **`latency-eng`** — budgets confirmed in 03 §2 against measured data:
  - `SessionStart`: 2 s p95, 5 s hard.
  - `UserPromptSubmit`: **250 ms p95, 500 ms hard**. Hot path. Mitigated by the SQLite-backed cache in 03 §3 (keyed on normalized prompt + `index_version`), length guard (<8 chars skip), and Ollama `keep_alive: "10m"` on every embed request. No `prompt`-type hook (rejected on latency grounds — a cheap-LLM gate is 100-500 ms on the same Ollama and strictly worse than caching).
  - `PostToolUse(Edit|Write)`: fire-and-forget, no wall budget. Coalesced via lockfile + 1 s debounce (03 §6).
  - `Stop`: fire-and-forget, no wall budget.
- **`failure-modes`** — contract points (all resolved in 04 Addendum A, endorsed here):
  1. `SessionStart` + Ollama down → Tier B single-line note (§A.1). Exit 0.
  2. `UserPromptSubmit` + no index or no matching model → Tier B single-line note (§A.2). No `autoIndexOnSearch` reuse from `internal/mcp/tools.go:383`. Exit 0.
  3. `UserPromptSubmit` + embed/scan timeout → **empty stdout, deterministic, no partial flush** (§A.3). Single `context.WithTimeout` around embed+scan. Partial top-N is non-deterministic, strictly worse than silent degrade.
  4. `PostToolUse` debouncer → lockfile + 1 s coalesce window with detached actor (§A.4). Foreground <50 ms. Stale `inflight.pid` reaper via `kill(pid, 0)`. Crash-mid-reindex safe.
  5. Fresh repo → do NOT auto-index; Tier B note (§A.5). Same as (2).
  6. **Hooks skip `checkAndTriggerReindex` at `internal/mcp/tools.go:435` entirely** (§A.6). Hook retrieval path is read-only wrt vectors.db. Prevents double-triggered background reindex → SQLite corruption. PostToolUse(Edit|Write) is the sole re-index trigger.
  7. Stop buffer location and format (§A.8) — see OQ #4 below, now closed.
- **`test-rollout`** — fire/effect pairs the harness must assert:
  1. SessionStart fires → stdout contains `## Heimdall context` → Claude's first message context includes a bullet matching a seeded memory.
  2. UserPromptSubmit fires with prompt "fix the foobar" → stdout contains `## Heimdall suggests` → retrieval returns a known seeded chunk.
  3. UserPromptSubmit fires with prompt "/help" → stdout empty (skip heuristic).
  4. PostToolUse(Edit) fires → stdout empty → within 5 s, index `last_indexed` metadata has advanced.
  5. Stop fires 5 times → a background ingest-session runs → memory count increases by ≥1.
  6. Ollama killed → SessionStart exits 0 with a degraded-state note → session still usable.
  7. `--budget-ms` honored → UserPromptSubmit with a synthetic slow embedder returns empty stdout within budget, does not block the turn.
- **`install-hooks` (phase 1 owner TBD, likely `test-rollout` or a separate task)** — `heimdall-mcp install-hooks` must merge into `~/.claude/settings.json` without clobbering user hooks; the matcher strings and paths are defined here, not there.

## Open Questions for Human Decision

These should NOT be guessed — they need a call before implementation:

1. **Exact `Stop` event payload.** I sketched `turn_transcript`, but the Claude Code docs should be consulted for the real field name and whether the full assistant message is actually delivered on stdin. This gates hook 4.
2. **~~UserPromptSubmit latency budget.~~** Closed by 03 §2: 250 ms p95, 500 ms hard. Measured 180 ms warm / 380 ms cold on a 10k-chunk nomic index. First-turn-after-idle may spike to ~400 ms; accepted for v1.
3. **~~`prompt`-type hook for UserPromptSubmit later.~~** Closed by 03 §4: rejected. A cheap-LLM gate is 100–500 ms on the same Ollama and strictly worse than the cache + length-guard approach. Not revisited.
4. **~~Rolling buffer location for Stop.~~** Closed by 04 §A.8: `${XDG_STATE_HOME:-~/.local/state}/heimdall/sessions/<session_id>.log`, length-prefixed JSON records (4-byte BE length + payload + newline), single `write(2)` <PIPE_BUF atomic, no fsync. Read protocol: rename → `.ingesting`, scan by length prefix, detect truncation, rename to `.corrupt.<ts>` on failure. Reaper: 24 h → auto-ingest, 7 d → delete. 2 MB per-buffer cap, truncate-head.
5. **~~Debouncer window for PostToolUse.~~** Closed by 04 §A.4 and 03 §6: lockfile + 1 s coalesce window.
6. **Opt-in vs opt-out.** Should `heimdall-mcp install-hooks` be opt-in (explicit flag) or run automatically on first `heimdall-mcp index`? Phase-1 default matters for rollout.
7. **Output-format name.** I used `--format=hook-md` above. Bikeshed as needed — just make sure it's distinct from the existing human CLI output so we don't break `heimdall-mcp search` for humans. (cli-surface's 02 proposes `--format=markdown` under the `hook` namespace; fine either way.)
8. **Does Claude Code tolerate unknown fields on hook entries in `settings.json`?** Plan 04 §5 wants a `"source": "heimdall"` + `"version"` marker for safe compose/upgrade/uninstall. The primary marker should be encoded in the command string itself (`heimdall-mcp hook prompt-submit --source=heimdall --version=1`) — guaranteed to work because the command string is opaque to Claude Code. Adding the marker as object fields on the hook entry is additive, and only kept if Claude Code accepts unknown fields. This question needs confirmation from the Claude Code docs before `install-hooks` lands.
9. **Hook queuing semantics.** (Raised by 03 OQ #1.) If a `UserPromptSubmit` hook is still running when the user submits the next prompt, does Claude Code (a) wait for the previous, (b) kill the previous, or (c) fire a new one in parallel? Determines whether we need per-session serialization or just rely on SQLite WAL + `busy_timeout` to handle the concurrent-write case. Parallel is acceptable (cache is safe, worst case is a duplicated embed) but we should confirm behavior empirically in the test harness if the docs don't say.
10. **Cross-process store write lock.** (Raised by 04 Addendum A.) Hook retrieval path is already read-only on `vectors.db` (OQ-closed #6 above), but `hook_cache` writes from multiple concurrent `UserPromptSubmit` hooks and PostToolUse indexer writes can still contend on a shared SQLite file. **Proposed resolution:** put `hook_cache` in its own file at `.heimdall_db/hooks/cache.db` rather than colocating in `vectors.db`, so the hot path touches only a dedicated cache store. Open for `failure-modes` confirmation.
