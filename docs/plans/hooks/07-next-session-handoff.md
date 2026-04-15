# 07 — Next Session Handoff

**Purpose:** everything needed to resume heimdall-mcp work in a fresh Claude Code
session without re-reading the prior-session conversation. Paste the
"Opening prompt for the next session" below into the new session, or just
point at this file.

---

## Opening prompt for the next session

> We just merged Wave 2 phase 1a of the Claude Code hooks integration for
> heimdall-mcp. Main is 21 commits ahead of origin/main locally (not pushed).
> The full roadmap, locked decisions, implementation plan, and review gate
> history are in `docs/plans/hooks/`. The immediate task is **dogfooding**:
> index this repo, install the hooks, verify with doctor, then restart
> Claude Code to see `SessionStart` and `PostToolUse(Edit|Write)` actually
> fire. Before touching code, read `docs/plans/hooks/07-next-session-handoff.md`
> — it has the exact command sequence, known caveats, and recovery paths.
> Do not start Wave 2 phase 1b (the UserPromptSubmit hot path) until
> phase 1a has soaked for at least a day.

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

## What shipped (Wave 0 → Wave 1 → Wave 2 phase 1a)

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

## Dogfood sequence (run in this order)

All commands assume CWD is `/home/noname/Code/heimdall-mcp`.

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

5. **The current worktree `wave1-stream-a-cli` is stale** from the prior
   session and can't be pruned while the prior session still holds it.
   After closing the prior session, run:
   `git -C /home/noname/Code/heimdall-mcp worktree remove .claude/worktrees/wave1-stream-a-cli --force`
   (or `git worktree prune` after deleting any remaining branches).

6. **Main is 21 commits ahead of `origin/main` locally, not pushed.** Per
   global CLAUDE.md rules, push only when you're ready and only through a
   feature branch + PR, never directly to main. The branches were already
   merged locally; re-creating feature branches for a remote push is
   awkward. Pragmatic option: push `main` to a new remote branch
   (`git push origin main:feat/hooks-integration-waves-0-2`) and open a
   PR from there.

---

## What to do after dogfood succeeds

**If dogfood goes clean:** open Wave 2 phase 1b. Scope:

- **T4** `--format=hook-md` on `search` + `--budget-ms`. Breaking change:
  adds `ctx context.Context` parameter to `SearchFiltered` (removes the
  Wave 1 `TestSearchFiltered_BudgetTimeout` skip stub). Required before T6.
- **T6** `hook user-prompt` — the hot path. 250 ms p95 uncached, 50 ms
  p50 cached, 500 ms hard timeout. Calls the Wave 1 `hook_cache` keyed on
  `(normalized_prompt, index_version, scope)`. Length-guard skip heuristic
  for trivial prompts (`len(prompt) < 8`). The `prompt`-hook LLM gate was
  rejected (see consolidated plan §5.2) — do not revisit.
- **T21** `BenchmarkUserPromptHook` regression baseline.

Phase 1b gate criteria (from `00-consolidated-plan.md §7`):
- Phase 1a stable ≥ 1 week.
- T21 benchmark green on a mid-size repo (≥ 10k chunks): p95 ≤ 250 ms
  uncached, p50 ≤ 50 ms cached, hard ≤ 500 ms.
- Model-mismatch Tier B path exercised end-to-end.
- Cache-invalidation path verified against a concurrent edit.

**If dogfood reveals a settings.json shape bug:** do NOT try to fix it
while blind. Uninstall, read a working Claude Code settings.json, update
the `phase1aHooks` template in `internal/cli/install.go`, add a regression
test, reinstall, retry dogfood.

**If dogfood reveals a fork+setsid bug:** the simplest first fix is to
fall back to synchronous in-process reindex in the actor path. Add a
`HEIMDALL_POST_EDIT_SYNC=1` env var that short-circuits the fork and
runs the actor body in the foreground goroutine. It's not production-ideal
(reindex blocks the hook for seconds) but it unblocks dogfood while the
real fork issue is triaged.

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
    hooks.go               — DispatchHooks (admin: tail/cache-clear/cache-stats/doctor)
    hooks_test.go          — Wave 1 hooks admin tests
    install.go             — CLIInstallHooks + CLIUninstallHooks
    doctor.go              — HooksDoctor (11-check pipeline)
    install_test.go        — 29 tests for install/uninstall/doctor
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

## Final git state at session end

```
$ git log --oneline -5
8027105 docs: mark Wave 2 phase 1a merged in TODO.md
fbcb4dc Merge feat/wave2-stream-f-install-uninstall-doctor (Wave 2 T14/T15/T16)
12cc751 Merge feat/wave2-stream-e-post-edit (Wave 2 T7 hook post-edit + detached actor)
c7f7f6c Merge feat/wave2-stream-d-session-start (Wave 2 T5 hook session-start)
2d9663a feat(cli): install-hooks, uninstall-hooks, hooks doctor (T14+T15+T16+T19)

$ git status --short
# nothing on main itself; only untracked noise under .claude/ and .idea/
```

Main is clean. 21 commits ahead of `origin/main`. Worktree `wave1-stream-a-cli`
still listed in `git worktree list` — ignore, prune manually.
