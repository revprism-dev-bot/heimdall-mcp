# Testing Strategy, Install UX & Rollout

Plan 05 of the Claude Code hooks integration. Owns *how we prove the hooks work*, *how users install/uninstall them without clobbering their setup*, and *in what order we ship them*. Planning only — no code changes.

## Context

Built on `01-architecture.md` (hook event selection), `02-cli-surface.md` (subcommand shapes), `03-latency.md` (budgets + cache), `04-failure-modes.md` (degradation tiers). Phase-1 hooks per plan 01: `SessionStart`, `UserPromptSubmit`, `PostToolUse(Edit|Write)`, `Stop`. Each is a `command` hook driven by `heimdall-mcp hook <name>` reading JSON on stdin and writing markdown (or nothing) to stdout. That contract is what we test, install, and roll out.

## Design Principles

1. Every hook has a Go-level test — hooks that live only in shell pipelines rot silently the moment anything changes.
2. No live `claude` binary required in CI. Hermetic harness is the gate; real-claude tests are a bonus layer.
3. Install is reversible, namespaced, and never clobbers user settings. Install → use → uninstall must leave `settings.json` byte-identical modulo the namespaced entries.
4. Ship in phases. Lowest-risk/highest-value first; hot-path hooks only after latency is proven.

## Test Strategy

### Layer 1 — Unit tests per hook command

Location: `internal/cli/hook_*_test.go`, colocated per repo convention. Per subcommand, ≥3 tests:

1. **Happy path** — construct event JSON per plan 01 §Per-Hook Contracts, call handler, assert stdout shape, empty stderr, exit 0.
2. **Degraded** — inject a faked dependency to simulate each tier from plan 04 §2 (Ollama down, index missing, embed timeout, stale index, fresh repo, model mismatch). Assert stdout matches the tier marker, stderr empty, **exit still 0** — phase-1 hooks never block.
3. **Edge** — malformed stdin, missing fields, empty prompt, slash command, huge prompt, concurrent invocation (`post-edit`), debouncer coalescing (`post-edit`). At least one per hook.

Harness shape — mirror `internal/heimdall/ollama_test.go`:

- `httptest.Server` for fake Ollama (`/api/embed`), no network.
- `t.TempDir()` for isolated project root + SQLite DB (per `internal/mcp/integration_test.go`).
- Hook handlers must be in-process testable: take `io.Reader`/`io.Writer`, return int, no `os.Exit` inside. Prerequisite ask to `cli-surface`.

**Assertions.** Golden files for the *template* (`testdata/hook-<name>-<case>.golden.md`) via a deterministic fake retriever — they catch header/fence/marker regressions. Structural asserts (`parseHookOutput(t, stdout)` + field checks) for semantic claims (top result is file X). Never put live retrieval output into a golden.

### Layer 2 — Hermetic integration tests

**Status: shipped (T20).** Location: `internal/cli/integration_test.go`, build-tag
`integration`. Run with `make test-integration` or
`go test -tags=integration ./... -count=1`. Default `make test` /
`go test ./...` does NOT include them.

Exercises the full process boundary: `os/exec` the built `heimdall-mcp`, pipe stdin, read stdout. Catches what unit tests can't — flag parsing, stdin buffering, exit codes, `main()`. The binary is built lazily on first test call into a package-scoped `os.MkdirTemp` dir and shared across cases. Each invocation wired to a `httptest` fake Ollama that stubs `/api/tags` + `/api/embed` with a fixed-dimension vector. Seed a real SQLite DB via `heimdall.OpenStore` + one record so `VerifyHookIndex` passes.

Concrete cases shipped:

- `TestIntegration_HookSessionStart_EndToEnd` — fire `hook session-start`, assert stdout has `## Heimdall context` + model marker, hooks.log has `stage=ok`.
- `TestIntegration_HookUserPrompt_CacheHit` — fire twice with identical prompt; second fire must hit cache (stdout identical, `/api/embed` request count unchanged, hooks.log `stage=cache_hit`).
- `TestIntegration_HookPostEdit_ActorReindexes` — fire `hook post-edit`, poll hooks.log for `actor_spawned` + `reindex_ok` from the detached actor (real fork+setsid exercised here).
- `TestIntegration_HookStop_ThenSessionEnd` — fire Stop twice, then SessionEnd; assert buffer file created with ≥2 lines, then removed, and `event=session-end` appears in hooks.log.

Hermetic env: every test pins `HEIMDALL_MCP_CONFIG`, `HEIMDALL_HOOK_LOG`, `XDG_STATE_HOME`, `XDG_CONFIG_HOME`, and `HOME` so nothing leaks to user state.

Assertions (each maps 1:1 to plan 01 §`test-rollout` — these *are* the phase-1 acceptance criteria):

1. SessionStart → stdout contains `## Heimdall context` with a seeded-memory bullet.
2. UserPromptSubmit with real prompt → `## Heimdall suggests` with the seeded chunk at rank 1.
3. UserPromptSubmit with `/help` → empty stdout (skip heuristic).
4. PostToolUse(Edit) on a seeded file → empty stdout; DB `last_indexed` advances within ≤5 s.
5. Stop fires ×5 → rolling buffer triggers ingest → memory row count increases by ≥1.
6. Ollama killed mid-test → SessionStart still exits 0, stdout contains degraded marker.
7. `--budget-ms` honored — sleeping fake Ollama → hook returns within budget + jitter, stdout empty.
8. Cache hit (from plan 03 §3) — repeated identical prompt → <50 ms, same output.
9. Cache invalidation — index write affecting the keyed range → next identical prompt re-computes.
10. Model mismatch (from plan 04 §3) — DB embedded with model X, hook using model Y → tier-B degraded line, no garbage results.

### Layer 3 — Real `claude` end-to-end (best-effort, not a CI gate)

**Status: harness shipped (T23).** Location: `internal/cli/e2e_test.go`,
build-tag `e2e`. Opt-in via `HEIMDALL_E2E_CLAUDE=1` AND `claude` on PATH —
otherwise `t.Skip`s cleanly. Run with `make test-e2e` or
`HEIMDALL_E2E_CLAUDE=1 go test -tags=e2e ./... -count=1`.

`claude --help` gives us four levers that make a real-claude harness possible:

- `--settings <file-or-json>` — throwaway `settings.json` that references our hooks. Key unlock.
- `-p/--print` — non-interactive.
- `--output-format=stream-json --include-hook-events` — emits every hook lifecycle event. This is what lets us assert "the hook fired."
- `--bare` / `--tools ""` — isolate from unrelated machinery.

Current harness (`internal/cli/e2e_test.go`, `TestE2E_ClaudeSessionStartHookFires`):

1. Build `heimdall-mcp` from source into a temp dir.
2. Create a temp project with one trivial Go file.
3. Seed the vector store (`heimdall.OpenStore` + one `Upsert`) so `VerifyHookIndex` passes — skips a real `heimdall-mcp index` run and its Ollama dependency.
4. Install hooks via `heimdall-mcp install-hooks --scope=project` against the temp project (hermetic env pins `HEIMDALL_MCP_CONFIG`, `XDG_*`, `HOME`).
5. Exec `claude --settings <path-to-temp-settings.json> -p "hi"` in the temp project with `PATH` pointing at our built binary.
6. Grep the output for the `## Heimdall context` banner as proof the SessionStart hook fired.

Caveats: requires `claude` on PATH (skip cleanly otherwise); requires Ollama or a stubbed endpoint; burns real model tokens unless the user has a local setup; gated behind `-tags e2e` AND `HEIMDALL_E2E_CLAUDE=1`, never default CI. On auth/network failures the test treats the run as "environment issue" and `t.Skip`s with the captured output — the harness is explicitly "best effort, not a CI gate." If claude auth flows or flag shapes drift, layer 3 degrades to a manual runbook and layers 1+2 remain the correctness gate — they already cover every assertion from plan 01 §`test-rollout`.

### Smoke harness — dispatchable dogfood substitute

**Status: shipped.** Location: `internal/cli/hooks_smoke.go`, entry point
`heimdall-mcp hooks smoke [--fake-ollama] [--format=text|json]`. Wrapper
Makefile target: `make hooks-smoke` (builds the binary, then runs the
harness with `--fake-ollama`).

Purpose: replace the manual "reopen Claude Code, trigger each hook, eyeball
hooks.log" ritual with a single command. This is **not** a substitute for
layers 1-3:

- Layer 1 proves handler logic in isolation.
- Layer 2 proves the binary's process boundary (flag parsing, stdin
  buffering, actor fork+setsid).
- Layer 3 proves the real `claude` CLI drives our hooks end-to-end.
- **Smoke harness** proves "all six installed hooks still fire cleanly
  against real CLI deps" — the sanity check a developer runs locally
  after touching hook code, or that a CI job runs nightly on a
  head-of-main binary to catch drift without needing a `claude` install.

Scope per fire:

- Synthesized payload per hook matching the Claude Code event schema
  (`SessionStart`, `UserPromptSubmit`, `PostToolUse(Edit|Write)`,
  `PreToolUse(Bash)`, `Stop`, `SessionEnd`).
- Assertions: exit 0 (OQ-5), empty stderr on retrieval hooks, expected
  `stage=` / `msg=` token present in the per-fire hooks.log slice.
- Per-run a fresh tempdir-scoped `hooks.log` via `HEIMDALL_HOOK_LOG`; the
  user's real log file is never touched.

`--fake-ollama` spins an in-process `httptest.Server` (same shape as
`internal/cli/integration_test.go`'s `fakeOllamaServer`) and seeds a vector
store so session-start / user-prompt pass `VerifyHookIndex` offline. Without
the flag, the harness hits whatever `heimdall-mcp config` resolves — useful
for confirming a real local Ollama setup before shipping.

Output modes:

- `--format=text` (default): per-hook `[PASS] / [FAIL]` lines with the
  captured log line + reason on failures, plus a final summary (passed /
  failed / total wall-time).
- `--format=json`: machine-readable `SmokeReport`; scripts and CI can
  gate on `failed > 0`.

Exit code: 0 if every step passes, 1 if any fails, 2 on a CLI usage error.

### Failure-mode coverage

Every tier in plan 04 §1 needs a unit *and* an integration test. Minimum: Ollama unreachable (SessionStart + UserPromptSubmit), index missing (SessionStart), embed timeout > budget (UserPromptSubmit with sleeping fake), concurrent PostToolUse debouncer (unit; 10 events/100 ms → single re-index), DB migration mismatch (SessionStart), model mismatch (plan 04 §3). Cross-reference by plan 04 tier ID.

### Performance regression tests

`internal/cli/hook_user_prompt_bench_test.go`: `BenchmarkUserPromptHook` over a fake Ollama (fixed 10 ms delay) and a seeded ~5 K-chunk DB. Baseline stored in `testdata/perf/hook_user_prompt.baseline`. CI comparison: **>50 % regression → fail** (non-gating at first, gating once stable). Two explicit gates from plan 03 §2: warm+cached p50 ≤ 50 ms, warm+uncached p95 ≤ 250 ms. Cold Ollama is excluded from the golden path — covered as a tier-B degradation case that must still meet the 500 ms plan 01 hard timeout.

### Long-running probes (opt-in, not CI-gated)

Two questions from plan 03 §8 need empirical answers fake dependencies can't produce:

- **Hook queuing semantics (§8 Q1).** Layer-3 probe: a hook that sleeps 2 s, two prompts fired 100 ms apart via `claude --print --include-hook-events`, parse stream-json to see whether the second hook is serialized, killed, or run in parallel. Gated `-tags e2e`. Result determines whether `prompt-submit` needs its own per-session lockfile.
- **Ollama `keep_alive` ceiling (§8 Q2).** `internal/cli/hook_keepalive_bench_test.go` build-tag `longbench`: embed once against real Ollama, sleep 5/10/30 min, re-embed, record the cold-swap delta. Run before releases. Fake Ollama can't exercise this — penalty is process-level.

## Install UX

### `heimdall-mcp install-hooks`

Flow:

1. **Resolve scope.** `--scope=project` → `<cwd>/.claude/settings.json`; `--scope=user` → `~/.claude/settings.json`. Default: project if cwd is in a git repo and a heimdall project is registered for it, else user.
2. **Read existing settings** as generic JSON (map), not a typed struct. Claude Code's schema will grow and we must not drop unknown fields. Missing file → start from `{}`.
3. **Detect conflicts.** Conflict = same `event` + same `matcher` + no `x-heimdall` marker. Non-conflicts (different events, or entries we already own) are safe.
4. **Namespace what we add.** Every entry we write carries `"x-heimdall": {"version": 1, "id": "<hook-id>", "heimdall_version": "<semver>", "installed_at": "<iso>"}`. Uninstall and upgrade both key off this marker.
5. **Handle conflicts.** Default: print conflict report, exit non-zero, write nothing. `--merge` → add ours alongside (Claude Code fires both). `--force` → replace existing, back up to `settings.json.bak-<ts>` first.
6. **Write atomically.** Tmp file, `fsync`, rename.
7. **Success output.** One line: `Installed N hooks (scope=…, file=…). Run 'heimdall-mcp hooks doctor' to verify.`

**Flags:** `--scope={user|project}`, `--dry-run`, `--merge`, `--force`, `--only=<list>`, `--disable=<list>`. `--only` is how we stage the rollout (see Rollout Phases).

**Non-goals:** no network calls, no auto-index, no rewriting of unknown JSON keys. `encoding/json` will reformat the file — acceptable for phase 1, always pair with a backup on destructive paths. Revisit with `hujson` only if users complain. (Open question.)

### `heimdall-mcp uninstall-hooks`

- Walk settings.json hooks, drop every entry with `x-heimdall` present. Leave everything else byte-identical.
- `--scope` like install.
- `--dry-run` prints the diff.
- Idempotent: running twice is fine, the second run reports `No heimdall hooks found, nothing to do.`
- If the resulting `hooks` array is empty, remove the key entirely rather than leaving `"hooks": []`.

### `heimdall-mcp hooks doctor`

One table, GREEN/YELLOW/RED per check:

| # | Check | Red | Yellow | Green |
|---|---|---|---|---|
| 1 | Settings file exists | Missing | — | Present |
| 2 | Heimdall hooks present | 0 `x-heimdall` entries | Some expected missing | All phase-1 present |
| 3 | `heimdall-mcp` on `$PATH` | Not found | Version ≠ install-time version | Match |
| 4 | Each hook command executes | Non-zero on synthetic event | Non-zero on degraded dep | Exit 0 |
| 5 | Ollama reachable | Unreachable | Reachable, model not loaded | Ready |
| 6 | Current project indexed | Not registered | Stale | Fresh |
| 7 | Hook schema version | `x-heimdall.version` > binary | < binary | Match |

Check 4 is the interesting one: spawn each installed hook command with a canned synthetic event (matching plan 01 schemas), read stdout, assert exit 0. This catches "heimdall upgraded but the binary in settings.json points at the old version" instantly. Exit 0 if all green/yellow, 1 if any red — scriptable for install verifiers and CI smoke tests. Reconcile the final check list with plan 04 §6 at consolidation time.

## Per-Project / Per-Hook Toggles

Toggle surface = `heimdall-mcp config hooks <name> <on|off>`, writing to heimdall project config, not `settings.json`. Each hook command reads that config at startup (single file stat + parse, <5 ms) and short-circuits with empty stdout + exit 0 if disabled. Global kill switch: `HEIMDALL_HOOKS=0`, checked first. Toggling `settings.json` directly would be fragile, racy with other tools, and reformat the user's file. Reconcile surface with plan 02 §Config file design on consolidation.

## Upgrades

Entries carry `x-heimdall.version: N`. `install-hooks` on existing entries: match → no-op (`Already up to date.`); older → auto-migrate (remove + reinstall, print diff summary); newer → refuse with "upgrade your binary first", exit non-zero. Migration rules live as code — a `migrations[oldVer]` table of pure functions over parsed JSON, unit-tested.

## Rollout Phases

| Phase | Hooks | Gate criteria to advance | Est. effort |
|---|---|---|---|
| **1a** — foundation | `SessionStart` + `PostToolUse(Edit\|Write)` | Layers 1+2 tests passing; doctor passing; install/uninstall round-trip verified on 3 real `settings.json` samples (empty, existing non-heimdall hooks, existing heimdall hooks from older version); dogfooded in one repo for ≥3 days with zero complaints | 1 week |
| **1b** — hot path | `UserPromptSubmit` | 1a shipped; latency benchmark green (p50 ≤ 300 ms, p99 ≤ 500 ms on a mid-size repo); skip-heuristic unit tested; degradation path tested under simulated Ollama failure | 1 week |
| **2** — session learning | `Stop` → rolling buffer → `ingest-session` | 1b stable for ≥1 week; `Stop` event payload confirmed (open question #1 in arch plan); session buffer location + retention decided | 1 week |
| **3** — guardrails | `PreToolUse(Bash(rm *\|git push --force*))` as `agent` hook | Phase 2 stable; `heimdall_recall` has enough signal to produce a useful "is this destructive?" answer; `agent` hook type support confirmed in current Claude Code | 2 weeks |

Justification for ordering:

- **Why 1a first?** `SessionStart` and `PostToolUse(Edit|Write)` are the two hooks with trivial contracts (one produces text once, one produces nothing ever). They exercise the entire install/doctor/toggle machinery without touching the hot path. If anything is wrong with the install or test harness, we catch it here cheaply.
- **Why `UserPromptSubmit` in its own phase?** It's the only hot-path hook. Bad latency here makes `claude` feel broken, and we want a full phase to measure + tune + back out if needed. Splitting it out also means if we have to revert, we only yank this one hook, not the whole install.
- **Why `Stop` in phase 2?** Its event payload is unconfirmed (arch plan open question #1), and its background-ingest path is the riskiest write-side operation (can balloon disk, can corrupt DB if crashy). Needs extra soak time.
- **Why guardrails last?** `PreToolUse` is the only hook that can *block* user actions (exit 2). A bad guardrail produces active frustration, not silent degradation. We ship it only after everything else has earned trust.

## Coordination Notes

All four sibling plans are in-tree. Load-bearing references — if any of these sections change, the corresponding section here must update.

- **Plan 01 — `docs/plans/hooks/01-architecture.md`.** §Per-Hook Contracts is the ground truth for unit asserts and golden-file templates. §Dependencies → `test-rollout` bullets 1–7 are the Layer 2 assertion seed. §Open Questions #1 (Stop payload) and #5 (debouncer window) gate `stop` and `post-edit` tests.
- **Plan 02 — `docs/plans/hooks/02-cli-surface.md`.** §Command signatures (`hook session-start`, `hook prompt-submit`, `hook post-edit`, `hook post-commit`, `hook session-end`, `hook pre-destructive`) defines the subcommand set — tests target the union. §`install-hooks` / §`uninstall-hooks` / §`hooks-doctor` are the shapes this plan's Install/Uninstall/Doctor sections must align with. §Config file design hosts the toggle surface. **Hard ask still open:** handlers must take `io.Reader`/`io.Writer` and return int, no `os.Exit` inside. Without it, Layer 1 is impossible.
- **Plan 03 — `docs/plans/hooks/03-latency.md`.** §2 Budget fixes **p95 ≤ 250 ms** for `UserPromptSubmit` (tighter than plan 01's 300 ms p50); this plan's benchmark anchors on 250 ms p95 with 50 % regression threshold. §3 Caching adds Layer 1 tests for cache-hit (<50 ms) and cache-invalidation. §4 Skip heuristic → expanded `/help` test variants. §6 Debouncing → exact lock-file window for the `post-edit` debouncer test.
- **Plan 04 — `docs/plans/hooks/04-failure-modes.md`.** §1 Failure taxonomy is the definitive failure list. §2 Per-tier design fixes the exact stdout markers and exit codes; degraded-state goldens key off these. §3 Model mismatch adds a new test: DB embedded with model X, hook embedding with model Y → tier-B line, not garbage. §4 Install-time failure modes → install-UX tests (perm-denied, malformed JSON, disk full). §5 Hook composition confirms non-clobber approach. §6 Doctor — reconcile the check list at consolidation.

**Known inconsistencies for task #6 (consolidator):** (1) subcommand name — plan 01 says `user-prompt`, plan 02 says `prompt-submit`; tests currently cover both. (2) Latency — plan 01 says 300 ms p50, plan 03 says 250 ms p95; this plan uses plan 03. (3) Plan 02 adds `post-commit`/`session-end`/`pre-destructive` that plan 01 defers to phase 2/3 — this plan keeps plan 01's phasing for install, tests the full set. (4) Install marker key — this plan uses `x-heimdall`; confirm against plans 02/04.

## Open Questions

1. **Layer 3 viability.** `claude --include-hook-events` exists but the stream-json schema for hook-event records isn't documented. Need a sample run or source read to confirm. Until then, layer 3 stays a manual runbook.
2. **JSON round-trip fidelity.** `encoding/json` reformats. Options: (a) accept + always back up, (b) vendor `tailscale/hujson`. Preference (a); revisit if users complain.
3. **Doctor version match.** Record `heimdall_version` in `x-heimdall` at install, compare to running binary. Need confirmation we have a version string embedded in the binary.
4. **Perf regression threshold.** 50 % is placeholder — confirm with `latency-eng`.
5. **Auto-run doctor at end of install?** Leaning yes; means a successful install can exit non-zero if Ollama is down. Team-lead call.
6. **Template timestamps.** Golden templates must not include dynamic fields; if they do, test helper substitutes `__TIMESTAMP__`. Flag to `cli-surface`.
7. **Subcommand name unification.** `user-prompt` vs `prompt-submit` — consolidator must pick one before implementation starts.
