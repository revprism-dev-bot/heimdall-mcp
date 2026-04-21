# Handoff — SessionEnd hook regressed after P0 binary (2026-04-21)

**Status:** P0 + P0.5 code shipped in PR #79 (`f099bc5`) on 2026-04-20, but the observable verification loop is broken. `SessionEnd` has not fired since 2026-04-19T18:46Z — ~24h *before* the new binary was built. Since the binary cut over, **zero** `event=session-end` lines have been written.

This handoff captures the evidence, the two sessions we analyzed, and a concrete first diagnostic for the next session.

---

## 1. Evidence

### 1.1 SessionEnd stopped firing around the P0 cutover

`heimdall-mcp hooks tail --event=session-end --since=168h` timeline (last 10 of 37):

```
2026-04-17T20:26:31Z
2026-04-17T21:49:54Z
2026-04-17T22:16:52Z
2026-04-18T11:03:07Z
2026-04-18T15:22:11Z
2026-04-18T15:25:22Z
2026-04-18T15:41:18Z
2026-04-18T16:12:53Z   reason=clear
2026-04-18T23:26:52Z   reason=prompt_input_exit
2026-04-19T18:46:50Z   reason=prompt_input_exit   ← last one ever
```

- **Binary built:** `heimdall-mcp` at `/home/noname/Code/heimdall-mcp/heimdall-mcp`, mtime `2026-04-20 21:30`.
- **Last SessionEnd:** `2026-04-19T18:46Z` — **~27 h before the new binary existed.**
- Between the binary build (20:30 UTC 04-20) and now (04-21 evening), multiple sessions have started and stopped (reboot at 22:46Z 04-20, session `13818937` closed pre-reboot, session `fa9b59da` killed by reboot, and at least one further session the user opened+closed today).
- Zero of those produced a `session-end` log line.

### 1.2 Other hooks still work on the new binary

- `event=stop` fires normally: `2026-04-21T18:51:36Z INFO event=stop bytes=1443 msg=buffer_appended session=aa7c554c`.
- `install-hooks` template is intact — `.claude/settings.json` carries all 6 hook entries:
  ```
  PostToolUse:       heimdall-mcp hook post-edit     --source=heimdall --version=1
  PreToolUse:        heimdall-mcp hook pre-tool-use  --source=heimdall --version=1
  SessionEnd:        heimdall-mcp hook session-end   --source=heimdall --version=1
  SessionStart:      heimdall-mcp hook session-start --source=heimdall --version=1
  Stop:              heimdall-mcp hook stop          --source=heimdall --version=1
  UserPromptSubmit:  heimdall-mcp hook user-prompt   --source=heimdall --version=1
  ```
- `SessionStart` context injection is working (we saw the `## Heimdall context` block at the top of every turn this session).

### 1.3 Side effects of SessionEnd never firing

- `.heimdall_db/hooks/last-session-review.json` — **missing** (never written). So the "Last session review" nag block in `formatSessionStartBlock` never renders, regardless of whether the previous session had misses.
- **Three orphaned rolling buffers** at `.heimdall_db/hooks/sessions/`:
  | File | Size | Last mtime | Why orphaned |
  |---|---|---|---|
  | `fa9b59da-….jsonl` | 80597 B | 2026-04-20 20:52 | Killed by reboot (uptime says boot at 22:46 04-20) |
  | `13818937-….jsonl` |   374 B | 2026-04-20 20:57 | Also killed by reboot (transcript went to 21:34 but buffer froze at 20:57 when Stop fired last) |
  | `aa7c554c-….jsonl` |  3392 B | 2026-04-20 23:01 | Current session; normal — it's live |
- The orphaned buffers will accumulate forever because nothing GCs them on startup. Small followup: boot-time sweep in `HookSessionStart` or a `hooks gc` admin command.

---

## 2. Two prior sessions analyzed (`heimdall-mcp hooks analyze-session`)

Run against the previous session transcripts for P0 observability signal:

| Session | Start | Turns | Heimdall MCP calls | Analyzer verdict |
|---|---|---|---|---|
| `fa9b59da-d8e1-49b7-8c6d-4f44a0f2bd9f` | Apr 20 20:56 | 248 | 5 (3 search, 2 recall, **0 remember / 0 index_text**) | 3 triggers, **0 followed** — 2 missed `user_correction → heimdall_remember` (turns 388, 878) + 1 missed `task_start_recall → heimdall_recall` (turn 5) |
| `13818937-d32c-41a2-b08b-57abf90f74a7` | Apr 20 21:34 | 8 | **0** | 1 trigger `task_start_recall`, 0 followed |

Interpretation: the analyzer is *correctly* flagging under-use even post-P0. But we cannot tell whether the new MCP instruction block is helping at all, because the nag-loop is broken (§1.1) and we can't do a clean before/after.

---

## 3. Hypotheses — ordered by likelihood

1. **Running Claude Code instances are still using pre-P0 hook subprocess launch state** — e.g. `settings.json` was amended but the running harness only reloads on restart. Unlikely alone because the `Stop` hook fires on the same path.
2. **The `session-end` subcommand on the new binary exits non-zero or panics before writing a log line.** This would be invisible to the harness. The `hook session-end` path was touched by P0 (added `extractTranscriptSummary` + `writeLastSessionReview` + review-record persistence). A nil-deref / missing-file edge case there would match the symptom exactly.
3. **Claude Code is not dispatching SessionEnd** for the close types that have occurred since 04-19 (reboot-kill, external close, prompt-input-exit after the P0 build). Less likely — the hook *did* fire for `prompt_input_exit` on 04-19T18:46, before the P0 binary.
4. **`PATH` resolution of `heimdall-mcp` differs for SessionEnd vs other hooks.** Very unlikely — same command shape, same PATH, and Stop/SessionStart from the same PATH resolution still work.

H2 is cheap to test and the most likely culprit. See §4 Task 1.

---

## 4. Next-session action list

### Task 1 — Isolate: does the binary's `session-end` path itself succeed? (10 min)

Run the exact command the harness runs, manually, against an orphaned buffer:

```sh
cd /home/noname/Code/heimdall-mcp

# Simulate what Claude Code would send on SessionEnd:
echo '{"session_id":"13818937-d32c-41a2-b08b-57abf90f74a7","transcript_path":"/home/noname/.claude/projects/-home-noname-Code-heimdall-mcp/13818937-d32c-41a2-b08b-57abf90f74a7.jsonl","cwd":"/home/noname/Code/heimdall-mcp","reason":"prompt_input_exit"}' \
  | ./heimdall-mcp hook session-end --source=heimdall --version=1
echo "exit=$?"

# Then check:
./heimdall-mcp hooks tail --event=session-end --since=5m
ls -la .heimdall_db/hooks/last-session-review.json
ls -la .heimdall_db/hooks/sessions/
```

**Expected if H2 is right:** non-zero exit or an error on stderr / panic trace, no `event=session-end` log line, no `last-session-review.json`, buffer not cleaned up.
**Expected if H2 is wrong:** exit 0, log line appears, `last-session-review.json` written if suppression rule (`candidates<=2 AND writes>=1 AND len(misses)==0`) fails, orphan buffer removed.

### Task 2 — If Task 1 shows the binary is fine, check hook invocation

```sh
# Newest Claude Code transcript lines that might tell us if SessionEnd was attempted
tail -200 /home/noname/.claude/projects/-home-noname-Code-heimdall-mcp/aa7c554c-*.jsonl | grep -i sessionend

# Claude Code log (if any) for hook dispatch attempts — location varies
ls ~/.claude/logs/ 2>/dev/null
```

### Task 3 — After root-cause fix, clean up orphan buffers

Either delete manually, or (better) add a one-shot sweep to `HookSessionStart`:
- If `.heimdall_db/hooks/sessions/<id>.jsonl` exists and no process has that session open, move to `.heimdall_db/hooks/sessions/_orphaned/` and log `event=session-gc`.
- This becomes the first self-healing path for reboot-kill.

### Task 4 — Only then: actually verify P0 observable fix

With SessionEnd + nag loop restored, follow the §4 protocol in `03-post-ship-handoff.md`: close cleanly, open fresh, do work, re-run `analyze-session`. Compare miss counts against the two baseline sessions in §2 above.

---

## 5. Working-tree state (informational — do not let this block root-cause work)

Branch: `main`, at `423a9ae` (post-handoff doc, post-P0).

29 uncommitted modified files (pre-existing gofmt drift called out in `03-post-ship-handoff.md` §3 item 5) plus the untracked `docs/plans/2026-04-19-legacy-db-fixes/` directory (planning docs from an earlier cycle). None of these touch the SessionEnd path. If they're in the way, `git stash -u` them; don't commit them in the same PR as the SessionEnd fix.

---

## 6. Useful commands

```sh
# Timeline of session-end events
./heimdall-mcp hooks tail --event=session-end --since=168h

# Stop events (still working — useful control)
./heimdall-mcp hooks tail --event=stop --since=2h

# MCP tool calls per tool, last 24h (will be empty until P0 nag loop works)
./heimdall-mcp hooks tail --event=mcp.tool_call --since=24h | grep -oE 'tool=[a-z_]+' | sort | uniq -c | sort -rn

# Re-run analyzer on the two baseline sessions
./heimdall-mcp hooks analyze-session --transcript /home/noname/.claude/projects/-home-noname-Code-heimdall-mcp/fa9b59da-d8e1-49b7-8c6d-4f44a0f2bd9f.jsonl
./heimdall-mcp hooks analyze-session --transcript /home/noname/.claude/projects/-home-noname-Code-heimdall-mcp/13818937-d32c-41a2-b08b-57abf90f74a7.jsonl

# Check live MCP subprocesses (two were running this session, both started post-reboot)
ps -eo pid,lstart,cmd | grep heimdall-mcp | grep -v grep
```

---

## 7. Kickoff prompt for next session

> Read `docs/plans/claude-heimdall-self-use/04-session-end-regression-handoff.md` end-to-end. The headline: `event=session-end` has not fired since 2026-04-19T18:46Z — before the P0 binary (`f099bc5`, built 2026-04-20 21:30) existed. So the whole nag loop (§4 of `03-post-ship-handoff.md`) is silently broken. Start with Task 1 in §4 to isolate whether the binary's `session-end` path itself is working. Do not implement anything speculative; we need the root cause first. Once isolated, propose the fix plan before touching code.

---

## 8. Cross-refs

- `01-p0-design.md` — original P0 design (introduced the SessionEnd review record).
- `02-p0-plan.md` — implementation plan; Task 5 ("manual two-session acceptance gate") is the one blocked by this regression.
- `03-post-ship-handoff.md` — previous handoff; §3 item 4 explicitly flagged Task 5 as "not run."
- `internal/cli/hook_stop.go` — `HookSessionEnd`, `extractTranscriptSummary`, `writeLastSessionReview`. Most likely regression site.
- `internal/cli/missed_calls.go` — `AnalyzeTranscript`, populates `misses[]` on the review record (also invoked by the SessionEnd path).
