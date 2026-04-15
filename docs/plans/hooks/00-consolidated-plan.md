# 00 — Consolidated Implementation Plan: Claude Code Hooks Integration

**Status:** Review-ready. Consolidates plans 01–05. No code changes yet.
**Audience:** human approver + implementing engineer(s).

---

## 1. Goal

heimdall-mcp exposes 11 MCP tools today, but tool exposure does not guarantee tool use — in practice Claude rarely calls them. Claude Code's native hooks system lets us shift from "Claude might call heimdall" to "heimdall runs deterministically at specific lifecycle points, and its output lands in Claude's context before Claude decides what to do." This plan fixes the contract between Claude Code and heimdall at four phase-1 hook points, reconciles the four cross-plan inconsistencies raised by `test-rollout`, and produces an ordered implementation plan an engineer can execute.

## 2. Scope

**In scope for v1 (phase 1a + 1b):**
- Four `command`-type hooks: `SessionStart`, `UserPromptSubmit`, `PostToolUse(Edit|Write)`, `Stop`.
- CLI surface needed to drive them: four `heimdall-mcp hook <event>` subcommands, plus top-level `recall` and `ingest-session` (phase 1a prerequisites), plus new `--format=hook-md` on `search`/`recall` and `--format=json` on `status`.
- Install/uninstall/doctor/tail/cache-clear tooling.
- Per-project opt-out via `.heimdall/hooks.disabled` marker file; global kill switch via `HEIMDALL_HOOKS=0`.
- SQLite-backed `hook_cache` table keyed on normalized prompt + index version + filter scope, 24 h TTL, 1 000 row cap.
- Failure-mode taxonomy (Tier A/B/C) with structured logging to `${XDG_STATE_HOME}/heimdall/hooks.log` and a deadletter queue for background write failures.

**Explicitly out of scope for v1:**
- `PostToolUse(Bash(git commit *))` — already covered by `runIndex`'s built-in git indexing + stale-check path. Revisit only if we remove that auto-path.
- `PreToolUse(Bash(rm *|git push --force*))` — defers to phase 3 (`agent` hook type, needs a destructive-op judgement primitive heimdall does not have today).
- `SessionEnd` — `Stop` is more reliable; `SessionEnd` does not fire on crash. Picked once, never revisit without new evidence.
- `prompt`-type hook for `UserPromptSubmit` — rejected on latency grounds (§5).
- Long-running heimdall daemon — accept cold-start for v1, design for a daemon upgrade path in v2 behind `HEIMDALL_DAEMON_SOCKET`.
- Real `claude`-binary end-to-end tests as CI gate — layer-3 harness ships as opt-in (`-tags e2e`, `HEIMDALL_E2E_CLAUDE=1`) only.

## 3. Chosen Design

All four phase-1 hooks are `command`-type: Claude Code writes event JSON to stdin, heimdall writes markdown (or nothing) to stdout, exit code is `0` always. Stderr is silenced; real errors go to the log file.

### 3.1 `SessionStart` (warm path)

- **Event / matcher:** `SessionStart`, no matcher.
- **Command:** `heimdall-mcp hook session-start`.
- **Stdin fields used:** `cwd`. Unknown fields ignored. Empty stdin → fall back to process CWD.
- **Flow:** compose `status --format=json` + `recall --query "<cwd-basename> project context" --limit 5 --format=hook-md` into the fixed shape.
- **Stdout:** single `## Heimdall context` block — project name + chunk count + last-indexed + model name, then up to 5 memory bullets, then a one-line footer. ≤1.5 K tokens. Empty state emits a one-line "no index yet, run `heimdall-mcp index .`" note. Degraded (Ollama down) emits `> heimdall: unavailable (<short reason>)`.
- **Latency budget:** 2 s p95, 3 s hard internal, 5 s harness kill.
- **Failure behavior:** `VerifyHookIndex` runs pre-embed; mismatch / missing index → Tier B note only, no retrieval. Ollama down → A.1 line from plan 04. Exit 0 always.

### 3.2 `UserPromptSubmit` (hot path)

- **Event / matcher:** `UserPromptSubmit`, no matcher.
- **Command:** `heimdall-mcp hook user-prompt` (name reconciled — see §4.1).
- **Stdin fields used:** `prompt`. Never read from argv.
- **Skip heuristic (runs before any cache/embed):** `len(trim(prompt)) < 8` OR starts with `/` OR matches `^[\s\p{P}\p{S}]+$` → exit 0 empty. The 8-char floor is the cheap version; the `hooks.user_prompt.min_prompt_chars` config knob (default 12) can tighten it.
- **Cache path:** `VerifyHookIndex` → length guard → compose cache key → `SELECT stdout FROM hook_cache WHERE key = ? AND created_at > ?`. Hit: bump `hit_count`, refresh `last_accessed` on referenced chunks, write stdout, exit. Miss: embed → `SearchFiltered` → format → `INSERT OR REPLACE` into `hook_cache` → write stdout.
- **Stdout:** `## Heimdall suggests` header, per-hit `### file:Lstart-Lend (score 0.NN)` + fenced snippet truncated to ~400 chars, trailing "retrieved via heimdall" footer. Zero hits → empty stdout (never inject a "no results" note). Budget exceeded → empty stdout.
- **Latency budget:** **≤250 ms p95 uncached, ≤50 ms p50 cached, 500 ms hard** (reconciled — see §4.2). Enforced internally via `context.WithTimeout`; harness timeout is a fallback.
- **Failure behavior:** Ollama down / model missing / index missing → Tier B note on first occurrence, then 5-min suppressed to Tier A silent. Model mismatch at the hook boundary → Tier B note, no retrieval, **no fuzzy fallback via `ResolveUsableModelDB`** (see §5.1). Embed timeout → empty stdout, deterministic. Exit 0 always.

### 3.3 `PostToolUse(Edit|Write)` (fire-and-forget)

- **Event / matcher:** `PostToolUse`, matcher `Edit|Write`.
- **Command:** `heimdall-mcp hook post-edit`.
- **Stdin fields used:** `tool_name`, `tool_input.file_path`.
- **Flow:** append path to `<project>/.heimdall_db/hooks/reindex.pending`; non-blocking `flock` on `reindex.lock`. If locked → exit 0 (coalesced). If acquired and `now - reindex.last_run < coalesce_window_ms` (default 2 000 ms) → release, exit 0. Else `fork+setsid` a detached actor that sleeps the coalesce window, drains pending, runs the incremental reindex, bumps `store_metadata.index_version` on successful commit, drains the deadletter, updates `reindex.last_run`, exits.
- **Stdout:** empty.
- **Latency budget:** foreground ≤50 ms, no wall budget on the background actor.
- **Failure behavior:** all Tier A (silent, exit 0). Fork failure → log, exit 0. Actor failures → deadletter JSONL at `.heimdall_db/hooks/reindex.deadletter.jsonl` with `err_code`, retried on next successful actor run (hard drop at N=10).

### 3.4 `Stop` (fire-and-forget, session learning)

- **Event / matcher:** `Stop`, no matcher.
- **Command:** `heimdall-mcp hook stop`.
- **Stdin fields used:** defensive fallback chain `turn_transcript → transcript → message → whole event JSON`. Exact shape is open question §1 in plan 01 (**still open**).
- **Flow:** length-prefixed JSON append to `${XDG_STATE_HOME}/heimdall/sessions/<session_id>.log` via a single `O_APPEND` write. If turn count ≥ `flush_every` (default 5) or file size ≥ `flush_kb` (default 64) → atomic rename to `<session_id>.log.ingesting` and fork `heimdall-mcp ingest-session --buffer <path>`; new Stop hooks write to a fresh buffer. Opportunistic cleanup on every Stop pass: ingest buffers older than 24 h, delete older than 7 d.
- **Stdout:** empty.
- **Latency budget:** foreground ≤200 ms, background actor owns its own context.
- **Failure behavior:** all Tier A. Partial writes detected by length-prefix mismatch in the reader; `.log.corrupt.<ts>` rename on ingest failure so data is not lost.

## 4. Reconciled Inconsistencies

### 4.1 Subcommand naming — chosen: `user-prompt`

Plan 01 uses `hook user-prompt`; plan 02 uses `hook prompt-submit`. Plan 05 noted both are currently tested.

**Chosen: `user-prompt`.** Plan 01 is the ground truth for the contract (plan 05 §Coordination Notes: "Plan 01 … is the ground truth for unit asserts and golden-file templates"). `user-prompt` also mirrors the Claude Code event name lowercased-and-hyphenated (`UserPromptSubmit` → `user-prompt`) consistently with `session-start`, `post-edit`, `stop`. Plan 02 is silently amended in every command reference below.

### 4.2 Latency target — chosen: **250 ms p95 uncached, 50 ms p50 cached, 500 ms hard**

Plan 01 originally proposed "300 ms p50". Plan 03 is backed by real measurements (`payments-analyzer` index, 10 544 chunks, nomic warm at 180 ms end-to-end, cold at 380 ms) and proposes "250 ms p95 uncached, 50 ms p50 cached, 500 ms hard". Plan 05 already anchors benchmarks to 250 ms p95.

**Chosen: plan 03's number.** Evidence beats gut estimates. First-turn-after-idle may still spike toward ~400 ms — accepted for v1, documented in the user-facing README. Cold-Ollama path is explicitly excluded from the golden-path budget; it is covered as a Tier B degradation that only needs to meet the 500 ms hard timeout.

### 4.3 CLI prerequisites — `recall` and `ingest-session` become phase 1a blockers

Plan 02 v2 promotes `recall` and `ingest-session` to top-level CLI commands. Plan 01 assumed they already existed. They do not — today both live only in the MCP server (`internal/mcp/memory_tools.go:137` and `:213`). `hook session-start` cannot ship without `recall`; `hook stop` cannot ship without `ingest-session`.

**Chosen: mark them as phase 1a prerequisite tasks.** They are the first two items in the implementation plan (§6, tasks T1 and T2), blocking every hook-command task. They are straight wrappers over existing `heimdall.Memory*` and `heimdall.IngestSession` functions — effort is shape-matching plus flag parsing, not new logic.

### 4.4 Install-marker key naming — chosen: **`"source": "heimdall"`** (+ secondary `"version"`)

Plan 04 uses `"source": "heimdall"`. Plan 05 uses `"x-heimdall": {...}` object wrapper. Plan 02 references the marker only abstractly but defers to 04.

**Chosen: `"source": "heimdall"` as the canonical primary key, with a sibling `"version": N` integer field.** Reasons: (a) plan 04 is the definitive failure-mode / install-time source-of-truth; (b) flat fields are cheaper to inspect in shell tools; (c) the `x-heimdall` wrapper from 05 is preserved as an *optional* secondary nested object for richer install metadata (`heimdall_version`, `installed_at`, `id`) because it is additive and useful for `hooks doctor`. Plan 01 open question §8 must be resolved against Claude Code docs first — if Claude Code *rejects* unknown fields on hook entries, the marker moves into the command string itself (`heimdall-mcp hook user-prompt --source=heimdall --version=1`). That fallback is mandatory regardless because the command string is opaque to Claude Code and is therefore guaranteed-safe.

## 5. Design Decisions & Rationale

### 5.1 Model-mismatch check reinstated at the hook boundary

A `heimdall.VerifyHookIndex(store, requestedModel) error` helper runs in every retrieval hook immediately after `OpenStore`, before any embed call. On mismatch, it emits the Tier B note on stdout *once* (first occurrence per project+failure_code, 5-min suppression thereafter collapses to Tier A silent) and skips retrieval entirely.

**Why not reuse `ResolveUsableModelDB`?** That helper silently fuzzy-picks any index whose backing model is pulled in Ollama. If the configured model is `bge-m3` (not pulled) and the user has `nomic` and `mxbai` indexes, it will pick one and search against it — cosine across mismatched embedding spaces returns plausible-looking noise, which is strictly worse than no retrieval. Fuzzy-match is appropriate for the interactive MCP path but a correctness failure on the hook path. Unit tests enforce three distinct sentinel errors (mismatch, missing metadata, dim mismatch).

### 5.2 `prompt`-type hook rejected

Plan 01 floated a `prompt`-type `UserPromptSubmit` hook (ask a cheap local model "should we retrieve?") as a phase-2 upgrade. Plan 03 rejected it on measured grounds: a cheap-LLM gate is 100–500 ms on the same Ollama instance, strictly worse than the cache + length-guard approach. It adds to the worst-case path and saves nothing on cache hits. **Decision: dead.** Not revisited in v1 or v2 unless evidence changes.

### 5.3 New `hook_cache` SQLite table

Real schema change. Added to the existing per-model store DB, so every cache row is implicitly scoped to one embedding space by file location (swapping models means opening a different DB, which means a different cache — zero cross-model contamination).

```sql
CREATE TABLE IF NOT EXISTS hook_cache (
  key        TEXT PRIMARY KEY,
  stdout     BLOB NOT NULL,
  created_at INTEGER NOT NULL,
  hit_count  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_hook_cache_created ON hook_cache(created_at);
```

Key: `sha256(normalize(prompt) || "\x00" || index_version || "\x00" || sub_project || "\x00" || source_type || "\x00" || canonical_json(metadata_filter))[:16]`. `index_version` is a new `store_metadata` integer key bumped once per successful `Upsert`/`RemoveByFile` transaction; the bump auto-invalidates every affected cache row on next read. TTL 24 h, row cap 1 000, oldest-by-creation eviction. Oversize (>32 KB stdout) refuses to cache. Cache-hit path refreshes `last_accessed` on referenced chunks via existing `store.UpdateLastAccessed` — load-bearing so the freshness weight doesn't penalize actively-used chunks that happen to cache-hit.

### 5.4 Hook handlers must be in-process testable (hard constraint)

**Non-negotiable design rule:** every hook command handler takes `io.Reader` (stdin), `io.Writer` (stdout), `io.Writer` (stderr), and an `env` map; returns `int` (exit code). No `os.Exit` inside the handler. `main()` wires `os.Stdin`/`os.Stdout`/`os.Stderr`/`os.Environ` + calls `os.Exit(handler(...))`. Without this, Layer 1 unit tests from plan 05 are impossible and every hook would rot the moment anyone touches it. This applies to the four hook subcommands *and* to `recall`/`ingest-session`/`install-hooks`/`hooks-doctor`.

### 5.5 Debouncer scheme

Filesystem lockfile (`flock`-based) + coalesce-window (2 s default) + coalesced pending-file list + `fork+setsid` detached actor. Four state files under `<project>/.heimdall_db/hooks/`: `reindex.lock`, `reindex.pending`, `reindex.last_run`, `reindex.inflight.pid`. Foreground ops are all non-blocking (<50 ms). Pending file is capped at 1 000 lines (drop oldest on overflow). Stale `inflight.pid` detected via `kill(pid, 0)` on next hook fire. `flock`-unsupported filesystems (NFS, some WSL configs) fall back to `O_CREAT|O_EXCL` lock with TTL stamp.

### 5.6 Cold start and Ollama `keep_alive`

Accepted for v1. Mitigation: every embed request sent by a hook carries `keep_alive: "10m"` — a one-field JSON change, trivially verified against measured warm-embed numbers. Daemon path deferred to v2 behind `HEIMDALL_DAEMON_SOCKET` env var. First-turn-after-idle spike to ~400 ms is documented, not fixed.

### 5.7 Per-project disable marker

Presence of `<project>/.heimdall/hooks.disabled` (empty file) → every hook exits 0 in <1 ms. Chosen over a TOML key because stat-ing a marker file is the cheapest presence check available and survives corrupt TOML. `hooks.toml` `[hooks] enabled = false` works as a secondary path. Global kill switch is `HEIMDALL_HOOKS=0`, checked first.

### 5.8 Logging & observability

`${XDG_STATE_HOME:-~/.local/state}/heimdall/hooks.log`, 5 MB size cap, one rotation generation (`hooks.log.1`), single-line `grep`-friendly format with timestamps + level + event + key=value pairs. `hooks-doctor` and `hooks tail` consume it. Log failures never abort the hook.

### 5.9 Stderr safety

Stderr must never leak file paths or stack traces. Panic handler writes a redacted `ERROR panic %v` line to the log file, not to stderr. Enforced via static check in `docs/reviews/code-review-context.md` (test-rollout's existing review tooling).

## 6. Implementation Plan

Effort: **S** = ~½ day, **M** = 1–2 days, **L** = 3–5 days. Every task inherits the §5.4 testability constraint — handlers take `io.Reader`/`io.Writer`/env/return int.

| ID | Title | Delivers | Source | Effort | Unblocks |
|---|---|---|---|---|---|
| **T1** | **Phase 1a prerequisite:** top-level `heimdall-mcp recall` CLI | Wrapper over `heimdall.Memory*` with `--query`, `--limit`, `--type`, `--tags`, `--project`, `--format={text,json,hook-md}`. Matches MCP `toolRecall`. | 02 §`heimdall-mcp recall` | M | T5, T9, T20 |
| **T2** | **Phase 1a prerequisite:** top-level `heimdall-mcp ingest-session` CLI | Wrapper over `heimdall.IngestSession` with `--summary-stdin` / `--buffer <path>` / `--project` / `--session-id`. Matches MCP `toolIngestSession`. | 02 §`heimdall-mcp ingest-session` | M | T8 |
| T3 | `--format=json` on `heimdall-mcp status` | JSON shape mirrors `toolStatus` response. | 02 §`heimdall-mcp status --format=json` | S | T5 |
| T4 | `--format=hook-md` on `heimdall-mcp search` + `--budget-ms` | Internal wall-clock budget enforced via `context.WithTimeout`; partial results discarded on deadline (deterministic empty). `--limit` flag exposure. | 02 §`heimdall-mcp search --format=hook-md`; 03 §3; 04 §A.3 | M | T6 |
| T5 | `heimdall-mcp hook session-start` | Composes T3 + T1 into the fixed `## Heimdall context` block. `VerifyHookIndex` gate. Tier B degraded-state handling per 04 §A.1/A.5. | 01 §1, 02 §hook session-start, 04 §A.1, A.5 | M | T19 |
| T6 | `heimdall-mcp hook user-prompt` | Skip heuristic → `VerifyHookIndex` → cache lookup → T4 → format. Budget `--budget-ms` (default 250), hard timeout 500 ms. Exit 0 always. | 01 §2, 02 §hook user-prompt, 03 §3–4, 04 §A.2/A.3 | L | T19 |
| T7 | `heimdall-mcp hook post-edit` | Debouncer (§5.5) + fork+setsid actor + deadletter consumer. Foreground <50 ms. Exit 0 always. | 01 §3, 02 §hook post-edit, 04 §A.4, Addendum B | L | T19 |
| T8 | `heimdall-mcp hook stop` | Length-prefixed JSON append to session buffer + flush trigger → T2. Defensive field-name chain. Opportunistic orphan reap. | 01 §4, 02 §hook stop, 04 §A.8 | M | T19 |
| T9 | `VerifyHookIndex` helper in `internal/heimdall` | Three sentinel errors (mismatch, missing metadata, dim mismatch). Pure function over store. Unit-tested. | 04 §3 | S | T5, T6 |
| T10 | `hook_cache` table + `store_metadata.index_version` bump | Migration adds the table + index. `Upsert`/`RemoveByFile` bumps integer in one transaction. | 03 §3 | M | T6 |
| T11 | Ollama embed `keep_alive: "10m"` | One-field JSON addition on hook-path embed calls. | 03 §5 | S | T6 |
| T12 | Tier B suppression store | 5-min-per-(project, failure_code) suppression keyed in `~/.local/state/heimdall/suppress.db`. | 04 §2 | S | T5, T6 |
| T13 | Hook log file + rotation | `${XDG_STATE_HOME}/heimdall/hooks.log`, 5 MB cap, one rotation, atomic rename. Safe-on-failure. | 04 §6 | S | all hooks |
| T14 | `heimdall-mcp install-hooks` | Atomic merge into `~/.claude/settings.json` or `.claude/settings.json` (scope). Primary marker `"source": "heimdall"` + secondary `x-heimdall` nested object. Conflict-report-and-exit default; `--merge`, `--force`, `--dry-run`, `--only`, `--disable`, `--scope`. | 02 §install-hooks, 04 §4–5, 05 §Install UX | L | Phase 1a |
| T15 | `heimdall-mcp uninstall-hooks` | Symmetric to T14. Idempotent. Removes empty `hooks` key entirely. | 02 §uninstall-hooks, 05 §uninstall | S | Phase 1a |
| T16 | `heimdall-mcp hooks doctor` | 11-check pass/fail table (synthesis of 04 §6 and 05 §doctor). Dry-fires each installed hook with canned event JSON. Exit 0 OK/WARN, 1 on any FAIL. | 02, 04 §6, 05 §doctor | M | Phase 1a gate |
| T17 | `heimdall-mcp hooks tail` | `tail -F` over the log file with `--level`, `--event`, `--since`, `--project` filters. One-shot. | 04 §6 | S | — |
| T18 | `heimdall-mcp hooks cache-clear` / `cache-stats` | Cache admin subcommands. | 03 §3 | S | — |
| T19 | Layer 1 unit tests (per-hook) | ≥3 tests per hook command — happy, degraded (every Tier from 04 §1), edge. Golden files for templates; structural asserts for semantics. `httptest` fake Ollama, `t.TempDir()`. Requires T5–T8 + §5.4. | 05 §Layer 1 | L | Phase 1a gate |
| T20 | Layer 2 integration tests | `os/exec` the built binary, fake Ollama via `httptest`, seeded SQLite. One test per bullet in 01 §`test-rollout` + cache hit, cache invalidation, model mismatch. | 05 §Layer 2 | L | Phase 1a gate |
| T21 | `BenchmarkUserPromptHook` regression test | Baseline in `testdata/perf/hook_user_prompt.baseline`; 50 % regression threshold (non-gating initially). 250 ms p95 uncached + 50 ms p50 cached golden checks. | 05 §Performance regression, 03 §2 | S | Phase 1b gate |
| T22 | Per-project toggle + global kill switch | `.heimdall/hooks.disabled` marker check + `HEIMDALL_HOOKS=0` env — both <1 ms exit path. Config file fallback via `hooks.toml`. | 02 §Config file design, 04 #15, 05 §Per-Project Toggles | S | Phase 1a gate |
| T23 | Layer 3 opt-in e2e harness | `test/e2e/hooks_claude_test.go` with `-tags e2e` + `HEIMDALL_E2E_CLAUDE=1`. `claude --settings … --include-hook-events` stream-json parse. Skips cleanly without `claude` on PATH. | 05 §Layer 3 | M | — (not a gate) |

**Dependency summary:**
- T1, T2, T3 unblock everything else in phase 1a.
- T4, T9, T10, T11 unblock T6.
- T5, T6, T7, T8 all require T13 (log file).
- T14, T15, T16 all need at least one hook command to exist end-to-end (T5 is the cheapest smoke target).
- T19, T20 are the Phase 1a gate.
- T21 is the Phase 1b gate.

## 7. Phased Rollout

Phases inherit plan 05's structure, updated for reconciled hook events and budgets.

| Phase | Hooks included | Gate criteria | Entry condition |
|---|---|---|---|
| **1a — foundation** | `SessionStart`, `PostToolUse(Edit\|Write)` | T1–T5, T7, T9–T17, T19, T20, T22 landed. T19+T20 green. `hooks doctor` green. Install/uninstall round-trip verified on 3 real `settings.json` samples (empty, existing non-heimdall hooks, existing heimdall hooks from older version). Dogfooded in one repo ≥3 days with zero complaints. | Plan approved; §4 reconciliations accepted. |
| **1b — hot path** | `UserPromptSubmit` | 1a shipped and stable ≥1 week. T6 landed. T21 benchmark green (p95 ≤ 250 ms uncached, p50 ≤ 50 ms cached, hard ≤ 500 ms) on a mid-size repo. Model-mismatch Tier B path exercised end-to-end. Cache-invalidation path verified against a concurrent edit. | 1a gate passed. |
| **2 — session learning** | `Stop` → rolling buffer → `ingest-session` | 1b stable ≥1 week. `Stop` event payload shape confirmed from Claude Code docs (plan 01 OQ §1). Session buffer retention policy confirmed. `ingest-session` failure rate in production <1 %. | 1b gate passed. |
| **3 — guardrails** | `PreToolUse(Bash(rm *\|git push --force*))` as `agent` hook | Phase 2 stable. `heimdall_recall` has enough seeded data to produce useful destructive-op answers. `agent` hook type confirmed available in current Claude Code. | Phase 2 gate passed; separate spec approved. |

**Why this order?** Plan 05's reasoning stands: trivial-contract hooks first to prove the install/test/toggle machinery, hot path isolated into its own phase so we can revert it cleanly if latency regresses, `Stop` held until its payload is confirmed and its write-path has soak time, guardrails last because they are the only hooks that can block user actions and a bad guardrail is strictly worse than no guardrail.

## 8. Open Questions for Human Decision

De-duplicated across all five plans. Not answered here — for human decision.

### Blocks phase 1a

1. **Claude Code unknown-field tolerance on hook entries.** Does Claude Code accept `"source": "heimdall"` + `"version": N` on a hook entry, or does it reject unknown fields? Gates T14 (install-hooks) and §4.4. Fallback (encode marker in command string) is mandatory regardless. *(01 OQ §8, 04 OQ §4, 05 §4.4)*
2. **Opt-in vs opt-out install.** Should `install-hooks` be an explicit user command (opt-in), or run automatically on first `heimdall-mcp index`? Rollout-sensitive — product call. *(01 OQ §6, 02 OQ §6, 05 §Install UX)*
3. **Binary name.** `heimdall-mcp` vs short `heimdall` alias. Hooks fire often; shorter is better in `settings.json`. Gates T14 because the baked-in manifest needs the canonical name. *(02 OQ §1)*
4. **JSON round-trip fidelity for `settings.json` writes.** Accept `encoding/json` reformat + always back up (current default), or vendor `tailscale/hujson`? Affects user-file friendliness. *(05 OQ §2)*
5. **Exit-code taxonomy vs always-0.** `cli-surface` proposed a 10/20/30 degraded/config/dependency taxonomy; `failure-modes` argues all phase-1 hooks exit 0 always and route codes into the log. Resolve before T19. *(02, 04 Open Questions Added)*

### Blocks phase 1b

6. **`UserPromptSubmit` degraded-state note.** Silent (cli-surface's default) or the same `> heimdall: unavailable` header as `SessionStart` (architect's position)? Every turn's token budget is tight. *(02 OQ §5, 01 §6)*
7. **Dim mismatch — block retrieval or degrade silently?** Proposed: block (return only the warning). Alternative: proceed with noisy results. *(04 OQ §1)*
8. **Hook queuing semantics.** If a `UserPromptSubmit` hook is still running when the user types the next prompt, does Claude Code (a) wait, (b) kill the previous, (c) fire in parallel? Determines whether per-session serialization is needed. Empirical test via T23 if docs are silent. *(01 OQ §9, 03 OQ §1)*
9. **Perf regression threshold.** 50 % is placeholder. Confirm with measured variance in T21 baseline. *(05 OQ §4)*

### Blocks phase 2

10. **Exact `Stop` event payload shape.** The defensive fallback chain in §3.4 ships blind. Need Claude Code docs confirmation or empirical sample. *(01 OQ §1, 05 §Rollout Phases)*
11. **Rolling buffer location & retention policy.** `${XDG_STATE_HOME}/heimdall/sessions/<session_id>.log` is the current proposal. Retention: 24 h opportunistic ingest, 7 d delete. Confirm. *(01 OQ §4, 04 §A.8)*
12. **Transcript cap per buffer.** 2 MB truncate-head proposed. *(04 OQ §5)*

### Later / nice-to-know

13. **Tier B rate-limit window.** 5 min per `(project, failure_code)` proposed. *(04 OQ §2)*
14. **Verbose footer default.** Off by default with opt-in `hooks.verbose = true` proposed. *(04 OQ §3)*
15. **"Stale" window for hooks.** Current code uses 30 min; hooks may want tighter. *(04 OQ §7)*
16. **Cross-process SQLite write lock.** Hook-side reindex and running MCP server both writing to the same store — WAL handles it but slows writes. Is a filesystem lock worth adding? *(04 Open Questions Added)*
17. **Ollama `keep_alive` ceiling.** The field exists but its effective ceiling under memory pressure is undocumented. Long-bench probe in T23 can measure it. *(03 OQ §2)*
18. **Auto-run `hooks doctor` at end of install.** Means a successful install can exit non-zero if Ollama is down. Team-lead call. *(05 OQ §5)*
19. **100 k+ index measurements.** No real-world index that size yet; extrapolated 450 ms number may motivate ANN (HNSW/IVF) later. *(03 OQ §3)*

## 9. Risks

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Hot-path latency regresses and makes `claude` feel broken | Medium | High | T21 benchmark gate + 1a → 1b phase split so `user-prompt` can be yanked independently; 500 ms hard timeout; `HEIMDALL_HOOKS=0` kill switch. |
| Model-mismatch produces plausible-looking noise | Medium | High | `VerifyHookIndex` at the hook boundary refuses fuzzy-match; dim-check as second gate; Tier B user-visible note; unit tests for each sentinel. |
| Install clobbers other tools' hooks in `settings.json` | Medium | High | JSON merge (not regex), `"source": "heimdall"` marker, conflict report + refuse default, atomic write with backup, uninstall-is-symmetric contract, round-trip tests on 3 real samples. |
| Background reindex races MCP server's in-process reindex → DB corruption | Low | High | A.6 hook-side path skips `checkAndTriggerReindex`; SQLite WAL + busy_timeout handles the write-write case; cross-process lock is OQ §16. |
| Cache stampede after model swap causes latency spike | Low | Medium | Cache lives *inside* the per-model DB file, so a model swap opens a different cache — cascade is isolated and expected. Keep-alive + length guard + 1 s coalesce on post-edit keep the miss path cheap. |
| Session buffer fills disk | Low | Medium | 2 MB per-buffer cap, truncate-head; 7-day retention sweep; `${XDG_STATE_HOME}` location outside project tree. |
| Log file leaks sensitive paths / stack traces | Medium | Medium | Stderr redacted (§5.9), panic handler logs to file only, static check in `docs/reviews/code-review-context.md`. |
| Claude Code rejects unknown `source`/`version` fields on hook entries | Low | Medium | Fallback marker in command string (`--source=heimdall --version=1`) is mandatory regardless; OQ §1 verifies. |
| Layer 3 e2e harness is infeasible because hook-event stream-json schema is undocumented | Medium | Low | Layers 1+2 already cover every phase-1 assertion; Layer 3 gated behind `-tags e2e` + env var, never CI gate; downgrade to manual runbook if needed. *(05 OQ §1)* |
| `flock` unsupported on user's filesystem (NFS, WSL) | Low | Low | `O_CREAT|O_EXCL` TTL-stamp fallback in the debouncer. |

## 10. Success Definition

The plan worked **if the following single metric crosses its threshold within two weeks of phase 1b GA**:

> **≥95 % of `UserPromptSubmit` events in dogfood sessions inject a non-empty `## Heimdall suggests` block within the 250 ms p95 budget, measured over a 7-day rolling window across ≥3 distinct indexed projects.**

This metric is the concrete operationalization of "Claude is actually using heimdall on every turn." Subsidiary indicators:
- Fewer than 5 % of turns hit a Tier A silent skip (timeout, Ollama blip, DB contention).
- Fewer than 1 % of turns hit a Tier B visible note (index missing, model mismatch).
- Zero crashes or exit-non-zero events from any hook command in `hooks.log` aggregation.
- Cache hit rate ≥40 % after the first turn of a session (evidence that the repeat-working-set assumption holds).

Measurement surface: `heimdall hooks tail --since=7d | awk`-friendly format already gives us everything — no new telemetry infrastructure required. Add one weekly command to the project runbook that computes the numbers and publishes them.

## 11. Source Plans

- `/home/noname/Code/heimdall-mcp/docs/plans/hooks/01-architecture.md`
- `/home/noname/Code/heimdall-mcp/docs/plans/hooks/02-cli-surface.md`
- `/home/noname/Code/heimdall-mcp/docs/plans/hooks/03-latency.md`
- `/home/noname/Code/heimdall-mcp/docs/plans/hooks/04-failure-modes.md`
- `/home/noname/Code/heimdall-mcp/docs/plans/hooks/05-testing-rollout.md`
