// calibrate-drift sweeps T1×T2 thresholds for the semantic-drift metric
// (plan 12) over a hand-labeled JSONL sample and emits a precision/recall
// grid. It is the operator-side companion to
// `internal/heimdall/semantic_drift.go` — it reuses the same post-hoc
// vector primitives (hooks.log pvec lookup, hit-vector SELECT,
// assistant-query re-embed via Ollama) against a labeled dataset so a
// human can pick the (T1, T2) pair that meets the Stage 2→3 promotion
// gate defined in plan-12a §3.3 (≥85% precision on both buckets).
//
// Design source of truth: docs/plans/hooks/12a-design-decisions.md §3.
//
// Drift computation is intentionally inlined in this binary (see
// runDriftForTurn) rather than imported from the heimdall package:
// plan-12 and the harness PR are parallel workstreams, and keeping the
// harness self-contained lets it ship independently. A follow-up PR can
// refactor onto the shared `ComputeSemanticDrift` entry point once that
// lands and exposes a clean per-turn function. The math is the §2.2 /
// §2.3 rules straight out of plan 12 — reuse `heimdall.CosineSimilarity`
// and `heimdall.DecodeFloat32Vec` to keep the vector primitives in sync
// with the metric itself.
//
// Usage:
//
//	calibrate-drift --labels=sample.jsonl                       # text top-10
//	calibrate-drift --labels=sample.jsonl --format=csv > grid.csv
//	calibrate-drift --labels=sample.jsonl --format=json \
//	                --t1-min=0.55 --t1-max=0.55 --t1-step=0.05 \
//	                --t2-min=0.45 --t2-max=0.45 --t2-step=0.05  # single-point
//
// Output determinism: given identical labels file + identical store +
// identical hooks.log + identical grid flags, `calibrate-drift` writes
// byte-identical output. Assistant-query embeddings come from an
// Ollama stub in tests; in production Ollama is *not* deterministic, so
// the determinism guarantee covers everything downstream of the embed
// step. Callers who need full determinism end-to-end must either mock
// Ollama or pre-populate `prompt_embed_b64` + the assistant-query
// vectors in the labels file.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// LabelCategory is the four-way hand-label vocabulary from plan-12a §3.2.
// Anything else fails the JSONL parser with a clear error — unknown
// labels would silently skew precision/recall, so we refuse them.
type LabelCategory string

const (
	LabelTrulyRedundant LabelCategory = "truly_redundant"
	LabelTrulyMissed    LabelCategory = "truly_missed"
	LabelNeither        LabelCategory = "neither"
	LabelUnclear        LabelCategory = "unclear"
)

// validLabels enumerates the accepted label values. Order drives the
// deterministic label-count pretty-print in text/JSON output.
var validLabels = []LabelCategory{
	LabelTrulyRedundant,
	LabelTrulyMissed,
	LabelNeither,
	LabelUnclear,
}

// LabeledTurn is one JSONL line in the hand-labeled calibration sample
// (schema per plan-12a §3.2). The `label` field must be filled before
// the harness is run; empty-label turns are a parse error so we don't
// silently count them as negatives.
type LabeledTurn struct {
	SessionID             string        `json:"session_id"`
	TurnIdx               int           `json:"turn_idx"`
	Prompt                string        `json:"prompt"`
	InjectedHitIDs        []string      `json:"injected_hit_ids"`
	HeimdallSearchQueries []string      `json:"heimdall_search_queries"`
	HeimdallToolsUsed     []string      `json:"heimdall_tools_used"`
	V1Flagged             bool          `json:"v1_flagged"`
	Label                 LabelCategory `json:"label"`
}

// GridPoint captures the confusion-matrix results for one (T1, T2)
// combination. F1 is computed once here so sort/render doesn't redo
// the arithmetic.
type GridPoint struct {
	T1                  float64 `json:"t1"`
	T2                  float64 `json:"t2"`
	PrecisionRedundant  float64 `json:"precision_redundant"`
	RecallRedundant     float64 `json:"recall_redundant"`
	F1Redundant         float64 `json:"f1_redundant"`
	PrecisionMissed     float64 `json:"precision_missed"`
	RecallMissed        float64 `json:"recall_missed"`
	F1Missed            float64 `json:"f1_missed"`
	TPRedundant         int     `json:"tp_redundant"`
	FPRedundant         int     `json:"fp_redundant"`
	FNRedundant         int     `json:"fn_redundant"`
	TNRedundant         int     `json:"tn_redundant"`
	TPMissed            int     `json:"tp_missed"`
	FPMissed            int     `json:"fp_missed"`
	FNMissed            int     `json:"fn_missed"`
	TNMissed            int     `json:"tn_missed"`
	NTurnsScored        int     `json:"n_turns_scored"`
}

// GridMetadata is the header of the JSON output — lets a consumer
// reproduce the run without re-reading inputs, and lets the Stage 2→3
// gate artifact (plan-12a §3.5) capture a full input digest.
type GridMetadata struct {
	LabelsPath      string                 `json:"labels_path"`
	LabelsSHA256    string                 `json:"labels_sha256"`
	HooksLogPath    string                 `json:"hooks_log_path,omitempty"`
	DBPath          string                 `json:"db_path"`
	DBSHA256        string                 `json:"db_sha256"`
	EmbeddingModel  string                 `json:"embedding_model"`
	Ollama          string                 `json:"ollama"`
	T1Min           float64                `json:"t1_min"`
	T1Max           float64                `json:"t1_max"`
	T1Step          float64                `json:"t1_step"`
	T2Min           float64                `json:"t2_min"`
	T2Max           float64                `json:"t2_max"`
	T2Step          float64                `json:"t2_step"`
	NTurnsTotal     int                    `json:"n_turns_total"`
	NTurnsSkipped   int                    `json:"n_turns_skipped"`
	LabelCounts     map[LabelCategory]int  `json:"label_counts"`
	GeneratedAt     string                 `json:"generated_at,omitempty"`
}

// GridReport is the full JSON payload. Slices are sorted so the
// serialized bytes are deterministic (assuming downstream embeds were
// stable — see package doc).
type GridReport struct {
	Metadata GridMetadata `json:"metadata"`
	Grid     []GridPoint  `json:"grid"`
}

// runConfig bundles flag + flag-derived state so the library entrypoint
// (runCalibrate) can be exercised from tests without parsing flags.
type runConfig struct {
	LabelsPath    string
	HooksLogPath  string
	DBDir         string
	Model         string
	Ollama        string
	T1Min, T1Max, T1Step float64
	T2Min, T2Max, T2Step float64
	Format        string   // "text" | "csv" | "json"
	Output        string   // "" means stdout
	Now           time.Time // optional; for stable GeneratedAt in tests
	IncludeGeneratedAt bool // set when caller wants a timestamp in metadata
}

// runDriftForTurn computes the per-turn drift predictions at the given
// (T1, T2). Inlines the §2.2 + §2.3 rules from plan 12 so the harness
// doesn't depend on Agent H's package.
//
//  1. If the turn has no heimdall calls AND pvec available AND best
//     cos(pvec, any indexed chunk in top-K replay) ≥ T2 → missed.
//  2. Else if the turn has ≥1 injected hit AND ≥1 heimdall_search
//     call AND for any search call max cos(qvec, hit_vec) ≥ T1 →
//     redundant.
//
// The two buckets are mutually exclusive by §2.4 invariant.
// `skipped` is true when the turn lacks data required to score it
// (missing pvec, all hit vectors missing from store, embed failure).
func runDriftForTurn(ctx context.Context, turn LabeledTurn, t1, t2 float64, pvec []float32, hitVecs [][]float32, qVecs [][]float32, searchTopK func([]float32) []heimdall.SearchResult) (predictedRedundant bool, predictedMissed bool, skipped bool) {
	if len(pvec) == 0 {
		return false, false, true
	}

	usedHeimdall := len(turn.HeimdallToolsUsed) > 0

	// Missed bucket: no heimdall calls + prompt close to indexed chunk.
	if !usedHeimdall {
		var best float64
		if searchTopK != nil {
			for _, r := range searchTopK(pvec) {
				if r.Similarity > best {
					best = r.Similarity
				}
			}
		}
		if best >= t2 {
			return false, true, false
		}
		return false, false, false
	}

	// Redundant bucket: injected hits + at least one heimdall_search
	// above T1 against any hit vector.
	if len(turn.InjectedHitIDs) == 0 || len(turn.HeimdallSearchQueries) == 0 {
		return false, false, false
	}
	if len(hitVecs) == 0 {
		// All hit IDs resolved to missing rows; F3 territory. Drop the
		// turn from the scored set to avoid fabricating a negative.
		return false, false, true
	}
	if len(qVecs) == 0 {
		return false, false, true
	}
	for _, qvec := range qVecs {
		if len(qvec) == 0 {
			continue
		}
		for _, hv := range hitVecs {
			if heimdall.CosineSimilarity(qvec, hv) >= t1 {
				return true, false, false
			}
		}
	}
	return false, false, false
}

// parseLabels reads a JSONL file one line per LabeledTurn. Unknown label
// values and empty label strings are hard errors (line-numbered) — the
// harness must never silently coerce them to "neither," because that
// biases the confusion matrix. Extra JSON keys are ignored by
// json.Unmarshal, which matches plan-12a's "extra keys → ignore" rule.
//
// Returned error wraps the line number so operators can jump straight
// to the broken row.
func parseLabels(r io.Reader) ([]LabeledTurn, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var out []LabeledTurn
	line := 0
	for sc.Scan() {
		line++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		var t LabeledTurn
		if err := json.Unmarshal([]byte(raw), &t); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if t.SessionID == "" {
			return nil, fmt.Errorf("line %d: session_id is required", line)
		}
		if t.Label == "" {
			return nil, fmt.Errorf("line %d: label is required (one of truly_redundant, truly_missed, neither, unclear)", line)
		}
		valid := false
		for _, lbl := range validLabels {
			if t.Label == lbl {
				valid = true
				break
			}
		}
		if !valid {
			return nil, fmt.Errorf("line %d: unknown label %q (must be one of truly_redundant, truly_missed, neither, unclear)", line, t.Label)
		}
		out = append(out, t)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan labels: %w", err)
	}
	return out, nil
}

// loadPromptVectors walks hooks.log for event=user-prompt stage=ok lines
// and builds a `(session_id, turn_idx)` → pvec map. `turn_idx` is the
// zero-based ordinal of stage=ok user-prompt events within one session
// — that's the same positional indexing the sampling tool uses when it
// builds calibration_sample.jsonl (plan-12a §3.2), so the two match.
//
// If hooksLogPath is empty or the file doesn't exist, we return an
// empty map and let the caller decide what to do (fallback re-embed).
func loadPromptVectors(hooksLogPath string) (map[string]map[int][]float32, error) {
	if hooksLogPath == "" {
		return map[string]map[int][]float32{}, nil
	}
	if _, err := os.Stat(hooksLogPath); os.IsNotExist(err) {
		return map[string]map[int][]float32{}, nil
	}
	entries, err := heimdall.ReadHookLog(heimdall.ReadHookLogOpts{
		Path:  hooksLogPath,
		Event: "user-prompt",
	})
	if err != nil {
		return nil, fmt.Errorf("read hooks.log: %w", err)
	}
	// Sort by timestamp then by Session for stable positional indexing.
	// ReadHookLog already returns chronological order, but we sort
	// explicitly so the calibrate output is invariant under scanner
	// ordering quirks.
	sort.SliceStable(entries, func(i, j int) bool {
		if !entries[i].Timestamp.Equal(entries[j].Timestamp) {
			return entries[i].Timestamp.Before(entries[j].Timestamp)
		}
		return entries[i].Session < entries[j].Session
	})

	out := map[string]map[int][]float32{}
	counters := map[string]int{}
	for _, e := range entries {
		if e.Session == "" {
			continue
		}
		if e.Fields["stage"] != "ok" {
			continue
		}
		turnIdx := counters[e.Session]
		counters[e.Session] = turnIdx + 1
		b64 := e.Fields["prompt_embed_b64"]
		if b64 == "" {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			continue
		}
		vec := heimdall.DecodeFloat32Vec(raw)
		if len(vec) == 0 {
			continue
		}
		if out[e.Session] == nil {
			out[e.Session] = map[int][]float32{}
		}
		out[e.Session][turnIdx] = vec
	}
	return out, nil
}

// loadHitVectors resolves each distinct chunk id to its decoded embedding
// via store.VectorByID. Missing ids are silently skipped (F3 per plan 12
// §7) — the caller's runDriftForTurn treats a turn with all hit ids
// missing as skipped to avoid a fabricated negative. Uses the shared
// heimdall.VectorStore helper (added in PR #58) so the harness and the
// in-process drift compute path go through the same query + decode
// primitive, keeping output byte-identical to the pre-helper raw-SQL
// version.
func loadHitVectors(store *heimdall.VectorStore, ids []string) map[string][]float32 {
	out := map[string][]float32{}
	if store == nil {
		return out
	}
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, seen := out[id]; seen {
			continue
		}
		vec, err := store.VectorByID(id)
		if err != nil {
			continue
		}
		if len(vec) == 0 {
			continue
		}
		out[id] = vec
	}
	return out
}

// sweepGrid iterates every (T1, T2) pair in the configured range and
// scores every labeled turn once per pair. The inner loop is
// O(turns × |grid|) — 100 × 35 = 3500 evaluations, cheap.
func sweepGrid(ctx context.Context, cfg runConfig, turns []LabeledTurn, promptVecs map[string]map[int][]float32, perTurn map[int]turnComputedData, topKProvider func(pvec []float32) []heimdall.SearchResult) []GridPoint {
	var grid []GridPoint
	t1Vals := rangeValues(cfg.T1Min, cfg.T1Max, cfg.T1Step)
	t2Vals := rangeValues(cfg.T2Min, cfg.T2Max, cfg.T2Step)
	for _, t1 := range t1Vals {
		for _, t2 := range t2Vals {
			pt := scorePoint(ctx, turns, promptVecs, perTurn, topKProvider, t1, t2)
			grid = append(grid, pt)
		}
	}
	return grid
}

// turnComputedData caches the vectors loaded per turn so the grid sweep
// doesn't re-run Ollama embed or SQL lookups 35 times.
type turnComputedData struct {
	HitVectors      [][]float32
	AssistantQueryVectors [][]float32
}

// scorePoint computes the confusion matrix for a single (T1, T2) pair.
// Turns without enough data to be predicted (skipped=true) are dropped
// from NTurnsScored; "unclear" labels are dropped from the precision /
// recall denominators but still counted in NTurnsScored so the total
// matches the input sample.
func scorePoint(ctx context.Context, turns []LabeledTurn, promptVecs map[string]map[int][]float32, perTurn map[int]turnComputedData, topKProvider func(pvec []float32) []heimdall.SearchResult, t1, t2 float64) GridPoint {
	pt := GridPoint{T1: roundTo(t1, 4), T2: roundTo(t2, 4)}
	for i, turn := range turns {
		var pvec []float32
		if sess, ok := promptVecs[turn.SessionID]; ok {
			pvec = sess[turn.TurnIdx]
		}
		hitVecs := perTurn[i].HitVectors
		qVecs := perTurn[i].AssistantQueryVectors
		predR, predM, skipped := runDriftForTurn(ctx, turn, t1, t2, pvec, hitVecs, qVecs, topKProvider)
		if skipped {
			continue
		}
		pt.NTurnsScored++
		if turn.Label == LabelUnclear {
			// Kept in NTurnsScored but excluded from p/r math —
			// by design (plan-12a §3.2 "<10%" cap).
			continue
		}
		// Redundant confusion matrix.
		if turn.Label == LabelTrulyRedundant {
			if predR {
				pt.TPRedundant++
			} else {
				pt.FNRedundant++
			}
		} else {
			if predR {
				pt.FPRedundant++
			} else {
				pt.TNRedundant++
			}
		}
		// Missed confusion matrix (independent axis).
		if turn.Label == LabelTrulyMissed {
			if predM {
				pt.TPMissed++
			} else {
				pt.FNMissed++
			}
		} else {
			if predM {
				pt.FPMissed++
			} else {
				pt.TNMissed++
			}
		}
	}
	pt.PrecisionRedundant = safeDiv(pt.TPRedundant, pt.TPRedundant+pt.FPRedundant)
	pt.RecallRedundant = safeDiv(pt.TPRedundant, pt.TPRedundant+pt.FNRedundant)
	pt.F1Redundant = f1(pt.PrecisionRedundant, pt.RecallRedundant)
	pt.PrecisionMissed = safeDiv(pt.TPMissed, pt.TPMissed+pt.FPMissed)
	pt.RecallMissed = safeDiv(pt.TPMissed, pt.TPMissed+pt.FNMissed)
	pt.F1Missed = f1(pt.PrecisionMissed, pt.RecallMissed)
	return pt
}

// rangeValues emits a closed-interval float range with step, rounded to
// 4 decimals so floating-point drift doesn't create ghost pairs at the
// grid edges (e.g. 0.6000000001).
func rangeValues(min, max, step float64) []float64 {
	if step <= 0 {
		return []float64{roundTo(min, 4)}
	}
	var out []float64
	// We allow min > max (empty grid); more common: min == max = single point.
	for v := min; v <= max+1e-9; v += step {
		out = append(out, roundTo(v, 4))
	}
	return out
}

func roundTo(f float64, places int) float64 {
	mult := 1.0
	for i := 0; i < places; i++ {
		mult *= 10
	}
	// Round-half-away-from-zero is good enough for the step grid; we
	// only use this to stabilize display + JSON.
	if f >= 0 {
		return float64(int64(f*mult+0.5)) / mult
	}
	return float64(int64(f*mult-0.5)) / mult
}

func safeDiv(num, den int) float64 {
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den)
}

func f1(p, r float64) float64 {
	if p+r == 0 {
		return 0
	}
	return 2 * p * r / (p + r)
}

// sumLabels tallies the label category counts, in the deterministic
// validLabels order. Unknown labels are impossible here because
// parseLabels rejects them up front.
func sumLabels(turns []LabeledTurn) map[LabelCategory]int {
	out := map[LabelCategory]int{}
	for _, lbl := range validLabels {
		out[lbl] = 0
	}
	for _, t := range turns {
		out[t.Label]++
	}
	return out
}

// sha256Hex returns the hex-encoded SHA-256 of a file's contents, or
// "" if the file cannot be read. Used in the JSON header so the
// Stage 2→3 artifact can prove which inputs were scored.
func sha256Hex(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// dbSHA256 digests the store's vectors.db file. Catches "labels ran
// against a DB that has since been reindexed." See plan-12a §3.5
// go-no-go artifact shape.
func dbSHA256(dbDir string) string {
	return sha256Hex(filepath.Join(dbDir, "vectors.db"))
}

// renderCSV writes the grid in spreadsheet-friendly form. Column order
// matches plan-12a §3.4 example. We sort by (t1, t2) ascending for
// deterministic row ordering; the top-N text view re-sorts on its own.
func renderCSV(w io.Writer, grid []GridPoint) error {
	cw := csv.NewWriter(w)
	defer cw.Flush()
	if err := cw.Write([]string{
		"t1", "t2",
		"precision_redundant", "recall_redundant", "f1_redundant",
		"precision_missed", "recall_missed", "f1_missed",
		"tp_redundant", "fp_redundant", "fn_redundant", "tn_redundant",
		"tp_missed", "fp_missed", "fn_missed", "tn_missed",
		"n_turns_scored",
	}); err != nil {
		return err
	}
	// Copy + sort so render order is grid-sweep-order invariant.
	cp := make([]GridPoint, len(grid))
	copy(cp, grid)
	sort.SliceStable(cp, func(i, j int) bool {
		if cp[i].T1 != cp[j].T1 {
			return cp[i].T1 < cp[j].T1
		}
		return cp[i].T2 < cp[j].T2
	})
	for _, pt := range cp {
		if err := cw.Write([]string{
			fmt.Sprintf("%.4f", pt.T1),
			fmt.Sprintf("%.4f", pt.T2),
			fmt.Sprintf("%.4f", pt.PrecisionRedundant),
			fmt.Sprintf("%.4f", pt.RecallRedundant),
			fmt.Sprintf("%.4f", pt.F1Redundant),
			fmt.Sprintf("%.4f", pt.PrecisionMissed),
			fmt.Sprintf("%.4f", pt.RecallMissed),
			fmt.Sprintf("%.4f", pt.F1Missed),
			fmt.Sprintf("%d", pt.TPRedundant),
			fmt.Sprintf("%d", pt.FPRedundant),
			fmt.Sprintf("%d", pt.FNRedundant),
			fmt.Sprintf("%d", pt.TNRedundant),
			fmt.Sprintf("%d", pt.TPMissed),
			fmt.Sprintf("%d", pt.FPMissed),
			fmt.Sprintf("%d", pt.FNMissed),
			fmt.Sprintf("%d", pt.TNMissed),
			fmt.Sprintf("%d", pt.NTurnsScored),
		}); err != nil {
			return err
		}
	}
	return nil
}

// renderText prints the top-10 grid points by combined precision
// (redundant + missed). Ties break on higher F1-sum then lower T1 then
// lower T2 so the result is stable.
func renderText(w io.Writer, meta GridMetadata, grid []GridPoint) {
	fmt.Fprintf(w, "Heimdall drift-calibration sweep\n")
	fmt.Fprintf(w, "================================\n")
	fmt.Fprintf(w, "Labels:   %s (sha256=%s)\n", meta.LabelsPath, truncateHash(meta.LabelsSHA256))
	fmt.Fprintf(w, "Store:    %s (sha256=%s)\n", meta.DBPath, truncateHash(meta.DBSHA256))
	fmt.Fprintf(w, "HooksLog: %s\n", meta.HooksLogPath)
	fmt.Fprintf(w, "Model:    %s (via %s)\n", meta.EmbeddingModel, meta.Ollama)
	fmt.Fprintf(w, "Sample:   %d turns (%d skipped)\n", meta.NTurnsTotal, meta.NTurnsSkipped)
	fmt.Fprintf(w, "Labels:")
	for _, lbl := range validLabels {
		fmt.Fprintf(w, " %s=%d", lbl, meta.LabelCounts[lbl])
	}
	fmt.Fprintf(w, "\n")
	fmt.Fprintf(w, "Grid:     T1 [%.2f..%.2f step %.2f] x T2 [%.2f..%.2f step %.2f] = %d points\n\n",
		meta.T1Min, meta.T1Max, meta.T1Step,
		meta.T2Min, meta.T2Max, meta.T2Step,
		len(grid))

	cp := make([]GridPoint, len(grid))
	copy(cp, grid)
	sort.SliceStable(cp, func(i, j int) bool {
		pa := cp[i].PrecisionRedundant + cp[i].PrecisionMissed
		pb := cp[j].PrecisionRedundant + cp[j].PrecisionMissed
		if pa != pb {
			return pa > pb
		}
		fa := cp[i].F1Redundant + cp[i].F1Missed
		fb := cp[j].F1Redundant + cp[j].F1Missed
		if fa != fb {
			return fa > fb
		}
		if cp[i].T1 != cp[j].T1 {
			return cp[i].T1 < cp[j].T1
		}
		return cp[i].T2 < cp[j].T2
	})
	top := cp
	if len(top) > 10 {
		top = top[:10]
	}
	fmt.Fprintf(w, "Top %d (T1,T2) pairs by P(redundant)+P(missed):\n", len(top))
	fmt.Fprintf(w, "  %-6s %-6s %-10s %-10s %-10s %-10s\n", "T1", "T2", "P(red)", "R(red)", "P(mis)", "R(mis)")
	fmt.Fprintf(w, "  %-6s %-6s %-10s %-10s %-10s %-10s\n", "----", "----", "------", "------", "------", "------")
	for _, pt := range top {
		fmt.Fprintf(w, "  %-6.2f %-6.2f %-10.4f %-10.4f %-10.4f %-10.4f\n",
			pt.T1, pt.T2,
			pt.PrecisionRedundant, pt.RecallRedundant,
			pt.PrecisionMissed, pt.RecallMissed)
	}
}

func truncateHash(h string) string {
	if len(h) > 12 {
		return h[:12] + "..."
	}
	return h
}

// renderJSON writes the full grid + metadata. Pretty-printed so the
// artifact is human-reviewable. Key order is pinned by the struct —
// `encoding/json` preserves field declaration order, so this is
// byte-stable.
func renderJSON(w io.Writer, report GridReport) error {
	// Sort grid by (t1, t2) to keep JSON invariant of sweep order.
	sort.SliceStable(report.Grid, func(i, j int) bool {
		if report.Grid[i].T1 != report.Grid[j].T1 {
			return report.Grid[i].T1 < report.Grid[j].T1
		}
		return report.Grid[i].T2 < report.Grid[j].T2
	})
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// resolveDBDir mirrors cmd/bench-retrieval's resolveDBDir. Copied
// verbatim so the harness shares the "walk up from CWD, land under
// .heimdall_db/<model>" auto-detect UX — see that file's comment for
// rationale. Duplicated instead of extracted because cmd/bench-retrieval
// keeps its version unexported; we'd rather duplicate 25 lines than
// promote it to the heimdall package for two callers.
func resolveDBDir(explicitDB, cwd, model string) (string, error) {
	if explicitDB != "" {
		if _, err := os.Stat(filepath.Join(explicitDB, "vectors.db")); err != nil {
			return "", fmt.Errorf("cannot find %s/vectors.db: %v", explicitDB, err)
		}
		return explicitDB, nil
	}
	repoRoot := heimdall.FindRepoRoot(cwd)
	if repoRoot == "" {
		return "", fmt.Errorf("no --db flag and CWD is not inside a repo with an .heimdall_db/ — pass --db to calibrate against a specific database")
	}
	baseDir := filepath.Join(repoRoot, ".heimdall_db")
	modelDir := heimdall.ModelDBDir(baseDir, model)
	if _, err := os.Stat(filepath.Join(modelDir, "vectors.db")); err != nil {
		available := heimdall.ListAvailableModels(baseDir)
		if len(available) == 0 {
			return "", fmt.Errorf("no index found under %s (auto-detected repo root %s); run `heimdall-mcp index` or pass --db explicitly", baseDir, repoRoot)
		}
		return "", fmt.Errorf("no index for model %q under %s; available: %v — pass --model to match one of these or --db to point at a specific vectors.db", model, baseDir, available)
	}
	return modelDir, nil
}

// embedAssistantQueries collapses every distinct assistant-query string
// across all turns into one batch call, then rehydrates the vectors
// back onto each turn. Deduplication saves Ollama round-trips when the
// sample includes the same query twice.
func embedAssistantQueries(ctx context.Context, embedder heimdall.BatchEmbedder, turns []LabeledTurn) (map[string][]float32, error) {
	uniq := map[string]struct{}{}
	for _, t := range turns {
		for _, q := range t.HeimdallSearchQueries {
			if q != "" {
				uniq[q] = struct{}{}
			}
		}
	}
	if len(uniq) == 0 {
		return map[string][]float32{}, nil
	}
	keys := make([]string, 0, len(uniq))
	for k := range uniq {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic batch order
	vecs, err := embedder.EmbedBatch(ctx, keys)
	if err != nil {
		return nil, err
	}
	if len(vecs) != len(keys) {
		return nil, fmt.Errorf("embedder returned %d vectors for %d inputs", len(vecs), len(keys))
	}
	out := make(map[string][]float32, len(keys))
	for i, k := range keys {
		out[k] = vecs[i]
	}
	return out, nil
}

// runCalibrate is the library-level entrypoint used by tests and main().
// Tests can call it directly; main() parses flags and then delegates.
// Takes already-opened dependencies so tests can swap in fakes.
func runCalibrate(ctx context.Context, cfg runConfig, store *heimdall.VectorStore, embedder heimdall.BatchEmbedder, w io.Writer) error {
	f, err := os.Open(cfg.LabelsPath)
	if err != nil {
		return fmt.Errorf("open labels: %w", err)
	}
	turns, err := parseLabels(f)
	f.Close()
	if err != nil {
		return err
	}
	// Sort the sample by (session, turn_idx) so every downstream loop
	// walks the same order — supports the determinism guarantee.
	sort.SliceStable(turns, func(i, j int) bool {
		if turns[i].SessionID != turns[j].SessionID {
			return turns[i].SessionID < turns[j].SessionID
		}
		return turns[i].TurnIdx < turns[j].TurnIdx
	})

	promptVecs, err := loadPromptVectors(cfg.HooksLogPath)
	if err != nil {
		return err
	}

	qvecMap, err := embedAssistantQueries(ctx, embedder, turns)
	if err != nil {
		return fmt.Errorf("embed queries: %w", err)
	}

	// Preload per-turn hit vectors and assistant-query vectors so the
	// grid sweep is just arithmetic.
	perTurn := make(map[int]turnComputedData, len(turns))
	nSkipped := 0
	for i, t := range turns {
		hv := loadHitVectors(store, t.InjectedHitIDs)
		var hitVecs [][]float32
		for _, id := range t.InjectedHitIDs {
			if v, ok := hv[id]; ok {
				hitVecs = append(hitVecs, v)
			}
		}
		var qvecs [][]float32
		for _, q := range t.HeimdallSearchQueries {
			if v, ok := qvecMap[q]; ok {
				qvecs = append(qvecs, v)
			}
		}
		perTurn[i] = turnComputedData{
			HitVectors:            hitVecs,
			AssistantQueryVectors: qvecs,
		}
		if _, ok := promptVecs[t.SessionID]; !ok || len(promptVecs[t.SessionID][t.TurnIdx]) == 0 {
			nSkipped++
		}
	}

	topKProvider := func(pvec []float32) []heimdall.SearchResult {
		if store == nil || len(pvec) == 0 {
			return nil
		}
		return store.SearchFiltered(ctx, pvec, 5, "", "", nil)
	}

	grid := sweepGrid(ctx, cfg, turns, promptVecs, perTurn, topKProvider)

	meta := GridMetadata{
		LabelsPath:     cfg.LabelsPath,
		LabelsSHA256:   sha256Hex(cfg.LabelsPath),
		HooksLogPath:   cfg.HooksLogPath,
		DBPath:         cfg.DBDir,
		DBSHA256:       dbSHA256(cfg.DBDir),
		EmbeddingModel: cfg.Model,
		Ollama:         cfg.Ollama,
		T1Min:          cfg.T1Min, T1Max: cfg.T1Max, T1Step: cfg.T1Step,
		T2Min: cfg.T2Min, T2Max: cfg.T2Max, T2Step: cfg.T2Step,
		NTurnsTotal:   len(turns),
		NTurnsSkipped: nSkipped,
		LabelCounts:   sumLabels(turns),
	}
	if cfg.IncludeGeneratedAt {
		t := cfg.Now
		if t.IsZero() {
			t = time.Now().UTC()
		}
		meta.GeneratedAt = t.UTC().Format(time.RFC3339)
	}

	switch cfg.Format {
	case "csv":
		return renderCSV(w, grid)
	case "json":
		return renderJSON(w, GridReport{Metadata: meta, Grid: grid})
	case "text", "":
		renderText(w, meta, grid)
		return nil
	default:
		return fmt.Errorf("unknown --format %q (want text|csv|json)", cfg.Format)
	}
}

func main() {
	var (
		labelsFlag   = flag.String("labels", "", "path to calibration_sample.jsonl (required)")
		hooksLogFlag = flag.String("hooks-log", heimdall.HookLogPath(), "path to hooks.log for prompt_embed_b64 lookup")
		dbFlag       = flag.String("db", "", "path to .heimdall_db/<model>/ directory (must contain vectors.db); when empty, auto-detects from CWD")
		modelFlag    = flag.String("model", "nomic-embed-text", "embedding model name")
		ollamaFlag   = flag.String("ollama", "http://localhost:11434", "Ollama endpoint")
		t1Min        = flag.Float64("t1-min", 0.40, "T1 grid min (redundant threshold)")
		t1Max        = flag.Float64("t1-max", 0.70, "T1 grid max")
		t1Step       = flag.Float64("t1-step", 0.05, "T1 grid step")
		t2Min        = flag.Float64("t2-min", 0.35, "T2 grid min (missed threshold)")
		t2Max        = flag.Float64("t2-max", 0.55, "T2 grid max")
		t2Step       = flag.Float64("t2-step", 0.05, "T2 grid step")
		formatFlag   = flag.String("format", "text", "text|csv|json")
		outputFlag   = flag.String("output", "", "output file path (default stdout)")
	)
	flag.Parse()

	if *labelsFlag == "" {
		fmt.Fprintln(os.Stderr, "calibrate-drift: --labels is required")
		os.Exit(2)
	}

	cwd, _ := os.Getwd()
	dbDir, err := resolveDBDir(*dbFlag, cwd, *modelFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	client := heimdall.NewOllamaClient(*ollamaFlag)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Ollama not reachable at %s: %v\n", *ollamaFlag, err)
		os.Exit(1)
	}
	embedder := heimdall.NewOllamaEmbedder(client, *modelFlag)

	store, err := heimdall.OpenStore(dbDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open store: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	cfg := runConfig{
		LabelsPath:   *labelsFlag,
		HooksLogPath: *hooksLogFlag,
		DBDir:        dbDir,
		Model:        *modelFlag,
		Ollama:       *ollamaFlag,
		T1Min:        *t1Min, T1Max: *t1Max, T1Step: *t1Step,
		T2Min: *t2Min, T2Max: *t2Max, T2Step: *t2Step,
		Format: *formatFlag,
		Output: *outputFlag,
		// Production runs intentionally omit IncludeGeneratedAt so byte
		// output is stable for the fixture golden; operators who want
		// timestamps bake them in via a post-processing step.
	}

	w := io.Writer(os.Stdout)
	if *outputFlag != "" {
		f, err := os.Create(*outputFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "create output: %v\n", err)
			os.Exit(1)
		}
		defer f.Close()
		w = f
	}

	if err := runCalibrate(ctx, cfg, store, embedder, w); err != nil {
		fmt.Fprintf(os.Stderr, "calibrate: %v\n", err)
		os.Exit(1)
	}
}
