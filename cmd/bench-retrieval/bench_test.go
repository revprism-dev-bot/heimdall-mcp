//go:build bench

// Package main's smoke test for the bench-retrieval binary. Build-tagged
// `bench` so `go test ./...` doesn't pick it up by default — run via
// `make bench` or `go test -tags=bench ./cmd/bench-retrieval/`.
//
// Why build-tagged: the smoke test builds a tiny in-process fixture index
// and uses a stub embedder, so it's deterministic and fast (<1 s), but
// it adds no value to the main unit-test suite. Gating it keeps the
// default `make test` surface small and the CI matrix focused.
package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// TestBenchRetrieval_Smoke runs the bench harness against a three-chunk
// in-memory fixture with three queries and asserts the summary-then-
// expand model saves >0% vs full. It exists to guard against regressions
// in (a) the bench wiring itself and (b) the WithDetail/SearchFiltered
// API it depends on.
func TestBenchRetrieval_Smoke(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".heimdall_db")
	store, err := heimdall.OpenStore(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	// Three small fixture records with realistic content lengths so the
	// full mode has measurably more bytes than summary. The summary
	// column is pre-populated so the detail=summary path hits the real
	// tier-0 shortcut (Record.Summary replaces Content) rather than the
	// fallback truncation.
	records := []heimdall.VectorRecord{
		{
			ID:         "fix:1:20",
			FilePath:   "internal/heimdall/fix.go",
			StartLine:  1,
			EndLine:    20,
			Content:    strings.Repeat("func FixSomething() error { return nil }\n", 20),
			Kind:       "function",
			Identifier: "FixSomething",
			Summary:    "func FixSomething — fixes something",
			Embedding:  []float32{1, 0, 0},
			ModTime:    1,
		},
		{
			ID:         "hk:5:50",
			FilePath:   "internal/cli/hooks.go",
			StartLine:  5,
			EndLine:    50,
			Content:    strings.Repeat("// detailed hook implementation line goes here\n", 30),
			Kind:       "function",
			Identifier: "SessionStart",
			Summary:    "func SessionStart — hook entrypoint",
			Embedding:  []float32{0, 1, 0},
			ModTime:    1,
		},
		{
			ID:         "st:10:80",
			FilePath:   "internal/heimdall/store.go",
			StartLine:  10,
			EndLine:    80,
			Content:    strings.Repeat("SELECT id, file_path, content FROM entries WHERE ...\n", 40),
			Kind:       "method",
			Identifier: "SearchFiltered",
			Summary:    "func SearchFiltered — filtered search path",
			Embedding:  []float32{0, 0, 1},
			ModTime:    1,
		},
	}
	if err := store.Upsert(records); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	store.Close() // the bench re-opens the dir

	// Stub embedder: we bypass Ollama entirely. Each query maps to one
	// of the three fixture vectors so the search returns deterministic
	// results. The embedder interface is minimal — Embed(ctx, text).
	stub := &heimdall.StubEmbedder{
		Vectors: map[string][]float32{
			"fix something":                {1, 0, 0},
			"session start hook":           {0, 1, 0},
			"search filtered store method": {0, 0, 1},
		},
		Dimension: 3,
	}
	queries := []string{
		"fix something",
		"session start hook",
		"search filtered store method",
	}

	report, err := runBench(context.Background(), BenchConfig{
		DBDir:      dir,
		Queries:    queries,
		TopK:       3,
		ExpandRate: 0.2,
		Embedder:   stub,
		ModelName:  "stub",
		Snapshot:   false, // small fixture, no need to copy
	})
	if err != nil {
		t.Fatalf("runBench: %v", err)
	}

	// Basic shape checks — three queries × three modes.
	if len(report.Queries) != 3 {
		t.Fatalf("expected 3 query reports, got %d", len(report.Queries))
	}
	for _, qr := range report.Queries {
		for _, mode := range []string{"summary", "snippet", "full"} {
			if _, ok := qr.Modes[mode]; !ok {
				t.Errorf("query %q missing mode %q", qr.Query, mode)
			}
		}
	}

	// Core assertion: summary is strictly cheaper than full, and the
	// blended model therefore saves >0% vs full. If this breaks, either
	// WithDetail stopped trimming content or the bench miscounts.
	if report.SummaryTotal >= report.FullTotal {
		t.Errorf("summary tokens (%d) must be < full tokens (%d)",
			report.SummaryTotal, report.FullTotal)
	}
	if report.SavingPercent <= 0 {
		t.Errorf("saving percent must be > 0, got %.2f", report.SavingPercent)
	}

	// Snippet should sit between summary and full — a sanity check that
	// catches the mode mapping getting swapped in future refactors.
	if report.SnippetTotal < report.SummaryTotal {
		t.Errorf("snippet tokens (%d) should not be < summary tokens (%d)",
			report.SnippetTotal, report.SummaryTotal)
	}
	if report.SnippetTotal > report.FullTotal {
		t.Errorf("snippet tokens (%d) should not be > full tokens (%d)",
			report.SnippetTotal, report.FullTotal)
	}

	t.Logf("smoke: summary=%d snippet=%d full=%d saving=%.1f%%",
		report.SummaryTotal, report.SnippetTotal, report.FullTotal, report.SavingPercent)
}
