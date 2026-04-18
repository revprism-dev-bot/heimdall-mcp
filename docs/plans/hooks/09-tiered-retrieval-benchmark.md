# Tiered-retrieval token-savings benchmark

Follow-up to **TODO section 2** ("Tiered retrieval"): the last open item was
"Token-savings measurement before/after". Phase 2 shipped `detail=summary|
snippet|full` on `heimdall_search` plus `heimdall_expand(chunk_id)` for
drill-down — this doc pins down how much it actually saves.

## Goal

For a representative query set against a real index, report:

1. Output tokens used by `detail=summary`, `detail=snippet`, `detail=full`.
2. The blended cost a realistic caller pays when starting at `summary` and
   drilling down on a fraction of results via `heimdall_expand`.
3. The percent saving vs the baseline (`detail=full`, the previous default).

## Methodology

### Binary

`cmd/bench-retrieval/main.go` is a standalone CLI. It opens an existing
Heimdall DB **read-only** (snapshot copy, so production `last_accessed`
mutation is impossible), runs each query three times — once per detail
mode with the same top-k — and tallies per-mode output size.

CLI surface:

```
bench-retrieval [--db=<path>]                   # optional; auto-detects from CWD
                [--queries=<file>]              # one per line; default built-in
                [--top-k=10]                    # results per search
                [--expand-rate=0.2]             # fraction caller would expand
                [--format=text|json]            # text (default) or JSON
                [--ollama=http://localhost:11434]
                [--model=nomic-embed-text]
```

When `--db` is omitted, the binary walks up from the current working
directory using `heimdall.FindRepoRoot` (same logic the hooks and MCP
server use) and resolves `<repoRoot>/.heimdall_db/<model>/` via
`heimdall.ModelDBDir`. Pass `--db` explicitly in CI, scripts, or when
benchmarking a database outside the current repo. If neither a `--db`
flag nor a repo-root `.heimdall_db/` can be found, the bench fails with
an actionable error listing the available models (if any).

### Token estimation

A **chars/4 approximation** is used. This is a deliberate simplification:

- The bench measures *relative* savings between modes over the same
  underlying content. Any constant factor cancels out of the saving
  percent, so absolute-tokenizer accuracy is not load-bearing.
- The 4:1 ratio matches the widely-documented ballpark for BPE
  tokenizers on English text (`cl100k_base`, `o200k_base`).
- Swapping to `tiktoken-go` would change the mean/p50/p95 numbers by a
  small multiplicative factor but would not change the **saving
  percent**. The bench is tokenizer-agnostic by construction.

The `estTokens()` helper is the single call site; wiring a real
tokenizer is a three-line change.

### Rendered shape

Each result slice is serialized as indented JSON matching
`SearchResultEnriched` (file, startLine, endLine, content, score,
chunkId, summary, contextPath). This matches what a real MCP client
sees on the wire — so byte-count reflects actual Claude input-token
cost, not just chunk-content size.

### Savings model: summary-then-expand

The bench reports:

```
summary_then_expand = sum(summary_tokens)
                    + expand_rate * (sum(full_tokens) - sum(summary_tokens))
```

Intuition: caller fetches summary for all `top-k` results, then pays the
"full minus summary" delta on `expand_rate` of them via
`heimdall_expand`. Default `expand_rate=0.2` — a reasonable upper bound
for browse-then-drill-down exploration.

`saving_percent = (1 - summary_then_expand / full_total) * 100`.

## Built-in queries

18 queries hard-coded in the binary so the bench is reproducible without
a side file. Four categories:

- **Function lookups**: `HookSessionStart`, `SearchFiltered functional
  options`, `ExpandByID chunk retrieval`, `VerifyHookIndex sentinel
  errors`.
- **Concept lookups**: `tier B suppression store`, `fork setsid detached
  actor`, `hook cache eviction`, `tiered retrieval detail summary
  snippet full`.
- **File-path / code-location lookups**: `how does install.go compute
  the scope`, `where is post-edit debouncer implemented`, `session-start
  hook template rendering`.
- **Error strings / memory types**: `redact home directory in hook log`,
  `skill memory type validation`, `memory type preference decision
  fact`.
- **Dogfood queries**: `what does WithScope do`, `context path hierarchy
  prefix filter`, `last_accessed freshness decay`, `how are embeddings
  stored in sqlite`.

Override with `--queries=file.txt` (one per line, `#` comments ok) when
running against a different corpus.

## Results (2026-04-16, nomic-embed-text, 2546 chunks)

Run against the dogfooded index at `.heimdall_db/nomic-embed-text/`:

```
Heimdall tiered-retrieval token-savings benchmark
=================================================
DB:        /home/noname/Code/heimdall-mcp/.heimdall_db/nomic-embed-text
Model:     nomic-embed-text
Records:   2546  (top-k=10, queries=18, expand-rate=0.20)
Tokenizer: chars/4 approximation (relative measure)

Per-query tokens (est.):
  #    summary    snippet    full        query
  ---  -------    -------    ----        -----
  1    864        1010       3023        HookSessionStart
  2    930        1124       4256        SearchFiltered functional options
  3    900        1109       4191        ExpandByID chunk retrieval
  4    825        1007       3122        VerifyHookIndex sentinel errors
  5    857        1071       4011        tier B suppression store
  6    910        1131       4384        fork setsid detached actor
  7    860        1079       3878        hook cache eviction
  8    845        1107       4030        tiered retrieval detail summary snippet full
  9    867        1161       4524        how does install.go compute the scope
  10   903        1141       3989        where is post-edit debouncer implemented
  11   860        1064       4133        session-start hook template rendering
  12   851        1077       4167        redact home directory in hook log
  13   870        1075       4202        skill memory type validation
  14   929        1133       4384        memory type preference decision fact
  15   908        1130       3622        what does WithScope do
  16   890        1137       3960        context path hierarchy prefix filter
  17   811        1024       3620        last_accessed freshness decay
  18   1027       1237       4720        how are embeddings stored in sqlite

Aggregate (tokens):
  mode     mean       p50        p95        total
  ----     ----       ---        ---        -----
  summary  883.7      867        1027       15907
  snippet  1100.9     1107       1237       19817
  full     4012.0     4030       4720       72216

Savings model (summary-then-expand):
  summary total           = 15907 tokens
  full total              = 72216 tokens
  expand-rate             = 0.20 (fraction of rows drilled down)
  blended (summary+expand)= 27169 tokens
  saving vs full          = 62.4%

Duration: 2196 ms
```

### Interpretation

- **4.5×** difference between `summary` and `full` on mean tokens
  (884 vs 4012) — summary is overwhelmingly cheaper.
- **3.6×** difference between `snippet` and `full` (1101 vs 4012) —
  snippet is a decent middle ground if the caller can't tell in
  advance which rows they'll need to drill into.
- **62.4%** blended saving vs full when a caller fetches summary first
  and expands 20% of results. That's a >2.6× shrink on the search side
  of a turn, which directly reduces input-token budget for the LLM.
- At `expand-rate=1.0` (caller expands *every* result), the blended
  model by construction degenerates back to full-detail cost — i.e.
  the only cost is the per-call summary header, which is negligible.

## How to run

```bash
# Smoke test (fixture + stub embedder; no Ollama needed)
make bench-test

# Real bench against the local index (needs Ollama + indexed DB).
# Auto-detects .heimdall_db/<model>/ from the current repo — works in any
# indexed project, not just heimdall-mcp itself.
make bench

# Or invoke the binary directly for custom parameters
go run ./cmd/bench-retrieval \
  --top-k=10 \
  --expand-rate=0.2 \
  --format=text

# Pin a specific DB (CI / scripts / benchmarking another project's index)
go run ./cmd/bench-retrieval \
  --db=/path/to/.heimdall_db/nomic-embed-text \
  --top-k=10 \
  --expand-rate=0.2
```

By default the bench walks up from the current working directory via
`heimdall.FindRepoRoot` and picks `<repoRoot>/.heimdall_db/<model>/`.
If you've indexed with a non-default model, pass `--model=<name>` so
the resolver lands on the matching subdirectory, or override the path
entirely with `--db=<path>`.

## Reproducing

The bench is fully deterministic given a fixed DB + query list + top-k,
because (a) queries are hard-coded, (b) the DB is snapshot-copied so no
in-place mutation, and (c) `nomic-embed-text` is a deterministic
embedding model at fixed precision. Re-running on the same commit +
same index should yield identical numbers modulo Ollama version
changes.
