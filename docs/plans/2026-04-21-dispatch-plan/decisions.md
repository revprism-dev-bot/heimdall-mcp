# Dispatch plan — user decisions

**Session:** 2026-04-21 (post PR #81 verification)
**Purpose:** Capture user answers to all dispatch questions before any agents are launched.

---

## Question queue

(Answers appended below as they come in.)

---

### Q1 — 27 smart-quoted files in working tree

**Answer:** 1 — revert all 27 files.

**User note:** "u are the only one editing this files" — no risk of clobbering human-authored changes; the substitution was introduced by tooling on the assistant's side, nothing to rescue.

**Dispatch implication:** agent A1 runs `git checkout -- <files>` on all 27 after a safety scan (scan is a sanity probe, not a gate — user has authorized the revert unconditionally).

---

### Q2 — superseded by Q2c (migration framework vs ad-hoc)

Original Q2 ("`heimdall_status` path behavior") was narrow. User first observed that most of Stream B is legacy migration (Q2b), then clarified they *do* want migration support for future schema changes. The real question is therefore about *how* migration is structured, not whether it exists. Re-asking as Q2c.

---

### Q2c — Migration framework vs ad-hoc

**Answer:** 2 — versioned migration framework.

**Dispatch implications:**
- New package (likely `internal/heimdall/schema/` or `internal/heimdall/migrate/`) with a numbered migration list, `schema_version` tracking, lock-protected idempotent apply, auto-run on store open.
- `_latest/` → bare rename becomes **migration-001**.
- Stale-registry-repair becomes **migration-002**.
- `BackfillSubProject` for sub-repo chunks becomes **migration-003**.
- PR1/PR2/PR3 from the original legacy-db-fixes plan merge into one PR that introduces the framework + three migrations; PR4/PR5 stay separate.
- Integration tests set up prior schema states via the framework's own test helpers, not ad-hoc fixtures.

---

### Q3 — `heimdall_status` target path

**Answer:** 3 — optional `path` input, default to `s.Index.Path`.

**User context note (roadmap, not a decision alteration):** "eventually we would also want to support a centralized db (on same machine ollama is running) so not everyone need to have same db on their machine."

Saved as project memory `project_centralized_db_roadmap.md`. Design implication carried forward: future work should prefer **project identifiers (name + stable ID) over filesystem paths** where possible, and avoid single-writer assumptions that would be painful to retrofit when a shared central DB arrives. This does **not** change today's answer — option 3 is fine for local usage and the `path` param can evolve into a `project_id` param later without breaking callers.

**Dispatch implication:** in the migration-framework PR or a sibling, add `path` input to `heimdall_status`. Validate against registry; unset → `s.Index.Path`.

**Roadmap note promoted to tracked work:** Centralized-DB direction added to `TODO.md` §8 as a future-work item (not blocking current dispatch). Design spike + scoping doc to be written before any implementation.

---

### Q4 — `sub_project=""` filter semantics

**Answer:** 3 — typed filter variant wrapping option 1 semantics.

**Shape:**
```go
// internal/heimdall/store.go (or a sibling)
type SubProjectFilter struct {
    Mode SubProjectMode  // All | RootOnly | Named
    Name string          // populated only when Mode == Named
}
```

**MCP boundary mapping (wire format unchanged, no caller breakage):**
- `sub_project` omitted or `""` → `{Mode: All}`
- `sub_project: "__root__"` → `{Mode: RootOnly}`
- `sub_project: "<anything-else>"` → `{Mode: Named, Name: "<anything-else>"}`

**Dispatch implications:**
- Introduce the type in `internal/heimdall/` alongside the migration framework or as a tiny prep PR.
- Refactor `SearchFiltered` / `SearchMemories` / any other callers to take the typed filter.
- Registry-name validation (reject `^__[a-z]+__$` per the existing rule at `store.go:17–21`) moves into the typed filter's constructor.
- Travels unchanged if/when the DB centralizes (per roadmap note above).

---

### Q5 — Stream B merge strategy

**Answer:** 3 — fixture-based integration test in CI.

**Dispatch implications:**
- Migration-framework PR must ship with a golden-path integration test: seed an old-schema DB fixture → open store → assert all pending migrations applied → assert post-migration queries return the same results as a pre-migration baseline. Fixture lives in `internal/heimdall/testdata/` (or equivalent).
- Each new migration added later must come with its own fixture entry. Document this contract in the migration package's doc comment so it's hard to miss.
- PR4 (ls UX) and PR5 (Ollama concurrency) ship on code evidence + regular CI — no migration-fixture gate required for them.
- No manual dogfood step required pre-merge for any PR. CI carries the weight.

---

### Q6 — Stream C scope

**Answer:** 2 — dispatch C1 + C2 now in parallel with A and B; defer C3 until Stream B lands.

**Critical directive (user, verbatim):** *"once done dispach the next one DO NOT WAIT FOR ME TO TELL U TO DISPACH IT."*

Interpretation: when Stream B merges to `main`, immediately dispatch C3 without asking for re-approval. C3 is already scoped and authorized as part of this planning round; waiting for an explicit "go" would just be rubber-stamping. Saved as a feedback memory (`feedback_auto_dispatch_chain.md`) for general application.

**Dispatch implications:**
- C1, C2 launch in the same batch as Stream A + Stream B (parallel).
- A chaining trigger fires on Stream B merge → dispatches C3 automatically, reports the dispatch to the user after it's already started (not before).
- C3 itself is: (a) UserPromptSubmit nag-after-N-turns, (b) promote `heimdall_remember` triggers in CLAUDE.md, (c) delete generic "use ALL tools proactively" line from MCP instructions.

---

### Q7 — Per-agent dispatch mechanics + legacy-db-fixes docs disposition

**Part A answer:** 2 — worktree per agent, PR per agent, single reviewer pass (no 95+ iteration cycle). Proportionate rigor: most tracks are small, one reviewer per PR plus main-agent diff pass is enough. Reserves the 95+ cycle for cases that actually need it.

**Part B answer:** (iii) — archive good parts of `docs/plans/2026-04-19-legacy-db-fixes/` (bug catalog + root-cause analyses + PR-review findings) into the migration-framework PR's design doc; delete the rest. No verbatim preservation of ad-hoc migration drafts.

**Additional directive (user, verbatim):** *"do not wait for me to dispatch the part B. once A is done u launch B."*

Interpretation: gate sequencing is A → B (not A ∥ B as I originally had in Q6). Stream A (and C1+C2 which are already parallelized with A) land first; the moment A is complete, Stream B auto-dispatches without further approval. Compounds with the Q6 auto-dispatch directive: B → C3 also auto-chains.

---

## Final dispatch plan

### Wave 1 — launch NOW, parallel

| Agent | Task | Worktree? | Output |
|-------|------|-----------|--------|
| A1 | Revert 27 smart-quoted files (`git checkout -- <files>`) after a safety scan. Agent reports any file that contains non-quote changes mixed in — those get rescued manually before revert. | No (main tree) | Clean working tree; summary report |
| A2 | Acceptance-gate step 4: run `./heimdall-mcp hooks analyze-session` on the two baseline transcripts (`fa9b59da-…` and `13818937-…`). Expected: 3 triggers/0 followed and 1 trigger/0 followed respectively. | No | Pass/fail report |
| A3 | Delete stale `.claude/worktrees/agent-a46e6572/` and `agent-ade14367/` after confirming abandoned (no uncommitted changes or unique branches). `git worktree prune` afterwards. | No (operates on worktree dir) | Cleanup report |
| C1 | TODO.md §3: "Auto-detect path for memories" — when `heimdall_remember` is called without an explicit `path`, infer it from MCP server's project root. Add tests. | Yes | PR |
| C2 | TODO.md §3: "Update hook injection to respect scope when CWD is a subpath" — fix `hook_user_prompt.go` so auto-inject honors the actual invocation subpath. Add tests. | Yes | PR |
| Docs-archive | Q7 Part B: extract bug catalog + root-cause analyses from the 17 files in `docs/plans/2026-04-19-legacy-db-fixes/` into a single `docs/plans/2026-04-21-migration-framework/00-problems-catalog.md` (new dir). Delete the old directory afterwards. Do NOT produce solution designs — that's Wave 2's job. | No (untracked → new tracked + deletion) | New consolidated doc; directory removed |

### Wave 2 — auto-fires when Wave 1 complete

| Agent | Task | Worktree? | Output |
|-------|------|-----------|--------|
| B-migration | Migration framework + migration-001 (`_latest` → bare) + migration-002 (stale registry repair) + migration-003 (sub_project backfill) + `SubProjectFilter` typed variant + `heimdall_status` path input + CI fixture-based integration test. Single PR. | Yes | PR |
| B-ls | PR4 from original plan: `toolLs` fanout across registry for sub-repo hierarchy display. Independent of migration framework. | Yes | PR |
| B-ollama | PR5 from original plan: Ollama bounded concurrency + configurable timeout. Independent of migration framework. | Yes | PR |

### Wave 3 — auto-fires when Wave 2 complete

| Agent | Task | Worktree? | Output |
|-------|------|-----------|--------|
| C3 | P1 bundle: (a) UserPromptSubmit nag-after-N-turns hook, (b) promote `heimdall_remember` triggers in CLAUDE.md, (c) delete generic "use ALL tools proactively" line from MCP instructions. Single PR. | Yes | PR |

### Per-agent contract (all waves)

- **Read** global CLAUDE.md, project CLAUDE.md (if any), MEMORY.md + all referenced memory files before starting.
- **Worktree discipline:** all code-writing agents work in an isolated worktree under `.claude/worktrees/<purpose>/`; main tree stays clean.
- **Feature branch + PR:** never push to `main` directly.
- **Tests:** all new code has tests; all tests pass (`go test ./...`) before PR.
- **Reviewer:** main agent spawns one `code-reviewer` agent per PR; if findings are raised, implementation agent iterates once; if still blocking, escalate to user.
- **Reporting:** agent reports summary + PR URL on completion.

### Blocked-on-user (not dispatchable)

- Acceptance-gate step 5 (full ingest round-trip with `last-session-review.json`) — requires user to `/exit` this session and start a new one with real work. Carried over.








