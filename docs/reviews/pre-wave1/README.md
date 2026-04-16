# Pre-Wave-1 review artifacts

These three review docs were written on **2026-04-11**, before the Wave 1
hooks integration work started. They describe code shape from a pre-Wave-1
iteration (e.g. `SearchFiltered` before its `ctx context.Context` first
argument was added in T4 / phase 1b). They are kept as historical record
of review gates that were passed at the time, not as a guide to current
code.

| File | Review scope | Historical score |
|---|---|---|
| `code-review-context.md` | Phase 3 typed external context (store/retriever/indexer/tools) | 96/100 |
| `code-review-memory.md` | Phase 2 session memory implementation | — |
| `code-review-rename.md` | Phase 1 `openviking-mcp` → `heimdall-mcp` rename | — |

Anything with a current review gate lives under
[`../wave1/`](../wave1/) (Wave 1 dimension reviews) or the relevant
`docs/plans/hooks/` plan files.
