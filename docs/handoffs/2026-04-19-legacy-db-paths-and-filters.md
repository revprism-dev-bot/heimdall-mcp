# Handoff — 2026-04-19 — Legacy DB paths, sub_project filter, reindex correctness

**Purpose:** resume from a fresh Claude Code session without re-reading the prior-session conversation. This handoff documents a cluster of indexer/CLI/MCP issues discovered during Mac-side dogfooding after the 2026-04-19 PR batch (#67 #68 #69 #70 #71).

Paste the **Opening prompt** below into the new session, or just point Claude at this file.

---

## Opening prompt for the next session

> **Status at `main` @ `f6fb16f` (2026-04-19 evening, post-#71 README clarity):** clean tree.
> Five PRs shipped today on `revprism-dev-bot/heimdall-mcp`: **#67** (nested sub-repos as separate projects), **#68** (sub-repo incremental + progress), **#69** (caller-wins multi-model sub-repo + interrupted-run marker), **#70** (always-prompt CLI UX + multi-model flags), **#71** (README clarity on Anthropic API boundary). Full suite `go test ./... -count=1` green. Gitea mirror at `192.168.1.167:3000` is synced to `f6fb16f`.
>
> **But Mac dogfooding on `~/Code/payments-analyzer` revealed a cluster of real bugs that the merged PRs don't fully address.** You must treat the symptoms below as indicators of an **active correctness problem in the indexer/CLI/MCP interaction**, not as user error or config drift.
>
> **First action: rebuild, then work the punch list top-down. Every item has a diagnostic command; run those before assuming a cause.**

---

## Session context — what just shipped

| PR | Title | Key effect |
|---|---|---|
| #67 | `feat(indexer): index nested sub-repos as separate projects` | Each nested git repo gets its own `.heimdall_db/` + registers as a separate project |
| #68 | `fix(indexer): sub-repo pass incremental + progress output` | Sub-repo pass uses `IndexIncremental` (was `IndexAll`), emits per-sub-repo progress lines |
| #69 | `fix(indexer): multi-model sub-repo indexing + interrupted-run prompt` | Caller-specified model wins over existing; marker file at `.heimdall_db/.indexing-in-progress`; C/R/Q prompt on restart |
| #70 | `fix(cli): always show model multi-select in TTY; add multi-model flags` | TTY always prompts, `--model a,b`, `--all-models`, `--model` repeated; unified multi-select replaces the C/R/Q dialog |
| #71 | `docs(readme): clarify Anthropic API boundary vs local-only Heimdall work` | README diagrams + Network Interactions section make the Anthropic data boundary explicit |

Downstream `revprism/*` PRs carrying committed `.heimdall_db/` indexes:

| Repo | PR | State | Notes |
|---|---|---|---|
| payments-analyzer | #90 | **MERGED** 2026-04-18 | wrapper, both models (nomic 768d + bge-m3 1024d), paths use `_latest` suffix |
| payments-analyzer-app | #253 | open | nomic + bge-m3, non-`_latest` paths, + CI-ignore patch on branch (`.github/workflows/ci.yml` + `.dockerignore` + `vitest.config.ts`) |
| payments-analyzer-service | #263 | open | nomic + bge-m3, non-`_latest` paths, **missing CI-ignore patch** |
| payments-analyzer-infra | #355 | open | nomic + bge-m3, non-`_latest` paths, **missing CI-ignore patch** |

Gitea mirror state at session close — all five mains in sync with GitHub mains.

---

## Open problems (ranked by evidence + severity)

### 1. Reindex appears to not land in the store the MCP reads (CRITICAL)

**Symptom (from Mac heimdall_status on 2026-04-19 after reindex):**
- `Last indexed: 2026-04-11` (8 days stale, pre-#67)
- `1556 files / 8351 chunks`
- User reports re-running `heimdall-mcp index ~/Code/payments-analyzer` today

**Why this matters:** if reindex runs, reports success, but the MCP still reads from an earlier store, the tool is quietly wrong. Users can't trust retrieval.

**Most likely root cause — path-naming split:**
- Legacy CLI (pre-#67 / #90-era): `.heimdall_db/<model>_latest/vectors.db`
- Current CLI (post-#67): `.heimdall_db/<model>/vectors.db`
- Wrapper's merged PR #90 used `_latest`. Sub-repo PRs #253/#263/#355 use non-`_latest`. The two aren't interchangeable.
- If the current binary writes to one path while MCP resolution prefers the other, reindex is effectively a no-op from the MCP's perspective.

**Diagnostic commands (Mac):**

```sh
find ~/Code/payments-analyzer -name vectors.db -not -path '*/.git/*' | \
  xargs -I {} ls -la {}
# Look for BOTH: .heimdall_db/<model>_latest/ AND .heimdall_db/<model>/

~/Code/heimdall-mcp/heimdall-mcp --version
# Must report f6fb16f or later. If older, the binary wasn't rebuilt.

~/Code/heimdall-mcp/heimdall-mcp status
# Compare the path it reports against what heimdall_status MCP returns.
```

**Fix approach (follow-up PR):**

- `internal/heimdall/dbpath.go::ModelDBDir` — on read, accept `<model>` OR `<model>_latest`, preferring the more recently-modified one. On write, standardize on non-`_latest` going forward.
- OR introduce auto-migration: on `OpenStore`, if legacy `_latest` dir exists and no current-style dir exists, rename it. Log migration. One-shot.
- Add regression test: open a repo with a pre-existing `<model>_latest/` dir, call the new `OpenStore`, assert it reads/writes the migrated location.
- Deprecation log line on any legacy path found, pointing to the follow-up's commit.

---

### 2. `sub_project` filter returns zero for every repo name (HIGH)

**Symptom:** `heimdall_search query=… sub_project="payments-analyzer-app"` → zero hits, even though unfiltered search returns hits with paths like `payments-analyzer-app/src/middleware.ts`.

**Hypotheses, in order of likelihood:**

1. **Chunks in the store predate sub_project tagging.** If the stale 2026-04-11 store is what's being searched (per problem #1), those chunks never had a `sub_project` field set because sub-repo support didn't exist then. Unfiltered search works because it doesn't look at that field.
2. **Current indexer doesn't tag chunks.** Grep `internal/heimdall/chunk.go`, `internal/heimdall/store.go`, `internal/heimdall/indexer.go` for where a sub_project / context_path is written during chunk creation. PR #67 added sub-repo discovery but we haven't confirmed it also tags the outer wrapper's own chunks with `sub_project=""` (or the wrapper's project name) in a way that survives search filtering.
3. **Filter mismatches on case/colon/namespace.** `sub_project` parameter might expect exactly what's stored (e.g. registry name `payments-analyzer-app` vs. path-derived label). Need to confirm the canonical value.

**Diagnostic commands:**

```
heimdall_search query="payment" detail=full
# Inspect a raw result — does the chunk metadata include sub_project at all?

heimdall_projects
# What are the exact project names? Filter must match these.

heimdall_explain query="payment"
# Shows which chunks match, with metadata.
```

```sh
# Go-side: show the SQL being used
grep -rn 'sub_project' internal/heimdall/ internal/mcp/ | head -30
```

**Fix approach (only after problem #1 is ruled out):**

- If chunks genuinely lack `sub_project`: indexer must set it at chunk creation time. Backfill path: a migration that re-tags chunks in an existing store based on their source path prefix.
- If chunks have it but filter logic is wrong: fix the SQL in `store.go`, add integration test covering both filtered + unfiltered paths, ensure `heimdall_ls` and `heimdall_search` agree on the value.

---

### 3. Registry auto-registration incomplete (MEDIUM)

**Symptom:** After Mac reindex today, `heimdall_projects` lists 3 projects (`payments-analyzer`, `heimdall-mcp`, `payments-analyzer-app`) but not `payments-analyzer-service` or `payments-analyzer-infra`, even though those sub-repos' `.heimdall_db/` dirs contain chunks.

**Likely cause:** PR #67's auto-register fires inside `IndexSubRepos` only when a sub-repo is freshly indexed. If the sub-repo was indexed earlier (stale store) AND the current run is a no-op incremental (no new files), the register call may be skipped.

**Fix approach:**

- Register unconditionally at the top of `IndexSubRepos` per iteration, before the incremental short-circuit. Registry dedupe handles repeat calls.
- Regression test: index a sub-repo twice; assert registry has exactly one entry per sub-repo after each run.

---

### 4. `heimdall_ls` hierarchy sparse (LOW, same root cause as #2)

Root says only `docs/ (72)` despite 8351 chunks. Same cause as the filter bug: chunks predate sub-repo tagging so the hierarchy they expose is what the old indexer computed. Re-indexing with current code **should** populate the full tree. Verify after problems #1–#2 are addressed.

---

### 5. Parallel Ollama timeouts on bursts (LOW, separate track)

**Symptom:** burst parallel embed calls deadline-exceed. Single calls fine.

**Fix approach (separate PR, not blocking):**

- Add a semaphore around embed requests, default concurrency 2.
- Raise the per-request deadline under bursty conditions; or surface it as a config knob (`embed.timeout_ms`, `embed.max_concurrent`).
- Documented retry with backoff for `context deadline exceeded`.

---

## Suggested PR sequence (follow-ups)

1. **`fix(dbpath): legacy <model>_latest auto-migration + tolerant read`** — closes problem #1. Small, focused, unblocks everything else.
2. **`fix(indexer): tag chunks with sub_project and context_path`** (only if #1 doesn't resolve problem #2 — confirm with heimdall_search detail=full after #1 merges).
3. **`fix(indexer): always register sub-repos in IndexSubRepos`** — closes problem #3.
4. **`fix(ollama): bounded parallelism + config for embed concurrency`** — closes problem #5.

PRs #253 (app) is the only sub-repo PR with CI-ignore patches so far. Before merging #263 (service) and #355 (infra), replicate the patch from #253:
- `.github/workflows/ci.yml` — add `'!.heimdall_db/**'` to the `code` paths-filter
- `.dockerignore` — add `.heimdall_db`
- `vitest.config.ts` or equivalent — exclude `.heimdall_db/**` from test globs

---

## Gitea mirror state at session close

All mirrors force-pushed to match GitHub mains during this session. Verify with:

```sh
for r in heimdall-mcp payments-analyzer payments-analyzer-app payments-analyzer-service payments-analyzer-infra; do
  gh=$(gh api "repos/$([ $r = heimdall-mcp ] && echo revprism-dev-bot || echo revprism)/$r/commits/main" --jq '.sha' | head -c 10)
  gt=$(curl -sf "http://admin:<token>@localhost:3000/api/v1/repos/admin/$r/branches/main" | python3 -c "import sys,json; print(json.load(sys.stdin)['commit']['id'][:10])")
  [ "$gh" = "$gt" ] && echo "$r IN SYNC $gh" || echo "$r DIVERGED gh=$gh gt=$gt"
done
```

(Token is in the `gitea` remote URL — `git remote -v | grep gitea | head -1`.)

---

## First commands to run in the new session

```sh
# 1. Rebuild heimdall-mcp
cd /home/noname/Code/heimdall-mcp && git pull --ff-only && go build -o heimdall-mcp ./cmd/heimdall-mcp && ./heimdall-mcp --version

# 2. Confirm the diagnostic picture matches this handoff on the Mac side (run on Mac)
find ~/Code/payments-analyzer -name vectors.db -not -path '*/.git/*' -print -exec ls -la {} \;

# 3. Pick problem #1 and spin up a fix PR
```

Do NOT start by re-indexing manually. Do NOT `rm -rf` the legacy paths — that's the anti-pattern we're shipping the fix for.
