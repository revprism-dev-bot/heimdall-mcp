# heimdall-mcp — TODO

Tracking all outstanding work across the "steal ideas from OpenViking" roadmap.
Tackled progressively — see status per item.

## 1. Claude Code Hooks Integration (**WAVE 1 MERGED** — Wave 2 next)

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

**Wave 2 (pending) — hook commands + install/test/gate:**
- [ ] T4 `--format=hook-md` on `search` + `--budget-ms` (breaking: adds ctx to `SearchFiltered`)
- [ ] T5 `hook session-start` command
- [ ] T6 `hook user-prompt` command (hot path; 250ms p95 budget)
- [ ] T7 `hook post-edit` command (fork+setsid debouncer)
- [ ] T8 `hook stop` command (session buffer → ingest-session handoff)
- [ ] T14 `install-hooks` (blocked on 5 OQs: unknown-field tolerance, opt-in/opt-out, binary name, JSON round-trip, exit-code taxonomy)
- [ ] T15 `uninstall-hooks`
- [ ] T16 `hooks doctor`
- [ ] T19 Layer 1 unit tests for every hook command
- [ ] T20 Layer 2 integration tests (`os/exec` + fake Ollama)
- [ ] T21 `BenchmarkUserPromptHook` regression baseline
- [ ] T23 Layer 3 opt-in e2e harness (`-tags e2e`, `HEIMDALL_E2E_CLAUDE=1`)
- [ ] T24 Windows path redaction (deferred until Windows adoption — security review flagged)
- [ ] Deferred perf optimizations: PERF-002 `bumpIndexVersionTx` single-query, PERF-003 O(cap) eviction → incremental row-count tracking
- [ ] Wave 2 test gaps: `hook-md` golden files, `SearchFiltered` budget-timeout live test (once ctx param lands)

**Open questions blocking phase 1a of Wave 2** (from consolidated plan §8):
1. Claude Code unknown-field tolerance on hook entries
2. Opt-in vs opt-out install
3. Binary name (`heimdall-mcp` vs `heimdall`)
4. `settings.json` JSON round-trip fidelity
5. Exit-code taxonomy vs always-0 for retrieval hooks

Hook surface to design:

- [ ] `SessionStart` → inject project memories + recent context (via `heimdall recall`).
- [ ] `UserPromptSubmit` → semantic search of the prompt, inject top hits (via `heimdall search`).
- [ ] `PostToolUse(Edit|Write)` → incremental re-index in the background.
- [ ] `PostToolUse(Bash(git commit *))` → re-index git commits.
- [ ] `Stop` / `SessionEnd` → auto-ingest session transcript into long-term memory.
- [ ] `PreToolUse(Bash(rm *|git push *))` → agent-type hook that queries prior memories about destructive ops for this repo.
- [ ] CLI flags/subcommands on the `heimdall` binary so hooks can shell out cleanly (stdout formats, exit codes, streaming).
- [ ] Failure-mode handling: Ollama down, index missing, embedding timeout, stale index, first-run in a fresh repo.
- [ ] Latency budget: `UserPromptSubmit` fires on every turn — measure + cache + skip heuristic.
- [ ] Testability: end-to-end harness that spins up Claude Code with test hooks and verifies they fire.
- [ ] User toggles: per-project on/off, per-hook on/off, quiet mode.
- [ ] Install/setup UX: a `heimdall install-hooks` command that wires `~/.claude/settings.json` without clobbering existing hooks.

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
