package heimdall

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Semantic-drift v2 thresholds. Starting values per plan 12 §5.2. Bump
// SemanticDriftThresholdVersion when either constant changes so existing
// JSON outputs stay traceable to a specific calibration.
//
// Global-only for v1 (OQ-7): no per-project override knobs. Revisit in v2
// if users ask.
const (
	SemanticDriftT1               = 0.55
	SemanticDriftT2               = 0.45
	SemanticDriftThresholdVersion = 1

	// NearThresholdDelta is the "borderline" window around T1/T2 used to
	// populate the near_threshold_{t1,t2} counts (§5.5 / OQ-6). A
	// redundant call whose best cosine lands in [T1, T1+0.05] is flagged
	// as borderline so a human skimming the report can tell "12 borderline
	// hits" from "12 confidently wasted calls."
	NearThresholdDelta = 0.05

	// SemanticDriftEmbedBatchTimeout caps the wall-clock budget for the
	// assistant-query re-embed batch (plan 12 §7.1 F9). If Ollama takes
	// longer the metric degrades to F2 (null + diagnostic) rather than
	// stalling the report.
	SemanticDriftEmbedBatchTimeout = 30 * time.Second

	// SemanticDriftEmbeddingModel is the model name echoed into the JSON
	// output. Hook-side embeds are produced by whichever model the hook
	// resolved (`ResolveUsableModelDB`). The metric trusts that choice
	// and echoes the model name stored in the hook log's `model=` field
	// when available; otherwise this default.
	SemanticDriftEmbeddingModel = "nomic-embed-text"

	// SemanticDriftMissedTopK is the top-K passed to SearchFiltered when
	// replaying "what would the hook have surfaced?" for a no-heimdall
	// turn (plan 12 §2.3). Matches the hook's live top-5.
	SemanticDriftMissedTopK = 5
)

// SemanticDriftReport is the structured output of ComputeSemanticDrift.
// Every count is non-negative. Nil slice for PerTurn when verbose is off.
// Stable key order (alphabetical) is enforced on JSON marshaling by the
// sessions renderer, not here.
type SemanticDriftReport struct {
	// Counters (plan 12 §6.1).
	SemanticRedundantCalls  int
	MissedCallOpportunities int
	NearThresholdRedundant  int
	NearThresholdMissed     int

	// Denominators.
	TurnsTotal     int
	TurnsWithHits  int
	TurnsSkipped   int
	TurnsSkippedBy map[string]int // OQ-1: reason → count

	// OQ-2 sibling: non-search heimdall_* calls made during a hits-present
	// turn. Transcript-level value (NonSearchHeimdallWhenHitsPresent) is
	// re-emitted here for convenience so the semantic_drift block is
	// self-contained.
	HeimdallNonSearchWhenHitsPresent int

	// Echoed thresholds / model for reproducibility (§6.1).
	ThresholdT1       float64
	ThresholdT2       float64
	ThresholdVersion  int
	EmbeddingModel    string

	// PerTurn is populated only when IncludePerTurn=true in inputs.
	PerTurn []SemanticDriftTurnRow
}

// SemanticDriftTurnRow is one per-turn detail row exposed under --verbose
// (OQ-10 / plan 12 §11). Verdict is one of:
//
//   "redundant"              — a heimdall_search qvec cosine >= T1.
//   "redundant_borderline"   — cosine in [T1, T1+Δ].
//   "missed"                 — zero-heimdall turn, pvec cosine >= T2.
//   "missed_borderline"      — cosine in [T2, T2+Δ].
//   "novel"                  — turn had search calls but all below T1.
//   "below_t2"               — zero-heimdall turn below T2.
//   "skip:<reason>"          — turn dropped from denominators.
//   "hook_worked"            — hits present, no heimdall call (the good
//                              case; not flagged either way).
type SemanticDriftTurnRow struct {
	TurnIdx  int
	Verdict  string
	BestSim  float64 // best cosine observed (-1 if none)
	Query    string  // search query text (redundant path) or prompt (missed path)
}

// DriftEmbedder is the minimal embedder surface ComputeSemanticDrift needs.
// OllamaEmbedder and StubEmbedder both satisfy it; tests pass StubEmbedder.
type DriftEmbedder interface {
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
}

// DriftStore is the minimal VectorStore surface ComputeSemanticDrift needs.
// Fakes in Layer-1 tests implement both methods in-process without SQLite.
type DriftStore interface {
	VectorByID(chunkID string) ([]float32, error)
	SearchFiltered(ctx context.Context, query []float32, topK int, sourceType string, subProject string, metadataFilter map[string]any, opts ...SearchOption) []SearchResult
}

// ComputeSemanticDriftInputs bundles the dependencies needed to score a
// session. The caller (sessions report) fills these from parsed transcript
// + hooks.log + live store/ollama handles.
type ComputeSemanticDriftInputs struct {
	Transcript  TranscriptSummary
	HookEntries []HookLogEntry // user-prompt events for this session only
	PromptTexts map[int]string // turnIdx → raw prompt text (for missed-bucket pvec reuse)

	Store    DriftStore
	Embedder DriftEmbedder

	T1 float64
	T2 float64

	// EmbeddingModel is echoed into the report's embedding_model field.
	// Empty → falls back to SemanticDriftEmbeddingModel.
	EmbeddingModel string

	IncludePerTurn bool // --verbose flag plumbing (OQ-10)
}

// ComputeSemanticDrift implements plan 12 §4.3. Fail-open: on any F1-F9
// failure the function returns (nil, nil) — the caller serializes
// `semantic_drift: null` plus a diagnostic key; the metric never aborts
// `sessions report`. An error return is reserved for invariant breakage
// (nil store, nil embedder) that indicates caller bugs, not data issues.
func ComputeSemanticDrift(ctx context.Context, in ComputeSemanticDriftInputs) (*SemanticDriftReport, string, error) {
	if in.Store == nil || in.Embedder == nil {
		return nil, "invariant_broken", errors.New("semantic drift: nil store or embedder")
	}
	// F7: invalid thresholds. Guard in one place so callers can't sneak in
	// 0 or >1 by forgetting to plumb defaults.
	t1, t2 := in.T1, in.T2
	if t1 == 0 {
		t1 = SemanticDriftT1
	}
	if t2 == 0 {
		t2 = SemanticDriftT2
	}
	if t1 <= 0 || t1 > 1 || t2 <= 0 || t2 > 1 {
		return nil, "invalid_threshold", nil
	}

	// F8: zero UserPromptSubmit events in the session (e.g. imported
	// transcript, empty hooks.log). Without hook-side data the metric
	// has no prompt vector and no hit-id list to work with.
	upEvents := 0
	for _, e := range in.HookEntries {
		if e.Event == "user-prompt" {
			upEvents++
		}
	}
	if upEvents == 0 {
		return nil, "no_hook_data", nil
	}

	// Index hook-log prompt vectors + hit ids by turn. We pair log lines
	// to user-prompts by chronological order: the Nth successful
	// user-prompt event in the log corresponds to transcript turn idx
	// (N-1) for a well-formed session. Cache-hit turns keep their log
	// line but without prompt_embed_b64 — they skip with cache_hit reason.
	//
	// The pairing assumes the hook log and transcript were recorded in
	// the same session; plan 12 §7 F8 already fails-open if events are
	// entirely absent, and F1 covers the pre-v2 log case (no keys).
	type turnHookData struct {
		pvec       []float32
		hitIDs     []string
		stage      string // "ok" / "cache_hit" / "skip" / etc.
		logged     bool
		dimErr     bool
	}
	// Walk user-prompt events in chronological order; they're already
	// sorted by ReadHookLog.
	turnHook := make([]turnHookData, 0, upEvents)
	for _, e := range in.HookEntries {
		if e.Event != "user-prompt" {
			continue
		}
		d := turnHookData{
			stage:  e.Fields["stage"],
			logged: true,
		}
		if b64 := e.Fields["prompt_embed_b64"]; b64 != "" {
			raw, derr := base64.StdEncoding.DecodeString(b64)
			if derr == nil {
				vec := DecodeFloat32Vec(raw)
				if len(vec) == 0 {
					d.dimErr = true
				} else {
					d.pvec = vec
				}
			}
		}
		if ids := e.Fields["hit_ids"]; ids != "" {
			for _, id := range strings.Split(ids, ",") {
				id = strings.TrimSpace(id)
				if id != "" {
					d.hitIDs = append(d.hitIDs, id)
				}
			}
		}
		turnHook = append(turnHook, d)
	}

	// Report skeleton.
	model := in.EmbeddingModel
	if model == "" {
		model = SemanticDriftEmbeddingModel
	}
	rep := &SemanticDriftReport{
		TurnsTotal:                       in.Transcript.UserMessages,
		TurnsSkippedBy:                   map[string]int{},
		ThresholdT1:                      t1,
		ThresholdT2:                      t2,
		ThresholdVersion:                 SemanticDriftThresholdVersion,
		EmbeddingModel:                   model,
		HeimdallNonSearchWhenHitsPresent: in.Transcript.NonSearchHeimdallWhenHitsPresent,
	}

	// Build queries-by-turn map and batch re-embed once. If the batch
	// fails we fail-open (F2).
	queriesByTurn := map[int][]string{}
	var allQueries []string
	for _, q := range in.Transcript.HeimdallSearchQueries {
		if q.TurnIdx < 0 || q.Query == "" {
			continue
		}
		queriesByTurn[q.TurnIdx] = append(queriesByTurn[q.TurnIdx], q.Query)
		allQueries = append(allQueries, q.Query)
	}
	var qvecsByIndex [][]float32
	if len(allQueries) > 0 {
		batchCtx, cancel := context.WithTimeout(ctx, SemanticDriftEmbedBatchTimeout)
		defer cancel()
		vecs, eerr := in.Embedder.EmbedBatch(batchCtx, allQueries)
		if eerr != nil || len(vecs) != len(allQueries) {
			// F2 / F6: Ollama unreachable or count mismatch.
			return nil, "ollama_unreachable", nil
		}
		qvecsByIndex = vecs
	}
	// Map back from (turn, position-within-turn) to qvecsByIndex offset so
	// each search call gets its own vector.
	qvecByCall := map[int][][]float32{}
	{
		off := 0
		for _, q := range in.Transcript.HeimdallSearchQueries {
			if q.TurnIdx < 0 || q.Query == "" {
				continue
			}
			qvecByCall[q.TurnIdx] = append(qvecByCall[q.TurnIdx], qvecsByIndex[off])
			off++
		}
	}

	// Walk each turn by index. TurnsTotal is from transcript UserMessages;
	// hook entries may be fewer (cache_hit dropped, missing log line).
	for turnIdx := 0; turnIdx < in.Transcript.UserMessages; turnIdx++ {
		var hook turnHookData
		if turnIdx < len(turnHook) {
			hook = turnHook[turnIdx]
		}

		heimdallCallsThisTurn := in.Transcript.HeimdallCallsByTurn[turnIdx]
		searchQueries := queriesByTurn[turnIdx]
		searchVecs := qvecByCall[turnIdx]

		// Did the hook actually inject hits? Non-empty hit_ids on stage=ok
		// is the positive signal. We fold "hits present" into
		// TurnsWithHits once per turn — not per search call.
		hitsPresent := len(hook.hitIDs) > 0
		if hitsPresent {
			rep.TurnsWithHits++
		}

		// F1 / cache-hit / OQ-1: no prompt_embed_b64 → drop turn from the
		// missed-bucket denominator with a specific reason. We still score
		// the redundant bucket if hit_ids are present (those don't need
		// pvec — they need qvec which comes from the transcript).
		missedEligible := true
		if !hook.logged {
			rep.TurnsSkipped++
			rep.TurnsSkippedBy["no_log_line"]++
			missedEligible = false
		} else if hook.stage == "cache_hit" && len(hook.pvec) == 0 {
			rep.TurnsSkipped++
			rep.TurnsSkippedBy["cache_hit"]++
			missedEligible = false
		} else if hook.dimErr {
			// F4: decoded blob length was zero. Note: a non-zero blob
			// that's the wrong dimension is caught during cosine (two
			// vectors of different length → 0.0), so we only treat
			// the hard decode-empty case as a skip here. The caller's
			// VerifyHookIndex guard is the real dimension watchdog.
			rep.TurnsSkipped++
			rep.TurnsSkippedBy["dim_mismatch"]++
			missedEligible = false
		} else if len(hook.pvec) == 0 {
			rep.TurnsSkipped++
			rep.TurnsSkippedBy["no_prompt_embed"]++
			missedEligible = false
		}

		// --- Redundant bucket (§2.2) ---------------------------------------
		// Runs independently of missedEligible: a turn without pvec can
		// still have hit_ids + heimdall_search queries, and the cosine
		// (qvec, hit-vec) doesn't need pvec.
		redundantCounted := false
		var bestRedundant float64 = -1
		if hitsPresent && len(searchQueries) > 0 {
			// Pre-fetch hit vectors (one SELECT per distinct id).
			hitVecs := make([][]float32, 0, len(hook.hitIDs))
			for _, id := range hook.hitIDs {
				hv, herr := in.Store.VectorByID(id)
				if herr != nil {
					// F3: hit id no longer in store. Skip this hit.
					continue
				}
				if len(hv) > 0 {
					hitVecs = append(hitVecs, hv)
				}
			}
			if len(hitVecs) == 0 {
				// All hits missing — can't score this turn's redundant
				// bucket. Not counted toward TurnsSkipped (missed-bucket
				// is still eligible if pvec is around).
				if in.IncludePerTurn {
					rep.PerTurn = append(rep.PerTurn, SemanticDriftTurnRow{
						TurnIdx: turnIdx, Verdict: "skip:hits_dropped", BestSim: -1,
					})
				}
			} else {
				for i, qvec := range searchVecs {
					// Max-over-hits cosine — most lenient reading.
					best := -1.0
					for _, hv := range hitVecs {
						sim := CosineSimilarity(qvec, hv)
						if sim > best {
							best = sim
						}
					}
					if best > bestRedundant {
						bestRedundant = best
					}
					if best >= t1 {
						rep.SemanticRedundantCalls++
						redundantCounted = true
						if best <= t1+NearThresholdDelta {
							rep.NearThresholdRedundant++
						}
						if in.IncludePerTurn {
							verdict := "redundant"
							if best <= t1+NearThresholdDelta {
								verdict = "redundant_borderline"
							}
							rep.PerTurn = append(rep.PerTurn, SemanticDriftTurnRow{
								TurnIdx: turnIdx, Verdict: verdict, BestSim: best,
								Query: searchQueries[i],
							})
						}
					} else if in.IncludePerTurn {
						rep.PerTurn = append(rep.PerTurn, SemanticDriftTurnRow{
							TurnIdx: turnIdx, Verdict: "novel", BestSim: best,
							Query: searchQueries[i],
						})
					}
				}
			}
		}

		// --- Missed bucket (§2.3) ------------------------------------------
		// Invariant (§2.4): only score missed if the assistant made ZERO
		// heimdall_* calls. OQ-4 lenient reading — any heimdall tool
		// (search / recall / expand / ls) disqualifies the turn.
		if missedEligible && heimdallCallsThisTurn == 0 {
			results := in.Store.SearchFiltered(ctx, hook.pvec, SemanticDriftMissedTopK, "", "", nil)
			best := -1.0
			for _, r := range results {
				if r.Similarity > best {
					best = r.Similarity
				}
			}
			if best >= t2 {
				rep.MissedCallOpportunities++
				if best <= t2+NearThresholdDelta {
					rep.NearThresholdMissed++
				}
				if in.IncludePerTurn {
					verdict := "missed"
					if best <= t2+NearThresholdDelta {
						verdict = "missed_borderline"
					}
					rep.PerTurn = append(rep.PerTurn, SemanticDriftTurnRow{
						TurnIdx: turnIdx, Verdict: verdict, BestSim: best,
						Query: in.PromptTexts[turnIdx],
					})
				}
			} else if in.IncludePerTurn {
				rep.PerTurn = append(rep.PerTurn, SemanticDriftTurnRow{
					TurnIdx: turnIdx, Verdict: "below_t2", BestSim: best,
					Query: in.PromptTexts[turnIdx],
				})
			}
		} else if in.IncludePerTurn && !redundantCounted && len(searchQueries) == 0 {
			// Hook worked (hits present, no heimdall call) or skip row.
			verdict := ""
			if hitsPresent {
				verdict = "hook_worked"
			} else if !missedEligible {
				verdict = "skip"
			}
			if verdict != "" {
				rep.PerTurn = append(rep.PerTurn, SemanticDriftTurnRow{
					TurnIdx: turnIdx, Verdict: verdict, BestSim: -1,
					Query: in.PromptTexts[turnIdx],
				})
			}
		}
		_ = bestRedundant // reserved for future per-turn richer verdicts
	}

	return rep, "", nil
}

// LookupVectorByID is a ctx-aware variant of VectorByID retained so tests
// can inject a slow store. Not used by the production path yet, but
// documented here so future callers can follow the pattern rather than
// bolt-on their own.
func LookupVectorByID(ctx context.Context, s DriftStore, id string) ([]float32, error) {
	_ = ctx
	vec, err := s.VectorByID(id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("VectorByID(%s): %w", id, err)
	}
	return vec, nil
}
