# Post-ship handoff — Claude's self-use of heimdall

**Status:** P0 + P0.5 shipped to `main` on 2026-04-20 as PR #79 (squash commit `f099bc5`). Pipeline verified working on a real transcript.

**For next session:** start by reading this file. It captures what works, what doesn't, and what to do next.

---

## 1. What was done

| PR / commit | Scope |
|---|---|
| [#79 on `main`](https://github.com/revprism-dev-bot/heimdall-mcp/pull/79) at `f099bc5` | Squash of 13 commits: planning docs, P0 behavior changes, P0.5 observability, three post-ship hotfixes. |

**P0 behavior levers**

- `internal/mcp/server.go` — instruction block at MCP `initialize` rewritten as 5 `WHEN → ACTION` trigger pairs + a pointer to the SessionStart nag. Extracted to `heimdallInstructions` constant so tests can verify without a live Ollama.
- `internal/cli/hook.go:emitTierBNote` — two-line read/write-scoped banner. `heimdall_search: unavailable` + `heimdall_remember and heimdall_index_text still work` on index-verify errors. Ollama-down banner unchanged.
- `internal/cli/hook.go:renderLastSessionReview` + `formatSessionStartBlock` — next-session nag reads `.heimdall_db/hooks/last-session-review.json`, renders "Last session review" section, consumes the file.
- `internal/cli/hook_stop.go:extractTranscriptSummary` — marker-scanning extractor (correction / frustration / teaching / workaround regex). Returns both a summary for `IngestSessionSummary` and a candidate list.
- `internal/cli/hook_stop.go:writeLastSessionReview` — persists the review record on SessionEnd with suppression rule `candidates <= 2 AND writes >= 1 AND len(misses) == 0`.

**P0.5 observability**

- `internal/mcp/server.go:handleToolsCall` — structured `event=mcp.tool_call` log line per dispatch (tool, duration_ms, input_bytes, is_error, result_size, `mcp_server=<host>-<pid>` key).
- `internal/cli/hook_user_prompt.go` — `stage=ok` log gains `auto_inject_bytes`, `auto_inject_hash` (12-char sha256), `top_hit_files`.
- `internal/cli/missed_calls.go` — `AnalyzeTranscript` with 4-rule registry mirroring the MCP instruction bullets; populates `misses[]` on the review record.
- `internal/cli/hooks_analyze_session.go` — `heimdall-mcp hooks analyze-session --transcript <path>` CLI.
- `internal/heimdall/hooklog.go` — rotation 10 MB → 20 MB.

**Post-ship hotfixes (critical — the pipeline was silently no-op without these)**

- `internal/cli/transcript_shape.go` — `extractRoleAndContent` handles both synthetic (top-level `role`/`content`) and real Claude Code (`type` discriminator + nested `message`) shapes. `normalizeToolName` strips `mcp__<server>__` prefix so `mcp__heimdall__heimdall_remember` matches `heimdall_remember`. Also filters out `isMeta: true` entries (skill-loader / tool-invocation echoes).

---

## 2. Verified working

- `go test ./... -count=1` — all packages green on `main`.
- `gofmt -l .` — clean for files touched by the PR (the 27 other drifted files are unrelated pre-existing gofmt debt).
- `./heimdall-mcp hooks analyze-session --transcript <live session>` — produces real output. Last run on `fa9b59da-…-jsonl` showed 2 triggers (1 real task-start-recall miss, 1 borderline user-correction), down from 5 with 4 false positives before the `isMeta` filter.
- Real-transcript fixture at `internal/cli/testdata/transcript_real_shape.jsonl` locks this in as a regression trap.

---

## 3. Known limitations — not done in this PR

Tracked as follow-ups, safe to do whenever:

1. **Correction regex over-eager on `no <noun>` constructions** (e.g. "no more questions" matches `\bno,?\s`). Not every `no` is a correction. Consider requiring a following verb or tightening the pattern.
2. **`task_start_recall` window is 3 assistant turns.** Brainstorming-style openings that recall after clarifying questions miss this window. Consider widening to 5–6.
3. **True per-session correlation on `mcp.tool_call` lines needs Claude Code to thread its `session_id` through MCP `initialize` params** — outside this repo's control. The `mcp_server=<host>-<pid>` key is a good-enough proxy meanwhile: one MCP subprocess ≈ one session, mostly.
4. **Manual two-session acceptance gate (Task 5 in `02-p0-plan.md`) not run.** Requires closing one real Claude Code session cleanly so `last-session-review.json` gets written, then opening another so the nag renders. Can only be done by a human.
5. **27 pre-existing `gofmt` struct-alignment drifts in the working tree** (cmd/bench-retrieval, cmd/calibrate-drift, and various internal/ files). Orthogonal to this work. Commit separately if you want them fixed.

---

## 4. Current state — expectations for next session

- `main` is at `f099bc5` with all the fixes merged.
- Binary on disk: `f099bc5` (built locally via `make build` post-merge).
- **Running `heimdall-mcp` subprocesses may still be on older binaries** — Linux pins the executable image at launch. When a Claude Code session starts, it spawns a fresh subprocess that loads the current on-disk binary. So: fresh sessions get the new code; currently-running sessions keep their old binary until they close.

**To observe the fix actually change behavior:**
1. Close the current session cleanly (exit, not kill) so `SessionEnd` fires and `.heimdall_db/hooks/last-session-review.json` is written.
2. Open a new session in this repo. The MCP `initialize` handshake serves the new instruction block AND the SessionStart context injection includes the "Last session review" nag from step 1.
3. Do some actual work (edit, read, correct, ask).
4. Check the log: `heimdall-mcp hooks tail -e mcp.tool_call --since 20m | awk '{print $NF}' | sort | uniq -c`.
5. Run the analyzer: `heimdall-mcp hooks analyze-session --transcript $(ls -t ~/.claude/projects/-home-noname-Code-heimdall-mcp/*.jsonl | head -1)`.

If after step 4 `heimdall_search` / `heimdall_recall` / `heimdall_remember` counts remain zero, the MCP instruction rewrite + nag together aren't sufficient — escalate to the P1 items in `00-problem-and-fixes.md` (UserPromptSubmit nag-after-N-turns is the next lever).

---

## 5. Next-session kickoff prompt

Paste this into a fresh Claude Code session in this repo:

> Read `docs/plans/claude-heimdall-self-use/03-post-ship-handoff.md` end-to-end. It captures the post-ship state of the P0 + P0.5 work. Then run the analyzer on the most-recent transcript of the previous session:
>
> ```
> heimdall-mcp hooks analyze-session --transcript $(ls -t ~/.claude/projects/-home-noname-Code-heimdall-mcp/*.jsonl | sed -n '2p')
> ```
>
> Report what the analyzer shows (Triggers / Followed / Misses). Also check whether `last-session-review.json` was written: `cat .heimdall_db/hooks/last-session-review.json 2>/dev/null || echo "missing"`.
>
> Then confirm whether the new instructions are being served: call `heimdall_search` once for something you'd normally Read, and confirm the call shows up in `hooks.log` as `event=mcp.tool_call`. If you see the log line AND you naturally reached for search, the P0 fix is working. If you didn't reach for search and had to be told, we need to escalate to P1.
>
> Do NOT implement anything from §3 ("Known limitations") without my approval — those are follow-ups, not this session's work.

---

## 6. Useful one-liners

```sh
# Live event tail
heimdall-mcp hooks tail -f -e mcp.tool_call

# Per-tool frequency, last 24h
heimdall-mcp hooks tail -e mcp.tool_call --since 24h | grep -oE 'tool=[a-z_]+' | sort | uniq -c | sort -rn

# Analyze the most recent transcript
heimdall-mcp hooks analyze-session --transcript $(ls -t ~/.claude/projects/-home-noname-Code-heimdall-mcp/*.jsonl | head -1)

# Peek at the next-session nag if present
cat .heimdall_db/hooks/last-session-review.json 2>/dev/null | jq .

# Find live heimdall-mcp processes and their launch times
ps -eo pid,lstart,cmd 2>/dev/null | grep heimdall-mcp | grep -v grep
```

---

## 7. References

- `00-problem-and-fixes.md` — original drift analysis + P0/P1/P2 prioritization.
- `01-p0-design.md` — design doc approved before implementation.
- `02-p0-plan.md` — implementation plan (steps, test names, commit messages) used to drive the subagent workflow.
- PR #79 on `main` — the merged squash commit with all 13 underlying commits rolled up.
