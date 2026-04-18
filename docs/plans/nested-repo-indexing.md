# Nested Repo Indexing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `heimdall-mcp index <path>` (CLI) and the `heimdall_index` MCP tool index both the outer wrapper AND each nested sub-repo as independent, auto-registered projects — each with its own `.heimdall_db/` inside itself — while giving the user a `--exclude` flag to opt out, and replacing the opaque `Skipped: N` summary with categorized counters.

**Architecture:** The outer-repo walk stays exactly as-is (outer files still land in outer `.heimdall_db/`). After that outer walk completes, we loop over every `DiscoverSubRepos(outerRoot)` entry that was not excluded, spawn a fresh `Indexer` with the sub-repo as `root` and the sub-repo's own `.heimdall_db/` as `baseDir`, inherit the outer's model (walking up from any existing `.heimdall_db/` metadata if the outer hasn't been indexed yet with that model), and register each sub-repo in the shared project registry. A new `IndexResult.Skip` struct with four counters (`UserExcluded`, `SubRepo`, `Binary`, `Unchanged`) replaces the opaque `FilesSkipped`. **Default-excluded hygiene directories** (`.git`, `.heimdall_db`, `node_modules`, `vendor`, `__pycache__`, `.idea`) are NOT counted — the `UserExcluded` counter reflects only patterns the user actually added via `cfg.ExcludePatterns` or `--exclude`. `ChunkerOpts.ExcludeGlobs` gets the `--exclude` additions merged in at CLI-parse time.

**Tech Stack:** Go 1.22+, `internal/heimdall` (indexer + dbpath), `internal/cli` (flag parsing + summary rendering), `internal/registry` (ProjectEntry), `internal/config` (global `ExcludePatterns`), `internal/mcp` (`heimdall_configure`, `heimdall_index`), standard lib `testing` with table-driven tests.

---

## Table of Contents

1. [Impact Analysis](#1-impact-analysis-pre-change-per-claudemd)
2. [Design — Goal by Goal](#2-design--goal-by-goal)
3. [Contract: Nested Sub-Repo Discovery & Indexing](#3-contract-nested-sub-repo-discovery--indexing)
4. [DB Path Strategy](#4-db-path-strategy)
5. [Registry Schema](#5-registry-schema)
6. [Exclude Flag Semantics](#6-exclude-flag-semantics)
7. [Config Persistence](#7-config-persistence)
8. [Edge Cases](#8-edge-cases)
9. [File Structure](#9-file-structure)
10. [Test Strategy (TDD)](#10-test-strategy-tdd)
11. [CLI UX](#11-cli-ux)
12. [Docs Updates](#12-docs-updates)
13. [Rollout](#13-rollout)
14. [Task Breakdown](#14-task-breakdown)
15. [Escalations](#15-escalations)

---

## 1. Impact Analysis (pre-change, per CLAUDE.md)

Every touched subsystem and the proof it will still work after the change:

| Subsystem | File:Lines | Change | Still works because |
|---|---|---|---|
| Indexer walker (outer) | `internal/heimdall/indexer.go:122-166` | Walker still runs against outer root; sub-repo directories still get `filepath.SkipDir`. No change to what outer indexes. | The `SkipDir` branch at `:141-146` is kept. Goal 1 (outer wrapper still indexed) is preserved by construction — we only *add* a second pass for sub-repos. |
| Indexer walker (sub-repo handling) | `internal/heimdall/indexer.go:141-146` | `.git` dir check extended to also accept `.git` as a file (worktree gitlink). The surrounding skip logic is unchanged. | `os.Stat` already returns info for both files and dirs; we drop `info.IsDir()` and also accept regular files. See `hasRepoMarker` in `scope.go:42-53` for the existing precedent. |
| `DiscoverSubRepos` | `internal/heimdall/dbpath.go:116-132` | Same extension: accept `.git` file (worktree gitlink) not just dir. Add a second return value (or a sibling function) that returns **absolute paths** so callers can skip path-joining. | `os.Stat` on a gitlink file succeeds. Existing call sites (`indexer.go:104`, `mcp/tools.go:360`) only read map keys — we keep the map shape and add a second helper. |
| `IndexResult` | `internal/heimdall/indexer.go:46-53` | Add `Skip SkipBreakdown` struct with four counters (`UserExcluded`, `SubRepo`, `Binary`, `Unchanged`). `FilesSkipped` retained as the sum for backward compat. Default-excluded hygiene dirs (`.git`, `.heimdall_db`, `node_modules`, etc.) are NOT counted. | All existing readers (CLI summary at `cli.go:313`, MCP result marshaling at `mcp/tools.go:240-246`, tests) read `FilesSkipped` and `FilesIndexed`; both still populate correctly. The existing `TestIndexAll_DoesNotSkip` (indexer_test.go:211-250) keeps its `FilesSkipped == 0` assertion — verified sound under the "user-only" counter semantics because no user patterns match in that fixture. |
| CLI flag parsing | `internal/cli/cli.go:86-110` | Add repeatable `--exclude <pattern>` parsed in the `args` preamble alongside `--out` and `--model`. | The existing loop is a hand-rolled parser, easy to extend. New flag is strictly additive. |
| CLI index orchestration | `internal/cli/cli.go:196-271` | After the existing outer-indexing loop, run a sub-repo pass that (a) filters out user-excluded sub-repos, (b) runs `indexWithModel` rooted at each sub-repo, (c) registers each sub-repo. | Same `indexWithModel` helper already handles arbitrary roots. Registry `.Register` is idempotent. |
| Registry `Register` | `internal/registry/registry.go:63-75` | No schema change — `ProjectEntry{Name, Path, DBPath}` already captures everything we need. Sub-repo `Name` = basename(subRepoAbsPath); `Path` = subRepoAbsPath; `DBPath` = `subRepo/.heimdall_db/`. | Dedupe key `p.Name == name OR p.Path == projectPath` handles re-runs correctly (sub-repo would collide by path). |
| `heimdall_configure` MCP tool | `internal/mcp/config_tools.go:34-146` | Add new config key `"exclude_patterns"` of type `[]string` (maps to `config.Config.ExcludePatterns`). `--llm-classifier-model`-style plumbing not needed; reuse `configKeys` map. | Existing validation pipeline handles `[]string` via `toStringSlice`; SaveConfig persists globally. |
| `heimdall_index` MCP tool | `internal/mcp/tools.go:202-395` | `runIndex` extended with the same sub-repo loop as CLI (indexWithModel equivalent, then register each). | `IndexGitCommits` for sub-repos already runs (`tools.go:360-368`) — that stays, but it will now target each sub-repo's OWN store, not the parent's. Must update that loop to open the sub-repo store. |
| `heimdall_projects` | `internal/cli/cli.go:564-601` + MCP equivalent | No code change — sub-repos auto-appear because they're registered. Existing `ListAvailableModels` / `store.Stats()` per project already handles the model subdirs. | Verified: `p.DBPath` is the base `.heimdall_db/` and `ListAvailableModels` enumerates model subdirs. |
| `heimdall_search` | `internal/mcp/tools.go:30-175` | No code change — registry lookup finds each registered sub-repo; existing `SubProject` filter still works when user searches within outer. | The outer's DB still tags sub-repo-relative files with `SubProject` via `subProjectForFile` (kept for backward-compat even though we now also index sub-repos separately; this gives the outer DB "names" of sub-repos for the deprecated filter path). |
| Global `ExcludePatterns` | `internal/config/config.go:17,46` | Per-invocation `--exclude` additions are **layered on top** of the global config, not persisted unless the user explicitly runs `heimdall_configure` or `heimdall-mcp config set`. | `ChunkerOpts.ExcludeGlobs` already does this compositing (`indexer.go:396`) and defaults are merged in `shouldExclude`. |
| Tests at `indexer_test.go:252-314, 316-368` | Encode the OLD skip contract. | Rewritten to assert sub-repos are **discovered** for separate indexing (new contract) and file-walk excludes them from the outer. Outer-only assertions kept verbatim. | See §10 for exact new assertions. |
| Tests at `integration_test.go:105-200` | Filter-by-SubProject tests. | Unchanged — the outer's DB still has SubProject-tagged rows when someone indexes with `IncludePaths` pointing at a sub-repo. | Verified SubProject stamping path at `indexer.go:240, 274, 299` is only invoked when a file's first path segment matches `subRepoDirs` — still occurs when `IncludePaths` forces it. |
| `.claude/worktrees` default exclude | `config.go:46` | Already default-excluded. If someone names a sub-repo inside `.claude/worktrees/`, it is excluded from both outer indexing AND sub-repo discovery. | Walker rejects `.claude/worktrees` before reaching `os.Stat(.git)`. |

## 2. Design — Goal by Goal

### G1. Always index outer wrapper (no regression)

**Change:** Nothing about the outer walk changes. The sub-repo skip branch in `indexer.go:140-146` stays.

**Why it holds:** The outer index path today writes to `<outer>/.heimdall_db/<model>/vectors.db`. After the change, that path is unchanged. The sub-repo pass runs *after* the outer pass and writes to `<outer>/<sub>/.heimdall_db/<model>/vectors.db` — a disjoint directory tree.

**Test:** `TestIndexAll_OuterWrapperStillIndexed` (see §10). Assert `store.HasFile("main.go")` AND `!store.HasFile("sub-service/app.go")` (sub files are NOT merged into outer).

### G2. Index nested sub-repos (each with own DB)

**Change:** New public function `Indexer.IndexSubRepos(ctx, progress)` that:
1. Calls `DiscoverSubReposAbs(root)` (new helper, see §4) to get abs paths.
2. For each sub-repo abs path NOT in the user-exclude list:
   - Opens (creates) `<subRepo>/.heimdall_db/<modelSanitized>/vectors.db` via `OpenStore`.
   - Builds a fresh `Indexer` rooted at the sub-repo with the same `ChunkerOpts`.
   - Calls `IndexAll(ctx)` (or `IndexIncremental` in the MCP stale-check path).
3. Returns a `[]SubRepoResult` summarizing what happened for each.

**Orchestration:** The CLI `cliIndex` function (and the MCP `runIndex` goroutine) drives both passes: outer first, then `IndexSubRepos`. The indexer package does **not** touch registry — registration is the orchestrator's job (CLI / MCP).

### G3. Auto-register sub-repos in registry

**Change:** After each sub-repo indexes successfully:

```go
subName := filepath.Base(subRepoAbsPath)
subBaseDir := filepath.Join(subRepoAbsPath, ".heimdall_db")
reg.Register(subName, subRepoAbsPath, subBaseDir)
```

Name collision handling: `registry.Register` already dedupes by `Name OR Path` (`registry.go:68-73`). If an outer and a sub-repo happen to share a basename (e.g. two `frontend/` dirs across workspaces), the dedupe-by-path branch still handles it because paths are absolute. We add a test that verifies this.

**Save:** Call `reg.Save()` **once** after all sub-repos are processed (not per sub-repo) — cheaper and the registry is small.

### G4. Sub-repo inherits parent's embedding model config

**Change:** For each sub-repo, the "model to use" is picked in this precedence:

1. **Sub-repo already has an existing index** (`ListAvailableModels(subRepo/.heimdall_db)` returns non-empty) → reuse the first existing model. **Always wins, even when the user passes `--model X` for the outer run.** This preserves the user's prior per-sub-repo pin. The CLI summary surfaces the mismatch explicitly via `[<model> — pinned]` next to the sub-repo line (§G6) so it's user-visible, not just a log entry (M2 resolution).
2. **Explicit `--model` flag** (user-passed for this CLI invocation) → applies to the outer AND to any sub-repos that are NOT already pinned (rule 1 is empty).
3. **Outer was just indexed with model X** → sub-repo inherits X.
4. **Fallback** → `cfg.Model` (global default, typically `nomic-embed-text`).

**"Don't clobber" (rule 1 > rule 2):** If a sub-repo has model Y (rule 1) AND user explicitly passed `--model X` (rule 2), keep Y. Emit a stdout note during the CLI run AND mark the sub-repo line with `[Y — pinned]`. This gives users a way to run `heimdall-mcp index /path/to/sub --model bge-m3` first to pin that sub-repo, then `heimdall-mcp index /path/to/outer --model nomic-embed-text` later without losing the pin. Rationale: explicit `--model` is scoped to "what should be created" not "what should be overwritten".

**User-visible surfacing (M2 resolution):** Every sub-repo summary line shows `[<model>]`. Lines where rule 1 beat rule 2 OR rule 1 beat rule 3 get ` — pinned` in the bracket. The CLI also prints, once per run when at least one pin was preserved: `Preserved pinned model(s) in sub-repo(s): sub-infra=bge-m3. Use --force-model (not implemented) to overwrite.` (The note is informational; the `--force-model` flag is intentionally NOT shipped in this PR — it's a hook for future work.)

**First-index case (outer has no config):** `cfg.Model` from global config is always defined by `DefaultConfig()` (`config.go:43`). So there's always a fallback.

### G5. `--exclude` CLI flag + `heimdall_configure` + per-invocation persistence

**CLI:** Repeatable. Syntax: `--exclude <pattern>`, parsed in the loop at `cli.go:92-102`.

```go
// new
var excludeFlags []string
// ...
} else if args[i] == "--exclude" && i+1 < len(args) {
    excludeFlags = append(excludeFlags, args[i+1])
    i++
}
```

Then in `cliIndex`, the effective exclude list is `append(cfg.ExcludePatterns, excludeFlags...)` passed into `ChunkerOpts.ExcludeGlobs`.

**MCP `heimdall_configure`:** New config key `exclude_patterns` (type `[]string`). Invoking `heimdall_configure` with action=set, key=`exclude_patterns`, value=`["node_modules","dist","third_party/vendor"]` overwrites `cfg.ExcludePatterns` and persists via `SaveConfig`. Append semantics are NOT provided by this tool — users fetch the current list, modify, and set back (same pattern as `git.branches`).

**Sub-repo exclusion:** Because `shouldExclude` in `indexer.go:393-410` matches both individual path components AND the full slash-joined path, a user can exclude a sub-repo by its directory name (e.g. `--exclude sub-service`) or by its relative path (e.g. `--exclude vendor/golang-sub`). Our **sub-repo discovery pass** respects the same exclude list (see §3).

### G6. CLI summary breakdown

**Change:** Replace `IndexResult.FilesSkipped int` usage with new fine-grained counters:

```go
type SkipBreakdown struct {
    UserExcluded int // hit shouldExclude AGAINST a user-configured pattern (cfg.ExcludePatterns ∪ --exclude). Counts both dir-prune and per-file hits.
    SubRepo      int // hit sub-repo skip branch (directories; files NOT counted — see note)
    Binary       int // hit isBinaryFile
    Unchanged    int // hit isUpToDate during incremental
}

type IndexResult struct {
    FilesScanned  int
    FilesIndexed  int
    FilesSkipped  int        // == sum of Skip.UserExcluded + SubRepo + Binary + Unchanged
    Skip          SkipBreakdown
    ChunksCreated int
    Duration      time.Duration
    Errors        []string
}
```

**What `UserExcluded` counts (H1 resolution):** Only patterns the user added — i.e. `cfg.ExcludePatterns` (global config) ∪ `--exclude` CLI flags ∪ MCP `exclude_patterns`. The ALWAYS-on `defaultExcludes` list inside `indexer.go:shouldExclude` (`.git`, `node_modules`, `.heimdall_db`, `vendor`, `__pycache__`, `.idea`) is **not** counted. This preserves the semantics of the existing `TestIndexAll_DoesNotSkip` assertion (`FilesSkipped == 0`) AND matches the better UX — the user cares about what THEY excluded, not default hygiene.

**Implementation split:** Refactor `shouldExclude(relPath)` into two helpers: `isDefaultExcluded(relPath) bool` (matches only `defaultExcludes`) and `isUserExcluded(relPath) bool` (matches only `idx.opts.ExcludeGlobs`). The walker calls both; only `isUserExcluded` bumps `Skip.UserExcluded`. Either triggers `SkipDir` / file-skip. `shouldExclude` is kept as `return isDefaultExcluded(p) || isUserExcluded(p)` for any remaining callers so behavior is unchanged for non-counter paths.

**Note on `SubRepo` counting:** We count **directories** skipped via `filepath.SkipDir`, not the files inside them (which we never statted). This matches how users think ("I skipped 2 sub-repos", not "I skipped the 8000 files inside 2 sub-repos").

**New CLI output format** (mocked):

```
Done.
  Model:    nomic-embed-text
  Elapsed:  12s
  Scanned:  342 files
  Indexed:  318 files
  Skipped:  24 files
    └─ 18 unchanged (incremental)
    └─  4 excluded by pattern
    └─  2 binary
  Sub-repos: 3 indexed separately
    └─ sub-service     [nomic-embed-text]  (12 files, 84 chunks)  → /abs/outer/sub-service/.heimdall_db
    └─ sub-infra       [bge-m3 — pinned]    ( 7 files, 32 chunks)  → /abs/outer/sub-infra/.heimdall_db
    └─ tools/generator [nomic-embed-text]  ( 4 files, 18 chunks)  → /abs/outer/tools/generator/.heimdall_db
  Note: first indexing of sub-repos may take longer; subsequent runs are incremental.
  Chunks:   1842
  Database: /abs/outer/.heimdall_db
```

**Model-in-brackets notation (M2 resolution):** Every sub-repo line shows its effective model in `[...]`. Sub-repos where the pinned model (pre-existing store) differs from the outer's model get a `[bge-m3 — pinned]` suffix so the user sees exactly which sub-repo kept its own model even when they ran `index outer` with a different one.

**First-time hint (L3 resolution):** When sub-repos are discovered for the first time (any of them had no prior store), append a one-line note: `"Note: first indexing of sub-repos may take longer; subsequent runs are incremental."`. Omit otherwise.

**Failure / nil-result rendering (L2 resolution):** If `SubRepoResult.Err != nil`, render `  └─ sub-service   FAILED: <err>` instead of file/chunk counts. See L2 invariant in §5.

When `Skip.SubRepo > 0` AND no sub-repos were indexed (e.g. all excluded), the "Sub-repos" section reads:

```
  Sub-repos: 0 indexed (2 discovered, both excluded by --exclude)
```

**CLI output lines that change (M5 resolution):** The only line whose text content changes is `cli.go:313` — previously `"Skipped: %d files (unchanged)\n"`, now `"Skipped: %d files\n"` followed by indented breakdown lines. The additions ("Sub-repos:", the "Note:" hint) are new lines that don't collide with any existing grep. **Test impact:** search `internal/cli/` and `internal/mcp/` tests for any assertion that matches `"(unchanged)"` or starts-with `"Skipped:"` — update those assertions as part of Task 11. (Verified: no current tests match that exact string; if Task 11 discovers any, fix them in the same commit.)

## 3. Contract: Nested Sub-Repo Discovery & Indexing

**Decision:** Post-walk enqueue. The walker itself does NOT recurse into sub-repos or call an indexer. Instead:

1. The outer walk runs to completion and still skips sub-repo directories (unchanged behavior).
2. After the outer walk, the orchestrator (CLI `cliIndex` / MCP `runIndex`) iterates `DiscoverSubReposAbs(outerRoot)` (filtered by exclude list) and builds a fresh `Indexer` per sub-repo, driving each one independently.

**Why this over (a) recursive call from inside the walker:**
- Clean separation: `Indexer` stays stateless-per-root. Easier to test and reason about.
- Per-sub-repo model resolution, store opening, registry writes, and progress reporting live in the orchestrator where they belong.
- Avoids re-entrant walks inside `filepath.WalkDir` callbacks (which is error-prone — you'd need to not call `SkipDir` and also not descend, which is inherently contradictory).

**Why this over (b) enqueue during walk, drain after:**
- Same result as our choice in practice, but decoupling discovery from the walker via `DiscoverSubReposAbs` is clearer and already tested.

**Circular sub-repo (sub-repo inside a sub-repo):** `DiscoverSubRepos` only scans **immediate** subdirectories (`os.ReadDir(root)`). When we recurse *logically* by spawning an indexer rooted at the sub-repo, that new indexer will itself run `DiscoverSubRepos(subRepoRoot)` and pick up deeper sub-repos. Naturally handled. We add a test `TestIndexAll_NestedSubRepoInSubRepo` to prove it.

## 4. DB Path Strategy

Each sub-repo's DB lives **inside the sub-repo**, following the same convention as any standalone index:

```
<outer>/
  .heimdall_db/
    nomic-embed-text/
      vectors.db            ← outer
  sub-service/
    .git/
    .heimdall_db/
      nomic-embed-text/
        vectors.db          ← sub-service (its own DB)
  sub-infra/
    .git/
    .heimdall_db/
      nomic-embed-text/
        vectors.db          ← sub-infra (its own DB)
```

Helper (new, in `dbpath.go`):

```go
// DiscoverSubReposAbs returns the absolute paths of immediate subdirectories
// of root that contain a .git entry (directory or file — .git may be a
// gitlink in worktrees). Read errors on root are returned (NOT silently
// swallowed — see L1 in review). Symlinks are NOT followed: matches the
// industry-standard default (git, fd, ripgrep). An entry whose type is a
// symlink (entry.Type()&os.ModeSymlink != 0) is ignored even if it would
// resolve to a directory with .git/.
func DiscoverSubReposAbs(root string) ([]string, error) {
    entries, err := os.ReadDir(root)
    if err != nil {
        return nil, fmt.Errorf("read sub-repo root %s: %w", root, err)
    }
    var out []string
    for _, entry := range entries {
        if !entry.IsDir() {
            continue // regular files AND symlinks (entry.IsDir is lstat-based on Unix, so symlinks return false)
        }
        // Defense-in-depth: reject symlinks explicitly in case a platform
        // reports them as dirs.
        if entry.Type()&os.ModeSymlink != 0 {
            continue
        }
        gitPath := filepath.Join(root, entry.Name(), ".git")
        if _, err := os.Stat(gitPath); err == nil {
            out = append(out, filepath.Join(root, entry.Name()))
        }
    }
    return out, nil
}
```

**Symlinked sub-repos (M4 resolution):** NOT auto-discovered. This matches `git`, `fd`, and `ripgrep` defaults and avoids the double-indexing hazard where `outer-a/child → outer-b/child` (via symlink) gets indexed twice under different absolute paths. Documented in §8 edge case 11 and in README. No flag to override in this PR (deferred to a follow-up if demand arises).

**Read-error propagation (L1 resolution):** The signature returns `([]string, error)`. Callers in `cli.go` and `mcp/tools.go` MUST check the error, log it to stderr / MCP log, and treat it as a sub-repo discovery failure (do NOT index the outer's sub-repos, but the outer pass itself still succeeds). The existing `DiscoverSubRepos` (map-returning) is left alone so we don't churn the `SubProject`-tagging code path; only the new helper propagates errors.

`DiscoverSubRepos` (existing, used for `SubProject` tagging) stays — we also extend it to accept `.git` as file (worktree gitlink):

```go
// existing at dbpath.go:116-132 — replace the `info.IsDir()` check with an
// existence-only check:
if _, err := os.Stat(gitDir); err == nil {
    result[entry.Name()] = true
}
```

## 5. Registry Schema

**No migration needed.** `ProjectEntry` already has the three fields we need:

```go
// registry.go:11-15 (VERIFIED)
type ProjectEntry struct {
    Name   string `json:"name"`
    Path   string `json:"path"`
    DBPath string `json:"dbPath"`
}
```

`Register(name, projectPath, dbPath)` at `registry.go:64-75` dedupes by name OR path — handles re-runs. `projects.json` format stays compatible with older heimdall-mcp versions.

**Sub-repo naming:** We use `filepath.Base(subRepoAbsPath)`. If two sub-repos across different outer projects have the same basename (e.g. `/proj-a/frontend` and `/proj-b/frontend`), the dedupe-by-path branch still distinguishes them because `Path` is absolute. Added test: `TestRegistry_SubReposWithSameBasenameAcrossProjects`.

### 5.1 `SubRepoResult` invariants (L2 resolution)

```go
type SubRepoResult struct {
    Path   string       // absolute path to the sub-repo; ALWAYS populated
    Name   string       // basename(Path); ALWAYS populated
    DBPath string       // absolute path to <sub>/.heimdall_db; ALWAYS populated
    Model  string       // effective model; populated iff store-open succeeded
    Result *IndexResult // iff Err == nil AND indexing proceeded; MAY BE NIL otherwise
    Err    error        // non-nil iff this sub-repo failed; callers MUST check before dereffing Result
}
```

**Invariant (documented in code godoc):** `result.Err != nil ⟹ result.Result may be nil`. Callers that render the summary or register the entry MUST gate on `Err == nil && Result != nil` before dereferencing `Result.FilesIndexed` / `Result.ChunksCreated`. Failures are NOT registered (we only register sub-repos that indexed successfully — otherwise `heimdall_projects` would list broken entries).

## 6. Exclude Flag Semantics

**Matching:** Same as existing `shouldExclude` (`indexer.go:393-410`): `filepath.Match` glob matching against either individual path components OR the full slash-joined relative path. This is what users get today for `cfg.ExcludePatterns` — we just add a CLI pathway and an MCP config key.

**Industry-standard reference:**
- `ripgrep --glob !pattern` → glob, relative to walk root, inverted to include.
- `fd --exclude pattern` → glob, matched against each filename AND each path component (same as us).
- `semgrep --exclude` → glob, matched against path components.

**Decision:** Match `fd` semantics (closest to what we already do). **Not** full `.gitignore` semantics (too complex for this scope).

**Scope (canonical form — M6 resolution):** Every exclude pattern is interpreted **relative to the project root** or as a bare glob matching any path component — never as an absolute path. This canonical form is enforced **at both CLI and MCP boundaries** to avoid the inconsistency called out in M6:

- **CLI (`--exclude`):** Reject any entry that begins with `/` (or matches `filepath.IsAbs`). Emit `fmt.Fprintln(os.Stderr, "--exclude takes a glob or project-relative path, not an absolute path: "+p)` and exit non-zero.
- **MCP (`heimdall_configure set exclude_patterns`):** Run the same `filepath.IsAbs` check inside `configKeys["exclude_patterns"].Set`. Return `fmt.Errorf("exclude_patterns[%d] %q is absolute; use a glob or project-relative path", i, p)` so the MCP client sees a structured error and the config is NOT saved.
- **Shared validator:** Extract `validateExcludePatterns([]string) error` in `internal/config/validate.go` (or nearest equivalent) and call from both surfaces so the rule cannot drift.

**Glob syntax limitation:** `filepath.Match` does NOT support `**` (recursive wildcard). Documented in README + `--help`. If a user writes `--exclude 'src/**/generated'`, it matches literally (won't expand) — they must use `--exclude generated` to hit any `generated/` anywhere.

**Composition with defaults:** Defaults (`.git`, `node_modules`, etc. from `indexer.go:395`) are ALWAYS applied. User excludes are **added** (union), never replace.

**Composition with `cfg.ExcludePatterns`:** Global config excludes are ALWAYS applied. CLI `--exclude` flags are **added** on top for that invocation only. `heimdall_configure set exclude_patterns` replaces `cfg.ExcludePatterns`.

**Effective list passed to indexer:** `append(cfg.ExcludePatterns, excludeFlagsFromCLI...)`. Order matters for neither correctness nor performance (we match everything).

**Sub-repo exclude semantics:** If a user pattern matches a sub-repo's directory name or relative path, that sub-repo is:
- NOT walked into by the outer pass (existing behavior via `shouldExclude`).
- NOT counted in `Skip.SubRepo` — counted in `Skip.UserExcluded` instead.
- NOT indexed as a separate project (orchestrator filters the discovery list against the same user patterns).
- NOT auto-registered.

Precedence test: `TestExcludePrecedenceSubRepo` (see §10).

## 7. Config Persistence

**Global:** `config.Config.ExcludePatterns` (`config.go:17,46`). Persisted via `SaveConfig` (`config.go:119-141`). Already in place — we just add an MCP config key and a CLI surface.

**Per-project:** NOT added. Rationale: we don't have a per-project config today; introducing one is a bigger migration (new `.heimdall/config.json` loader, precedence rules, test coverage — ~4 tasks) and it's a separate plan. Instead, the CLI `--exclude` flag is per-invocation; if a user wants a persistent per-project config, they set it globally (or run `heimdall-mcp config set exclude_patterns '[...]'` from within that project's dir — still global).

### 7.1 Out of scope (explicit deferral — M1 resolution)

- **Per-project config file (e.g. `.heimdall/config.json` inside the repo).** The user's original request said "persistable via `heimdall_configure` / project config file". We interpret that as the global config under XDG (which is what `heimdall_configure` writes today). Per-project files (industry pattern: `.ripgreprc`, `.eslintrc`) are a strictly larger change and sit outside the scope of this PR. If the user confirms they want per-project files, file a follow-up plan and re-enter the brainstorm — do NOT bolt it into this PR.

- **`--format=json` machine-readable summary.** Industry standard exists (`cargo --message-format=json`, `npm --json`). Deferred to a follow-up; the text format change in Task 11 is called out explicitly in §11.3 so scrapers can adapt.

- **Symlink-following sub-repo discovery.** Deferred per M4 resolution in §4.

- **Per-project embedding model selection beyond the "pinned store wins" rule.** The M2 bracketed-model hint is the UI surface; deeper policy (e.g. "always use model X for sub-repos matching pattern Y") is out of scope.

**MCP `heimdall_configure`:** Add to `configKeys` map at `config_tools.go:34-146`:

```go
"exclude_patterns": {
    Type: "[]string",
    Get:  func(c *config.Config) any { return c.ExcludePatterns },
    Set: func(c *config.Config, v any) error {
        s, err := toStringSlice(v)
        if err != nil {
            return fmt.Errorf("expected []string for exclude_patterns: %w", err)
        }
        c.ExcludePatterns = s
        return nil
    },
},
```

**Also add** to `getConfigKey`/`setConfigKey` in `internal/cli/cli.go:703-780` so the `heimdall-mcp config set exclude_patterns '[...]'` CLI form works too (for parity with the MCP tool).

## 8. Edge Cases

| # | Edge Case | Handling |
|---|---|---|
| 1 | User points indexer AT a git repo (the root IS a sub-repo) | `DiscoverSubReposAbs(root)` returns the sub-repos INSIDE root; it does NOT include root itself. The root is still indexed as a top-level project. Test: `TestIndexAll_RootIsGitRepo`. |
| 2 | Git worktree (`.git` is a file, not a dir) | Fixed in `DiscoverSubRepos` and `indexer.go:141-146` by removing `info.IsDir()` check on `.git`. Test: `TestDiscoverSubRepos_GitlinkFile`. |
| 3 | User excludes a path that IS a sub-repo via `--exclude` | User-pattern match trips first; the sub-repo is NOT walked (counted in `Skip.UserExcluded`) AND NOT added to the sub-repo indexing queue (orchestrator filters the queue against `isUserExcluded`). Test: `TestExcludePrecedenceSubRepo`. |
| 4 | Sub-repo with different embedding model already configured | Detected by `ListAvailableModels(subRepo/.heimdall_db)`. If non-empty and differs from the outer's model, **keep existing model** (rule 2 in §G4). Log `"sub-repo X pinned to model Y; outer is using Z — keeping Y"`. Test: `TestSubRepoPinnedModelNotClobbered`. |
| 5 | Parent hasn't been configured yet (first index) | Outer uses `cfg.Model` (global default). Sub-repo inherits the SAME model that the outer just used. Test: `TestSubRepoInheritsModelFirstIndex`. |
| 6 | Sub-repo inside sub-repo | Logical recursion: the sub-repo's indexer runs its own `DiscoverSubReposAbs` and spawns indexers for deeper sub-repos. Test: `TestIndexAll_NestedSubRepoInSubRepo`. |
| 7 | Two sub-repos with same basename (or sub-repo same basename as outer) | `reg.Register` dedupes by Path (absolute) → works. Test: `TestRegistry_SubReposWithSameBasenameAcrossProjects`. |
| 8 | Sub-repo contains another sub-repo that's excluded | `--exclude` matches on the INNER indexer's invocation (same global list), so the exclude propagates correctly. Tested indirectly by `TestExcludePrecedenceSubRepo`. |
| 9 | Outer has `ExcludePatterns: [".claude/worktrees"]` and user has sub-repo `foo` that is inside `.claude/worktrees/` | Excluded before discovery: `DiscoverSubReposAbs(outer)` only scans **immediate** subdirectories, so it never sees `.claude/worktrees/foo/`. The outer walker `SkipDir`s `.claude/worktrees` too. Net: not indexed, not registered. |
| 10 | Sub-repo indexing fails (Ollama flakes partway) | Outer has already completed and is committed. Sub-repo `IndexAll` returns an error; orchestrator logs it, skips registration for that sub-repo only, continues to next sub-repo. CLI exits 0 with a warning line. Test: `TestSubRepoFailureDoesNotFailOuter`. |
| 11 | Immediate sub-repo is a symbolic link to another git repo | NOT discovered (intentional — matches git, fd, ripgrep defaults; see §4 M4 resolution). If a user wants their vendored-via-symlink repo indexed, they `cd` into it and run `heimdall-mcp index .` directly. Documented in README and `--help`. |
| 12 | Exclude pattern uses `**` (gitignore-style recursive glob) | `filepath.Match` does NOT support `**` — the pattern matches literally. Documented in §6 and `--help`. Users are guided toward bare component patterns (e.g. `--exclude generated` instead of `--exclude 'src/**/generated'`). |
| 13 | User passes explicit `--model X` AND a sub-repo is pre-pinned to model Y | Explicit `--model` from the user is treated as "apply to outer ONLY". Sub-repo pin wins (rule 2 in §G4 beats rule 1 for sub-repos). The CLI summary surfaces the mismatch via the `[bge-m3 — pinned]` bracket notation (§G6). This preserves the user's per-sub-repo pin without making `--model` on the outer silently clobber it. Test: `TestSubRepoPinnedModelBeatsExplicitOuterModel`. |

## 9. File Structure

### 9.1 Files to modify

| File | Responsibility | Change summary |
|---|---|---|
| `internal/heimdall/indexer.go` | Walker + chunker pipeline | Add `Skip SkipBreakdown` to `IndexResult`; split counter updates; extend walker `.git` check; add `IndexSubRepos` orchestrator method |
| `internal/heimdall/dbpath.go` | Sub-repo discovery helpers | Add `DiscoverSubReposAbs`; extend `DiscoverSubRepos` to accept `.git` as file |
| `internal/heimdall/indexer_test.go` | Indexer tests | Rewrite sub-repo skip test into separate-indexing test; add 10 new tests (see §10) |
| `internal/cli/cli.go` | CLI entry + orchestration | Add `--exclude` flag parse; call `IndexSubRepos` after outer pass; render new summary format; add `exclude_patterns` to config get/set |
| `internal/mcp/tools.go` | MCP `heimdall_index` | Add sub-repo indexing pass in `runIndex`; register each sub-repo; open a sub-repo store for `IndexGitCommits` (the existing loop at `tools.go:360-368` currently writes sub-repo commits to the OUTER store — that's a bug we fix as part of this plan) |
| `internal/mcp/config_tools.go` | `heimdall_configure` MCP tool | Add `"exclude_patterns"` key to `configKeys` map |
| `README.md` | Top-level docs | Add a "Nested repos" section with the new CLI output |
| `docs/plans/nested-repo-indexing-TODO.md` | This plan's tracker | Update status table as tasks complete |

### 9.2 Files to create

None. All changes fit into existing files — no new packages, no new top-level sources. This keeps the diff reviewable.

### 9.3 Files NOT touched (but verified)

| File | Reason verified |
|---|---|
| `internal/registry/registry.go` | Schema supports what we need, no migration. |
| `internal/config/config.go` | `ExcludePatterns` already exists; SaveConfig handles `[]string` via JSON. |
| `internal/mcp/integration_test.go` | SubProject filter tests still pass (outer DB still has sub-project tagging via `IncludePaths` path). |
| `internal/heimdall/verify.go` | Per-store `embedding_model` verification still works; sub-repo stores stamp their own model metadata. |
| `internal/heimdall/scope.go` | `FindRepoRoot` uses `.git`-file-tolerant `hasRepoMarker`; our extension matches that precedent. |

## 10. Test Strategy (TDD)

All new tests live in `internal/heimdall/indexer_test.go` unless noted.

### 10.1 Tests REWRITTEN (OLD contract → NEW contract)

**`TestIndexAll_SkipsSubRepoDirectories` → `TestIndexAll_SubRepoFilesNotInOuterStore`** (`indexer_test.go:252-314`)

Old assertion: `FilesScanned == 3`, outer has only parent files.
New assertion: same — **file counting in outer DB is unchanged**. The old name implied "we skip sub-repos entirely", which is misleading. Rename and keep the existing assertions (they're still correct for what the outer DB contains). Add a new block to assert the return value's `Skip.SubRepo == 2` counter.

Exact new test body:

```go
func TestIndexAll_SubRepoFilesNotInOuterStore(t *testing.T) {
    // (setup block unchanged — same tempdir + 2 sub-repos + pkg/ regular subdir)
    // ...
    result, err := indexer.IndexAll(ctx)
    // ...
    // Outer store contains only outer files (unchanged):
    if result.FilesScanned != 3 { ... }
    if result.FilesIndexed != 3 { ... }
    if store.HasFile("sub-service/app.go") { t.Error(...) }
    if !store.HasFile("main.go") { t.Error(...) }
    // NEW: sub-repo skip is counted:
    if result.Skip.SubRepo != 2 {
        t.Errorf("Skip.SubRepo = %d, want 2 (sub-service, sub-infra)", result.Skip.SubRepo)
    }
    // INVARIANT: default-excluded hygiene dirs DO NOT bump UserExcluded.
    // Even though the outer tree contains .git/ (inside each sub-repo), the
    // UserExcluded counter stays 0 because no user pattern was supplied.
    if result.Skip.UserExcluded != 0 {
        t.Errorf("Skip.UserExcluded = %d, want 0 (no user patterns configured)", result.Skip.UserExcluded)
    }
}
```

**`TestIndexAll_SubProjectTagging`** (`indexer_test.go:316-368`)

Already tests `DiscoverSubRepos` map shape and `subProjectForFile`. Change: update assertions to use both `DiscoverSubRepos` (map) and `DiscoverSubReposAbs` (slice) — prove both helpers return the same logical set. No semantics change.

**`TestDiscoverSubRepoDirs_Empty`** (`indexer_test.go:370-386`)

Add a mirrored assertion: `DiscoverSubReposAbs(root)` returns nil/empty slice for an empty root.

### 10.2 NEW tests — separate sub-repo indexing

| Test name | What it asserts |
|---|---|
| `TestIndexSubRepos_CreatesSeparateDBs` | Given outer with 2 sub-repos, after `IndexSubRepos` runs, each `<subrepo>/.heimdall_db/<model>/vectors.db` exists and contains that sub-repo's files. |
| `TestIndexSubRepos_OuterWrapperStillIndexed` | Outer's main.go AND pkg/lib.go are in the outer DB; sub-repo files are NOT. This is the **G1 regression test**. |
| `TestIndexSubRepos_AutoRegistersInRegistry` | After indexing, `registry.LoadRegistry().All()` contains entries for outer AND each sub-repo; each sub-repo entry has `DBPath == <subrepo>/.heimdall_db`. |
| `TestIndexSubRepos_InheritsOuterModel` | Outer is indexed with `cfg.Model = "nomic-embed-text"`. Sub-repo's store metadata `embedding_model == "nomic-embed-text"` too. |
| `TestSubRepoPinnedModelNotClobbered` | Pre-create `<subrepo>/.heimdall_db/bge-m3/vectors.db` with metadata. Run outer with `cfg.Model = "nomic-embed-text"`. Sub-repo's bge-m3 store exists; no nomic-embed-text store is created for the sub-repo. Warning logged. |
| `TestIndexAll_NestedSubRepoInSubRepo` | `outer → sub-a → sub-b`. After indexing, `outer/.heimdall_db`, `outer/sub-a/.heimdall_db`, AND `outer/sub-a/sub-b/.heimdall_db` all exist. |
| `TestIndexAll_RootIsGitRepo` | Root has `.git/` AND a sub-repo `child/` with `.git/`. Outer root gets indexed, child gets separately indexed. Both registered. |
| `TestDiscoverSubRepos_GitlinkFile` | Create a directory with `.git` as a regular FILE (content: `gitdir: /path/to/worktree`). Assert it's detected as a sub-repo. |
| `TestSubRepoFailureDoesNotFailOuter` | Mock a sub-repo with an embedder that errors for that specific sub-repo. Outer indexing succeeds; sub-repo returns an error; CLI exit is still 0 (with stderr warning). |

### 10.3 NEW tests — exclude flag

Location: `internal/cli/cli_test.go` (if exists) OR new file `internal/heimdall/indexer_exclude_test.go`.

| Test name | What it asserts |
|---|---|
| `TestExcludeFlag_ExcludesDirsFromOuterWalk` | Indexer called with `ExcludeGlobs: []string{"gen"}` — files under `gen/` are NOT indexed (counted in `Skip.UserExcluded`). |
| `TestExcludeFlag_ExcludesSubRepoFromDiscovery` | Outer has sub-repos `sub-a` and `sub-b`. Indexer + orchestrator invoked with `--exclude sub-b`. Only `sub-a` gets a separate DB + registry entry. |
| `TestExcludePrecedenceSubRepo` | Same as above but asserts `Skip.UserExcluded == 1` (sub-b hit the user-exclude branch) AND `Skip.SubRepo == 1` (sub-a hit the sub-repo branch), so the counters don't double-count. |
| `TestExclude_DefaultHygieneNotCounted` | **H1 regression** — create fixture with only `.git/` and `node_modules/` (both default-excluded) plus one regular `a.go`. Run `IndexAll`. Assert `Skip.UserExcluded == 0`, `Skip.SubRepo == 0`, `FilesIndexed == 1`, `FilesSkipped == 0`. Guards against default-exclude matches leaking into user-facing counters. |
| `TestExclude_AbsolutePathRejectedByCLI` | **M6 regression** — invoke CLI `--exclude /etc/foo`; assert non-zero exit + stderr contains "absolute path". |
| `TestExclude_AbsolutePathRejectedByMCP` | **M6 regression** — call `heimdall_configure set exclude_patterns '["/etc/foo","ok"]'`; assert error returned, `cfg.ExcludePatterns` NOT mutated, `config.json` NOT written. |
| `TestCLIExcludeFlag_Repeatable` | Parse `"index" "/tmp/foo" "--exclude" "a" "--exclude" "b"` — both flags captured into a single `[]string`. |
| `TestConfigureSetExcludePatterns_MCP` | Call `heimdall_configure set exclude_patterns '["dist","build"]'` — after call, `cfg.ExcludePatterns == ["dist","build"]` AND persisted to `config.json`. |

### 10.4 NEW tests — summary breakdown

| Test name | What it asserts |
|---|---|
| `TestIndexResult_SkipBreakdownSums` | Invariant: `result.FilesSkipped == result.Skip.UserExcluded + result.Skip.SubRepo + result.Skip.Binary + result.Skip.Unchanged` for several fixtures. |
| `TestIndexResult_SkipBinaryCounted` | One `.png`-like file (first byte is 0x00) → `Skip.Binary == 1`, `FilesIndexed == 0`. |
| `TestIndexResult_SkipUnchangedCounted` | Incremental re-index where nothing changed → `Skip.Unchanged == N`, all other `Skip.*` zero. |

### 10.5 Regression tests

The existing tests that assert on `FilesSkipped` (e.g. `TestIncrementalIndex_SkipsUnchangedFiles` at `indexer_test.go:28-115`) MUST STILL PASS. `FilesSkipped` is computed as the sum of the breakdown, so those assertions hold verbatim.

### 10.6 Integration tests (MCP)

**Ollama dependency strategy (H2 resolution):** `runIndex` calls `heimdall.NewOllamaClient(s.Cfg.OllamaEndpoint).Ping(ctx)` inline. Rather than refactor for a client interface (out of scope), the MCP integration tests stand up an `httptest.NewServer` that speaks the Ollama embed + tags protocol — **the same pattern already in the repo** at:

- `internal/cli/integration_test.go` (uses `httptest.NewServer(mux)` with `/api/embed`, `/api/tags` handlers).
- `internal/heimdall/ollama_test.go` and `llm_classifier_test.go` (same pattern).
- `internal/heimdall/semantic_drift_httptest_helpers_test.go` (shared helper builder).

**Chosen approach:** Option (b) from H2 — reuse the existing httptest-based Ollama fake. Set `s.Cfg.OllamaEndpoint = fakeServer.URL` in the test's setup, then call the MCP `heimdall_index` handler directly. No code refactor needed; the plan stays tight.

**Test 1 — `TestMCPIndex_RegistersSubRepos` (primary sub-repo registration test):**

- Setup: tempdir outer with `main.go` + 2 sub-repos (`sub-a`, `sub-b`), each with their own `.git/` and a single `.go` file.
- Stand up httptest fake speaking Ollama `/api/embed` + `/api/tags` (copy the pattern from `integration_test.go`; see `fakeOllama` helper there).
- Construct `Server` with `s.Cfg.OllamaEndpoint = fake.URL`, `s.Cfg.Model = "nomic-embed-text"`.
- Invoke the `heimdall_index` tool handler with the outer path.
- Poll `heimdall_status` until `Done=true` (use `time.Tick(50*time.Millisecond)` with a 10s ceiling — existing integration tests do this).
- Assert: `registry.LoadRegistry().All()` contains 3 entries — outer, sub-a, sub-b — each with correct absolute `Path` and `DBPath = <project>/.heimdall_db`.
- Assert: each sub-repo has a populated `vectors.db` at `<sub>/.heimdall_db/nomic-embed-text/vectors.db`.

**Test 2 — `TestIndexSubRepo_GitCommitsWriteToSubRepoStore` (H3 regression — bug fix at `mcp/tools.go:362`):**

- Setup: outer tempdir with `main.go` (no commits on outer), plus sub-repo `sub-a` that IS a real git repo (use `git init` + `git commit -m ...` via `exec.Command` in the test, OR hand-craft a minimal `.git/HEAD` + refs — the simpler path is `exec.Command("git", "init")` + `commit --allow-empty -m "seed"`). The real-git path is the only way to exercise `IndexGitCommits` because it shells out to `git log`.
- Stand up httptest Ollama fake (same as Test 1).
- Invoke `heimdall_index` on outer. Wait until Done.
- **Assertion A (sub-repo commits land in sub-repo store):** Open `sub-a/.heimdall_db/nomic-embed-text/vectors.db` directly via `OpenStore`. Query for chunks with `source_type = 'git_commit'` (or equivalent — use the existing accessor; if none exists, a raw SQL `SELECT COUNT(*) FROM chunks WHERE source_type='git_commit'` via the exported DB handle). Assert count > 0.
- **Assertion B (sub-repo commits do NOT land in outer store):** Open `outer/.heimdall_db/nomic-embed-text/vectors.db`. Query for chunks whose `source_path` starts with `sub-a/` AND `source_type='git_commit'`. Assert count == 0. This is the specific bug being fixed.
- **Assertion C (outer-only commits stay in outer, if applicable):** If the outer itself is a git repo with commits, assert its commits land in the outer store and NOT in sub-a's store.

**Test 3 — `TestMCPIndex_SearchFindsSubRepoContent` (end-to-end coverage for the "no search changes" claim):**

- Same setup as Test 1.
- After indexing, invoke `heimdall_search` with a query that should match a sub-repo file.
- Assert the result includes the sub-repo's file (by checking `project` or `sub_project` fields in the result).
- This is the test that proves the plan's §1 claim "heimdall_search: No code change" is actually true.

## 11. CLI UX

### 11.1 Flag syntax

```
heimdall-mcp index <path> [--out <dir>] [--model <name>] [--exclude <pattern>...]
```

- `--exclude` is repeatable (Go stdlib `flag` idiom uses a custom `flag.Value`; we stay with the hand-rolled loop for consistency with the rest of `cli.go`).
- `--out` still routes DBs. For sub-repos it's ignored — each sub-repo's DB goes inside itself regardless. We document this in `--help` output.

### 11.2 Help text addition

In `cli.go:162-190` help block, insert:

```
  heimdall-mcp index <path> [--out <dir>] [--model <name>] [--exclude <pat>...]
                                                   Index a directory.
                                                   Nested git repos inside <path>
                                                   are indexed as separate projects
                                                   in their own .heimdall_db/.
                                                   Use --exclude to skip dirs or
                                                   sub-repos.
  heimdall-mcp config set exclude_patterns '["node_modules","build"]'
                                                   Persist global exclude patterns.
```

### 11.3 Output mock

See §G6 above. Key additions:

- Replace single "Skipped: N" line with one line + indented breakdown.
- New "Sub-repos: N indexed separately" section listing each one with counts and DB path.
- If zero sub-repos discovered, the "Sub-repos" section is suppressed entirely (don't add noise).

## 12. Docs Updates

| File | Update |
|---|---|
| `README.md` | Add "Nested Repositories" section under "Usage". Show new CLI output. Mention `--exclude`. Remove any line that claims sub-repos are skipped. |
| `docs/plans/nested-repo-indexing-TODO.md` | Tick off goals as tasks complete; update status table after each phase. |
| Inline comments in `indexer.go:15-27` | Update the docblock that enumerates back-pressure sources to mention "sub-repo discovery spawns child indexers; they share the same exclude rules". |
| `cli.go:186` help text | Reflect new `--exclude` flag (see §11.2). |

No new doc files are created. No migration guide needed (behavior change is opt-out via `--exclude`).

## 13. Rollout

**Single PR, no feature flag, no migration.** Per user's pre-change impact analysis:

- Existing users with indexed outer repos: next `heimdall-mcp index <outer>` run discovers sub-repos and creates their DBs. Outer DB is unchanged (no files added or removed).
- Existing users who have ALREADY manually indexed a sub-repo with a different model: detected and preserved (rule 2 in §G4).
- Existing `heimdall_projects` output: grows (more entries). Existing tooling that reads `projects.json` must tolerate more rows — and they already do (it's a list).

**Back-out plan:** If regressions appear, revert the PR; no data migration needed since we only wrote into new `<subrepo>/.heimdall_db/` directories that didn't exist before.

**Monorepo first-index expectation (L3 resolution):** Users with a large monorepo (many sub-repos) will see the FIRST post-upgrade `heimdall-mcp index <outer>` take noticeably longer than before because every sub-repo now gets indexed for the first time. Subsequent runs are incremental (per-sub-repo). The CLI summary emits an inline hint when sub-repos are newly created (see §G6 "Note: first indexing of sub-repos may take longer..."). The README's "Nested Repositories" section repeats this hint.

## 14. Task Breakdown

Each task is a ~2-5 minute TDD cycle: **write failing test → run to confirm it fails → implement → run to confirm it passes → commit.** Commits are frequent (one per task).

### Task 1: Add `SkipBreakdown` struct and `Skip` field to `IndexResult`

**Files:**
- Modify: `internal/heimdall/indexer.go:46-53` (add `Skip SkipBreakdown`)
- Test: `internal/heimdall/indexer_test.go` (new test `TestIndexResult_SkipBreakdownSums`)

- [ ] **Step 1: Write the failing test**

```go
func TestIndexResult_SkipBreakdownSums(t *testing.T) {
    files := map[string]string{"a.go": "package a\n"}
    root := createTestProject(t, files)
    dbDir := filepath.Join(t.TempDir(), ".heimdall_db")
    store, _ := OpenStore(dbDir)
    defer store.Close()
    embedder := &StubEmbedder{Vectors: map[string][]float32{}, Dimension: 3}
    idx := NewIndexer(root, embedder, store, ChunkerOpts{MaxChunkSize: 1500})
    result, _ := idx.IndexAll(context.Background())
    got := result.Skip.UserExcluded + result.Skip.SubRepo + result.Skip.Binary + result.Skip.Unchanged
    if got != result.FilesSkipped {
        t.Errorf("breakdown sum %d != FilesSkipped %d", got, result.FilesSkipped)
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/heimdall/ -run TestIndexResult_SkipBreakdownSums -v`
Expected: FAIL with `result.Skip undefined`.

- [ ] **Step 3: Implement**

In `indexer.go`, add:

```go
// SkipBreakdown classifies skipped files by reason. UserExcluded counts ONLY
// user-configured exclusions (cfg.ExcludePatterns + --exclude); default
// hygiene dirs (.git, node_modules, etc.) are NOT counted here.
type SkipBreakdown struct {
    UserExcluded int `json:"user_excluded"`
    SubRepo      int `json:"sub_repo"`
    Binary       int `json:"binary"`
    Unchanged    int `json:"unchanged"`
}
```

Add `Skip SkipBreakdown` to `IndexResult`. Leave counter updates for later tasks.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/heimdall/ -run TestIndexResult_SkipBreakdownSums -v`
Expected: PASS (breakdown is zero, FilesSkipped is zero, sum equality holds).

- [ ] **Step 5: Commit**

```bash
git add internal/heimdall/indexer.go internal/heimdall/indexer_test.go
git commit -m "feat(indexer): add SkipBreakdown to IndexResult"
```

### Task 2: Wire `Skip.UserExcluded`, `Skip.SubRepo`, `Skip.Binary` counters in walker

**Files:**
- Modify: `internal/heimdall/indexer.go:122-166` (walker)
- Test: extend existing tests

- [ ] **Step 1: Write the failing test** (binary + excluded dir fixtures)

```go
func TestIndexResult_SkipBinaryCounted(t *testing.T) {
    root := t.TempDir()
    os.WriteFile(filepath.Join(root, "bin.dat"), []byte{0x00, 0x01, 0x02}, 0644)
    os.WriteFile(filepath.Join(root, "ok.go"), []byte("package a\n"), 0644)
    store, _ := OpenStore(filepath.Join(t.TempDir(), "db"))
    defer store.Close()
    embedder := &StubEmbedder{Vectors: map[string][]float32{}, Dimension: 3}
    idx := NewIndexer(root, embedder, store, ChunkerOpts{MaxChunkSize: 1500})
    result, _ := idx.IndexAll(context.Background())
    if result.Skip.Binary != 1 {
        t.Errorf("Skip.Binary = %d, want 1", result.Skip.Binary)
    }
}
```

- [ ] **Step 2: Run → FAIL** (counter not incremented).

- [ ] **Step 3: Implement**

First, split the exclusion check inside `indexer.go` (this is the H1 resolution):

```go
// isDefaultExcluded returns true iff p matches the ALWAYS-on hygiene list
// (.git, node_modules, .heimdall_db, vendor, __pycache__, .idea). Does NOT
// consult user config.
func (idx *Indexer) isDefaultExcluded(p string) bool { /* matches defaultExcludes only */ }

// isUserExcluded returns true iff p matches a user-configured pattern from
// idx.opts.ExcludeGlobs. Does NOT consult defaults.
func (idx *Indexer) isUserExcluded(p string) bool { /* matches idx.opts.ExcludeGlobs only */ }

// shouldExclude stays as the union (kept for any remaining callers).
func (idx *Indexer) shouldExclude(p string) bool {
    return idx.isDefaultExcluded(p) || idx.isUserExcluded(p)
}
```

Then in the walker at `indexer.go:122-166`:

- **Directory branch (line 136-147):** replace the single `idx.shouldExclude(relPath) → SkipDir` with:
  1. If `idx.isUserExcluded(relPath)` → `result.Skip.UserExcluded++; return filepath.SkipDir`.
  2. Else if `idx.isDefaultExcluded(relPath)` → `return filepath.SkipDir` (NO counter bump — default hygiene is silent).
  3. Then the sub-repo branch (unchanged; bumps `result.Skip.SubRepo++`).
- **File branch (line 150-157):** replace `idx.shouldExclude(relPath) → return nil` with:
  1. If `idx.isUserExcluded(relPath)` → `result.Skip.UserExcluded++; return nil`.
  2. Else if `idx.isDefaultExcluded(relPath)` → `return nil` (no counter).
  3. If `isBinaryFile(path)` → `result.Skip.Binary++; return nil`.

- **Incremental branch** (~line 207): replace the existing `result.FilesSkipped++` with `result.Skip.Unchanged++`.

Finally, at the end of `indexFiles` (just before the final `result.Duration = time.Since(start); return result, nil`), recompute the legacy counter:

```go
result.FilesSkipped = result.Skip.UserExcluded + result.Skip.SubRepo + result.Skip.Binary + result.Skip.Unchanged
```

- [ ] **Step 4: Run tests** — `go test ./internal/heimdall/ -v`. All green including existing `TestIncrementalIndex_SkipsUnchangedFiles`.

- [ ] **Step 5: Commit**

```bash
git commit -m "feat(indexer): count skipped files by category (excluded/sub-repo/binary/unchanged)"
```

### Task 3: Extend `.git` check to accept gitlink files (worktree support)

**Files:**
- Modify: `internal/heimdall/dbpath.go:116-132` (drop `info.IsDir()` on `.git`)
- Modify: `internal/heimdall/indexer.go:141-146` (same)
- Test: new `TestDiscoverSubRepos_GitlinkFile`

- [ ] **Step 1: Write the failing test**

```go
func TestDiscoverSubRepos_GitlinkFile(t *testing.T) {
    root := t.TempDir()
    sub := filepath.Join(root, "wt")
    os.MkdirAll(sub, 0755)
    // .git is a FILE (gitlink), not a directory
    os.WriteFile(filepath.Join(sub, ".git"), []byte("gitdir: /some/worktrees/wt\n"), 0644)
    got := DiscoverSubRepos(root)
    if !got["wt"] {
        t.Errorf("expected wt in subRepoDirs, got %v", got)
    }
}
```

- [ ] **Step 2: Run → FAIL** (current code requires `info.IsDir()`).

- [ ] **Step 3: Implement** — delete the `info.IsDir()` check in both `dbpath.go:127` and `indexer.go:143`, replace with existence-only `if _, err := os.Stat(gitDir); err == nil`.

- [ ] **Step 4: Run tests.** Pass.

- [ ] **Step 5: Commit**

```bash
git commit -m "fix(indexer): detect git worktrees (gitlink files) as sub-repos"
```

### Task 4: Add `DiscoverSubReposAbs` helper

**Files:**
- Modify: `internal/heimdall/dbpath.go` (add new function)
- Test: add `TestDiscoverSubReposAbs_ReturnsAbsolutePaths` to `indexer_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestDiscoverSubReposAbs_ReturnsAbsolutePaths(t *testing.T) {
    root := t.TempDir()
    os.MkdirAll(filepath.Join(root, "a", ".git"), 0755)
    os.MkdirAll(filepath.Join(root, "b", ".git"), 0755)
    got, err := DiscoverSubReposAbs(root)
    if err != nil {
        t.Fatal(err)
    }
    if len(got) != 2 {
        t.Fatalf("got %d, want 2", len(got))
    }
    for _, p := range got {
        if !filepath.IsAbs(p) {
            t.Errorf("expected absolute, got %q", p)
        }
    }
}

func TestDiscoverSubReposAbs_ReadErrorPropagates(t *testing.T) {
    // Nonexistent root → error propagates (L1 regression).
    _, err := DiscoverSubReposAbs("/definitely/not/a/real/path/abc123")
    if err == nil {
        t.Fatal("expected error for nonexistent root, got nil")
    }
}

func TestDiscoverSubReposAbs_IgnoresSymlinks(t *testing.T) {
    // M4 regression — symlinks are NOT followed.
    realRepo := t.TempDir()
    os.MkdirAll(filepath.Join(realRepo, ".git"), 0755)
    root := t.TempDir()
    if err := os.Symlink(realRepo, filepath.Join(root, "linked-sub")); err != nil {
        t.Skipf("symlink unsupported on this platform: %v", err)
    }
    got, err := DiscoverSubReposAbs(root)
    if err != nil {
        t.Fatal(err)
    }
    if len(got) != 0 {
        t.Errorf("expected 0 discovered (symlinks ignored), got %v", got)
    }
}
```

- [ ] **Step 2: Run → FAIL** (function doesn't exist).

- [ ] **Step 3: Implement** (see §4 for exact code).

- [ ] **Step 4: Run tests.** Pass.

- [ ] **Step 5: Commit**

```bash
git commit -m "feat(indexer): add DiscoverSubReposAbs helper"
```

### Task 5: Implement `Indexer.IndexSubRepos` — core separate-indexing loop

**Files:**
- Modify: `internal/heimdall/indexer.go` (add new method + new result type)
- Test: new `TestIndexSubRepos_CreatesSeparateDBs`, `TestIndexSubRepos_OuterWrapperStillIndexed`

- [ ] **Step 1: Write the failing test**

```go
func TestIndexSubRepos_CreatesSeparateDBs(t *testing.T) {
    root := t.TempDir()
    os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0644)
    sub := filepath.Join(root, "child")
    os.MkdirAll(filepath.Join(sub, ".git"), 0755)
    os.WriteFile(filepath.Join(sub, "c.go"), []byte("package c\n"), 0644)
    // outer indexer
    outerStore, _ := OpenStore(filepath.Join(t.TempDir(), "outer-db"))
    defer outerStore.Close()
    embedder := &StubEmbedder{Vectors: map[string][]float32{}, Dimension: 3}
    idx := NewIndexer(root, embedder, outerStore, ChunkerOpts{MaxChunkSize: 1500})
    _, _ = idx.IndexAll(context.Background())

    // new API — returns per-sub-repo results
    subResults, err := idx.IndexSubRepos(context.Background(), "nomic-embed-text", SubRepoOpts{})
    if err != nil {
        t.Fatal(err)
    }
    if len(subResults) != 1 {
        t.Fatalf("got %d sub-results, want 1", len(subResults))
    }
    subDB := filepath.Join(sub, ".heimdall_db", "nomic-embed-text", "vectors.db")
    if _, err := os.Stat(subDB); err != nil {
        t.Errorf("sub DB not created at %s: %v", subDB, err)
    }
}
```

- [ ] **Step 2: Run → FAIL** (method doesn't exist).

- [ ] **Step 3: Implement**

In `indexer.go`, add:

```go
// SubRepoOpts parameterizes IndexSubRepos.
type SubRepoOpts struct {
    // ExcludeGlobs inherited from ChunkerOpts via Indexer; nothing extra here yet.
}

// SubRepoResult describes one sub-repo's indexing outcome.
//
// Invariant: Err != nil ⟹ Result may be nil. Callers MUST gate on Err before
// dereferencing Result.
type SubRepoResult struct {
    Path    string       // absolute path to the sub-repo; always populated
    Name    string       // basename(Path); always populated
    DBPath  string       // absolute path to <sub>/.heimdall_db; always populated
    Model   string       // model used (inherited or preserved); populated iff store-open succeeded
    Result  *IndexResult // nil iff Err != nil or store-open failed
    Err     error        // non-nil iff this sub-repo failed
}

// IndexSubRepos discovers immediate sub-repos of idx.root and indexes each
// one as a separate project rooted at itself, writing to that sub-repo's own
// .heimdall_db. model is the caller's requested model (typically the outer's);
// if a sub-repo already has a store with a different model, that pinned model
// is preserved (rule 1 in §G4).
//
// The returned error is non-nil ONLY for discovery-level failures (the root
// dir is unreadable). Per-sub-repo failures are reported via out[i].Err and
// do NOT cause the function to return a non-nil error — the caller walks
// `out` and aggregates. This preserves the "outer-indexed-successfully"
// contract even if all sub-repos fail.
func (idx *Indexer) IndexSubRepos(ctx context.Context, model string, opts SubRepoOpts) ([]SubRepoResult, error) {
    subs, err := DiscoverSubReposAbs(idx.root)
    if err != nil {
        return nil, fmt.Errorf("discover sub-repos: %w", err)
    }
    var out []SubRepoResult
    for _, subAbs := range subs {
        relName, _ := filepath.Rel(idx.root, subAbs)
        // Respect the outer excludes: if the sub-repo directory is user-excluded
        // by pattern, skip it entirely (it was already not walked into).
        // NOTE: default-excluded dirs never reach here (they were filtered at
        // the walker level), so checking only isUserExcluded is sufficient AND
        // keeps the counter semantics clean.
        if idx.isUserExcluded(relName) {
            continue
        }
        subRes := SubRepoResult{Path: subAbs, Name: filepath.Base(subAbs), DBPath: filepath.Join(subAbs, ".heimdall_db")}

        // Resolve model for this sub-repo (see §G4 rules — rule 1 beats all).
        effectiveModel := model
        if existing := ListAvailableModels(subRes.DBPath); len(existing) > 0 {
            // Rule 1: keep existing pinned model, EVEN if the caller (outer)
            // asked for a different one via --model. The CLI surfaces this in
            // the summary via the "[pinned]" bracket.
            effectiveModel = existing[0]
        }
        subRes.Model = effectiveModel

        // Open/create sub-repo store.
        subDBDir := ModelDBDir(subRes.DBPath, effectiveModel)
        subStore, err := OpenStore(subDBDir)
        if err != nil {
            subRes.Err = fmt.Errorf("open sub-repo store %s: %w", subDBDir, err)
            out = append(out, subRes)
            continue
        }

        // Spawn a fresh indexer rooted at the sub-repo, inheriting opts/embedder.
        subIdx := NewIndexer(subAbs, idx.embedder, subStore, idx.opts)
        r, err := subIdx.IndexAll(ctx)
        subRes.Result = r
        subRes.Err = err
        subStore.SetMetadata("embedding_model", effectiveModel)
        subStore.Close()
        out = append(out, subRes)
    }
    return out, nil
}
```

- [ ] **Step 4: Run tests** — `TestIndexSubRepos_CreatesSeparateDBs` PASS.

- [ ] **Step 5: Commit**

```bash
git commit -m "feat(indexer): add IndexSubRepos to index nested git repos separately"
```

### Task 6: Add `TestIndexSubRepos_OuterWrapperStillIndexed` regression test

**Files:**
- Test: extend `indexer_test.go`

- [ ] **Step 1: Write the test** (outer has main.go + sub has c.go; assert outer store has main.go but NOT child/c.go; sub store has c.go but NOT main.go).

- [ ] **Step 2: Run → PASS** immediately (the assertion follows from Task 5's implementation). If it fails, fix Task 5.

- [ ] **Step 3: Commit**

```bash
git commit -m "test(indexer): regression test — outer wrapper still indexed when sub-repos exist"
```

### Task 7: Rewrite `TestIndexAll_SkipsSubRepoDirectories` → `TestIndexAll_SubRepoFilesNotInOuterStore`

**Files:**
- Modify: `internal/heimdall/indexer_test.go:252-314`

- [ ] **Step 1: Rename + adjust** as described in §10.1.

- [ ] **Step 2: Run** — should pass (same assertions on outer, plus new `Skip.SubRepo == 2` assertion).

- [ ] **Step 3: Commit**

```bash
git commit -m "test(indexer): rewrite sub-repo skip test to assert sub-repo skip counter"
```

### Task 8: CLI `--exclude` flag parsing

**Files:**
- Modify: `internal/cli/cli.go:86-110` (preamble flag loop)
- Test: `internal/cli/cli_test.go` (new file if needed) — `TestCLIExcludeFlag_Repeatable`

- [ ] **Step 1: Write the failing test**

```go
func TestCLIExcludeFlag_Repeatable(t *testing.T) {
    args := []string{"index", "/tmp/x", "--exclude", "a", "--exclude", "b"}
    excludes := parseCLIExcludeFlags(args) // new helper we'll extract
    if len(excludes) != 2 || excludes[0] != "a" || excludes[1] != "b" {
        t.Errorf("got %v", excludes)
    }
}
```

- [ ] **Step 2: Run → FAIL** (no helper).

- [ ] **Step 3: Implement** — extract a tiny `parseCLIExcludeFlags([]string) []string` helper and call it from `RunCLI`. Add corresponding entry in the main loop at `cli.go:92-102` to strip `--exclude <val>` pairs from `cleanArgs`.

- [ ] **Step 4: Run tests.** PASS.

- [ ] **Step 5: Commit**

```bash
git commit -m "feat(cli): add repeatable --exclude flag to index command"
```

### Task 9: Wire `--exclude` into `ChunkerOpts.ExcludeGlobs` for both outer and sub-repo passes

**Files:**
- Modify: `internal/cli/cli.go:196-271` (cliIndex)
- Modify: `internal/cli/cli.go:273-338` (indexWithModel signature — accept extra excludes)

- [ ] **Step 1: Write the failing test** — `TestExcludeFlag_ExcludesSubRepoFromDiscovery` (see §10.3). Uses the indexer directly with `ChunkerOpts{ExcludeGlobs: []string{"sub-b"}}`; run `IndexSubRepos`; assert only `sub-a` in results.

- [ ] **Step 2: Run → FAIL** or PASS depending on Task 5's behavior. (Task 5 already filters via `shouldExclude` — if the assertion already works, this test is a protection against regressions.) If it PASSes immediately, that's fine.

- [ ] **Step 3: Wire CLI** — in `cliIndex`, accept a `extraExcludes []string` arg, prepend to `cfg.ExcludePatterns` when building `ChunkerOpts`.

- [ ] **Step 4: Run tests.**

- [ ] **Step 5: Commit**

```bash
git commit -m "feat(cli): pass --exclude values through to indexer"
```

### Task 10: CLI orchestration — run sub-repo pass after outer pass

**Files:**
- Modify: `internal/cli/cli.go:248-265` (the per-model indexing loop in cliIndex)
- Test: new `TestCLIIndex_IndexesSubReposSeparately` (or MCP-level test if CLI is too plumbing-heavy).

- [ ] **Step 1: Write the test** — invoke `cliIndex` in-process against a fixture; assert `<outer>/.heimdall_db` AND `<outer>/child/.heimdall_db` both exist.

- [ ] **Step 2: Run → FAIL** (no sub-repo pass yet).

- [ ] **Step 3: Implement** — after the existing `indexer.IndexProjectAsync` loop completes (one model, outer done), call `idx.IndexSubRepos(ctx, modelName, SubRepoOpts{})`. For each `SubRepoResult`, `reg.Register` it, and collect for the summary.

- [ ] **Step 4: Run tests.**

- [ ] **Step 5: Commit**

```bash
git commit -m "feat(cli): index nested sub-repos as separate projects after outer pass"
```

### Task 11: CLI summary rendering (new format)

**Files:**
- Modify: `internal/cli/cli.go:300-337` (the progress-to-done rendering block)

- [ ] **Step 1: Write the test** (capture stdout via `os.Pipe` and assert substrings: `"Skipped:"`, `"unchanged"`, `"Sub-repos:"`).

- [ ] **Step 2: Run → FAIL**.

- [ ] **Step 3: Implement** — replace the current summary with the format in §G6. Add a helper `renderIndexSummary(w io.Writer, r *IndexResult, subResults []SubRepoResult)` and call it from both the CLI and the progress sink.

- [ ] **Step 4: Run tests.**

- [ ] **Step 5: Commit**

```bash
git commit -m "feat(cli): new summary format — categorized skip counters + sub-repo list"
```

### Task 12: MCP `heimdall_index` — mirror the sub-repo pass in `runIndex`

**Files:**
- Modify: `internal/mcp/tools.go:266-395` (runIndex goroutine)

- [ ] **Step 1: Write the test** — `internal/mcp/tools_test.go` `TestMCPIndex_RegistersSubRepos`. Wire a fake embedder, invoke the MCP index handler against a fixture with sub-repos, poll `heimdall_status`, assert 3 registry entries appear.

- [ ] **Step 2: Run → FAIL**.

- [ ] **Step 3: Implement** — after the `if p.Done { ... Registry.Register(...) ... }` block at `tools.go:329-335`, call `indexer.IndexSubRepos(ctx, s.Cfg.Model, heimdall.SubRepoOpts{})` and register each sub-repo. Also: fix the existing `IndexGitCommits` loop at `tools.go:360-368` — it currently writes sub-repo commits into the OUTER store. Change it to open each sub-repo's OWN store and write commits there (the sub-repo's own store was just created by `IndexSubRepos`).

- [ ] **Step 4: Run tests.**

- [ ] **Step 5: Commit**

```bash
git commit -m "feat(mcp): heimdall_index now indexes nested sub-repos as separate projects"
```

### Task 13: MCP `heimdall_configure` — add `exclude_patterns` key

**Files:**
- Modify: `internal/mcp/config_tools.go:34-146` (configKeys map)
- Test: `internal/mcp/config_tools_test.go` — `TestConfigureSetExcludePatterns_MCP`.

- [ ] **Step 1: Write the failing test**.

- [ ] **Step 2: Run → FAIL** (unknown key).

- [ ] **Step 3: Implement** — add the entry shown in §7.

- [ ] **Step 4: Run tests.**

- [ ] **Step 5: Commit**

```bash
git commit -m "feat(mcp): heimdall_configure supports exclude_patterns key"
```

### Task 14: CLI `config set exclude_patterns` parity

**Files:**
- Modify: `internal/cli/cli.go:703-780` (getConfigKey/setConfigKey)

- [ ] **Step 1: Add `exclude_patterns` case** that calls `json.Unmarshal([]byte(rawVal), &[]string)` (same shape as `git.branches` at `cli.go:744-749`).

- [ ] **Step 2: Run `heimdall-mcp config set exclude_patterns '["a","b"]'`** in a manual smoke test → saved to disk, `config get` returns it.

- [ ] **Step 3: Commit**

```bash
git commit -m "feat(cli): add 'config set exclude_patterns' support"
```

### Task 15: Edge case tests

- [ ] `TestSubRepoPinnedModelNotClobbered` — pre-create bge-m3 store in sub; run outer with nomic; assert bge-m3 store intact, no nomic store in sub. Commit.
- [ ] `TestSubRepoPinnedModelBeatsExplicitOuterModel` (§8 edge 13 — pinned still wins over `--model X`) — pre-create bge-m3 store in sub-a; invoke `cliIndex` with `--model nomic-embed-text`; assert sub-a keeps bge-m3, outer gets nomic, and the CLI summary line for sub-a includes `[bge-m3 — pinned]`. Commit.
- [ ] `TestSubRepoInheritsModelFirstIndex` — sub has no prior store; assert it gets indexed with the outer's model. Commit.
- [ ] `TestIndexAll_NestedSubRepoInSubRepo` — 3 levels (outer → a → a/b); assert all three DBs exist. Commit.
- [ ] `TestIndexAll_RootIsGitRepo` — root has `.git/` + a sub-repo; outer + child both indexed and registered. Commit.
- [ ] `TestExcludePrecedenceSubRepo` — `--exclude sub-b` when sub-b is a sub-repo; assert `Skip.UserExcluded == 1` AND `Skip.SubRepo == 0` (because sub-b is filtered BEFORE the sub-repo branch). Commit.
- [ ] `TestExclude_DefaultHygieneNotCounted` (H1 regression) — fixture with `.git/` + `node_modules/` + one regular file; assert `Skip.UserExcluded == 0` and `FilesSkipped == 0` (matches existing `TestIndexAll_DoesNotSkip` semantics). Commit.
- [ ] `TestSubRepoFailureDoesNotFailOuter` — inject embedder that errors on a specific sub-repo path; assert outer still indexed + registered, sub-repo has an error recorded, CLI exits 0 with stderr warning. Commit.
- [ ] `TestSubRepoResult_NilResultOnStoreOpenFailure` (L2 regression) — force `OpenStore` to fail for one sub-repo (e.g. make its `.heimdall_db` path a read-only file); assert `SubRepoResult{Err != nil, Result == nil}` and the summary renderer emits `FAILED: ...` without panicking. Commit.
- [ ] `TestRegistry_SubReposWithSameBasenameAcrossProjects` — pre-register `frontend` pointing at `/a/frontend`; re-register `frontend` at `/b/frontend`. Current Register dedupes by name; assert new behavior still OK (path-wins) or document. Commit.
- [ ] `TestDiscoverSubReposAbs_IgnoresSymlinks` (M4 regression — already in §10.2 new-test list; included here for implementation tracking). Commit.
- [ ] `TestExclude_AbsolutePathRejectedByCLI` and `TestExclude_AbsolutePathRejectedByMCP` (M6 regression) — both surfaces reject absolute paths. Commit.

### Task 16: Docs updates

- [ ] Update `README.md` — add "Nested repos" section. Commit.
- [ ] Update inline doc comment at `indexer.go:15-27`. Commit.
- [ ] Update `docs/plans/nested-repo-indexing-TODO.md` status table. Commit.

### Task 17: Verification

- [ ] Run `go test ./... -count=1` — all green. Paste output in PR description.
- [ ] Run manual smoke test: a test fixture with 1 outer + 2 sub-repos + `.claude/worktrees/foo`. Run `heimdall-mcp index ./fixture --exclude "internal/*_generated.go"`. Verify summary format. Verify `heimdall-mcp projects` lists 3 entries.
- [ ] Run `go vet ./...` and `gofmt -l .` — no issues.

## 15. Escalations

No escalations required to proceed. All 6 goals fit cleanly into the existing architecture:

- Registry schema is already sufficient (no migration).
- Per-project config is NOT added — see §7.1 Out of scope. If the user confirms they want per-project files (`.heimdall/config.json`), file a follow-up plan; do NOT bolt onto this PR.
- No new packages, no circular dependencies.
- No feature flag, single PR, per user's briefing.

### 15.1 Follow-up PRs (explicit list — do NOT land in this PR)

| ID | Item | Rationale |
|---|---|---|
| F1 | Per-project config file (`.heimdall/config.json`) | Bigger change; M1 deferral. |
| F2 | `--format=json` machine-readable summary | M5 follow-up; industry standard (`cargo`, `npm`). |
| F3 | `--force-model` flag to overwrite pinned sub-repo models | G4 / edge 13 hook. |
| F4 | Opt-in symlink-following discovery (`--follow-symlinks`) | M4 deferral. |
| F5 | Recursive glob (`**`) in exclude patterns | §6 limitation; would need custom matcher (filepath.Match doesn't support it). |

---

## Self-Review Checklist

- [x] **Spec coverage:** All 6 goals mapped to concrete tasks (G1→Task 6, G2→Task 5, G3→Tasks 10+12, G4→Tasks 5+15, G5→Tasks 8+9+13+14, G6→Tasks 2+11).
- [x] **Placeholder scan:** No "TBD", "TODO", or "implement later" in code steps.
- [x] **Type consistency:** `SkipBreakdown`, `SubRepoResult`, `SubRepoOpts`, `IndexSubRepos`, `DiscoverSubReposAbs` — names used uniformly across all tasks. `UserExcluded` (not `ExcludedDir`) used throughout after H1 fix.
- [x] **Impact analysis:** Every touched subsystem listed in §1 with proof it still works.
- [x] **Industry-standard references:** `fd --exclude`, `ripgrep --glob`, `semgrep --exclude` cited in §6.
- [x] **Evidence:** Every line number cited against verified current code (grep -n output confirmed during plan authoring).

---

## Revision notes (post-review, score 77 → addressed)

Each finding from `docs/plans/nested-repo-indexing-review.md` and how it was resolved in this revision. Status legend: **fixed** = incorporated into the plan; **deferred-with-rationale** = explicitly out of scope with a follow-up entry in §15.1.

### HIGH (blocking — all fixed)

- **H1** (skip counters collide with default-excludes): **fixed.** Chose option (b) from the review brief — reserved the counter for user-added patterns only. Renamed `ExcludedDir` → `UserExcluded` throughout. Added `isDefaultExcluded` / `isUserExcluded` split in Task 2 implementation. Added `TestExclude_DefaultHygieneNotCounted` regression test (§10.3, Task 15). Preserved existing `TestIndexAll_DoesNotSkip` assertion semantics.
- **H2** (`TestMCPIndex_RegistersSubRepos` not implementable): **fixed.** Picked option (b) — reuse the existing `httptest.NewServer` Ollama fake pattern (verified in `internal/cli/integration_test.go`, `internal/heimdall/ollama_test.go`, `llm_classifier_test.go`, `semantic_drift_httptest_helpers_test.go`). Rewrote §10.6 with concrete setup: inject `fakeServer.URL` into `s.Cfg.OllamaEndpoint`, poll `heimdall_status` until Done. No code refactor needed.
- **H3** (missing regression test for `mcp/tools.go:362` bug): **fixed.** Added `TestIndexSubRepo_GitCommitsWriteToSubRepoStore` to §10.6 with three explicit assertions: sub-repo commits land in sub-repo store (A), NOT in outer store (B), outer commits land in outer (C). Uses real `git init` fixtures via `exec.Command` since `IndexGitCommits` shells out.

### MEDIUM (all fixed except M1 which is explicitly deferred)

- **M1** (per-project config scope): **deferred-with-rationale.** Added §7.1 "Out of scope" with explicit reasoning + follow-up entry F1 in §15.1. The plan keeps global-config-only scope for this PR.
- **M2** (pinned-model notification only logged, not user-visible): **fixed.** §G4 and §G6 now require the CLI summary to surface pinned models via `[<model> — pinned]` bracket notation and a one-line stdout note when any pin is preserved.
- **M3** (`ExcludedDir` naming): **fixed.** Renamed to `UserExcluded` across §G6, §8, §10, Task 1, Task 2, Task 15.
- **M4** (symlink sub-repos unspecified): **fixed.** §4 explicitly states symlinked sub-repos are NOT followed (matches git/fd/ripgrep defaults). Added `entry.Type()&os.ModeSymlink` defense check in `DiscoverSubReposAbs`. Added edge case 11 in §8 and `TestDiscoverSubReposAbs_IgnoresSymlinks` regression.
- **M5** (CLI text breaking change): **fixed.** §G6 now enumerates the exact line that changes (`cli.go:313`) and specifies the test-assertion audit in Task 11 ("search `internal/cli/` and `internal/mcp/` tests for `(unchanged)` / starts-with `Skipped:`").
- **M6** (absolute-path validation inconsistency between CLI and MCP): **fixed.** §6 defines canonical form (project-relative OR bare glob) and requires both CLI `--exclude` and MCP `heimdall_configure` to reject absolute paths via a shared `validateExcludePatterns` helper. Added two regression tests (`TestExclude_AbsolutePathRejectedByCLI`, `TestExclude_AbsolutePathRejectedByMCP`).

### LOW (all fixed)

- **L1** (silent read-error in `DiscoverSubReposAbs`): **fixed.** Signature changed to `([]string, error)`; errors propagate. Callers in CLI/MCP log to stderr and treat as discovery failure. Added `TestDiscoverSubReposAbs_ReadErrorPropagates`.
- **L2** (`SubRepoResult` nil invariant undocumented): **fixed.** Added §5.1 with explicit invariant godoc. Summary renderer (§G6) specifies `FAILED: <err>` path for nil-Result case. Added `TestSubRepoResult_NilResultOnStoreOpenFailure`.
- **L3** (no monorepo re-index-time hint): **fixed.** §G6 now prescribes an inline CLI hint when sub-repos are newly discovered ("Note: first indexing of sub-repos may take longer..."). §13 rollout repeats the guidance for release notes / README.

### Preserved bug fixes (unchanged — already in plan)

- `mcp/tools.go:362` outer-store git-commits bug — fix is in Task 12; H3 adds the missing regression test.
- `dbpath.go:127` worktree (.git gitlink file) bug — fix is in Task 3 with `TestDiscoverSubRepos_GitlinkFile`.
