# 12a — Design Decisions for Plan 12 (Semantic-Drift Metric) + `cmd/calibrate-drift`

**Status:** Design-only. Resolves the 10 open questions in §9 of
`docs/plans/hooks/12-semantic-drift-metric.md` and specifies the
calibration harness (`cmd/calibrate-drift`) that gates the Stage 2→3
rollout. No code lands from this doc — the next session starts
implementation of Stage 1 logging + the harness skeleton.

**Audience:** implementer who will ship the Stage-1 hook-side logging
change, the `internal/heimdall/semantic_drift.go` computation, and
`cmd/calibrate-drift`.

**Reading order:** §1 recap → §2 OQ resolutions (OQ-1..10) → §3 OQ-3
deep-dive (calibration harness) → §4 Stage-1 integration plan → §5
implementation punch list → §6 needs human decision.

---

## 1. Recap — what plan 12 already pinned

| Decision | Value | Source |
|---|---|---|
| Two metrics, orthogonal | `semantic_redundant_calls`, `missed_call_opportunities` | plan 12 §2 |
| Compute placement | Post-hoc inside `sessions report` | plan 12 §4 |
| Starting thresholds | T1=0.55 (redundant), T2=0.45 (missed) | plan 12 §5.2 |
| Fail-open | All F1..F9 → `semantic_drift: null` + diagnostic key | plan 12 §7 |
| Rollout | Stage 0 (v1 shipped) → 1 (log vectors) → 2 (compute metric) → 3 (promote, bump schema) | plan 12 §10 |
| Quality bar | ≥85% precision on both metrics (precision-first, not F1) | plan 12 §5.3 |
| Schema posture | Additive under `tool_use.semantic_drift`; `schema_version="v1"` until Stage 3 | plan 12 §6.3 |

§2 below resolves the 10 questions §9 left open. §3 designs the
harness plan 12 §5.3 assumed but did not specify. §4 details the
Stage-1 log-only change so it can land independently of the metric
compute.

---

## 2. Open-question resolutions

Format per question: **Question / Industry prior-art / Recommendation /
Rationale / Tradeoffs**.

### OQ-1 — Cache-hit prompt vectors

**Question:** plan 12 §3.5 — cache-hit turns have no `prompt_embed_b64`
in `hooks.log` because the embed step is skipped. Do we expand
`hook_cache` to carry the pvec, or accept cache-hit turns drop from
the semantic-drift denominator?

**Prior art:** The Anthropic
[prompt-caching guide](https://docs.claude.com/en/docs/build-with-claude/prompt-caching)
treats cache as a cost-optimization on repeat inputs — it does not
advocate double-storing derived state to support auxiliary analytics.
Retrieval-eval systems (e.g. BEIR runs in Thakur et al. 2021,
[arXiv:2104.08663](https://arxiv.org/abs/2104.08663)) routinely
exclude from the denominator any query that did not produce a
comparable measurement, with reporting of the excluded fraction. The
precedent is "report the denominator, don't fabricate data."

**Recommendation:** Accept the drop (option 2). Track `turns_skipped`
in the JSON output and split it by reason via a sub-map:

```json
"turns_skipped": 5,
"turns_skipped_by_reason": {
  "cache_hit": 3,
  "no_prompt_embed": 1,
  "dim_mismatch": 1
}
```

Soak for two weeks on `stage=ok`-only data. Only revisit (expand
`hook_cache` to carry the 3 KB pvec) if `cache_hit` dominates
`turns_skipped` **and** the fraction exceeds 30% of total turns in
sampled real sessions.

**Rationale:** The hook_cache schema is shared code surface —
a change there ripples to cache eviction, TTL, doctor checks,
`hooks cache-stats`, the 32 KB oversize refusal in Wave 1 T10. For a
metric whose headline precision target is ≥85%, a coarse censoring of
cache-hit turns is fine **if** we report the censoring rate. The
by-reason sub-map makes the censoring visible; if it's pathological
we'll see it and can promote the schema change on evidence.

**Tradeoffs:**

- **Accept drop (chosen):** zero schema change, zero cache-eviction
  reasoning. Downside: metric under-reports missed opportunities on
  repeat-question sessions. Repeat-questions are exactly where
  `missed_call_opportunities` is *least* interesting (the cached hit
  *did* serve context; it just can't be re-scored).
- **Expand hook_cache:** accurate denominator, 3 MB worst-case
  storage, schema + eviction + doctor-check changes for a metric
  that's not yet proved out.

### OQ-2 — `heimdall_recall` in the redundant bucket

**Question:** plan 12 §2.2 excludes `heimdall_recall`,
`heimdall_expand`, `heimdall_ls` from the redundant-call cosine check.
Does that leak a real redundancy path?

**Prior art:** Retrieval-evaluation harnesses (MTEB, Muennighoff
et al. 2022, [arXiv:2210.07316](https://arxiv.org/abs/2210.07316))
score only the retrieval function whose output is comparable to the
query — they do not mix different retrieval primitives (e.g. BM25
and dense) into the same cosine score.

**Recommendation:** Keep exclusion. Ship a **sibling counter**
`heimdall_non_search_when_hits_present` in the same `semantic_drift`
JSON block (purely textual — "the hook injected hits and the model
used `heimdall_recall`/`heimdall_expand`/`heimdall_ls`"). Do **not**
fold this into the cosine metric.

**Rationale:**

- `heimdall_recall` hits the **memory** vector space (skills,
  decisions, facts) — a different embedding domain than the code-
  index chunks in `VectorRecord`. A cosine between a code chunk and a
  memory query has no defined meaning.
- `heimdall_expand(chunk_id)` embeds an **id string**. That's
  semantically vacuous.
- `heimdall_ls` is a path browse. Not retrieval.

A textual companion counter captures the "model kept poking heimdall
despite hits being injected" signal without polluting the core metric.

**Tradeoffs:** risk of noise — recall-after-hits is often legitimate
(e.g. the model looks up a project decision *because* a hit mentioned
it). We don't treat the counter as a deficit, just exposure.

### OQ-3 — Calibration harness

See **§3 OQ-3 deep-dive** below. Summary recommendation: ship
`cmd/calibrate-drift` as a sibling of `cmd/bench-retrieval`, reading
an operator-maintained labeled JSONL, replaying vectors via the same
hooks-log + store primitives `ComputeSemanticDrift` uses, and
emitting a precision/recall CSV across a T1×T2 grid.

### OQ-4 — "Did the model use heimdall?" for missed-call exclusion

**Question:** plan 12 §2.3 excludes from `missed_call_opportunities`
any turn where the model made "zero heimdall_* calls." Is that the
right granularity?

**Prior art:** In retrieval-augmented generation literature
([Lewis et al. 2020](https://arxiv.org/abs/2005.11401);
[Asai et al. 2023 Self-RAG](https://arxiv.org/abs/2310.11511))
"consulted retrieval" is treated atomically per turn, not per-tool.
The semantically honest reading is "did the model attempt to retrieve
anything." Splitting that into tool-subfamilies is a false precision.

**Recommendation:** Keep the lenient reading — **any** `heimdall_*`
tool call in the turn disqualifies it from the missed bucket.

**Rationale:** We do not have ground truth on whether the model chose
the right primitive. Second-guessing the tool choice on cosine alone
mis-uses the signal. If the model ran `heimdall_ls` instead of
`heimdall_search`, that's a UX/routing question, not a drift
question.

**Tradeoffs:** we'll undercount some genuinely-missed opportunities
(model used `heimdall_ls`, should have used `heimdall_search`). OQ-2's
companion counter `heimdall_non_search_when_hits_present` catches the
"hits present + model reached for non-search" case; the uncovered
shape "no hits + model used non-search" is the blind spot, and it's
rare enough that the false-positive risk from flagging these as
"missed" outweighs the recall gain.

### OQ-5 — Scope replay

**Question:** plan 12 §7.2 — hook-time scope filtering (from PR #17)
is not replayed in the missed-bucket search. Over-counts missed
opportunities in scope-constrained sessions.

**Prior art:** search-eval frameworks (TREC, MS-MARCO) keep the
evaluation environment **identical** to the production environment or
they report the mismatch as a caveat. The
[MS-MARCO eval README](https://microsoft.github.io/msmarco/)
warns against running eval on a different corpus than production.

**Recommendation:** Add a `--scope-aware` flag to `sessions report`,
**default off** in v1 of the metric. Default-on in v2 once the
scope-aware code path has been soaked.

**Rationale:**

- Default-off matches plan 12 §7.2's explicit "over-count is
  acceptable in v1" position — over-counting missed is a safer failure
  mode than under-counting (false-positives on `missed` say "heimdall
  *could* have helped"; gentler claim than `redundant`'s "heimdall
  *did* help but you ignored it").
- Default-on v2 requires `scope` to be persisted alongside
  `prompt_embed_b64` in the hook log (one more field). Cheap, but
  additional to Stage 1.

**Tradeoffs:** two phases instead of one. Pays off because v1 of the
metric stays simple; v2 graduates only after calibration has baselined
the un-scoped numbers.

### OQ-6 — Confidence-bucket width

**Question:** plan 12 §5.5 uses `T+0.05` for the "near_threshold_*"
bucket. Arbitrary.

**Prior art:** Threshold-sweep papers in retrieval evaluation (e.g.
[Formal et al. 2021, SPLADE](https://arxiv.org/abs/2109.10086))
typically use step sizes of 0.05 when sweeping cosine thresholds on
dense retrievers. Matches the folk "5% is a humanly distinguishable
similarity gap" heuristic.

**Recommendation:** Ship `NearThresholdDelta = 0.05` as an exported
constant in `internal/heimdall/semantic_drift.go`. Revisit after the
OQ-3 bake-off — if the hand-labeled sample shows the distribution of
"borderline" similarities is tighter (e.g. 0.02 median gap), narrow
the bucket in a follow-up PR.

**Rationale:** matches §5.3's precision-first framing — we calibrate
once, then treat constants as frozen until the next bake-off.

**Tradeoffs:** none — the value is echoed in JSON, so downstream
consumers tolerate changes.

### OQ-7 — Per-corpus thresholds

**Question:** plan 12 §9 — different corpora → different natural
similarity distributions. Do we expose per-project overrides?

**Prior art:** Embedding-eval frameworks (MTEB, BEIR) distribute
**per-dataset calibrated thresholds** because corpus-level
distributions shift meaningfully (Thakur et al. 2021 show up to 0.15
difference in optimal cosine between BEIR splits). However, when the
embedding model is fixed (as here: `nomic-embed-text`), the
*relative* shape of the distribution is stable across corpora even if
the absolute median shifts — matching
[nomic-ai/nomic-embed-text-v1.5 model card](https://huggingface.co/nomic-ai/nomic-embed-text-v1.5)
where "similarity ≥ 0.5 is a meaningful threshold" is reported as
corpus-independent.

**Recommendation:** Keep global constants for v1. Do **not** ship
a configure-surface in v1. If a user reports miscalibration against
their corpus post-v1, add `heimdall-mcp configure
--semantic-drift-t1 --semantic-drift-t2` in v2.

**Rationale:** adds a surface we may not need. The metric is already
behind a 3-stage rollout; we should see if the calibration sample
(§3 OQ-3) is representative across the dogfood indexes before
promoting a per-project knob.

**Tradeoffs:** if a user's corpus has systematically different
distributions (e.g. a Chinese-language codebase with
`nomic-embed-text`, where cross-lingual cosine is known to shift),
they'll see noisy output with no knob. Mitigated by documenting the
threshold values and the calibration method so advanced users can
fork + repin.

### OQ-8 — Query-text normalization

**Question:** plan 12 §9 — `EmbedForHook` may normalize/clean the
prompt before embedding. Assistant `heimdall_search` queries from the
transcript may have different shapes. Do we match the hook's
normalization on the re-embed path?

**Prior art:** Dense-retrieval practice (DPR, Karpukhin et al. 2020,
[arXiv:2004.04906](https://arxiv.org/abs/2004.04906)) is emphatic
that "query at eval must be encoded identically to query at index."
Any pre-processing skew silently biases the metric.

**Recommendation:** Audit at implementation. **Do not design around a
presumed normalization we can't verify.** Today
`hookEmbedder.Embed()` calls `OllamaClient.Embed(ctx, model, prompt)`
directly (plan 12 §3.1, `EmbedForHook` wrapper); the assistant query
goes through the same `EmbedBatch` path. If and only if the
implementer finds a normalization helper on the hook side (e.g.
`strings.TrimSpace` before embed), they extract it into a shared
`internal/heimdall.NormalizeForEmbed(s string) string` helper and
call it on both sides.

Document the finding in the implementation PR description. If no
normalization exists, say so explicitly so the next reader doesn't
have to re-audit.

**Rationale:** DRY over a presumed helper would be premature. The
current hook path has no obvious normalization — the cache-key
normalization (`normalizePromptForCache`, lowercase + whitespace
collapse) is applied to the **cache key**, not the embed input, per
the function doc:

> `// The normalization is deliberately aggressive so "What does X do?" and`
> `// "what does x do?   " collide on the same cache row.`

So the current state is: no embed-side normalization. The re-embed
path already matches by inheritance. Implementer should confirm, not
assume.

**Tradeoffs:** if the implementer discovers silent normalization (e.g.
a `.TrimSpace` inside Ollama go-bindings we didn't see), the fix is
trivial: a single-call helper + call it on both sides. Catching it
once during implementation is cheaper than debugging drift in Stage 2.

### OQ-9 — Metric names

**Question:** plan 12 §9 — `semantic_redundant_calls` vs. shorter
`drift_wasted`; `missed_call_opportunities` vs. `drift_missed`.

**Prior art:** Prometheus metric-naming conventions
([prometheus.io/docs/practices/naming](https://prometheus.io/docs/practices/naming/))
prefer descriptive plural nouns over cryptic abbreviations. OpenTelemetry
[semantic conventions](https://opentelemetry.io/docs/specs/semconv/)
name metrics "what they count, as a sentence."

**Recommendation:** Keep the long forms — `semantic_redundant_calls`
and `missed_call_opportunities`. Also keep the v1 `redundant_heimdall_calls`
name at Stage 3 deprecation rather than renaming.

**Rationale:**

- Long form self-documents. A user reading the JSON sees what the
  number means without cross-referencing docs.
- "Redundant" matches v1 vocabulary (the existing
  `redundant_heimdall_calls` key), so the v1↔v2 co-existence in
  Stages 2–3 is legible: "oh, v2's `semantic_redundant_calls` is the
  semantic-aware version of v1's `redundant_heimdall_calls`."
- Shorter names would be a gratuitous rename with real breakage:
  anyone grepping session reports by key would have to re-learn.

**Tradeoffs:** slightly wider JSON. Negligible.

### OQ-10 — Per-turn verbose output

**Question:** plan 12 §9 — per-turn similarity rows are useful for
debugging but noisy in the report headline.

**Prior art:** `cmd/bench-retrieval` already operates under a
`--format=text|json` split (see `main.go:122`). The convention in
this codebase is "text = human summary; json = machine detail." A
`--verbose` flag on top of `sessions report` is orthogonal.

**Recommendation:** Add `--verbose` on `sessions report`. Verbose mode
emits **per-turn similarity rows** in both text and JSON.

```
## Semantic drift (per-turn, verbose)
  turn 14  query="why did vectors.db grow"  sim=0.62  T1=0.55  → REDUNDANT
  turn 23  no heimdall call  best-pvec-sim=0.48  T2=0.45  → MISSED
  turn 27  no heimdall call  best-pvec-sim=0.31  T2=0.45  → below threshold
```

JSON under a new key:

```json
"semantic_drift": { ..., "per_turn": [{...}, {...}] }
```

Off by default. Gated by `--verbose`.

**Rationale:** calibration (§3 OQ-3) and user-facing debugging both
need per-turn data. Keeping it behind a flag preserves the headline's
signal-to-noise.

**Tradeoffs:** verbose JSON is large (one object per turn × hundreds
of turns). Acceptable — `sessions report` is an explicit human-run
command, not a streaming endpoint.

---

## 3. OQ-3 deep-dive — `cmd/calibrate-drift` harness design

### 3.1 Problem framing

Plan 12 §5.3 specified a **100-turn hand-labeled sample** as the
calibration dataset. Three things were not specified:

1. How the sample is collected (which sessions, which turns).
2. What the labeling tool looks like.
3. How the precision/recall sweep is operationalized — what
   commands to run, what artifacts drop, what the
   Stage-2→3 gate actually evaluates against.

This section closes those gaps.

### 3.2 Data collection strategy

**Source of ground-truth labels:** the operator running the bake-off
(human, offline) labels a sample drawn from real Stage-1 data. No
synthetic data — synthetic samples would bake our own priors into the
calibration and give us a precision-recall curve that looks nothing
like production.

**Sampling protocol:**

1. After ≥2 weeks of Stage-1 logging, run
   `heimdall-mcp sessions list --since=336h --format=json` to
   enumerate all sessions with `prompt_embed_b64` data.
2. Filter sessions with ≥10 `user-prompt stage=ok` events to avoid
   trivial test sessions.
3. **Stratified random sample**: from each session, select
   at most 5 turns; across the corpus draw 100 turns total, with a
   **50/50 split of "v1-flagged-redundant" vs. "v1-unflagged."**
   Rationale per plan 12 §5.3 — tests both sensitivity and
   specificity.
4. Persist the sample as `calibration_sample.jsonl`, one turn per
   line, schema (NEW — not in plan 12):

   ```json
   {
     "session_id": "...",
     "turn_idx": 14,
     "prompt": "...",
     "injected_hit_ids": ["id1", "id2"],
     "heimdall_search_queries": ["..."],
     "heimdall_tools_used": ["heimdall_search"],
     "v1_flagged": true,
     "label": null
   }
   ```

**Labeling protocol:**

1. Human reads the `prompt`, `injected_hit_ids` (rendered with a
   helper that resolves them to `file:lines — summary`), and
   `heimdall_search_queries`.
2. Writes one of four labels into `label`:
   - `"truly_redundant"` — injected hits really did cover the
     question and the model's search was wasted.
   - `"truly_missed"` — model made zero heimdall calls but the
     corpus had something directly relevant.
   - `"neither"` — legitimate novel query, or trivially-unrelated
     turn.
   - `"unclear"` — human can't tell. Must be <10% of sample; if
     >10%, re-draw.

Labeling target: **~3 minutes/turn × 100 turns ≈ 5 hours of
operator time.** Real cost; budgeted into the Stage 2→3 gate as a
one-time investment.

### 3.3 Metric definition — precision/recall targets

Given hand labels:

| Predicted | Label truly_redundant | Label neither |
|---|---|---|
| `semantic_redundant_calls` incremented | TP | FP |
| Not incremented | FN | TN |

For the redundant bucket:
- **Precision** = TP / (TP + FP)
- **Recall** = TP / (TP + FN)

Mirror the matrix for `missed_call_opportunities` against the
`truly_missed` label.

**Target (plan 12 §5.3 restatement):**
- Both metrics ≥85% precision.
- Recall ≥50% for missed (looser — under-counting missed is
  cheaper than over-counting).
- Recall ≥70% for redundant (stricter — the counter is the headline
  signal, so it needs to catch most of the cases).

**Rationale for asymmetric recall targets:** over-calling `missed`
flags "heimdall could have helped here" which is a soft nudge; over-
calling `redundant` accuses the model of wasted work, which is
loud. Plan 12 §5.2 already uses T1 > T2 for the same asymmetry; we
extend it to the recall target.

### 3.4 Harness CLI design

Model it on `cmd/bench-retrieval` — same conventions: `--db` auto-
detect, `--format=text|json`, deterministic given inputs.

```
calibrate-drift --labels=<path>     # calibration_sample.jsonl (required)
                [--hooks-log=<path>] # default: $XDG_STATE_HOME/heimdall/hooks.log
                [--db=<path>]        # default: FindRepoRoot/.heimdall_db/<model>/
                [--t1-min=0.40 --t1-max=0.70 --t1-step=0.05]
                [--t2-min=0.35 --t2-max=0.55 --t2-step=0.05]
                [--format=text|csv|json]  # default text; csv for spreadsheet work
                [--ollama=http://localhost:11434]
                [--model=nomic-embed-text]
                [--output=<path>]    # default stdout
```

**What it does:**

1. Parses `--labels` JSONL.
2. Opens the DB + hooks-log just like `ComputeSemanticDrift` does
   (same helpers, same `VectorByID` lookups, same post-hoc
   `EmbedBatch` for assistant queries).
3. For each `(T1, T2)` in the grid, runs the full drift computation
   over the labeled sample, then diffs predictions against labels to
   produce:

   ```
   T1,T2,precision_redundant,recall_redundant,f1_redundant,
         precision_missed,recall_missed,f1_missed,
         near_t1,near_t2,n_turns_scored
   ```

4. Text mode prints a human-readable top-10 of "best (T1,T2)
   combinations by P(redundant)+P(missed)."
5. CSV mode dumps the full grid — this is the artifact a spreadsheet
   user pivots.
6. JSON mode dumps the full grid + grid metadata (label counts,
   skipped turns, etc.) — this is what a follow-up visualizer
   consumes.

**Deterministic given inputs:** same corpus, same sample,
same hooks.log, same grid → same output bytes. Smoke-testable with
a fixture sample + stub store + fake Ollama via `httptest` (same
pattern as `cmd/bench-retrieval/bench_test.go`).

**Not in scope for the harness:**

- The labeling UI. Labeling is a text-editor exercise — operator
  opens `calibration_sample.jsonl` in `$EDITOR`, fills in `label` per
  turn. Zero tooling burden.
- Cross-corpus calibration. The harness calibrates against whatever
  single DB is pointed at.
- Automatic threshold picking. The harness reports the grid; a human
  picks the pair and updates the constants in `semantic_drift.go`.

### 3.5 Stage 2→3 gate — what the harness evaluates against

Plan 12 §10 ("Promotion criteria — Stage 2 → 3"):

> - §5.3 bake-off shows ≥85% precision on both metrics at chosen T1/T2.
> - Divergence audit shows v2 more often correct than v1 on hand-
>   labeled turns (majority vote from the §5.3 sample).
> - Explicit human decision to deprecate v1. Not implicit, not
>   time-based — someone signs off.

Operationalization:

1. Operator runs
   `calibrate-drift --labels=calibration_sample.jsonl --format=csv > grid.csv`.
2. Picks the (T1, T2) row from grid.csv with
   `precision_redundant >= 0.85 AND precision_missed >= 0.85` and
   maximum `f1_redundant + f1_missed`.
3. Runs `calibrate-drift` once more with fixed `--t1 --t2`
   (single-point mode; same flags, zero-width grid) and `--format=json`
   to produce a **go-no-go artifact**
   `calibration_result_<date>.json`:

   ```json
   {
     "labels_sha256": "...",
     "db_sha256": "...",
     "t1": 0.55,
     "t2": 0.45,
     "embedding_model": "nomic-embed-text",
     "precision_redundant": 0.89,
     "recall_redundant": 0.71,
     "precision_missed": 0.86,
     "recall_missed": 0.58,
     "divergence_v1_correct": 12,
     "divergence_v2_correct": 27,
     "divergence_tie": 3,
     "timestamp": "2026-05-05T12:00:00Z"
   }
   ```

4. Operator commits the `calibration_result_*.json` to the repo
   (under `docs/plans/hooks/calibration/`) as the
   **audit-trail artifact** plan 12 §10 requires for "Explicit human
   decision to deprecate v1."
5. Stage 3 PR bumps `schema_version` to `"v2"`, drops
   `redundant_heimdall_calls`, and cites the artifact in the
   commit message.

**Divergence audit** is computed by the harness: count turns where
v1 and v2 disagreed, partition by which one matched the hand label.
Tie is "both correct or both wrong." The numbers sit in the JSON
artifact and feed the commit message.

### 3.6 Staged answer when production data is thin

Per the task brief — if OQ-3 can't be fully specified without
production data, give a staged answer:

**v1 of the harness (ship alongside Stage 2 of plan 12 rollout):**
- 100-turn sample target; accept as low as 50 if the 2-week data
  window didn't produce 100 v1-flagged turns.
- Grid: T1 ∈ {0.40, 0.45, 0.50, 0.55, 0.60, 0.65, 0.70},
        T2 ∈ {0.35, 0.40, 0.45, 0.50, 0.55}. 35 pairs.
- Labeling is single-human. No inter-annotator agreement check.

**Triggers to re-run / re-design:**
- If sample size < 50 after 2 weeks → extend Stage 1 by two more
  weeks before running the bake-off. Do not compensate with synthetic
  data.
- If single-human labeler runs into >10% `"unclear"` labels → escalate
  to two-labeler with majority-vote disambiguation; doc the
  disagreement rate in the artifact.
- If the best-precision row in the grid has precision < 85% → the
  embedding model itself isn't discriminative enough on our corpus;
  escalate to "re-evaluate `nomic-embed-text` vs.
  `nomic-embed-text-v1.5` / `mxbai-embed-large`" before pushing any
  threshold into production.

### 3.7 Harness file layout

```
cmd/calibrate-drift/
  main.go                     # CLI entry point; ~200 lines, mirrors bench-retrieval
  calibrate_test.go           # httptest-fake-Ollama Layer 2 tests
  labels_parse_test.go        # JSONL parser unit tests
  fixtures/
    small_sample.jsonl        # 5-turn fixture for smoke tests
    expected_grid.csv         # deterministic expected output
```

Re-used from `cmd/bench-retrieval`:

- `heimdall.FindRepoRoot` for `--db` auto-detect.
- `heimdall.ModelDBDir` for model resolution.
- The `snapshotDB` helper (move to a shared location or duplicate —
  implementer's call; current practice is duplicate, bench-retrieval's
  is private to its package).
- `--format` dispatch pattern.

---

## 4. Stage-1 integration plan — how log-only rollout lands

Stage 1 is the **only** pre-Stage-2 change. It is write-only, additive,
no behavior change. Lands as a standalone PR independent of the
metric compute.

### 4.1 Change summary

- **File:** `internal/cli/hook_user_prompt.go`
- **Scope:** the `stage=ok` log emission at line ~331.
- **New keys added to the log map:**
  - `hit_ids`: comma-joined `chunk_id`s of the injected hits.
  - `prompt_embed_b64`: base64-encoded 3072-byte little-endian float32
    block from `heimdall.EncodeFloat32Vec(queryVec)`.

### 4.2 Patch-shape (pseudocode — do not copy verbatim)

Before the existing `logHookEventWithSession(...stage=ok...)` call,
build the two new fields:

```go
// Build the hit_ids list — comma-joined for log friendliness.
hitIDs := make([]string, 0, len(results))
for _, r := range results {
    hitIDs = append(hitIDs, r.Record.ID)
}

// Encode the prompt vector. Cheap: 3 KB per call,
// <1 ms. See plan 12 §3.2.
promptEmbedB64 := base64.StdEncoding.EncodeToString(
    heimdall.EncodeFloat32Vec(queryVec),
)

logHookEventWithSession("INFO", "user-prompt", sessionID, map[string]any{
    "stage":            "ok",
    "hits":             len(results),
    "skills":           len(skillBullets),
    "bytes":            len(body),
    "model":            resolvedModel,
    "scope":            scope,
    "hit_ids":          strings.Join(hitIDs, ","),
    "prompt_embed_b64": promptEmbedB64,
})
```

### 4.3 Why no `schema_version` bump

Plan 12 §6.3 and PR #39 precedent: additive fields do not bump
`schema_version`. `hooks.log` is a keyed line format (not JSON), but
the same policy applies — existing consumers (`hooks tail`,
`ReadHookLog`, `AggregateHookLogBySession`) **ignore unknown keys**
because the parser (`parseHookLogLine` in `hooklog_reader.go:39`)
already emits a generic `Fields map[string]string`. New keys land as
additional map entries. Consumers of specific keys keep working.

**Verification at implementation:**
- `hooks tail` rendering: confirm `--event=user-prompt` shows the two
  new keys as extra trailing fields in the tail output.
- `AggregateHookLogBySession`: unchanged — it reads
  `Fields["stage"]` and `Fields["bytes"]`, both already defined.
  New keys are ignored.
- `hooks audit-guardrails`: unchanged — it reads
  `event=pre-tool-use`; doesn't touch user-prompt events.
- `hooklog_reader_test.go`: existing round-trip tests should all
  pass. Add a new case `TestParseHookLogLine_WithPromptEmbed` that
  asserts the parser round-trips a long base64 value through
  `strconv.Quote/Unquote` (PR #49 already made that lossless).

### 4.4 Size budget

- 3 KB per prompt `prompt_embed_b64`.
- ~30 chars per `hit_ids` value (≤5 hit ids × 6 chars each).
- At 50 prompts/day × 14 days ≈ 2 MB of new log bytes over the
  Stage 1 soak window.
- 10 MB hooks.log cap with 1 rotation = 20 MB total ceiling (cap
  bumped from 5 MB to 10 MB in HD-5, 2026-04-18).
- ~10–12% of total budget. Well under cap.

**Telemetry to watch** (plan 12 §10 Stage 1 promotion criteria):

- `ls -la $XDG_STATE_HOME/heimdall/hooks.log*` over the 14-day window —
  size should stay under 10 MB + one rotation.
- `hooks doctor` — no new warnings introduced.
- `hooks smoke --fake-ollama` — PASS on all 6 hooks after the Stage 1
  change.

### 4.5 Rollback

Per plan 12 §10 rollback table:

- Stage 1 → Stage 0: revert the PR. Existing log lines with the new
  keys are harmless to rollback readers. No schema implications.

---

## 5. Implementation punch list

Hand-off to the next session. Ordered for minimum-coupling; every
item has a clear DoD and a verification step.

### Stage 1 — hook-side logging (Wave F, ship first)

1. **Add `hit_ids` + `prompt_embed_b64` to `stage=ok` log.**
   - File: `internal/cli/hook_user_prompt.go`.
   - Add import: `encoding/base64`, `strings`.
   - DoD: log line at `stage=ok` includes both keys; no other stages
     emit them; `hooks.log` round-trips through `ReadHookLog`.
   - Verify: `go test ./internal/cli/... -run HookUserPrompt -race`
     passes; `hooks smoke --fake-ollama` PASSes.

2. **Extend `hooklog_reader_test.go` round-trip cases.**
   - Add `TestParseHookLogLine_WithPromptEmbed`: constructs a
     synthetic stage=ok line with a 4 KB base64 payload, parses,
     asserts fields preserved.
   - Verify: `go test ./internal/heimdall/... -run HookLogLine -race`.

3. **Smoke-fire via `hooks smoke --fake-ollama`.**
   - Expect PASS on user-prompt event.
   - No new WARN/ERROR lines.

### Stage 2 — metric compute (separate PR, after Stage 1 has 2+ weeks of data)

4. **Add `VectorByID` helper to `VectorStore`.**
   - File: `internal/heimdall/store.go`.
   - Signature: `VectorByID(chunkID string) ([]float32, error)`.
   - Select the `embedding` column by id, decode with
     `DecodeFloat32Vec`.
   - DoD: returns vec on known id, `sql.ErrNoRows` on unknown.
   - Verify: add `TestVectorByID_*` cases to `store_test.go`.

5. **Extend transcript parser to capture `heimdall_search` input.**
   - File: `internal/heimdall/transcript.go`.
   - In `applyAssistantMessage`, when `tool_use.Name ==
     "mcp__heimdall__heimdall_search"`, capture `input.query` into a
     new `TranscriptSummary` field
     `HeimdallSearchQueries []struct{TurnIdx int; Query string}`.
   - DoD: struct populated from `testdata/transcript_heimdall.jsonl`.
   - Verify: update `transcript_test.go`.

6. **Implement `ComputeSemanticDrift`.**
   - File: `internal/heimdall/semantic_drift.go` (new).
   - Signature per plan 12 §4.3.
   - Dependency injection for store + embedder.
   - 20 Layer-1 tests per plan 12 §8.1 (SD1..SD20).
   - 4 Layer-2 tests per plan 12 §8.2 (SD-L2-1..SD-L2-4).
   - DoD: all 24 tests green under `-race -count=1`; fail-open on
     F1..F9.
   - Verify: no net new `go vet` or lint issues.

7. **Wire into `SessionReport`.**
   - File: `internal/cli/sessions.go`.
   - Emit the `semantic_drift` block in JSON and the trailing
     `semantic_drift:` line in text mode, per plan 12 §6.
   - Echo `threshold_t1`, `threshold_t2`, `embedding_model`,
     `threshold_version`.
   - Add `turns_skipped_by_reason` sub-map per OQ-1.
   - Add `heimdall_non_search_when_hits_present` sibling counter per
     OQ-2.
   - DoD: `TestSessionsReport_JSONSchemaVersion` still passes (no
     bump); `TestRender_JSONSchema` (new) pins the shape.

8. **Add `--verbose` flag to `sessions report`.**
   - Per OQ-10.
   - Flag parses, emits per-turn rows.
   - DoD: `--verbose` default off; verbose JSON has stable key order.

### Harness — ships alongside Stage 2 compute (same or following PR)

9. **Ship `cmd/calibrate-drift`.**
   - File: `cmd/calibrate-drift/main.go` (new).
   - Signature per §3.4.
   - Reuse `FindRepoRoot`, `ModelDBDir`, snapshotDB pattern.
   - Layer-2 tests with fixture + httptest Ollama.
   - DoD: `go test ./cmd/calibrate-drift/... -race` green;
     deterministic output on fixture.

10. **Add `make calibrate-drift-smoke` target.**
    - Runs the harness against the fixture sample + stub embedder.
    - Expected output matches `fixtures/expected_grid.csv` byte-for-
      byte.

### Stage 3 — after calibration artifact is committed

11. **Promote: drop v1 counter, bump schema_version.**
    - File: `internal/cli/sessions.go` — remove
      `redundant_heimdall_calls` from JSON/text output.
    - `SessionsReportSchemaVersion = "v2"`.
    - Commit message cites the `calibration_result_*.json` artifact.
    - DoD: one full session-report cycle on dogfood DB emits v2 JSON
      without v1 key.

---

## 6. Needs human decision

Items where no amount of codebase reading resolves the question —
plus a recommendation the human can ratify or reject.

1. **Operator committing to ~5 hours of labeling.** The OQ-3 harness
   is ineffective without a hand-labeled sample. Operator time is
   the scarce resource, not code.
   - *Recommendation:* commit to the labeling at the Stage 2 shipping
     point. If that's a hard no, defer Stage 3 indefinitely — v1 and
     v2 co-exist in perpetuity. Not a correctness problem, just an
     unclear state.

2. **Whether the Stage 1 hit_ids + prompt_embed_b64 keys should
   land now, or gate on a retention decision.** The design says
   "~2 MB over 14 days — under cap." The cap is shared with every
   other hook event. If you've got plans for another verbose event
   stream in that cap, this eats room.
   - *Recommendation:* ship now. Re-litigate if `hooks.log` cap starts
     rotating more than once per 14 days.

3. **Who owns re-running the calibration harness when the embedding
   model changes.** Today the index is pinned to `nomic-embed-text`.
   A future migration to `nomic-embed-text-v1.5` or
   `mxbai-embed-large` (both 768+-dim) would invalidate the calibrated
   thresholds — but there's no mechanism to force a re-bake.
   - *Recommendation:* add a post-v2 follow-up: an
     `index_version`-style `embedding_model_version` field in
     `store_metadata`, and a pre-flight check in `sessions report`
     that warns if the calibration artifact's recorded
     `embedding_model` differs from the current store's.

4. **Whether `heimdall_non_search_when_hits_present` (OQ-2 companion
   counter) belongs inside `semantic_drift` or in a new
   `tool_use.recall_patterns` sub-object.** Argument for `semantic_drift`:
   it's a drift-shaped signal. Argument for splitting: it's
   textually-derived, not semantically-derived.
   - *Recommendation:* ship it inside `semantic_drift` as a
     `companion_counters` sub-map — keeps the drift block
     self-contained and makes the "we added a companion metric, not
     a semantic one" distinction explicit via structure.

---

## 7. References

- **Nomic embed model card** — similarity-band folk thresholds:
  https://huggingface.co/nomic-ai/nomic-embed-text-v1.5
- **BEIR** — Thakur et al. 2021. "BEIR: A Heterogeneous Benchmark
  for Zero-shot Evaluation of Information Retrieval Models."
  arXiv:2104.08663. Precision-first evaluation convention for
  retrieval.
- **MTEB** — Muennighoff et al. 2022. "MTEB: Massive Text Embedding
  Benchmark." arXiv:2210.07316. Per-task threshold calibration;
  model-family consistency argument.
- **DPR** — Karpukhin et al. 2020. "Dense Passage Retrieval for
  Open-Domain Question Answering." arXiv:2004.04906. Query/passage
  normalization symmetry (source for OQ-8 recommendation).
- **Self-RAG** — Asai et al. 2023. "Self-RAG: Learning to Retrieve,
  Generate, and Critique through Self-Reflection." arXiv:2310.11511.
  Per-turn atomic "did the model retrieve" framing (OQ-4).
- **SPLADE** — Formal et al. 2021. "SPLADE: Sparse Lexical and
  Expansion Model for First Stage Ranking." arXiv:2109.10086.
  0.05-step threshold sweeps (OQ-6).
- **Anthropic prompt caching docs** —
  https://docs.claude.com/en/docs/build-with-claude/prompt-caching
- **Prometheus naming conventions** —
  https://prometheus.io/docs/practices/naming/
- **OpenTelemetry semconv** —
  https://opentelemetry.io/docs/specs/semconv/

Internal references:

- Plan 12 itself: `docs/plans/hooks/12-semantic-drift-metric.md`
- Prior handoff: `docs/plans/hooks/07-next-session-handoff.md`
- Bench harness prior art: `cmd/bench-retrieval/main.go:100-345`
- UserPromptSubmit hook: `internal/cli/hook_user_prompt.go:317-340`
  (`stage=ok` log emission)
- Log parser (tolerates new keys): `internal/heimdall/hooklog_reader.go:39-79`
- Vector helpers: `internal/heimdall/vecmath.go:1-42`
- Transcript parser (input.query extension point):
  `internal/heimdall/transcript.go:169-201`
