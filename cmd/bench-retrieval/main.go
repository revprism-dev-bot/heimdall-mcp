// bench-retrieval measures the token-savings delta of Heimdall's tiered
// retrieval modes (detail=summary|snippet|full).
//
// It is a follow-up to the TODO section 2 item "Token-savings measurement
// before/after". For a query set against a real index it runs the same search
// in three modes, counts an approximate token total per mode, and reports:
//   - per-query bytes + est-tokens for each mode
//   - mean / p50 / p95 across the query set
//   - a summary_then_expand savings model: assume a caller first fetches
//     summary and then drills down on `expand_rate` of the results — what
//     fraction of the full-mode token spend does that avoid?
//
// Token counting uses a chars/4 approximation. This is deliberately crude —
// the bench measures *relative* savings between modes, which is unaffected
// by absolute tokenizer accuracy for homogeneous text. The assumption is
// documented where it's used.
//
// Usage:
//
//	bench-retrieval                                # auto-detect from CWD
//	bench-retrieval --db=/path/to/.heimdall_db/<model>/ [flags]
//	bench-retrieval --format=json
//	bench-retrieval --queries=queries.txt --top-k=10 --expand-rate=0.2
//
// When --db is omitted, the binary walks up from the current working
// directory the same way retrieval hooks do (heimdall.FindRepoRoot) and
// resolves the model-specific index directory via heimdall.ModelDBDir. This
// lets `make bench` work from any project that's been indexed, not just
// heimdall-mcp itself. Pass --db explicitly in CI / scripts.
//
// The DB is opened read-only via a snapshot copy so the production index
// is never mutated by search-side last_accessed updates the underlying
// VectorStore would otherwise perform.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// charsPerTokenApprox is the character-per-token ratio used to estimate
// output-token cost. Four is the standard rule of thumb for English text
// with BPE-style tokenizers (cl100k_base, o200k_base). We use it only for
// *relative* comparison across detail modes over the same underlying
// content, where the ratio cancels out of the saving percent. If you wire
// tiktoken-go later, just swap estTokens() — the rest of the bench is
// tokenizer-agnostic.
const charsPerTokenApprox = 4

// DefaultQueries is the built-in query set. Queries cover the four classes
// called out in the task spec — function lookup, concept lookup, file-path
// lookup, error string / memory types — plus a few realistic Heimdall
// dogfood queries we'd actually ask during development. Keeping them
// hard-coded in the binary makes the bench reproducible without a side
// file.
var DefaultQueries = []string{
	// Function lookups
	"HookSessionStart",
	"SearchFiltered functional options",
	"ExpandByID chunk retrieval",
	"VerifyHookIndex sentinel errors",
	// Concept lookups
	"tier B suppression store",
	"fork setsid detached actor",
	"hook cache eviction",
	"tiered retrieval detail summary snippet full",
	// File-path lookups
	"how does install.go compute the scope",
	"where is post-edit debouncer implemented",
	"session-start hook template rendering",
	// Error strings / memory types
	"redact home directory in hook log",
	"skill memory type validation",
	"memory type preference decision fact",
	// Realistic dogfood queries
	"what does WithScope do",
	"context path hierarchy prefix filter",
	"last_accessed freshness decay",
	"how are embeddings stored in sqlite",
}

// Mode describes one detail setting of the search call.
type Mode struct {
	Name   string             // "summary", "snippet", "full"
	Option heimdall.SearchOption
}

// QueryMetric captures the raw counters for one query in one mode.
type QueryMetric struct {
	Bytes    int `json:"bytes"`
	Tokens   int `json:"tokens"`
	Results  int `json:"results"`
}

// QueryReport aggregates the three modes for a single query.
type QueryReport struct {
	Query   string                 `json:"query"`
	Modes   map[string]QueryMetric `json:"modes"`
	Summary QueryMetric            `json:"-"`
	Snippet QueryMetric            `json:"-"`
	Full    QueryMetric            `json:"-"`
}

// Report is the full benchmark output.
type Report struct {
	DB              string          `json:"db"`
	Model           string          `json:"model"`
	IndexRecords    int             `json:"indexRecords"`
	TopK            int             `json:"topK"`
	ExpandRate      float64         `json:"expandRate"`
	Queries         []QueryReport   `json:"queries"`
	Aggregate       map[string]Aggr `json:"aggregate"`
	SavingPercent   float64         `json:"savingPercent"`
	SummaryTotal    int             `json:"summaryTotalTokens"`
	SnippetTotal    int             `json:"snippetTotalTokens"`
	FullTotal       int             `json:"fullTotalTokens"`
	ExpandBlendTok  int             `json:"summaryThenExpandTokens"`
	CharsPerToken   int             `json:"charsPerTokenApprox"`
	DurationMs      int64           `json:"durationMs"`
}

// Aggr reports mean, p50, p95 over the query set for a single mode.
type Aggr struct {
	Mean   float64 `json:"mean"`
	P50    int     `json:"p50"`
	P95    int     `json:"p95"`
	Total  int     `json:"total"`
}

// estTokens approximates token count from character count using the
// chars/4 rule. See charsPerTokenApprox for rationale.
func estTokens(b int) int {
	if b <= 0 {
		return 0
	}
	t := b / charsPerTokenApprox
	if b%charsPerTokenApprox != 0 {
		t++
	}
	return t
}

// renderResults serializes a result slice the same way the MCP layer does
// — indented JSON. We use the same format so the token estimate matches
// what a real Claude client would see on the wire.
func renderResults(results []heimdall.SearchResult) (string, error) {
	// Minimal shape mirroring SearchResultEnriched — we intentionally
	// don't import internal/mcp to keep the bench a clean leaf binary.
	type row struct {
		File           string  `json:"file"`
		StartLine      int     `json:"startLine"`
		EndLine        int     `json:"endLine"`
		Content        string  `json:"content"`
		Score          float64 `json:"score"`
		ChunkID        string  `json:"chunkId"`
		Summary        string  `json:"summary,omitempty"`
		ContextPath    string  `json:"contextPath,omitempty"`
	}
	rows := make([]row, 0, len(results))
	for _, r := range results {
		rows = append(rows, row{
			File:        r.Record.FilePath,
			StartLine:   r.Record.StartLine,
			EndLine:     r.Record.EndLine,
			Content:     r.Record.Content,
			Score:       r.Similarity,
			ChunkID:     r.Record.ID,
			Summary:     r.Record.Summary,
			ContextPath: r.Record.ContextPath,
		})
	}
	b, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// snapshotDB copies the vectors.db file so the bench can open it without
// mutating the caller's production index (SearchFiltered itself is
// read-only, but *callers* run UpdateLastAccessed; we want to keep the
// bench side-effect-free even from the library code's perspective).
func snapshotDB(srcDir string) (string, error) {
	tmp, err := os.MkdirTemp("", "heimdall-bench-*")
	if err != nil {
		return "", err
	}
	src := filepath.Join(srcDir, "vectors.db")
	dst := filepath.Join(tmp, "vectors.db")
	sf, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("open source db: %w", err)
	}
	defer sf.Close()
	df, err := os.Create(dst)
	if err != nil {
		return "", fmt.Errorf("create snapshot db: %w", err)
	}
	if _, err := io.Copy(df, sf); err != nil {
		df.Close()
		return "", fmt.Errorf("copy db: %w", err)
	}
	if err := df.Close(); err != nil {
		return "", err
	}
	// Also copy WAL/SHM sidecars if present — SQLite sometimes needs them
	// to see recent commits.
	for _, suffix := range []string{"-wal", "-shm"} {
		s := src + suffix
		if _, err := os.Stat(s); err != nil {
			continue
		}
		d := dst + suffix
		f1, _ := os.Open(s)
		f2, _ := os.Create(d)
		io.Copy(f2, f1)
		f1.Close()
		f2.Close()
	}
	return tmp, nil
}

// BenchConfig bundles the knobs for runBench so the smoke test can drive
// it programmatically without shelling out.
type BenchConfig struct {
	DBDir       string   // directory containing vectors.db
	Queries     []string // query set (must be non-empty)
	TopK        int      // search top-k (default 10)
	ExpandRate  float64  // 0..1 fraction of results the caller expands
	Embedder    heimdall.Embedder // required — caller wires a real or stub embedder
	ModelName   string   // for the report header; cosmetic
	Snapshot    bool     // if true, copy the DB before opening (prod-safe)
}

// runBench is the library entry point. It returns a populated Report or
// an error. The CLI main() simply configures BenchConfig and renders the
// result.
func runBench(ctx context.Context, cfg BenchConfig) (*Report, error) {
	if len(cfg.Queries) == 0 {
		return nil, fmt.Errorf("queries cannot be empty")
	}
	if cfg.TopK <= 0 {
		cfg.TopK = 10
	}
	if cfg.ExpandRate < 0 || cfg.ExpandRate > 1 {
		return nil, fmt.Errorf("expand-rate must be in [0,1]")
	}
	if cfg.Embedder == nil {
		return nil, fmt.Errorf("embedder is required")
	}

	dbDir := cfg.DBDir
	if cfg.Snapshot {
		snap, err := snapshotDB(cfg.DBDir)
		if err != nil {
			return nil, fmt.Errorf("snapshot: %w", err)
		}
		defer os.RemoveAll(snap)
		dbDir = snap
	}

	store, err := heimdall.OpenStore(dbDir)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	stats := store.Stats()

	modes := []Mode{
		{Name: "summary", Option: heimdall.WithDetail("summary")},
		{Name: "snippet", Option: heimdall.WithDetail("snippet")},
		{Name: "full", Option: heimdall.WithDetail("full")},
	}

	start := time.Now()
	report := &Report{
		DB:            cfg.DBDir,
		Model:         cfg.ModelName,
		IndexRecords:  stats.TotalRecords,
		TopK:          cfg.TopK,
		ExpandRate:    cfg.ExpandRate,
		CharsPerToken: charsPerTokenApprox,
		Aggregate:     map[string]Aggr{},
	}

	modeTokens := map[string][]int{"summary": nil, "snippet": nil, "full": nil}

	for _, q := range cfg.Queries {
		qvec, err := cfg.Embedder.Embed(ctx, q)
		if err != nil {
			return nil, fmt.Errorf("embed %q: %w", q, err)
		}
		qr := QueryReport{Query: q, Modes: map[string]QueryMetric{}}
		for _, m := range modes {
			res := store.SearchFiltered(ctx, qvec, cfg.TopK, "", "", nil, m.Option)
			rendered, err := renderResults(res)
			if err != nil {
				return nil, fmt.Errorf("render: %w", err)
			}
			metric := QueryMetric{
				Bytes:   len(rendered),
				Tokens:  estTokens(len(rendered)),
				Results: len(res),
			}
			qr.Modes[m.Name] = metric
			modeTokens[m.Name] = append(modeTokens[m.Name], metric.Tokens)
		}
		report.Queries = append(report.Queries, qr)
	}

	for name, toks := range modeTokens {
		report.Aggregate[name] = summarize(toks)
	}

	report.SummaryTotal = report.Aggregate["summary"].Total
	report.SnippetTotal = report.Aggregate["snippet"].Total
	report.FullTotal = report.Aggregate["full"].Total

	// summary_then_expand model: start at summary, then expand expand_rate
	// of the results. Each expand call replaces the summary-tier cost with
	// the full-tier cost for that one row — so the blended cost is
	// sum(summary) + expand_rate * (sum(full) - sum(summary)). That
	// matches the blended-cost formula in the task spec.
	blend := float64(report.SummaryTotal) +
		cfg.ExpandRate*float64(report.FullTotal-report.SummaryTotal)
	report.ExpandBlendTok = int(math.Round(blend))
	if report.FullTotal > 0 {
		report.SavingPercent = (1.0 - float64(report.ExpandBlendTok)/float64(report.FullTotal)) * 100
	}
	report.DurationMs = time.Since(start).Milliseconds()

	return report, nil
}

// summarize computes mean, p50, p95, and sum for a sorted copy of xs.
func summarize(xs []int) Aggr {
	if len(xs) == 0 {
		return Aggr{}
	}
	cp := make([]int, len(xs))
	copy(cp, xs)
	sort.Ints(cp)
	sum := 0
	for _, v := range cp {
		sum += v
	}
	return Aggr{
		Mean:  float64(sum) / float64(len(cp)),
		P50:   percentile(cp, 0.50),
		P95:   percentile(cp, 0.95),
		Total: sum,
	}
}

// percentile returns the value at fraction p of a sorted slice using a
// simple nearest-rank method — good enough for a small N like our 18
// queries.
func percentile(sorted []int, p float64) int {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// renderText produces the human-readable stdout report.
func renderText(w io.Writer, r *Report) {
	fmt.Fprintf(w, "Heimdall tiered-retrieval token-savings benchmark\n")
	fmt.Fprintf(w, "=================================================\n")
	fmt.Fprintf(w, "DB:        %s\n", r.DB)
	fmt.Fprintf(w, "Model:     %s\n", r.Model)
	fmt.Fprintf(w, "Records:   %d  (top-k=%d, queries=%d, expand-rate=%.2f)\n",
		r.IndexRecords, r.TopK, len(r.Queries), r.ExpandRate)
	fmt.Fprintf(w, "Tokenizer: chars/%d approximation (relative measure)\n\n",
		r.CharsPerToken)

	// Per-query table
	fmt.Fprintf(w, "Per-query tokens (est.):\n")
	fmt.Fprintf(w, "  %-4s %-10s %-10s %-10s  %s\n", "#", "summary", "snippet", "full", "query")
	fmt.Fprintf(w, "  %-4s %-10s %-10s %-10s  %s\n", "---", "-------", "-------", "----", "-----")
	for i, qr := range r.Queries {
		q := qr.Query
		if len(q) > 60 {
			q = q[:57] + "..."
		}
		fmt.Fprintf(w, "  %-4d %-10d %-10d %-10d  %s\n",
			i+1,
			qr.Modes["summary"].Tokens,
			qr.Modes["snippet"].Tokens,
			qr.Modes["full"].Tokens,
			q)
	}

	fmt.Fprintf(w, "\nAggregate (tokens):\n")
	fmt.Fprintf(w, "  %-8s %-10s %-10s %-10s %-10s\n", "mode", "mean", "p50", "p95", "total")
	fmt.Fprintf(w, "  %-8s %-10s %-10s %-10s %-10s\n", "----", "----", "---", "---", "-----")
	for _, mode := range []string{"summary", "snippet", "full"} {
		a := r.Aggregate[mode]
		fmt.Fprintf(w, "  %-8s %-10.1f %-10d %-10d %-10d\n", mode, a.Mean, a.P50, a.P95, a.Total)
	}

	fmt.Fprintf(w, "\nSavings model (summary-then-expand):\n")
	fmt.Fprintf(w, "  summary total           = %d tokens\n", r.SummaryTotal)
	fmt.Fprintf(w, "  full total              = %d tokens\n", r.FullTotal)
	fmt.Fprintf(w, "  expand-rate             = %.2f (fraction of rows drilled down)\n", r.ExpandRate)
	fmt.Fprintf(w, "  blended (summary+expand)= %d tokens\n", r.ExpandBlendTok)
	fmt.Fprintf(w, "  saving vs full          = %.1f%%\n", r.SavingPercent)
	fmt.Fprintf(w, "\nDuration: %d ms\n", r.DurationMs)
}

// resolveDBDir picks the DB directory to benchmark against. Priority:
//  1. If explicitDB is non-empty, use it verbatim (the caller pinned a path,
//     typically from --db in a script/CI). We still validate that
//     vectors.db exists inside so we fail fast with a clean error rather
//     than crashing in OpenStore.
//  2. Else walk up from cwd via heimdall.FindRepoRoot — same logic the
//     hooks and MCP server use — and compose <repoRoot>/.heimdall_db, then
//     pass through heimdall.ModelDBDir(baseDir, model) to land on the
//     right per-model subdirectory (including the "_latest" legacy
//     fallback).
//  3. Else error out pointing the user at the --db flag.
//
// All errors are user-facing strings — main() prints them straight to
// stderr with no wrapping.
func resolveDBDir(explicitDB, cwd, model string) (string, error) {
	if explicitDB != "" {
		if _, err := os.Stat(filepath.Join(explicitDB, "vectors.db")); err != nil {
			return "", fmt.Errorf("cannot find %s/vectors.db: %v", explicitDB, err)
		}
		return explicitDB, nil
	}

	repoRoot := heimdall.FindRepoRoot(cwd)
	if repoRoot == "" {
		return "", fmt.Errorf("no --db flag and CWD is not inside a repo with an .heimdall_db/ — pass --db to benchmark a specific database")
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

// loadQueriesFile reads one query per line, skipping blanks and
// '#'-prefixed comments.
func loadQueriesFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no non-comment queries in %s", path)
	}
	return out, nil
}

func main() {
	var (
		dbFlag      = flag.String("db", "", "path to .heimdall_db/<model>/ directory (must contain vectors.db); when empty, auto-detects from CWD via FindRepoRoot")
		queriesFile = flag.String("queries", "", "optional file with one query per line; default uses built-in list")
		topK        = flag.Int("top-k", 10, "results per search")
		expandRate  = flag.Float64("expand-rate", 0.2, "fraction of results the caller would expand")
		format      = flag.String("format", "text", "text|json")
		ollamaHost  = flag.String("ollama", "http://127.0.0.1:11434", "Ollama endpoint")
		modelFlag   = flag.String("model", "nomic-embed-text", "embedding model name")
	)
	flag.Parse()

	cwd, _ := os.Getwd()
	dbDir, err := resolveDBDir(*dbFlag, cwd, *modelFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	queries := DefaultQueries
	if *queriesFile != "" {
		qs, err := loadQueriesFile(*queriesFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "load queries: %v\n", err)
			os.Exit(1)
		}
		queries = qs
	}

	client := heimdall.NewOllamaClient(*ollamaHost)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Ollama not reachable at %s: %v\n", *ollamaHost, err)
		os.Exit(1)
	}
	embedder := heimdall.NewOllamaEmbedder(client, *modelFlag)

	report, err := runBench(ctx, BenchConfig{
		DBDir:      dbDir,
		Queries:    queries,
		TopK:       *topK,
		ExpandRate: *expandRate,
		Embedder:   embedder,
		ModelName:  *modelFlag,
		Snapshot:   true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "bench failed: %v\n", err)
		os.Exit(1)
	}

	if *format == "json" {
		out, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(out))
		return
	}
	renderText(os.Stdout, report)
}
