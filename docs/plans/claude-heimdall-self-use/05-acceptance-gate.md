# Acceptance gate — SessionEnd timeout fix (PR #81, merged 2026-04-21)

**One paragraph for future-you:** PR #81 raised the SessionEnd hook timeout to 30 s and reordered `HookSessionEnd` to log `msg=session_ended` before the slow ingest. The binary at `/home/noname/Code/heimdall-mcp/heimdall-mcp` (symlinked on PATH) already has the fix. `.claude/settings.json` still has the **pre-fix shape** (no `timeout: 30`) — it auto-upgrades on the next SessionStart via `autoUpgradeHooks`. So this session's `/exit` is the *first* real end-to-end test, and the *second* `/exit` is the one that exercises the timeout override.

---

## Immediately after `/exit` from this session

Open a new Claude Code session **in this same repo** (`/home/noname/Code/heimdall-mcp`) and run, in order:

```sh
# 1. Did this session's /exit fire SessionEnd? (THIS is the acceptance gate.)
#    Session ID of the session you just exited — look it up via:
ls -t ~/.claude/projects/-home-noname-Code-heimdall-mcp/*.jsonl | head -2
#    The second-newest is "the one you just exited" (newest = current session).
#    Grab its UUID from the filename, then:
./heimdall-mcp hooks tail --event=session-end --since=30m | grep <that-uuid>
```

Expected: one or two lines. At minimum `INFO event=session-end msg=session_ended reason=prompt_input_exit session=<uuid>`. Ideally also `msg=ingest_ok memories=N`. **If you see `session_ended` but not `ingest_ok`, that's still a pass — it means the early-log reorder saved us while the ingest was still slow under the unchanged 1500 ms budget.**

```sh
# 2. Did autoUpgradeHooks rewrite settings.json on the new session's start?
jq '.hooks.SessionEnd[0]' .claude/settings.json
```

Expected: `x-heimdall.heimdall_version` = `"wave2-phase4"` and `hooks[0].timeout` = `30`. If not, autoUpgradeHooks didn't run — check `hooks tail --event=auto-upgrade --since=5m`.

```sh
# 3. Did sweepOrphanBuffers move the reboot-killed buffers?
ls -la .heimdall_db/hooks/sessions/
ls -la .heimdall_db/hooks/sessions/_orphaned/ 2>&1
./heimdall-mcp hooks tail --event=session-gc --since=5m
```

Expected: the stale buffers from handoff §1.3 (`fa9b59da`, `13818937`, possibly `aa7c554c`) now live under `_orphaned/`; a `session-gc msg=orphan_buffers_moved` line in the log.

```sh
# 4. Re-run the analyzer on the two baseline sessions from 04-handoff §2.
#    Miss counts should match (3 triggers/0 followed, 1 trigger/0 followed) —
#    analyzer logic didn't change in PR #81, this is just a sanity probe.
./heimdall-mcp hooks analyze-session --transcript ~/.claude/projects/-home-noname-Code-heimdall-mcp/fa9b59da-d8e1-49b7-8c6d-4f44a0f2bd9f.jsonl
./heimdall-mcp hooks analyze-session --transcript ~/.claude/projects/-home-noname-Code-heimdall-mcp/13818937-d32c-41a2-b08b-57abf90f74a7.jsonl
```

## After one more `/exit` round-trip

Once steps 1–3 are green, do *one more* `/exit` + new session, this time on a session that did real work (some `heimdall_search` / `heimdall_remember` calls, at least one user correction). Then:

```sh
# 5. Full ingest path with the 30 s budget in play.
#    The session you just exited should have both log lines AND a
#    last-session-review.json file.
./heimdall-mcp hooks tail --event=session-end --since=10m | tail -5
cat .heimdall_db/hooks/last-session-review.json
```

Expected: `msg=session_ended` AND `msg=ingest_ok memories=N` for that session, and a review record on disk. This is the full P0 nag loop working end-to-end — which was the original goal behind the whole `claude-heimdall-self-use` plan.

## If any step fails

- **Step 1 fails (no `session_ended` line)**: the binary wasn't what we think. Compare `sha256sum heimdall-mcp` against the one from the merge commit (`2c01e9f`). If they differ, rebuild with `go build -o heimdall-mcp ./cmd/heimdall-mcp`.
- **Step 2 fails (`heimdall_version` still `wave2-phase3`)**: `autoUpgradeHooks` panicked silently or the scope resolution is wrong. Check `hooks tail --event=auto-upgrade --since=10m` for a `write_failed` line. Inspect `internal/cli/install.go` `autoUpgradeHooks` for the scope it picked (`project` vs `user`). Fallback: run `./heimdall-mcp install-hooks --scope=project` manually.
- **Step 3 fails (buffers didn't move)**: the sweep's grace period is 48 h. If the orphans are newer than that (unlikely given the 04-20 reboot), age one manually: `touch -d '3 days ago' .heimdall_db/hooks/sessions/<uuid>.jsonl` and trigger another SessionStart.

## Files touched in PR #81

- `internal/cli/install.go` — `phase1aHook.timeoutSeconds`, `buildHookEntry` emits `timeout`, `autoUpgradeHooks` refreshes stale entries, version bump
- `internal/cli/hook_stop.go` — `HookSessionEnd` log reorder, `sweepOrphanBuffers` helper
- `internal/cli/hook.go` — calls `sweepOrphanBuffers` in `HookSessionStart`
- `CHANGELOG.md` — Fixed-section entry

## Cross-refs

- `04-session-end-regression-handoff.md` — the diagnosis that led to PR #81 (keep as historical record)
- `03-post-ship-handoff.md` §4 — the original P0 acceptance protocol this is gated by
- `01-p0-design.md` / `02-p0-plan.md` — the design the fix is unblocking
