# 03 — Latency Budget & Caching for Hooks Integration

**Owner:** latency-eng
**Status:** draft for consolidation
**Scope:** `UserPromptSubmit` (hot), `SessionStart` (warm-up), `PostToolUse(Edit|Write)` (background)

The UX constraint: `UserPromptSubmit` fires on every turn and its stdout becomes
session context before the model sees the prompt. Wall-clock time on that hook
is user-visible latency *added to every message*. This plan bounds it.

## 1. Per-turn cost model

### Measured components

Everything below measured on the author's machine against the real
`payments-analyzer` index (10,544 chunks, nomic-embed-text, 59 MB SQLite WAL).
Heimdall built at HEAD, Ollama 0.x, Linux 6.17, SSD.

| Component | Measurement | Notes |
|---|---|---|
| Go binary cold start (`heimdall-mcp help`) | ~5ms | Static linked, negligible |
| Go binary + registry read (`projects` cold) | 600ms | First invocation, disk cache cold |
| Go binary + registry read (`projects` warm) | 40–50ms | Repeated invocations |
| Ollama `/api/embed` nomic-embed-text cold | 350ms | Model swap-in |
| Ollama `/api/embed` nomic-embed-text warm | 40ms | Model resident |
| Ollama `/api/embed` bge-m3 cold | 3,590ms | 568 MB model, slow swap-in |
| Ollama `/api/embed` bge-m3 warm | 230–290ms | Resident but heavier |
| `heimdall-mcp search` end-to-end (nomic, warm, 10k chunks) | 180–380ms | Includes binary start + Ollama Ping + embed + scan + format |
| `heimdall-mcp search` end-to-end cold (fresh query) | 220ms | Ollama and FS cache already warm from prior use |

### Decomposed estimate — `UserPromptSubmit` on `heimdall-mcp search`

For nomic-embed-text on a warm box. Components are additive because the
current CLI path is serial.

| Phase | Warm | Cold (post-idle) | Notes |
|---|---|---|---|
| Hook spawn + stdin JSON parse | 2ms | 2ms | Claude Code `command` hook overhead |
| Go binary start + SQLite open (WAL cache hot) | 15ms | 80ms | WAL header + metadata reads |
| Ollama Ping (2s timeout, 1 req) | 3ms | 3ms | Cheap, keep-alive |
| Embed call | 40ms | 350ms | Dominant cold-path term |
| Store `SearchFiltered` 10k chunks (full-scan cosine) | 25ms | 40ms | O(n) in-process, Go slice math; no disk I/O after load |
| `FormatContextBlocks` top-5 | <1ms | <1ms | String build |
| Stdout write to hook pipe | <1ms | <1ms | Tens of KB max |
| **Total observed** | **~180ms** | **~380ms** | Matches `time` results above |

### Extrapolation to other index sizes

Store cost scales with chunk count only (embed cost is size-independent). The
full-scan is `CosineSimilarity` over every row — a dot product over ~768-dim
float32 vectors, decoded lazily from a BLOB column per row. Row decode + math
is the dominant term, not SQL. Linear scaling.

| Size | Chunks | Full-scan cost (extrapolated) | Total (warm, nomic) |
|---|---|---|---|
| Small | 1,000 | ~3ms | ~60ms |
| Medium | 10,000 | ~25ms (measured) | ~180ms (measured) |
| Large | 100,000 | ~250ms (est.) | ~450ms (est.) |
| XL | 500,000 | ~1.2s (est.) | ~1.4s (est.) |

Estimates for 100k+ are linear extrapolations and should be labeled as such —
GC pressure and L3 cache effects at that size may push them higher. We have no
index that large in the wild yet.

### bge-m3 penalty

If the user's index was built with bge-m3, add +200ms warm / +3.2s cold to
every row above. bge-m3 is the "good" embedder but it blows the budget on
cold-Ollama paths. See §5 for mitigation.

**bge-m3 SessionStart risk:** a 3.6s cold embed is already over the 3s
SessionStart hard kill. Users on bge-m3 hitting a cold Ollama on session open
will fall through to the visible-banner degradation path (see §7 "To
failure-modes"). That's the correct outcome — bge-m3 is opt-in, users who
chose it accept the slow-start trade, and the banner makes the degradation
legible rather than silent.

## 2. Budget

### Target: **≤250ms p95** for `UserPromptSubmit`, **≤50ms p50 cached**

Rationale:
- Below ~200ms, added latency feels like normal typing delay. Users don't
  attribute it to the hook.
- Above ~400ms, the "type Enter, brief pause, response starts streaming" cycle
  acquires a new, noticeable gap before streaming. Bad.
- 250ms p95 gives headroom for `fork/exec` jitter, syscalls, and the
  occasional Ollama slow path.
- p50 cached target exists because most turns in a session target the same
  working set of files — the cache hit rate should be high.

### Budget table

| Hook | Wall target | Enforcement | If exceeded |
|---|---|---|---|
| `UserPromptSubmit` | 250ms p95, 500ms hard timeout | `--timeout=500ms` flag; kill + empty stdout | No context injected, session unaffected |
| `SessionStart` | 2s p95, 3s hard timeout | Same | Visible one-line banner on Ollama-down (see §7), else empty |
| `PostToolUse(Edit\|Write)` | Fire-and-forget, no wall budget | Background process + debounce | N/A — user never waits |
| `Stop` / `SessionEnd` | 5s p95 | Hard timeout | Transcript ingest deferred to next session start |

> **Not a hook in phase 1:** git-commit indexing is already covered by the
> existing indexer auto-path (`internal/mcp/server.go` runIndex → stale-check
> at the search path). A dedicated `PostToolUse(Bash(git commit *))` hook
> would race the existing path on the same rows. Deferred to phase 2 iff we
> ever remove the auto-path. Confirmed with hook-architect.

**Hot path ≤250ms requires caching and/or a daemon to stay there on large
indexes with bge-m3.** See §3 and §5.

## 3. Caching design (minimum viable)

### Key and semantics

```
cache_key = sha256(
  normalize(prompt)           || "\x00" ||
  index_version               || "\x00" ||
  sub_project                 || "\x00" ||
  source_type                 || "\x00" ||
  canonical_json(metadata_filter)
)[:16]

index_version = store_metadata["index_version"]  // monotonic int, bumped on Upsert/Remove
```

- `normalize(prompt)` — lowercase, collapse whitespace, strip trailing punctuation.
  Zero cost. Dedupes "fix the bug" / "Fix the bug " / "fix  the bug.".
- `index_version` — already have `SetMetadata`/`GetMetadata`. Add one integer
  key bumped once per successful `Upsert`/`RemoveByFile` transaction. Read is
  a single indexed row — sub-millisecond.
- `sub_project`, `source_type`, `metadata_filter` — folded into the key so
  different filter scopes don't collide on the same prompt (OQ#6, closed with
  hook-architect). `canonical_json` = sorted keys, no whitespace, deterministic
  across Go runs.
- **Model scoping is implicit, not in the key.** The cache table lives *inside*
  `<dbdir>/<model>/vectors.db`, so every cache row is already scoped to one
  embedding space by construction. Swapping models means opening a different
  DB file, which means a different (and independent) cache. Zero cross-model
  contamination risk.
- The key implicitly invalidates on any index write. No separate invalidation
  plumbing needed.

### Storage: SQLite table in the same DB

```sql
CREATE TABLE IF NOT EXISTS hook_cache (
  key        TEXT PRIMARY KEY,
  stdout     BLOB NOT NULL,         -- pre-formatted markdown, ready to write
  created_at INTEGER NOT NULL,
  hit_count  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_hook_cache_created ON hook_cache(created_at);
```

Why in-DB, not tmpfs or in-memory:

- The store is already open on every hook invocation — zero extra file handles.
- Survives reboots. A user who reopens the repo tomorrow gets yesterday's cache
  for unchanged files.
- Trivially bounded via `DELETE WHERE key NOT IN (SELECT key FROM hook_cache ORDER BY created_at DESC LIMIT N)`.
- No extra process, no lock file, no socket. Smallest viable surface.

### TTL and size bound

- **TTL:** 24 hours wall-clock. Belt-and-braces on top of `index_version`
  invalidation — covers freshness-weighted score drift from `last_accessed`
  updates (which happen at read time and don't bump `index_version`).
- **Max rows:** 1,000. Evict oldest on insert when count exceeds cap. Cheap
  LRU-by-creation-time; we don't need true LRU.
- **Max row size:** 32 KB stdout. Refuse to cache oversize (rare — top-5
  snippets cap out well under this).

### Read path

Ordered pipeline (confirmed with hook-architect, aligned with failure-modes §3
`VerifyHookIndex`):

1. Hook binary opens store.
2. **`VerifyHookIndex(store, cfg.Model)`** — if the store's stamped
   `embedding_model` doesn't match the caller's `cfg.Model`, bail Tier B
   (warn, skip, exit 0). **No cache read on mismatch** — a warmed stdout from
   the previous embedding space is worse than a slow correct answer. The
   cache miss cascade after a model swap is the correct outcome.
3. Length guard (<8 chars → exit 0, no stdout). Pre-cache, pre-embed.
4. Hash prompt + filters, read `index_version`, compose key.
5. `SELECT stdout FROM hook_cache WHERE key = ? AND created_at > ?`.
6. If hit: write stdout, flush, exit. Defer `UPDATE hit_count` and
   `store.UpdateLastAccessed(chunk_ids)` to the post-stdout best-effort phase
   (same pattern as the cache INSERT write — see "Write-path contention rule"
   below). Target: ≤20ms warm for the user-visible path.
7. If miss: embed → search → format → write stdout → flush → exit. Cache
   INSERT and `last_accessed` refresh both happen post-stdout, best-effort.

Step 6's `last_accessed` refresh is load-bearing: otherwise the freshness
weight (`store.go:freshnessWeight`, linear decay over 90 days) penalizes
things the user is actively using but which happen to hit the cache instead
of the scan. Because the refresh is a write on `entries` (not `hook_cache`),
it contends with the MCP-server reindex writer directly — so we treat it
identically to the cache INSERT: post-stdout, `busy_timeout=50`, log and
drop on contention. Losing a `last_accessed` refresh under contention is
harmless: freshness decay is daily-scale, and the next cache miss (or the
next un-contended hit) will refresh. Confirmed with hook-architect — the
correct interpretation of their "batched async writer" proposal in a
short-lived-process world is "defer to post-stdout fire-and-forget", not a
long-lived drain goroutine.

No background refresh, no stampede protection at v1 — a second concurrent
miss on the same key just redoes the work. Acceptable for hook traffic patterns
(at most one hook per turn per session).

### Write-path contention rule (WAL lock with MCP server)

The `hook_cache` table shares `vectors.db` with the vector `entries` table,
which means hook cache writes contend with MCP-server reindex transactions
under SQLite WAL. A long `Upsert` (first-time index, bulk re-embed) can hold
the write lock for hundreds of milliseconds to several seconds. Without
mitigation, a hot-path cache-write that lands mid-reindex blocks behind the
store-wide `busy_timeout=5000`, pushing p95 well past the 500ms hard kill.

**Rule: cache writes are best-effort and happen *after* stdout is flushed.**
The user-visible path is:

```
embed → search → format → write stdout → flush → exit(0)
                                  ↓
                           (detached, best-effort, PRAGMA busy_timeout=50)
                           INSERT OR REPLACE hook_cache  (miss path)
                           UPDATE hit_count              (hit path)
                           UpdateLastAccessed(chunk_ids) (both paths)
                           on block: log cache.write.contended, give up
```

- Stdout is flushed *before* the cache insert is attempted, so the user
  never waits on WAL contention.
- **"Detached" here means "same process, post-flush, pre-exit" — NOT
  `fork()+setsid`.** The UserPromptSubmit binary keeps executing after
  `os.Stdout.Sync()` and completes the write attempt synchronously before
  returning from `main()`. Claude Code's harness already consumed stdout
  and moved on the moment flush returned, so wall-clock from the harness's
  perspective is the flush timestamp, not the exit timestamp. This is a
  different pattern from the PostToolUse debouncer (04 §A.4), which *does*
  `fork()+setsid` — that hook needs a genuinely detached background
  process because the reindex work can outlive the foreground script by
  seconds. Do not `fork()` the UserPromptSubmit path: forking would
  invalidate the SQLite handle, and the post-stdout phase is short enough
  (<50ms p99) that a same-process continuation is trivially correct.
- A tight local `busy_timeout=50` (not the store-wide 5000) bounds the
  worst-case wait. If the write can't land in 50ms, skip it — the next
  turn gets a miss and re-populates.
- On the happy path (no MCP reindex in flight) the insert lands in <2ms;
  contention cost is invisible.
- `log cache.write.contended` feeds the hooks-doctor diagnostic stream so
  operators can see if contention is chronic.

### Invalidation hooks

- `PostToolUse(Edit|Write)` → re-index path → store bumps `index_version` →
  every subsequent cache read misses automatically. No explicit flush needed.
- Explicit `heimdall hooks cache-clear` command for emergencies.

## 4. Skip heuristic

### Decision: **always-on with caching + cheap length guard**

Rationale from evidence:

- The cost of running retrieval is 180ms warm, and the cache drives repeat
  costs to ~20ms. That's already in budget for most cases.
- The `prompt`-hook option (ask a cheap local model "should we retrieve?") is
  strictly worse than caching. One cheap-LLM call is 100–500ms on the same
  Ollama instance. It adds to the worst-case path and saves nothing on cache
  hits. Reject.
- The length/entropy heuristic has near-zero false-positive cost but catches
  few cases in practice. Most prompts are long enough to look "code-shaped"
  to a naive classifier. Keep it only as a pre-cache early exit.

Guard (runs *before* embed, *before* cache read — cheap):

```go
if len(strings.TrimSpace(prompt)) < 8 { return "" } // no retrieval
```

Eight characters catches "hi", "ok", "yes", "thx", "what?", trivial acks.
Anything longer goes through the normal cache → embed → search path.

Optional second gate (off by default, enable via config):

```go
if promptLooksLikeChitchat(prompt) { return "" }
```

Implemented as a tiny stopword-ratio check. Not required for v1.

Explicitly *not* doing:

- `prompt` hook type — too slow on its own latency path.
- Full classifier — premature.

## 5. Cold-start vs daemon

### Decision: **accept cold-start for v1; design a daemon upgrade path for v2**

Hot-path numbers (warm) at 180ms already fit the 250ms budget for nomic +
medium indexes. Cold-path (600ms registry + 350ms Ollama + 40ms scan) blows
it. The two knobs:

**Option A — accept cold-start.** Costs ~600ms on the first turn after idle.
Subsequent turns are cached or warm. We document "first turn after idle may
be slow."

**Option B — long-running daemon over unix socket.**
`heimdall-mcp daemon` listens on `$XDG_RUNTIME_DIR/heimdall/hook.sock`,
holds the SQLite connection and a keep-alive Ollama client. Hook scripts
become thin socket clients, 1–2ms overhead. Eliminates cold-start entirely.

**v1 choice: A, with one mitigation.** Mitigation: after a cache miss, `heimdall`
spawns a detached `ollama-keepalive` goroutine that issues a no-op embed every
`KEEPALIVE_INTERVAL` (default 4 minutes) *only if the last hook fired within
the last 15 minutes.* That keeps the model resident through an active session
without paying cold-swap on every turn. ~40ms keepalive cost, invisible.

**v2 upgrade:** Ship the daemon when we have concrete evidence that v1's tail
latency hurts. Daemon is additive — the same CLI commands work against either
path, chosen by env var `HEIMDALL_DAEMON_SOCKET`. No hook script changes
required later.

### Ollama keepalive alternative

Ollama itself supports `keep_alive` on `/api/generate` and `/api/embed`.
Set `keep_alive: "10m"` in the embed request body. That removes the need for
a spawned keepalive process entirely. **Use this.** It's one JSON field.

```json
{"model":"nomic-embed-text","input":"...","keep_alive":"10m"}
```

Trivially verified against the measurements above — warm embed is 40ms; with
`keep_alive: 10m`, idle returns should stay warm throughout a session.

## 6. Debouncing re-index hooks

`PostToolUse(Edit|Write)` fires per-file, and bursty edits (agent patches, mass
rename, paste-from-clipboard) can trigger dozens of hooks in seconds. Re-
embedding 50 chunks takes 50 × 40ms = 2s minimum, plus transaction overhead.
We must coalesce.

### Strategy: lock file + deferred coalesce window

```
$DBDIR/.reindex.lock        # flock-held while a reindex is running
$DBDIR/.reindex.pending     # list of file paths appended by each hook
```

Hook script pseudocode:

```
append $FILE to .reindex.pending
if flock(.reindex.lock, non-blocking) succeeds:
  sleep 1s                           # coalesce window
  paths := drain .reindex.pending
  heimdall-mcp hook post-edit --batch $paths
  release lock
else:
  exit 0                             # another hook already queued it
```

- Coalesce window: 1s default, `--debounce-window` flag to override.
- Lock granularity: per database (per project / per model dir).
- Batch limit: drop oldest if `.reindex.pending` exceeds 500 lines to bound
  worst-case memory.
- Exit 0 either way — this hook is fire-and-forget, never blocks the turn.

### Hooks that don't need debouncing

- `SessionEnd` / `Stop` ingest-session — fires once per session.

Only file-edit hooks need coalescing. (Git-commit indexing is not a hook in
phase 1 — see the note under §2; the indexer's existing auto-path covers it.)

**Cross-reference:** this debouncer is the sole freshness path available to
the hook subsystem in phase 1. The read path (§3) intentionally has no
reindex side-effect — see 04 §A.6 for the cross-process write-lock constraint
that makes "hook triggers reindex" unsafe until phase 2. External-editor or
`git pull` drift is covered by the next in-session edit tripping this
debouncer, or by explicit `heimdall-mcp index`.

## 7. Coordination asks

(Will send via SendMessage immediately after this doc lands.)

### To `cli-surface`

Flags the CLI needs to expose on the `heimdall hook` subcommand tree:

- `--cache-key <hex>` — override computed key (test-only)
- `--cache-ttl <duration>` — override 24h default
- `--no-cache` — force miss path (debug)
- `--timeout <duration>` — hard kill budget; default 500ms on prompt-submit, 5s elsewhere
- `--debounce-window <duration>` — coalesce window for post-edit (default 1s)
- `--batch <path>...` — accept multiple paths for a single re-index call
- `--keepalive <duration>` — forwarded as Ollama `keep_alive` (default "10m")

Also: a top-level `heimdall hooks cache-clear` and `heimdall hooks cache-stats`.

### To `hook-architect`

- Hot path ≤250ms: **`UserPromptSubmit` only.**
- Warm path ≤2s: `SessionStart`.
- Background / fire-and-forget: all `PostToolUse` variants, `Stop`, `SessionEnd`.
- `UserPromptSubmit` should be a `command` hook (not `prompt`) — we evaluated
  the `prompt`-type option and rejected it on latency grounds (§4).
- All hot-path hooks must tolerate empty stdout on timeout (Claude just gets
  no injected context for that turn).

### To `failure-modes`

- **All phase-1 hooks exit 0 always.** Failure class lives in the structured
  log line (parsed by `hooks-doctor`), not in the exit code. Claude Code
  collapses non-zero non-2 to "ignore and proceed" anyway, so a distinct
  latency-fail exit code would be user-invisible and double the test matrix
  for no benefit. (Confirmed with failure-modes.)
- **Timeout on `UserPromptSubmit` = write empty stdout, exit 0.** Never exit
  2 from a latency failure — that would block the turn on a performance
  regression, which is strictly worse than losing context for one turn.
- **Ollama unreachable on `UserPromptSubmit` = same as timeout** (silent
  skip, empty stdout, exit 0). Repeating a banner on every turn would be
  noise.
- **Ollama unreachable on `SessionStart` = visible one-line banner, exit 0.**
  Exception to the silent-skip rule: SessionStart fires once per session, so
  a user who goes silent here has no idea heimdall didn't run. Banner shape:
  `> heimdall: ollama unreachable at <endpoint> — retrieval disabled this session. Start with: ollama serve`
  Still exit 0. Costs ~80 bytes of session context; buys "why is my memory
  not showing up" debuggability.
- `index_version` missing (fresh store) = treat as "0", cache warming phase
  hits only on exact repeat prompts.
- Cache DB corruption = log once, drop cache table, continue. Never crash
  the hook.
- **Read path has no reindex side-effect.** The hot `UserPromptSubmit` hook
  never spawns or requests a reindex, even when the stale flag is set. This
  is deliberate: cross-process write-lock contention between the MCP server's
  background indexer and a hook-triggered reindex is unsafe (see 04 §A.6).
  Stale results are served Tier A silent; freshness is maintained by the
  PostToolUse(Edit|Write) debouncer (§6) and explicit `heimdall-mcp index`.
  Revisit in phase 2 once the write-lock question is resolved.

## 8. Open questions

1. **Claude Code hook queuing semantics.** If a `UserPromptSubmit` hook is
   still running when the user types the next prompt, does Claude Code (a)
   wait, (b) kill the previous, (c) fire a new one in parallel? Behavior
   determines whether we need per-session serialization. **Needs doc check or
   empirical test in `test-rollout`.**
2. **Does Ollama `keep_alive` actually extend across idle periods we care
   about?** Field exists but its effective ceiling (memory pressure, other
   model swap-ins) is undocumented. Test: measure embed latency after 5, 10,
   30 minute idles with `keep_alive: "10m"` set.
3. **100k+ index measurements.** No real-world index that size yet. The
   extrapolated 450ms budget number will either hold or motivate ANN
   (HNSW/IVF) — scope for tiered retrieval milestone, not this one.
4. ~~**Multi-model indexes.**~~ **Closed with hook-architect + failure-modes.**
   Hooks always use `cfg.Model`. If no index exists for that model, Tier B
   skip (warn, exit 0, no stdout). The "smallest-dim wins" latency optimization
   originally proposed here would silently cross-pollute embedding spaces via
   `ResolveUsableModelDB`'s fuzzy matcher — that's a correctness failure, not
   a latency trade. See 01 §hook-selection and 04 §3.
5. ~~**Concurrent hook cache writes.**~~ **Closed by existing SQLite config.**
   `OpenStore` already sets `_journal_mode=WAL&_busy_timeout=5000`. Empirical
   verification lives in test-rollout's harness.
6. ~~**Filter-inclusive cache key.**~~ **Closed — folded into §3 above.** Key
   now includes `sub_project`, `source_type`, and
   `canonical_json(metadata_filter)`.

## 9. Summary

- **Budget:** 250ms p95, 500ms hard timeout on `UserPromptSubmit`. Everything
  else is either warm-path (2s) or fire-and-forget.
- **Cache:** one new SQLite table in the existing store, keyed on
  `(normalized_prompt, index_version)`, 24h TTL, 1k row cap. Auto-invalidates
  on any `Upsert`/`Remove` via a single `store_metadata` integer bump.
- **Skip heuristic:** length guard only. No LLM gate. Always-on retrieval with
  caching is cheaper and less brittle than a classifier.
- **Cold start:** mitigate with `keep_alive: "10m"` on Ollama embed requests.
  Daemon deferred to v2 behind an env var upgrade path.
- **Debounce:** lock file + 1s coalesce window + batched re-index for
  `PostToolUse(Edit|Write)`. Other hooks need no debouncing.
- **First-turn-after-idle** may still spike to ~400ms. Accept, document.
