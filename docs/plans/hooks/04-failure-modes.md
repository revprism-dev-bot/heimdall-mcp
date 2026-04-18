# 04 — Failure Modes & Graceful Degradation

**Owner:** failure-modes
**Status:** Draft (peer plans 01/02/03 not yet written — assumptions annotated)
**Golden rule:** A heimdall hook MUST NEVER block Claude Code from proceeding. Degradation must be detectable, not silent.

## 1. Failure taxonomy

The table covers every failure a hook may encounter. `Tier` is A (silent), B (visible note), or C (block — reserved). `Exit` refers to the hook script's exit code: `0` → proceed with stdout; `2` → block with stderr (we never use this for retrieval hooks — see §2); other → Claude Code ignores stdout and proceeds.

| # | Failure | Hook(s) affected | Tier | Exit | User-visible text | Log entry |
|---|---|---|---|---|---|---|
| 1 | Ollama not running | all retrieval hooks | B | 0 | `heimdall: ollama unreachable — retrieval disabled this turn` | `WARN ollama.ping failed endpoint=%s err=%v` |
| 2 | Ollama up, model not pulled | all retrieval hooks | B | 0 | `heimdall: model %q not pulled — run: ollama pull %s` | `WARN model.missing model=%s` |
| 3a | Embed request exceeds soft p95 target (result still emitted) | UserPromptSubmit, SessionStart | — | 0 | (normal stdout, not a failure) | `WARN embed.slow elapsed=%dms target=%dms` |
| 3b | Embed request exceeds hard kill timeout | UserPromptSubmit, SessionStart | A | 0 | (none — silent skip) | `WARN embed.timeout elapsed=%dms hard=%dms` |
| 4 | Embed request errors (5xx, connection reset) | UserPromptSubmit, SessionStart | A | 0 | (none) | `WARN embed.error err=%v` |
| 5 | No `.heimdall_db/` in CWD (first run) | UserPromptSubmit | B | 0 | `heimdall: no index for this project — run: heimdall index .` | `INFO index.missing cwd=%s` |
| 6 | `.heimdall_db/` exists, no model subdir matches configured model | UserPromptSubmit, SessionStart | B | 0 | `heimdall: index models [%v] don't match configured model %s — run: heimdall index . --model=%s` | `WARN model.mismatch indexed=%v configured=%s` |
| 7 | Index is stale (> 5 min default for hook path, configurable) | UserPromptSubmit | A* | 0 | (none — serve stale result, background reindex owned by latency-eng §6) | `INFO index.stale age=%s threshold=%s` |
| 8 | Index is corrupt (SQLite `database disk image is malformed`) | all | B | 0 | `heimdall: index corrupt — run: heimdall index . --rebuild` | `ERROR store.corrupt dbDir=%s err=%v` |
| 9 | DB is locked (concurrent writer) | UserPromptSubmit, PostToolUse | A | 0 | (none) | `INFO store.busy retry_count=%d` |
| 10 | `heimdall` binary not on PATH | all (hook exec fails at shell level) | A | 127 | (none — shell produces `command not found` to stderr; Claude Code ignores) | n/a (nothing runs to log it) |
| 11 | Invalid event JSON on stdin | all | A | 0 | (none) | `WARN hook.bad_stdin event=%s` |
| 12 | Malformed prompt (null bytes, > `max_prompt_bytes`) | UserPromptSubmit | A | 0 | (none — fall through to no-retrieval) | `WARN prompt.rejected reason=%s len=%d` |
| 13 | Transcript too large for `Stop` hook ingestion | Stop / SessionEnd | A | 0 | (none) | `WARN transcript.too_large bytes=%d cap=%d truncated=true` |
| 14 | `~/.claude/settings.json` invalid JSON | install-hooks | C | 1 | `heimdall install-hooks: ~/.claude/settings.json is not valid JSON — fix or delete and retry` | `ERROR install.settings_parse err=%v` (stderr + log) |
| 15 | Per-project toggle disabled | all | A | 0 | (none — this is the configured behavior, not a failure) | (no log — noisy) |
| 16 | Concurrent prompt submissions (previous hook still running) | UserPromptSubmit | A | 0 | (none — skip this turn) | `INFO hook.skip reason=inflight prev_pid=%d` |
| 17 | Clock skew (`last_accessed` in the future → freshness weight < 0) | scoring | A | 0 | (none — clamp) | `WARN clock.skew record_ts=%d now=%d clamped=1.0` |
| 18 | `PostToolUse(Edit)` burst — 50 edits in 2s | PostToolUse | A | 0 | (none — debounce) | `INFO reindex.debounced events=%d window=%s` |
| 19 | `git commit` hook but `.git` not writable | PostToolUse(Bash) | — | — | (deferred to phase 2 — post-commit hook is not in phase-1 scope; server-side indexer already handles git commits) | n/a |
| 20 | Hook script OOM / panic | all | A | >0 | (none — Claude Code swallows) | best-effort: panic handler writes `ERROR panic %v` before exit |
| 21 | Unknown model dimension vs stored dimension mismatch | UserPromptSubmit, SessionStart | B | 0 | `heimdall: embedding dim mismatch (index=%d model=%d) — re-index required` | `ERROR dim.mismatch index_dim=%d model_dim=%d` |
| 22 | Config file (`~/.config/heimdall/hooks.toml`) invalid | all | B | 0 | `heimdall: config invalid — using defaults. Fix: %s` | `WARN config.invalid path=%s err=%v` |
| 23 | Log file path unwritable | all | A | 0 | (none — drop to stderr of the hook, which Claude Code ignores) | (inline) |

\* Stale index (hook path, default 5 min per latency-eng §6): serve current results immediately (Tier A silent), no user-visible note. The hook path itself does **not** trigger the MCP-side `checkAndTriggerReindex` (see A.6) — background freshness is maintained by the `PostToolUse(Edit|Write)` debouncer owned by latency-eng §6. If the user edited files outside Claude Code (external editor, git pull, branch checkout), they get stale results until the next in-session edit or the next explicit `heimdall-mcp index` run.

**Additions discovered during investigation:**
- **#21 embedding dimension mismatch**: the store already records `embedding_dim` metadata (see `internal/mcp/tools.go:318`, `internal/mcp/server.go:603`). Different model of the *same name* (or a different model altogether) can produce vectors of a different dim; cosine search across mismatched dims is undefined behavior. We MUST check this in the hook path, not just at index time.
- **#22 config invalid**: once hooks.toml exists, it becomes a failure surface.
- **#23 log unwritable**: rare (read-only home, full disk) but real; we must not crash on it.

## 2. Per-tier design

### Tier A — degrade silently

**Policy:** exit 0 with empty stdout. Claude Code adds zero characters to context, user notices nothing, log line captured. This is the default for anything that is normally transient (network blips, DB contention, timeouts, debouncing, panics).

**Why exit 0 and not exit-other?** Exit-other causes Claude Code to discard stdout *and* surface a hook failure indicator. Our policy is that a retrieval hook failing is not a user-facing event — it's a silent degradation. Only the install-time command (Tier C, §5) is allowed to exit non-zero.

**Why not exit 2?** Exit 2 blocks and passes stderr to Claude as feedback. We never want to block — retrieval failures must not interrupt the user's prompt.

### Tier B — degrade visibly

**Policy:** exit 0 with a single-line note on stdout prefixed `heimdall: `. The note becomes part of Claude's session context and is visible to the user. Budget: ≤120 chars, one line, never multi-line, never fenced.

Tier B is used when the user needs to *do* something (pull a model, install, re-index) and silently proceeding would leave them puzzled about why heimdall is "not working." Examples: Ollama missing, model missing, first-run index missing, dim mismatch.

**Rate limiting:** same Tier B message suppressed for 5 minutes per project (keyed by `sha256(project_root + failure_code)` in `~/.local/state/heimdall/suppress.db`). Prevents the same "ollama unreachable" banner appearing on every turn while the user's restarting their daemon. Cleared when the failure stops firing.

### Tier C — block with actionable error

**Policy:** exit non-zero, one-line error on stderr. **Only** used for `heimdall install-hooks` (not a runtime hook). Never used for `UserPromptSubmit`/`SessionStart`/`PostToolUse`/`Stop`. Rationale: hooks run in the hot path; blocking them breaks the user. Install runs once, interactively, so blocking is fine.

## 3. Wrong-but-confident prevention (model mismatch reinstated)

This is the trap. heimdall already has the plumbing (`embedding_model` and `embedding_dim` in the store metadata table). The removed `checkModelMismatch` warning needs to come back — but in a harder-to-bypass form and at the *hook* boundary, not just the MCP tool boundary.

**Design:** every hook that hits the store runs `verifyIndexModel(store, currentModel)` immediately after `OpenStore`. The check:

1. Read `store.GetMetadata("embedding_model")` → `indexedModel`.
2. Read `store.GetMetadata("embedding_dim")` → `indexedDim`.
3. Normalize both sides with the existing `NormalizeModelName` (strip `:latest`).
4. If `indexedModel == ""` (pre-metadata index): Tier B — `heimdall: index has no model metadata — rebuild recommended: heimdall index . --rebuild`. Proceed anyway (backward compat) but the note is visible.
5. If `indexedModel != currentModel`: Tier B — `heimdall: index built with %s, current model is %s — results may be noise. Run: heimdall index .`. Do **not** run the search — return the warning as the only stdout content. Rationale: cosine across different embedding spaces returns *plausible-looking* noise, which is the worst possible outcome.

   **Combined behavior with §2 suppression (confirmed with hook-architect):** on UserPromptSubmit, the first occurrence of the mismatch emits the Tier B warning on stdout. The §2 5-minute suppression then kicks in on key `(project, model_mismatch)`, so subsequent turns within the window fall back to Tier A silent (empty stdout, no retrieval). Net effect: "first turn shows the warning, subsequent turns are silent until the user fixes it." On SessionStart the Tier B warning shows once per session (no suppression needed — the event only fires once).
6. If both models match but a test embed returns a vector whose dim differs from `indexedDim`: Tier B failure #21 above. Do not search.

**Why not just use `ResolveUsableModelDB`?** That helper already picks an index whose backing model is pulled in Ollama. But it silently picks *any* matching model — if the user has indexes for `mxbai` and `nomic` and both are pulled, and the *configured* model is `bge-m3` (not pulled), it will pick one of the other two and search against it. That's exactly the wrong-but-confident failure mode. The hook path must honor the configured/requested model and refuse to fuzzy-match.

**Enforcement surface:** this check lives in a new helper `heimdall.VerifyHookIndex(store, requestedModel) error` in `internal/heimdall` and is called from every hook's `main` before any embed. Unit tests enforce: (a) mismatch returns error, (b) missing metadata returns a specific sentinel error distinguishable from mismatch, (c) dim mismatch returns a third sentinel.

**Positive signal:** when the check passes, emit one footer line in Tier B format in stdout *only* at verbose level (config: `hooks.verbose = true`): `heimdall: %d hits from index (%s, %s)`. Off by default — users can opt in to see the signal that heimdall is working.

## 4. Install-time failure modes (`heimdall install-hooks`)

Install is interactive / one-shot, so we can use Tier C (exit non-zero + actionable stderr). All writes are atomic: write to `settings.json.heimdall-tmp`, `fsync`, rename. Never partial-write `~/.claude/settings.json`.

| Situation | Behavior |
|---|---|
| `~/.claude/` missing | Create it (0700). |
| `~/.claude/settings.json` missing | Create with just the heimdall hooks block. |
| `settings.json` exists, not valid JSON | Exit 1. stderr: `settings.json is not valid JSON at line %d — fix or delete and retry`. Do NOT overwrite. |
| `settings.json` exists, valid, no `hooks` key | Add `hooks` key with heimdall entries. |
| `settings.json` exists with existing `hooks` but none are heimdall's | See §5 — compose, do not clobber. |
| `settings.json` exists with previous heimdall hooks from an older version | Detect via `"source": "heimdall"` marker. Replace in place (upgrade). |
| User lacks write permission on `settings.json` | Exit 1. stderr: `cannot write settings.json: permission denied — check ownership`. |
| Disk full | Exit 1. stderr: `cannot write settings.json: no space left on device`. Atomic temp means the existing file is untouched. |
| `heimdall` binary path not resolvable at install time | Exit 1. stderr: `cannot determine heimdall binary path — set HEIMDALL_BIN or reinstall`. We need the absolute path in the hook command so it works from any CWD. |
| `--dry-run` flag | Print proposed diff to stdout, exit 0, touch nothing. |

**Uninstall** (`heimdall uninstall-hooks`) is symmetric: reads settings.json, removes every hook entry marked `"source": "heimdall"`, writes atomically, exit 0. If nothing to remove, exit 0 silently.

## 5. Hook composition strategy (don't clobber the user)

Users will have other hooks — from other MCPs, from their own dotfiles, from the Claude Code docs examples. Our install must be neighborly.

**Marker convention.** Every hook entry heimdall writes carries an explicit `"source": "heimdall"` field and a `"version": "N"` field. (Claude Code ignores unknown fields in hook entries — assumption; `hook-architect` should confirm before implementation.) This makes our entries identifiable for upgrade and uninstall.

**Composition rules.**

1. **Same event, different matcher, different source:** append. Both hooks run. No conflict.
2. **Same event, same matcher, our own older version:** replace in place (upgrade path).
3. **Same event, same matcher, foreign source:** append — Claude Code runs all hooks for a matching event. No clobber.
4. **Same event, no matcher vs our scoped matcher:** append. Claude Code merges.
5. **Foreign hook that appears to do the same thing (e.g. another `UserPromptSubmit` context injector):** append. Non-heimdall hooks are opaque to us; we don't diff them. The user can uninstall one if they conflict in practice.

**No interactive prompt.** Prior design considered prompting on conflict. Rejected: install must be scriptable for CI/dotfiles. Instead: `install-hooks --force` is a no-op (we never conflict); `install-hooks --print-diff` shows what will change.

**Conflict detection for doctor.** `heimdall hooks doctor` (a diagnostic command, not install) enumerates all `UserPromptSubmit` hooks and flags any that are not marked `"source": "heimdall"`, so the user *knows* another tool is also injecting context. This is an observation, not an error.

## 6. Observability

### Log file

**Location:** `${XDG_STATE_HOME:-~/.local/state}/heimdall/hooks.log`. XDG-conformant, per-user, not in the project tree (never commits by accident). Created 0600.

**Format:** single line per event, leading timestamp, level, event, key=value pairs. No JSON — `grep` / `awk`-friendly for an operator. Example:

```
2026-04-14T15:04:33Z WARN ollama.ping endpoint=http://localhost:11434 err="dial tcp: connect refused" hook=UserPromptSubmit project=/home/u/proj
2026-04-14T15:04:33Z INFO hook.degraded tier=B code=ollama_unreachable suppressed=false
```

**Rotation:** size-capped at 10 MB (bumped from 5 MB in HD-5, 2026-04-18). On write, if the file exceeds the cap the hook renames `hooks.log` → `hooks.log.1` (replacing any prior `.1`) and starts fresh. One generation only — we're not building logrotate. Total disk footprint ≤ 20 MB. Rotation runs in the hook's main path (not a background daemon), with the rename as an atomic syscall, so it can't leave a half-state.

**Failure modes of the log itself:** if the log file cannot be opened (permissions, disk full), the hook silently writes nothing. We never abort the hook because logging failed.

### `heimdall hooks tail`

A small subcommand that `tail -F`s the log with optional `--level`, `--event`, `--since`, `--project` filters. One-shot helper — does not daemonize. Handy for "why did my prompt feel weird just now."

### `heimdall hooks doctor`

Diagnostic subcommand that runs every check a hook would run, reports pass/fail for each, and prints the last 20 log lines. Does not modify state. Suggested checks:

1. `heimdall` binary on PATH — yes/no + absolute path.
2. `~/.claude/settings.json` exists + valid JSON + hooks block present.
3. Our hook entries present (by `"source": "heimdall"` marker).
4. Ollama reachable at configured endpoint.
5. Configured model pulled.
6. `.heimdall_db/` present in CWD.
7. Index `embedding_model` matches configured model.
8. Index `embedding_dim` matches current model's test-embed dim.
9. Log file writable.
10. Config file (if present) parseable.
11. Foreign `UserPromptSubmit` hooks (listed, not flagged).

Each check emits one line: `OK|WARN|FAIL description [remediation]`. Exit 0 if all OK/WARN, non-zero if any FAIL. Safe to run in CI.

## 7. Open questions (for human decision)

1. **Dim mismatch: block or degrade?** Proposed: block retrieval (§3 step 6) — return only the warning and no results. Alternative: proceed anyway because *some* noise is better than none. I lean block — industry standard for similarity search is "same space or no search." User to confirm.
2. **Tier B rate-limit window.** Proposed: 5 min per `(project, failure_code)`. Alternative: per-session (reset on `SessionStart`) or per-turn (annoying). 5 min is a gut feel; needs a user call.
3. **Verbose footer default.** Proposed: off by default; on via `hooks.verbose = true`. Alternative: on by default so users *see* that heimdall ran. Trades off screen noise vs. silent-success trust. Recommend off + a first-run tip.
4. **Hook entry extra fields.** The compose strategy depends on Claude Code ignoring unknown fields in hook entries (`source`, `version`). `hook-architect` to confirm from the Claude Code hook-schema docs. If Claude Code rejects unknown fields, we encode the marker in the command string itself (e.g., `heimdall hook prompt-submit --tag=heimdall-v1`).
5. **`Stop` hook transcript cap.** What's the byte cap for session transcripts? A 100k-line conversation is a real tail case. Propose 2 MB, truncate-head (keep the end). `latency-eng` needs to weigh in.
6. **Per-project disable.** Where does the disable live — project-local `.heimdall/hooks.disabled` file, or a key in hooks.toml? `cli-surface` owns this; we just need the presence check to be Tier A silent.
7. **What counts as "stale" for #7.** Current code uses 30 min. Hooks may want tighter (5 min?) because the user is actively editing. `latency-eng` should decide the default; failure-modes is happy with any value the config exposes.

## Addendum A — Contract points for hook-architect (phase 1 hooks)

Resolves the 8 contract points raised against 01-architecture.md. All phase-1 hooks exit 0 always. All user-visible text uses the `heimdall: ` prefix and is ≤120 chars on one line.

### A.1 SessionStart — Ollama down

Exact stdout (single line, becomes the only content of the `## Heimdall context` block):

```
> heimdall: ollama unreachable at <endpoint> — retrieval disabled this session. Start with: ollama serve
```

No retry. `SessionStart` fires once per session, and a cold `ollama serve` can take >10s. Retrying eats the 3s timeout and the user still sees nothing useful. Log: `WARN ollama.ping endpoint=%s err=%v hook=SessionStart`. Suppression: exempt (SessionStart fires once).

### A.2 UserPromptSubmit — no index exists

Exact stdout (single line):

```
> heimdall: no index for this project — run: heimdall-mcp index .
```

Distinguish "no `.heimdall_db/` at all" from "`.heimdall_db/` exists but empty / no model subdir matches pulled models":

- No directory → the text above.
- Directory but no matching model → `> heimdall: indexed models %v but Ollama has %v — run: heimdall-mcp hooks doctor`

Neither variant attempts an auto-index. `autoIndexOnSearch` in `internal/mcp/tools.go:383` spawns a multi-minute background job — great for MCP, wrong for a hook because the *first* hook fire would misleadingly succeed (spawn OK → exit 0 → empty stdout) while the user has no idea an index is building. The Tier B note makes the state visible; the user runs `heimdall-mcp index .` explicitly. Never blocks. Log: `INFO index.missing cwd=%s`. Suppression: 5 min per project.

### A.3 UserPromptSubmit — embedding timeout

**Empty stdout. Deterministic.** No partial flush.

Rationale: partial retrieval means "we ran the embed but gave up mid-scan" which produces a non-deterministic top-N (depends on which chunks were scored before the budget hit). Deterministic empty is easier to test, easier to reason about, and the user's turn proceeds with zero extra latency beyond the budget. The hook wrapper enforces this by running the embed + scan under a single `context.WithTimeout(ctx, budgetMs)` and, on deadline, discarding any accumulated partial results and writing nothing.

Exit 0, stdout empty, log `WARN embed.timeout elapsed=%dms budget=%dms prompt_len=%d`. Suppression: never (timeouts are signal — latency-eng needs the data).

### A.4 PostToolUse — concurrent re-index debouncer

The existing `runIndex` in `internal/mcp/server.go:246` guards against concurrent *MCP-side* invocations via `s.Index.Mu` + `s.Index.Running`, but that lives in the MCP server process. Hook scripts are one-shot processes with no shared memory, so we need a filesystem-level debouncer.

**Scheme: lockfile + coalesce-window + coalesced file list.**

Per-project state dir: `<project>/.heimdall_db/hooks/`. Four files:

| File | Purpose |
|---|---|
| `reindex.lock` | `flock`-based advisory lock. Only the actor holding this lock may spawn the reindex. |
| `reindex.pending` | Newline-delimited set of absolute file paths queued since the last run. Written by any hook; read+truncated by the actor. |
| `reindex.last_run` | Unix timestamp of the last completed reindex. |
| `reindex.inflight.pid` | PID of the running background reindex, or absent. |

**Algorithm per hook fire (in the foreground hook process):**

1. Append `tool_input.file_path` to `reindex.pending` (append is atomic on POSIX for <PIPE_BUF bytes; one path per line fits).
2. `flock(reindex.lock, LOCK_EX|LOCK_NB)`. If already held, exit 0 immediately — the existing holder will pick up our appended path. This is the coalesce.
3. If acquired:
   a. Read `reindex.last_run`. If now - last_run < `coalesce_window_ms` (default **2000 ms**), release the lock and exit 0 — too soon; batch the work into the next window.
   b. Otherwise `fork+setsid` into a detached actor. Parent releases the lock *after* the child is running, exits 0.
4. **Detached actor:** sleep `coalesce_window_ms` (lets late arrivers append), then atomically rename `reindex.pending` → `reindex.pending.<pid>`, read it, write its PID to `reindex.inflight.pid`, run the incremental reindex on the collected path set, update `reindex.last_run`, remove the inflight file, exit. If the actor dies mid-run, the next hook fire will find a stale `inflight.pid` — reap with a `kill(pid, 0)` check.

**Foreground guarantee:** steps 1–3 are all non-blocking file ops + a non-blocking flock. Budget is ≤50 ms for step-1 through step-3 → foreground hook exits in <50 ms. Step 4 happens in the detached child and doesn't count against the hook budget.

**Failure modes for the debouncer itself:**

- `.heimdall_db/hooks/` not writable → log WARN, exit 0 silently. No retrieval degradation because this is a fire-and-forget hook.
- `flock` unsupported on the filesystem (NFS, some WSL configs) → fall back to `O_CREAT|O_EXCL` on a `.lock` file with a TTL stamp. Not perfect but functional.
- Actor panics mid-run → `inflight.pid` orphans. Reaper logic in step 4 handles it.
- `reindex.pending` grows unbounded (user `git checkout` dumps 5000 modified files) → cap at 1000 lines, drop overflow, log `WARN reindex.pending_truncated dropped=%d`.

### A.5 SessionStart — fresh repo, zero indexed data

Confirmed: **do not auto-index**. Exact stdout:

```
> heimdall: no index for this project yet — run: heimdall-mcp index .
```

Same wording as A.2 (consistent guidance regardless of hook point). The reasoning matters: SessionStart has a 3s hard budget. A real indexing run on a medium repo takes minutes and would either (a) blow the budget and be killed mid-write (potentially corrupting the store — see failure #8 in the main table) or (b) fork-and-forget, in which case the hook exits 0 with empty stdout and the user has no clue anything is happening. Tier B visible note is the only safe choice.

Log: `INFO index.missing cwd=%s hook=SessionStart`. Suppression: exempt (once per session).

### A.6 Stale-index interaction with server-side stale check

Hooks **must skip** the stale-check-with-background-reindex path. The MCP tool at `internal/mcp/tools.go:435` already owns that behavior for the in-process server. Double-triggering risks:

1. Hook process forks a reindex at T=0 (via the A.4 debouncer).
2. MCP server receives a search request at T=100ms, stale-check fires, server-side `s.Index.Running` is false (different process!), server also spawns a reindex.
3. Two writers against the same SQLite store → DB lock contention, possibly corruption.

Resolution: the hook-side `heimdall-mcp hook user-prompt` command never calls `checkAndTriggerReindex`. It only runs search. If the index is stale, the user gets current (slightly stale) results with no note — Tier A silent, exactly row #7 in the taxonomy table. The *next* MCP tool call will still trigger the server-side refresh on its normal path. If the user is running hooks without the MCP server (not the common case, but possible), they keep stale data until the next `PostToolUse(Edit|Write)` naturally refreshes the affected files. That's an acceptable corner.

The debouncer lockfile in A.4 also means: if the MCP server happens to be running a reindex when a `PostToolUse` hook fires, the hook's flock acquire will succeed (different lock scope — MCP uses in-memory mutex, hook uses filesystem lock), BUT the hook's actor opens the store in WAL mode and SQLite handles the write-write serialization. Not ideal — recommend adding a cross-process lock. Logged as an open question below.

### A.7 Ollama model not pulled — hook parallel to `ollamaSetupError`

The MCP initialize path calls `ollamaSetupError` (see `internal/mcp/server.go:40`) which returns a multi-line rich error. Hooks cannot do that — stdout goes into Claude's context and multi-line error blocks pollute it. Hook behavior: single-line Tier B note, as per row #2 of the taxonomy table:

```
> heimdall: embedding model %q not pulled — run: ollama pull %s
```

No fallback to another model even if one is pulled. Honors the configured model strictly (see §3 wrong-but-confident prevention — the whole point is to refuse implicit fuzzy-match).

Log: `WARN model.missing requested=%s available=%v`. Suppression: 5 min per project.

`heimdall-mcp hooks doctor` renders the multi-line rich version (it runs interactively, not in the hot path, so multi-line is fine).

### A.8 `Stop` hook — rolling buffer corruption / leftover state

Buffer location: `${XDG_STATE_HOME:-~/.local/state}/heimdall/sessions/<session_id>.log`. Per-user, not in the project tree, survives `rm -rf .heimdall_db` (transcripts are user history, not project data).

**Write protocol (atomic append):**

1. Open with `O_APPEND | O_WRONLY | O_CREAT`, mode 0600.
2. Serialize the turn into a single length-prefixed JSON record: `<4-byte big-endian length><JSON payload>\n`. Length prefix lets the reader detect truncation.
3. Single `write(2)` call. POSIX guarantees appends <PIPE_BUF are atomic; for larger turns, truncation mid-write is detectable by the reader (length prefix mismatch).
4. `fsync` NOT called in the hook path (too slow). We accept that a hard crash mid-write loses the last turn.

**Read protocol (when ingest-session actually runs):**

1. Rename the buffer to `<session_id>.log.ingesting` atomically (rename is always atomic on POSIX). New Stop hooks write to a fresh `<session_id>.log`.
2. Scan records by reading the length prefix, then exactly that many bytes. If a length prefix indicates more bytes than remain in the file → the tail is truncated; log `WARN transcript.truncated session=%s at_offset=%d` and stop at the last intact record.
3. If the JSON in a record fails to parse → log `WARN transcript.bad_record session=%s offset=%d`, skip that record, continue.
4. Feed parsed records to `heimdall_ingest_session`.
5. On successful ingest, unlink `.ingesting`. On failure, rename to `.log.corrupt.<timestamp>` so we don't lose it and the next run won't reprocess it.

**Leftover buffers from old sessions:**

A separate cleanup pass (runs opportunistically on every Stop hook, cheap): `stat` every file under `sessions/`. If `mtime` is older than **24 hours** and the filename matches `*.log` (not `.ingesting`), and the session_id is not the current one → fork the ingest actor on it, same code path. If older than **7 days** and still sitting there → delete. This reaps orphans from crashed sessions.

**Size cap per buffer:** 2 MB. On ingest (not on write — the buffer itself is append-only; the cap is enforced by the reader before handing to `ingest-session`), if the assembled record stream exceeds 2 MB, keep the first 512 KB (session opening, goals, initial context) plus the last 1.5 MB (most recent turns and outcomes) and drop the middle with a single `[... %d bytes truncated ...]` sentinel record. Rationale (per latency-eng): the least informative segment is the middle — intermediate tool calls are already captured by other hooks; the high-signal content lives at the top (goal) and bottom (latest state). Log `WARN transcript.truncated_middle session=%s dropped_bytes=%d kept_bytes=%d`.

**Concurrent writers to the same session_id:** not possible — `session_id` is unique per Claude Code session, and Claude Code serializes hook execution within a session. Cross-session concurrency is handled by distinct filenames. No locking needed.

### Open questions added

- **Cross-process store write lock.** ~~The hook-side reindex and a running MCP server can both try to write to the same sqlite-vec store.~~ **Resolved:** see 03 §3. `latency-eng` committed to a 50ms `busy_timeout` floor on the hook SQLite connection, applied to both read and write paths. Any contention (read or write) fails fast to Tier A silent skip. The hook never holds the lock long enough to contend with indexer writes, so the cross-process lock concern collapses without requiring a separate file. `hook_cache` stays in the per-model `vectors.db`.
- **Exit codes for phase-1 hook commands.** `cli-surface`'s 02 proposes a 10/20/30 degraded/config/dependency taxonomy. Failure-modes position: phase-1 hooks should exit 0 always and route the failure *code* into the log file (§6), not into the exit code. Rationale: Claude Code's behavior on non-zero non-2 codes is "silently ignore stdout and proceed," which is observationally identical to exit 0 + empty stdout, so the richer exit-code taxonomy is user-invisible but adds a test surface and a `--strict` footgun (env-var strict mode hides failures from future-you who forgot they set it). `hooks-doctor` is the right place for structured failure reporting; the hot path should be behaviorally boring. Needs resolution with `cli-surface`.

### Addendum B — Dead-letter queue (cross-references cli-surface 02)

`cli-surface` raised a dead-letter queue for `post-edit` failures in the background actor (e.g., actor woke up, Ollama still down, can't reindex). This overlaps with my A.4 debouncer but is distinct — A.4 handles "work not yet picked up" (the `reindex.pending` file), deadletter handles "work the actor tried and failed." The taxonomy of what goes into the deadletter lives in this doc; the file format lives in 02-cli-surface.md.

**Location:** `<project>/.heimdall_db/hooks/reindex.deadletter.jsonl` (same state dir as the debouncer — keeps all hook state in one place so `uninstall-hooks` has a clean wipe target).

**Scope:** write-path hook failures only. `post-edit` background reindex failures, `stop` ingest failures. Never retrieval failures — `user-prompt` is a fresh attempt every turn, nothing to persist.

**What goes in (one JSON line per failed work item):**

```json
{"ts": 1776210386, "op": "reindex", "path": "/abs/foo.go", "err_code": "ollama_unreachable", "attempts": 3}
```

`err_code` comes from the failure taxonomy table (§1). `attempts` increments on each retry.

**Give-up cap:** `attempts >= 5` → drop on next drain, log `WARN deadletter.abandoned path=%s err_code=%s`. Fixed default for phase 1, not exposed in `hooks.toml`. Rationale: (1) no telemetry yet to justify a configurable value — any knob is guessing; (2) the cap interacts with the coalesce window and retry backoff, and tuning one in isolation invites pathological combinations; (3) phase-1 scope deliberately excludes knobs (no `--strict`, no `HEIMDALL_HOOK_STRICT`, no separate latency exit code) — adding one for this would be inconsistent; (4) escape hatch: `heimdall-mcp index .` reindexes unconditionally, so an abandoned file is not permanently lost. Revisit in phase 2 if `WARN deadletter.abandoned` exceeds ~0.1% of attempts in real telemetry.

**Drain trigger:** next successful actor run, not a user-invoked command. When A.4's detached actor completes an incremental reindex *successfully* (Ollama up, model matches, DB write committed), it reads `reindex.deadletter.jsonl` into memory, retries each entry, rewrites the file with remaining failures. If the deadletter is empty after the pass, unlink the file. Additional drain points (`heimdall-mcp index`, `heimdall-mcp status`) are fine fallbacks but not load-bearing.

**Size cap:** 10 MB or 10 000 lines, whichever first. Truncate-head on overflow (drop oldest), log `WARN deadletter.truncated dropped=%d`. Same policy as §6 log file — no unbounded growth on a user's disk.

**What does NOT belong in the deadletter:**
- Retrieval failures (ephemeral, see above).
- Debounce-swallowed edits (those already succeeded — the path is in `reindex.pending` and will be picked up on the next window).
- Transcript ingest failures — those already have a recovery path (`.log.corrupt.<ts>` rename in A.8) and a separate reaper schedule.
- Install-time failures (Tier C, interactive, user-facing — no queue needed).
- Config parse failures (Tier B, user-visible, resolved by fixing config).

## Coordination asks

- **cli-surface:** confirm exit code taxonomy (`0` proceed, `2` block, other ignored) matches what we assume here. Confirm install flags (`--dry-run`, `--print-diff`, `--force` no-op). Confirm `heimdall hooks doctor` and `heimdall hooks tail` land in the same subtree.
- **hook-architect:** confirm which hook events are retrieval hooks (candidates: `SessionStart`, `UserPromptSubmit`) vs fire-and-forget (`PostToolUse`, `Stop`). The Tier mapping depends on this split. Also confirm Claude Code tolerates unknown fields on hook entries (§5, OQ #4).
- **latency-eng:** per-hook timeout budgets. We propose: `UserPromptSubmit` ≤ 250ms (else Tier A skip), `SessionStart` ≤ 2s (runs once), `PostToolUse` ≤ 50ms handoff to background (debounced), `Stop` ≤ 5s. Needs confirmation.
- **test-rollout:** please include golden tests for *every* row in the failure taxonomy table. Each row should have: (1) a test that triggers the failure (fault injection — `OLLAMA_ENDPOINT=http://127.0.0.1:1` for #1), (2) an assertion on exit code, (3) an assertion on stdout content (empty vs. Tier B line), (4) an assertion on a log-line match. This is the most testable part of the integration — don't let it slip.
