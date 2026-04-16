# 07 — Next Session Handoff

**Purpose:** everything needed to resume heimdall-mcp work in a fresh Claude Code
session without re-reading the prior-session conversation. Paste the
"Opening prompt for the next session" below into the new session, or just
point at this file.

---

## Opening prompt for the next session

> **Wave 2 phase 2** (session learning) plus TODO sections 2–5 (tiered
> retrieval, path hierarchy, skills, LOW fixes, perf) are all merged to
> `origin/main` via PR #11 (merge `b05af92`). Phase 1a shipped in PR #5,
> phase 1b in PR #10. The full roadmap, locked decisions, and review
> history are in `docs/plans/hooks/`.
>
> **Heimdall now installs 5 hooks**: `SessionStart`, `PostToolUse(Edit|Write)`,
> `UserPromptSubmit`, `Stop`, and `SessionEnd`. The Stop event payload
> shape was confirmed from Claude Code docs on 2026-04-16: `session_id`,
> `transcript_path`, `last_assistant_message`, `cwd`, `stop_hook_active`.
> SessionEnd provides `reason` (clear/resume/logout/etc.). Stop appends
> assistant messages to a rolling JSONL buffer per session; SessionEnd
> triggers `ingest-session` from the transcript path and cleans up.
>
> **New MCP tools shipped**: `heimdall_expand` (chunk drill-down for tiered
> retrieval), `heimdall_ls` (path hierarchy navigation). `heimdall_search`
> gained `detail=summary|snippet|full`, `scope=` (path prefix filter).
> Schema has `summary` and `context_path` columns, both auto-generated at
> index time. Skills are a new memory type (`type=skill`).
>
> **All 6 LOW-severity findings fixed**, PERF-002/003 implemented, first-run
> hint after `index`, T24 Windows path redaction verified, `MockEmbedder`
> renamed to `StubEmbedder`, `EmbedBatchSize` now config-driven, ETA
> computation extracted and tested.
>
> **What's next**: Phase 3 guardrails (`PreToolUse` destructive-op hook),
> live dogfood of Stop/SessionEnd hooks, with-vs-without comparison,
> and the follow-up items listed in TODO.md. Phase 3 is blocked on
> designing a destructive-op judgment primitive.
>
> Before touching code, read this file + `TODO.md` for the full picture.

---

## Ground truth — where everything lives

| Artifact | Path |
|---|---|
| Main roadmap | `TODO.md` |
| Global consolidated plan | `docs/plans/hooks/00-consolidated-plan.md` |
| Per-stream plans (informational) | `docs/plans/hooks/01-architecture.md` … `05-testing-rollout.md` |
| **Locked decisions (OQ-1..OQ-5)** | `docs/plans/hooks/06-decisions.md` |
| Wave 1 dimension reviews | `docs/reviews/wave1/01-quality.md` … `05-design.md` |
| This handoff | `docs/plans/hooks/07-next-session-handoff.md` |

Main repo: `/home/noname/Code/heimdall-mcp`. All Wave 1/2 work is on the
`main` branch. Feature branches were deleted after merge.

---

## What shipped (Wave 0 → Wave 1 → Wave 2 phases 1a/1b/2 → Sections 2–5)

### Wave 0 — initial review fixes (commits up to `93384c8`)
11 code fixes from a 1224-line review pass: dead `checkModelMismatch` removed,
`heimdall.ResolveUsableModelDB` / `NormalizeModelName` extracted, git-commit
batch embedding, EmbedBatch size cap, default context deadline in `Embed`,
`DiscoverSubRepos` helper, ETA divide-by-zero guard, `toolExplain` uses
resolved model. New `internal/heimdall/ollama_test.go` with 6 `httptest`
cases and a `ResolveUsableModelDB` table-driven test.

### Wave 1 — hook foundations (merges `cdf890b`, `4a3c643`, `55ab7c3`, `8189180`)

- **T1** `heimdall-mcp recall` CLI (shared core with MCP `toolRecall`)
- **T2** `heimdall-mcp ingest-session` CLI + length-prefixed buffer parser
- **T3** `status --format=json`
- **T9** `heimdall.VerifyHookIndex(store, model)` — three sentinel errors
  (`ErrIndexModelMissing`, `ErrIndexModelMismatch`, `ErrIndexDimMismatch`).
  **Explicitly refuses fuzzy fallback via `ResolveUsableModelDB`.**
- **T10** `hook_cache` SQLite table + `store_metadata.index_version` bump
  inside the same `Upsert` / `RemoveByFile` transaction, + two-phase
  RLock/Lock pattern in `HookCacheGet`, + 24 h TTL + 1000-row cap + 32 KB
  oversize refusal.
- **T11** Ollama `keep_alive: "10m"` on hook path only (`EmbedForHook`
  wrapper). Interactive `Embed` path unchanged.
- **T12** Tier B suppression store (SQLite at
  `$XDG_STATE_HOME/heimdall/suppress.db`, 5-min window per `(project, code)`).
- **T13** Hook log file at `$XDG_STATE_HOME/heimdall/hooks.log`, 5 MB cap,
  one rotation, panic-guarded, POSIX + Windows path redaction.
- **T17** `heimdall-mcp hooks tail` with `--level/--event/--since/--project/--follow`.
- **T18** `heimdall-mcp hooks cache-clear / cache-stats`.
- **T22** `HooksDisabled` fast-path: `HEIMDALL_HOOKS=0` env (14 ns) or
  `<project>/.heimdall/hooks.disabled` marker file (1.24 µs warm).
- Review gate: weighted 94.5/100, merged by judgment at structural ceiling
  (remaining gaps were architecturally deferred to Wave 2).

### Wave 2 phase 1a — retrieval hooks + install tooling (merges `c7f7f6c`, `12cc751`, `fbcb4dc`)

- **T5** `heimdall-mcp hook session-start` — `DispatchHook` (singular,
  retrieval) with `HookSessionStartDeps` injection. Calls `VerifyHookIndex`
  → `GatherStatus` + `RunRecall` → emits `## Heimdall context` markdown
  block. Budget default 2 s, hard timeout 3 s via `context.WithTimeout`.
  Tier B suppression on Ollama down / model mismatch / missing index.
- **T7** `heimdall-mcp hook post-edit` — `flock` (with `O_CREAT|O_EXCL`
  TTL-stamp fallback for NFS/WSL) + 2 s coalesce window + `fork+setsid`
  detached actor (`heimdall-mcp hook post-edit-actor`, hidden). State
  files under `<project>/.heimdall_db/hooks/`: `reindex.lock`,
  `reindex.pending` (capped 1000), `reindex.last_run`,
  `reindex.inflight.pid`, `reindex.deadletter.jsonl`. Deadletter retries
  with hard-drop at N=10. Foreground ≤ 50 ms.
- **T14** `heimdall-mcp install-hooks` — `--scope=user|project`, `--dry-run`,
  `--merge`, `--force`, `--only`. Atomic write via tempfile + rename.
  Always writes a backup at
  `settings.json.heimdall-backup-<UTC-ISO8601>`. Dual marker detection:
  JSON field `"source": "heimdall"` OR command-string substring
  `--source=heimdall`. Same-version no-op, different-version upgrade.
- **T15** `heimdall-mcp uninstall-hooks` — symmetric, idempotent.
- **T16** `heimdall-mcp hooks doctor` — 11-check pass/fail table,
  dry-fires installed hooks, skips the internal `post-edit-actor`.
- Review gate: **97.8/100** weighted — Quality 98, Security 99,
  Performance 97, Tests 96, Design 98. 110 tests in `internal/cli`, all
  green under `-race`.

### Wave 2 phase 1b — hot path (merged as PR #10, merge `320c4bd`, impl `648a388`)

- **T4** `heimdall-mcp search --format=hook-md` + `--budget-ms`. Breaking
  change: `heimdall.SearchFiltered` now takes `ctx context.Context` as its
  first argument. 30+ call sites updated across CLI, MCP server, and
  tests. The Wave 1 `TestSearchFiltered_BudgetTimeout` skip stub was
  deleted and replaced with real pre-cancelled-ctx assertions plus a
  nil-ctx tolerance case. The row loop now checks `ctx.Err()` so a
  budget timeout cuts scoring mid-flight rather than only at the end.
- **T6** `heimdall-mcp hook user-prompt` — the hot-path retrieval hook.
  250 ms default budget, 500 ms hard cap via `context.WithTimeout`.
  Length-guard skip heuristic: prompts under 8 chars short-circuit with
  no output (rejected LLM-gate variant stays rejected — see 00 §5.2).
  Cache lookup keyed on `(normalized_prompt, index_version, project)`
  using the Wave 1 `hook_cache` table. Tier B suppression on Ollama
  down / model mismatch / missing index via the Wave 1 suppress store.
  Cache store on miss; cache hit short-circuits embedding entirely.
  Empty stdin falls back to CWD for project resolution. `--prompt` flag
  override for doctor dry-fires. All retrieval-hook OQ-5 rules apply:
  always exits 0, routes everything through `LogHookEvent`, never
  writes to stderr.
- **T14 update** — `install-hooks` template now installs **three** hooks:
  `SessionStart`, `PostToolUse(Edit|Write)`, and `UserPromptSubmit`. The
  dual marker detection (JSON `"source": "heimdall"` + command-string
  `--source=heimdall`) extends to the new entry. Version constants
  bumped to `wave2-phase1b` on both the `--version` handler and the
  install envelope.
- **T21 skipped.** Original plan called for `BenchmarkUserPromptHook`
  asserting p95 ≤ 250 ms uncached, p50 ≤ 50 ms cached, hard ≤ 500 ms.
  Replaced per user direction with a post-merge same-task
  with-vs-without-hooks comparison on a real Claude Code session.
  Tracked as an open follow-up below, not a gate.
- **Soak gate killed.** The `00-consolidated-plan.md §7` language still
  says "1a verified end-to-end" is the 1b entry criterion and it is —
  but the "stable ≥ 1 week" version of that gate was dropped. Phase 1b
  tests instead are: 16 new unit cases under `-race`, `hooks doctor`
  dry-fire of `user-prompt` green, and live dogfood.
- **Tests:** 16 new cases in `internal/cli/hook_user_prompt_test.go`
  covering: skip (length guard), disabled (env + marker file), Ollama
  down (Tier B suppressed first occurrence, silent after), no index,
  model mismatch, happy path caches result, cache hit short-circuits,
  cache invalidation on `index_version` bump, budget timeout cuts row
  loop, prompt normalization, empty stdin → CWD fallback, `--prompt`
  flag override, malformed JSON payload, dispatcher routing, version
  stamp.

### Wave 2 phase 2 + sections 2–5 (merged as PR #11, merge `b05af92`, impl `8b3442b`)

**T8 — session learning (hook stop + session-end):**
- `hook stop` appends `last_assistant_message` to a per-session JSONL
  rolling buffer at `<project>/.heimdall_db/hooks/sessions/<session_id>.jsonl`,
  capped at 2 MB. Confirmed Stop event payload: `session_id`,
  `transcript_path`, `last_assistant_message`, `cwd`, `stop_hook_active`.
- `hook session-end` triggers `ingest-session` from `transcript_path`
  JSONL (last 5 assistant messages, 500 chars each, 5 KB total cap),
  then cleans up the rolling buffer file. SessionEnd payload: `session_id`,
  `transcript_path`, `cwd`, `reason`.
- Install template updated to **5 hooks**: SessionStart, PostToolUse,
  UserPromptSubmit, Stop, SessionEnd.
- 12 unit tests in `hook_stop_test.go`.

**Section 2 — tiered retrieval (L0/L1/L2):**
- `summary TEXT` column on entries, auto-generated via heuristic
  `GenerateSummary()` (identifier+kind for named chunks, first
  non-comment line for paragraphs).
- `heimdall_search` gained `detail=summary|snippet|full` param.
- New `heimdall_expand(chunk_id)` MCP tool for drill-down.
- `SearchResultEnriched` includes `summary` and `contextPath`.

**Section 3 — path-based context hierarchy:**
- `context_path TEXT` column, auto-derived from file path directory
  hierarchy via `deriveContextPath()`, slash-normalized.
- `heimdall_search` gained `scope=` prefix filter via `WithScope()`
  functional option.
- New `heimdall_ls(path)` MCP tool — returns child paths with chunk
  counts for filesystem-style navigation.

**Section 4 — skills as indexable content:**
- `MemoryTypeSkill` added to memory type system + validation.
- `heimdall_remember --type=skill` accepted in MCP tool schema.

**Section 5 — all 6 LOW-severity findings fixed:**
- SEC-001: SAFETY comments on dynamic SQL builders.
- SEC-002: `sub_project` 255-char length cap in toolSearch.
- SEC-003: `sanitizeStoreError()` logs full error server-side, returns
  generic message to client.
- DES-006: `EmbedBatchSize` added to Config with default 32.
- DES-010: `computeETA()` extracted as pure function + 6 tests.
- TEST-013: `MockEmbedder` → `StubEmbedder` across all source files.

**Performance (PERF-002, PERF-003):**
- `bumpIndexVersionTx`: single atomic `INSERT...ON CONFLICT UPDATE`.
- hook_cache eviction: lazy-init row counter, no O(n) COUNT per insert.
- 3 new perf tests in `hook_cache_test.go`.

**Other:**
- First-run hint after `index` (6 tests in `install_test.go`).
- T24 Windows path redaction verified correct (no changes needed).
- Pre-Wave-1 review docs archived to `docs/reviews/pre-wave1/`.

### Wave 2 phase 1a dogfood follow-ups — merged as PR #7 (merge commit `15d1723`)

Four bugs surfaced on the first real dogfood run that blocked or soft-failed
the runbook below. All fixed in a single follow-up commit (`367d225`) that
landed on `main` after the phase 1a body.

- **`heimdall-mcp index` hung on `/dev/tty`** when ≥2 embedding models were
  pulled. `huh.NewMultiSelect` opens `/dev/tty` directly — no non-interactive
  bypass, and `config.model` was only used as a pre-highlight. Meant CI, cron,
  and the post-edit actor's reindex path all had no way through. **Fix:**
  new `--model <name>` flag and `resolveIndexModels` helper. Resolution
  order: explicit flag → `config.model` auto-pick → single-embeddable
  auto-pick → interactive `huh` prompt. Tolerates `:latest` tag variance.
- **`hooks doctor` reported `[XX] heimdall-mcp --version exit status 1`** on
  a clean install. Doctor check #5 shells out to `<bin> --version` but the
  CLI had no version handler at all. **Fix:** added `version` / `--version`
  / `-V` backed by `runtime/debug.ReadBuildInfo()`. Works with `go build`
  and `go install` without `-ldflags` injection. Falls back to
  `"wave2-phase1a"` when build info is unavailable.
- **`hooks tail --since=1h` rejected the `=` form.** Parser only matched
  `--since 1h` (space-separated). **Fix:** `splitEqualsFlags` normalization
  pass at the top of `parseTailFlags` rewrites `--flag=value` into two
  tokens uniformly so both forms work on every flag.
- **`.claude/worktrees` missing from default `excludePatterns`.** First
  dogfood index took 18m28s because it re-embedded three stale Apr-11
  agent worktrees. **Fix:** added `.claude/worktrees` (narrowly — not all
  of `.claude/`, so user `commands/`, `agents/`, `skills/` remain indexable).
- **Tests:** 10 new unit tests (`discover_test.go` ×8, `version_test.go` ×2)
  plus an assertion in `TestDefaultConfig_NewFields` that the new exclude is
  present. Full suite green under `-race`.

---

## Locked decisions (OQ-1..OQ-5) — do not relitigate

From `docs/plans/hooks/06-decisions.md`:

1. **OQ-1 Unknown-field tolerance on hook entries.** Proceed assuming
   Claude Code tolerates `"source": "heimdall"` + `"version": 1` on hook
   entries. **The `--source=heimdall --version=1` command-string fallback
   is mandatory regardless** — every installed hook command carries these
   flags so detection works even if Claude Code is strict.
2. **OQ-2 Install mode.** **Opt-in.** `heimdall-mcp install-hooks` is an
   explicit user-run command. No auto-install on first index.
3. **OQ-3 Binary name.** Keep `heimdall-mcp`. No rename. No `heimdall`
   alias. Hook command strings bake in `heimdall-mcp` verbatim.
4. **OQ-4 `settings.json` JSON round-trip.** `encoding/json` only +
   always-on backup. No `hujson` vendoring. Top-level keys sorted
   alphabetically — users with hand-edited settings will see a reformat.
5. **OQ-5 Exit codes.** **Retrieval hooks always exit 0** (`hook
   session-start`, `hook post-edit`, `hook post-edit-actor`, and even
   `DispatchHook`'s usage-error paths). Interactive commands
   (`install-hooks`, `uninstall-hooks`, `hooks doctor`, `hooks tail`,
   `hooks cache-clear`, `hooks cache-stats`, `recall`, `ingest-session`,
   `status`) use `0 / 1 / 2`.

---

## Next steps (in order)

1. ~~Phase 1a dogfood.~~ **✅ VERIFIED 2026-04-16.**
2. ~~Soak gate.~~ **❌ KILLED.**
3. ~~Phase 1b (T4 + T6).~~ **✅ SHIPPED** — PR #10.
4. ~~Phase 2 (T8 + sections 2–5).~~ **✅ SHIPPED** — PR #11.

5. **Reinstall hooks.** The install template now has 5 hooks (was 3).
   Run `heimdall-mcp install-hooks --scope=project --force` to pick up
   the Stop + SessionEnd entries, then `hooks doctor` to verify.

6. **Dogfood Stop + SessionEnd end-to-end.** Work a real session, then
   close Claude Code. Check:
   - `heimdall-mcp hooks tail --event=stop --since=1h` — buffer_appended lines
   - `heimdall-mcp hooks tail --event=session-end --since=1h` — ingest_ok or session_ended
   - `ls <project>/.heimdall_db/hooks/sessions/` — buffer files should be cleaned up

7. **Dogfood tiered retrieval.** Use `heimdall_search` with `detail=summary`
   then `heimdall_expand` on a result. Use `heimdall_ls` to browse the
   path hierarchy. Verify summaries are useful and context_paths are correct.

8. **With-vs-without comparison (T21 replacement).** Pick a real task, run
   twice in fresh sessions — `HEIMDALL_HOOKS=0` control vs defaults.
   Compare quality, token spend, tool-call count.

9. **Phase 3 — guardrails.** `PreToolUse(Bash(rm *|git push --force*))`
   as an `agent`-type hook. **Blocked** on designing a destructive-op
   judgment primitive — heimdall has no way to classify a bash command
   as dangerous today. Needs a design decision before implementation.

---

## Dogfood sequence (run in this order — status shown per step)

All commands assume CWD is `/home/noname/Code/heimdall-mcp`.

| # | Step | Status | Notes |
|---|---|---|---|
| 0 | `go build ./... && go vet ./... && go test ./... -race` | ✅ | Green under race |
| 1 | `heimdall-mcp status` | ✅ | Ollama up, no prior index |
| 2 | `heimdall-mcp index .` | ✅ | 234 files, 139 indexed, 1758 chunks, 18m28s (pre-`.claude/worktrees` exclude) |
| 3 | `heimdall-mcp status --format=json` | ✅ | `embedding_dim=768`, `lastIndexed` set |
| 4 | `heimdall-mcp install-hooks --scope=project --dry-run` | ✅ | Diff showed both hooks + dual markers |
| 5 | `heimdall-mcp install-hooks --scope=project` | ✅ | Wrote `.claude/settings.json`, no backup (no prior file) |
| 6 | `heimdall-mcp hooks doctor` | ✅ | **11/11 green** after PR #7's `--version` fix |
| 7 | `heimdall-mcp hooks tail --since=1h` | ✅ | Confirmed dry-fire `bullets=5 chunks=1758 stage=ok` |
| 8 | Restart Claude Code → verify `SessionStart` fires | ✅ | 2026-04-16: `bullets=5 chunks=1758 model=nomic-embed-text stage=ok` from real hook pipeline |
| 9 | Real `Edit` tool call → verify `PostToolUse` + actor reaches `reindex_ok` | ✅ | 2026-04-16: `actor_spawned` → `files=1 msg=reindex_ok` in ~67 s (first embed cold), no deadletter |

Raw command sequence for reference / re-running after a reindex:

```bash
# 0. Confirm clean build
go build ./... && go vet ./... && go test ./... -race

# 1. See what heimdall thinks exists
heimdall-mcp status

# 2. Index this repo (first time → no DB → creates one)
heimdall-mcp index .

# 3. Verify the index landed
heimdall-mcp status --format=json
# expect: non-zero chunkCount, embedding_model set, embedding_dim set

# 4. Preview the hook install — DO NOT skip --dry-run the first time
heimdall-mcp install-hooks --scope=project --dry-run

# 5. If the diff looks right, install for real
heimdall-mcp install-hooks --scope=project
# expect: backup path on stdout, no error

# 6. Diagnose — this is the pre-restart health check
heimdall-mcp hooks doctor
# expect: mostly ✓; ⚠ on Ollama if it's not running;
# ✗ on anything means stop and debug before restarting Claude

# 7. Log sanity
heimdall-mcp hooks tail --since=1h
# expect: empty or a few lines from install events

# 8. Close this Claude Code session

# 9. Reopen Claude Code in this repo
# → SessionStart hook should fire and inject `## Heimdall context` into
# the initial context. You should see it referenced in the very first
# turn without the model having to call heimdall_search / heimdall_recall
# manually.

# 10. Edit any file (e.g. a one-char typo fix)
# → PostToolUse(Edit|Write) should fire and enqueue the file for
# background reindex. Verify:
heimdall-mcp hooks tail --event=post-edit --since=5m
# expect: one or more "event=post-edit" lines
```

---

## Caveats and known unknowns

1. **`~/.claude/settings.json` schema is a best-guess.** Stream F's
   install-hooks built the shape from the consolidated plan's example JSON,
   not from Claude Code docs — we didn't have docs in-tree. If Claude Code
   rejects the shape at runtime, the dogfood step 9 will fail silently
   (hook never fires) or loudly (Claude Code startup error). Recovery:
   `heimdall-mcp uninstall-hooks --scope=project` reverts cleanly, then
   the follow-up is to inspect a working Claude Code settings.json, learn
   the real shape, and patch `phase1aHooks` in `internal/cli/install.go`.

2. **`fork+setsid` in Go is environment-dependent.** The post-edit actor
   uses `exec.Command(self, "hook", "post-edit-actor", ...)` with
   `SysProcAttr.Setsid = true` + `cmd.Start()` + `cmd.Process.Release()`.
   This is unit-tested via an injectable `spawnFn` spy — **the real spawn
   is not unit-tested**. First dogfood run is the first real exercise.
   Watch `heimdall-mcp hooks tail --event=post-edit` and
   `reindex.deadletter.jsonl` for surprises.

3. **Ollama must be running before `SessionStart` fires, or the hook will
   emit a Tier B "unavailable" note on its first occurrence** and then
   suppress for 5 minutes per `(project, failure_code)`. If Ollama is
   starting slow and you reopen Claude Code before it's ready, the first
   turn won't get heimdall context. Restart once Ollama is up.

4. **`VerifyHookIndex` will fail if the index was built with a different
   model than your current `heimdall-mcp config`.** The gate is strict by
   design (OQ-5 golden rule: no fuzzy match on hook path). If doctor
   check #9 is red, either re-index or switch the configured model.

5. **Three stale `.claude/worktrees/agent-*` worktrees from 2026-04-11**
   still on disk, pointing at the pre-Wave-1 commit `1e94085`. They are
   now **excluded from `heimdall-mcp index` by default** via PR #7's
   `excludePatterns` addition, so they don't inflate reindex time. They
   still show up in `git worktree list` though — prune them at leisure
   with `git worktree remove --force .claude/worktrees/agent-a68e0b3c`
   (and the other two). Not blocking anything.

6. **Main is up to date with `origin/main`.** Wave 1 + Wave 2 phase 1a
   shipped via **PR #5** (merge commit `67e8335`, merged 2026-04-15
   23:04Z). The four dogfood follow-up fixes shipped via **PR #7**
   (merge commit `15d1723`, merged 2026-04-15 23:07Z). PR #6 was a
   stacked-PR collateral casualty — auto-closed when its base
   (`feat/hooks-integration-waves-0-2`) was deleted on the #5 merge.
   Same commit was resubmitted and merged as #7.

7. **Unit tests in `internal/cli` write to the real
   `$XDG_STATE_HOME/heimdall/hooks.log`** instead of `t.TempDir()`.
   Dogfood's `hooks tail` showed test artifacts (timestamps with
   `pid=999999` and fake `context deadline exceeded` errors) polluting
   user state. Worth a follow-up PR — scoped out of PR #7 because it
   touches many test files. Symptom on your end: `hooks tail` will
   show occasional lines that don't match anything you actually did.

---

## What's next — post phase 2

Phases 1a, 1b, and 2 are all **shipped**. TODO sections 2–5 are done.

**Immediate follow-ups (manual, need human):**
- Reinstall hooks (5 hooks now, was 3) — `install-hooks --scope=project --force`
- Dogfood Stop + SessionEnd live — close a real session, check hooks tail
- Dogfood tiered retrieval — test `detail=summary` + `heimdall_expand`
- With-vs-without comparison (T21 replacement)

**Open follow-ups (incremental, from TODO.md):**
- Auto-surface skills in SessionStart/UserPromptSubmit hooks
- Memory path auto-detect for `context_path`
- Hook scope filtering by CWD subpath
- Token-savings measurement for tiered retrieval
- Two-way sync with `~/.claude/skills/` directory

**Blocked:**
- Phase 3 guardrails — needs a destructive-op judgment primitive

**Deferred (testing infrastructure):**
- T20 Layer 2 integration tests (os/exec + fake Ollama)
- T23 Layer 3 e2e harness (needs real Claude CLI)

---

## File layout quick reference

```
internal/
  cli/
    cli.go                 — RunCLI dispatcher, wires every subcommand
    hook.go                — DispatchHook (retrieval) + HookSessionStart
    hook_test.go           — 15 tests for session-start
    hook_post_edit.go      — HookPostEdit (foreground + spawn wrapper)
    hook_post_edit_actor.go — runPostEditActor (pure testable actor core)
    hook_post_edit_test.go — 20 tests for post-edit + actor
    hook_user_prompt.go    — HookUserPrompt (phase 1b, cache-first hot path)
    hook_user_prompt_test.go — 16 tests for user-prompt hook
    hook_stop.go           — HookStop (rolling buffer) + HookSessionEnd (ingest trigger)
    hook_stop_test.go      — 12 tests for stop + session-end
    eta_test.go            — 6 tests for computeETA (DES-010)
    hooks.go               — DispatchHooks (admin: tail/cache-clear/cache-stats/doctor)
    hooks_test.go          — Wave 1 hooks admin tests
    install.go             — CLIInstallHooks + CLIUninstallHooks + hooksDetected
    doctor.go              — HooksDoctor (11-check pipeline)
    install_test.go        — 35+ tests for install/uninstall/doctor/hooksDetected
    recall.go              — CLIRecall (top-level, shared core)
    ingest_session.go      — CLIIngestSession (top-level)
    status.go              — CLIStatus (top-level)
    testdata/
      hook_session_start.golden.md — SessionStart block template
      status.golden.json   — status --format=json golden

  heimdall/
    verify.go              — VerifyHookIndex + sentinel errors (T9)
    hook_cache.go          — hook_cache table + index_version bump (T10)
    ollama.go              — EmbedForHook (T11)
    suppress.go            — ShouldEmitTierB (T12)
    hooklog.go             — LogHookEvent + rotation + redaction (T13)
    hookgate.go            — HooksDisabled (T22)
    dbpath.go              — NormalizeModelName, ResolveUsableModelDB,
                             ModelDBDir, DiscoverSubRepos
    cli_core.go            — RunRecall, IngestSessionSummary,
                             ReadLengthPrefixedBuffer (Wave 1 shared core)
    cli_status.go          — GatherStatus (shared by CLI + MCP)

docs/plans/hooks/
  00-consolidated-plan.md  — authoritative implementation plan
  01-architecture.md       — hook event contracts
  02-cli-surface.md        — CLI command design
  03-latency.md            — measured latency budgets
  04-failure-modes.md      — Tier A/B/C failure matrix + install-time
  05-testing-rollout.md    — test strategy + phased rollout
  06-decisions.md          — OQ-1..OQ-5 locked answers
  07-next-session-handoff.md — this file
```

---

## Red flags — stop and ask if you see any of these

- `go test -race` fails anywhere on `main`. Phase 1a's gate was 110 tests
  green under `-race`; any regression means a commit after `8027105`
  broke something.
- `heimdall-mcp hooks doctor` prints any `✗` (red) after a clean install.
- `heimdall-mcp hooks tail --event=post-edit` shows the same file path
  retried 10+ times — that's the hard-drop threshold on deadletter; the
  actor is failing on the same input every time.
- Claude Code refuses to start after `install-hooks`. Immediately run
  `heimdall-mcp uninstall-hooks --scope=project` from a terminal; the
  backup at `settings.json.heimdall-backup-*` is the authoritative
  pre-install state.
- Any retrieval hook writes to stderr on a Claude Code fire. That's a
  §5.9 / OQ-5 violation and means a handler bypassed `LogHookEvent`.

---

## Final git state at session end (2026-04-16, post phase 2 + sections 2–5 merge)

```
$ git log --oneline -6
b05af92 Merge pull request #11 from revprism-dev-bot/docs/hooks-phase1b-post-merge-handoff
8b3442b feat: tiered retrieval, path hierarchy, session learning, LOW fixes, perf
7550aa3 docs(hooks): post phase 1b handoff + archive pre-wave1 reviews
320c4bd Merge pull request #10 from revprism-dev-bot/feat/hooks-phase1b-wave2
648a388 feat(hooks): Wave 2 phase 1b — T4 search --format=hook-md + T6 hook user-prompt
d0fb79c Merge pull request #9 from revprism-dev-bot/docs/hooks-phase1a-fully-verified
```

Main is **in sync with `origin/main`** at `b05af92`. Three stale
`agent-*` worktrees from 2026-04-11 remain in `git worktree list` —
harmless, excluded from indexing, prune manually.

**Open known-unknowns (ordered by urgency):**

1. ~~SessionStart fires?~~ **✅ yes.**
2. ~~PostToolUse fires + actor works?~~ **✅ yes.**
3. ~~Stop event payload shape?~~ **✅ CONFIRMED from Claude Code docs.**
4. **Do Stop + SessionEnd hooks work end-to-end in real sessions?**
   Unit-tested but not yet dogfooded. Need to reinstall hooks (5 now,
   was 3) and close a session to exercise both.
5. **Does `UserPromptSubmit` cache-hit ratio improve over sessions?**
   The hook fires on every turn — check `hooks tail --event=user-prompt`.
6. **With-vs-without comparison (T21 replacement).** Not yet run.
7. **Phase 3 guardrails design.** Needs a destructive-op judgment
   primitive that doesn't exist. Design decision required.
8. Is `.claude/settings.json` worth gitignoring? (Cosmetic.)
9. Do unit-test hook-log artifacts need a temp-dir fix? (Follow-up.)
10. Should the post-edit actor pre-warm Ollama on install?
