# 08 — Destructive-Op Judgment Primitive (Phase 3 Guardrails)

**Status:** Draft. No code yet. Design proposal for the primitive that
unblocks Phase 3 (`PreToolUse(Bash(rm *|git push --force*))`).
**Audience:** human approver + the engineer who will implement T-phase3-*.
**Reads like:** 04-failure-modes.md and 06-decisions.md. Numbered sections,
concrete examples, no aspirational prose.

---

## 1. Goal & Non-Goals

### 1.1 Goal

Give `heimdall-mcp hook pre-tool-use` a deterministic, fast way to classify
a proposed `Bash` command as **safe / warn / block** so Claude Code can
short-circuit obviously-destructive invocations (`rm -rf /`, `git push
--force` against `main`, `git reset --hard` against an unpushed branch)
before the user has to babysit them.

This primitive is the *only* thing standing between the rest of the
Phase 3 hook plumbing and shipping. Wave 1+2 already proved we can
register hooks, time-box them, and route output safely; Phase 3 reuses all
of that. What is missing is the rule engine that turns a string like
`"rm -rf node_modules"` into one of three exit codes.

### 1.2 Non-goals

We are explicitly **not**:

- Building a general policy engine (no OPA, no CEL, no scripting). The
  rules are flat, audit-readable, and shipped in the binary.
- Becoming a security boundary. A motivated human can rename a binary,
  pipe through `bash -c`, or use `xargs` to bypass any string match. We
  do not pretend otherwise. This is **paper-cut prevention**, in line
  with Wave 2's "Claude is using heimdall" framing.
- Trying to *understand* the command's intent via LLM by default
  (latency + non-determinism — see §4).
- Re-implementing what the shell or `git` already protects (e.g.
  `rm: cannot remove '/': Is a directory` is a kernel-level safety
  net we still get for free).
- Asking heimdall to make project-wide architectural calls (e.g.
  "should I run a migration?"). That is `heimdall_recall` territory,
  not a guardrail hook.

---

## 2. Classification Levels

We adopt the smallest set that maps cleanly to Claude Code's PreToolUse
exit-code contract.

| Level | Exit code | stdout | Claude Code behavior | Example |
|---|---|---|---|---|
| `allow` | `0` | empty | Tool runs normally. | `ls -la`, `rm tmp/foo`, `git push origin feature` |
| `warn` | `0` | one-line `## Heimdall guardrail` block | Tool runs, but Claude sees the warning in context and can decide to abort or rephrase. | `rm -rf node_modules` (recoverable), `git reset --hard HEAD~1` (recoverable on `feature/*`) |
| `block` | `2` | empty stdout, **stderr** = one-line reason | Claude Code cancels the tool call and surfaces stderr to the model as feedback. The model can then decide whether to retry differently. | `rm -rf /`, `rm -rf $HOME`, `git push --force origin main`, `git reset --hard` on a branch with unpushed commits matching `^main$|^master$|^release/.+` |

Three levels exactly. No `confirm` (Claude Code's PreToolUse contract has
no built-in interactive prompt — it is exit-code or nothing), no
`allow-but-log-loudly` (degenerate with `warn`).

**Why stderr on `block`?** Per Claude Code's PreToolUse hook contract
(observed via existing docs and the hooks ecosystem; *if the contract
changes we adapt* — see §11 OQ-1), exit code `2` blocks the tool and
passes stderr to the model as actionable feedback. This is the *only*
hook surface in heimdall that uses non-zero exit, and the *only* one that
writes to stderr. Both deviations are deliberate and isolated to this
single command.

---

## 3. Rule Representation

We considered three options. Picked: **static allowlist + denylist
shipped in the Go binary**, with a clean extension point for declarative
rules and LLM-based judgment later.

### 3.1 Option A — static rules in Go (PICKED)

```go
// internal/heimdall/guardrails/rules.go (proposed shape — not implemented)
type Rule struct {
    ID       string         // stable for tests + logs (e.g. "RM_RECURSIVE_HOME")
    Level    Level          // allow | warn | block
    Pattern  *regexp.Regexp // matched against normalized command
    Reason   string         // <=120 chars, becomes stderr/stdout text
    Examples []string       // hand-picked, doubles as test fixtures
}

var defaultRules = []Rule{
    {ID: "RM_RF_ROOT", Level: Block,
     Pattern: regexp.MustCompile(`^\s*rm\s+(-[a-zA-Z]*r[a-zA-Z]*f|-[a-zA-Z]*f[a-zA-Z]*r)\s+/+\s*$`),
     Reason: "rm -rf / would delete the root filesystem"},
    {ID: "RM_RF_HOME", Level: Block,
     Pattern: regexp.MustCompile(`^\s*rm\s+-[a-zA-Z]*rf?[a-zA-Z]*\s+(\$HOME|~)/?\s*$`),
     Reason: "rm -rf $HOME would wipe the user home directory"},
    // ... see §3.4 for the full starter set
}
```

**Why:** matches existing heimdall idioms (`internal/heimdall/verify.go`
sentinels, `internal/heimdall/suppress.go` constants), zero new
dependencies, trivially unit-testable, fast (compiled regex, microseconds
per call), survives `go install` without a config-file dance, and stays
inside Wave 2's "boring infra" voice.

**Cost:** changing a rule requires a release. That is acceptable for v1
— the rule set is small, the failure mode of a stale rule is
"hook keeps blocking something it shouldn't", and the user can always
disable the hook (§9 rollout) or set `HEIMDALL_GUARDRAILS=0`.

### 3.2 Option B — declarative YAML/TOML rules

```yaml
# ~/.config/heimdall/guardrails.yaml — REJECTED for v1
- id: RM_RF_ROOT
  level: block
  pattern: '^\s*rm\s+-rf\s+/+\s*$'
  reason: "rm -rf / would delete the root filesystem"
```

**Pros:** users can edit without rebuilding; community can contribute
rules; we get a config-file surface for free.

**Cons that killed it:** every config surface is a failure mode (parse
errors, schema drift, untrusted patterns). Users who want this can add
custom rules in their own pre-commit hooks or in their own
`PreToolUse` chain. The §10 extension point keeps the door open: we can
add `[guardrails] extra_rules_file = "..."` later without breaking the
in-binary defaults.

### 3.3 Option C — LLM-based judgment

Pipe the command + cwd + recent transcript to a local model, ask "is
this destructive given the project context?", parse the JSON.

**Pros:** can catch genuinely contextual cases ("`rm -rf migrations/`
is fine in a scratch repo, dangerous in a Rails project").

**Cons that killed it for v1:**
- **Latency.** PreToolUse runs synchronously in the user's tool-call
  path. Even a warm Ollama is 100–500 ms (see plan 03 §4 — already
  rejected for `UserPromptSubmit` on the same grounds). Adding that to
  every Bash call would make the editor feel sticky.
- **Non-determinism.** Two identical commands could classify
  differently across runs. A guardrail that flickers is worse than no
  guardrail.
- **Trust ceiling.** Blocking the user requires very high confidence.
  An LLM saying "65% destructive" is unactionable; we would have to
  threshold it, at which point we have rebuilt a rules engine with
  extra steps.

The §10 extension point keeps an LLM judge as a future `level: judge`
rule type that is opt-in and bounded (e.g. only consulted when a
deterministic rule already returned `warn`, never on the hot path for
`allow`).

### 3.4 Concrete starter ruleset (v1)

| ID | Level | Pattern (illustrative — final regex in implementation) | Reason |
|---|---|---|---|
| `RM_RF_ROOT` | block | `^\s*rm\s+-[rRf]+\s+/+\s*$` | `rm -rf /` would wipe the root filesystem. |
| `RM_RF_HOME` | block | `^\s*rm\s+-[rRf]+\s+(\$HOME\|~)/?\s*$` | `rm -rf $HOME` would wipe the user home directory. |
| `RM_RF_STAR_ROOT` | block | `^\s*rm\s+-[rRf]+\s+/\*` | `rm -rf /*` would wipe the root filesystem. |
| `RM_RF_DOT_GIT` | warn | `^\s*rm\s+-[rRf]+\s+\.git\b` | Removing `.git/` discards the entire repo history. Recoverable from `origin` only if pushed. |
| `RM_RF_RECURSIVE_GENERIC` | warn | `^\s*rm\s+-[rRf]+\s+\S+` (and not matched by a more-specific block above) | Recursive deletion. Confirm path before running. |
| `GIT_PUSH_FORCE_PROTECTED` | block | `^\s*git\s+push\s+(--force\|-f)\b.*\b(main\|master\|release/\S+\|prod\|production)\b` | Force-push to a protected branch rewrites shared history. |
| `GIT_PUSH_FORCE_GENERIC` | warn | `^\s*git\s+push\s+(--force\|-f)\b` (and not matched by `GIT_PUSH_FORCE_PROTECTED`) | Force-push rewrites history on the remote. |
| `GIT_PUSH_FORCE_WITH_LEASE` | allow | `^\s*git\s+push\s+--force-with-lease\b` | `--force-with-lease` is the safer form; allow without warning. |
| `GIT_RESET_HARD_PROTECTED` | block | `^\s*git\s+reset\s+--hard\b` *with cwd-side check that current branch matches `^(main\|master\|release/.+)$`* | Hard reset on a protected branch; cannot be undone without a reflog dive. |
| `GIT_RESET_HARD_GENERIC` | warn | `^\s*git\s+reset\s+--hard\b` | Hard reset discards uncommitted changes. |
| `GIT_CLEAN_FDX` | warn | `^\s*git\s+clean\s+-[a-zA-Z]*[fdx][a-zA-Z]*` | `git clean -fdx` removes ignored files (often `.env`). |
| `GIT_BRANCH_FORCE_DELETE` | warn | `^\s*git\s+branch\s+-D\b` | Force-deleting an unmerged branch loses unique commits. |
| `DROP_DATABASE` | block | `(?i)\bdrop\s+database\b` (any wrapper: `psql -c "..."`, `mysql -e "..."`, etc.) | `DROP DATABASE` is irreversible. |
| `KUBECTL_DELETE_PROD` | block | `^\s*kubectl\s+delete\b.*\b(--namespace=prod\|-n\s+prod\|--context=.+prod.+)\b` | Direct prod mutation — flagged as out-of-process per CLAUDE.md "NEVER SSH into prod" rule. |
| `KUBECTL_DELETE_GENERIC` | warn | `^\s*kubectl\s+delete\b` | Cluster-state mutation; confirm namespace/context. |
| `DD_OF_DEV` | block | `^\s*(sudo\s+)?dd\b.*\bof=/dev/(sd\|nvme\|hd)` | `dd` to a raw block device is irreversible and usually wrong. |
| `MKFS_ANY` | block | `^\s*(sudo\s+)?mkfs(\.\w+)?\s+/dev/` | Filesystem creation on a real device. |
| `CHMOD_777_RECURSIVE` | warn | `^\s*chmod\s+-R\s+0?7{3}\b` | Recursive 777 is almost never what you want. |
| `CURL_PIPE_BASH` | warn | `\|\s*(sudo\s+)?(bash\|sh\|zsh)\b` (left side of pipe is `curl` or `wget`) | Piping remote scripts to a shell is a common compromise vector. |

**Allowlist precedence:** every command is checked against `allow` rules
first. If any matches, classification short-circuits to `allow`. This
lets `git push --force-with-lease` win over `GIT_PUSH_FORCE_GENERIC`.

**Block precedence:** after allow, every `block` rule is checked. First
match wins; the rule ID is logged so we can debug "why did this block?"
without re-running.

**Warn precedence:** if no block matched, every `warn` rule is checked.
First match wins.

**Default:** `allow` (no rule fired = nothing destructive recognized).
This is the §1.2 paper-cut-protector contract restated as a defaults
choice.

### 3.5 Command normalization (before pattern match)

A regex over raw stdin is fragile against `  rm   -rf  /` (multiple
spaces) and `rm -fr /` (flag re-ordering). We normalize first:

1. Trim leading/trailing whitespace.
2. Collapse runs of horizontal whitespace to a single space.
3. Strip `env VAR=val ...` and `nice ...`/`time ...` prefixes (so
   `time rm -rf /` still matches `RM_RF_ROOT`). Whitelist of strippable
   prefixes is hardcoded; do not strip arbitrary commands.
4. **Do not** expand `$HOME`, `~`, globs, or subshells. The patterns
   above explicitly cover both `$HOME` and `~`. Globs we deliberately
   leave alone — pretending we know what `rm -rf $TMP/*` will do is
   exactly the over-confidence trap §1.2 forbids.
5. **Do not** parse `bash -c "..."` payloads. If a command launches
   another shell with a string, classify the *outer* command. Treating
   the inner string as code gives users a predictable bypass anyway
   (just escape further) and adds parsing surface we do not need.

Normalization is a pure function with golden-file tests (§7).

---

## 4. Dry-Run / Explain CLI

`heimdall-mcp hooks explain-command "<bash command>"` classifies a
command without executing anything. Output:

```text
$ heimdall-mcp hooks explain-command "rm -rf node_modules"
level:   warn
rule:    RM_RF_RECURSIVE_GENERIC
reason:  Recursive deletion. Confirm path before running.
matched: rm -rf node_modules
```

```text
$ heimdall-mcp hooks explain-command "git push --force origin main"
level:   block
rule:    GIT_PUSH_FORCE_PROTECTED
reason:  Force-push to a protected branch rewrites shared history.
matched: git push --force origin main
```

```text
$ heimdall-mcp hooks explain-command "ls -la"
level:   allow
rule:    -
reason:  -
matched: ls -la
```

Flags:
- `--format=json` — machine-readable for tests and `hooks doctor`.
- `--rules` — list all rules in the binary (id, level, pattern, reason).
  Useful for users who want to know what is checked.
- `--cwd <path>` — override the working directory used by cwd-aware
  rules (e.g. `GIT_RESET_HARD_PROTECTED` checks the current branch).
  Defaults to `os.Getwd()`.
- `--branch <name>` — override the detected branch (test ergonomics +
  CI determinism). Defaults to `git rev-parse --abbrev-ref HEAD` from
  the resolved cwd, with a graceful "unknown" fallback.

Exit codes (per OQ-5: this is an *interactive* command, not a hook):
`0` for `allow`/`warn`, `2` for `block`. Lets shell scripts gate on it
trivially:

```sh
heimdall-mcp hooks explain-command "$cmd" --format=json >/dev/null 2>&1 \
  && echo "ok" || echo "blocked"
```

---

## 5. Failure Modes

The primitive must default to `allow` on any internal failure. Per §1.2
this is a paper-cut protector, not a security boundary; a guardrail that
blocks because heimdall crashed is strictly worse than no guardrail.

| # | Failure | Behavior | Log |
|---|---|---|---|
| F1 | Classifier slow (>50 ms wall-clock) | Cancel via `context.WithTimeout`, exit 0 (`allow`), log `WARN guardrail.timeout elapsed=%dms cmd_len=%d`. | yes |
| F2 | Malformed stdin (truncated JSON, missing `tool_input.command`) | Exit 0 (`allow`), log `WARN guardrail.bad_stdin`. | yes |
| F3 | Regex panic (shouldn't happen; rules are compiled at init) | `recover()` in handler, exit 0 (`allow`), log `ERROR guardrail.panic err=%v`. | yes |
| F4 | `HEIMDALL_HOOKS=0` or `.heimdall/hooks.disabled` present | Exit 0 (`allow`), no log (per §5.7 — disable is configured behavior, not failure). | no |
| F5 | `HEIMDALL_GUARDRAILS=0` (new env, scoped just to this hook) | Exit 0 (`allow`), no log. Lets a user keep retrieval hooks but turn off blocking. | no |
| F6 | Rule fires but `Reason` empty (programmer bug) | Treat as `allow`, log `ERROR guardrail.empty_reason rule=%s`. Defensive. | yes |
| F7 | Hook log unwritable (per 04 §6) | Classify normally, drop the log line silently. Never abort the hook on log failure. | no |

Per-tier model (extends 04 §1):

- **Allow** = exit 0, empty stdout, empty stderr.
- **Warn** = exit 0, one-line `## Heimdall guardrail` block on stdout,
  empty stderr.
- **Block** = exit 2, empty stdout, one-line reason on stderr.

The 50 ms timeout is internal: the entire classification must be a
compiled-regex sweep, no I/O except the optional `git rev-parse`
(cached for the process lifetime). 50 ms is generous by 100x; the budget
exists only so a pathological regex (we do not write any) cannot stall
a tool call.

---

## 6. Interaction with OQ-5

OQ-5 (06-decisions.md) says **retrieval hooks always exit 0**, and
explicitly enumerates `hook session-start`, `hook user-prompt`, `hook
post-edit`, `hook stop`, `hook session-end` as the affected handlers.

`hook pre-tool-use` is **not a retrieval hook** — it is a guardrail
hook. The exit-code rules for guardrail hooks are different and
documented here:

> **OQ-5 addendum (Phase 3, this doc):** retrieval hooks exit 0 always;
> guardrail hooks (`hook pre-tool-use`) use `0 / 2` with `2` reserved
> exclusively for `block` classifications. `0` covers both `allow` and
> `warn`. Internal failures (F1–F7 above) collapse to `allow`/exit 0.
> Stderr is empty except for the single-line block reason.

This is the *only* place in the heimdall hook surface where we use exit
2 or write to stderr from a hook. It is justified because PreToolUse's
contract makes exit 2 the *only* mechanism to actually stop the tool —
without it the hook is decorative.

**Cross-doc sync (closed):**
01-architecture.md §2 previously said "Reserve exit 2 for the phase-2
destructive-op hook only." That was stale terminology (it is phase 3,
not phase 2) and has been updated to "phase-3" with a pointer back to
this document. 06-decisions.md OQ-5 has been extended with a sibling
clause for guardrail (PreToolUse) hooks that references this doc as the
authoritative source. This doc remains the detailed reference for the
destructive-op primitive; cross-references are now consistent.

---

## 7. Tests Needed

All Go, all Layer 1 (in-process), all `t.TempDir()` for any filesystem
need. No real `bash`, no real `git`. Same idioms as
`internal/cli/hook_test.go` and `internal/cli/hook_user_prompt_test.go`.

| ID | Test | Asserts |
|---|---|---|
| G1 | `TestNormalizeCommand_*` table-driven | Whitespace collapse, prefix stripping (`time`, `nice`, `env VAR=x`), no `~`/`$HOME` expansion, no `bash -c` parsing. |
| G2 | `TestClassify_AllowDefault` | Empty command → `allow`. `ls -la` → `allow`. `echo hi` → `allow`. |
| G3 | `TestClassify_BlockRules` (one row per `block` rule in §3.4) | Each canonical example classifies as `block` and emits the rule's `ID` and `Reason`. |
| G4 | `TestClassify_WarnRules` (one row per `warn` rule in §3.4) | Each canonical example classifies as `warn`. |
| G5 | `TestClassify_AllowOverridesBlock` | `git push --force-with-lease origin main` → `allow` despite matching the `--force` prefix. |
| G6 | `TestClassify_BlockOverridesWarn` | `rm -rf /` → `block` (RM_RF_ROOT), not `warn` (RM_RF_RECURSIVE_GENERIC). |
| G7 | `TestClassify_FlagOrderInvariant` | `rm -fr /`, `rm -rf /`, `rm -Rf /` all → `block` via `RM_RF_ROOT`. |
| G8 | `TestClassify_BranchAware_Reset` | `git reset --hard` with `--branch=main` → `block`; with `--branch=feature/x` → `warn`. |
| G9 | `TestHookPreToolUse_AllowExit0` | Stdin = allow command → exit 0, empty stdout, empty stderr. |
| G10 | `TestHookPreToolUse_WarnExit0` | Stdin = warn command → exit 0, stdout has `## Heimdall guardrail`, empty stderr. |
| G11 | `TestHookPreToolUse_BlockExit2` | Stdin = block command → exit 2, empty stdout, stderr has reason. |
| G12 | `TestHookPreToolUse_DisabledEnv` | `HEIMDALL_GUARDRAILS=0` → exit 0 (`allow`), empty everything. |
| G13 | `TestHookPreToolUse_DisabledMarker` | `.heimdall/hooks.disabled` present → exit 0 (`allow`), empty everything. |
| G14 | `TestHookPreToolUse_BadStdin` | Truncated JSON → exit 0 (`allow`), log line. |
| G15 | `TestHookPreToolUse_TimeoutFailsOpen` | Injected slow classifier → cancel after 50 ms → exit 0 (`allow`), log line. |
| G16 | `TestExplainCommand_TextFormat` | CLI golden test for the text output in §4. |
| G17 | `TestExplainCommand_JSONFormat` | CLI golden test for `--format=json`. |
| G18 | `TestExplainCommand_ListRules` | `--rules` enumerates all rules with stable IDs. |
| G19 | `TestExplainCommand_ExitCode` | `allow`/`warn` → exit 0; `block` → exit 2. |
| G20 | `TestRollout_ShadowMode` | `HEIMDALL_GUARDRAILS=shadow` → never blocks, only logs (see §9). |
| G21 | `TestRollout_WarnMode` | `HEIMDALL_GUARDRAILS=warn` → block rules degrade to warn. |
| G22 | `TestRollout_BlockMode` | `HEIMDALL_GUARDRAILS=1` (or unset) → full enforcement. |

22 tests, ≥1 per starter rule plus mode/exit-code coverage. Same shape
as Wave 2 phase 1b's 16-test sweep on `hook user-prompt` (`internal/cli/
hook_user_prompt_test.go`).

---

## 8. Rollout Plan

Three modes, one env var, ship in order. Same flavour as the OQ-5
"interactive vs retrieval" split — rollout is a knob, not a release
gate.

| Stage | Env | Behavior |
|---|---|---|
| **1. Shadow** | `HEIMDALL_GUARDRAILS=shadow` (default for first release) | Classifier runs. Block rules collapse to log-only; warn rules collapse to log-only. Hook always exits 0. Goal: collect telemetry on which rules fire in real workflows so we can tune patterns before turning enforcement on. Log line: `INFO guardrail.shadow rule=%s level=%s cmd_hash=%s`. |
| **2. Warn** | `HEIMDALL_GUARDRAILS=warn` | Block rules degrade to warn. Warn rules emit on stdout. Hook still always exits 0. Users see the `## Heimdall guardrail` block in Claude's context but no tools are blocked. |
| **3. Block** | `HEIMDALL_GUARDRAILS=1` (or unset, after the shadow soak) | Full enforcement. Block rules exit 2 with stderr reason. Warn rules emit on stdout. |
| **Off** | `HEIMDALL_GUARDRAILS=0` | Hook exits 0 immediately, classifier never runs. Same precedence as `HEIMDALL_HOOKS=0` (which also disables it transitively). |

**Default for the first release:** `shadow`. We have zero telemetry on
how often the starter rules fire on real Claude Code sessions. Shipping
in shadow mode for ~2 weeks gives `hooks tail --event=pre-tool-use`
enough data to either (a) confirm the rules are tight enough to flip to
`block`, or (b) find the false-positive cases we missed.

**Promotion criteria** (gate to `warn` and then `block`):

1. Zero `ERROR guardrail.*` log lines in the previous 7 days.
2. False-positive rate < 1 per 100 fires (measured by hand-classifying
   the last 100 shadow-fires from `hooks tail`).
3. `hooks doctor` includes a check that `HEIMDALL_GUARDRAILS` is set to
   one of the four documented values (catches typos like
   `HEIMDALL_GUARDRAIL=...`).

**Install behavior:** `install-hooks` adds the `PreToolUse` entry by
default once Phase 3 ships, with the matcher
`Bash` (no inline filter — we want to see all bash commands so the
shadow telemetry is complete; the rules are the filter). The
`--only=...` flag from T14 already supports per-hook opt-out:
`heimdall-mcp install-hooks --only=session-start,user-prompt,post-edit,stop,session-end`
installs the existing five without the new guardrail. Symmetric in
`uninstall-hooks`.

---

## 9. Open Questions

Numbered for citation in the next session's handoff (07-style).

1. **PreToolUse exit-code contract.** This doc assumes `2 = block,
   stderr → model feedback` per the existing hooks ecosystem. We do not
   have a vendored copy of Claude Code's hook docs. Action: confirm
   against current Claude Code docs before writing the implementation
   spec; if it differs (e.g. dedicated `decision: "block"` JSON shape),
   adapt §2 and §6 accordingly. Until then this is a reasonable
   working assumption.
2. **Branch detection in `GIT_RESET_HARD_PROTECTED`.** Calling
   `git rev-parse` on every PreToolUse fire is 5–20 ms (filesystem
   walk to find `.git`). Acceptable inside the 50 ms budget but worth
   measuring once we have a benchmark. Cache by `cwd` for the process
   lifetime if needed.
3. **`kubectl` context detection.** `KUBECTL_DELETE_PROD` matches
   command-line flags but not the active `KUBECONFIG` context. Future
   tightening: shell out to `kubectl config current-context` and match
   against `^prod\|^production`. Skipped in v1 to keep the primitive
   I/O-free.
4. **Rule extension surface.** §3.2 was rejected for v1 but the §10
   extension point promises one. Concrete proposal for v2: a single
   `~/.config/heimdall/guardrails.d/*.yaml` directory; `install-hooks
   --add-rules <file>` validates and installs; rules are
   merge-appended after the in-binary defaults so users cannot weaken
   built-ins, only add to them. Not in v1 scope.
5. **Telemetry shape.** Should we add a dedicated `guardrails_log`
   SQLite table (queryable by `hooks doctor`) or rely on `hooks.log`
   greps? Lean: rely on the existing log file for v1, add a table
   only if the §8 promotion-criteria queries become painful to write.
6. **Allowlist for known-safe destructive operations.** Some users will
   genuinely want `rm -rf node_modules` to never warn. v1 ships no
   per-user allowlist (relying on `HEIMDALL_GUARDRAILS=0` as the
   global escape hatch). Add a `[guardrails] suppress_rules = [...]`
   config knob in v2 if users ask.
7. **Interaction with multi-line bash.** Claude Code can pass
   `tool_input.command` containing `&&`/`;`/`|` chains. Current §3
   patterns are line-anchored to the start (`^\s*`), so
   `cd safe && rm -rf /` would not match `RM_RF_ROOT`. Two options:
   (a) split on `&&|;|\|` and classify each segment, or (b) accept the
   gap because anyone writing chained `rm -rf /` is doing it on
   purpose. Lean: ship with (b), add (a) if shadow telemetry shows
   chained destructives in the wild.

---

## 10. Extension Point (post-v1)

The package layout below preserves room for the rejected options:

```
internal/heimdall/guardrails/
  rules.go         — Rule struct + defaultRules slice (§3.1)
  classify.go      — Normalize + Classify(cmd, ctx) (Level, Rule, error)
  level.go         — Level enum + String()
  cwd.go           — currentBranch(cwd) with process-lifetime cache
  rules_test.go    — G1–G8 tables
  classify_test.go — G2 / G5 / G6 / G7 ordering invariants
internal/cli/
  hook_pre_tool_use.go      — DispatchHook case + handler
  hook_pre_tool_use_test.go — G9–G15
  explain_command.go        — `hooks explain-command` (§4)
  explain_command_test.go   — G16–G19
```

To add LLM-judge support later: add a `Level = "judge"` value, a
`Judge func(ctx, cmd) (Level, string, error)` field on `Rule`, and a
codepath in `Classify` that runs the judge only when (a) a deterministic
rule returned `warn` and (b) the user opted in via
`HEIMDALL_GUARDRAILS=judge`. This keeps the hot path zero-LLM by
default.

To add YAML rules later: add a `loadExtraRules(path)` call that
appends to `defaultRules`, with schema validation that refuses any
rule trying to *weaken* a default (i.e. cannot add an `allow` rule
that overrides a built-in `block`).

---

## 11. Summary

- Three levels (`allow`/`warn`/`block`) → exit codes (`0`/`0`/`2`) per
  Claude Code's PreToolUse contract.
- Static Go rules as the primitive; YAML + LLM judges deferred behind a
  clean extension point (§10).
- Deterministic, fail-open, ≤50 ms, zero new deps. Ship in shadow mode
  by default; promote to warn → block via §8 telemetry-driven gate.
- Adds the *only* exit-2/stderr surface in heimdall's hook fleet —
  isolated, documented, and OQ-5-compatible (§6).
