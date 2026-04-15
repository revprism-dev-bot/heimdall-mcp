# 06 — Locked Decisions for Wave 2 Phase 1a

**Status:** Approved 2026-04-15. Resolves the five open questions from
`00-consolidated-plan.md §8 — Blocks phase 1a`. All Wave 2 agents MUST read
this file before touching code.

---

## Decisions

### OQ-1 — Claude Code unknown-field tolerance on hook entries

**Decision:** Proceed assuming Claude Code tolerates unknown fields
(`"source": "heimdall"` + `"version": N`) on hook entries. **Ship the
command-string fallback anyway** — every installed hook command string
carries `--source=heimdall --version=1` so that marker detection works even if
Claude Code is strict about JSON keys.

**Rationale:** Most JSON config parsers tolerate unknown fields. The
command-string fallback is cheap (≤20 bytes per hook entry), works
unconditionally, and lets `install-hooks` detect "heimdall owns this hook"
without relying on Claude Code's JSON semantics. Belt-and-braces.

**What to implement in T14:** both (a) the `source`/`version` fields on the
entry object and (b) `--source=heimdall --version=1` baked into the command
string.

---

### OQ-2 — Opt-in vs opt-out install

**Decision:** **Opt-in.** `heimdall-mcp install-hooks` is an explicit
user-run command. First-time indexing does **not** auto-modify
`~/.claude/settings.json`.

**Rationale:** Industry standard for dev tools. Silently editing a user's
Claude Code settings on first index would be invasive and erode trust even
with a backup file. Opt-in keeps the "it only does what you asked it to"
contract intact.

**What to implement in T14:** an explicit command flow with a friendly
first-run hint emitted by `heimdall-mcp index` when no hooks are installed
(`"Tip: run \`heimdall-mcp install-hooks\` to have Claude Code call heimdall
automatically."`), not an auto-install.

---

### OQ-3 — Binary name (`heimdall-mcp` vs `heimdall`)

**Decision:** Keep `heimdall-mcp`. Do not rename. No short `heimdall` alias
for Wave 2.

**Rationale:** The existing CLI already exposes this name, users have
muscle-memory and shell history, and the gain of a shorter name is a few
bytes per hook entry in `settings.json`. A later symlink is cheap if users
ask for it.

**What to implement in T14:** hook command strings are `heimdall-mcp hook ...`
verbatim. Do not introduce a shim or alias.

---

### OQ-4 — `settings.json` JSON round-trip fidelity

**Decision:** Use `encoding/json` standard library. Always write a backup
copy before mutating the file. Do **not** vendor `tailscale/hujson` or any
other comment-preserving parser for Wave 2.

**Rationale:** Vendoring a parser for a two-hook install operation is
overkill. Backup-then-rewrite keeps Wave 2 simple, and users who have
comments or unusual formatting in their `settings.json` can run `--dry-run`
first to see the diff. If real users complain post-launch, we can revisit.

**What to implement in T14:**
- Read current `settings.json` via `encoding/json` into a generic
  `map[string]any`.
- Write backup to `settings.json.heimdall-backup-<YYYY-MM-DD-HH-MM-SS>`.
- Mutate the map.
- Write back via `json.MarshalIndent(..., "", "  ")`.
- `--dry-run` flag prints the diff without writing.

---

### OQ-5 — Exit-code taxonomy vs always-0

**Decision:**
- **Retrieval hooks (`hook session-start`, `hook user-prompt`, `hook
  post-edit`, `hook stop`) always exit 0.** Never block Claude Code.
  Errors are logged via `LogHookEvent` but never surfaced as non-zero exit.
- **Interactive commands (`install-hooks`, `uninstall-hooks`, `hooks doctor`,
  `hooks tail`, `hooks cache-clear`, `hooks cache-stats`, `recall`,
  `ingest-session`, `status`, etc.) use real exit codes:** `0` success,
  `1` runtime error, `2` usage error.

**Rationale:** This matches `failure-modes` plan §2 golden rule ("hooks must
never block Claude Code from proceeding") and keeps non-hook CLI commands
discoverable/scriptable as usual. `cli-surface`'s 10/20/30 taxonomy is
rejected — too much structure for too little benefit, and any structured
error detail belongs in the log file, not a shell exit code.

**What to implement in T5, T7 (and later T6, T8):** every handler returns
`int 0` from its top-level function regardless of internal errors. Internal
errors are captured via `LogHookEvent(level="ERROR", ...)`.

---

## Cross-cutting notes for Wave 2 agents

1. **Base branch:** all Wave 2 phase 1a work is cut from `main` at
   `c85f82a` (the Wave 1 merge + TODO update). Verify with
   `git rev-parse --short HEAD` inside your worktree before starting.

2. **Testability (§5.4) is still non-negotiable:** every new handler takes
   `io.Reader`, `io.Writer`, `io.Writer`, `map[string]string`, `[]string`
   and returns `int`. No `os.Exit` inside handlers.

3. **Hook-path vs interactive-path:** retrieval hooks call
   `heimdall.VerifyHookIndex` before any embed, and use `EmbedForHook` (not
   `Embed`) so the Ollama `keep_alive: "10m"` is in place. The regular CLI
   commands do not.

4. **Logging goes through `heimdall.LogHookEvent`:** never write to stderr
   from a hook retrieval handler. Logs must be redacted (§5.9) and bounded
   (§5.8 5 MB cap + one rotation).

5. **No destructive git operations.** No `reset --hard`, no `push --force`,
   no `branch -D`, no `worktree remove --force`. If you encounter a
   merge conflict you can't resolve, report it back and stop.
