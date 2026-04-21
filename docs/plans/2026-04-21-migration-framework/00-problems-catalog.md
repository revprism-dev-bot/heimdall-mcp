# Problems catalog — carried forward from docs/plans/2026-04-19-legacy-db-fixes/

**Status:** Superseded by the migration-framework direction (decisions.md Q2c, 2026-04-21). This doc preserves the *problems* and *root causes* from the 2026-04-19 ad-hoc-helper planning round. Old solution designs (per-PR walkthroughs, `MigrateLegacyLatestDir` drafts, planner-1/2/3 variants) are intentionally discarded — the unit of work is changing to a versioned migration framework. The Wave 2 migration-framework PR will produce its own design.

File references are to `main @ 6de55b2` (the base the original planning round was written against).

---

## Problem #1 — Reindex succeeds but MCP reads a stale or empty store

CRITICAL. Three interacting root causes produce the same symptom: `heimdall-mcp index` reports success, but `heimdall_status` / `heimdall_search` keep returning pre-reindex results.

**1a — MCP `runIndex` writes to the server's cwd, not the caller's target.** `tools.go:286-287` (`runIndex`) and `:565-566` (`toolStatus`) compute `baseDir := filepath.Join(os.Getwd(), ".heimdall_db")`. `os.Getwd()` here is the MCP server process's cwd — typically the Claude Code project root, **not** the project the caller asked to index. `toolIndex` correctly passes `absPath` for the indexer root but `runIndex` ignores it for baseDir. `tools.go:332` then registers the wrong baseDir. `autoIndexOnSearch` at `:435` also uses `os.Getwd()` but there it is documented behavior (no explicit target) and legitimate.

**1b — `ModelDBDir` mixed-state reader prefers empty canonical over populated legacy.** `dbpath.go:30-43`: the fallback to `<model>_latest/` only fires when `os.Stat(<model>/)` errors. If anything ever creates an empty `<model>/` (partial MkdirAll, or a post-#90 MCP run writing under the wrong baseDir per 1a), reads silently land on the empty dir while populated data lives in `<model>_latest/`. No mtime- or content-based tiebreak. Verified on `payments-analyzer` wrapper.

**1c — `ListAvailableModels` leaks the `_latest` suffix.** `dbpath.go:47-62` returns raw directory names. A caller iterating models sees `nomic-embed-text_latest` as a distinct model. `IndexSubRepos` uses this list as "caller-wins when model=''" and feeds the suffix-bearing name back into `ModelDBDir`, which re-sanitizes to `nomic-embed-text_latest_latest`. Compounds 1b.

**Affected files:** `internal/mcp/tools.go` (runIndex, toolStatus, registry call at `:332`); `internal/heimdall/dbpath.go` (`ModelDBDir`, `ListAvailableModels`, `ResolveUsableModelDB` — the last also checks only directory existence, not `vectors.db` presence).

---

## Problem #2 — `sub_project` filter always returns zero

Schema is wired end-to-end (`store.go:136-138` ALTER TABLE, `:220-229` WHERE, `:342` INSERT) and the MCP tool exposes the filter. But every row in every store on disk has `sub_project=''`. Three producer-side bugs:

**2a — Outer indexer cannot see sub-repo names.** `indexer.go:434-445` does `filepath.SkipDir` on every sub-repo dir. `subProjectForFile` (`:780-789`) tags a chunk only when the file's first path component matches a known sub-repo name — but the outer walk never enters those dirs, so no such file is visited. Outer chunks are correctly empty (they *are* wrapper chunks); the rule just never discriminates against outer data.

**2b — Sub-repo indexer sees no self-reference.** `IndexSubRepos` at `indexer.go:295` spawns each sub-indexer with `idx.root = subAbs`. `DiscoverSubRepos(idx.root)` at `:374` enumerates the sub-repo's *children*, not itself. The sub-repo's own basename is never in `subRepoDirs`; `subProjectForFile` returns `""` for every file. On-disk verification: `SELECT DISTINCT sub_project FROM entries` against any sub-repo's `vectors.db` returns exactly one row, empty string.

**2c — Git indexer and `toolIndexText` never write `SubProject`.** `git_indexer.go` constructs commit records without ever setting the field (zero-value `""`). `toolIndexText` does the same for externally ingested text. Even with 2a/2b fixed, these writers stay silently untagged — a producer/consumer mismatch that will recur with every future writer.

**Affected files:** `internal/heimdall/indexer.go`, `internal/heimdall/git_indexer.go`, `internal/mcp/server.go` (`toolIndexText`). `internal/mcp/memory_tools.go` is explicitly out of scope (memories are a separate table).

---

## Problem #3 — Stale sub-repo registry entries persist indefinitely

Verified on disk: `projects.json` contains `payments-analyzer-app` with `dbPath = .../payments-analyzer/.heimdall_db` (outer wrapper's dir), not the sub-repo's own correct dir. Pre-#67 row from before sub-repos got their own DBs.

The current register logic (`cli.go:342-347`, `tools.go:362-367`) *does* fire on incremental no-op runs (`sr.Err == nil && sr.Result != nil`). `Registry.Register` (`registry.go:68-74`) dedupes by `Name OR Path` and always overwrites `DBPath` on match — so register *should* be fixing the row. That it isn't means the sub-repo either isn't being discovered, errors earlier in `IndexSubRepos`, or the MCP `os.Getwd()` bug (1a) caused writes to land elsewhere so the right dbPath was never constructed. The observed drift is downstream of 1a.

Separately, the dedupe-by-`Name OR Path` is itself a latent bug: two distinct projects sharing a sub-repo basename (two wrappers each with a `shared/`) clobber each other on registration. Any repair approach must also handle this clobber risk.

**Affected files:** `internal/registry/registry.go` (dedupe key, silent `json.Unmarshal` error-swallow at `:42`, non-atomic `Save`); `internal/heimdall/indexer.go` `IndexSubRepos` (register-after-success ordering); `internal/cli/cli.go`, `internal/mcp/tools.go` (call sites).

---

## Problem #4 — `heimdall_ls` shows sparse sub-repo hierarchy

Downstream of Problem #2. `ListByContextPath` (`store.go:586-632`) groups by `context_path`, derived from paths relative to the indexer root — so a sub-repo's own store yields entries like `src`, not `alpha/src`. The outer store never sees sub-repo files (skipped by outer walk). `heimdall_ls` at the wrapper root shows only wrapper children; the sub-repo hierarchy is invisible. Additionally, `toolLs` (`tools.go:700-724`) reads one store and does not fan out across the registry. Most of the symptom dissolves once Problem #2 is fixed; the fan-out question is a UX follow-up.

**Affected files:** `internal/mcp/tools.go` (`toolLs`), `internal/heimdall/store.go` (`ListByContextPath`).

---

## Problem #5 — Ollama parallel timeouts under burst indexing

Independent of the storage/registry cluster. `ollama.go` has no client-level semaphore around concurrent embed calls. Per-call timeout is 30s plus 2s/text for batches. When the indexer fans out across multiple sub-repos firing batch embeds in parallel, Ollama's single-slot queue (default `OLLAMA_NUM_PARALLEL=1` on consumer hardware) serializes them; the per-call deadline fires before Ollama dequeues. Burst indexing of 5+ sub-repos produces stochastic `context deadline exceeded` errors.

**Affected files:** `internal/heimdall/ollama.go`.

---

## Review findings worth preserving

Principles abstracted from the PR #73 / PR #74 review rounds. Evergreen; describe failure modes, not specific fixes.

- **Silent-failure hygiene.** Recurring pattern of discarding errors (`json.Unmarshal` unchecked, migrations returning `(false, nil)` on contention with no log, env parsing that accepts only literal `"1"` and fails-open otherwise, `defer os.Remove` without capturing failures). Migration code must log on every no-op branch so users can distinguish "nothing to do" from "couldn't do it."
- **Lock-file hygiene.** `O_EXCL` lock files leak permanently when the process dies before `defer` runs (Ctrl-C, SIGKILL, `os.Exit`). Stale-lock detection is required — age-based via `os.Lstat` mtime (not `os.Stat`, to refuse attacker-planted symlinks). Use a single explicit deferred closure for Close+Remove, not two `defer` lines; reviewers will silently break ordering on a "simplifying" refactor.
- **SQLite WAL awareness.** Opening a store read-only with `immutable=1` bypasses the WAL and returns a stale snapshot under any concurrent writer. Readers computing "which store is newer" via row count or `MAX(mod_time)` must use `?mode=ro&_journal_mode=WAL&_busy_timeout=5000` matching the writer side.
- **Filesystem-mtime tiebreaks are unreliable.** WAL checkpoints bump `vectors.db` file mtime without inserting rows. Content-aware tiebreaks (row count, `MAX(mod_time) FROM entries`) are correct; file mtime only as fallback when the DB cannot be opened.
- **Path resolution must be explicit.** Every MCP tool entry point needs a resolver that preserves caller intent: registry-first lookup, exact path match, cwd only as documented last resort. `Registry.Find`'s substring/partial-name matching is unsafe for absolute-path inputs. Apply `filepath.Clean` on both sides of equality comparisons and `filepath.EvalSymlinks` before registry writes to dedupe aliased paths.
- **Config must be transient when env-sourced.** Env overrides applied to the persisted `Config` struct get baked to disk by the next `SaveConfig`. Effective-config snapshots are required. Sentinel-vs-zero semantics (e.g. `0 = unbounded` vs `-1 = unset`) require a raw-map JSON pre-pass to distinguish "absent key" from "explicit zero." Clamp integer env vars at parse time to prevent `time.Duration` overflow and retry-amplification DoS.
- **Concurrency primitives need server scope.** Per-client semaphores bound nothing when each caller builds its own client. The client must be a server-level singleton keyed on its config tuple, rebuilt only on change. Acquire+release via `defer` on every exit path (panic, ctx cancel, decode error, HTTP error). `time.After` leaks a goroutine on cancel — use `time.NewTimer` + `t.Stop()`.
- **Test determinism.** `time.Sleep`-based synchronization is flaky under `-race` and loaded CI. Use event-driven `started chan struct{}` + `waitStarted(N)` patterns. Mtime tests must use `os.Chtimes(path, time.Unix(100,0), ...)` with explicit timestamps — APFS has 1-second granularity and `time.Sleep(5ms)` does not differentiate mtimes. Prefer behavioral assertions over unexported-field white-box reads.

---

## Known edge cases

- **iCloud-synced paths on macOS.** `~/Code/` may be under `~/Library/Mobile Documents/com~apple~CloudDocs/`. Files can be "evicted" pointer-only state; `os.Rename` may stall or fail. `.icloud` pointer files signal this.
- **APFS case-insensitivity.** `<Model>/` and `<MODEL>_latest/` may collapse to the same directory on macOS default FS. Rename succeeds but target name may flip case.
- **Symlinked sources.** If `<model>_latest` is a symlink, `os.Rename` renames the link, not the target. `Lstat` first, skip-with-WARN on symlinks. Same applies to lockfile age checks.
- **Cross-filesystem renames.** `os.Rename` returns EXDEV on Linux when source and target are on different filesystems (atypical but plausible when `$TMPDIR` is tmpfs). Copy-then-delete fallback required.
- **Stale pre-#67 registry entries.** The observed `payments-analyzer-app` row (dbPath pointing at the outer wrapper's `.heimdall_db`) is the canonical reproducer. A repair pass must rewrite when `entry.DBPath != filepath.Clean(filepath.Join(entry.Path, ".heimdall_db"))` AND the canonical location exists and contains a `vectors.db` under some model subdir. Simpler "not a dir / no vectors.db" criteria miss this — the outer wrapper's dir *is* valid, just not the right one for the sub-repo.
- **Downstream repos commit `.heimdall_db/`.** `payments-analyzer` (merged with `_latest/` paths), `payments-analyzer-app/-service/-infra` (non-`_latest/`). Any `_latest → canonical` rename surfaces as mass-delete + mass-create in the first post-migration commit; `git commit -a` lets rename detection collapse the diff. Downstream CI-ignore patches may be missing on some of these repos.
- **Concurrent indexer + MCP on the same project.** Rename-vs-open races are not protected by the rename-vs-rename lock. A reader mid-`OpenStore` on the legacy path during migration risks WAL-file orphaning. Test coverage gap in the original planning round.
- **Corrupt `projects.json`.** `json.Unmarshal` failure is silently swallowed and the registry resets to empty. Atomic save (temp-file + rename) on write; log-and-preserve on read for forensics.
