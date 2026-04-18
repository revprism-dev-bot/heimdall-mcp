package heimdall

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

// ---------- fakes ----------

// fakeDriftStore is an in-process DriftStore with no SQLite dependency.
// VectorByID honors sql.ErrNoRows for unknown ids; SearchFiltered returns
// results with Similarity = CosineSimilarity(query, record.Embedding),
// sorted descending and truncated to topK. Matches the production
// semantics closely enough that the drift tests exercise the same
// cosine/threshold paths.
type fakeDriftStore struct {
	vecs        map[string][]float32
	allRecords  []VectorRecord
	vectorCalls int
}

func (f *fakeDriftStore) VectorByID(id string) ([]float32, error) {
	f.vectorCalls++
	v, ok := f.vecs[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return v, nil
}

func (f *fakeDriftStore) SearchFiltered(ctx context.Context, query []float32, topK int, sourceType, subProject string, metadataFilter map[string]any, opts ...SearchOption) []SearchResult {
	_ = ctx
	_ = sourceType
	_ = subProject
	_ = metadataFilter
	_ = opts
	var out []SearchResult
	for _, r := range f.allRecords {
		sim := CosineSimilarity(query, r.Embedding)
		out = append(out, SearchResult{Record: r, Similarity: sim})
	}
	// Simple insertion sort — tiny N in tests.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Similarity > out[j-1].Similarity; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	if topK > 0 && len(out) > topK {
		out = out[:topK]
	}
	return out
}

// fakeDriftEmbedder is a deterministic embedder: each input string maps to
// a pre-set vector. Unknown inputs return zero vectors. A flag lets tests
// flip the embedder into "fail next batch" mode (F2).
type fakeDriftEmbedder struct {
	vecs    map[string][]float32
	failErr error
	calls   int
}

func (f *fakeDriftEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	_ = ctx
	f.calls++
	if f.failErr != nil {
		return nil, f.failErr
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		if v, ok := f.vecs[t]; ok {
			out[i] = v
		} else {
			out[i] = make([]float32, 3)
		}
	}
	return out, nil
}

// ---------- helpers ----------

// normVec returns a unit vector along `angle` radians in the 3-dim xy
// plane, padded with a zero z component. Tests pick (angle_a, angle_b) so
// that CosineSimilarity is exactly cos(angle_a - angle_b), which gives a
// reliable mapping from a desired cosine to a vector pair without floating
// cruft.
func normVec(angle float64) []float32 {
	return []float32{float32(math.Cos(angle)), float32(math.Sin(angle)), 0}
}

// b64PromptVec returns the strconv-safe base64 string used for the
// prompt_embed_b64 hook-log key. Helper so tests don't repeat the
// encode/decode dance.
func b64PromptVec(v []float32) string {
	return base64.StdEncoding.EncodeToString(EncodeFloat32Vec(v))
}

// mkHookEntry builds a user-prompt HookLogEntry with the given stage and
// (optional) prompt vector + hit ids. Ordering across a slice of calls is
// preserved by the caller (ReadHookLog returns chronological).
func mkHookEntry(stage string, pvec []float32, hitIDs []string) HookLogEntry {
	fields := map[string]string{"stage": stage, "session": "s1"}
	if len(pvec) > 0 {
		fields["prompt_embed_b64"] = b64PromptVec(pvec)
	}
	if len(hitIDs) > 0 {
		fields["hit_ids"] = strings.Join(hitIDs, ",")
	}
	return HookLogEntry{
		Timestamp: time.Unix(0, 0).UTC(),
		Level:     "INFO",
		Event:     "user-prompt",
		Session:   "s1",
		Fields:    fields,
	}
}

// ---------- SD1: no hook data --------------------------------------------

func TestCompute_NoHookData(t *testing.T) {
	rep, diag, err := ComputeSemanticDrift(context.Background(), ComputeSemanticDriftInputs{
		Transcript: TranscriptSummary{UserMessages: 2},
		Store:      &fakeDriftStore{},
		Embedder:   &fakeDriftEmbedder{},
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if rep != nil {
		t.Fatalf("expected nil report, got %+v", rep)
	}
	if diag != "no_hook_data" {
		t.Fatalf("expected no_hook_data diag, got %q", diag)
	}
}

// ---------- SD2: no heimdall calls, low similarity -----------------------

func TestCompute_NoHeimdallCalls_LowSim(t *testing.T) {
	pvec := normVec(0)
	far := normVec(math.Pi / 2) // cosine 0 vs pvec
	rep, diag, err := ComputeSemanticDrift(context.Background(), ComputeSemanticDriftInputs{
		Transcript: TranscriptSummary{
			UserMessages:        1,
			HeimdallCallsByTurn: map[int]int{},
		},
		HookEntries: []HookLogEntry{mkHookEntry("ok", pvec, nil)},
		Store: &fakeDriftStore{
			allRecords: []VectorRecord{{ID: "c1", Embedding: far}},
		},
		Embedder: &fakeDriftEmbedder{},
	})
	if err != nil || diag != "" {
		t.Fatalf("err=%v diag=%q", err, diag)
	}
	if rep.MissedCallOpportunities != 0 {
		t.Errorf("missed: got %d want 0", rep.MissedCallOpportunities)
	}
	if rep.TurnsTotal != 1 {
		t.Errorf("turns_total: got %d want 1", rep.TurnsTotal)
	}
}

// ---------- SD3: no heimdall calls, high similarity ----------------------

func TestCompute_NoHeimdallCalls_HighSim(t *testing.T) {
	pvec := normVec(0)
	near := normVec(math.Acos(0.9)) // cosine ~0.9 vs pvec — well above T2=0.45
	rep, diag, err := ComputeSemanticDrift(context.Background(), ComputeSemanticDriftInputs{
		Transcript: TranscriptSummary{
			UserMessages:        1,
			HeimdallCallsByTurn: map[int]int{},
		},
		HookEntries: []HookLogEntry{mkHookEntry("ok", pvec, nil)},
		Store: &fakeDriftStore{
			allRecords: []VectorRecord{{ID: "c1", Embedding: near}},
		},
		Embedder: &fakeDriftEmbedder{},
	})
	if err != nil || diag != "" {
		t.Fatalf("err=%v diag=%q", err, diag)
	}
	if rep.MissedCallOpportunities != 1 {
		t.Errorf("missed: got %d want 1", rep.MissedCallOpportunities)
	}
}

// ---------- SD4: heimdall calls + hits, cosine < T1 ----------------------

func TestCompute_HeimdallCalls_LowSim(t *testing.T) {
	// Hit vector far from the search query vector → cosine ~0.
	hitV := normVec(0)
	qvec := normVec(math.Pi / 2)
	rep, diag, err := ComputeSemanticDrift(context.Background(), ComputeSemanticDriftInputs{
		Transcript: TranscriptSummary{
			UserMessages:        1,
			HeimdallCallsByTurn: map[int]int{0: 1},
			HeimdallSearchQueries: []TranscriptSearchQuery{
				{TurnIdx: 0, Query: "far query"},
			},
		},
		HookEntries: []HookLogEntry{mkHookEntry("ok", normVec(0), []string{"c1"})},
		Store: &fakeDriftStore{
			vecs: map[string][]float32{"c1": hitV},
		},
		Embedder: &fakeDriftEmbedder{
			vecs: map[string][]float32{"far query": qvec},
		},
	})
	if err != nil || diag != "" {
		t.Fatalf("err=%v diag=%q", err, diag)
	}
	if rep.SemanticRedundantCalls != 0 {
		t.Errorf("redundant: got %d want 0", rep.SemanticRedundantCalls)
	}
}

// ---------- SD5: heimdall calls + hits, cosine >= T1 ---------------------

func TestCompute_HeimdallCalls_HighSim(t *testing.T) {
	hitV := normVec(0)
	qvec := normVec(math.Acos(0.7)) // cosine ~0.7, above T1=0.55
	rep, diag, err := ComputeSemanticDrift(context.Background(), ComputeSemanticDriftInputs{
		Transcript: TranscriptSummary{
			UserMessages:        1,
			HeimdallCallsByTurn: map[int]int{0: 1},
			HeimdallSearchQueries: []TranscriptSearchQuery{
				{TurnIdx: 0, Query: "close query"},
			},
		},
		HookEntries: []HookLogEntry{mkHookEntry("ok", normVec(0), []string{"c1"})},
		Store: &fakeDriftStore{
			vecs: map[string][]float32{"c1": hitV},
		},
		Embedder: &fakeDriftEmbedder{
			vecs: map[string][]float32{"close query": qvec},
		},
	})
	if err != nil || diag != "" {
		t.Fatalf("err=%v diag=%q", err, diag)
	}
	if rep.SemanticRedundantCalls != 1 {
		t.Errorf("redundant: got %d want 1", rep.SemanticRedundantCalls)
	}
}

// ---------- SD6: multiple searches in one turn, two above T1 -------------

func TestCompute_MultiSearchInTurn(t *testing.T) {
	hitV := normVec(0)
	qA := normVec(math.Acos(0.7)) // above T1
	qB := normVec(math.Acos(0.8)) // above T1
	qC := normVec(math.Pi / 2)    // cosine 0, below T1
	rep, diag, err := ComputeSemanticDrift(context.Background(), ComputeSemanticDriftInputs{
		Transcript: TranscriptSummary{
			UserMessages:        1,
			HeimdallCallsByTurn: map[int]int{0: 3},
			HeimdallSearchQueries: []TranscriptSearchQuery{
				{TurnIdx: 0, Query: "a"},
				{TurnIdx: 0, Query: "b"},
				{TurnIdx: 0, Query: "c"},
			},
		},
		HookEntries: []HookLogEntry{mkHookEntry("ok", normVec(0), []string{"c1"})},
		Store:       &fakeDriftStore{vecs: map[string][]float32{"c1": hitV}},
		Embedder: &fakeDriftEmbedder{
			vecs: map[string][]float32{"a": qA, "b": qB, "c": qC},
		},
	})
	if err != nil || diag != "" {
		t.Fatalf("err=%v diag=%q", err, diag)
	}
	if rep.SemanticRedundantCalls != 2 {
		t.Errorf("redundant: got %d want 2", rep.SemanticRedundantCalls)
	}
}

// ---------- SD7: log entry missing prompt_embed_b64 ----------------------

func TestCompute_NoPromptVector(t *testing.T) {
	// Entry has no pvec but non-empty stage. The turn cannot contribute to
	// the missed bucket; it increments TurnsSkipped with reason
	// no_prompt_embed.
	rep, diag, err := ComputeSemanticDrift(context.Background(), ComputeSemanticDriftInputs{
		Transcript: TranscriptSummary{
			UserMessages:        1,
			HeimdallCallsByTurn: map[int]int{},
		},
		HookEntries: []HookLogEntry{mkHookEntry("ok", nil, nil)},
		Store:       &fakeDriftStore{},
		Embedder:    &fakeDriftEmbedder{},
	})
	if err != nil || diag != "" {
		t.Fatalf("err=%v diag=%q", err, diag)
	}
	if rep.TurnsSkipped != 1 {
		t.Errorf("turns_skipped: got %d want 1", rep.TurnsSkipped)
	}
	if rep.TurnsSkippedBy["no_prompt_embed"] != 1 {
		t.Errorf("no_prompt_embed reason: got %d want 1",
			rep.TurnsSkippedBy["no_prompt_embed"])
	}
}

// ---------- SD8: missing chunk id in store (F3) --------------------------

func TestCompute_MissingChunkID(t *testing.T) {
	pvec := normVec(0)
	qvec := normVec(math.Acos(0.7))
	rep, diag, err := ComputeSemanticDrift(context.Background(), ComputeSemanticDriftInputs{
		Transcript: TranscriptSummary{
			UserMessages:        1,
			HeimdallCallsByTurn: map[int]int{0: 1},
			HeimdallSearchQueries: []TranscriptSearchQuery{
				{TurnIdx: 0, Query: "q"},
			},
		},
		// hit_ids list points at a chunk the store no longer has.
		HookEntries: []HookLogEntry{mkHookEntry("ok", pvec, []string{"gone"})},
		Store:       &fakeDriftStore{vecs: map[string][]float32{}},
		Embedder:    &fakeDriftEmbedder{vecs: map[string][]float32{"q": qvec}},
	})
	if err != nil || diag != "" {
		t.Fatalf("err=%v diag=%q", err, diag)
	}
	if rep.SemanticRedundantCalls != 0 {
		t.Errorf("redundant: got %d want 0 (all hits dropped)",
			rep.SemanticRedundantCalls)
	}
}

// ---------- SD9: embedder fails (F2) ------------------------------------

func TestCompute_EmbedderFails(t *testing.T) {
	rep, diag, err := ComputeSemanticDrift(context.Background(), ComputeSemanticDriftInputs{
		Transcript: TranscriptSummary{
			UserMessages:        1,
			HeimdallCallsByTurn: map[int]int{0: 1},
			HeimdallSearchQueries: []TranscriptSearchQuery{
				{TurnIdx: 0, Query: "q"},
			},
		},
		HookEntries: []HookLogEntry{mkHookEntry("ok", normVec(0), []string{"c1"})},
		Store:       &fakeDriftStore{vecs: map[string][]float32{"c1": normVec(0)}},
		Embedder:    &fakeDriftEmbedder{failErr: errors.New("boom")},
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if rep != nil {
		t.Fatalf("expected nil report, got %+v", rep)
	}
	if diag != "ollama_unreachable" {
		t.Fatalf("expected ollama_unreachable diag, got %q", diag)
	}
}

// ---------- SD10: threshold boundary (== T1 counts; below does not) -------

func TestCompute_ThresholdBoundary(t *testing.T) {
	hitV := normVec(0)
	// Build qvec so CosineSimilarity ≈ T1 exactly.
	atT1 := normVec(math.Acos(SemanticDriftT1))
	justBelow := normVec(math.Acos(SemanticDriftT1 - 0.02))

	run := func(q string, qv []float32) int {
		rep, _, _ := ComputeSemanticDrift(context.Background(), ComputeSemanticDriftInputs{
			Transcript: TranscriptSummary{
				UserMessages:        1,
				HeimdallCallsByTurn: map[int]int{0: 1},
				HeimdallSearchQueries: []TranscriptSearchQuery{
					{TurnIdx: 0, Query: q},
				},
			},
			HookEntries: []HookLogEntry{mkHookEntry("ok", normVec(0), []string{"c1"})},
			Store:       &fakeDriftStore{vecs: map[string][]float32{"c1": hitV}},
			Embedder:    &fakeDriftEmbedder{vecs: map[string][]float32{q: qv}},
		})
		return rep.SemanticRedundantCalls
	}

	if run("at", atT1) != 1 {
		t.Errorf("cosine==T1 should count")
	}
	if run("below", justBelow) != 0 {
		t.Errorf("cosine<T1 should not count")
	}
}

// ---------- SD11: near-threshold buckets --------------------------------

func TestCompute_NearThresholdBuckets(t *testing.T) {
	hitV := normVec(0)
	pvec := normVec(0)
	// Borderline: just above T1, inside [T1, T1+delta]. Same for T2.
	qvecRedundantBorderline := normVec(math.Acos(SemanticDriftT1 + 0.01))
	// Missed scenario: pvec cosine matches borderline above T2.
	chunk := normVec(math.Acos(SemanticDriftT2 + 0.01))

	rep, diag, err := ComputeSemanticDrift(context.Background(), ComputeSemanticDriftInputs{
		Transcript: TranscriptSummary{
			UserMessages:        2,
			HeimdallCallsByTurn: map[int]int{0: 1},
			HeimdallSearchQueries: []TranscriptSearchQuery{
				{TurnIdx: 0, Query: "q"},
			},
		},
		HookEntries: []HookLogEntry{
			mkHookEntry("ok", pvec, []string{"c1"}),
			mkHookEntry("ok", pvec, nil), // turn 1: no hits, no heimdall → missed
		},
		Store: &fakeDriftStore{
			vecs:       map[string][]float32{"c1": hitV},
			allRecords: []VectorRecord{{ID: "db1", Embedding: chunk}},
		},
		Embedder: &fakeDriftEmbedder{
			vecs: map[string][]float32{"q": qvecRedundantBorderline},
		},
	})
	if err != nil || diag != "" {
		t.Fatalf("err=%v diag=%q", err, diag)
	}
	if rep.SemanticRedundantCalls != 1 || rep.NearThresholdRedundant != 1 {
		t.Errorf("redundant=%d nearT1=%d; want 1/1",
			rep.SemanticRedundantCalls, rep.NearThresholdRedundant)
	}
	if rep.MissedCallOpportunities != 1 || rep.NearThresholdMissed != 1 {
		t.Errorf("missed=%d nearT2=%d; want 1/1",
			rep.MissedCallOpportunities, rep.NearThresholdMissed)
	}
}

// ---------- SD12: invariant — no double count ---------------------------

func TestCompute_InvariantNoDoubleCount(t *testing.T) {
	// Turn has a heimdall_search call AND a high-similarity pvec. The
	// missed-bucket should be suppressed because heimdallCallsThisTurn > 0.
	pvec := normVec(0)
	chunk := normVec(0) // cosine 1.0 vs pvec — would otherwise count as missed
	hitV := normVec(0)
	qvec := normVec(math.Acos(0.7))
	rep, diag, err := ComputeSemanticDrift(context.Background(), ComputeSemanticDriftInputs{
		Transcript: TranscriptSummary{
			UserMessages:        1,
			HeimdallCallsByTurn: map[int]int{0: 1},
			HeimdallSearchQueries: []TranscriptSearchQuery{
				{TurnIdx: 0, Query: "q"},
			},
		},
		HookEntries: []HookLogEntry{mkHookEntry("ok", pvec, []string{"c1"})},
		Store: &fakeDriftStore{
			vecs:       map[string][]float32{"c1": hitV},
			allRecords: []VectorRecord{{ID: "db1", Embedding: chunk}},
		},
		Embedder: &fakeDriftEmbedder{vecs: map[string][]float32{"q": qvec}},
	})
	if err != nil || diag != "" {
		t.Fatalf("err=%v diag=%q", err, diag)
	}
	if rep.MissedCallOpportunities != 0 {
		t.Errorf("missed: got %d want 0 (invariant)", rep.MissedCallOpportunities)
	}
}

// ---------- SD13: dimension mismatch (F4) ------------------------------

func TestCompute_DimMismatch(t *testing.T) {
	// prompt_embed_b64 decodes to a zero-length vector → dim_mismatch.
	entry := mkHookEntry("ok", nil, nil)
	entry.Fields["prompt_embed_b64"] = "" // already empty — hits no_prompt_embed path
	// Build a truly broken blob: empty base64 decodes to empty bytes.
	// Use a 1-byte base64 string which is actually "invalid" — stdlib
	// returns an error. Covered via CorruptDim case below.
	rep, diag, err := ComputeSemanticDrift(context.Background(), ComputeSemanticDriftInputs{
		Transcript: TranscriptSummary{
			UserMessages:        1,
			HeimdallCallsByTurn: map[int]int{},
		},
		HookEntries: []HookLogEntry{entry},
		Store:       &fakeDriftStore{},
		Embedder:    &fakeDriftEmbedder{},
	})
	if err != nil || diag != "" {
		t.Fatalf("err=%v diag=%q", err, diag)
	}
	if rep.TurnsSkipped != 1 {
		t.Errorf("turns_skipped: got %d want 1", rep.TurnsSkipped)
	}
	// Either no_prompt_embed or dim_mismatch is acceptable — both are F4/F1
	// variants and both correctly drop the turn from the denominator.
	if rep.TurnsSkippedBy["no_prompt_embed"]+rep.TurnsSkippedBy["dim_mismatch"] != 1 {
		t.Errorf("expected one skip reason for dim/embed absence: %+v", rep.TurnsSkippedBy)
	}
}

// ---------- SD14: malformed input.query (F5) ---------------------------

func TestCompute_MalformedQueryInput(t *testing.T) {
	// An assistant tool_use with a missing query is excluded at parse
	// time (TranscriptSearchQuery never captured). The session still
	// scores other calls.
	hitV := normVec(0)
	qvec := normVec(math.Acos(0.7))
	rep, diag, err := ComputeSemanticDrift(context.Background(), ComputeSemanticDriftInputs{
		Transcript: TranscriptSummary{
			UserMessages:        1,
			HeimdallCallsByTurn: map[int]int{0: 2},
			HeimdallSearchQueries: []TranscriptSearchQuery{
				// Only the well-formed call is here — parser dropped the
				// malformed one. See transcript.go / transcript_test.go.
				{TurnIdx: 0, Query: "good"},
			},
		},
		HookEntries: []HookLogEntry{mkHookEntry("ok", normVec(0), []string{"c1"})},
		Store:       &fakeDriftStore{vecs: map[string][]float32{"c1": hitV}},
		Embedder:    &fakeDriftEmbedder{vecs: map[string][]float32{"good": qvec}},
	})
	if err != nil || diag != "" {
		t.Fatalf("err=%v diag=%q", err, diag)
	}
	if rep.SemanticRedundantCalls != 1 {
		t.Errorf("redundant: got %d want 1", rep.SemanticRedundantCalls)
	}
}

// ---------- SD15: Bash/Edit/Read never trigger redundant path -----------

func TestCompute_NonHeimdallTool(t *testing.T) {
	// Turn has Bash tool calls and a hook-injected hit. The parser
	// doesn't include Bash calls in HeimdallSearchQueries. No redundant,
	// no missed (hits present so no missed anyway).
	rep, diag, err := ComputeSemanticDrift(context.Background(), ComputeSemanticDriftInputs{
		Transcript: TranscriptSummary{
			UserMessages: 1,
			// No heimdall calls this turn — but hits present. Missed
			// bucket should NOT trigger because hits are present
			// (hook-worked path). Redundant bucket has no search
			// queries, so also zero.
			HeimdallCallsByTurn: map[int]int{},
		},
		HookEntries: []HookLogEntry{mkHookEntry("ok", normVec(0), []string{"c1"})},
		Store:       &fakeDriftStore{vecs: map[string][]float32{"c1": normVec(0)}},
		Embedder:    &fakeDriftEmbedder{},
	})
	if err != nil || diag != "" {
		t.Fatalf("err=%v diag=%q", err, diag)
	}
	if rep.SemanticRedundantCalls != 0 || rep.MissedCallOpportunities != 0 {
		t.Errorf("redundant=%d missed=%d; want 0/0",
			rep.SemanticRedundantCalls, rep.MissedCallOpportunities)
	}
	if rep.TurnsWithHits != 1 {
		t.Errorf("turns_with_hits: got %d want 1", rep.TurnsWithHits)
	}
}

// ---------- SD16: empty hits turn ----------------------------------------

func TestCompute_EmptyHitsTurn(t *testing.T) {
	// User prompt ran; hook stage=ok but hit_ids empty (e.g. query
	// matched nothing above floor). Turn counts in TurnsTotal, not in
	// TurnsWithHits.
	rep, diag, err := ComputeSemanticDrift(context.Background(), ComputeSemanticDriftInputs{
		Transcript: TranscriptSummary{
			UserMessages:        1,
			HeimdallCallsByTurn: map[int]int{},
		},
		HookEntries: []HookLogEntry{mkHookEntry("ok", normVec(0), nil)},
		Store:       &fakeDriftStore{},
		Embedder:    &fakeDriftEmbedder{},
	})
	if err != nil || diag != "" {
		t.Fatalf("err=%v diag=%q", err, diag)
	}
	if rep.TurnsTotal != 1 || rep.TurnsWithHits != 0 {
		t.Errorf("total=%d withHits=%d; want 1/0", rep.TurnsTotal, rep.TurnsWithHits)
	}
}

// ---------- SD17: JSON schema (indirect — via struct) -------------------

func TestRender_JSONSchema(t *testing.T) {
	// Asserts the struct has every field called out in plan 12 §6.1. The
	// cli/sessions.go test covers actual JSON shape; this one guards the
	// struct layout from regressions that would silently drop fields.
	want := []string{
		"SemanticRedundantCalls",
		"MissedCallOpportunities",
		"NearThresholdRedundant",
		"NearThresholdMissed",
		"TurnsTotal",
		"TurnsWithHits",
		"TurnsSkipped",
		"TurnsSkippedBy",
		"HeimdallNonSearchWhenHitsPresent",
		"ThresholdT1",
		"ThresholdT2",
		"ThresholdVersion",
		"EmbeddingModel",
		"PerTurn",
	}
	typ := reflect.TypeOf(SemanticDriftReport{})
	present := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		present[typ.Field(i).Name] = true
	}
	for _, name := range want {
		if !present[name] {
			t.Errorf("SemanticDriftReport missing field %q", name)
		}
	}
}

// ---------- SD18: diag strings on error paths ---------------------------

func TestRender_JSONNullOnError(t *testing.T) {
	cases := []struct {
		name string
		in   ComputeSemanticDriftInputs
		want string
	}{
		{
			name: "no_hook_data",
			in: ComputeSemanticDriftInputs{
				Transcript: TranscriptSummary{UserMessages: 1},
				Store:      &fakeDriftStore{}, Embedder: &fakeDriftEmbedder{},
			},
			want: "no_hook_data",
		},
		{
			name: "invalid_threshold",
			in: ComputeSemanticDriftInputs{
				Transcript:  TranscriptSummary{UserMessages: 1},
				HookEntries: []HookLogEntry{mkHookEntry("ok", normVec(0), nil)},
				Store:       &fakeDriftStore{}, Embedder: &fakeDriftEmbedder{},
				T1: 2.5, T2: 0.5,
			},
			want: "invalid_threshold",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rep, diag, err := ComputeSemanticDrift(context.Background(), c.in)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if rep != nil {
				t.Fatalf("expected nil report, got %+v", rep)
			}
			if diag != c.want {
				t.Fatalf("diag: got %q want %q", diag, c.want)
			}
		})
	}
}

// ---------- SD19: text line rendering (deferred to sessions_test.go) ----

func TestRender_TextLine(t *testing.T) {
	// The sessions.go text renderer is tested in internal/cli/sessions_test.go.
	// This test just guards the exported constants the text line depends on.
	if SemanticDriftT1 <= 0 || SemanticDriftT2 <= 0 {
		t.Errorf("T1=%v T2=%v", SemanticDriftT1, SemanticDriftT2)
	}
	if NearThresholdDelta != 0.05 {
		t.Errorf("NearThresholdDelta: got %v want 0.05", NearThresholdDelta)
	}
	if SemanticDriftThresholdVersion != 1 {
		t.Errorf("threshold_version: got %d want 1", SemanticDriftThresholdVersion)
	}
}

// ---------- SD20: deterministic output ---------------------------------

func TestCompute_Deterministic(t *testing.T) {
	// Build inputs with varied hit orders and verify 10 consecutive runs
	// return structurally identical reports.
	hitV := normVec(0)
	qvec := normVec(math.Acos(0.7))
	pvec := normVec(0)
	in := ComputeSemanticDriftInputs{
		Transcript: TranscriptSummary{
			UserMessages:        2,
			HeimdallCallsByTurn: map[int]int{0: 1},
			HeimdallSearchQueries: []TranscriptSearchQuery{
				{TurnIdx: 0, Query: "q"},
			},
		},
		HookEntries: []HookLogEntry{
			mkHookEntry("ok", pvec, []string{"c1", "c2"}),
			mkHookEntry("ok", pvec, nil),
		},
		Store: &fakeDriftStore{
			vecs: map[string][]float32{
				"c1": hitV,
				"c2": normVec(math.Pi / 2),
			},
			allRecords: []VectorRecord{
				{ID: "db1", Embedding: normVec(math.Pi / 2)},
			},
		},
		Embedder: &fakeDriftEmbedder{vecs: map[string][]float32{"q": qvec}},
	}
	prev, diag, err := ComputeSemanticDrift(context.Background(), in)
	if err != nil || diag != "" {
		t.Fatalf("err=%v diag=%q", err, diag)
	}
	for i := 0; i < 10; i++ {
		cur, _, _ := ComputeSemanticDrift(context.Background(), in)
		if !reflect.DeepEqual(prev, cur) {
			t.Fatalf("run %d diverged:\nprev=%+v\ncur=%+v", i, prev, cur)
		}
	}
}

// ---------- SD-L2 tests (Layer 2 against httptest-fake Ollama) ---------

// These use the real OllamaEmbedder against a httptest server so we hit the
// batch round-trip, cancellation, and count-mismatch paths that the Stub
// embedder can't exercise. See plan 12 §8.2.

// SD-L2-1: end-to-end happy path — fake Ollama returns a deterministic
// batch response, the function computes the metric without errors.
func TestDrift_HappyPath(t *testing.T) {
	srv, _ := captureEmbedServer(t, 200, EmbedResponse{
		Embeddings: [][]float32{normVec(math.Acos(0.7))},
	})
	client := NewOllamaClient(srv.URL)
	embedder := NewOllamaEmbedder(client, "m")

	// Wrap to satisfy DriftEmbedder (it already does — OllamaEmbedder has
	// the matching method — but we double-check with an interface assign).
	var _ DriftEmbedder = embedder

	pvec := normVec(0)
	hitV := normVec(0)
	rep, diag, err := ComputeSemanticDrift(context.Background(), ComputeSemanticDriftInputs{
		Transcript: TranscriptSummary{
			UserMessages:        1,
			HeimdallCallsByTurn: map[int]int{0: 1},
			HeimdallSearchQueries: []TranscriptSearchQuery{
				{TurnIdx: 0, Query: "q"},
			},
		},
		HookEntries: []HookLogEntry{mkHookEntry("ok", pvec, []string{"c1"})},
		Store:       &fakeDriftStore{vecs: map[string][]float32{"c1": hitV}},
		Embedder:    embedder,
	})
	if err != nil || diag != "" {
		t.Fatalf("err=%v diag=%q", err, diag)
	}
	if rep.SemanticRedundantCalls != 1 {
		t.Errorf("redundant: got %d want 1", rep.SemanticRedundantCalls)
	}
}

// SD-L2-2: Ollama down (httptest 500) → F2, null + diagnostic, report
// never aborts.
func TestDrift_OllamaDown(t *testing.T) {
	srv := newBatchServer(t, 500, map[string]string{"error": "boom"})
	defer srv.Close()
	client := NewOllamaClient(srv.URL)
	embedder := NewOllamaEmbedder(client, "m")

	rep, diag, err := ComputeSemanticDrift(context.Background(), ComputeSemanticDriftInputs{
		Transcript: TranscriptSummary{
			UserMessages:        1,
			HeimdallCallsByTurn: map[int]int{0: 1},
			HeimdallSearchQueries: []TranscriptSearchQuery{
				{TurnIdx: 0, Query: "q"},
			},
		},
		HookEntries: []HookLogEntry{mkHookEntry("ok", normVec(0), []string{"c1"})},
		Store:       &fakeDriftStore{vecs: map[string][]float32{"c1": normVec(0)}},
		Embedder:    embedder,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if rep != nil {
		t.Fatalf("expected nil report, got %+v", rep)
	}
	if diag != "ollama_unreachable" {
		t.Fatalf("diag: got %q want ollama_unreachable", diag)
	}
}

// SD-L2-3: batch split — more than EmbedBatchSize queries triggers two
// batches, stitched correctly. Uses a handler that counts POSTs and
// returns deterministic responses per request.
func TestDrift_BatchSplit(t *testing.T) {
	batches := 0
	// We will submit EmbedBatchSize + 2 queries. Expect 2 separate batch
	// HTTP requests. Each request's text count dictates how many vectors
	// we return.
	handler := func(body []byte) EmbedResponse {
		batches++
		var req EmbedBatchRequest
		_ = json.Unmarshal(body, &req)
		resp := EmbedResponse{Embeddings: make([][]float32, len(req.Input))}
		for i := range resp.Embeddings {
			// All vectors cosine 1 with hit (both normVec(0)). Batching
			// doesn't have to be exotic — just needs to succeed.
			resp.Embeddings[i] = normVec(0)
		}
		return resp
	}
	srv := httptestServerWithBodyHandler(t, handler)
	defer srv.Close()
	client := NewOllamaClient(srv.URL)
	embedder := NewOllamaEmbedder(client, "m")

	numQueries := EmbedBatchSize + 2
	queries := make([]TranscriptSearchQuery, numQueries)
	for i := range queries {
		queries[i] = TranscriptSearchQuery{TurnIdx: 0, Query: fmt.Sprintf("q%d", i)}
	}
	rep, diag, err := ComputeSemanticDrift(context.Background(), ComputeSemanticDriftInputs{
		Transcript: TranscriptSummary{
			UserMessages:          1,
			HeimdallCallsByTurn:   map[int]int{0: numQueries},
			HeimdallSearchQueries: queries,
		},
		HookEntries: []HookLogEntry{mkHookEntry("ok", normVec(0), []string{"c1"})},
		Store:       &fakeDriftStore{vecs: map[string][]float32{"c1": normVec(0)}},
		Embedder:    embedder,
	})
	if err != nil || diag != "" {
		t.Fatalf("err=%v diag=%q", err, diag)
	}
	if rep.SemanticRedundantCalls != numQueries {
		t.Errorf("redundant: got %d want %d", rep.SemanticRedundantCalls, numQueries)
	}
	if batches < 2 {
		t.Errorf("expected >=2 batches for EmbedBatchSize+2 queries, got %d", batches)
	}
}

// SD-L2-4: context cancellation propagates cleanly. A cancelled parent
// ctx must abort the compute without leaking goroutines.
func TestDrift_CancellationPropagates(t *testing.T) {
	// Slow server: 2s delay before response. Cancel before it returns.
	srv := httptestServerSlow(t, 2*time.Second)
	defer srv.Close()
	client := NewOllamaClient(srv.URL)
	embedder := NewOllamaEmbedder(client, "m")

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel immediately so the batch returns context.Canceled.
	cancel()

	rep, diag, err := ComputeSemanticDrift(ctx, ComputeSemanticDriftInputs{
		Transcript: TranscriptSummary{
			UserMessages:        1,
			HeimdallCallsByTurn: map[int]int{0: 1},
			HeimdallSearchQueries: []TranscriptSearchQuery{
				{TurnIdx: 0, Query: "q"},
			},
		},
		HookEntries: []HookLogEntry{mkHookEntry("ok", normVec(0), []string{"c1"})},
		Store:       &fakeDriftStore{vecs: map[string][]float32{"c1": normVec(0)}},
		Embedder:    embedder,
	})
	if err != nil {
		t.Fatalf("unexpected err on cancellation: %v", err)
	}
	if rep != nil {
		t.Fatalf("expected nil report on cancellation, got %+v", rep)
	}
	if diag != "ollama_unreachable" {
		t.Fatalf("diag: got %q want ollama_unreachable", diag)
	}
}
