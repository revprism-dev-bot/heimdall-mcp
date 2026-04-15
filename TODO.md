# heimdall-mcp — TODO

Tracking all outstanding work across the "steal ideas from OpenViking" roadmap.
Tackled progressively — see status per item.

## 1. Claude Code Hooks Integration (**IN PLANNING**)

**Goal:** make Claude actually use heimdall on every turn via Claude Code hooks,
not via hopeful tool exposure. Highest-leverage item by a wide margin.

Scope for this pass: **planning only**. 5 planners in parallel, then consolidation.
Implementation blocked on approved plan.

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
