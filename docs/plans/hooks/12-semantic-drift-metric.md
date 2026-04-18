# 12 — Semantic-Drift Metric for Missed Tool-Call Opportunities

**Status:** Draft. **Design only — no implementation in this PR.** Chains off
`10-per-session-savings-report.md` (the `SessionReport` JSON envelope) and
the v1 textual counter shipped in PR #39 (`redundant_heimdall_calls`;
see `internal/heimdall/transcript.go` lines 36–43, 191–199).
**Audience:** human approver + the engineer who will implement Wave F
item 3(A) once the v1 counter has ≥2 weeks of real-session data to
calibrate thresholds against.
**Reads like:** `08-destructive-op-primitive.md` and
`11-llm-classification-fallback.md`. Numbered sections, concrete
examples, one recommendation per open question, every choice gets a
"Why:".

---

## 1. Goal & Non-Goals

### 1.1 Goal

Replace the purely textual `redundant_heimdall_calls` counter with a
**semantic-aware** pair of metrics:

1. **`semantic_redundant_calls`** — UserPromptSubmit injected hits,
   and the assistant's subsequent `heimdall_search` query vector is
   cosine-close (≥ T1) to ≥1 injected hit's vector. The hit *was*
   relevant, the call was wasted.
2. **`missed_call_opportunities`** — assistant made zero heimdall_*
   calls in a turn whose prompt vector is close (≥ T2) to ≥1 indexed
   chunk. The model could have benefited and didn't ask.

Both slot into the existing `SessionReport.ToolUse` block as additive
JSON fields under `semantic_drift`.

**Why we care:** the v1 counter is textual — "hit-injected turn + any
heimdall call" increments. Real session: the hook injects 5 hits
about `HookPreToolUse`, the model then `heimdall_search`es "tiered
retrieval plan" (unrelated). v1 logs `redundant=1`. That's a false
positive against the metric's stated purpose ("hook already covered
what the model needed"). Conversely, v1 cannot see the *missed* case
at all. The semantic version fixes both.

### 1.2 Non-goals

- **Not a retrieval-quality evaluator.** We score embedding-cosine
  against the corpus we have; "was that the right answer" needs human
  labels, not this doc.
- **Not user-intent modeling.** We don't classify "is the user asking
  for code vs. chit-chat." If the user says "thanks" and the hook
  injects 5 hits, that's a low-similarity turn, no special casing.
- **Not a v1-replacement in one PR.** v1 and v2 co-exist for a soak
  period (§10) so we can compare them on real sessions before
  deprecating v1.
- **Not on any hot path.** Compute is post-hoc by default (§4). If we
  ever put drift scoring on the UserPromptSubmit budget we re-open
  this doc.
- **Not a second retrieval engine.** Reuse existing vectors, no
  LLM-paraphrase, no intent classification. The LLM-classification
  extension from `11-*.md` is orthogonal.
- **Not shipping in v1 of Wave F.** v1 (textual) already shipped in
  PR #39. This design is v2, gated on ≥2 weeks of v1 telemetry.

---

## 2. Precise Metric Definition

### 2.1 Vocabulary

| Term | Meaning | Source |
|---|---|---|
| **Turn** | Span from one `type:"user"` transcript line to the next (or EOF). | `transcript.go:119–154` |
| **Injected hits** | `VectorRecord`s appearing in the `## Heimdall context` block emitted by `UserPromptSubmit`. | `hook_user_prompt.go` (`results`) |
| **Prompt vector (`pvec`)** | 768-dim `nomic-embed-text` embedding of the user prompt, computed in-hook for the live search. | `EmbedForHook` |
| **Assistant query vector (`qvec`)** | 768-dim embedding of a `heimdall_search` `query` param. | Post-hoc re-embed (§3.4). |
| **Hit vector** | The `VectorRecord.Embedding` blob on an injected hit. | `store.go:27` |
| **T1** / **T2** | Cosine thresholds for "call was redundant" / "turn was a missed opportunity". Calibrated; §5. |

### 2.2 `semantic_redundant_calls`

Per session, over every assistant turn:

```
if turn had >=1 injected hit (UserPromptSubmit stdout non-empty):
    for each heimdall_search call the assistant made this turn:
        qvec = embed(search.query)
        best = max_i cosine(qvec, hit_i.embedding)
        if best >= T1: semantic_redundant_calls++
```

**Why only `heimdall_search`:** `heimdall_recall` pulls typed
memories (different vector space usage), `heimdall_expand` takes a
`chunk_id` (embedding an id string is nonsense), `heimdall_ls` is a
filesystem browse. Restricting to `heimdall_search` is the only
signal where query↔hit cosine is meaningful. See §9 OQ-2 for recall.

**Why max-over-hits:** the hook injected ≥1 relevant chunk means the
call was avoidable. Max is the most lenient reading for the model —
if *any* injected hit was close, we count the call as redundant.

**Why cosine (not Euclidean / dot):** `nomic-embed-text` vectors are
L2-normalized by Ollama; cosine is already what
`VectorStore.SearchFiltered` uses (`store.go:204` →
`CosineSimilarity` in `vecmath.go:9`). No new math to justify.

### 2.3 `missed_call_opportunities`

Per session:

```
if assistant made ZERO heimdall_* tool calls this turn AND pvec available:
    best = max over top-K indexed chunks of cosine(pvec, chunk.embedding)
    if best >= T2: missed_call_opportunities++
```

**Why "zero heimdall_* calls":** if the model already called
heimdall, whether the call was good is §2.2's question. This metric
is strictly the "no call at all" bucket.

**Why pvec required:** if the hook skipped (prompt < 8 chars,
`HEIMDALL_HOOKS=0`, cache hit — see §3.5), we have no vector. The
turn drops from the denominator (`turns_skipped++`). See F1/F8.

**Why max over top-K (replayed search), not full-DB scan:** the
existing `SearchFiltered(pvec, 5, ...)` already gives top-5 in a
single query; taking max-over-top-5 is a faithful replay of "what the
hook would have surfaced." Full-DB is wasteful with no benefit.

### 2.4 Two-metric invariant

Per turn, *at most one* of the two metrics increments:

- **Redundant bucket:** turn had hits + ≥1 heimdall_search call. One
  increment per redundant call (a turn with 3 searches, 2 above T1,
  contributes 2).
- **Missed bucket:** turn had *no* heimdall calls, pvec available,
  best-chunk sim ≥ T2. Max 1 increment per turn.
- **Neither:** turn had hits + no heimdall call (hook worked), or
  turn had heimdall calls all below T1 (genuinely novel), or pvec
  unavailable.

**Why the invariant:** keeps the two metrics orthogonal. Never
double-count a turn where the model searched (even ineffectively)
*and* the prompt was still close to indexed content — if the model
already went looking, "missed" is nonsensical.

---

## 3. Data Source

### 3.1 What we already have

- **Prompt vector.** `hook_user_prompt.go` computes
  `queryVec = embedder.EmbedForHook(prompt)`, then discards it.
  Needs to be persisted somewhere the post-hoc pass can find it
  (§3.5).
- **Injected hits.** The hook renders
  `results []SearchResult` via `formatSearchHookMD`
  (`internal/cli/cli.go:541`) into a markdown body that's written to
  stdout + `HookCachePut`. The per-hit `chunk_id`s are not
  structurally preserved — they live only in the rendered
  `file:line-line — snippet` strings.
- **Hit vectors.** Every `VectorRecord` in `entries` carries an
  `Embedding` blob (`store.go:27`, encoded by `EncodeFloat32Vec`).
  `ExpandByID(chunkID)` fetches rows by id but **does not select
  the embedding column** — add a sibling
  `VectorByID(chunkID) ([]float32, error)` that selects only the
  blob. No new table.
- **Assistant query text.** Transcript `tool_use` items for
  `mcp__heimdall__heimdall_search` carry `input.query`. The parser
  in `transcript.go` reads tool *names* today; capturing
  `input.query` on `heimdall_search` calls is a small extension.
- **Assistant query vector.** Computed post-hoc — the hook never
  sees assistant tool calls. See §3.4.

### 3.2 The minimal hook-side log change

One write-only change at `stage=ok`: add two keys to the existing
log map.

- `hit_ids="id1,id2,..."` — comma-joined chunk ids of the injected
  hits. Already short strings.
- `prompt_embed_b64="<base64>"` — stdlib base64 over the 3072-byte
  little-endian float32 block produced by
  `EncodeFloat32Vec(queryVec)`.

Cost: one `base64.Encode` over 3 KB + two map writes. <1 ms, fits
inside the 250 ms budget with ~99% slack. Not on the guardrail
surface — no OQ-5 implications.

**Size:** 3 KB per prompt × ~50 prompts/session × many sessions into
the 5 MB `hooks.log` cap. At 50 prompts/day × 14 days ≈ 2 MB, well
under cap. Rotation behavior unchanged.

**Why not log to a separate table/file:** `hooks.log` already
has the machinery (rotation, session=<uuid> tagging from Wave A,
`ReadHookLog` reader in `hooklog_reader.go`). The post-hoc reader
already streams this file. Additive keys are the cheapest
integration point.

### 3.3 Why log the prompt vector instead of re-embedding at report time

The prompt text is in the transcript, so we *could* re-embed at
`sessions report` time. But:

1. **Re-embedding every prompt on every report run** adds latency
   proportional to session length. One-time log write at hook time
   is free thereafter.
2. **Normalization drift.** The hook may normalize/clean the prompt
   before embedding; the transcript stores the raw user content.
   Re-embedding the transcript form gives a drifted vector, and the
   metric would lie about what the hook actually compared against.
3. **3 KB × low-count is nothing.** 1–2% of `hooks.log` cap per
   week of real use.

### 3.4 Assistant query vectors — post-hoc batch re-embed

At `sessions report` time:

1. Parse transcript, collect `(turn_idx, query_text)` for each
   `heimdall_search` tool_use.
2. Batch-embed queries via existing `EmbedBatch(ctx, model, queries)`
   (`ollama.go:121`; cap 32/batch, auto-splits).
3. Assign vectors back to `(turn_idx, qvec)`.

**Why post-hoc:** the assistant tool call happens *after* the hook
returns. No existing PreToolUse matcher fires on heimdall tools (the
PreToolUse matcher is `Bash`-only). Adding a second PreToolUse
matcher would be a new install-plumb change; ROI doesn't justify it.
Post-hoc is cheap and has no latency impact on user-facing paths.

**Why Ollama at report time:** retrieval hooks already require
Ollama. `sessions report` is explicit and synchronous — requiring
Ollama for the semantic variant of the metric is fine. If Ollama is
down, the metric degrades to `n/a` (§7 F2), not an error.

### 3.5 Cache-hit turns

`stage=cache_hit` means the hook short-circuited — the cached body
was served, no embed happened. We do not currently persist the
prompt vector in `hook_cache` rows.

**Two options:**

1. Expand each `hook_cache` row to carry the vector. ~3 KB per row ×
   1000-row cap = 3 MB worst case.
2. Accept that cache-hit turns drop from the semantic-drift
   denominator.

**Recommendation: (2) for v1 of the metric.** Option 1 is a schema
change to `hook_cache` we'd want to soak independently. Lost
denominators are tracked as `turns_skipped`. See §9 OQ-1.

### 3.6 Pipeline summary

```
UserPromptSubmit (hot)           Stop / transcript (live, no vectors)
 ─ embed(prompt) -> pvec          ─ writes turn to transcript.jsonl
 ─ search -> hits[]               ─ tool_use entries carry input.query
 ─ render + inject                      │
 ─ log stage=ok hits=N                  │
 ─ NEW: + hit_ids + prompt_embed_b64    │
           │                            │
           ▼                            ▼
         hooks.log          ~/.claude/projects/.../*.jsonl
                  │
                  ▼
         sessions report (post-hoc, cold)
           for each turn:
             decode pvec from log
             lookup hit vectors via VectorByID
             batch-embed assistant heimdall_search queries
             apply §2.2 / §2.3 rules → counts
```

---

## 4. Compute Placement: Live vs. Post-Hoc

### 4.1 Recommendation: post-hoc inside `sessions report`

**Why:**

1. **Budget.** `UserPromptSubmit` has 250 ms soft / 500 ms hard (03-*.md;
   PR #31 bumped first-turn to 450 ms for cold Ollama). Every ms of
   drift scoring steals from retrieval itself. `sessions report` has
   no budget — the human asked for it.
2. **Data asymmetry.** `missed_call_opportunities` requires knowing
   the assistant made zero heimdall calls *this turn*, information
   the hook can never have — it fires *before* the assistant turn.
   Live scoring of that metric is structurally impossible.
3. **Threshold iteration.** We expect to tune T1/T2 (§5). Live
   scoring means re-shipping the hook per tuning pass. Post-hoc
   replays the same logged vectors against a different threshold
   instantly. Essentially a replay harness.
4. **Separation of concerns.** v1's textual counter lives entirely in
   the transcript parser. Keeping v2 post-hoc preserves that
   boundary; hook path stays retrieval-only.

### 4.2 What goes live (minimal)

Only logging (§3.2). No similarity math, no I/O, no Ollama calls on
the hook side. All similarity compute lives in
`internal/heimdall/semantic_drift.go` (new) called from
`sessions report`.

### 4.3 New file layout

```
internal/heimdall/semantic_drift.go   NEW
  ComputeSemanticDrift(ctx, inputs) *Result
    inputs:  TranscriptSummary + hooks.log entries for session +
             VectorStore + OllamaClient
    output:  {semantic_redundant_calls, missed_call_opportunities,
              near_threshold_t1, near_threshold_t2, turns_total,
              turns_with_hits, turns_skipped, threshold_t1, threshold_t2,
              embedding_model, threshold_version, error?}

internal/heimdall/store.go            +VectorByID helper
internal/heimdall/transcript.go       capture input.query on
                                        heimdall_search tool_use
internal/heimdall/hooklog_reader.go   decode hit_ids + prompt_embed_b64
internal/cli/sessions.go              render + marshal in SessionReport
internal/cli/hook_user_prompt.go      +2 log keys at stage=ok only
```

`ComputeSemanticDrift` takes injected deps (store, embedder) so tests
can stub both (same idiom as `HookUserPromptDeps`).

---

## 5. Thresholds and Calibration

### 5.1 Rough similarity bands on `nomic-embed-text` code+docs

- **0.30–0.45**: loosely related topic.
- **0.45–0.60**: clearly related, useful context.
- **0.60–0.80**: strongly related, probable answer.
- **0.80+**: near-duplicate.

These are rules of thumb from the model card, **not measured on our
corpus.** The §5.3 bake-off is how we fix that.

### 5.2 Recommended starting values

| Threshold | Start | Interpretation |
|---|---|---|
| **T1** (redundant) | **0.55** | Assistant's query is solidly in the injected-hit topic. |
| **T2** (missed) | **0.45** | Prompt is at least in the same topic area as something indexed. |

**Why T1 > T2:** the false-positive cost of
`semantic_redundant_calls` is "we accuse the model of wastage" — a
louder claim. The FP cost of `missed_call_opportunities` is "we flag
a turn where heimdall would have been mid." Former should require
stronger similarity to cry wolf.

**Why not a single threshold:** the two questions are orthogonal
(§2). One knob couples their FP rates.

### 5.3 Calibration plan (the bake-off)

Prerequisite: ≥2 weeks of v1 telemetry + Stage-1 vector logging.

1. **Sample 100 turns.** 50 flagged by v1, 50 unflagged. Tests both
   sensitivity and specificity.
2. **Hand-label.** A human skims `(prompt, injected hits, assistant
   heimdall_search calls)` and labels each turn
   `{truly-redundant, truly-missed, neither}`.
3. **Sweep.** Run `ComputeSemanticDrift` over the sample with
   T1 ∈ {0.40..0.70 step 0.05}, T2 ∈ {0.35..0.55 step 0.05}.
   Compute precision / recall per pair against hand labels.
4. **Pick precision-first.** Target ≥85% precision on both metrics;
   accept whatever recall comes with it.
5. **Pin constants** in `semantic_drift.go`; echo the chosen pair in
   the JSON output as `threshold_t1` / `threshold_t2` and bump
   `threshold_version` on any future change.

**Why precision-first (not F1):** a false-positive tells the user
"your tool calls are wasteful" when they aren't. That erodes trust
faster than missing a real redundancy. Precision > recall.

### 5.4 Reusing `bench-retrieval`?

`cmd/bench-retrieval` has read-only-snapshot plumbing +
`FindRepoRoot` auto-detect we can reuse. Not the query set — that's
token-savings, not drift.

**Recommendation:** ship a sibling `cmd/calibrate-drift` that reads a
labeled JSONL, pulls vectors from logs/store, sweeps (T1, T2), and
emits a precision/recall CSV. Not in scope for the metric PR itself.
Flagged in §9 OQ-3.

### 5.5 Confidence buckets

Alongside the two core counts, expose:

```
near_threshold_t1: count of semantic_redundant_calls with best ≤ T1+0.05
near_threshold_t2: count of missed_call_opportunities with best ≤ T2+0.05
```

**Why:** 12 redundants all barely over threshold is a different
situation than 12 all well-over. Exposing the "borderline" share
lets the human reading the report triangulate without re-running
raw numbers.

---

## 6. SessionReport Field Shape

### 6.1 JSON

Extend `tool_use` in `SessionReport.toJSON` (see PR #39 diff,
`sessions.go:325–335`):

```json
"tool_use": {
  "total": 87,
  "heimdall": 14,
  "redundant_heimdall_calls": 6,
  "by_name": { "Bash": 32, "Edit": 11, ... },
  "semantic_drift": {
    "semantic_redundant_calls": 4,
    "missed_call_opportunities": 3,
    "near_threshold_t1": 2,
    "near_threshold_t2": 1,
    "turns_total": 42,
    "turns_with_hits": 31,
    "turns_skipped": 5,
    "threshold_t1": 0.55,
    "threshold_t2": 0.45,
    "embedding_model": "nomic-embed-text",
    "threshold_version": 1
  }
}
```

**Why nested under `semantic_drift`:** keeps top-level `tool_use`
readable; cleanly separates v2 from v1's flat
`redundant_heimdall_calls`.

**Why echo thresholds + model + threshold_version in the output:**
reproducibility. The report is a statement of fact against a
specific threshold configuration and embedding model; encoding them
makes it self-describing and makes §5.3 recalibrations non-breaking
for downstream consumers.

**Why `turns_total` / `turns_with_hits` / `turns_skipped`:**
`semantic_redundant_calls=0` means different things over 100-turn
session with 95 hits vs. a 5-turn session with 0 hits. Denominators
let consumers compute rates without re-parsing.

### 6.2 Text

Existing `Tool use` block gains one trailing line:

```
## Tool use
  total=87 heimdall=14 redundant_heimdall_calls=6
  - Bash: 32
  - ...

  semantic_drift: redundant=4 missed=3 (T1=0.55 T2=0.45, near: t1=2 t2=1)
```

### 6.3 Schema version policy

Per PR #39: additive field → `schema_version` stays `"v1"`. Bump to
`"v2"` only when we *drop* `redundant_heimdall_calls` (§10 Stage 3).

---

## 7. Failure Modes

Every failure degrades to `semantic_drift: null` + a diagnostic key.
`sessions report` never fails on drift errors. Same fail-open posture
as 08 §5 and 11 §6.

| # | Failure | Behavior | Log |
|---|---|---|---|
| F1 | `prompt_embed_b64` missing (pre-v2 log lines, or cache-hit turn per §3.5) | Drop turn from both denominators; `turns_skipped++`. | no (silent, accounted) |
| F2 | Ollama unreachable at report time | `semantic_drift: null`; `semantic_drift_error = "ollama_unreachable"`; text shows `n/a (ollama unreachable)`. | yes — one ERROR to stderr of `sessions report` |
| F3 | Chunk id in `hit_ids` no longer in store (reindex between hook fire + report) | Skip that hit; if all hits of a turn missing, turn drops from redundant denominator. | yes — WARN per dropped id, deduped |
| F4 | `prompt_embed_b64` decodes to non-768-dim vector | Skip turn, `turns_skipped++`. Usually model/version mismatch; the `embedding_model` field in the JSON makes it visible. | yes — one WARN per session |
| F5 | Tool_use for `heimdall_search` has malformed/missing `input.query` | Skip that call; other calls in the turn still scored. | yes — WARN with tool_use_id |
| F6 | Batch re-embed returns fewer vectors than inputs | `EmbedBatch` already errors on mismatch; bubble up as F2. | yes |
| F7 | T1/T2 corrupted at build (e.g. `0` or `>1`) | Refuse to compute; `semantic_drift_error = "invalid_threshold"`. Guard in `init()`. | yes — ERROR |
| F8 | Zero UserPromptSubmit events in session (e.g. imported transcript) | `semantic_drift: null`; `semantic_drift_error = "no_hook_data"`. | no (expected on imports) |
| F9 | Cost blowup — batch re-embed > 30 s | `context.WithTimeout(ctx, 30s)` on the batch step; timeout degrades to F2. | yes — WARN with elapsed_ms |

### 7.1 Cost envelope

Per session:

- **Prompt vectors:** zero Ollama calls — pre-logged.
- **Assistant queries:** one batch per 32 queries. Typical session
  with ≤30 `heimdall_search` calls = 1 round-trip.
- **Hit-vector lookup:** one SQLite SELECT per distinct hit id. 30
  prompts × 5 hits = 150 lookups ≈ 10 ms on warm SQLite.
- **Missed-call replay:** one `SearchFiltered(pvec, 5)` per
  no-heimdall-call turn. ~30 ms each.

**Worst case** (200 turns, all no-heimdall, all vectors logged):
200 × 30 ms = 6 s + 1 batch ≈ 1 s. Well inside any `sessions report`
UX budget. Mitigation if it grows: worker-pool the per-turn replay.

### 7.2 Deliberately not handled

- **Embedding drift across model versions.** `VerifyHookIndex`
  catches full mismatches; F4 catches dimension mismatches;
  within-version content drift is invisible to us (accepted risk).
- **Scope filtering.** Hook-time scope (PR #17) is not replayed; we
  search the full index. Missing a hit because scope would have
  hidden it is *also* a missed opportunity, so over-count is
  acceptable in v1. A `--scope-aware` flag is §9 OQ-5.

---

## 8. Test Strategy

All Go, Layer 1 in-process with deterministic stubs, Layer 2 against
`httptest`. No live-Ollama in CI. Same idioms as
`internal/heimdall/transcript_test.go` and
`internal/cli/sessions_test.go`.

### 8.1 Layer 1 — unit with `FakeEmbedder`

Fake embedder returns deterministic vectors keyed on input string.
Vectors are hand-constructed so cosine hits known values (e.g.
`(1,0,0,...)` vs `(0.9, 0.436, 0,...)` = cosine 0.9).

| ID | Test | Asserts |
|---|---|---|
| SD1 | `TestCompute_NoHookData` | No UserPromptSubmit events → null + `no_hook_data`. |
| SD2 | `TestCompute_NoHeimdallCalls_LowSim` | No heimdall calls, pvec sim < T2 → missed=0. |
| SD3 | `TestCompute_NoHeimdallCalls_HighSim` | Same, sim ≥ T2 → missed=1. |
| SD4 | `TestCompute_HeimdallCalls_LowSim` | Turn with hits + 1 search, max cos < T1 → redundant=0. |
| SD5 | `TestCompute_HeimdallCalls_HighSim` | Same, max cos ≥ T1 → redundant=1. |
| SD6 | `TestCompute_MultiSearchInTurn` | 3 searches, 2 above T1 → redundant=2. |
| SD7 | `TestCompute_NoPromptVector` | Log entry missing prompt_embed_b64 → `turns_skipped++`. |
| SD8 | `TestCompute_MissingChunkID` | Hit id not in store → F3: hit skipped, WARN. |
| SD9 | `TestCompute_EmbedderFails` | Query embed errors → F2: null. |
| SD10 | `TestCompute_ThresholdBoundary` | Cos == T1 counts (≥ semantics); T1-0.0001 does not. |
| SD11 | `TestCompute_NearThresholdBuckets` | Borderline values populate near_threshold_{t1,t2}. |
| SD12 | `TestCompute_InvariantNoDoubleCount` | Turn with heimdall calls never counted toward missed. |
| SD13 | `TestCompute_DimMismatch` | 512-dim pvec → F4, skipped. |
| SD14 | `TestCompute_MalformedQueryInput` | Missing input.query → F5, call skipped, session still scores. |
| SD15 | `TestCompute_NonHeimdallTool` | Bash/Edit/Read never trigger redundant path. |
| SD16 | `TestCompute_EmptyHitsTurn` | Stdout present but empty hits → in `turns_total`, not `turns_with_hits`. |
| SD17 | `TestRender_JSONSchema` | `toJSON` matches §6.1 exactly; stable key order. |
| SD18 | `TestRender_JSONNullOnError` | F1–F9 error paths serialize null + correct `semantic_drift_error`. |
| SD19 | `TestRender_TextLine` | Text output emits §6.2 line with correct values. |
| SD20 | `TestCompute_Deterministic` | Run 10×, assert identical JSON bytes. Same guard as
`destructive_ops_test.go:391–397`. |

### 8.2 Layer 2 — `httptest`-fake Ollama

| ID | Test | Asserts |
|---|---|---|
| SD-L2-1 | `TestDrift_HappyPath` | Fake Ollama + fake store + hooks.log with 2 prompt vectors → end-to-end metric. |
| SD-L2-2 | `TestDrift_OllamaDown` | httptest refuses → F2; report still renders. |
| SD-L2-3 | `TestDrift_BatchSplit` | >32 assistant queries → two batches, stitched correctly. |
| SD-L2-4 | `TestDrift_CancellationPropagates` | ctx cancelled mid-batch → clean HTTP cancel, no goroutine leak. |

### 8.3 No live-Ollama in CI

Live smoke behind `//go:build livellm` (same posture as 11 §7.3),
hand-run before release; CI gates on Layers 1+2 only.

---

## 9. Open Questions

1. **Cache-hit prompt vectors.** §3.5 drops cache-hit turns from the
   denominator. Alternative: expand `hook_cache` to carry the vector
   (~3 MB worst case at 1000-row cap). **Recommendation:** defer to
   v2.1; soak the metric first on `stage=ok`-only data.
2. **`heimdall_recall` in the redundant bucket.** §2.2 excludes it.
   **Recommendation:** keep exclusion; track a sibling counter
   `heimdall_recall_when_hits_present` as a companion field if
   there's appetite. Do not fold into the cosine score.
3. **Calibration harness.** §5.3 assumes a 100-turn hand-labeled
   sample; we have no labeling tool. **Recommendation:** ship a
   small `cmd/calibrate-drift` binary alongside the metric PR;
   first real bake-off is the pre-v2-graduation gate.
4. **Which tools count as "model used heimdall" for missed-call
   exclusion.** Today: any `heimdall_*`. **Recommendation:** keep
   the lenient reading — if the model chose `heimdall_ls` instead of
   `heimdall_search`, they checked, we can't second-guess.
5. **Scope replay.** Full-index replay over-counts misses in
   scope-constrained sessions. **Recommendation:** defer to a
   `--scope-aware` flag on `sessions report` in v2; not in scope
   (pun intended) for this metric's v1.
6. **Confidence-bucket width.** `T+0.05` is arbitrary.
   **Recommendation:** ship 0.05 as `NearThresholdDelta` constant;
   revisit after the calibration sample shows what borderline looks
   like empirically.
7. **Per-corpus thresholds.** Different corpora → different natural
   similarity distributions. **Recommendation:** keep global
   constants for v1; add `heimdall-mcp configure
   --semantic-drift-t1=... --semantic-drift-t2=...` in v2 if users
   ask.
8. **Query-text normalization.** The hook may normalize/clean the
   prompt before embedding. `heimdall_search` queries from the
   assistant may have odd shapes (filters inline, etc.).
   **Recommendation:** match hook-side normalization on re-embeds;
   re-use whatever helper the hook uses, confirm at implementation.
9. **Metric names.** `semantic_redundant_calls` /
   `missed_call_opportunities` vs. shorter forms (`drift_wasted` /
   `drift_missed`). **Recommendation:** keep the proposal —
   "redundant" matches v1 vocabulary; plain English wins.
10. **Per-turn verbose output.** Useful for debugging the metric;
    noisy for the headline. **Recommendation:** add `--verbose` on
    `sessions report` that dumps per-turn similarity rows; keep the
    headline clean.

---

## 10. Rollout Plan

Three stages. v1 and v2 **co-exist** until Stage 3 — we need to diff
them on real traffic to justify the promotion.

| Stage | Duration | Behavior |
|---|---|---|
| **0 — v1-only** | Shipped 2026-04-18 (PR #39). | Textual counter only; no semantic metric; no log changes. |
| **1 — log vectors** | ≥2 weeks | Hook-side change only: log `hit_ids` + `prompt_embed_b64` on `stage=ok`. No semantic-metric compute. Goal: accumulate replay data. Verify: `hooks tail --event=user-prompt --since=24h \| grep prompt_embed_b64` non-empty; log stays under cap. |
| **2 — log-only semantic metric** | ≥2 weeks | `sessions report` computes `semantic_drift` from Stage-1 logs and emits the block. Both v1 and v2 counts present. Goal: hand-audit 20 divergences (v1 flagged, v2 didn't, or vice versa) to drive §5.3 calibration. |
| **3 — promote v2, deprecate v1** | Schema bump | Drop `redundant_heimdall_calls`. Bump `schema_version` to `"v2"`. Keep v1 as a handoff migration hint for one release. |

**Why co-existence matters:** if v2 shows `semantic_redundant_calls=3`
where v1 showed `redundant_heimdall_calls=6`, the delta is the model's
*genuinely novel* heimdall queries v1 mis-flagged. Without
co-existence there's no way to compute that delta.

**Promotion criteria — Stage 1 → 2:**
- Hook log size stays under cap over 14 days (3 KB × ~50
  prompts/day × 14 ≈ 2 MB, well under 5 MB cap).
- No panics / parse regressions in `hooks tail` with new keys.
- Dry-fire `sessions report` against a logged session returns a
  non-null `semantic_drift` block validating §6.1.

**Promotion criteria — Stage 2 → 3:**
- §5.3 bake-off shows ≥85% precision on both metrics at chosen T1/T2.
- Divergence audit shows v2 more often correct than v1 on hand-
  labeled turns (majority vote from the §5.3 sample).
- Explicit human decision to deprecate v1. Not implicit, not
  time-based — someone signs off.

**Rollback:**
- 1 → 0: stop writing new log keys. Old logs remain harmless.
- 2 → 1: `--no-semantic-drift` flag on `sessions report`.
- 3 → 2: revert schema bump + re-emit v1 key. Only stage with real
  schema implications — which is why it's last and explicit.

---

## 11. Extension Points (post-v1)

- **Per-turn explainability.** `--verbose` on `sessions report`
  dumps per-turn `(query, best-sim, verdict)` rows. Useful for
  calibration sample and for users surprised by a score. See §9
  OQ-10.
- **Per-project thresholds.** §9 OQ-7: `cfg.SemanticDriftT1` /
  `T2` fall through to global constants when zero.
- **Live shadow scoring.** PostToolUse(`heimdall_search`) hook could
  score drift live and surface a soft `## Heimdall note` back to
  the model. Not on the roadmap; flagged so design doesn't
  foreclose it. Same framing as 08 §10.
- **Bench-retrieval integration.** Drift-regression mode that
  replays a fixed query set and flags if chosen T1/T2 re-classify
  calls under a new embedding model — long-term corpora-drift
  guard.

---

## 12. Summary

- Two metrics under `tool_use.semantic_drift`:
  - `semantic_redundant_calls`: hit-injected turn + assistant
    `heimdall_search` query vector ≥ T1 cosine vs. best hit vector.
  - `missed_call_opportunities`: zero heimdall calls + prompt vector
    ≥ T2 cosine vs. best indexed chunk.
- **T1=0.55, T2=0.45** starting, calibrated via 100-turn labeled
  sample from ≥2 weeks of Stage-1 data. Precision-first (≥85%).
  Thresholds pinned + echoed in report JSON for reproducibility.
- **Post-hoc compute** in `sessions report`. Hook side only
  learns two new log keys (`hit_ids`, `prompt_embed_b64`) on
  `stage=ok` — ~3 KB per prompt, <1 ms cost.
- **Fail-open everywhere** — any F1–F9 failure serializes
  `semantic_drift: null` with a diagnostic key; never errors
  `sessions report`. Cost bounded to ≤30 s worst case.
- **Additive JSON shape**, `schema_version` stays `"v1"` until
  Stage 3 drops the v1 textual counter.
- **Rollout: log-vectors → log-only-metric → promote-and-deprecate.**
  v1/v2 co-exist until the bake-off is done.
- **Tests all stubbable:** FakeEmbedder for Layer 1 (20 cases),
  httptest for Layer 2 (4 cases), no live Ollama in CI.
- **Non-goals:** not a retrieval-quality evaluator, not user-intent
  modeling, not a second retrieval engine. We score
  embedding-cosine against the corpus we have; labeling whether
  that was the *right* answer is a human's job.
- **Defer via §9:** cache-hit prompt vectors, recall-vs-search
  boundary, calibration harness binary, scope-aware replay,
  per-corpus thresholds, query normalization specifics, metric
  names, verbose per-turn output.
