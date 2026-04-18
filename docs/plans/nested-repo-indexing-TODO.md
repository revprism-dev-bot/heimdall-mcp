# TODO — Nested Repo Indexing (feat/index-nested-repos-separately)

Orchestration task tracking for the "index nested sub-repos separately" feature.

Worktree: `/home/noname/Code/heimdall-mcp-worktrees/index-nested-repos`
Branch:   `feat/index-nested-repos-separately`
Base:     `main`
Plan:     `docs/plans/nested-repo-indexing.md`

## User-visible goals (all 6 must ship)

- [x] G1. Outer wrapper is still indexed correctly (status quo preserved)
- [x] G2. Each nested sub-repo gets its OWN `.heimdall_db/` inside itself (not merged)
- [x] G3. Each discovered sub-repo is auto-registered in the project registry
- [x] G4. Sub-repo inherits parent's embedding model config on auto-discovery
- [x] G5. `--exclude` CLI flag (repeatable), `heimdall_configure` MCP wiring, and persistent per-project config
- [x] G6. CLI summary replaces `Skipped: N` with categorized counters (`excluded-dir`, `sub-repo (indexed separately)`, `binary`, `unchanged`) plus a list of sub-repos indexed separately with their DB paths

## Orchestration phases

- [ ] P1. Plan agent — produces `docs/plans/nested-repo-indexing.md`
- [ ] P2. Plan-review agent — deep-code-review scores plan ≥95
- [ ] P3. Implementation agent — TDD, tests first, all 6 goals
- [ ] P4. Implementation-review agent — deep-code-review + pr-review-toolkit + silent-failure-hunter + type-design-analyzer, scores ≥95
- [ ] P5. Improve loop (≤5 cycles) until score ≥95
- [ ] P6. Open PR against `main` from `feat/index-nested-repos-separately`

## Compliance rails (all MUST hold)

- [ ] No direct push to `main`
- [ ] No `--no-verify` / skip hooks
- [ ] No force-push
- [ ] No merging the PR (open only)
- [ ] No history rewrite on main
- [ ] No silent failures / hidden fallbacks
- [ ] No unrelated refactors
- [ ] Tests green before PR
- [ ] Quality score ≥95 before PR
- [ ] Every sub-agent reads CLAUDE.md + MEMORY.md + ~/.custom, uses skills, backs claims with evidence

## Status table (updated after every agent)

| Phase | Agent | Status | Notes |
|-------|-------|--------|-------|
| P1 Plan | (pending) | COMPLETE | Plan landed at docs/plans/nested-repo-indexing.md |
| P2 Plan review | (pending) | COMPLETE | Review landed at docs/plans/nested-repo-indexing-review.md |
| P3 Implement | impl-agent | COMPLETE | All 6 goals + 13 tasks shipped |
| P4 Impl review | (pending) | PENDING | |
| P5 Improve loop | (pending) | PENDING | |
| P6 PR | impl-agent | COMPLETE | Branch pushed, PR opened |
