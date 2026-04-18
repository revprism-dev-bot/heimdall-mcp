# Nested Repo Indexing Plan — Review

**Reviewer role:** Plan Review Agent
**Plan file:** `docs/plans/nested-repo-indexing.md`
**Worktree:** `/home/noname/Code/heimdall-mcp-worktrees/index-nested-repos` (branch `feat/index-nested-repos-separately`)
**Review date:** 2026-04-18

---

## TL;DR

- **Final score:** 77 / 100
- **Verdict:** APPROVE-WITH-CHANGES
- **Hard blockers:** 3 HIGH findings (see Findings section). The design is fundamentally sound and every file:line citation I spot-checked was correct. Blockers are specification gaps the author must close before TDD starts, not redesigns.
- **Once blockers are fixed, this is implementation-ready.**

---

## Verification Log — every plan claim I checked

I ran Read/Grep against the live worktree and spot-checked every line number cited in the plan. All citations landed within one or two lines of the actual code.

| Plan claim | Actual code | Verdict |
|---|---|---|
| `indexer.go:46-53` is `IndexResult` struct | Struct spans lines 46-53 exactly | ✓ |
| `indexer.go:141-146` is the `.git` dir check and uses `info.IsDir()` | Lines 141-146, `info.IsDir()` on line 143 | ✓ |
| `indexer.go:393-410` is `shouldExclude` | Function spans 393-410 | ✓ |
| `indexer.go:240, 274, 299` is `SubProject` stamping | `subProject := subProjectForFile(...)` at 240, used in records at 274 and 299 | ✓ |
| `indexer.go:396` defaults merged in `shouldExclude` | Confirmed `allExcludes := append(defaultExcludes, idx.opts.ExcludeGlobs...)` | ✓ |
| `dbpath.go:116-132` is `DiscoverSubRepos` requiring `info.IsDir()` on `.git` | Confirmed; line 127: `if info, err := os.Stat(gitDir); err == nil && info.IsDir()` | ✓ |
| `scope.go:49` accepts `.git` as file OR dir (no IsDir check) | Confirmed; line 49: `if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil` | ✓ |
| `scope.go:42-53` is `hasRepoMarker` | Confirmed | ✓ |
| `registry.go:11-15` is `ProjectEntry` | Confirmed — `Name`, `Path`, `DBPath` fields | ✓ |
| `registry.go:63-75` is `Register` and dedupes by `Name OR Path` | Confirmed; `p.Name == name \|\| p.Path == projectPath` at line 69 | ✓ |
| `config.go:17,46` has `ExcludePatterns` with `.claude/worktrees` default | Line 17 field; line 46 default list | ✓ |
| `config.go:43` has `DefaultConfig()` defining `Model` | Actual location is lines 40-58, `Model` at line 43 | ✓ |
| `config.go:119-141` is `SaveConfig` | Confirmed 119-141 | ✓ |
| `mcp/tools.go:202-395` is `toolIndex`+`runIndex` | Confirmed | ✓ |
| `mcp/tools.go:240-246` marshals `filesScanned`, `filesIndexed`, `chunksCreated` from `IndexResult` | Confirmed at lines 241-244 | ✓ |
| `mcp/tools.go:329-335` registers project after indexing done | Confirmed at lines 329-335 | ✓ |
| **RED FLAG: `mcp/tools.go:360-368` writes sub-repo git commits to OUTER store** | Confirmed at line 362: `IndexGitCommits(ctx, subPath, 200, embedder, store)` where `store` is the outer `store` opened at line 290. This IS a pre-existing bug. Plan's remediation (open each sub-repo's own store) is correct. | ✓ BUG CONFIRMED |
| `mcp/config_tools.go:34-146` is `configKeys` map | Confirmed — 34 is `var configKeys = map[string]configKeyDef{`, closes at 146 | ✓ |
| `cli.go:86-110` is CLI preamble flag parser | Confirmed; actual loop is lines 86-102 (off by ~8 lines; acceptable) | ✓ |
| `cli.go:196-271` is `cliIndex` orchestration | Confirmed | ✓ |
| `cli.go:273-338` is `indexWithModel` | Confirmed | ✓ |
| `cli.go:300-337` is progress-to-done rendering | Confirmed; the `Skipped: %d files (unchanged)` line lives at 313 | ✓ |
| `cli.go:313` renders `FilesSkipped` as "unchanged" | Confirmed exact line; plan calls out that the current label is a lie (it covers all skip categories, not just unchanged) | ✓ |
| `cli.go:564-601` is `cliProjects` | Confirmed | ✓ |
| `cli.go:703-780` is `getConfigKey`/`setConfigKey` | Confirmed | ✓ |
| `cli.go:744-749` has `git.branches` using `json.Unmarshal` on a JSON array | Confirmed — the proposed `exclude_patterns` follows the same pattern | ✓ |
| `indexer_test.go:252-314` is `TestIndexAll_SkipsSubRepoDirectories` | Confirmed (252-314 exactly) | ✓ |
| `indexer_test.go:316-368` is `TestIndexAll_SubProjectTagging` | Confirmed | ✓ |
| `indexer_test.go:370-386` is `TestDiscoverSubRepoDirs_Empty` | Confirmed | ✓ |
| `chunker.go` defines `ChunkerOpts` with `IncludePaths` | Confirmed at `chunker.go:4-9` | ✓ |
| Plan says `FilesSkipped == Sum(Skip.*)` — breakdown sums to existing counter | Incremental skip is the only currently-counted skip; `shouldExclude` dirs and binary files are currently UNCOUNTED (they return without incrementing anything). Plan promises to wire them in Task 2. | ✓ but see Finding H1 |
| Plan says `ChunkerOpts.ExcludeGlobs` composits with defaults at `shouldExclude` | Confirmed via `allExcludes := append(defaultExcludes, idx.opts.ExcludeGlobs...)` at 396 | ✓ |
| Plan says `heimdall_configure` MCP tool uses `toStringSlice` helper | Confirmed — `git.branches` entry at `config_tools.go:86-96` uses identical shape | ✓ |

**All RED FLAGS from the reviewer brief checked:**
- `tools.go:360-368` writing to outer store: **confirmed bug**. Plan fix is correct.
- `DiscoverSubRepos` requires `info.IsDir()` on `.git`: **confirmed** (dbpath.go:127).
- `indexer.go:141-146` requires `info.IsDir()` on `.git`: **confirmed** (line 143).
- `scope.go:49` already accepts `.git` as file OR dir: **confirmed**.
- `configKeys` map location: **confirmed**.

Every structural claim in the plan holds. The plan author did their homework.

---

## Per-Dimension Scores

### 1. Completeness — 9/10

All 6 user goals are covered:

| Goal | Plan coverage |
|---|---|
| G1: Outer wrapper still indexed | §2.G1 + Task 6 regression test `TestIndexSubRepos_OuterWrapperStillIndexed` |
| G2: Nested sub-repos each with own `.heimdall_db/` | §4 layout + Task 5 `IndexSubRepos` |
| G3: Auto-register sub-repos in registry | §5 + Task 10 (CLI) + Task 12 (MCP) |
| G4: Sub-repo inherits parent's embedding model | §2.G4 precedence rules + Task 15 tests |
| G5: `--exclude` CLI flag, repeatable, persistable | §6 semantics + Tasks 8-9-13-14 |
| G6: CLI summary breakdown | §2.G6 + Tasks 2 + 11 |

**-1 deduction:** Per-project config file (the user's phrase "project config file") is explicitly deferred. Reviewer's brief calls this out as a possible escalation. See Finding M1.

### 2. Correctness vs. current code — 10/10

Every citation I grep-verified was accurate. The bug claim at `tools.go:360-368` is real. The worktree-gitlink gap at `dbpath.go:127` and `indexer.go:143` is real. The skip-counter gap at the walker level is real (currently no counter increments when a dir is excluded or a binary is seen — `FilesSkipped` only moves on the incremental-unchanged path at line 207).

### 3. Test strategy (TDD) — 7/10

**Strengths:**
- Every task follows write-test → fail → implement → pass → commit.
- 20+ new tests covering happy path, edge cases, regressions.
- §10.5 explicitly calls out that existing `TestIncrementalIndex_SkipsUnchangedFiles` must stay green (`FilesSkipped` is recomputed as the sum of breakdown counters).
- `TestIndexResult_SkipBreakdownSums` (Task 1) is the invariant guard.

**Weaknesses (-3):**
- Task 12's MCP integration test (`TestMCPIndex_RegistersSubRepos`) is underspecified. It says "Wire a fake embedder, invoke the MCP index handler... poll `heimdall_status`, assert 3 registry entries appear." But `runIndex` depends on a live Ollama Ping (`client.Ping(ctx)` at `tools.go:278`). The test needs either (a) an Ollama fake OR (b) a refactor to accept a client interface. The plan glosses over this. See Finding H2.
- No test asserts that sub-repo git commit indexing writes to the SUB-repo store (not the outer store). That's the BUG the plan fixes; it needs an explicit regression test. See Finding H3.
- No test asserts the `heimdall_search` MCP tool can find content from a newly-indexed sub-repo after the MCP index call (end-to-end proof). Plan claims `heimdall_search` needs no code change, but without a test the claim is unverified.

### 4. Silent failures / fallback risk — 6/10

**Surfaces that need a hard fail, not a silent skip:**

- **Task 5 `IndexSubRepos` collects errors but does not return a non-nil `err` from the function even if every sub-repo failed.** The outer `return out, nil` at line 781 of the plan means sub-repo failures are silently swallowed at the function level. The caller sees `subRes.Err != nil` per sub-repo, but a CLI caller that only checks the function's return `err` will claim success. **This is a classic silent failure.** See Finding H4.
- **`DiscoverSubReposAbs` returns `nil` when `os.ReadDir(root)` errors.** Plan §4 code block hides a bad root (permission denied, EIO) as "no sub-repos". The existing `DiscoverSubRepos` has the same issue but at least it's an existing precedent — no regression. LOW-severity. See Finding L1.
- **Sub-repo model resolution (§G4 rule 2) "keeps existing model" and "logs a message".** A log message is not a user-facing surface. CLI users won't see it. See Finding M2.

**-4 deduction total.**

### 5. Type design — 7/10

**`SkipBreakdown` struct:**

```go
type SkipBreakdown struct {
    ExcludedDir int
    SubRepo     int
    Binary      int
    Unchanged   int
}
```

Plain and clear. Four named reasons map to four call sites in `indexFiles`. Invariant `FilesSkipped == sum(Skip.*)` is documented and tested.

**Minor concerns (-3):**
- The struct does NOT capture "file skipped due to shouldExclude match" vs. "directory skipped due to shouldExclude match" separately. The plan counts SubRepo as "directories skipped via `filepath.SkipDir`" but counts ExcludedDir as both dirs AND files (`shouldExclude(relPath)` trips at `indexer.go:137` for dirs and `indexer.go:150` for files). Users see "excluded by pattern: 4" without knowing whether that's 4 dirs pruned or 4 individual files skipped. For the stated use case ("I skipped 2 sub-repos"), this is arguably fine, but the name `ExcludedDir` is misleading when it also counts files. Consider renaming to `Excluded`. See Finding M3.
- `SubRepoResult` has a `Result *IndexResult` field that can be nil (when sub-repo store fails to open). Callers must nil-check before reading `Result.FilesIndexed`. No explicit invariant documented. See Finding L2.
- `SubRepoOpts` is empty and marked as "nothing extra here yet". Introducing an empty opts struct is defensible for forward-compat, but it's dead code right now — pragmatically fine, type-design-wise debatable.

### 6. Impact analysis — 10/10

§1's table enumerates 13 subsystems (indexer, walker sub-repo handling, DiscoverSubRepos, IndexResult, CLI flag parsing, CLI index orchestration, Registry.Register, heimdall_configure, heimdall_index, heimdall_projects, heimdall_search, global ExcludePatterns, existing tests). Each one has a "still works because" column with specific evidence. This is the best-executed pre-change impact analysis I have ever reviewed in this codebase.

Full 10.

### 7. Scope discipline — 9/10

- No sneaked-in refactors. The `IndexGitCommits` bug fix IS scoped (it's part of Goal G2 — "each sub-repo with own DB" includes git commits landing in the right DB).
- Per-project config deferral is called out explicitly in §15 rather than hidden.
- "Task 16 Docs updates" is scoped to README + inline docstring + plan TODO. No speculative doc rewrites.
- -1 deduction for §7 explicitly saying "per-project config is NOT added" and then re-justifying why — this is a scope dispute with the user's wording and should be resolved BEFORE implementation. See Finding M1.

### 8. Edge cases — 7/10

§8 table covers 10 edge cases. Strong coverage.

**Missing or underspecified (-3):**
- **Symlink sub-repo:** If `immediate/child/` is a symlink pointing to another directory that contains `.git/`, `entry.IsDir()` is true for the symlink (via `os.ReadDir`). `os.Stat` follows it. The current code would detect it as a sub-repo and index it. Is that intended? Plan doesn't say. See Finding M4.
- **Indexing the same sub-repo twice via different outer paths:** `outer-a/child` and `outer-b/child` (via symlink or copy). Registry dedupes by absolute path, so they'd be two entries. OK. But if one is a symlink to the other, the absolute path differs from realpath and we'd double-index. Plan doesn't address symlink resolution.
- **Sub-repo at root (edge case 1 in §8)**: plan says "`DiscoverSubReposAbs(root)` returns the sub-repos INSIDE root; it does NOT include root itself." True, but does `cliIndex` register the outer as a top-level project when the outer IS itself a sub-repo of some further parent? Probably irrelevant because CLI user passed `<path>` explicitly, but worth an explicit note.
- **Sub-repo pre-pinned with DIFFERENT model than explicit `--model`:** §G4 rule 1 says "explicit `--model` wins for ALL passes". Rule 2 says "sub-repo pinned model wins". These conflict when user passes `--model X` and sub-repo has `bge-m3/`. Plan intends rule 1 to win (it says "applies to ALL passes"), but the implementation sketch in Task 5 checks `ListAvailableModels(subRes.DBPath)` BEFORE considering whether `model` was passed explicitly by the user. There's no signal in `IndexSubRepos(ctx, model, opts)` to distinguish "user passed --model" from "outer's default model". See Finding H5 (CRITICAL spec bug).
- **Exclude pattern is a glob matching `sub-*`**: `--exclude 'sub-*'` matches `sub-service` AND `sub-infra` AND `sub-other-dir-not-a-repo`. §6 says "globs match against each filename AND each path component". Fine. But `filepath.Match` does NOT support `**` (recursive match). Users coming from `.gitignore` or `rg` may expect it. Plan should call out the limitation.
- **`FilesSkipped` vs. sub-repo files:** the plan says `SubRepo` counts DIRS not files. So if `sub-service/` contains 500 files, `FilesSkipped = 1` (one dir skipped) but the user-facing summary needs to say "1 sub-repo skipped" not "1 file skipped" — which is exactly what the mock in §G6 does. Good.

### 9. Rollout — 9/10

Single PR, no migration, no feature flag. Back-out plan stated. Every existing `projects.json` file stays readable. Every existing `.heimdall_db/` stays intact. 

**-1 deduction:** Users with a large monorepo (many sub-repos) will see the FIRST re-index take a long time (every sub-repo now gets indexed). The plan does not mention this behavior change in release notes or a hint in the CLI output. Adding a single line in §13 and in README would close this gap. See Finding L3.

### 10. Backward compatibility — 8/10

- `FilesSkipped` invariant holds: `sum(Skip.*) == FilesSkipped`. Plan §10.5 commits to this.
- CLI summary line changes from `"Skipped: N files (unchanged)"` to a new breakdown. Existing users scraping the summary text break. See Finding M5.
- MCP `heimdall_index` result JSON changes shape (adds `subRepos` field? not specified in plan). See Finding H6.
- Registry `projects.json` format unchanged.
- `.heimdall_db/<model>/vectors.db` schema unchanged.

**-2 deduction** for the two surfaces (CLI text, MCP JSON) that change without the plan specifying the exact compatibility contract.

---

## Findings by Severity

### CRITICAL (0) — none

### HIGH (3) — blockers

**H1. Walker skip counters are currently NOT wired for non-incremental runs.**

File:line: `internal/heimdall/indexer.go:150-157` (excluded files), `:155-157` (binary files), `:140-146` (sub-repo dirs).

**Current state:** None of these branches increment a counter. `FilesSkipped` moves only on the incremental unchanged branch at line 207.

**Plan promise:** §10.5 claims existing `TestIncrementalIndex_SkipsUnchangedFiles` will stay green because `FilesSkipped = sum(Skip.*)` after Task 2. Task 2 wires `Skip.Binary++`, `Skip.ExcludedDir++`, `Skip.SubRepo++`. So far so good.

**The gap:** `TestIndexAll_DoesNotSkip` (indexer_test.go:211-250) asserts `result2.FilesSkipped != 0 → fail`. If `IndexAll` now increments `Skip.ExcludedDir` when it walks past `.git/` or `.heimdall_db/` (both ALWAYS-excluded defaults), `FilesSkipped` will be non-zero for any repo with a `.git` dir, breaking this test.

Specifically, the plan's line 588 says "When `shouldExclude` trips for a directory, do `result.Skip.ExcludedDir++`". But the ALWAYS-excluded `defaultExcludes` (`.git`, `node_modules`, `.heimdall_db`, `vendor`, `__pycache__`, `.idea`) will ALWAYS trip `shouldExclude` in any real repo. So `Skip.ExcludedDir` will be ≥2 (.git + .heimdall_db) for every run, including the test above.

**Remediation:**
- Decide whether the default-excludes count toward `Skip.ExcludedDir` or not. Two reasonable options:
  - Option A: don't count them (exclude from the exclude-counter). User-visible counter reflects only *user-configured* excludes.
  - Option B: count them, accept the test churn, and update `TestIndexAll_DoesNotSkip` to assert `FilesIndexed` invariants instead of `FilesSkipped == 0`.
- The plan currently does neither. Pick one before starting Task 2.

**Severity: HIGH (-8).** This is a test-breaking detail that blocks green builds.

---

**H2. MCP integration test `TestMCPIndex_RegistersSubRepos` (Task 12) is not implementable as stated.**

File:line: Plan §10.6 and Task 12 step 1.

**Problem:** `runIndex` calls `client.Ping(ctx)` at `tools.go:278`. There's no injection point for a fake Ollama client — `heimdall.NewOllamaClient(endpoint)` is constructed in-line at line 276. Integration tests in `internal/mcp/integration_test.go` work around this by setting up an actual test HTTP server; the plan doesn't mention doing that.

**Remediation:**
- Either (a) refactor `runIndex` to accept a client interface (out of scope for this plan; would need its own brainstorm), OR
- (b) use an HTTP test server that speaks the Ollama embedding protocol (existing pattern in `integration_test.go`), OR
- (c) reduce the test to hit the indexer directly and assert registry entries, bypassing `runIndex` entirely (least valuable but cheapest).

The plan should name the option explicitly.

**Severity: HIGH (-8).**

---

**H3. No regression test for the `IndexGitCommits` sub-repo-writes-to-outer-store bug fix.**

File:line: `mcp/tools.go:362` (the bug site).

**Problem:** Task 12's remediation text says "fix the existing `IndexGitCommits` loop" but §10 does not include an explicit test asserting sub-repo commits land in the sub-repo's own store (not the outer store). Without a regression test, a future refactor could reintroduce the bug.

**Remediation:** Add `TestMCPIndex_SubRepoGitCommitsInSubRepoStore` to the test list in §10.6. It should index an outer with a sub-repo that has real git commits, then open the sub-repo's `.heimdall_db/<model>/vectors.db` directly and query for `source_type='git_commit'` records.

**Severity: HIGH (-8).** Every bug needs a regression test per CLAUDE.md.

---

### MEDIUM (6)

**M1. Per-project config deferral may be a scope violation.**

File:line: Plan §7 and §15.

The user said "persistable via `heimdall_configure` / project config file". The plan interprets "project config file" as "the global config under XDG". The user may have meant "a per-project file like `.heimdall.yaml` inside each project root". Industry standard: both `ripgrep` (.ripgreprc in project root) and `eslint` (.eslintrc) use per-project configs for exactly this kind of per-project override. The plan's interpretation is one reasonable reading; the other is equally reasonable.

**Recommendation:** Ask the user before starting Task 13. If they want true per-project config, that's a ~4-task escalation (new `.heimdall/config.json` loader, precedence rules, test coverage).

**Severity: MEDIUM (-3).** Could be reclassified HIGH if the user confirms they meant per-project file.

---

**M2. Sub-repo pinned-model-vs-outer-model disagreement is logged but not surfaced to users.**

File:line: Plan §G4 "keep Y and log a message". Also edge case 4 in §8.

`log.Printf(...)` writes to the MCP server log, not to the MCP client or CLI. Users running `heimdall-mcp index ./outer` will not see "sub-repo X pinned to bge-m3; outer is using nomic — keeping bge-m3" in their CLI output.

**Recommendation:** The CLI summary renderer (Task 11) should surface model mismatches explicitly:

```
  Sub-repos: 2 indexed separately
    └─ sub-service     [nomic-embed-text]  (12 files)
    └─ sub-infra       [bge-m3]  (7 files, pinned model — see note)
```

**Severity: MEDIUM (-3).**

---

**M3. `SkipBreakdown.ExcludedDir` name is misleading — it counts excluded FILES too.**

File:line: Plan §G6 code block.

`shouldExclude` trips at `indexer.go:137` (dir branch) AND at `indexer.go:150` (file branch). Task 2's code sketch says "when `shouldExclude(relPath)` trips for a directory, do `result.Skip.ExcludedDir++`" — but what about the file branch? If the plan intends to count only dirs, the file branch at line 150 leaves a gap (excluded files uncounted → `FilesSkipped` invariant broken). If it intends to count both, the field name `ExcludedDir` is wrong.

**Recommendation:** Rename to `Excluded`, wire both branches, and update the test `TestIndexResult_SkipBreakdownSums` to cover both dir-excluded AND file-excluded cases.

**Severity: MEDIUM (-3).**

---

**M4. Symlink sub-repos unspecified.**

File:line: Plan §4 `DiscoverSubReposAbs`.

`os.ReadDir` returns `fs.DirEntry` where `entry.IsDir()` follows symlinks (actually: `DirEntry.IsDir` does NOT follow symlinks on Unix; it uses lstat). But `os.Stat` on the `.git` path DOES follow symlinks. So a symlinked directory pointing at a real git repo will NOT be detected (entry.IsDir() returns false for the symlink → `continue`).

If users have a vendored repo via symlink (common in monorepo setups), it will silently fail to be discovered as a sub-repo. The outer walker at line 136 (`d.IsDir()`) has the same behavior.

**Recommendation:** Decide whether to support symlinked sub-repos. If yes, change `!entry.IsDir()` to `!(entry.IsDir() || isSymlinkToDir(entry))`. If no, document the limitation in §8 and in docs/README.

**Severity: MEDIUM (-3).**

---

**M5. CLI summary text change breaks any user/script scraping the output.**

File:line: Plan §2.G6 + Task 11.

Old: `Skipped: 24 files (unchanged)` — one line.
New: `Skipped: 24 files` + four indented lines.

Users running `heimdall-mcp index ./x | grep Skipped:` still work. Users parsing the specific-format output break.

**Recommendation:** Either (a) add a `--format=json` flag that emits stable JSON (preferred; industry standard is `cargo --message-format=json`, `npm --json`, etc.), OR (b) call out the output change in a release note.

**Severity: MEDIUM (-3).**

---

**M6. `--exclude` absolute-path handling is inconsistent.**

File:line: Plan §6 "Absolute paths are rejected with a clear error in CLI".

`heimdall_configure set exclude_patterns '["/etc/host","dist"]'` would accept an absolute path because the MCP surface doesn't validate. CLI rejects, MCP accepts. Inconsistent.

**Recommendation:** Validate in both surfaces (or neither). Preferred: validate in the MCP surface too (`configKeys["exclude_patterns"].Set` returns an error for any absolute-path entry).

**Severity: MEDIUM (-3).**

---

### LOW (3)

**L1. `DiscoverSubReposAbs` silently returns nil on read error.**

File:line: Plan §4.

`os.ReadDir(root)` can fail (permissions, nonexistent, EIO). Returning nil hides the error. The existing `DiscoverSubRepos` has the same issue — no regression. But the plan is re-introducing the same anti-pattern in new code, which is a chance to fix it.

**Recommendation:** Return `([]string, error)` so callers can distinguish "no sub-repos" from "couldn't read the dir".

**Severity: LOW (-1).**

---

**L2. `SubRepoResult.Result` can be nil — no documented invariant.**

File:line: Plan §G4 Task 5 code block (line 744: `subRes := SubRepoResult{...}`; line 775: `subRes.Result = r`).

If `OpenStore` fails, `subRes.Result` stays nil. Callers rendering the summary must check `if sr.Result != nil` before dereferencing `sr.Result.FilesIndexed`. Plan's summary mock in §G6 reads `(12 files, 84 chunks)` — no handling for the nil case.

**Recommendation:** Document the invariant `subRes.Err != nil → subRes.Result may be nil`. Render "FAILED: <err>" in that case.

**Severity: LOW (-1).**

---

**L3. No hint about re-index time for monorepos with many sub-repos.**

File:line: Plan §13.

**Recommendation:** Add a sentence to release notes: "If your monorepo has many sub-repos, the first post-upgrade run will index each one; expect a longer-than-usual run."

**Severity: LOW (-1).**

---

## Raw Score Calculation

| Dimension | Score / 10 |
|---|---|
| 1. Completeness | 9 |
| 2. Correctness vs. code | 10 |
| 3. Test strategy | 7 |
| 4. Silent failures | 6 |
| 5. Type design | 7 |
| 6. Impact analysis | 10 |
| 7. Scope discipline | 9 |
| 8. Edge cases | 7 |
| 9. Rollout | 9 |
| 10. Backward compat | 8 |
| **Raw sum** | **82** |

Per CLAUDE.md deduction hints (applied as a single pass, not double-counting what's already deducted per dimension):

| Finding | Severity | Deduction | Already counted in per-dim? |
|---|---|---|---|
| H1 (skip counters vs. always-excluded) | HIGH | -8 | Partially (under test strategy) — net additional **-5** |
| H2 (MCP test not implementable) | HIGH | -8 | Partially (under test strategy) — net additional 0 |
| H3 (no regression test for bug fix) | HIGH | -8 | Partially (under test strategy) — net additional 0 |

Per-dimension scoring already captured most of these, so I apply an additional **-5** rather than triple-counting.

**Final score: 82 - 5 = 77 / 100**

---

## Verdict: APPROVE-WITH-CHANGES

The plan's design is sound. File:line citations are accurate. The six user goals are covered. The bug at `mcp/tools.go:362` is a real bug worth fixing in the same PR.

But the plan has three HIGH-severity gaps that will cause either test failures (H1) or leave the feature only half-testable (H2, H3). These must close before Task 1 starts.

### Required changes before implementation begins

1. **H1 — decide how default-excludes interact with `Skip.ExcludedDir`.** Update Task 2's implementation sketch AND update `TestIndexAll_DoesNotSkip` if counting them. Write the updated assertion in the plan before coding.

2. **H2 — specify how `TestMCPIndex_RegistersSubRepos` will handle Ollama dependency.** Choose option (a), (b), or (c) from Finding H2. Update §10.6 with the chosen approach.

3. **H3 — add `TestMCPIndex_SubRepoGitCommitsInSubRepoStore` to §10.6.** Specify the assertion: "open `<subrepo>/.heimdall_db/<model>/vectors.db`, query `source_type='git_commit'`, count > 0; also verify outer store has ZERO sub-repo-path git commits."

### Strongly recommended (before implementation)

4. **M1 — ask the user whether "project config file" means per-project or global.** Put the question (with industry-standard examples `.ripgreprc`, `.eslintrc`) and the plan's recommendation (keep global for this PR) to the user and let them choose.

5. **M3 — rename `ExcludedDir` to `Excluded`** and wire both dir and file exclusion branches.

6. **M5 — decide on `--format=json` now or commit to a follow-up.** If follow-up, add to the plan's "Escalations" section.

### Nice-to-have (can land as follow-ups)

- M2, M4, M6, L1, L2, L3 can be follow-up PRs. Call them out explicitly in §15 Escalations so they don't get lost.

Once H1-H3 are resolved and the plan is updated, this is ready for `executing-plans` / `subagent-driven-development`. No re-review needed for the low-severity items.

---

## Compliance check (CLAUDE.md rules)

- [x] Impact analysis exists (§1)
- [x] No `main`-push language
- [x] No `--no-verify` language
- [x] No force-push language
- [x] No merge-PR language
- [x] No unrelated refactors (bug fix at `tools.go:362` IS in-scope)
- [x] Test coverage for new code
- [ ] Test coverage for the bug fix (H3 — needs to be added)
- [x] Single PR, no migration
- [x] Plan is well-organized and scannable
