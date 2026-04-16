# Code Review: Rename openviking -> heimdall (Phase 1)

**Reviewer:** Rename Review Agent (5 independent review passes)  
**Date:** 2026-04-11  
**Scope:** Completeness of rename, migration logic, import paths, tool names, build verification  

---

## Summary

The rename from openviking-mcp to heimdall-mcp is **complete and correct** across all Go source files, Makefile, .gitignore, and README.md. Build (`go build ./...`), vet (`go vet ./...`), and all tests (`go test ./... -race`) pass clean.

---

## 1. Completeness — Stale Reference Audit

### Grep Results: `openviking|viking_db|OPENVIKING|OpenViking` in Go files

Only **legitimate migration references** remain:

| File | Line | Reference | Verdict |
|---|---|---|---|
| `internal/config/config.go:109` | `// migrateConfigDir renames the config directory from openviking-mcp to heimdall-mcp.` | CORRECT — comment documenting migration |
| `internal/config/config.go:121` | `oldDir := filepath.Join(xdgConfig, "openviking-mcp")` | CORRECT — migration source path |
| `internal/cli/cli.go:85` | `// Migrate legacy .viking_db directory to .heimdall_db` | CORRECT — comment documenting migration |
| `internal/heimdall/migrate.go:9` | `// MigrateDBDir renames .viking_db/ to .heimdall_db/` | CORRECT — comment documenting migration |
| `internal/heimdall/migrate.go:13` | `oldDir := filepath.Join(parentDir, ".viking_db")` | CORRECT — migration source path |
| `cmd/heimdall-mcp/main.go:31` | `// Migrate legacy .viking_db directory to .heimdall_db in CWD` | CORRECT — comment documenting migration |

**No stale references found in Go code.** All `openviking`/`viking_db` references are in migration code or migration comments — exactly where they belong.

### Makefile

Zero references to openviking/viking_db. Binary target is `heimdall-mcp`, install target is `./cmd/heimdall-mcp`. PASS.

### .gitignore

Contains `/heimdall-mcp` and `.heimdall_db/`. No stale openviking entries. PASS.

### README.md

All references are `heimdall-mcp`/`.heimdall_db`/`heimdall_search` etc. The only `openviking` references are in the "Migration from openviking-mcp" section (lines 125-130), which is documentation for users migrating from the old name. PASS.

### docs/ directory

10 markdown files in `docs/` contain openviking references. These are all **plan documents and prior review documents** — historical artifacts that should not be modified. PASS.

---

## 2. Migration Logic

### `internal/heimdall/migrate.go` — DB Directory Migration

| Check | Result |
|---|---|
| Lock file prevents TOCTOU race | PASS — `os.OpenFile` with `O_CREATE|O_EXCL` is atomic |
| Lock file cleaned up on exit | PASS — `defer os.Remove(lockPath)` + `defer lock.Close()` |
| Pre-check before lock (fast path) | PASS — avoids unnecessary lock acquisition |
| Re-check after lock (race protection) | PASS — both old-exists and new-not-exists verified |
| Idempotent | PASS — if old doesn't exist OR new already exists, returns cleanly |
| Error handling | PASS — logs failure, does not panic or crash |
| Called from correct locations | PASS — called from `main.go` (CWD) and `cli.go:cliIndex` (target path) |

### `internal/config/config.go` — Config Directory Migration

| Check | Result |
|---|---|
| Lock file prevents TOCTOU race | PASS — same pattern as DB migration |
| Lock file cleaned up on exit | PASS |
| Pre-check + re-check after lock | PASS |
| Idempotent | PASS |
| Called on startup | PASS — `LoadConfig()` calls `migrateConfigDir()` before resolving config path |
| XDG_CONFIG_HOME respected | PASS — checks env var first, falls back to `~/.config` |

---

## 3. Import Paths

All Go files verified to use `github.com/caio-silva/heimdall-mcp/internal/...`:

| File | Imports |
|---|---|
| `cmd/heimdall-mcp/main.go` | `internal/cli`, `internal/config`, `internal/heimdall`, `internal/mcp`, `internal/registry` |
| `internal/mcp/server.go` | `internal/config`, `internal/heimdall`, `internal/registry` |
| `internal/mcp/tools.go` | `internal/heimdall`, `internal/registry` |
| `internal/mcp/types.go` | `internal/heimdall` |
| `internal/mcp/memory_tools.go` | `internal/heimdall` |
| `internal/cli/cli.go` | `internal/config`, `internal/heimdall`, `internal/registry` |
| `internal/mcp/tools_test.go` | `internal/config`, `internal/registry` |
| `internal/mcp/integration_test.go` | `internal/config`, `internal/heimdall`, `internal/registry` |
| `internal/mcp/memory_tools_test.go` | `internal/config`, `internal/heimdall`, `internal/registry` |

**No stale `openviking-mcp` import paths.** PASS.

---

## 4. Tool Names

All 5 original tools plus 4 new tools verified in `internal/mcp/server.go`:

| Tool Name | Listed in `handleToolsList` | Dispatched in `handleToolsCall` | Status |
|---|---|---|---|
| `heimdall_search` | line 80 | line 314 | PASS |
| `heimdall_index` | line 111 | line 316 | PASS |
| `heimdall_status` | line 125 | line 318 | PASS |
| `heimdall_index_text` | line 133 | line 320 | PASS |
| `heimdall_projects` | line 179 | line 322 | PASS |
| `heimdall_remember` | line 187 | line 324 | PASS |
| `heimdall_recall` | line 217 | line 326 | PASS |
| `heimdall_ingest_session` | line 249 | line 328 | PASS |
| `heimdall_explain` | line 257 | line 330 | PASS |

No old tool names (`search_context`, `index_project`, `openviking_status`, `index_text`, `list_projects`) found anywhere in Go source. PASS.

---

## 5. Build Verification

| Check | Result |
|---|---|
| `go build ./...` | PASS — clean, no errors |
| `go vet ./...` | PASS — clean, no warnings |
| `go test ./... -race -count=1 -timeout 60s` | PASS — all tests pass |
| `go.mod` module path | `github.com/caio-silva/heimdall-mcp` — CORRECT |

---

## 6. Findings

### LOW: Stale `openviking-mcp` binary in working directory

The old `openviking-mcp` binary (15MB) exists in the project root as an untracked file. The `.gitignore` excludes `/heimdall-mcp` but not `/openviking-mcp`, so it shows in `git status` as untracked. This is a local artifact that should be deleted (`rm openviking-mcp`). It does not affect the build, tests, or any functionality. Deduction: -1.

---

## Scoring

| Category | Deductions | Notes |
|---|---|---|
| Completeness (stale references) | 0 | All Go, Makefile, .gitignore, README clean |
| Migration logic | 0 | Lock-based, idempotent, correct |
| Import paths | 0 | All use `heimdall-mcp` module path |
| Tool names | 0 | All 9 tools correctly named and dispatched |
| Build verification | 0 | build + vet + test all pass |
| LOW: stale binary on disk | -1 | Local artifact, not tracked |

**Quality Score: 99/100** — PASS

---

## Verdict

The rename implementation is thorough and correct. All Go source files, configuration, tool names, import paths, and migration logic are properly updated. The only finding is a cosmetic issue (stale local binary) that does not affect functionality or correctness.
