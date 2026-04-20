# P0 design — Claude's self-use of heimdall

**Status:** design approved, ready for implementation plan.
**Date:** 2026-04-19.
**Source problem:** [`00-problem-and-fixes.md`](./00-problem-and-fixes.md).
**Scope:** P0 items only (P1/P2 deferred).
**Implementation gate:** plan doc skips the 3-reviewer / 95-score gate per the kickoff note in `00-problem-and-fixes.md`. The eventual implementation PRs still apply the normal quality gate.

---

## 1. Goal and success criterion

A Claude Code session in this repo, asked "have you been using heimdall?" one hour in, can list at least 3 direct (non-sub-agent, non-hook-injected) heimdall tool calls with concrete reasons, and has made at least one `heimdall_remember` or `heimdall_index_text` write from that session.

The P0 work addresses the three highest-leverage drivers of drift identified in the problem doc:

1. Cross-session decay — non-obvious decisions evaporate at session boundary.
2. Abstract MCP instructions — "be proactive" doesn't fire; concrete trigger→action pairs do.
3. Misleading error scope — one "unavailable" banner makes Claude treat the whole MCP as broken, so writes stop too.

---

## 2. Architecture overview

Four units, each at a distinct existing seam. No new hook events, no new processes, no DB schema changes.

```
┌────────────────────────────────────────────────────────────────────┐
│  At SessionEnd (hook_stop.go:HookSessionEnd)                       │
│  ┌─────────────────────────┐   ┌─────────────────────────────┐     │
│  │ widen extractor         │──▶│ count candidate events      │     │
│  │ (replace last-5/500ch)  │   │ (markers + write-call ratio)│     │
│  └─────────────────────────┘   └──────────────┬──────────────┘     │
│           │                                   │                    │
│           ▼                                   ▼                    │
│  IngestSessionSummary              write .heimdall_db/hooks/       │
│  (existing)                        last-session-review.json        │
└────────────────────────────────────────────────────────────────────┘
                                                │
                                                ▼
┌────────────────────────────────────────────────────────────────────┐
│  At next SessionStart (hook.go:DispatchHook "session-start")       │
│  ┌────────────────────────────────────────────────────────┐        │
│  │ if last-session-review.json exists → render one-line   │        │
│  │ nag into "## Heimdall context" block, then consume     │        │
│  └────────────────────────────────────────────────────────┘        │
└────────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────────┐
│  At MCP initialize (mcp/server.go:handleInitialize)                │
│  ┌────────────────────────────────────────────────────────┐        │
│  │ rewrite instruction string: 5 trigger→action pairs     │        │
│  │ + one line about the SessionStart nag                  │        │
│  └────────────────────────────────────────────────────────┘        │
└────────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────────┐
│  At SessionStart Tier-B note (cli/hook.go:emitTierBNote)           │
│  ┌────────────────────────────────────────────────────────┐        │
│  │ split read-vs-write: say search is unavailable, call   │        │
│  │ out heimdall_remember/index_text still work            │        │
│  └────────────────────────────────────────────────────────┘        │
└────────────────────────────────────────────────────────────────────┘
```

**Coupling surface:** one new on-disk artifact — `.heimdall_db/hooks/last-session-review.json` — written by SessionEnd, read-and-unlinked by SessionStart. No cross-process RPC, no new tables.

**Blast radius:** three files plus tests.

| File | Change |
|---|---|
| `internal/cli/hook_stop.go` | rewrite `extractTranscriptSummary`; extend `HookSessionEnd` to write review record. |
| `internal/cli/hook.go` | extend `formatSessionStartBlock` / Tier-B banner formatter. |
| `internal/mcp/server.go` | replace string literal in `handleInitialize`. |

---

## 3. Unit 1 — widened extractor (P0 #1 part A)

**Replaces** `extractTranscriptSummary` in `hook_stop.go:179-231`.

**Current behavior:** last 5 assistant messages, 500ch each, 5000ch cap. Misses mid-session corrections entirely.

**New behavior:**

1. Parse the whole transcript (already JSONL, one message per line).
2. Scan user messages for markers grouped by intent:
   - **correction**: `\bactually\b`, `\bno,?\s`, `\bdon['']?t\b`, `\bstop\b`, `\binstead\b`, `\bwrong\b`
   - **frustration**: `\bfuck\b`, `\bwhy\b(?!\s+not)`, `\bbroken\b`
   - **teaching**: `\bturns out\b`, `\bfyi\b`, `\bheads up\b`, `\bfor reference\b`
   - **workaround**: `\bworkaround\b`, `\bhack\b`, `\btrick\b`, `\bgotcha\b`
3. For each hit, pull a context window — the user message containing the hit plus the preceding assistant message (omitted for a first-turn hit).
4. Append a tail fallback: last 3 assistant messages, trimmed to 800ch each, so sessions with zero marker hits still produce a non-empty summary.
5. Dedup overlapping windows, join with `\n\n---\n\n` separators, cap at 20 KB total.
6. Emit a per-hit structured record alongside the summary — consumed by Unit 2.

**New signature (replaces the current `extractTranscriptSummary(data []byte) string`):**

```go
type CandidateEvent struct {
    Marker  string // correction | frustration | teaching | workaround
    Excerpt string // ≤ 200 chars
}

// extractTranscriptSummary returns the summary text fed to IngestSessionSummary
// plus the candidate list consumed by the review-record writer. Both come from
// the same single-pass scan — no double-parse.
func extractTranscriptSummary(data []byte) (summary string, candidates []CandidateEvent)
```

**Why these markers and not others:** the problem doc §7 names three classes — user frustration/correction, non-obvious workaround, user-taught fact. Each group here maps to one of those three. Architectural or decision markers are explicitly not added: that's what embedding-based ingestion in `IngestSessionSummary` is for; the extractor must not duplicate that job.

**Bounded cost:** single-pass byte scan with cheap regex. Typical transcripts are <1 MB; a 10 MB session scans in <100 ms single-threaded.

**What stays the same:** `IngestSessionSummary` (embedding, dedup, memory-type classification) is untouched. The extractor just feeds it a richer summary.

---

## 4. Unit 2 — missed-remember nag (P0 #1 part B)

Closes the visible-cost gap identified in the problem doc §6. SessionEnd writes a small review record; next SessionStart renders it once, then deletes.

### 4.1 Write path (SessionEnd)

After the existing `IngestSessionSummary` call in `HookSessionEnd`:

- `candidates` = the slice returned by the rewritten extractor.
- `writeCount` = count of `heimdall_remember` + `heimdall_index_text` MCP tool calls in this session. Source: scan the same transcript for `tool_use` entries with matching names. (Rejected alternative: `hooks.log` doesn't record MCP tool calls, only hook firings, so it can't answer this.)

Then write `.heimdall_db/hooks/last-session-review.json`:

```json
{
  "session_id": "…",
  "ended_at": 1745097600,
  "candidates": 7,
  "writes": 0,
  "top_markers": ["correction", "correction", "workaround"],
  "excerpts": ["…≤200ch…", "…", "…"]
}
```

**Excerpt ranking:** only the 3 highest-priority events are persisted. Priority order: `correction` > `workaround` > `teaching` > `frustration`. Ties broken by recency (later in transcript first).

**Skip conditions** (do not write the file):
- Transcript missing, empty, or unreadable.
- `candidates <= 2 AND writes >= 1` — well-behaved session, no nag warranted. (This subsumes the zero-candidate case.)

`ended_at` is Unix seconds (same clock as the SessionStart staleness check in §4.2).

**File location:** `<project>/.heimdall_db/hooks/last-session-review.json` — same per-project directory as the session buffer. Overwrite-on-write: only the most-recent session needs reviewing, so a newer write replaces an unread older file.

### 4.2 Read path (SessionStart)

In `formatSessionStartBlock` after the "Recent memories" subsection, check for `last-session-review.json`. If present and `now - ended_at < 7 days`, render:

```
### Last session review
7 candidate remember-moments (3 correction, 1 workaround, 3 frustration) but
only 0 heimdall_remember / heimdall_index_text calls this session.

Examples:
- correction: "no, the other file — move the check before the loop not after"
- correction: "actually heimdall_remember still works when search is down"
- workaround: "binding to 127.0.0.1 instead of localhost fixed macOS ::1"

If any of these still matter, heimdall_remember them now.
```

**Then unlink the file.** Consume-once. If two sessions open in parallel, only the first sees the nag — acceptable: this is a soft nudge, not a hard alert.

**Stale-file handling:** if `ended_at` is older than 7 days, unlink without rendering. Prevents week-old nags from ambushing a fresh week.

**Doesn't enforce anything.** Doesn't block startup. Doesn't call `heimdall_remember` on the model's behalf. It is pure visibility — Claude decides whether to act. That decision loop is what the problem doc wants reinforced.

---

## 5. Unit 3 — MCP instruction block rewrite (P0 #2)

Replace the string literal in `internal/mcp/server.go:192-204`. Current ≈ 240 words, prose-numbered "use X proactively." New ≈ 200 words, 5 explicit `WHEN → ACTION` pairs plus one nag-pointer line.

### 5.1 New text

```
You have access to Heimdall, a local semantic code + memory store. Use it via
these concrete trigger→action pairs, not as a general reminder to "be proactive":

• WHEN you're about to Read a file > 200 lines or Grep/Glob to explore
  unfamiliar architecture → call heimdall_search FIRST, then read only the
  ranges that matter.

• WHEN the user corrects you ("actually", "no, do X", "don't"), teaches a
  non-obvious fact, or you discover a workaround (env quirk, build flag, API
  gotcha) → call heimdall_remember the same turn, with the rule + the reason.

• WHEN a WebFetch, Read, or external-MCP call (Jira, Slack, GitHub,
  Confluence, email) returns > 500 chars of content → call heimdall_index_text
  silently (no user-facing mention). Include the source URL/ID in metadata.

• WHEN starting a task that references past decisions, plans, or prior
  architecture → call heimdall_recall first to surface what was already
  decided. If multiple projects are indexed, heimdall_projects lists them.

• WHEN search feels wrong (empty results, surprising ranking) call
  heimdall_explain; WHEN setup seems off call heimdall_status.

At SessionStart you may see a "Last session review" block listing
remember-moments you missed in the previous session — treat it as a task,
not status: heimdall_remember the items that still matter before moving on.
```

### 5.2 What changed and why

| Old | New | Why |
|---|---|---|
| "Use heimdall_search FIRST when exploring" | "WHEN file > 200 lines OR unfamiliar architecture → heimdall_search FIRST" | Old rule has no off-switch; fires for trivial 20-line reads and feels like noise. New rule has a concrete threshold. |
| "Use heimdall_remember to store important decisions" | "WHEN user corrects / teaches / you find workaround → heimdall_remember same turn" | Old "important" is fuzzy; new fires on named speech acts. |
| "Whenever you read content from external sources → heimdall_index_text" | "WHEN external call returns > 500 chars → heimdall_index_text silently" | Adds explicit threshold plus silent-mode instruction. |
| (no such rule) | "WHEN starting a task referencing past decisions → heimdall_recall first" | The problem doc named this as missing. |
| "CROSS-REPO CONTEXT" paragraph | folded into the recall rule as a sentence | Low-signal standalone. |
| "Do not wait to be asked" trailing nudge | dropped | The generic exhortation is exactly what the problem doc identified as not-firing. |
| (no mention) | "Last session review block — treat it as a task" | Primes the nag from Unit 2 so it reads as actionable. |

### 5.3 Scope boundary

The Ollama-down fallback block (`instructions = fmt.Sprintf(…)` at `internal/mcp/server.go` ~line 213) is a separate string for a different failure mode. It keeps its current wording. Only the happy-path instructions literal changes.

---

## 6. Unit 4 — banner scoping (P0 #3)

Change `internal/cli/hook.go:371` — the single-line Tier-B note — into a two-line form that names which tool is down and what still works. Applies only when the banner would have fired anyway (Ollama up, index mismatch/missing/dim).

### 6.1 Diff

Current:

```go
fmt.Fprintf(w, "## Heimdall context\n\n> heimdall: unavailable (%s)\n", humanReason)
```

New:

```go
fmt.Fprintf(w,
    "## Heimdall context\n\n"+
    "> heimdall_search: unavailable (%s)\n"+
    "> heimdall_remember and heimdall_index_text still work — use them for this session's decisions.\n",
    humanReason,
)
```

### 6.2 Scope

Only the `classifyVerifyErr` branches — `index_model_missing | index_model_mismatch | index_dim_mismatch` — get the new two-line form. In those branches, Ollama is up, the memory DB is independent of the code index, and `heimdall_remember` / `heimdall_index_text` genuinely work.

The Ollama-unreachable banner elsewhere in `hook.go` keeps its current wording. In that branch, writes don't work either — the new reassurance line would be a lie.

`humanReason` values stay as-is (e.g., `"index model mismatch"`). No new strings, just a wrapper change.

---

## 7. Testing

| Surface | Test |
|---|---|
| **Extractor** | `TestExtractTranscriptSummary_MarkerWindows` — fixture transcripts with each marker group; assert excerpts + candidates returned. `TestExtractTranscriptSummary_NoMarkers_TailFallback` — transcript without any markers; assert tail fallback still emits. `TestExtractTranscriptSummary_BoundedOutput` — 10 MB fixture; assert ≤ 20 KB output and runtime < 100 ms. |
| **Review-record writer** | `TestHookSessionEnd_WritesReviewRecord` — synthetic transcript with 5 corrections + 0 writes; assert `last-session-review.json` written with candidates=5 writes=0. `TestHookSessionEnd_SuppressesWhenClean` — 1 candidate + 2 writes; assert no file written. `TestHookSessionEnd_SuppressesOnEmptyTranscript` — no transcript; assert no file and no crash. |
| **Nag renderer** | `TestSessionStartBlock_IncludesLastSessionReview` — planted `last-session-review.json`; assert block contains the "N candidate remember-moments" line and ≤ 3 excerpts. `TestSessionStartBlock_ConsumesReviewFile` — assert file deleted after render. `TestSessionStartBlock_SkipsStaleReviewFile` — 8-day-old file; assert unlinked without render. `TestSessionStartBlock_SkipsMissingFile` — no file present; assert normal block, no review section. |
| **Banner** | `TestEmitTierBNote_NewTwoLineForm` — assert output contains both `heimdall_search: unavailable` and `still work` lines. Extend existing `TestHookSessionStart_ModelMismatch` golden to the new two-line form. `TestEmitTierBNote_OllamaDownBannerUnchanged` — regression guard that Ollama-down branch still emits the old single-line form. |
| **MCP instructions** | `TestHandleInitialize_InstructionsContainTriggerPairs` — assert each of the 5 bullet markers is present (substring match); guards against accidental truncation in future edits. |

---

## 8. Explicit non-goals

- No chat-model fallback for candidate extraction (marker-based only).
- No per-tool status matrix banner.
- No changes to `LogHookEvent`, no new hook events, no new DB tables.
- No rewrite of `IngestSessionSummary`. The extractor hands it a plain-text summary as today.
- No changes to `CLAUDE.md` or system prompt (P1).
- No `UserPromptSubmit` nag-after-N-turns hook (P1).
- No transcript post-processor that flags skipped remember moments at session close (P2).
- No auto-fire of `heimdall_index_text` by the runtime on external-content reads (P2).

---

## 9. Acceptance

The design is accepted when:

- The five test groups in §7 all pass.
- A real end-to-end session in this repo produces a `last-session-review.json` on close, and the following session renders and consumes it.
- The MCP initialize response contains the new 5-bullet text verbatim.
- The `index model mismatch` case (reproducible by pulling a different embedding model than the index was built with) produces the new two-line banner.

Matches the success criterion in §1: within one hour of a fresh session, Claude can cite ≥ 3 direct heimdall calls with concrete reasons and has made ≥ 1 heimdall_remember / heimdall_index_text write.
