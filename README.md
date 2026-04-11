# heimdall-mcp

Local semantic code search + persistent memory for Claude Code, powered by [Ollama](https://ollama.ai) embeddings (BGE-M3) and a SQLite vector store.

No cloud APIs. No API keys. Everything runs on your machine.

## Prerequisites

- [Go 1.25+](https://go.dev/dl/)
- [Ollama](https://ollama.ai) installed and running
- An embedding model pulled: `ollama pull nomic-embed-text` (default, recommended)

## Install

```bash
go install github.com/caio-silva/heimdall-mcp@latest
```

Or build from source:

```bash
git clone https://github.com/caio-silva/heimdall-mcp.git
cd heimdall-mcp
make build
```

## Connect to Claude Code

```bash
claude mcp add heimdall /path/to/heimdall-mcp
```

Then just start using Claude. Heimdall auto-indexes your project on first search — no manual setup needed. It also tells Claude to silently index any external content (Jira tickets, Slack messages, PRs, etc.) it reads via other MCP tools.

## MCP Tools

| Tool | Description |
|---|---|
| `heimdall_search` | Semantic search over indexed code, external content, and memories. Auto-indexes on first use. Supports `source_type` and `metadata_filter`. |
| `heimdall_index` | Index a directory in the background. Auto-detects git repos and indexes recent commits. |
| `heimdall_index_text` | Index external content (Jira tickets, docs, PRs, Slack messages, etc.) with typed metadata and relationships. |
| `heimdall_status` | Ollama reachability, model status, index stats, indexing progress, registered projects. |
| `heimdall_projects` | List all projects in the registry. |
| `heimdall_remember` | Store a memory for persistent recall across sessions. Supports tags, types, and project scoping. |
| `heimdall_recall` | Retrieve memories by semantic search with optional type/tag/project filters. |
| `heimdall_ingest_session` | Auto-extract memories from a conversation summary. Deduplicates against existing memories. |
| `heimdall_explain` | Deep diagnostic for a search query — score distribution, source counts, timing, related items. |
| `heimdall_configure` | Get or set Heimdall config. Supports dot-notation keys like `git.depth` or `lifecycle.active_days`. |
| `heimdall_manage_paths` | Add, remove, or list directories to index. |

## Smart Defaults

Heimdall works out of the box with no configuration:

- **Auto-index on first search** — When you search and no index exists, Heimdall starts indexing your current directory automatically. It returns "indexing started" and the next search hits the populated index.
- **Git commit indexing** — If your project has a `.git/` directory, the last 200 commits are indexed automatically alongside your code. Commit messages become searchable.
- **Stale index detection** — If your index is older than 30 minutes, Heimdall triggers a background re-index on the next search. Current results are returned immediately.
- **Silent external content capture** — The MCP initialize response instructs Claude to automatically call `heimdall_index_text` whenever it reads external content from other MCP tools (Jira, Slack, Confluence, GitHub, etc.).

## Session Memory

Heimdall maintains a persistent memory store across sessions:

```
"Remember that the user prefers TDD workflow"
→ heimdall_remember stores it as type: "preference"

"What do I know about this user's preferences?"
→ heimdall_recall finds it via semantic search

"Here's a summary of our conversation..."
→ heimdall_ingest_session auto-extracts and deduplicates memories
```

Memory types: `preference`, `decision`, `fact`, `context`. Memories can be scoped to projects and tagged for filtering.

The memory database lives at `~/.config/heimdall-mcp/memories.db`.

## Content Lifecycle

Indexed content follows a three-stage lifecycle to prevent unbounded growth:

| Stage | Condition | What Happens |
|-------|-----------|--------------|
| **Active** | Accessed within 30 days | Full content, full embedding, scores normally |
| **Archived** | 30–90 days since last access | Content compressed to 200 chars. Still searchable. Claude can re-fetch from source if needed. |
| **Pruned** | >90 days since last access | Deleted entirely |

- **Code and memory entries are exempt** — they never get archived or pruned.
- **Relevance decay** — Search scores factor in freshness: `final_score = cosine_similarity × freshness_weight` (1.0 → 0.5 over 90 days). New content is not penalized.
- **Size cap** — 10,000 chunks per project. When exceeded, the oldest external entries are pruned first.
- **Lifecycle runs automatically** on each search, throttled to once per hour.

All thresholds are configurable via `heimdall_configure`.

## CLI Usage

```bash
# Index a project
heimdall-mcp index /path/to/project

# Index with a custom DB location
heimdall-mcp index /path/to/project --out /tmp/my-index

# Search indexed files
heimdall-mcp search "payment processing logic"

# Check Ollama status and index stats
heimdall-mcp status

# List all registered projects
heimdall-mcp projects

# View full config
heimdall-mcp config get

# View a specific config key
heimdall-mcp config get git.depth

# Set a config value
heimdall-mcp config set git.depth 500
heimdall-mcp config set lifecycle.active_days 14

# Manage indexed paths
heimdall-mcp paths list
heimdall-mcp paths add /path/to/another/project
heimdall-mcp paths remove /path/to/another/project
```

## Configuration

Optional. Heimdall works with zero config — defaults to local Ollama with BGE-M3.

Create `~/.config/heimdall-mcp/config.json` or use `heimdall_configure` / `heimdall-mcp config set`:

```json
{
  "ollamaEndpoint": "http://localhost:11434",
  "model": "nomic-embed-text",
  "contextDepth": 1,
  "maxContextTokens": 4096,
  "excludePatterns": [".git", "node_modules", "vendor", ".heimdall_db", "__pycache__", ".idea"],
  "gitEnabled": true,
  "gitDepth": 200,
  "gitIncludeDiffs": false,
  "gitBranches": [],
  "staleTimeoutMinutes": 30,
  "lifecycleActiveDays": 30,
  "lifecycleArchiveDays": 90,
  "maxChunksPerProject": 10000,
  "indexedPaths": []
}
```

Or set `HEIMDALL_MCP_CONFIG=/path/to/config.json`.

Config precedence:
1. `$HEIMDALL_MCP_CONFIG` env var
2. `$XDG_CONFIG_HOME/heimdall-mcp/config.json`
3. `~/.config/heimdall-mcp/config.json`

### Config keys (for `heimdall_configure` / `config set`)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `git.enabled` | bool | `true` | Index git commits during `heimdall_index` |
| `git.depth` | int | `200` | Number of recent commits to index |
| `git.include_diffs` | bool | `false` | Include diffs in commit content (reserved) |
| `git.branches` | []string | `[]` | Branches to index (empty = current only) |
| `stale_timeout_minutes` | int | `30` | Minutes before an index is considered stale |
| `lifecycle.active_days` | int | `30` | Days before content is archived |
| `lifecycle.archive_days` | int | `90` | Days before content is pruned |
| `max_chunks_per_project` | int | `10000` | Hard cap on chunks per project DB |

## Embedding Models

Heimdall defaults to `nomic-embed-text` (Nomic AI, Apache 2.0, 137M params, 768 dimensions). You can use any Ollama embedding model.

To switch models:
```bash
ollama pull <model-name>
heimdall-mcp config set model <model-name>
```

Then re-index your projects — Heimdall detects model mismatches and warns you if the index was built with a different model.

### Recommended models

| Model | Origin | Size | Dims | Best for |
|-------|--------|------|------|----------|
| `nomic-embed-text` | Nomic AI (US) | 137M | 768 | **Default.** Best balance of quality and speed. |
| `snowflake-arctic-embed:s` | Snowflake (US) | 33M | 384 | Minimal resource usage. |
| `snowflake-arctic-embed` | Snowflake (US) | 110M | 768 | Medium footprint, good quality. |
| `all-minilm` | Microsoft (US) | 33M | 384 | Fastest, smallest. Lower quality. |
| `mxbai-embed-large` | Mixedbread (DE) | 335M | 1024 | High quality, heavier. |
| `bge-m3` | BAAI (CN) | 567M | 1024 | Multilingual, heaviest. |

### Model mismatch protection

Heimdall stores which model was used to build each index. If you change your model without re-indexing, `heimdall_search` returns a warning and `heimdall_status` shows the mismatch. Different models produce incompatible embedding spaces — cosine similarity across models is meaningless.

## Project Registry

When you index a project (via CLI or MCP), it gets registered automatically. The registry maps project names to their database locations.

This means `heimdall_search` works from any working directory — if you're inside a registered project, the correct index is found automatically. You can also pass a `project` parameter to search a specific project by name.

The registry lives at `~/.config/heimdall-mcp/projects.json`.

Resolution order for finding the database:
1. `project` parameter (looked up in registry by name or path)
2. CWD-based lookup (if CWD is inside a registered project)
3. Fallback to `<cwd>/.heimdall_db/`

## Typed External Content

`heimdall_index_text` supports structured types for external content:

```json
{
  "content": "As a user I want to retry failed payments automatically",
  "source": "JIRA-456",
  "url": "https://jira.example.com/browse/JIRA-456",
  "type": "ticket",
  "metadata": {
    "status": "in_progress",
    "priority": "high"
  },
  "relationships": [
    {"type": "implements", "target": "JIRA-400"},
    {"type": "blocks", "target": "JIRA-470"}
  ]
}
```

Source types: `ticket`, `doc`, `pr`, `message`, `changelog`, `note`, `custom`.

Search with filters:
```json
{
  "query": "payment retry",
  "source_type": "ticket",
  "metadata_filter": {"status": "in_progress"}
}
```

When a result has relationships, `heimdall_explain` includes related items with snippets.

## Incremental Indexing

Re-indexing is fast because Heimdall only processes files that actually changed:

1. **Modtime check** — If the file's modification time hasn't changed since it was last indexed, skip it. This is the fast path and handles most cases.
2. **Content hash fallback** — If the modtime changed but the SHA-256 hash of the file content is the same (e.g. `git checkout`, copied DB, `touch`), skip it anyway.
3. **New files** — Files not in the index are always processed.

This means if you have 500 indexed files and change 1, re-indexing makes 1 embedding call instead of 500.

The CLI shows this in action:
```
heimdall-mcp index /path/to/project
  [0:02] 500/500 files 100% (12 chunks) — done
  Scanned:  500 files
  Indexed:  1 file       ← only the changed one
  Skipped:  499 files (unchanged)
```

## Portable Indexes

The `.heimdall_db/` directory contains a single `vectors.db` SQLite file. You can copy it between machines as long as the same embedding model is used (the model name is stored in the DB and checked automatically). File paths stored in the index are relative, so projects can live at different absolute paths.

## Ollama Tuning

The included `ollama-env.sh` sets environment variables optimized for embedding workloads:

```bash
source ollama-env.sh && ollama serve
```

## How It Works

1. **Index** — `heimdall_index` scans files, chunks them, generates embeddings via Ollama, stores in `.heimdall_db/vectors.db`. Git commits are indexed automatically if `.git/` exists.
2. **Search** — `heimdall_search` embeds your query, finds similar chunks via cosine similarity with freshness decay, updates `last_accessed` timestamps, and triggers lifecycle maintenance.
3. **Incremental** — Re-indexing only processes files that actually changed. Two-tier detection: fast modtime check first, then SHA-256 content hash fallback (handles copied DBs and git clones). Unchanged files are skipped entirely — no embedding calls, no DB writes. Background indexing with progress tracking and stall detection.
4. **Memory** — `heimdall_remember` embeds and stores memories with two-tier deduplication (content hash + semantic similarity). `heimdall_recall` retrieves them via vector search.
5. **Lifecycle** — Stale external content is progressively archived then pruned. Code and memory entries are exempt. Size caps prevent unbounded growth.

## Migration from openviking-mcp

Heimdall automatically migrates existing data:
- `.viking_db/` directories are renamed to `.heimdall_db/` on first access
- `~/.config/openviking-mcp/` is renamed to `~/.config/heimdall-mcp/` on startup

No manual migration steps are needed.

## License

Apache-2.0
