# heimdall-mcp — TODO

Tracking all outstanding work across the "steal ideas from OpenViking" roadmap.
Tackled progressively — see status per item.

## 1. Claude Code Hooks Integration (**WAVE 2 PHASE 1b LANDING** — hot-path UserPromptSubmit)

**Goal:** make Claude actually use heimdall on every turn via Claude Code hooks,
not via hopeful tool exposure. Highest-leverage item by a wide margin.

**Wave 1 shipped** (merges `cdf890b`, `4a3c643`, `55ab7c3`, `8189180`):
- [x] T1 `heimdall-mcp recall` CLI + shared-core extraction
- [x] T2 `heimdall-mcp ingest-session` CLI + length-prefixed buffer parser
- [x] T3 `status --format=json`
- [x] T9 `VerifyHookIndex` + three sentinel errors, refuses fuzzy fallback
- [x] T10 `hook_cache` SQLite table + `store_metadata.index_version` bump + two-phase RLock/Lock for `HookCacheGet`
- [x] T11 Ollama `keep_alive: "10m"` on hook path only
- [x] T12 Tier B suppression store
- [x] T13 `hooks.log` file, rotation, redaction (POSIX + Windows)
- [x] T17 `heimdall-mcp hooks tail` with filter flags
- [x] T18 `heimdall-mcp hooks cache-clear` / `cache-stats` wired to real store methods
- [x] T22 `HooksDisabled` fast-path (env + marker file, <1µs warm)
- [x] Review gate: weighted 94.5/100, merged by judgment at structural ceiling

**Wave 2 phase 1a shipped** (merges `c7f7f6c`, `12cc751`, `fbcb4dc`):
- [x] T5 `heimdall-mcp hook session-start` — retrieval, VerifyHookIndex gate, Tier B suppression, EmbedForHook keep_alive, budget-ms context timeout, golden-file tested
- [x] T7 `heimdall-mcp hook post-edit` — flock debouncer, 2-s coalesce window, fork+setsid detached actor, deadletter JSONL with retry + N=10 hard-drop
- [x] T14 `heimdall-mcp install-hooks` — settings.json merge with atomic write, backup, `--dry-run`/`--merge`/`--force`/`--only`/`--scope`, dual marker detection (`"source": "heimdall"` + `--source=heimdall` command-string fallback)
- [x] T15 `heimdall-mcp uninstall-hooks` — symmetric, idempotent
- [x] T16 `heimdall-mcp hooks doctor` — 11-check pass/fail table, dry-fires each installed hook, skips internal `post-edit-actor`
- [x] OQ-1..OQ-5 locked in `docs/plans/hooks/06-decisions.md`
- [x] Review gate: weighted 97.8/100 (Quality 98, Security 99, Performance 97, Tests 96, Design 98). 110 tests in internal/cli, all green under `-race`.

**Dogfood ✅ 2026-04-16 (PR #9):**
- [x] `heimdall-mcp index .` — 1758 chunks, `nomic-embed-text`
- [x] `install-hooks --scope=project`
- [x] `hooks doctor` — 11/11 green, all three hooks dry-fire OK
- [x] Reopen Claude Code → `SessionStart` fires, `## Heimdall context` block in first turn
- [x] `Edit` tool call → `PostToolUse` → `fork+setsid` actor reached `files=1 msg=reindex_ok` (no deadletter, ~67 s first embed, Ollama cold)

**Wave 2 phase 1b ✅ landing (this PR):**
- [x] T4 `--format=hook-md` on `search` + `--budget-ms` — breaking `SearchFiltered` signature (ctx first arg), 30+ call sites updated, real ctx cancellation in the row loop
- [x] T4 side-effect: killed the `TestSearchFiltered_BudgetTimeout` skip stub, replaced with real pre-cancelled-ctx assertions + nil-ctx tolerance
- [x] T6 `hook user-prompt` command — 250 ms default budget, skip heuristic (`len < 8`), cache lookup keyed on `(normalized_prompt, index_version, project)`, Tier B suppression on Ollama down / model mismatch, cache store on miss
- [x] `install-hooks` template now installs `UserPromptSubmit` alongside `SessionStart` + `PostToolUse` (3 hooks total)
- [x] Unit tests: 16 new cases in `hook_user_prompt_test.go` — skip, disabled, Ollama down (Tier B + suppressed), no index, model mismatch, happy path caches result, cache hit short-circuits, cache invalidation on index_version bump, budget timeout, normalization, empty stdin → CWD fallback, `--prompt` flag override, malformed JSON, dispatcher routing
- [x] Version constants bumped to `wave2-phase1b` (doctor `--version` check + install envelope)
- [~] **T21 benchmark skipped for now** — original plan called for a microbenchmark; replaced with a same-task with-vs-without-hooks comparison after merge (per user direction)
- [~] **Soak gate killed** — wall-clock soak tests nothing for a solo local tool; phase 1b rides on the unit test suite + dogfood instead

**Wave 2 phase 2 (pending, session learning):**
- [ ] T8 `hook stop` command (rolling buffer → ingest-session handoff)
- [ ] Stop-event payload shape confirmed against real Claude Code
- [ ] Session buffer retention policy confirmed

**Wave 2 phase 3 (pending, guardrails):**
- [ ] `PreToolUse(Bash(rm *|git push --force*))` as `agent`-type hook — needs a destructive-op judgment primitive heimdall does not have today

**Carried over / not yet in scope:**
- [ ] T20 Layer 2 integration tests (`os/exec` + fake Ollama) — deferred, unit tests cover 110 cases; Layer 2 is for future hardening
- [ ] T23 Layer 3 opt-in e2e harness (`-tags e2e`, `HEIMDALL_E2E_CLAUDE=1`)
- [ ] T24 Windows path redaction (already have the regex in Wave 1; confirm it holds when anyone actually runs on Windows)
- [ ] Deferred perf optimizations: PERF-002 `bumpIndexVersionTx` single-query, PERF-003 O(cap) eviction → incremental row-count tracking
- [ ] `heimdall-mcp index` first-run hint ("Tip: run `install-hooks` to have Claude Code call heimdall automatically") — OQ-2 follow-up, not blocking anything

## 2. Tiered retrieval (L0/L1/L2) — **PENDING**

**Goal:** return one-line summaries first, expand to snippet or full chunk on demand. Biggest token/quality win once hooks are live.

- [ ] Schema: add `summary TEXT` to `entries`; migration.
- [ ] Index-time summary generation (heuristic first, optional LLM upgrade).
- [ ] `heimdall_search` gains `detail: summary|snippet|full` param, defaults to `summary`.
- [ ] New `heimdall_expand(chunk_id)` MCP tool.
- [ ] Integration into the `UserPromptSubmit` hook format (summaries in stdout, IDs for expansion).
- [ ] Token-savings measurement before/after.

## 3. Path-based context hierarchy — **PENDING**

**Goal:** replace flat `sub_project` with a real tree, so retrieval can scope by path prefix and walk the hierarchy.

- [ ] Schema: `context_path TEXT` replaces/augments `sub_project`; migration from existing rows.
- [ ] `heimdall_search` gains `scope` path param.
- [ ] New `heimdall_ls(path)` MCP tool — filesystem-style navigation.
- [ ] All three content kinds (code, memory, skill) live under path prefixes in one table.
- [ ] Auto-detect path for memories (e.g. `memories/decisions/<date>-<slug>`).
- [ ] Update hook injection to respect scope when CWD is a subpath.

## 4. Skills as indexable content — **PENDING**

**Goal:** store reusable procedures as first-class, semantically retrievable content.

- [ ] Decision: how is this different from Claude Code's built-in `~/.claude/skills/`? (design question — decide before coding).
- [ ] New `source_type = "skill"` in the store.
- [ ] `heimdall_remember --type=skill` with required fields (name, when, steps).
- [ ] Auto-surface via `SessionStart` (top-N skills for repo) and `UserPromptSubmit` (situationally relevant).
- [ ] Optional: two-way sync with `~/.claude/skills/` directory.

## 5. Misc / carried over

- [ ] LOW-severity findings from the 2026-04-14 code review that weren't in the top-6
      fix batch (SEC-001 SQL-builder audit comment, SEC-002 subProject length cap,
      SEC-003 sanitized error strings, DES-006 config-driven EmbedBatchSize,
      DES-010 ETA unit test, TEST-013 mock rename).
