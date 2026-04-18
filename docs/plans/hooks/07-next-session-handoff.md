# 07 — Next Session Handoff

**Purpose:** everything needed to resume heimdall-mcp work in a fresh Claude Code
session without re-reading the prior-session conversation. Paste the
"Opening prompt for the next session" below into the new session, or just
point at this file.

---

## Opening prompt for the next session

> **Status at `main` @ commit `c3d28ca` (2026-04-18 evening):** clean
> tree, **7 more PRs merged this session (#52–#58)** on top of the
> earlier 2026-04-18 afternoon batch (#39–#49). Full suite green under
> `go test ./... -race -count=1`. `go vet ./...` clean.
> `heimdall-mcp hooks smoke --fake-ollama` → **6/6 PASS**. Binary at
> `/home/noname/.local/bin/heimdall-mcp` → symlink →
> `/home/noname/Code/heimdall-mcp/heimdall-mcp`, reports `(c3d28ca)`
> after rebuild. **First action: rebuild, then pick an item from the
> priority list below. Every non-time-gated item from the prior
> handoff's punch list has shipped — the remaining work is telemetry
> collection, calibration-gated rollouts, and human sign-off on seven
> flagged product decisions (see §Human sign-off queue).**
>
> **Priority punch list (pick in order, none are blocking each other):**
>
> 1. **Dogfood Plan 11 Stage 0 (LLM classifier fallback).** PR #57
>    shipped the opt-in classifier — default OFF. Flip
>    `HEIMDALL_LLM_CLASSIFIER=1` in a shadow-mode shell, run real
>    sessions, and collect `class=unknown → llm:<class>` fires in
>    `hooks.log`. Per 11a OQ-1, build a ≥100-entry shadow-trace eval
>    set to validate the `llama3.2:3b` primary pick vs. the
>    `qwen2.5-coder:3b` fallback. No code change expected unless the
>    model pick needs revising.
>
> 2. **Start Plan 12 Stage 1 data collection.** PR #55 landed the
>    log-only keys (`hit_ids` + `prompt_embed_b64` on every
>    UserPromptSubmit `stage=ok`). Every real prompt from now on
>    contributes to the calibration sample. The draw window opens
>    ~**2026-05-02** (14 days after #55). No action now — just let
>    traffic accumulate.
>
> 3. **Verify `cmd/calibrate-drift` against the 5-turn fixture.**
>    PR #56 shipped the T1×T2 threshold-sweep harness with
>    deterministic tests; sanity-check it manually against the
>    fixture today. The real 100-turn stratified sample isn't
>    draw-able for two weeks.
>
> 4. **Shadow-mode guardrail audit** — blocked on calendar. Run
>    `heimdall-mcp hooks audit-guardrails --since=168h --format=json`
>    around **2026-04-25** (one week after the afternoon batch). If
>    zero false-positive candidates, flip default
>    `HEIMDALL_GUARDRAILS=shadow → warn`.
>
> 5. **Follow-up PRs identified this session** (see §Follow-up PRs):
>    - 3-line `calibrate-drift` refactor: switch direct
>      `store.DB().QueryRow` to `store.VectorByID(id)`.
>    - Plan 11 §5.5 F5 `model_missing` logging: wire into
>      `hooks doctor` once that check lands.
>    - README guardrails paragraph: one-paragraph note on the LLM
>      fallback with a link to `docs/reference/llm-classifier.md`.
>
> 6. **Human sign-off queue (7 items).** Until these are decided,
>    Stage 3 of both Plan 11 and Plan 12 stays parked. See the
>    §Human sign-off queue table below.
>
> **This session's shipped PRs (afternoon → evening, 2026-04-18):**
> Morning/afternoon batch #39–#49 (prior handoff, for context).
> Evening batch (covered here):
> - **#52** `chore(sessions): consolidate cache-hit counters + deprecate
>   nested alias` (`daae783`) — the two fields shared the same
>   underlying counter; consolidated with nested alias marked
>   `// Deprecated:`.
> - **#53** `docs(hooks): design decisions for plan 11 (LLM classifier
>   fallback)` (`e6b2490`) — 11a decision doc, 6/8 OQs resolved,
>   picks `llama3.2:3b` primary + `qwen2.5-coder:3b` fallback, flags
>   HD-1/HD-2/HD-3.
> - **#54** `docs(hooks): design decisions for plan 12 (semantic-drift
>   metric + calibrate-drift)` (`6930ad4`) — 12a decision doc, 10/10
>   OQs resolved, OQ-3 deep-dive specifies the calibration harness,
>   flags 4 §6 items for human sign-off.
> - **#55** `feat(hooks): log hit_ids + prompt_embed_b64 on user-prompt
>   stage=ok` (`18b4f81`) — Plan 12 Stage 1 logging, additive, no
>   schema bump.
> - **#56** `feat(calibrate-drift): T1xT2 threshold-sweep harness for
>   semantic-drift metric` (`b3524c4`) — `cmd/calibrate-drift` binary,
>   10 tests, deterministic.
> - **#57** `feat(guardrails): plan 11 stage 0 — LLM classifier fallback
>   (opt-in)` (`9f345a8`) — full Stage 0 code, default-off guarded by
>   explicit test, 27 new tests. `HEIMDALL_LLM_CLASSIFIER=1` +
>   `cfg.LLMClassifierModel` opts in.
> - **#58** `feat(sessions): plan-12 stage-2 semantic-drift compute +
>   JSON/text wire-up` (`c3d28ca`) — `semantic_drift.go` + sessions.go
>   wiring, 24 drift tests + 8 siblings. `--verbose` flag, reserved
>   `--scope-aware`.
>
> **Prior merged work (reference only, don't re-litigate):**
> 2026-04-18 afternoon shipped PRs #39–#49 (Wave F follow-ups,
> `bench-retrieval` auto-scope, `audit-guardrails`, `ClassUnknown`,
> Plan 11/12 design docs, `hooks smoke`, `hooks.log` quote-aware
> parser, event-key dedup). Earlier Wave F (2026-04-17) shipped PRs
> #33–#37; Waves A–E (PRs #25–#29) shipped the per-session savings
> feature (`docs/plans/hooks/10-per-session-savings-report.md`).
> PR #31 bumped the first-turn `UserPromptSubmit` budget 250→450ms.
>
> **Heimdall installs 6 hooks**: `SessionStart`,
> `PostToolUse(Edit|Write)`, `UserPromptSubmit`, `Stop`, `SessionEnd`,
> `PreToolUse(Bash)`. Every retrieval-hook fire carries
> `session=<uuid>` in `hooks.log` (Wave A). PreToolUse guardrail
> default is **shadow** — classifier runs, never blocks. With
> `ClassUnknown` (PR #45), the default fall-through for commands no
> rule matches is also allow-equivalent. Toggle the mode via
> `HEIMDALL_GUARDRAILS=shadow|warn|block|off`. **Plan 11 Stage 0
> (LLM classifier fallback) is installed but opt-in — default OFF;
> set `HEIMDALL_LLM_CLASSIFIER=1` with `cfg.LLMClassifierModel` to
> enable.**
>
> **Measured wins already on the table:**
> - 62.4% token savings from tiered retrieval at 20% expand rate
>   (`docs/plans/hooks/09-tiered-retrieval-benchmark.md`, `make bench`).
> - Per-session savings are queryable post-hoc via
>   `heimdall-mcp sessions report --session-id=<id>` — joins
>   transcript (tokens, tool calls, hook_success bytes) with hooks.log
>   (cache hits, guardrail verdicts, reindex counts). `--format=json`
>   emits `schema_version: "v1"`. Session-end JSON includes
>   `redundant_heimdall_calls` (#39), top-level
>   `user_prompt_cache_hits` / `user_prompt_cache_total` (#43), and
>   (Plan 12 Stage 2, #58) `tool_use.semantic_drift` block when
>   `--verbose`.
> - Plan 12 Stage 1 log size budget: **~2 MB over a 14-day soak**
>   (well under the shared 5 MB `hooks.log` cap with 1 rotation). If
>   rotation starts firing >1×/14 days, revisit (12a §6 item 2).
> - `heimdall-mcp hooks smoke --fake-ollama` (#47) validates the full
>   6-hook pipeline after a change; expect "PASS" on all six events.
>
> Before any code edit, sanity-check:
> ```bash
> cd /home/noname/Code/heimdall-mcp
> git status && git log --oneline -15
> go build ./... && go vet ./... && go test ./... -race -count=1
> heimdall-mcp hooks smoke --fake-ollama   # 6/6 PASS
> ```
> Expect: clean tree on top of `c3d28ca`. All tests pass.

---

## 2026-04-18 evening session — what shipped (PRs #52–#58)

Seven PRs closing every non-time-gated item from the prior handoff
(#39–#49 punch list items 1–7). All merged to `main`, commit range
`3701954..c3d28ca`.

**Investigation / consolidation (1 PR):**
- **#52 `chore(sessions): consolidate cache-hit counters + deprecate
  nested alias`** — closed priority-list item 7 ("cache-hit ratio
  consolidation"). Investigation confirmed `heimdall_contribution.cache_hits`
  and the top-level `user_prompt_cache_hits` read from the same
  `SessionHookAggregate.UserPromptCacheHits` counter — no semantic
  split. Top-level pair (with companion `user_prompt_cache_total`
  denominator) is canonical; nested alias kept for back-compat with
  a `// Deprecated:` marker. New
  `TestSessionsReport_CacheHitsAliasMatchesTopLevel` asserts the two
  never diverge. Schema stays `v1`; alias can drop on v2.

**Design decisions (2 PRs):**
- **#53 `docs(hooks): design decisions for plan 11 (LLM classifier
  fallback)`** — `11a-design-decisions.md` (574 lines). Resolves 6/8
  of plan 11's open questions; 2 deferred to v2 with rationale.
  Model bake-off: `llama3.2:3b` primary (larger context + better
  adherence to JSON-schema `format`), `qwen2.5-coder:3b` fallback
  (code-aware, tighter semantic match on bash idioms). Flags three
  cross-cutting human decisions (HD-1 FP rate, HD-2 auto-pull UX,
  HD-3 prompt-version bump).
- **#54 `docs(hooks): design decisions for plan 12 (semantic-drift
  metric + calibrate-drift)`** — `12a-design-decisions.md` (895 lines).
  All 10 OQs resolved. OQ-3 expanded into a full harness design:
  `cmd/calibrate-drift` sweeps the T1 (hit-miss distance) × T2
  (drift threshold) plane against an operator-labeled 100-turn
  stratified sample. Flags four §6 items for human sign-off
  (see §Human sign-off queue).

**Feature code (4 PRs):**
- **#55 `feat(hooks): log hit_ids + prompt_embed_b64 on user-prompt
  stage=ok`** — Plan 12 Stage 1. Additive keys on every successful
  `user-prompt` event in `hooks.log`. No schema bump (12a §4.3). Size
  budget: ~2 MB / 14-day soak. Data collection window opens now,
  draws at ~2026-05-02.
- **#56 `feat(calibrate-drift): T1xT2 threshold-sweep harness for
  semantic-drift metric`** — new `cmd/calibrate-drift` binary.
  Deterministic, 10 unit tests, emits a threshold-sweep JSON report
  for Stage 2→3 gate evaluation. Ships alongside Stage 2 compute (#58)
  so both land before the calibration window opens.
- **#57 `feat(guardrails): plan 11 stage 0 — LLM classifier fallback
  (opt-in)`** — full Stage 0 code path. Default OFF: guarded by the
  explicit `HEIMDALL_LLM_CLASSIFIER=1` env plus a non-empty
  `cfg.LLMClassifierModel`. 27 new tests covering: env gating, model
  gating, timeout, schema-validation failure → `ClassAllow` + WARN,
  budget blown → `ClassAllow` + WARN, happy path → `llm:<class>`
  label stamped into `hooks.log`. A default-off invariant test
  ensures we never accidentally flip the switch.
- **#58 `feat(sessions): plan-12 stage-2 semantic-drift compute +
  JSON/text wire-up`** — `internal/cli/semantic_drift.go` +
  `sessions.go` wiring. Computes per-turn drift from the logged
  `prompt_embed_b64` vectors against the UserPromptSubmit hit vectors
  in the store. New `--verbose` flag surfaces the `semantic_drift`
  block in both text and JSON output. Reserved `--scope-aware` flag
  for a follow-up that joins drift with the scope cache key
  (per 12a OQ-5). 24 drift-specific tests + 8 sibling coverage tests.

**Verification:**
- Full suite green under `-race -count=1` at `c3d28ca`.
- `hooks smoke --fake-ollama` → 6/6 PASS.
- `bench-retrieval` auto-scope (PR #41, prior session) re-verified
  at session start.
- `hooks smoke` harness (PR #47, prior session) re-verified at
  session start.

---

## Follow-up PRs (identified this session, not yet scheduled)

Small, independent, none of them gate any of the priority work.

| # | PR idea | Size | Notes |
|---|---|---|---|
| F1 | `refactor(calibrate-drift)`: switch `store.DB().QueryRow` → `store.VectorByID(id)` | ~3 lines | Agent G noted during #58 review; now unblocked because #58 merged. Pure refactor, same behavior. |
| F2 | `feat(doctor)`: wire Plan 11 §5.5 F5 `model_missing` logging into `hooks doctor` | ~1 check | Agent E deferred during #57 scope review. Surfaces a clean hint when `HEIMDALL_LLM_CLASSIFIER=1` is set but the configured model isn't pulled. |
| F3 | `docs(readme)`: add one paragraph on LLM classifier fallback + link to `docs/reference/llm-classifier.md` | ~5 lines | Intentionally out-of-scope for #57. |

---

## Human sign-off queue

All seven items are flagged in the design docs. None are resolvable
by the agent alone. Until they clear, Stage 3 of Plan 11 and Stage 3
of Plan 12 remain parked (this is by design — the agent is not
allowed to pick the FP rate or commit operator hours to labeling).

| ID | Topic | Recommendation (from design doc) | Source |
|---|---|---|---|
| **11a HD-1** | Stage 2→3 false-block-rate threshold | 5 % FP on `would_have=block` LLM verdicts, last-100-fires denominator, operator-discretion rollback | `docs/plans/hooks/11a-design-decisions.md` §4 |
| **11a HD-2** | Auto-pull UX on first `HEIMDALL_LLM_CLASSIFIER=1` | Never auto-pull; surface the `ollama pull <model>` hint via plan 04 Tier-B suppressor | 11a §4 |
| **11a HD-3** | `prompt_version` bump policy | Bump on any character change that is not pure whitespace or comment; pair with a CHANGELOG entry | 11a §4 |
| **12a §6 item 1** | Operator labeling commitment (~5 h for 100 turns) | Commit at Stage 2 shipping point. If hard no, defer Stage 3 indefinitely (v1+v2 co-exist forever — not a correctness problem) | `docs/plans/hooks/12a-design-decisions.md` §6 |
| **12a §6 item 2** | Stage 1 log-cap sharing (hit_ids + prompt_embed_b64) | Ship now. Re-litigate if `hooks.log` rotates >1×/14 days | 12a §6 |
| **12a §6 item 3** | Embedding-model migration ownership | Post-v2 follow-up: `embedding_model_version` in `store_metadata` + pre-flight warn in `sessions report` | 12a §6 |
| **12a §6 item 4** | `heimdall_non_search_when_hits_present` placement | Inside `semantic_drift` under a `companion_counters` sub-map — keeps drift block self-contained and makes the "companion, not semantic" distinction explicit via structure | 12a §6 |

When the operator replies, update the relevant design doc to record
the decision, then file the implementation PR.

---

## Calendar reminders

| Date | Action | Source |
|---|---|---|
| **2026-04-25** (~1 week after #39–#49 batch) | Run `heimdall-mcp hooks audit-guardrails --since=168h --format=json`. If zero FP candidates, flip default `HEIMDALL_GUARDRAILS=shadow → warn` | Prior handoff item 5 |
| **~2026-05-02** (14 days after #55) | Calibration sample draw: 100-turn stratified sample from `hooks.log`. Follow with ~5 h of operator labeling (gated on 12a §6 item 1 sign-off) | 12a §3.2 |

No other time-gated items are in flight.

---

## Heimdall's 6 installed hooks

All at `/home/noname/Code/heimdall-mcp/.claude/settings.json`
(template envelope `wave2-phase3`). Every retrieval-hook fire carries
`session=<uuid>` in `hooks.log`.

| Event | Handler | Behavior |
|---|---|---|
| `SessionStart` | `heimdall-mcp hook session-start` | Injects `## Heimdall context` markdown. 2 s budget, 3 s hard cap. Auto-upgrades the template if out of date (PR #13). |
| `PostToolUse(Edit\|Write)` | `heimdall-mcp hook post-edit` | Foreground ≤50 ms; spawns detached `post-edit-actor` for reindex. |
| `UserPromptSubmit` | `heimdall-mcp hook user-prompt` | 450 ms budget (PR #31), 500 ms hard cap. Cache-first. Length-guard skips <8-char prompts. **Plan 12 Stage 1 (#55)** logs `hit_ids` + `prompt_embed_b64` on `stage=ok`. |
| `Stop` | `heimdall-mcp hook stop` | Appends `last_assistant_message` to per-session JSONL buffer. |
| `SessionEnd` | `heimdall-mcp hook session-end` | Triggers `ingest-session`; cleans rolling buffer. |
| `PreToolUse(Bash)` | `heimdall-mcp hook pre-tool-use` | Classifier. Default `shadow`. Static 19-rule ruleset. **Plan 11 Stage 0 (#57) installed but OPT-IN: default OFF; `HEIMDALL_LLM_CLASSIFIER=1` + `cfg.LLMClassifierModel` to enable.** Only mode+class combo that exits non-zero is `HEIMDALL_GUARDRAILS=block` AND `ClassBlock` (OQ-5). |

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
| Destructive-op guardrail design | `docs/plans/hooks/08-destructive-op-primitive.md` |
| Tiered-retrieval benchmark | `docs/plans/hooks/09-tiered-retrieval-benchmark.md` (62.4 % win) |
| Per-session savings report | `docs/plans/hooks/10-per-session-savings-report.md` |
| **Plan 11 (LLM classifier fallback)** | `docs/plans/hooks/11-llm-classification-fallback.md` (design) + `11a-design-decisions.md` (resolutions + HD-1..3) |
| **Plan 12 (semantic-drift metric)** | `docs/plans/hooks/12-semantic-drift-metric.md` (design) + `12a-design-decisions.md` (resolutions + §6 items) |

Main repo: `/home/noname/Code/heimdall-mcp`. All Wave 1/2 work and
Plan 11 Stage 0 + Plan 12 Stages 1–2 are on `main`.

---

## Prior merged work reference (don't re-litigate)

### 2026-04-18 afternoon (PRs #39–#49, main from `d231d92`..`1b9e92e`)
Wave F follow-ups, `bench-retrieval` auto-scope, `audit-guardrails`,
`ClassUnknown`, Plan 11/12 design docs, `hooks smoke`, `hooks.log`
quote-aware parser, event-key dedup. See the prior handoff version
(git log `git show 3701954:docs/plans/hooks/07-next-session-handoff.md`
if you need the detailed list) — or read commits directly:
```bash
git log --oneline d231d92..1b9e92e
```

### Wave F (2026-04-17)
PRs #33–#37 shipped: sessions `--since`, JSON `schema_version`,
doctor check #14, skill-body chunking, `sessions report --current`,
README audit, `part-*` skill dedup, post-edit actor session
attribution.

### Per-session savings feature (Waves A–E, PRs #25–#29)
`docs/plans/hooks/10-per-session-savings-report.md`. The
`sessions report` command joins the Claude Code transcript (tokens,
tool-call counts, `hook_success` attachment bytes) with `hooks.log`
(cache hits, guardrail verdicts, reindex counts) keyed on
`session_id`. Runs: `heimdall-mcp sessions list && heimdall-mcp
sessions report --session-id=<latest>`.

### Pre-Wave-F backbone (PRs #1–#23)
Wave 0 (11 review fixes) → Wave 1 (hook foundations T1/T2/T3/T9/T10/
T11/T12/T13/T17/T18/T22) → Wave 2 phase 1a (T5/T7/T14/T15/T16) →
phase 1b (T4/T6) → phase 2 + sections 2–5 (T8 + tiered retrieval +
path hierarchy + skills) → phase 3 (Phase 3 guardrails PR #18+#23).
Plus the late-2026-04-16 parallel push (#14–#23) that added:
test-log isolation, skills in SessionStart/UserPromptSubmit, Layer
2/3 test harnesses, CWD scope filter, Ollama pre-warm on install,
two-way `~/.claude/skills/` sync, and tiered-retrieval benchmark.

---

## Locked decisions (OQ-1..OQ-5) — do not relitigate

From `docs/plans/hooks/06-decisions.md`:

1. **OQ-1 Unknown-field tolerance on hook entries.** Proceed assuming
   Claude Code tolerates `"source": "heimdall"` + `"version": 1` on
   hook entries. **The `--source=heimdall --version=1` command-string
   fallback is mandatory regardless** — every installed hook command
   carries these flags so detection works even if Claude Code is
   strict.
2. **OQ-2 Install mode.** **Opt-in.** `heimdall-mcp install-hooks` is
   an explicit user-run command. No auto-install on first index.
3. **OQ-3 Binary name.** Keep `heimdall-mcp`. No rename. No
   `heimdall` alias. Hook command strings bake in `heimdall-mcp`
   verbatim.
4. **OQ-4 `settings.json` JSON round-trip.** `encoding/json` only +
   always-on backup. No `hujson` vendoring. Top-level keys sorted
   alphabetically — users with hand-edited settings will see a
   reformat.
5. **OQ-5 Exit codes.** **Retrieval hooks always exit 0** (`hook
   session-start`, `hook post-edit`, `hook post-edit-actor`, `hook
   user-prompt`, `hook stop`, `hook session-end`, and even
   `DispatchHook`'s usage-error paths). **Guardrail hooks (PreToolUse)
   — added in Phase 3 (PR #23), extended by Plan 11 Stage 0 (#57)** —
   may exit 2 + write one stderr line, but ONLY when
   `HEIMDALL_GUARDRAILS=block` AND classification is `block`. Shadow,
   warn, off, allow, timeout, and error cases all exit 0 with no
   stderr. Interactive commands use `0 / 1 / 2`.

---

## Measured wins (update: Plan 12 Stage 1 log budget added)

- **62.4 % token savings** from tiered retrieval at 20 % expand rate
  (`docs/plans/hooks/09-tiered-retrieval-benchmark.md`, `make bench`).
- **Per-session savings** queryable via `heimdall-mcp sessions report
  --session-id=<id>`. `--format=json` emits `schema_version: "v1"`.
  Session-end JSON includes `redundant_heimdall_calls` (#39), top-level
  `user_prompt_cache_hits` / `user_prompt_cache_total` (#43), and the
  `tool_use.semantic_drift` block when `--verbose` (#58).
- **Plan 12 Stage 1 log size budget: ~2 MB over a 14-day soak**
  (12a §4.4). Well under the shared 5 MB `hooks.log` cap (+1
  rotation = 10 MB ceiling). If rotation fires >1×/14 days, revisit
  per 12a §6 item 2.
- **`hooks smoke --fake-ollama` (PR #47)** validates all 6 hooks
  end-to-end without a real Claude Code restart. 6/6 PASS at `c3d28ca`.

---

## Caveats and known unknowns

1. **`~/.claude/settings.json` schema is a best-guess.** Stream F's
   install-hooks built the shape from the consolidated plan's example
   JSON, not from Claude Code docs. Recovery if rejected:
   `heimdall-mcp uninstall-hooks --scope=project` reverts cleanly.

2. **`fork+setsid` in Go is environment-dependent.** The post-edit
   actor uses `exec.Command(self, "hook", "post-edit-actor", ...)`
   with `SysProcAttr.Setsid = true` + `cmd.Start()` +
   `cmd.Process.Release()`. Unit-tested via an injectable `spawnFn`;
   real spawn is exercised by `hooks smoke` (#47) and real dogfood.

3. **Ollama must be running before `SessionStart` fires, or the hook
   will emit a Tier B "unavailable" note** on its first occurrence
   and then suppress for 5 minutes per `(project, failure_code)`.
   `install-hooks` does a best-effort prewarm (5 s timeout,
   `--no-prewarm` to skip).

4. **`VerifyHookIndex` will fail if the index was built with a
   different model than your current `heimdall-mcp config`.** Gate is
   strict by design (OQ-5 golden rule: no fuzzy match on hook path).
   If doctor check #9 is red, either re-index or switch the
   configured model.

5. **Two skills were too long for nomic-embed-text's 8192-token
   context** — fixed by chunk-at-import in Wave F. Large SKILL.md
   files are pre-split on H2/H3/paragraph boundaries into
   `mem:skill:disk:<slug>:part-<n>` rows.

6. **Plan 11 Stage 0 is installed but OPT-IN.** Default OFF is
   guarded by an explicit test. Setting `HEIMDALL_LLM_CLASSIFIER=1`
   **without** `cfg.LLMClassifierModel` also keeps the LLM branch
   off (both gates must clear). If you see `llm:<class>` labels in
   `hooks.log`, the dogfood is live.

7. **Stale worktrees** from prior parallel pushes may still be in
   `git worktree list`. Harmless, excluded from indexing. Prune at
   leisure:
   ```bash
   for d in .claude/worktrees/agent-*; do
     git worktree remove --force "$d"
   done
   ```

---

## Red flags — stop and ask if you see any of these

- `go test -race` fails anywhere on `main`.
- `heimdall-mcp hooks doctor` prints any `✗` after a clean install.
- `heimdall-mcp hooks smoke --fake-ollama` is not 6/6 PASS.
- `heimdall-mcp hooks tail --event=post-edit` shows the same file
  path retried 10+ times (hard-drop deadletter threshold; the actor
  is failing on the same input every time).
- Claude Code refuses to start after `install-hooks`. Immediately
  run `heimdall-mcp uninstall-hooks --scope=project` from a terminal;
  the backup at `settings.json.heimdall-backup-*` is the
  authoritative pre-install state.
- Any retrieval hook writes to stderr on a Claude Code fire. That's
  a §5.9 / OQ-5 violation and means a handler bypassed
  `LogHookEvent`.
- **Plan 11 Stage 0 default flipped ON.** If the default classifier
  LLM branch runs without an explicit `HEIMDALL_LLM_CLASSIFIER=1`,
  the default-off invariant test should have caught it in CI.
  Investigate before dogfooding.

---

## Final git state at session end (2026-04-18 evening)

```
$ git log --oneline -8
c3d28ca feat(sessions): plan-12 stage-2 semantic-drift compute + JSON/text wire-up (#58)
b3524c4 feat(calibrate-drift): T1xT2 threshold-sweep harness for semantic-drift metric (#56)
9f345a8 feat(guardrails): plan 11 stage 0 — LLM classifier fallback (opt-in) (#57)
18b4f81 feat(hooks): log hit_ids + prompt_embed_b64 on user-prompt stage=ok (#55)
daae783 chore(sessions): consolidate cache-hit counters + deprecate nested alias (#52)
6930ad4 docs(hooks): design decisions for plan 12 (semantic-drift + calibrate-drift) (#54)
e6b2490 docs(hooks): design decisions for plan 11 (LLM classifier fallback) (#53)
3701954 docs(handoff): refresh for 2026-04-18 session (PRs #39-#49) (#51)
```

Main is **in sync with `origin/main`** at `c3d28ca`. All 7 of
2026-04-18 evening's PRs (#52–#58) merged. Binary at
`/home/noname/.local/bin/heimdall-mcp` rebuilt and reports
`(c3d28ca)`. Hooks installed at
`/home/noname/Code/heimdall-mcp/.claude/settings.json` (6 hooks,
template envelope `wave2-phase3`). Plan 11 Stage 0 code path present
but OFF by default.

**Open known-unknowns (ordered by urgency):**

1. **Plan 11 Stage 0 dogfood** — flip `HEIMDALL_LLM_CLASSIFIER=1` and
   collect shadow traces (≥100 entries for the 11a OQ-1 eval set).
2. **Plan 12 Stage 1 data accumulation** — no action; just let
   traffic flow. Calibration window opens ~2026-05-02.
3. **Human sign-off queue** (7 items, see §Human sign-off queue).
   Until these clear, Stage 3 of both plans stays parked.
4. **PreToolUse shadow-mode audit** — run `hooks audit-guardrails`
   ~2026-04-25, flip `shadow → warn` if report recommends promotion.
5. **UserPromptSubmit cache-hit ratio over sessions** — surfaced in
   `sessions report` JSON. Track over weeks.
6. **Follow-up PRs F1/F2/F3** — small, none blocking. File when
   convenient.
