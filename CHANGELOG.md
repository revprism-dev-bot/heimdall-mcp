# Changelog

All notable changes to this project will be documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html) in spirit
(the v0.x line still reserves the right to make breaking changes in minor bumps).

## [Unreleased]

### Changed

- **Default `ollamaEndpoint` is now `http://127.0.0.1:11434`** (was `http://localhost:11434`).
  On macOS and some Linux configurations, `localhost` resolves to `::1` (IPv6)
  while Ollama's `ollama serve` binds IPv4 `127.0.0.1` only, producing spurious
  "Ollama not reachable" errors when the daemon is actually running. Using the
  literal IPv4 address removes that resolution surprise. Users with a custom
  `ollamaEndpoint` in `config.json` or `HEIMDALL_OLLAMA_ENDPOINT` are unaffected.

### Upgrade notes

- **`embed_max_concurrent: 0` now means UNBOUNDED** (previously silently
  capped at the safe default of 2). This fixes the long-standing bug
  where explicit `0` was remapped to the default. If your `config.json`
  contains a literal `embed_max_concurrent: 0` (possible if you ran a
  pre-fix build with `HEIMDALL_EMBED_*` env vars set and those values
  leaked to disk before the H1 persistence fix), it NOW means "no
  bound per server". To restore the safe default of 2 embeds in
  flight, set `embed_max_concurrent: -1` or remove the key entirely.
  Most operators only need to audit `~/.config/heimdall-mcp/config.json`
  for the literal `0` after upgrading.
- **`HEIMDALL_EMBED_MAX_CONCURRENT=-1` is rejected at the env layer.**
  The `-1` sentinel is an internal config.json convention only; the
  env surface treats any negative value as invalid input (matching
  `HEIMDALL_EMBED_TIMEOUT_MS` / `..._MAX_RETRIES`). A rejection is
  logged; the on-disk value remains in effect.
- **Legacy `<model>_latest/` dirs are auto-migrated** on first access.
  Opt out with `HEIMDALL_DISABLE_LEGACY_MIGRATION=1`; when opt-out is
  set, writes still land in the canonical dir — only the rename is
  skipped. Use `heimdall-mcp cleanup-legacy-latest [path]` to audit
  and remove legacy dirs manually once their canonical siblings
  exist.

### Fixed

- **`event=session-end` hook now survives Claude Code's 1.5 s SIGTERM budget.**
  Claude Code 2.1.114 hardcodes a 1500 ms per-subprocess cap on SessionEnd
  hooks (harness constant `E$8`). The P0 `HookSessionEnd` path does
  transcript summarization + ollama embeddings + memory-store ingest +
  review-record persistence, which on real sessions with ≥10 memories to
  ingest measures ~1.7 s — past the budget, so the subprocess got SIGTERM'd
  before any log line landed. Observed effect: zero `event=session-end`
  entries since 2026-04-19T18:46Z, breaking the nag loop that depends on
  the review record. Fix is threefold: (1) the installer now emits
  `hooks[0].timeout = 30` (seconds) on the SessionEnd entry, which Claude
  Code reads through `getSessionEndHookTimeoutMs → AbortSignal.timeout`;
  (2) `HookSessionEnd` logs the `msg=session_ended` "ran" marker BEFORE the
  slow ingest block so evidence survives any future tighter budget; and
  (3) a new `sweepOrphanBuffers` pass on `HookSessionStart` moves rolling
  buffers idle past 48 h into `.heimdall_db/hooks/sessions/_orphaned/`, so
  reboot-killed session buffers stop accumulating. `heimdallBinaryVersion`
  bumped to `"wave2-phase4"` and `autoUpgradeHooks` now refreshes stale
  heimdall-sourced entries in place so existing installs pick up the new
  shape without a manual `install-hooks` run. Full investigation in
  `docs/plans/claude-heimdall-self-use/04-session-end-regression-handoff.md`.
- **Sub-repos with a broken `.heimdall_db` are now registered anyway.**
  `IndexSubRepos` used to fire `OnSubRepoDiscovered` only AFTER
  `OpenStore` succeeded — so a sub-repo whose SQLite file was truncated,
  wrong-schema, WAL-desynced, or permission-denied (common when users
  sync `.heimdall_db/` across machines via git / gitea) was silently
  dropped from the project registry. Symptom reported against v0.0.3:
  `heimdall-mcp index <wrapper>` auto-registered `wrapper-app` (already
  present with a stale dbPath) but not `wrapper-service` or
  `wrapper-infra`. The callback now fires immediately after the sub-repo
  is identified (post user-exclude filter, pre-model-resolution,
  pre-`OpenStore`), so registration is independent of per-sub-repo
  store health. The stored `dbPath` is the sub-repo's `.heimdall_db`
  root (model-agnostic); `Registry.Register` tuple-dedupes so repeat
  runs stay a no-op. Sub-repo `OpenStore` failures are additionally
  surfaced on stderr (`sub-repo <name> (<path>) failed: <err>`) so they
  stand out in CI logs and are not hidden when stdout is piped.
  Regression tests: `TestIndexSubRepos_RegistersEvenWhenStoreOpenFails`
  (exact v0.0.3 repro) and
  `TestIndexSubRepos_DiscoveryFiresForEveryDiscoveredSubRepo` (property:
  event count == non-user-excluded discovered count, regardless of
  per-sub-repo outcome).
- **`heimdall_index` and `heimdall_index_text`: write to the target path, not the MCP server's CWD.**
  Before this fix, `runIndex` and `toolIndexText` computed `baseDir` from
  `os.Getwd()` and `filepath.Join(cwd, ".heimdall_db")`, so reindexes invoked
  against an absolute path quietly dropped the new data into the server's
  working directory instead of the project's. This was Problem #1 in the
  2026-04-19 handoff. Resolution now goes through the registry-first
  `resolveDBDir(project)` helper for every write path except
  `autoIndexOnSearch` (which keeps its Phase 4 CWD-based intent and is the
  single documented exception).
- **`ModelDBDir` mixed-state resolution is content-aware.**
  When both `<model>/` and `<model>_latest/` directories exist on disk,
  `ModelDBDir` now picks whichever has the higher `MAX(mod_time)` in its
  `entries` table. When neither DB can be opened (e.g. corrupt files), it
  falls back to file mtime. An empty canonical dir no longer shadows a
  populated legacy dir.
- **Legacy `<model>_latest/` directories are auto-migrated to `<model>/`
  on the first write.**
  `MigrateLegacyLatestDir(baseDir, model)` renames the legacy dir,
  clearing its `hook_cache` rows first to avoid cross-store cache
  pollution. The migration is lock-protected (idempotent under
  concurrent writers), skips symlinks for safety, and does not delete
  data when both dirs are populated (log-and-leave). Set
  `HEIMDALL_DISABLE_LEGACY_MIGRATION=1` to opt out of the rename while
  keeping the mixed-state reader active.
- **Ollama: bounded parallelism on embed bursts** (closes handoff Problem #5).
  Burst parallel embed calls from the indexer against a single-GPU Ollama
  instance were deadline-exceeding (`context deadline exceeded`). The
  `OllamaClient` now bounds concurrent `/api/embed` requests with a counting
  semaphore (default cap = 2) shared across `Embed`, `EmbedForHook`, and
  `EmbedBatch`. Per-request deadlines that fire (vs. caller-ctx cancellation)
  are retried with configurable backoff (default 2 retries, 250 ms + 500 ms).
- **PR #74 follow-up review fixes:**
  - *CLI honors embed config:* CLI entry points (indexer, hooks, doctor,
    skills, sessions, install, discover, recall, ingest_session) now route
    Ollama construction through a config-aware helper so
    `HEIMDALL_EMBED_MAX_CONCURRENT` / `HEIMDALL_EMBED_TIMEOUT_MS` /
    `HEIMDALL_EMBED_MAX_RETRIES` and `config.json` values take effect on
    the primary burst-parallelism surface. (F1)
  - *Env vars no longer persist to disk:* `LoadConfig` no longer applies
    env overrides to the returned Config. A new `config.ResolveEmbedConfig`
    produces a transient effective-config snapshot used by the MCP server
    and CLI clients. Any later `SaveConfig(cfg)` therefore cannot bake
    env-var tuning into `config.json`. (F2 / H1)
  - *`embed_max_concurrent: 0` honoured as "unbounded":* `NewOllamaClientFromConfig`
    no longer silently remaps 0 → default. Per-README contract 0 disables
    the semaphore. The "use default" sentinel is now `-1`; `DefaultConfig()`
    seeds `-1` for fresh installs so the safe default (cap = 2) still
    applies when the key is absent or set to `-1`. (F3 / H2)
  - *Retry backoff no longer leaks timers on ctx-cancel:* `time.After`
    replaced with `time.NewTimer` + `Stop()` in both retry loops so a
    cancelled caller ctx releases its timer immediately. (F4)
  - *Server-scoped Ollama client singleton:* `Server.newOllamaClient()` now
    returns a singleton keyed on resolved endpoint / concurrency / timeout /
    retries. Two concurrent MCP tool calls share one counting semaphore so
    the effective cap is per-server, not per-tool-call. HTTP keep-alive
    pool is also reused across tool calls. (F5 / M1)
  - *Env-int clamps:* `HEIMDALL_EMBED_TIMEOUT_MS` is clamped to
    `[100 ms, 10 min]` (avoids `time.Duration` overflow →
    instant-DeadlineExceeded retry storms). `HEIMDALL_EMBED_MAX_CONCURRENT`
    is clamped to `[0, 64]`. `HEIMDALL_EMBED_MAX_RETRIES` is clamped to
    `[0, 10]`. Violations log and clamp to the nearest boundary. (F6 / M2)
  - *Event-driven test synchronisation:* 10 × `time.Sleep(200ms)` "goroutines
    queued by now" oracles replaced with channel-based `waitStarted` that
    blocks until the expected in-flight count is observed server-side.
    Kills `-race` / loaded-CI flakiness without affecting coverage. (F7)
  - *`Server.newOllamaClient` test coverage:* new table-driven regressions
    in `internal/mcp/server_test.go` verify positional-arg wiring across
    sentinel / literal / clamped inputs plus singleton identity / rebuild-
    on-config-change behaviour. (F8)
  - *Semaphore-release regression for non-ctx errors:* new test iterates
    HTTP 500, decode-error, and empty-response paths with cap=1 for 3
    consecutive calls — a slot leak would block the 2nd/3rd on the
    semaphore wait. (F9)

### Added

- **`heimdall_status` accepts an optional `path` parameter.**
  Resolution routes through `resolveDBDir(path)` (registry-first,
  cwd-last). `heimdall_status path=/path/to/project` returns that
  project's stats regardless of the MCP server's CWD. When `path` is
  omitted, the tool still falls back to the registry entry whose path
  contains the current CWD, and finally to `<cwd>/.heimdall_db`.
- **`HEIMDALL_DISABLE_LEGACY_MIGRATION=1` environment variable.**
  Opts a session out of the `<model>_latest/` → `<model>/` rename so
  operators can audit or manually merge the mixed state. Documented in
  the README migration section.
- **Added `dbPath` to `heimdall_status` output.**
  The resolved baseDir is now echoed so callers can confirm which store
  the MCP is reading without re-running resolution elsewhere.
- **`heimdall-mcp config show [--effective]` CLI subcommand.**
  Prints the effective embed config (after `HEIMDALL_EMBED_*` env
  resolution + safety clamps) with per-field source attribution so
  operators can diagnose "why is indexing slow" without reading source
  or guessing. Without `--effective`, prints the raw config.json.
- **`heimdall-mcp cleanup-legacy-latest [path] [--force]` CLI subcommand.**
  Scans `<path>/.heimdall_db/` for `<model>_latest/` dirs whose
  canonical `<model>/` sibling exists on disk, reports row counts,
  and removes them on confirmation (or `--force` for scripts). Safe
  by default: never removes a legacy dir whose canonical sibling is
  missing.
- `heimdall.NewOllamaClientWithLimit(endpoint, maxConcurrent int)` — additive
  constructor. The original `NewOllamaClient(endpoint)` remains and now
  applies `DefaultEmbedMaxConcurrent = 2` transparently for backward
  compatibility.
- `heimdall.NewOllamaClientWithOptions(endpoint, OllamaOptions)` — full
  tunables: `MaxConcurrent`, `EmbedTimeout`, `MaxRetries`, `RetryBackoff`.
- `heimdall.NewOllamaClientFromConfig(endpoint, maxConcurrent, timeoutMs, maxRetries int)`
  — convenience wiring for `Config.EmbedMaxConcurrent` /
  `Config.EmbedTimeoutMs` / `Config.EmbedMaxRetries`. Sentinel contract:
  `-1` = use default; `0` = explicit unbounded / zero retries; `N` = literal.
- `config.ResolveEmbedConfig(base Config) Config` — returns a transient
  effective config with `HEIMDALL_EMBED_*` env overrides merged and values
  clamped to documented safety ranges. Never mutates `base`.
- `config.MaxEmbedConcurrent`, `config.MaxEmbedTimeoutMs`,
  `config.MinEmbedTimeoutMs`, `config.MaxEmbedRetries` — clamp bounds for
  operator-tunable fields.
- New config keys:
  - `embedMaxConcurrent` (int, sentinel `-1` = default 2; `0` = unbounded;
    `N` = cap at N). Default on fresh install: `-1`.
  - `embedTimeoutMs` (int, default 30000 via package constant)
  - `embedMaxRetries` (int, sentinel `-1` = zero retries; `0` = default 2;
    `N` = literal). Default on fresh install: `-1`.
- New env-var overrides (clamped at parse time):
  - `HEIMDALL_EMBED_MAX_CONCURRENT`
  - `HEIMDALL_EMBED_TIMEOUT_MS`
  - `HEIMDALL_EMBED_MAX_RETRIES`

### Changed

- All MCP tools now construct the Ollama client through the central
  `Server.newOllamaClient()` helper so config-driven concurrency / timeout
  / retry tunables take effect globally. Post-fix this helper returns a
  Server-scoped singleton so the semaphore is shared across concurrent
  tool calls.
- CLI indexer / hook / discovery / doctor / install / recall / sessions /
  skills paths now build Ollama clients via the new `newOllamaClient`
  helper so `HEIMDALL_EMBED_*` env vars and `embedMaxConcurrent` /
  `embedTimeoutMs` / `embedMaxRetries` config take effect on the
  command-line burst path too.
- Per-request embed deadline is now `min(caller_deadline, embedTimeout)`.
  This caps runaway single embed calls that previously inherited huge
  caller deadlines (e.g. the 24h ctx used by the indexer).

### Notes

- Retries are scoped strictly to `context.DeadlineExceeded` from our
  per-request timeout. Caller-ctx cancellation and non-timeout errors
  (HTTP 4xx/5xx, decode failures) are NOT retried.
- `MaxConcurrent = 0` is an explicit opt-out for callers that manage
  concurrency upstream. Negative values coerce to the default with a
  one-line WARN log.

### Tests

- `TestModelDBDir_MixedLegacyAndCurrent_PrefersRecent`,
  `TestModelDBDir_MixedState_CurrentNewer`,
  `TestModelDBDir_MixedState_EmptyCanonical_PrefersLegacy`,
  `TestModelDBDir_MixedState_BothUnopenable_FallsBackToFileMtime`
  cover the mixed-state reader with `os.Chtimes`-based deterministic
  timestamps (no `time.Sleep`). First `os.Chtimes` usage in the repo.
- `TestMigrateLegacyLatestDir_HappyPath`,
  `TestMigrateLegacyLatestDir_BothExist_LogsAndLeaves`,
  `TestMigrateLegacyLatestDir_Idempotent`,
  `TestMigrateLegacyLatestDir_DisabledByEnvVar`,
  `TestMigrateLegacyLatestDir_SymlinkSource`,
  `TestMigrateLegacyLatestDir_ConcurrentInvocations`,
  `TestMigrateLegacyLatestDir_ClearsLegacyHookCache`,
  `TestMigrateDisabled_WritesStillGoToCanonical`
  cover the migration surface.
- `TestToolStatus_UsesPathParam`,
  `TestToolStatus_FallsBackToResolveDBDir`,
  `TestToolStatus_DoesNotUseContaminatedIndexPath`,
  `TestRunIndex_WritesToTargetBaseDir`,
  `TestRunIndex_FromSubdirectory_BaseDirResolvesToProvidedPath`,
  `TestToolIndexText_WritesToTargetNotCWD` cover the MCP-layer fix.
- `TestIntegration_CLIIndex_WritesToTargetPath` and
  `TestIntegration_CLIIndex_LegacyLatestMigrated` (build tag
  `integration`) exercise the compiled binary against a fake Ollama
  from a cwd that differs from the target path.

### Recently shipped (context for this PR)

These PRs merged in the 2026-04-19 batch but predate the Unreleased
section above — included here for audit visibility:

- **#67** `feat(indexer): index nested sub-repos as separate projects`
- **#68** `fix(indexer): sub-repo pass incremental + progress output`
- **#69** `fix(indexer): multi-model sub-repo indexing + interrupted-run prompt`
- **#70** `fix(cli): always show model multi-select in TTY; add multi-model flags`
- **#71** `docs(readme): clarify Anthropic API boundary vs local-only Heimdall work`

[Unreleased]: https://github.com/caio-silva/heimdall-mcp/compare/v0.0.2...HEAD
