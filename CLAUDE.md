# Heimdall MCP — project CLAUDE.md

This is a project-scoped CLAUDE.md for the heimdall-mcp repository. It exists
to reinforce the same trigger->action contract that the MCP server's
`initialize` response provides at runtime, so that even before the MCP
handshake completes (or in any tool-use surface that doesn't render server
instructions) the rules are visible.

## Heimdall — when to call which tool

These are imperative rules, not advisory ones. Each pair names the trigger
and the exact tool to call. Generic "be proactive" guidance has been
deliberately removed in favor of named triggers — see PR #79 for the original
rewrite of the MCP instruction block, which is the canonical source.

### `heimdall_remember` — capture decisions in-flight

- **WHEN the user corrects you** ("actually", "no, do X", "don't", "stop",
  "wrong", "instead") -> call `heimdall_remember` the same turn with
  `type: feedback` plus a one-line rule and the reason.
- **WHEN the user teaches you a non-obvious fact** about the project (env
  quirk, build flag, API gotcha, deploy idiosyncrasy, undocumented
  invariant) -> call `heimdall_remember` with `type: project` plus the
  fact and how to apply it. Future-you will not re-derive it.
- **WHEN you discover a workaround** (a hack, a "trick", a step that
  isn't obvious from the code or docs) -> call `heimdall_remember` the
  turn you confirm it works, before you forget the context that made you
  reach for it.
- **WHEN a user-correction loop ends** (you fix something, they confirm
  it's right) -> `heimdall_remember` the rule that prevented re-doing
  the wrong thing. The correction was the signal; the fix is the lesson.

If in doubt, remember. The cost of a forgotten lesson is much higher
than the cost of one extra memory entry.

### `heimdall_search` — read before you Read

- **WHEN you're about to Read a file > 200 lines** or Grep/Glob to
  explore unfamiliar architecture -> call `heimdall_search` first, then
  Read only the ranges that matter.
- **WHEN you don't recognize a function or package name** that just
  appeared in a stack trace, error message, or PR diff -> search for it
  before guessing.

### `heimdall_recall` — start tasks with prior context

- **WHEN starting a task that references past decisions, plans, or
  prior architecture** -> call `heimdall_recall` first. If multiple
  projects are indexed, `heimdall_projects` lists them.
- **WHEN you see a "Last session review" block at SessionStart** ->
  treat it as a task: `heimdall_remember` the items that still matter
  before doing anything else.

### `heimdall_index_text` — capture what you read

- **WHEN a `WebFetch`, `Read`, or external-MCP call (Jira, Slack,
  GitHub, Confluence, email) returns > 500 chars of content** -> call
  `heimdall_index_text` silently (no user-facing mention). Include the
  source URL/ID in metadata so future searches can cite it.

### `heimdall_explain` / `heimdall_status` — diagnostics

- **WHEN search feels wrong** (empty results, surprising ranking) ->
  `heimdall_explain` on the prompt to inspect cosine ranks.
- **WHEN setup feels off** (model mismatch banner, missing index) ->
  `heimdall_status` for the actual state.

## Why this file exists

The MCP `initialize` response carries the canonical version of these rules
(see `internal/mcp/server.go`'s `heimdallInstructions` constant). This file
is a redundant copy at a different surface — the project-tier CLAUDE.md —
specifically because the global CLAUDE.md and the MCP server instructions
operate at different lifecycle points. CLAUDE.md is read at session start
even before the first tool call; the MCP instruction block is read after
the MCP handshake. Having both reduces the chance of drift in either
direction.

If you find yourself updating one set of trigger pairs, update the other
in the same PR. They must stay in sync.

## What is NOT in this file

This file deliberately does NOT contain a "Development Standards" section.
The global `~/.claude/CLAUDE.md` covers PR workflow, quality gates, agent
dispatch protocol, and review process for ALL projects; duplicating it
here would create drift. Project-specific deviations from those defaults
go in `docs/plans/`, not here.
