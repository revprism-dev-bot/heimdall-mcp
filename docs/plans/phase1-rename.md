# Phase 1: Rename openviking → heimdall

**Date:** 2026-04-11
**Status:** Plan
**Scope:** Pure mechanical rename — NO logic changes except migration helpers
**Design Spec:** `docs/superpowers/specs/2026-04-11-heimdall-evolution-design.md`

---

## Table of Contents

1. [Summary of Changes](#1-summary-of-changes)
2. [File-by-File Change Manifest](#2-file-by-file-change-manifest)
3. [Migration Logic](#3-migration-logic)
4. [Execution Order](#4-execution-order)
5. [Test Strategy](#5-test-strategy)
6. [Task Breakdown](#6-task-breakdown)
7. [Spec Discrepancies](#7-spec-discrepancies)
8. [Risk Assessment](#8-risk-assessment)

---

## 1. Summary of Changes

### Rename Mappings

| Category | Old | New |
|---|---|---|
| Go module | `github.com/caio-silva/openviking-mcp` | `github.com/caio-silva/heimdall-mcp` |
| Binary | `openviking-mcp` | `heimdall-mcp` |
| Package dir | `internal/openviking/` | `internal/heimdall/` |
| cmd dir | `cmd/openviking-mcp/` | `cmd/heimdall-mcp/` |
| DB dir | `.viking_db/` | `.heimdall_db/` |
| Config dir | `~/.config/openviking-mcp/` | `~/.config/heimdall-mcp/` |
| Config env var | `OPENVIKING_MCP_CONFIG` | `HEIMDALL_MCP_CONFIG` |
| Registry file | `~/.config/openviking-mcp/projects.json` | `~/.config/heimdall-mcp/projects.json` |
| Log prefix | `openviking-mcp: ` | `heimdall-mcp: ` |

### Tool Name Mappings

| Old | New |
|---|---|
| `search_context` | `heimdall_search` |
| `index_project` | `heimdall_index` |
| `openviking_status` | `heimdall_status` |
| `index_text` | `heimdall_index_text` |
| `list_projects` | `heimdall_projects` |

### String Literal Replacements (non-path)

| Old | New | Context |
|---|---|---|
| `"openviking-mcp"` | `"heimdall-mcp"` | Server info name, log prefix, help text, comments |
| `"OpenViking"` | `"Heimdall"` | Comments, descriptions |
| `".viking_db"` | `".heimdall_db"` | Default DB dir name, exclude patterns |
| `"openviking"` | `"heimdall"` | MCP add command in help/docs |
| `OPENVIKING_MCP_CONFIG` | `HEIMDALL_MCP_CONFIG` | Env var name |

---

## 2. File-by-File Change Manifest

### 2.1 `go.mod`

**Line 1:** Change module path.
```
- module github.com/caio-silva/openviking-mcp
+ module github.com/caio-silva/heimdall-mcp
```

No other changes. Dependencies are unaffected.

---

### 2.2 `cmd/openviking-mcp/main.go` → `cmd/heimdall-mcp/main.go`

**File must be moved** from `cmd/openviking-mcp/main.go` to `cmd/heimdall-mcp/main.go`.

Changes within the file:

| Line | Old | New |
|---|---|---|
| 1 | `// openviking-mcp is a stdio MCP server` | `// heimdall-mcp is a stdio MCP server` |
| 10 | `//	claude mcp add openviking /path/to/openviking-mcp` | `//	claude mcp add heimdall /path/to/heimdall-mcp` |
| 20 | `"github.com/caio-silva/openviking-mcp/internal/cli"` | `"github.com/caio-silva/heimdall-mcp/internal/cli"` |
| 21 | `"github.com/caio-silva/openviking-mcp/internal/config"` | `"github.com/caio-silva/heimdall-mcp/internal/config"` |
| 22 | `"github.com/caio-silva/openviking-mcp/internal/mcp"` | `"github.com/caio-silva/heimdall-mcp/internal/mcp"` |
| 23 | `"github.com/caio-silva/openviking-mcp/internal/registry"` | `"github.com/caio-silva/heimdall-mcp/internal/registry"` |
| 28 | `log.SetPrefix("openviking-mcp: ")` | `log.SetPrefix("heimdall-mcp: ")` |

---

### 2.3 `internal/openviking/` → `internal/heimdall/`

**Entire directory must be renamed** from `internal/openviking/` to `internal/heimdall/`.

All 6 files in this directory change their `package` declaration:

| File | Old | New |
|---|---|---|
| `chunker.go` | `package openviking` | `package heimdall` |
| `embedder.go` | `package openviking` | `package heimdall` |
| `ollama.go` | `package openviking` | `package heimdall` |
| `retriever.go` | `package openviking` | `package heimdall` |
| `store.go` | `package openviking` | `package heimdall` |
| `indexer.go` | `package openviking` | `package heimdall` |

**No other changes inside these files** except:

- `indexer.go:359` — `".viking_db"` in `defaultExcludes` → `".heimdall_db"`

---

### 2.4 `internal/mcp/server.go`

| Line | Old | New |
|---|---|---|
| 1 | `// Package mcp implements the JSON-RPC based MCP server for OpenViking.` | `// Package mcp implements the JSON-RPC based MCP server for Heimdall.` |
| 14 | `"github.com/caio-silva/openviking-mcp/internal/openviking"` | `"github.com/caio-silva/heimdall-mcp/internal/heimdall"` |
| 67 | `"name": "openviking-mcp"` | `"name": "heimdall-mcp"` |
| 77 | `Name: "search_context"` | `Name: "heimdall_search"` |
| 78 | Description referencing `search_context` | Updated description |
| 100 | `Name: "index_project"` | `Name: "heimdall_index"` |
| 101 | Description: `"...call openviking_status..."` + `".viking_db/"` | `"...call heimdall_status..."` + `".heimdall_db/"` |
| 114 | `Name: "openviking_status"` | `Name: "heimdall_status"` |
| 115 | Description: `"...OpenViking context engine..."` | `"...Heimdall context engine..."` |
| 122 | `Name: "index_text"` | `Name: "heimdall_index_text"` |
| 123 | Description: `"...search_context..."` | `"...heimdall_search..."` |
| 148 | `Name: "list_projects"` | `Name: "heimdall_projects"` |
| 149 | Description: `"...OpenViking index registry..."` | `"...Heimdall index registry..."` |
| 179 | `case "search_context":` | `case "heimdall_search":` |
| 181 | `case "index_project":` | `case "heimdall_index":` |
| 183 | `case "openviking_status":` | `case "heimdall_status":` |
| 185 | `case "index_text":` | `case "heimdall_index_text":` |
| 187 | `case "list_projects":` | `case "heimdall_projects":` |
| 213 | `openviking.NewOllamaClient` | `heimdall.NewOllamaClient` |
| 230 | `".viking_db"` | `".heimdall_db"` |
| 234 | `openviking.OpenStore` | `heimdall.OpenStore` |
| 240 | `openviking.NewOllamaEmbedder` | `heimdall.NewOllamaEmbedder` |
| 251 | `openviking.VectorRecord` | `heimdall.VectorRecord` |
| 257 | `openviking.VectorRecord{` | `heimdall.VectorRecord{` |

All `openviking.` package qualifier references → `heimdall.` (**5 references** — verified by grep):
- Line 213: `openviking.NewOllamaClient`
- Line 234: `openviking.OpenStore`
- Line 240: `openviking.NewOllamaEmbedder`
- Line 251: `openviking.VectorRecord` (in `var records []`)
- Line 257: `openviking.VectorRecord{` (in struct literal)

Import path: `"github.com/caio-silva/openviking-mcp/internal/openviking"` → `"github.com/caio-silva/heimdall-mcp/internal/heimdall"`

Other imports:
- `"github.com/caio-silva/openviking-mcp/internal/config"` → `"github.com/caio-silva/heimdall-mcp/internal/config"`
- `"github.com/caio-silva/openviking-mcp/internal/registry"` → `"github.com/caio-silva/heimdall-mcp/internal/registry"`

---

### 2.5 `internal/mcp/tools.go`

| Line | Old | New |
|---|---|---|
| 12 | `"github.com/caio-silva/openviking-mcp/internal/openviking"` | `"github.com/caio-silva/heimdall-mcp/internal/heimdall"` |
| 13 | `"github.com/caio-silva/openviking-mcp/internal/registry"` | `"github.com/caio-silva/heimdall-mcp/internal/registry"` |
| 37 | `// 3. Fallback: cwd/.viking_db/` | `// 3. Fallback: cwd/.heimdall_db/` | *(comment)* |
| 56 | `".viking_db"` | `".heimdall_db"` |
| 127 | `"Use openviking_status to check progress."` | `"Use heimdall_status to check progress."` |
| 164 | `"Use openviking_status to check progress. Call index_project again..."` | `"Use heimdall_status to check progress. Call heimdall_index again..."` |
| 188 | `".viking_db"` | `".heimdall_db"` |
| 293 | `".viking_db"` | `".heimdall_db"` |

All `openviking.` package qualifier references → `heimdall.` (**11 references** — verified by grep, lines 29, 63, 69, 70, 177, 189, 199, 200, 206, 274, 295)

---

### 2.6 `internal/mcp/types.go`

| Line | Old | New |
|---|---|---|
| 9 | `"github.com/caio-silva/openviking-mcp/internal/openviking"` | `"github.com/caio-silva/heimdall-mcp/internal/heimdall"` |
| 85 | `*openviking.IndexResult` | `*heimdall.IndexResult` |

---

### 2.7 `internal/config/config.go`

| Line | Old | New |
|---|---|---|
| 9 | `// Config holds the OpenViking MCP server configuration.` | `// Config holds the Heimdall MCP server configuration.` |
| 25 | `".viking_db"` in ExcludePatterns | `".heimdall_db"` |
| 65-67 | Comment: `$OPENVIKING_MCP_CONFIG`, `openviking-mcp` | `$HEIMDALL_MCP_CONFIG`, `heimdall-mcp` |
| 69 | `os.Getenv("OPENVIKING_MCP_CONFIG")` | `os.Getenv("HEIMDALL_MCP_CONFIG")` |
| 77 | `"openviking-mcp"` in config path | `"heimdall-mcp"` |

---

### 2.8 `internal/registry/registry.go`

| Line | Old | New |
|---|---|---|
| 14 | Comment: `// absolute path to .viking_db directory` | `// absolute path to .heimdall_db directory` |
| 31 | `"openviking-mcp"` in registry path | `"heimdall-mcp"` |

---

### 2.9 `internal/cli/cli.go`

| Line | Old | New |
|---|---|---|
| 1 | `// Package cli implements the command-line interface for openviking-mcp.` | `// Package cli implements the command-line interface for heimdall-mcp.` |
| 11 | `"github.com/caio-silva/openviking-mcp/internal/config"` | `"github.com/caio-silva/heimdall-mcp/internal/config"` |
| 12 | `"github.com/caio-silva/openviking-mcp/internal/openviking"` | `"github.com/caio-silva/heimdall-mcp/internal/heimdall"` |
| 13 | `"github.com/caio-silva/openviking-mcp/internal/registry"` | `"github.com/caio-silva/heimdall-mcp/internal/registry"` |
| 34 | `"Usage: openviking-mcp index..."` | `"Usage: heimdall-mcp index..."` |
| 42 | `"Usage: openviking-mcp search..."` | `"Usage: heimdall-mcp search..."` |
| 49-62 | All help text: `openviking-mcp` → `heimdall-mcp`, `.viking_db` → `.heimdall_db`, `openviking` → `heimdall` |
| **58** | **`".viking_db/"` in help text** | **`".heimdall_db/"`** |
| 64 | `"Run: openviking-mcp help"` | `"Run: heimdall-mcp help"` |
| 96 | `".viking_db"` | `".heimdall_db"` |
| 205 | `".viking_db"` | `".heimdall_db"` |
| 250 | `".viking_db"` | `".heimdall_db"` |

**Total `.viking_db` occurrences in cli.go: 4** (lines 58, 96, 205, 250 — verified by grep).

All `openviking.` package qualifier references → `heimdall.` (**12 references** — verified by grep, lines 86, 98, 105, 106, 120, 178, 208, 241, 252, 259, 260, 303)

---

### 2.10 `Makefile`

| Line | Old | New |
|---|---|---|
| 6 | `go build ... -o openviking-mcp ./cmd/openviking-mcp` | `go build ... -o heimdall-mcp ./cmd/heimdall-mcp` |
| 9 | `go install ./cmd/openviking-mcp` | `go install ./cmd/heimdall-mcp` |
| 12 | `rm -f openviking-mcp` | `rm -f heimdall-mcp` |

---

### 2.11 `.gitignore`

| Line | Old | New |
|---|---|---|
| 1 | `/openviking-mcp` | `/heimdall-mcp` |
| 2 | `.viking_db/` | `.heimdall_db/` |

---

### 2.12 `README.md`

Full rewrite of all references. Key changes:

| Old | New |
|---|---|
| `# openviking-mcp` | `# heimdall-mcp` |
| `go install github.com/caio-silva/openviking-mcp@latest` | `go install github.com/caio-silva/heimdall-mcp@latest` |
| `git clone .../openviking-mcp.git` | `git clone .../heimdall-mcp.git` |
| `cd openviking-mcp` | `cd heimdall-mcp` |
| `claude mcp add openviking /path/to/openviking-mcp` | `claude mcp add heimdall /path/to/heimdall-mcp` |
| `search_context` | `heimdall_search` |
| `index_project` | `heimdall_index` |
| `openviking_status` | `heimdall_status` |
| `list_projects` | `heimdall_projects` |
| `index_text` | `heimdall_index_text` |
| `~/.config/openviking-mcp/projects.json` | `~/.config/heimdall-mcp/projects.json` |
| `~/.config/openviking-mcp/config.json` | `~/.config/heimdall-mcp/config.json` |
| `$OPENVIKING_MCP_CONFIG` | `$HEIMDALL_MCP_CONFIG` |
| `$XDG_CONFIG_HOME/openviking-mcp/config.json` | `$XDG_CONFIG_HOME/heimdall-mcp/config.json` |
| `.viking_db/` | `.heimdall_db/` |
| `openviking-mcp index ...` | `heimdall-mcp index ...` |
| `openviking-mcp search ...` | `heimdall-mcp search ...` |
| `openviking-mcp status` | `heimdall-mcp status` |
| `openviking-mcp projects` | `heimdall-mcp projects` |

---

### 2.13 `ollama-env.sh`

No changes needed. This file contains no references to openviking/viking.

---

### 2.14 `docs/superpowers/specs/2026-04-11-heimdall-evolution-design.md`

No changes. This is the design spec — it already references both old and new names intentionally.

---

## 3. Migration Logic

### 3.1 DB Directory Migration (`.viking_db/` → `.heimdall_db/`)

**Where:** Every place that resolves a DB directory (tools.go, cli.go, server.go).

**Logic:** Add a migration helper function in `internal/heimdall/migrate.go` (new file):

```go
package heimdall

import (
    "log"
    "os"
    "path/filepath"
)

// MigrateDBDir renames .viking_db/ to .heimdall_db/ if the old dir exists
// and the new one does not. Uses a lock file to prevent TOCTOU races when
// multiple server instances start simultaneously (SEC-3 fix).
func MigrateDBDir(parentDir string) {
    oldDir := filepath.Join(parentDir, ".viking_db")
    newDir := filepath.Join(parentDir, ".heimdall_db")

    // Quick pre-check (common case: nothing to migrate)
    if _, err := os.Stat(oldDir); err != nil {
        return // old dir doesn't exist, nothing to migrate
    }

    // Acquire exclusive lock file to prevent race conditions
    lockPath := filepath.Join(parentDir, ".heimdall-migrate.lock")
    lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
    if err != nil {
        return // another process is migrating
    }
    defer os.Remove(lockPath)
    defer lock.Close()

    // Re-check conditions after acquiring lock
    if _, err := os.Stat(oldDir); err != nil {
        return // another process already migrated
    }
    if _, err := os.Stat(newDir); err == nil {
        return // new dir already exists, skip
    }

    if err := os.Rename(oldDir, newDir); err != nil {
        log.Printf("heimdall: failed to migrate %s → %s: %v", oldDir, newDir, err)
        return
    }
    log.Printf("heimdall: migrated database directory %s → %s", oldDir, newDir)
}
```

**Call sites:**
- `cmd/heimdall-mcp/main.go` — call `heimdall.MigrateDBDir(cwd)` early in `main()`, before any DB access
- `internal/cli/cli.go` — call `heimdall.MigrateDBDir(absPath)` in `cliIndex()` before opening the store
- The MCP server tools already use the resolved DB path, so the main.go migration covers the MCP path

### 3.2 Config Directory Migration (`~/.config/openviking-mcp/` → `~/.config/heimdall-mcp/`)

**Where:** `internal/config/config.go` in `resolveConfigPath()`.

**Logic:** Add migration in `resolveConfigPath()`:

```go
// migrateConfigDir renames the config directory from openviking-mcp to heimdall-mcp.
// Uses a lock file to prevent TOCTOU races (SEC-3 fix).
func migrateConfigDir() {
    xdgConfig := os.Getenv("XDG_CONFIG_HOME")
    if xdgConfig == "" {
        home, err := os.UserHomeDir()
        if err != nil {
            return // can't determine home dir
        }
        xdgConfig = filepath.Join(home, ".config")
    }

    oldDir := filepath.Join(xdgConfig, "openviking-mcp")
    newDir := filepath.Join(xdgConfig, "heimdall-mcp")

    // Quick pre-check
    if _, err := os.Stat(oldDir); err != nil {
        return // old dir doesn't exist
    }

    // Acquire exclusive lock file
    lockPath := filepath.Join(xdgConfig, ".heimdall-config-migrate.lock")
    lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
    if err != nil {
        return // another process is migrating
    }
    defer os.Remove(lockPath)
    defer lock.Close()

    // Re-check after lock
    if _, err := os.Stat(oldDir); err != nil {
        return
    }
    if _, err := os.Stat(newDir); err == nil {
        return // new dir already exists
    }

    if err := os.Rename(oldDir, newDir); err != nil {
        log.Printf("heimdall: failed to migrate config %s → %s: %v", oldDir, newDir, err)
        return
    }
    log.Printf("heimdall: migrated config directory %s → %s", oldDir, newDir)
}
```

**Call:** `migrateConfigDir()` at the start of `LoadConfig()`.

### 3.3 Registry Migration

The registry file lives inside the config directory (`~/.config/openviking-mcp/projects.json`). The config directory migration (3.2) handles this automatically — when the directory is renamed, the registry file moves with it.

However, the registry entries contain `DBPath` values that may still reference `.viking_db`. These existing entries will continue to work as-is because they store absolute paths and the actual directories may not have been renamed yet. The DB directory migration (3.1) runs per-project, so as each project is accessed, its `.viking_db` will be migrated to `.heimdall_db`. The registry entries' `DBPath` will be stale after migration but the next `heimdall_index` call will re-register with the new path.

**Enhancement (optional but recommended):** After migrating the config directory, scan registry entries and update any `DBPath` that ends in `.viking_db` to `.heimdall_db`:

```go
func migrateRegistryPaths(reg *Registry) {
    changed := false
    for i, p := range reg.Projects {
        if filepath.Base(p.DBPath) == ".viking_db" {
            newDB := filepath.Join(filepath.Dir(p.DBPath), ".heimdall_db")
            reg.Projects[i].DBPath = newDB
            changed = true
        }
    }
    if changed {
        reg.Save()
        log.Printf("heimdall: updated registry paths from .viking_db to .heimdall_db")
    }
}
```

---

## 4. Execution Order

The rename must happen in a specific order to keep the codebase compilable at each step.

### Step 1: Rename package directory `internal/openviking/` → `internal/heimdall/`

```bash
git mv internal/openviking internal/heimdall
```

Update all `package openviking` declarations to `package heimdall` in the 6 files.

**The code will NOT compile after this step** — all import paths still reference the old path.

### Step 2: Update `go.mod` module path

```
module github.com/caio-silva/heimdall-mcp
```

### Step 3: Update all import paths across all Go files

Every file that imports `github.com/caio-silva/openviking-mcp/internal/...` must be updated to `github.com/caio-silva/heimdall-mcp/internal/...`.

Files affected:
- `cmd/heimdall-mcp/main.go` (4 imports)
- `internal/mcp/server.go` (3 imports)
- `internal/mcp/tools.go` (2 imports)
- `internal/mcp/types.go` (1 import)
- `internal/cli/cli.go` (3 imports)

Also update all `openviking.XXX` package qualifier references to `heimdall.XXX` in:
- `internal/mcp/server.go` (**5 references** — verified by grep)
- `internal/mcp/tools.go` (**11 references** — verified by grep)
- `internal/mcp/types.go` (1 reference)
- `internal/cli/cli.go` (12 references)

### Step 4: Rename cmd directory `cmd/openviking-mcp/` → `cmd/heimdall-mcp/`

```bash
git mv cmd/openviking-mcp cmd/heimdall-mcp
```

### Step 5: Update all string literals and comments

This can be done in any order. Update:
- Tool names in `server.go` (handleToolsList, handleToolsCall)
- Tool descriptions in `server.go`
- Server info name in `server.go`
- Config paths in `config.go`
- Registry paths in `registry.go`
- DB paths (`.viking_db` → `.heimdall_db`) in all files
- CLI help text in `cli.go`
- Comments in all files
- Env var name in `config.go`

### Step 6: Add migration logic

- Create `internal/heimdall/migrate.go`
- Add `migrateConfigDir()` to `config.go`
- Add `MigrateDBDir()` calls in `main.go` and `cli.go`

### Step 7: Update non-Go files

- `Makefile`
- `.gitignore`
- `README.md`

### Step 8: Verify

```bash
go build ./cmd/heimdall-mcp
go vet ./...
go test ./... -race -count=1
```

---

## 5. Test Strategy

### 5.1 Compilation Verification

```bash
go build ./cmd/heimdall-mcp
go vet ./...
```

### 5.2 Existing Tests

```bash
go test ./... -race -count=1 -timeout 60s
```

If any tests exist, they must pass. Currently no test files are present in the codebase, so this step is a safety check.

### 5.3 String Audit

After all changes, verify no stale references remain:

```bash
# Must return ZERO results (excluding the design spec and this plan)
grep -rn "openviking\|OpenViking\|viking_db\|OPENVIKING" \
  --include="*.go" --include="*.md" --include="Makefile" --include=".gitignore" \
  . | grep -v "docs/superpowers/specs/" | grep -v "docs/plans/"
```

### 5.4 Migration Testing (manual)

1. **DB migration test:**
   - Create a `.viking_db/` directory with a `vectors.db` file
   - Run `heimdall-mcp status`
   - Verify `.viking_db/` was renamed to `.heimdall_db/`
   - Verify stderr shows migration message

2. **Config migration test:**
   - Create `~/.config/openviking-mcp/config.json` with custom config
   - Run `heimdall-mcp status`
   - Verify `~/.config/openviking-mcp/` was renamed to `~/.config/heimdall-mcp/`
   - Verify the config is still loaded correctly

3. **Idempotency test:**
   - Run migration again — should be a no-op (no errors, no messages)

4. **Both exist test:**
   - Create both `.viking_db/` and `.heimdall_db/`
   - Run `heimdall-mcp status`
   - Verify `.viking_db/` is left untouched (no overwrite)

### 5.5 MCP Protocol Test

Send JSON-RPC requests via stdin and verify tool names:

```bash
echo '{"jsonrpc":"2.0","id":1,"method":"initialize"}' | ./heimdall-mcp
# Verify: serverInfo.name == "heimdall-mcp"

echo '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' | ./heimdall-mcp
# Verify: tool names are heimdall_search, heimdall_index, heimdall_status, heimdall_index_text, heimdall_projects
```

### 5.6 Binary Name Test

```bash
make build
ls -la heimdall-mcp  # must exist
ls -la openviking-mcp 2>/dev/null  # must NOT exist
```

---

## 6. Task Breakdown

### Task 1: Directory and Module Rename (Steps 1-4)
**Estimated changes:** ~20 lines across go.mod + git mv commands
**Dependencies:** None
**Subtasks:**
- 1a. `git mv internal/openviking internal/heimdall`
- 1b. Update all `package openviking` → `package heimdall` (6 files)
- 1c. Update `go.mod` module path
- 1d. Update all import paths (5 files, 13 imports total)
- 1e. Update all `openviking.XXX` package qualifiers (4 files, **29 references**: server.go=5, tools.go=11, types.go=1, cli.go=12)
- 1f. `git mv cmd/openviking-mcp cmd/heimdall-mcp`

### Task 2: Tool Names and Server Info (Step 5 partial)
**Estimated changes:** ~30 lines in `internal/mcp/server.go`
**Dependencies:** Task 1
**Subtasks:**
- 2a. Rename tool names in `handleToolsList()` (5 tools)
- 2b. Rename tool names in `handleToolsCall()` switch (5 cases)
- 2c. Update server info name in `handleInitialize()`
- 2d. Update all tool descriptions

### Task 3: String Literals — DB Paths (Step 5 partial)
**Estimated changes:** ~15 lines across 5 files
**Dependencies:** Task 1
**Subtasks:**
- 3a. `.viking_db` → `.heimdall_db` in `config.go` ExcludePatterns
- 3b. `.viking_db` → `.heimdall_db` in `indexer.go` defaultExcludes
- 3c. `.viking_db` → `.heimdall_db` in `tools.go` (3 occurrences)
- 3d. `.viking_db` → `.heimdall_db` in `cli.go` (**4 occurrences** — lines 58, 96, 205, 250)
- 3e. `.viking_db` → `.heimdall_db` in `registry.go` comment

### Task 4: String Literals — Config Paths and Env Vars (Step 5 partial)
**Estimated changes:** ~8 lines in `config.go` and `registry.go`
**Dependencies:** Task 1
**Subtasks:**
- 4a. `OPENVIKING_MCP_CONFIG` → `HEIMDALL_MCP_CONFIG` in `config.go`
- 4b. `openviking-mcp` → `heimdall-mcp` in config dir path (`config.go`)
- 4c. `openviking-mcp` → `heimdall-mcp` in registry path (`registry.go`)

### Task 5: Comments and Help Text (Step 5 partial)
**Estimated changes:** ~25 lines across 4 files
**Dependencies:** Task 1
**Subtasks:**
- 5a. Update all comments referencing OpenViking/openviking
- 5b. Update all CLI help text in `cli.go`
- 5c. Update doc comments in `main.go`
- 5d. Update status/progress messages referencing old tool names

### Task 6: Migration Logic (Step 6)
**Estimated changes:** ~60 lines (new file + modifications)
**Dependencies:** Tasks 1-5
**Subtasks:**
- 6a. Create `internal/heimdall/migrate.go` with `MigrateDBDir()`
- 6b. Add `migrateConfigDir()` to `config.go`
- 6c. Add `MigrateDBDir()` call in `main.go`
- 6d. Add `MigrateDBDir()` call in `cli.go` (index command)

### Task 7: Non-Go Files (Step 7)
**Estimated changes:** ~50 lines across 3 files
**Dependencies:** Task 1
**Subtasks:**
- 7a. Update `Makefile`
- 7b. Update `.gitignore`
- 7c. Update `README.md`

### Task 8: Verification (Step 8)
**Dependencies:** Tasks 1-7
**Subtasks:**
- 8a. `go build ./cmd/heimdall-mcp`
- 8b. `go vet ./...`
- 8c. `grep` audit for stale references
- 8d. MCP protocol smoke test

---

## 7. Spec Discrepancies

The design spec and the actual codebase have some differences. The plan follows the **actual codebase** where they diverge:

| Spec Says | Code Actually Uses | Plan Uses |
|---|---|---|
| Config at `~/.openviking/` | `~/.config/openviking-mcp/` (XDG) | `~/.config/heimdall-mcp/` (XDG) |
| Registry at `~/.openviking/registry.json` | `~/.config/openviking-mcp/projects.json` | `~/.config/heimdall-mcp/projects.json` |
| Migrate `~/.openviking/` → `~/.heimdall/` | N/A (never existed) | Migrate `~/.config/openviking-mcp/` → `~/.config/heimdall-mcp/` |

The spec's `~/.openviking/` convention was likely a simplification. The actual code uses the XDG convention (`~/.config/openviking-mcp/`), and we should maintain that pattern with the new name.

---

## 8. Risk Assessment

### Low Risk
- **Import path changes:** Mechanical, compiler catches any mistakes immediately.
- **Package qualifier changes:** Mechanical, compiler catches mismatches.
- **String literal changes:** Low risk since tool names are matched in switch statements — a mismatch results in "unknown tool" errors, caught by smoke test.

### Medium Risk
- **Migration logic:** New code that touches the filesystem. Mitigated by:
  - Only runs if old path exists AND new path does NOT exist (no data loss possible)
  - Logs all actions to stderr
  - Idempotent by design
- **Registry path staleness:** After DB migration, registry entries still point to old `.viking_db` paths. Mitigated by: entries get updated on next `heimdall_index`, and search falls back to CWD-based resolution.

### No Risk
- **go.sum:** Does not need manual changes. `go mod tidy` after renaming will handle it.
- **ollama-env.sh:** No changes needed.
- **Git history:** `git mv` preserves file history.

---

## Appendix: Complete File List

| # | File | Action | Changes |
|---|---|---|---|
| 1 | `go.mod` | Edit | Module path |
| 2 | `cmd/openviking-mcp/main.go` | Move + Edit | → `cmd/heimdall-mcp/main.go`, imports, comments, log prefix |
| 3 | `internal/openviking/chunker.go` | Move + Edit | → `internal/heimdall/chunker.go`, package name |
| 4 | `internal/openviking/embedder.go` | Move + Edit | → `internal/heimdall/embedder.go`, package name |
| 5 | `internal/openviking/ollama.go` | Move + Edit | → `internal/heimdall/ollama.go`, package name |
| 6 | `internal/openviking/retriever.go` | Move + Edit | → `internal/heimdall/retriever.go`, package name |
| 7 | `internal/openviking/store.go` | Move + Edit | → `internal/heimdall/store.go`, package name |
| 8 | `internal/openviking/indexer.go` | Move + Edit | → `internal/heimdall/indexer.go`, package name + `.viking_db` |
| 9 | `internal/mcp/server.go` | Edit | Imports, qualifiers, tool names, descriptions, server info, comments |
| 10 | `internal/mcp/tools.go` | Edit | Imports, qualifiers, DB paths, status messages |
| 11 | `internal/mcp/types.go` | Edit | Import, qualifier |
| 12 | `internal/config/config.go` | Edit | Comment, exclude pattern, env var, config path, migration fn |
| 13 | `internal/registry/registry.go` | Edit | Comment, registry path |
| 14 | `internal/cli/cli.go` | Edit | Comment, imports, qualifiers, help text, DB paths |
| 15 | `internal/heimdall/migrate.go` | **Create** | DB directory migration function |
| 16 | `Makefile` | Edit | Binary name, cmd path |
| 17 | `.gitignore` | Edit | Binary name, DB dir |
| 18 | `README.md` | Edit | All references |

**Total: 17 existing files modified, 1 new file created.**
**2 directories renamed via git mv.**
