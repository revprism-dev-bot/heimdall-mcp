# 07 — Next Session Handoff

**Purpose:** everything needed to resume heimdall-mcp work in a fresh Claude Code
session without re-reading the prior-session conversation. Paste the
"Opening prompt for the next session" below into the new session, or just
point at this file.

---

## Opening prompt for the next session

> **Status at `main` @ commit `1b9e92e` (2026-04-18 evening):** clean
> tree, 11 PRs merged this session (#39–#49). Full suite green under
> `go test ./... -race -count=1`. `go vet ./...` clean. Binary at
> `/home/noname/.local/bin/heimdall-mcp` → symlink →
> `/home/noname/Code/heimdall-mcp/heimdall-mcp`, reports `(1b9e92e)`
> after rebuild. **First action: rebuild, then pick an item from the
> priority list below. The two biggest open tracks — semantic-drift
> metric and LLM classifier fallback — both have design docs in
> `docs/plans/hooks/` with open questions in §9 that need resolving
> before code lands.**
>
> **Priority punch list (pick in order, none are blocking each other):**
>
> 1. **`bench-retrieval` auto-scope verification.** PR #41 made
>    `cmd/bench-retrieval` pick up the CWD repo's DB via
>    `FindRepoRoot`; `--db` override still works. Trivial to verify:
>    `cd` into any indexed project, run the binary, confirm it finds
>    the right DB without a flag. 5 minutes of work.
>
> 2. **Smoke-harness re-run.** `heimdall-mcp hooks smoke --fake-ollama`
>    (PR #47) now exercises all 6 hooks end-to-end without a real
>    Claude Code restart. Last live run at end of 2026-04-18: **all 6
>    PASS.** Re-run after any hook change before merging.
>
> 3. **Semantic-drift metric implementation** per
>    `docs/plans/hooks/12-semantic-drift-metric.md` (PR #46 design, 705
>    lines, 3-stage rollout). Stage 1 is log-only: add `hit_ids` and
>    `prompt_embed_b64` to UserPromptSubmit success events, no behavior
>    change. **Prerequisite — resolve the 10 open questions in §9 of
>    plan 12, especially OQ-3 (calibration harness design) before
>    writing code.** Stage 2→3 is gated on the `cmd/calibrate-drift`
>    harness itself (item 6 below).
>
> 4. **LLM classifier fallback implementation** per
>    `docs/plans/hooks/11-llm-classification-fallback.md` (PR #40
>    design, 768 lines). The prerequisite `ClassUnknown` default
>    fall-through shipped as PR #45 — the design's integration point is
>    now unblocked. **Prerequisite — resolve the open questions in §9
>    of plan 11, especially the model-recommendation bake-off (the
>    doc lists several candidates but does not pick one).**
>
> 5. **Shadow-mode guardrail audit.** Wait ≥1 week of real-session
>    traffic accumulates in `hooks.log`, then run
>    `heimdall-mcp hooks audit-guardrails --since=168h --format=json`
>    (PR #44 — 14th hooks subcommand). If the report recommends
>    `promote_to_warn=true` with zero false-positive candidates,
>    switch the default `HEIMDALL_GUARDRAILS=shadow` → `warn`.
>
> 6. **`cmd/calibrate-drift` harness** — gates Stage 2→3 of the
>    semantic-drift rollout per plan 12. Design details in the plan's
>    §Calibration section; OQ-3 in §9 is the main open question.
>
> 7. **✅ Resolved: cache-hit ratio consolidation.** Investigation
>    confirmed `heimdall_contribution.cache_hits` and the top-level
>    `user_prompt_cache_hits` read from the same
>    `SessionHookAggregate.UserPromptCacheHits` counter — no semantic
>    split. The top-level pair (with companion
>    `user_prompt_cache_total` denominator) is now the canonical source;
>    `heimdall_contribution.cache_hits` is retained as a deprecated
>    back-compat alias and documented as such in `sessions.go toJSON()`.
>    A new test (`TestSessionsReport_CacheHitsAliasMatchesTopLevel`)
>    asserts the two fields never diverge. Schema stays `v1` — safe to
>    drop the alias on a future v2 revision.
>
> **This session's shipped PRs (all merged, for reference):**
> - **#39** `feat(sessions): add redundant_heimdall_calls to SessionReport`
>   (`f205a21`) — Wave F item 3(B) cheap counter, additive JSON field.
> - **#40** `docs(hooks): design doc for LLM classification fallback`
>   (`c920625`) — plan 11, flagged ClassUnknown as prerequisite.
> - **#41** `feat(bench): auto-scope bench-retrieval to CWD's repo`
>   (`e96fe10`) — `cmd/bench-retrieval` picks up DB via `FindRepoRoot`.
> - **#42** `feat(install): run skills import as best-effort
>   post-install step` (`c4a3383`) — `--no-skills-import` flag, mirrors
>   prewarm contract.
> - **#43** `feat(sessions): add user-prompt cache-hit counters to
>   SessionReport` (`8808e99`) — top-level counters.
> - **#44** `feat(hooks): add audit-guardrails subcommand for PreToolUse
>   shadow-mode review` (`7af921c`) — 14th hooks subcommand.
> - **#45** `feat(guardrails): introduce ClassUnknown for default
>   fall-through` (`1bed701`) — exit 0 in all modes, never blocks.
> - **#46** `docs(hooks): semantic-drift metric design for missed
>   tool-call opportunities` (`b039c9c`) — plan 12, 3-stage rollout.
> - **#47** `feat(hooks): add hooks smoke end-to-end harness for all 6
>   hooks` (`e5a738d`) — dispatchable substitute for manual restart.
> - **#48** `fix(hooks): drop redundant event key from Stop/SessionEnd
>   kv maps` (`18e217f`) — pre-existing bug surfaced by PR #47.
> - **#49** `fix(hooks): quote-aware parser + lossless producer for
>   hooks.log values` (`1b9e92e`) — pre-existing bug surfaced by PR
>   #44; `parseHookLogLine` now round-trips via `strconv.Quote/Unquote`.
>
> **Prior merged work (reference only, don't re-litigate):** Wave F
> (2026-04-17) shipped PRs #33–#37: sessions `--since`, JSON
> `schema_version`, doctor check #14, skill-body chunking,
> `sessions report --current`, README audit, `part-*` skill dedup,
> post-edit actor session attribution. Earlier, PRs #25–#29 shipped
> the per-session savings feature
> (`docs/plans/hooks/10-per-session-savings-report.md`) in Waves A–E.
> PR #31 bumped the first-turn `UserPromptSubmit` budget 250→450ms.
>
> **Heimdall installs 6 hooks**: `SessionStart`, `PostToolUse(Edit|Write)`,
> `UserPromptSubmit`, `Stop`, `SessionEnd`, `PreToolUse(Bash)`. Every
> retrieval-hook fire carries `session=<uuid>` in `hooks.log` (Wave A).
> PreToolUse guardrail default is **shadow** — classifier runs, never
> blocks. With `ClassUnknown` (PR #45), the default fall-through for
> commands no rule matches is also allow-equivalent. Toggle the mode
> via `HEIMDALL_GUARDRAILS=shadow|warn|block|off`.
>
> **Measured wins already on the table:**
> - 62.4% token savings from tiered retrieval at 20% expand rate
>   (`docs/plans/hooks/09-tiered-retrieval-benchmark.md`, `make bench`).
> - Per-session savings are queryable post-hoc via
>   `heimdall-mcp sessions report --session-id=<id>` — joins
>   transcript (tokens, tool calls, hook_success bytes) with hooks.log
>   (cache hits, guardrail verdicts, reindex counts). `--format=json`
>   emits `schema_version: "v1"`. Session-end JSON includes
>   `redundant_heimdall_calls` (#39) and top-level
>   `user_prompt_cache_hits` / `user_prompt_cache_total` (#43).
> - `heimdall-mcp hooks smoke --fake-ollama` (#47) is the fastest way
>   to validate the full 6-hook pipeline after a change; expect "PASS"
>   on all six events.
>
> Before any code edit, sanity-check:
> ```bash
> cd /home/noname/Code/heimdall-mcp
> git status && git log --oneline -15
> go build ./... && go vet ./... && go test ./... -race -count=1
> ```
> Expect: clean tree on top of `1b9e92e`. All tests pass.

---

## 2026-04-17 polish pass (uncommitted when this doc was written)

Four follow-ups shipped in one session, in response to "do all work now —
stop asking, implement everything, tests later":

**1. `sessions list --since=<duration>` filter**
- `internal/heimdall/hooklog_reader.go`: `SessionHookAggregate` gained
  `FirstSeen` / `LastSeen` fields, populated in `AggregateHookLogBySession`.
- `internal/cli/sessions.go`: new `--since` flag (Go duration).
  Invalid duration → rc=2. Empty window prints a clear hint rather than
  the generic no-sessions line.
- JSON rows now include `first_seen` / `last_seen` (RFC3339) and the top
  level is `{schema_version, sessions: []}` instead of a bare array.
- Tests: `TestSessionsList_SinceFilter`, `TestSessionsList_SinceInvalid`,
  `TestSessionsList_JSONSchemaVersion`, `TestSessionsList_JSONEmpty`.

**2. JSON `schema_version`**
- `SessionsReportSchemaVersion = "v1"` constant in `internal/cli/sessions.go`.
- Emitted on both `sessions list --format=json` and `sessions report
  --format=json`.
- Bump on **breaking** changes only (rename/removal/type change). New
  fields are additive and do not require a version bump.
- Tests: `TestSessionsReport_JSONSchemaVersion` + the list-side tests above.

**3. Doctor check #14: `sessions pipeline`**
- `internal/cli/doctor.go`: reads `hooks.log` via `ReadHookLog`, folds by
  session via `AggregateHookLogBySession`, reports:
  - Pass: `N session(s) available for `sessions list``
  - Warn: `no sessions in hooks.log yet (expected on fresh install)`
  - Fail: `hooks.log unreadable: <err>` (only on non-ENOENT errors)
- Does NOT try to render a full report — that would false-fail on any
  machine where the transcript hasn't been written yet. See
  `docs/plans/hooks/10-per-session-savings-report.md` §Open decisions #6.
- Check count bumped 13→14 in the file header comment and in `cap(checks)`.
- Tests: `TestDoctorChecks_SessionsPipeline_NoLogWarns`,
  `TestDoctorChecks_SessionsPipeline_ReadsSessions`, plus the existing
  all-pass test updated to expect 14.

**4. Chunk-at-import for oversized SKILL.md files**
- `internal/heimdall/skills_sync.go`:
  - `SkillBodyChunkThreshold = 6000` (chars) — well under `nomic-embed-text`'s
    8192-token context with headroom for the skill header.
  - `ChunkSkillBody(body, maxChars)` — prefers H2 splits, then H3, then
    paragraph, then hard-split on rune boundaries as last resort.
  - Chunks beyond the first stored under
    `mem:skill:disk:<slug>:part-<n>` (n is 1-based, so `part-2` is the
    second chunk; the first chunk uses the non-suffixed slug ID).
  - Each chunk keeps the full `skill: <name>\ndescription: <desc>\n\n`
    header for retrieval relevance, plus a trailing `(part N of M)` marker.
  - Orphan `part-*` rows are pruned across re-imports via a new
    `ListMemoryIDsByPrefix` + `DeleteMemoryByID` pair on `MemoryStore`.
  - `CountSyncedSkillMemories` now counts **disk-file-equivalents** (ignores
    part-* suffix rows) so `hooks doctor` sync rollup stays accurate after
    chunking.
- Tests: `TestChunkSkillBody_*` ×5,
  `TestImportSkillsFromDir_ChunksLargeSkill`,
  `TestImportSkillsFromDir_PrunesOrphanPartsWhenShrunk`.

**5. Handoff doc fix**
- Line that said "`a405b24` on top" → "`ff01aef` or newer on top" (the
  ff01aef top commit is a docs-only PR #32 refresh, so it counts).

**Suggested commit breakdown** (one PR, or split into logical PRs):
- `feat(sessions): --since filter + JSON schema_version` —
  sessions.go, sessions_test.go, hooklog_reader.go, 10-*.md table update.
- `feat(doctor): 14th check for sessions pipeline` —
  doctor.go, install_test.go, 10-*.md table update.
- `feat(skills): chunk oversized SKILL.md bodies on import` —
  skills_sync.go, skills_sync_test.go, memory_store.go.
- `docs(handoff): mark 4 polish items shipped` — 07-*.md.

All four land cleanly as independent commits; the CI gate is `go build
./... && go vet ./... && go test ./... -race -count=1`, all green at
the time of writing.

---

---

## First actions on resumption (2026-04-17, post-Wave-E)

**Binary state at session close:**
- `/home/noname/.local/bin/heimdall-mcp` → symlink → `/home/noname/Code/heimdall-mcp/heimdall-mcp`.
- File on disk is `6b7d8c4` (sessions report + user-prompt budget bump live).
  `heimdall-mcp --version` → `(6b7d8c4)`.
- 6 hooks remain installed at `/home/noname/Code/heimdall-mcp/.claude/settings.json`.

**Step 1 — confirm the restart picked up the new binary:**
```bash
heimdall-mcp --version
# expect: ...(6b7d8c4) or newer

heimdall-mcp hooks tail --event=session-start --since=5m
# expect: one line with session=<uuid-of-this-new-session>, stage=ok,
#         bullets=N, chunks=NNNN, skills=K.
# If session= is missing, the hook ran under the OLD binary — investigate.
```

**Step 2 — send a couple of real prompts + bash/edit calls, then:**
```bash
heimdall-mcp hooks tail --since=5m
# Every line except flag_parse / panic paths should carry session=<uuid>.

heimdall-mcp sessions list
# Pick the row matching this session id.

heimdall-mcp sessions report --session-id=<that id>
# Expect non-zero:
#   - Transcript tokens (input + cache_read)
#   - Tool use total + breakdown
#   - Heimdall contribution: SessionStart bytes>0 events=1
#   - UserPromptSubmit bytes>0 events=N (one per real prompt ≥8 runes)
#     — PR #31 bumped the default budget 250→450ms specifically so the
#     first uncached prompt of a session clears the Ollama first-embed
#     latency on CPU. If you see `bytes=0 events=0` for the first-turn
#     prompt, either Ollama is genuinely down or the budget regressed.
#   - prompts=N cache_hits=0..N on hook side (cache hits land on repeat queries)
#   - Guardrail verdicts map[allow:N] (N = Bash tool calls)
```

**Step 3 — if #2 looks right, the whole plan is validated.** Nothing
actionable unless you want to pick from the "What's next" menu above.

**What "healthy" looks like on first open:**
- `session-start stage=ok bullets=N chunks=2546 model=nomic-embed-text skills=K scope=`
  (the new `skills=` and `scope=` fields were added in PR #15 and #17 respectively;
  scope is empty when CWD == repo root)
- `user-prompt stage=ok hits=N skills=K bytes=B model=nomic-embed-text scope=` on every prompt
- `pre-tool-use` lines only appear when you run a `Bash` tool call. Format:
  `mode=shadow class=allow rule=... reason=...`. Exit is always 0 in shadow mode.
- `post-edit` + actor `reindex_ok` if you edit any file
- `stop` `buffer_appended` per assistant turn; `session-end session_ended` on close

**Red flags to watch for:**
- `pre-tool-use mode=shadow class=block` for a command you expect to be safe →
  false positive in the 19-rule starter set. Run
  `heimdall-mcp hooks explain-command "<cmd>"` to reproduce. Add a test case
  + rule refinement in `internal/heimdall/destructive_ops.go`.
- `guardrail classifier` warn in `hooks doctor` → primitive panic on load.
- Any retrieval hook (non-`pre-tool-use`) writes to stderr on a real fire —
  that's an OQ-5 violation.

**Known warn from `hooks doctor`:**
```
[!!] skills sync    5/7 synced — run 'heimdall-mcp skills import'
```
Two skill files in `~/.claude/skills/` exceed `nomic-embed-text`'s 8192-token
context window (`code-improvement-orchestrator/SKILL.md`,
`deep-code-review/SKILL.md`). Their import errored out with
`ollama embed: status 400: the input length exceeds the context length`. Not
blocking — the other 5 skills imported fine. Follow-ups: either (a) chunk
large skill files before embedding, (b) swap to a larger-context embed
model, or (c) accept that outlier skills stay disk-only. Track as a
section 4 follow-up in TODO.md.

**Promoting PreToolUse guardrails from shadow:**
After ≥1 day of shadow-mode telemetry with zero false-positive blocks,
promote by setting `HEIMDALL_GUARDRAILS=warn` (`block` mode is the eventual
target, but only after a false-positive audit). See the rollout section of
`docs/plans/hooks/08-destructive-op-primitive.md`.

**How to run the audit:**
`heimdall-mcp hooks audit-guardrails [--since=<dur>] [--format=text|json]`
reads `hooks.log`, filters to `event=pre-tool-use` entries, and produces a
structured report: time window, totals by class/mode, top 10 rules by
block-verdict count, the full list of shadow-mode blocks (the
false-positive candidate dataset), and a `promote_to_warn` recommendation.
Promotion flips to `true` when block count in window is zero OR the window
covers less than 24h; otherwise the top offending rule is surfaced. JSON
output includes `schema_version: "v1"` so scripts can assert compatibility.
Typical usage: `heimdall-mcp hooks audit-guardrails --since=168h --format=json`
after a week of real traffic.

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

### Late-2026-04-16 parallel push (PRs #14–#23, 10 merges, main at `65a396b`)

All 10 PRs were dispatched as parallel background agents under the user directive
"launch as many agents as you can, all to be done now." Each landed independently
with its own test suite; conflicts were resolved during merge. Final `go test
./... -race -count=1` green across all 5 packages after every merge.

**Hooks foundation / DX (5 PRs):**
- **PR #14** `fix(cli)`: `internal/cli/testmain_test.go` redirects `HEIMDALL_HOOK_LOG`
  to a per-binary temp dir. Closes long-running caveat #7 — unit tests no
  longer pollute `~/.local/state/heimdall/hooks.log`.
- **PR #15** `feat(hooks)`: surface top-N `type=skill` memories in both
  SessionStart and UserPromptSubmit blocks. New helper
  `internal/cli/hook_skills.go::surfaceRelevantSkills()`. Section omitted
  when no skills match. Respects existing 2 s / 250 ms budgets.
  Test helper isolates memory store per test.
- **PR #16** `test(hooks)`: Layer-2 integration tests (`//go:build integration`,
  `internal/cli/integration_test.go` — 4 cases using `os/exec` + fake Ollama)
  and Layer-3 e2e harness (`//go:build e2e`, gated by `HEIMDALL_E2E_CLAUDE=1` +
  `claude` on PATH, skips cleanly otherwise). Makefile targets `test-integration`,
  `test-e2e`, `test-all`.
- **PR #17** `feat(hooks)`: CWD scope filter + auto-detect memory path.
  `internal/heimdall/scope.go::FindRepoRoot` + `ComputeScope`; shared by CLI
  and MCP. Session/UserPrompt hooks pass `scope=<relpath>` when CWD is a
  subpath of repo root. `heimdall_remember` auto-fills `context_path` from
  MCP request CWD. Cache key for UserPrompt now includes scope (subpath vs
  root get different cache rows).
- **PR #19** `feat(install)`: pre-warm Ollama after `install-hooks` with a
  5 s-bounded `EmbedForHook` call. `--no-prewarm` to skip. Skips on
  `--dry-run` or empty `cfg.Model`. Best-effort — install never fails on
  prewarm error. Removes the ~67 s first-turn cold start that was
  burning the first real PostToolUse.

**Phase 3 guardrails (design + impl, 2 PRs):**
- **PR #18** `docs(hooks)`: `docs/plans/hooks/08-destructive-op-primitive.md`
  (499 lines). Recommends 3-level classification (`allow` / `warn` / `block`),
  static Go rules as v1, shadow→warn→block rollout. Justifies static rules
  over YAML and LLM judgment.
- **PR #23** `feat(hooks)`: full Phase 3 implementation per the design.
  - `internal/heimdall/destructive_ops.go` — 19-rule classifier (`ClassifyBashCommand`),
    allowlist-first precedence (`git push --force-with-lease` beats generic
    `--force` block). `DestructiveRuleCount()` asserts against drift. 80
    classifier test assertions.
  - `internal/cli/hook_pre_tool_use.go` — new hook handler, honors
    `HEIMDALL_GUARDRAILS=shadow|warn|block|off`, `HEIMDALL_HOOKS=0`, and
    `<project>/.heimdall/hooks.disabled`. Default `shadow`. Exit 2 + stderr
    ONLY in `block` mode on `ClassBlock`.
  - `internal/cli/hooks_explain_test.go` — `heimdall-mcp hooks explain-command
    "<cmd>"` CLI for dry-running the classifier.
  - `internal/cli/install.go` — 6-hook template, envelope bumped to `wave2-phase3`,
    auto-upgrade path (PR #13) picks this up cleanly.
  - `internal/cli/doctor.go` — 13th check (`guardrail classifier: 19 rules loaded`).
  - ~105 new test cases total.

**TODO follow-ups closed (3 PRs):**
- **PR #20** `feat(skills)`: two-way sync with `~/.claude/skills/`.
  - Inbound: `heimdall-mcp skills import [--dir PATH] [--dry-run] [--format text|json]`
    walks `<dir>/<name>/SKILL.md`, parses YAML frontmatter (stdlib only, no deps),
    upserts `type=skill` memory with deterministic ID `mem:skill:disk:<slug>`.
    Idempotent via `ContentHash`.
  - Outbound: `heimdall_remember --type=skill --write_file=true` writes
    `<skills-dir>/<slug>/SKILL.md`. Default OFF.
  - Doctor: 12th check `skills sync` reports drift; warn, not fail.
  - `HEIMDALL_CLAUDE_SKILLS_DIR` env override isolates tests from real
    user state. 35 new test cases.
- **PR #21** `chore`: gitignore `.claude/` + `.idea/`; fix stale "phase-2
  destructive-op hook" references across `01-architecture.md`,
  `02-cli-surface.md`, `08-destructive-op-primitive.md`; extend OQ-5 in
  `06-decisions.md` with a guardrail-hook sibling clause (PreToolUse may
  exit 2 + stderr ONLY on `block` mode + `ClassBlock`).
- **PR #22** `feat(bench)`: `cmd/bench-retrieval` + `docs/plans/hooks/09-tiered-retrieval-benchmark.md`.
  Measured: summary 884 / snippet 1101 / full 4012 mean tokens per query.
  **62.4% saving vs full at expand-rate=0.2.** Makefile `bench` +
  `bench-test` targets. Tokenizer-agnostic (chars/4 approximation, constant
  factor cancels). Read-only snapshot of the production DB — never mutates.

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
   session-start`, `hook post-edit`, `hook post-edit-actor`, `hook user-prompt`,
   `hook stop`, `hook session-end`, and even `DispatchHook`'s usage-error paths).
   **Guardrail hooks (PreToolUse) — added in Phase 3 (PR #23)** — may exit 2 +
   write one stderr line, but ONLY when `HEIMDALL_GUARDRAILS=block` AND
   classification is `block`. Shadow, warn, off, allow, timeout, and error
   cases all exit 0 with no stderr. Interactive commands (`install-hooks`,
   `uninstall-hooks`, `hooks doctor`, `hooks tail`, `hooks cache-clear`,
   `hooks cache-stats`, `hooks explain-command`, `recall`, `ingest-session`,
   `status`, `skills import`) use `0 / 1 / 2`. See
   `docs/plans/hooks/06-decisions.md` for the full sibling clause and
   `docs/plans/hooks/08-destructive-op-primitive.md` for the classification
   contract.

---

## Next steps (in order)

1. ~~Phase 1a dogfood.~~ **✅ VERIFIED 2026-04-16.**
2. ~~Soak gate.~~ **❌ KILLED.**
3. ~~Phase 1b (T4 + T6).~~ **✅ SHIPPED** — PR #10.
4. ~~Phase 2 (T8 + sections 2–5).~~ **✅ SHIPPED** — PR #11.
5. ~~Auto-upgrade hooks on SessionStart.~~ **✅ SHIPPED** — PR #13.
6. ~~Stop + SessionEnd dogfood.~~ **✅ VERIFIED 2026-04-16** — stop `buffer_appended`
   and session-end `session_ended` both fired in real session.
7. ~~Phase 3 guardrails design.~~ **✅ SHIPPED** — PR #18 (design) + PR #23 (impl).
8. ~~Phase 3 implementation.~~ **✅ SHIPPED** — PR #23, shadow mode default.
9. ~~Token-savings measurement.~~ **✅ SHIPPED** — PR #22, 62.4% saving at
   20% expand rate.
10. ~~Two-way `~/.claude/skills/` sync.~~ **✅ SHIPPED** — PR #20.
11. ~~Ollama pre-warm.~~ **✅ SHIPPED** — PR #19.
12. ~~CWD scope filter + memory path auto-detect.~~ **✅ SHIPPED** — PR #17.
13. ~~Skills in SessionStart/UserPromptSubmit.~~ **✅ SHIPPED** — PR #15.
14. ~~T20 integration + T23 e2e harness.~~ **✅ SHIPPED** — PR #16.
15. ~~Test-log isolation.~~ **✅ SHIPPED** — PR #14.

**Remaining (all require live sessions or telemetry, not dispatchable work):**

16. **Dogfood the full 6-hook pipeline.** Especially PreToolUse in shadow mode.
    Collect `hooks tail --event=pre-tool-use` for a session or two. Audit for
    false-positive `class=block` verdicts. Only after an audit with zero
    false positives, promote to `HEIMDALL_GUARDRAILS=warn`.

17. ~~**T21 with-vs-without comparison.**~~ **SUPERSEDED by `heimdall-mcp
    sessions report`** (PR #28, Wave D of
    `docs/plans/hooks/10-per-session-savings-report.md`). The report
    command joins the Claude Code transcript (tokens, tool-call counts,
    hook_success attachment bytes) with `hooks.log` (cache hits, guardrail
    verdicts, reindex counts) keyed on `session_id` — i.e. the T21 metrics
    surface automatically on every real session, no dedicated with-vs-without
    runs required. Run `heimdall-mcp sessions list && heimdall-mcp sessions
    report --session-id=<latest>` after any session to see the numbers.

18. ~~**Re-embed the oversized skill files.**~~ ✅ SHIPPED 2026-04-17 —
    chunk-at-import is the chosen option. `ChunkSkillBody` pre-splits any
    body exceeding `SkillBodyChunkThreshold` (6000 chars) on H2/H3/paragraph
    boundaries with a hard-split fallback. `code-improvement-orchestrator`
    (26 KB) and `deep-code-review` (75 KB) now import as multiple
    `mem:skill:disk:<slug>:part-<n>` rows per file. Run `heimdall-mcp skills
    import` once after the restart to materialize the new rows.

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
   turn won't get heimdall context. Restart once Ollama is up. Note:
   `install-hooks` now fires a 5s-bounded `EmbedForHook` warm-up against
   the configured model after a successful install (and on `--force`),
   removing the ~67 s first-turn cold start observed in the dogfood. Pass
   `--no-prewarm` to skip.

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

7. ~~Unit tests in `internal/cli` write to the real
   `$XDG_STATE_HOME/heimdall/hooks.log`.~~ **✅ FIXED in PR #14**
   (merged 2026-04-16). `internal/cli/testmain_test.go` redirects
   `HEIMDALL_HOOK_LOG` to a per-binary temp dir for the whole package;
   per-test `t.Setenv` still overrides.

8. **Two skills too long for nomic-embed-text's 8192-token context.**
   `~/.claude/skills/code-improvement-orchestrator/SKILL.md` and
   `~/.claude/skills/deep-code-review/SKILL.md` both failed
   `skills import` with `status 400: the input length exceeds the context
   length`. Other 5 skills imported fine. See the "Known warn from hooks
   doctor" note above. Not blocking.

9. **Background-agent leakage into the main checkout.** Several of
   today's parallel agents (#5, #6, #4) touched files in
   `/home/noname/Code/heimdall-mcp/` despite being spawned in worktrees.
   Root cause was `cd` drifting out of the worktree. Each agent cleaned
   up after itself; no changes leaked into merged PRs. Mitigation in
   future: prompts already include "never cd to main checkout — use
   absolute paths rooted at the worktree." Still worth watching.

---

## What's next — post 2026-04-18

All numbered Phase 1/2/3 items and every TODO section follow-up that could
be shipped without a live Claude Code session are merged. 11 PRs landed
2026-04-18 (#39–#49). Everything remaining requires either design resolution
(plan 11 §9 / plan 12 §9 open questions) or accumulated real-session
telemetry.

**Priority work (in order — see "Opening prompt" for the call-to-action):**
1. **`bench-retrieval` auto-scope verification** — trivial follow-up to PR #41.
2. **Smoke-harness re-run** — `heimdall-mcp hooks smoke --fake-ollama` after
   any future hook change (PR #47).
3. **Semantic-drift metric implementation** per
   `docs/plans/hooks/12-semantic-drift-metric.md` (PR #46 design). Prereq:
   resolve §9 open questions, especially OQ-3 (calibration harness).
4. **LLM classifier fallback implementation** per
   `docs/plans/hooks/11-llm-classification-fallback.md` (PR #40 design).
   Prereq: resolve §9 open questions, especially the model-recommendation
   bake-off. ClassUnknown unblock shipped in PR #45.
5. **Shadow-mode guardrail audit** — wait ≥1 week of real traffic, then run
   `heimdall-mcp hooks audit-guardrails --since=168h --format=json` (PR #44).
   Flip default `shadow → warn` if report recommends promotion with zero
   false-positive candidates.
6. **`cmd/calibrate-drift` harness** — gates Stage 2→3 of semantic-drift
   rollout per plan 12.
7. ~~**Cache-hit ratio consolidation**~~ — ✅ Resolved: investigation
   confirmed the two fields share the same underlying counter (no
   semantic split). The top-level `user_prompt_cache_hits` /
   `user_prompt_cache_total` pair is canonical;
   `heimdall_contribution.cache_hits` is retained as a deprecated
   back-compat alias with a `// Deprecated:` comment in `sessions.go`.
   `TestSessionsReport_CacheHitsAliasMatchesTopLevel` asserts the
   invariant. Schema stays `v1`; alias can drop on a future v2.

**Manual / dogfood-only (not dispatchable work):**
- **Dogfood the 6-hook pipeline on reopen.** First-actions block at the top
  of this file has the tail commands and expected output.
- **UserPromptSubmit cache-hit ratio over sessions.** Surfaced now via
  `sessions report` top-level counters (PR #43); track the ratio over weeks.

**Closed TODO follow-ups (all merged):**
- ~~Auto-surface skills in SessionStart/UserPromptSubmit hooks~~ → PR #15
- ~~Memory path auto-detect for `context_path`~~ → PR #17
- ~~Hook scope filtering by CWD subpath~~ → PR #17
- ~~Token-savings measurement for tiered retrieval~~ → PR #22
- ~~Two-way sync with `~/.claude/skills/`~~ → PR #20
- ~~Phase 3 guardrails (design + impl)~~ → PR #18 + PR #23
- ~~T20 Layer 2 integration tests~~ → PR #16
- ~~T23 Layer 3 e2e harness~~ → PR #16
- ~~Ollama pre-warm on install~~ → PR #19
- ~~Gitignore `.claude/settings.json`~~ → PR #21
- ~~Chunk-at-import for oversized SKILL.md files~~ → PR #33 (Wave F)
- ~~Fold `skills import` into `install-hooks`~~ → PR #42
- ~~Auto-scope `bench-retrieval` to CWD's project~~ → PR #41
- ~~LLM-based classification fallback design~~ → PR #40 (design only; impl open)
- ~~Dedup `part-*` skill chunks in hook context~~ → PR #36
- ~~Post-edit actor session attribution~~ → PR #37
- ~~Redundant-heimdall-call counter in `SessionReport`~~ → PR #39
- ~~ClassUnknown default fall-through for guardrails~~ → PR #45
- ~~`audit-guardrails` subcommand~~ → PR #44
- ~~Hooks smoke-test harness (`hooks smoke --fake-ollama`)~~ → PR #47
- ~~Stop/SessionEnd event-key double-stamp bug~~ → PR #48
- ~~`hooks.log` parser/producer quote-aware round-trip~~ → PR #49

---

## File layout quick reference

```
cmd/
  heimdall-mcp/
    main.go                — CLI + MCP server entry
  bench-retrieval/
    main.go                — tiered-retrieval token-savings benchmark (PR #22)
    bench_test.go          — `//go:build bench` smoke test

internal/
  cli/
    cli.go                 — RunCLI dispatcher, wires every subcommand
    hook.go                — DispatchHook (retrieval) + HookSessionStart +
                             auto-upgrade (PR #13). Injects skills (PR #15),
                             passes scope (PR #17).
    hook_skills.go         — surfaceRelevantSkills helper (PR #15)
    hook_skills_test.go    — 16 tests for skills helper
    hook_test.go           — tests for session-start (extended by PR #15, #17)
    hook_post_edit.go      — HookPostEdit (foreground + spawn wrapper)
    hook_post_edit_actor.go — runPostEditActor (pure testable actor core)
    hook_post_edit_test.go — 20 tests for post-edit + actor
    hook_user_prompt.go    — HookUserPrompt (phase 1b, cache-first hot path,
                             scope + skills integrated PR #15/#17)
    hook_user_prompt_test.go — tests for user-prompt hook
    hook_stop.go           — HookStop + HookSessionEnd
    hook_stop_test.go      — 12 tests for stop + session-end
    hook_pre_tool_use.go   — HookPreToolUse (Phase 3 guardrail, PR #23)
    hook_pre_tool_use_test.go — 18 tests for shadow/warn/block modes
    hooks.go               — DispatchHooks (admin: tail/cache-clear/
                             cache-stats/doctor/explain-command)
    hooks_explain_test.go  — 7 tests for `hooks explain-command` CLI (PR #23)
    hooks_test.go          — admin-commands tests
    scope.go               — package-local wrappers for heimdall/scope.go (PR #17)
    install.go             — CLIInstallHooks + Uninstall + prewarm (PR #19).
                             Template now lists 6 hooks (envelope `wave2-phase3`).
    install_test.go        — 35+ install/uninstall/doctor/prewarm tests
    doctor.go              — HooksDoctor, 13 checks including guardrail
                             classifier + skills-sync
    testmain_test.go       — TestMain redirects HEIMDALL_HOOK_LOG (PR #14)
    skills.go              — CLIImportSkills (PR #20)
    skills_test.go         — 11 tests for skills CLI
    recall.go              — CLIRecall
    ingest_session.go      — CLIIngestSession
    status.go              — CLIStatus
    integration_test.go    — `//go:build integration` (PR #16)
    e2e_test.go            — `//go:build e2e` (PR #16)
    eta_test.go            — 6 tests for computeETA (DES-010)
    testdata/
      hook_session_start.golden.md — SessionStart block template
      status.golden.json   — status --format=json golden
      skills/              — skills-sync fixtures (PR #20)

  heimdall/
    verify.go              — VerifyHookIndex + sentinel errors (T9)
    hook_cache.go          — hook_cache table + index_version bump (T10)
    ollama.go              — EmbedForHook (T11)
    suppress.go            — ShouldEmitTierB (T12)
    hooklog.go             — LogHookEvent + rotation + redaction (T13)
    hookgate.go            — HooksDisabled (T22)
    dbpath.go              — NormalizeModelName, ResolveUsableModelDB,
                             ModelDBDir, DiscoverSubRepos
    scope.go               — FindRepoRoot + ComputeScope (PR #17)
    scope_test.go          — 8 tests for scope helpers
    destructive_ops.go     — ClassifyBashCommand + 19-rule ruleset (PR #23)
    destructive_ops_test.go — 80 classifier assertions
    skills_sync.go         — import + write-back primitives (PR #20)
    skills_sync_test.go    — 17 tests for skills sync
    cli_core.go            — RunRecall, IngestSessionSummary,
                             ReadLengthPrefixedBuffer. `RunRecall` accepts scope.
    cli_status.go          — GatherStatus (shared by CLI + MCP)
    memory.go / memory_store.go — memory persistence, now with
                             `Memory.ContextPath`, `MemoryFilter.ContextPath`
                             prefix filter, `SearchMemoriesByIDPrefix`.

  mcp/
    server.go              — MCP server. `heimdall_remember` schema extended
                             for skills write-back + context_path auto-detect.
    memory_tools.go        — toolRemember + toolRecall + deriveMemoryContextPath
    skills_writeback_test.go — 7 tests for outbound sync (PR #20)
    types.go               — MCP input/output types. `rememberInput` has
                             `context_path`, `skill_name`, `skill_description`,
                             `write_file`, `write_file_overwrite`.

docs/plans/hooks/
  00-consolidated-plan.md  — authoritative implementation plan
  01-architecture.md       — hook event contracts (phase-3 labels updated PR #21)
  02-cli-surface.md        — CLI command design (phase-3 labels updated PR #21)
  03-latency.md            — measured latency budgets
  04-failure-modes.md      — Tier A/B/C failure matrix + install-time
  05-testing-rollout.md    — test strategy + phased rollout + Layer 2/3 (PR #16)
  06-decisions.md          — OQ-1..OQ-5 locked answers (OQ-5 extended PR #21)
  07-next-session-handoff.md — THIS FILE
  08-destructive-op-primitive.md — Phase 3 design doc (PR #18)
  09-tiered-retrieval-benchmark.md — bench methodology + 62.4% result (PR #22)
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

## Final git state at session end (2026-04-18 evening)

```
$ git log --oneline -12
1b9e92e fix(hooks): quote-aware parser + lossless producer for hooks.log values (#49)
18e217f fix(hooks): drop redundant `event` key from Stop/SessionEnd kv maps (#48)
e5a738d feat(hooks): add `hooks smoke` end-to-end harness for all 6 hooks (#47)
b039c9c docs(hooks): semantic-drift metric design for missed tool-call opportunities (#46)
1bed701 feat(guardrails): introduce ClassUnknown for default fall-through (#45)
7af921c feat(hooks): add audit-guardrails subcommand for PreToolUse shadow-mode review (#44)
8808e99 feat(sessions): add user-prompt cache-hit counters to SessionReport (#43)
c4a3383 feat(install): run skills import as best-effort post-install step (#42)
e96fe10 feat(bench): auto-scope bench-retrieval to CWD's repo (#41)
c920625 docs(hooks): design doc for LLM classification fallback (#40)
f205a21 feat(sessions): add redundant_heimdall_calls to SessionReport (#39)
d231d92 docs(handoff): refresh for Wave F (2026-04-17 late) (#38)
```

Main is **in sync with `origin/main`** at `1b9e92e`. All 11 of 2026-04-18's
PRs (#39–#49) merged. Binary at `/home/noname/.local/bin/heimdall-mcp`
rebuilt and reports `(1b9e92e)`. Hooks installed at
`/home/noname/Code/heimdall-mcp/.claude/settings.json` (6 hooks, template
envelope `wave2-phase3`).

**Stale worktrees** — prior `agent-*` worktrees from earlier parallel
pushes may still be in `git worktree list`. Harmless, excluded from
indexing. Prune when you feel like it:
```bash
for d in .claude/worktrees/agent-*; do
  git worktree remove --force "$d"
done
```
(Each held a merged branch with a local lock — may need
`git worktree remove -f -f` for locked ones.)

**Open known-unknowns (ordered by urgency):**

1. ~~SessionStart, PostToolUse, Stop, SessionEnd fire?~~ **✅ verified.**
2. **PreToolUse shadow-mode telemetry on real sessions.** Run
   `heimdall-mcp hooks audit-guardrails --since=168h` (PR #44) after a week
   of traffic and inspect the report's `promote_to_warn` recommendation.
3. **UserPromptSubmit cache-hit ratio over sessions.** Surfaced in
   `sessions report` JSON as top-level `user_prompt_cache_hits` /
   `user_prompt_cache_total` (PR #43). Track over weeks.
4. **Semantic-drift metric (plan 12 / PR #46).** Not yet implemented —
   prerequisite §9 open questions need resolution first.
5. **LLM classifier fallback (plan 11 / PR #40).** Not yet implemented —
   ClassUnknown prereq shipped as PR #45; §9 open questions still outstanding.
6. **Cache-hit ratio consolidation (cosmetic).** See priority-list item 7
   in the Opening prompt.
7. ~~Should the post-edit actor pre-warm Ollama on install?~~ **✅ done.**
   `install-hooks` does a best-effort `EmbedForHook` warm-up (5s timeout,
   `--no-prewarm` to skip) after a successful install or `--force` re-install.
   Auto-upgrade SessionStart intentionally skips its own prewarm — the
   recall step that runs immediately after warms the model anyway, and a
   redundant call would eat the 2s budget.
8. ~~Skills auto-import on `install-hooks`?~~ **✅ done** — PR #42. Runs as
   a best-effort step; `--no-skills-import` to skip.
