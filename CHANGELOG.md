# Changelog

All notable changes to this project will be documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html) in spirit
(the v0.x line still reserves the right to make breaking changes in minor bumps).

## [Unreleased]

### Fixed

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
