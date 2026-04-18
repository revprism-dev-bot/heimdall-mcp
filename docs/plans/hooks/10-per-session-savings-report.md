# Per-Session Savings Report — Implementation Plan

> **✅ SHIPPED 2026-04-17** — all 5 waves merged to `origin/main`:
>
> | Wave | PR | Summary |
> |------|----|---------|
> | A — `session_id` in hook logs | [#25](https://github.com/revprism-dev-bot/heimdall-mcp/pull/25) | Stamp `session=<id>` on every retrieval-hook log fire |
> | B — Transcript parser | [#26](https://github.com/revprism-dev-bot/heimdall-mcp/pull/26) | `heimdall.ParseTranscript` streams Claude Code JSONL |
> | C — Hooklog reader + aggregator | [#27](https://github.com/revprism-dev-bot/heimdall-mcp/pull/27) | `heimdall.ReadHookLog(opts)` + `AggregateHookLogBySession` |
> | D — `sessions` CLI | [#28](https://github.com/revprism-dev-bot/heimdall-mcp/pull/28) | `heimdall-mcp sessions list` + `sessions report` |
> | E — Docs + handoff | (this PR) | README section, handoff + TODO marked shipped |
>
> Plan kept for history; all subsequent `/session` work should start from the `sessions report` output, not this plan.

---

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. **This plan is split into 5 waves with a mandatory context reset between waves** — at the end of each wave, commit, open the PR, merge, then clear context and resume from the next wave header.

**Goal:** add a `heimdall-mcp sessions report` CLI that aggregates per-session metrics — tokens consumed, heimdall context bytes injected, hook cache hit ratio, tool-call breakdown, PreToolUse guardrail verdicts — so we can measure what heimdall actually contributes to each Claude Code session.

**Architecture:** two data sources joined on `session_id`. (1) The Claude Code transcript JSONL at `~/.claude/projects/<slug>/<session>.jsonl` — authoritative for token usage and tool-use history. (2) heimdall's own `hooks.log` at `$XDG_STATE_HOME/heimdall/hooks.log` — authoritative for hook-internal state (cache hits, suppression, guardrail verdicts). Wave A is a prerequisite: `hooks.log` currently only carries `session=<id>` on `stop`/`session-end` events; we need it on every retrieval-hook event so aggregation works. Waves B/C build two pure parsers. Wave D wires them together behind a CLI. Wave E lands docs.

**Tech stack:** Go 1.22+, stdlib only (`encoding/json`, `bufio`, `strings`). No new external deps. Matches existing repo conventions.

---

## Context for the engineer

Read these before touching code:

- `docs/plans/hooks/07-next-session-handoff.md` — current state (main at `65a396b`, 6 hooks live, Phase 3 guardrails in shadow mode by default)
- `docs/plans/hooks/06-decisions.md` — OQ-1..OQ-5 locked decisions. **OQ-5 matters here:** retrieval hooks always exit 0. New CLI (`heimdall-mcp sessions report`) is interactive, not a retrieval hook — it may use exit codes 0/1/2 freely.
- `internal/heimdall/hooklog.go` — existing log format. The line format is:
  ```
  2026-04-16T20:56:22Z INFO event=session-start bullets=5 chunks=2956 model=nomic-embed-text scope= skills=3 stage=ok
  ```
  Keys are sorted alphabetically. Values go through `redactLogValue`. Adding a `session=<id>` key to the kv map in `LogHookEvent` calls is sufficient — the format code already sorts and emits correctly.
- `internal/cli/hook_stop.go` — reference for how `session_id` is parsed from Claude Code hook payloads today (`stopPayload.SessionID` with JSON tag `session_id`). Other retrieval hooks need the same treatment.

## Data-flow diagram

```
 Claude Code session lifecycle
 ┌─────────────────────────────────────────────────────────────────┐
 │                                                                 │
 │  SessionStart                                                   │
 │   ├─ stdin has session_id, cwd    ──┐                           │
 │   │                                 │                           │
 │  UserPromptSubmit (every turn)      │                           │
 │   ├─ stdin has session_id, prompt ──┤                           │
 │   │                                 │                           │
 │  PreToolUse (every Bash)            ├──> LogHookEvent ──────┐   │
 │   ├─ stdin has session_id, cmd    ──┤                       │   │
 │   │                                 │                       │   │
 │  PostToolUse (Edit|Write)           │                       │   │
 │   ├─ stdin has session_id, file   ──┤                       ▼   │
 │   │                                 │           $XDG_STATE_HOME │
 │  Stop (per assistant turn)          │            /heimdall/     │
 │   ├─ stdin has session_id         ──┤             hooks.log     │
 │   │                                 │                       ▲   │
 │  SessionEnd                         │                       │   │
 │   └─ stdin has session_id         ──┘                       │   │
 │                                                             │   │
 │  Full conversation transcript:                              │   │
 │   ~/.claude/projects/<slug>/<session>.jsonl                 │   │
 │   (usage, tool_use, attachments, ...)                       │   │
 │                   │                                         │   │
 └───────────────────┼─────────────────────────────────────────┼───┘
                     │                                         │
                     ▼                                         │
           internal/heimdall/transcript.go      internal/heimdall/hooklog_reader.go
           (Wave B: parse tokens,               (Wave C: parse session=<id>-
            tool-calls, hook_success              tagged entries, aggregate)
            attachment stdout bytes)
                     │                                         │
                     └───────────────────┬─────────────────────┘
                                         ▼
                          internal/cli/sessions.go
                          (Wave D: report + list CLI)
```

## File structure

### Files created

| Path | Responsibility |
|---|---|
| `internal/heimdall/transcript.go` | Parse Claude Code transcript JSONL into `TranscriptSummary`. Stream line-by-line; tolerate unknown event types. |
| `internal/heimdall/transcript_test.go` | Unit tests with JSONL fixtures. |
| `internal/heimdall/testdata/transcript_basic.jsonl` | Fixture: two user turns, two assistant turns with `usage`, one `tool_use`, one `hook_success` attachment. |
| `internal/heimdall/testdata/transcript_heimdall.jsonl` | Fixture: includes `tool_use` with `name=mcp__heimdall__heimdall_search` + hook_success attachments for SessionStart and UserPromptSubmit. |
| `internal/heimdall/hooklog_reader.go` | Parse `hooks.log` lines; filter by session/event/time; aggregate. |
| `internal/heimdall/hooklog_reader_test.go` | Unit tests for parser + filters. |
| `internal/cli/sessions.go` | CLI handler for `heimdall-mcp sessions report` + `sessions list`. Joins transcript + hooklog. |
| `internal/cli/sessions_test.go` | CLI-level tests with stub store/fixtures. |

### Files modified

| Path | Change |
|---|---|
| `internal/cli/hook.go` | Extend `resolveProjectRoot` to also return `session_id`. Add shared helper `logHookEventWithSession`. Thread `sessionID` through every `LogHookEvent` call in `HookSessionStart`. |
| `internal/cli/hook_user_prompt.go` | Extend `readUserPromptStdin` to also return `session_id`. Thread `sessionID` through every `LogHookEvent` call in `HookUserPrompt`. |
| `internal/cli/hook_pre_tool_use.go` | Add `SessionID` field to `preToolUseEvent`. Thread `sessionID` through every `LogHookEvent` call in `HookPreToolUse`. |
| `internal/cli/hook_post_edit.go` | Refactor `extractEditedFilePath` → `extractPostEditStdin` returning `(filePath, sessionID)`. Thread `sessionID` through every `LogHookEvent` call in `HookPostEdit`. |
| `internal/cli/cli.go` | Add `sessions` subcommand dispatch to `RunCLI`. One switch case. |
| `internal/cli/doctor.go` | ✅ SHIPPED post-Wave-E: 14th check `sessions pipeline` reads `hooks.log`, aggregates by session, warns if empty (fresh install), passes with session count otherwise. |
| `README.md` | Add a short "Per-session savings" subsection pointing at `heimdall-mcp sessions report`. |
| `TODO.md` | Strike the per-session tracking follow-up. |
| `docs/plans/hooks/07-next-session-handoff.md` | Mark this feature shipped; note new dogfood workflow (`heimdall-mcp sessions report`). |

---

## Wave A — Thread `session_id` through retrieval hook logs

**Goal of this wave:** every line in `hooks.log` that corresponds to a Claude Code hook fire carries a `session=<uuid>` field. Enables Waves C/D to aggregate. Independently valuable: even without the new CLI, `hooks tail --session=<id>` becomes useful.

**Branch:** `feat/session-id-in-hook-logs`

**Files modified:** `internal/cli/hook.go`, `internal/cli/hook_user_prompt.go`, `internal/cli/hook_pre_tool_use.go`, `internal/cli/hook_post_edit.go`. Tests: `internal/cli/hook_test.go`, `internal/cli/hook_user_prompt_test.go`, `internal/cli/hook_pre_tool_use_test.go`, `internal/cli/hook_post_edit_test.go` (amend existing tests).

### A.1 — Add shared helper `logHookEventWithSession`

**Files:** Modify `internal/cli/hook.go` (append near the bottom, after `hookEmbedder`).

- [ ] **Step 1: Write the failing test** — `internal/cli/hook_test.go`, append:
  ```go
  func TestLogHookEventWithSession_StampsSessionKey(t *testing.T) {
      tmp := t.TempDir()
      t.Setenv("HEIMDALL_HOOK_LOG", filepath.Join(tmp, "hooks.log"))

      logHookEventWithSession("INFO", "test-event", "abc-123", map[string]any{"stage": "ok"})

      data, err := os.ReadFile(filepath.Join(tmp, "hooks.log"))
      if err != nil {
          t.Fatalf("read log: %v", err)
      }
      line := string(data)
      if !strings.Contains(line, "session=abc-123") {
          t.Fatalf("expected session=abc-123 in %q", line)
      }
      if !strings.Contains(line, "stage=ok") {
          t.Fatalf("expected stage=ok in %q", line)
      }
  }

  func TestLogHookEventWithSession_EmptySessionOmitsKey(t *testing.T) {
      tmp := t.TempDir()
      t.Setenv("HEIMDALL_HOOK_LOG", filepath.Join(tmp, "hooks.log"))

      logHookEventWithSession("INFO", "test-event", "", map[string]any{"stage": "ok"})

      data, _ := os.ReadFile(filepath.Join(tmp, "hooks.log"))
      if strings.Contains(string(data), "session=") {
          t.Fatalf("empty sessionID should not emit session= key: %q", string(data))
      }
  }

  func TestLogHookEventWithSession_NilKVStillStamps(t *testing.T) {
      tmp := t.TempDir()
      t.Setenv("HEIMDALL_HOOK_LOG", filepath.Join(tmp, "hooks.log"))

      logHookEventWithSession("INFO", "test-event", "abc-123", nil)

      data, _ := os.ReadFile(filepath.Join(tmp, "hooks.log"))
      if !strings.Contains(string(data), "session=abc-123") {
          t.Fatalf("expected session=abc-123 even when kv nil: %q", string(data))
      }
  }
  ```

- [ ] **Step 2: Verify tests fail**

  Run: `go test ./internal/cli/ -run TestLogHookEventWithSession -v`
  Expected: `undefined: logHookEventWithSession` compile error.

- [ ] **Step 3: Implement the helper** — append to `internal/cli/hook.go` (after the `hookEmbedder` struct, before end of file):
  ```go
  // logHookEventWithSession wraps heimdall.LogHookEvent, stamping session=<id>
  // onto the kv map when non-empty. All retrieval hook handlers route through
  // this helper so hooks.log can be filtered per session by the sessions-report
  // CLI (see internal/cli/sessions.go).
  //
  // Empty sessionID is the intentional signal for "no Claude Code payload"
  // (flag_parse errors, empty_stdin, dry-fires from `hooks doctor`), and those
  // lines deliberately do NOT carry a session key — a downstream "all events
  // without session=" filter catches them as pre-payload noise.
  func logHookEventWithSession(level, event, sessionID string, kv map[string]any) {
      if sessionID != "" {
          if kv == nil {
              kv = map[string]any{"session": sessionID}
          } else {
              kv["session"] = sessionID
          }
      }
      heimdall.LogHookEvent(level, event, kv)
  }
  ```

- [ ] **Step 4: Verify tests pass**

  Run: `go test ./internal/cli/ -run TestLogHookEventWithSession -v -race`
  Expected: 3 PASS.

- [ ] **Step 5: Commit**
  ```bash
  git add internal/cli/hook.go internal/cli/hook_test.go
  git commit -m "feat(hooks): add logHookEventWithSession helper"
  ```

### A.2 — SessionStart: parse session_id + stamp on all log calls

**Files:** Modify `internal/cli/hook.go`.

- [ ] **Step 1: Write the failing test** — `internal/cli/hook_test.go`, append:
  ```go
  func TestHookSessionStart_LogsSessionID(t *testing.T) {
      tmp := t.TempDir()
      t.Setenv("HEIMDALL_HOOK_LOG", filepath.Join(tmp, "hooks.log"))
      t.Setenv("HEIMDALL_HOOKS", "1")

      // Force a non-happy path so the handler exits early but still logs.
      // HEIMDALL_HOOKS=0 would short-circuit before session extraction; we
      // want the resolve_root → ollama_ping path which always logs.
      stdin := strings.NewReader(`{"session_id":"abc-xyz","cwd":"/tmp/does-not-exist"}`)
      var out, errBuf bytes.Buffer
      rc := HookSessionStart(config.Config{OllamaEndpoint: "http://127.0.0.1:1"}, stdin, &out, &errBuf, map[string]string{}, nil, HookSessionStartDeps{})
      if rc != 0 {
          t.Fatalf("expected exit 0, got %d", rc)
      }

      logData, _ := os.ReadFile(filepath.Join(tmp, "hooks.log"))
      if !strings.Contains(string(logData), "session=abc-xyz") {
          t.Fatalf("expected session=abc-xyz in hooks.log:\n%s", string(logData))
      }
  }
  ```

- [ ] **Step 2: Verify it fails**

  Run: `go test ./internal/cli/ -run TestHookSessionStart_LogsSessionID -v`
  Expected: FAIL — no `session=abc-xyz` in log (current handler discards the id).

- [ ] **Step 3: Update `resolveProjectRoot` signature** — in `internal/cli/hook.go`, replace the existing function:
  ```go
  // resolveProjectRoot picks the project root from, in order:
  //  1. --project flag (absolutized)
  //  2. stdin JSON `cwd` field (absolutized)
  //  3. os.Getwd()
  //
  // Also extracts `session_id` from the same stdin payload when present, so
  // callers can thread it into LogHookEvent. SessionID is empty when the flag
  // path is taken (no stdin consumed) or stdin is empty/malformed.
  func resolveProjectRoot(stdin io.Reader, projectFlag string) (cwd, sessionID string) {
      if projectFlag != "" {
          if abs, err := filepath.Abs(projectFlag); err == nil {
              return abs, ""
          }
          return projectFlag, ""
      }
      if stdin != nil {
          var evt struct {
              CWD       string `json:"cwd"`
              SessionID string `json:"session_id"`
          }
          limited := io.LimitReader(stdin, 64*1024)
          data, _ := io.ReadAll(limited)
          if len(bytesTrimSpace(data)) > 0 {
              _ = json.Unmarshal(data, &evt)
              sessionID = evt.SessionID
              if evt.CWD != "" {
                  if abs, err := filepath.Abs(evt.CWD); err == nil {
                      return abs, sessionID
                  }
                  return evt.CWD, sessionID
              }
          }
      }
      wd, err := os.Getwd()
      if err != nil {
          return "", sessionID
      }
      return wd, sessionID
  }
  ```

- [ ] **Step 4: Update `HookSessionStart` to capture sessionID and use helper** — in `internal/cli/hook.go`, replace the `resolveProjectRoot(...)` call and all 8 subsequent `heimdall.LogHookEvent` call sites inside `HookSessionStart`. The pattern at the call site:
  ```go
  // Before:
  rawCWD := resolveProjectRoot(stdin, projectFlag)
  // After:
  rawCWD, sessionID := resolveProjectRoot(stdin, projectFlag)
  ```
  And change each `heimdall.LogHookEvent(level, "session-start", kv)` to `logHookEventWithSession(level, "session-start", sessionID, kv)`. The 8 call sites to update are at current lines (approximate): 155, 201, 216, 239, 262, 272, 284, 320, 336, 347, 360. (Count them in the current file; all inside `HookSessionStart`.)

  Also update the `autoUpgradeHooks` path log call near line 131 — it fires *before* `resolveProjectRoot`, so it has no sessionID. Keep it as `heimdall.LogHookEvent` if applicable; if it doesn't log, leave alone.

- [ ] **Step 5: Run the new test**

  Run: `go test ./internal/cli/ -run TestHookSessionStart_LogsSessionID -v -race`
  Expected: PASS.

- [ ] **Step 6: Run the full SessionStart test file** — confirm no regressions:

  Run: `go test ./internal/cli/ -run TestHookSessionStart -v -race`
  Expected: all existing tests PASS. (If any existing test asserts on exact log contents without a `session=` substring, it should still pass because empty sessionID omits the key.)

- [ ] **Step 7: Commit**
  ```bash
  git add internal/cli/hook.go internal/cli/hook_test.go
  git commit -m "feat(hooks): stamp session=<id> on SessionStart log events"
  ```

### A.3 — UserPromptSubmit: parse session_id + stamp on all log calls

**Files:** Modify `internal/cli/hook_user_prompt.go` and `internal/cli/hook_user_prompt_test.go`.

- [ ] **Step 1: Write the failing test** — append to `internal/cli/hook_user_prompt_test.go`:
  ```go
  func TestHookUserPrompt_LogsSessionID(t *testing.T) {
      tmp := t.TempDir()
      t.Setenv("HEIMDALL_HOOK_LOG", filepath.Join(tmp, "hooks.log"))
      t.Setenv("HEIMDALL_HOOKS", "1")

      stdin := strings.NewReader(`{"session_id":"sess-777","cwd":"/tmp/does-not-exist","prompt":"this is a long enough prompt to pass"}`)
      var out, errBuf bytes.Buffer
      rc := HookUserPrompt(config.Config{OllamaEndpoint: "http://127.0.0.1:1"}, stdin, &out, &errBuf, map[string]string{}, nil, HookUserPromptDeps{})
      if rc != 0 {
          t.Fatalf("expected exit 0, got %d", rc)
      }

      data, _ := os.ReadFile(filepath.Join(tmp, "hooks.log"))
      if !strings.Contains(string(data), "session=sess-777") {
          t.Fatalf("expected session=sess-777 in hooks.log:\n%s", string(data))
      }
  }

  func TestHookUserPrompt_SkipPromptTooShortStillLogsSession(t *testing.T) {
      tmp := t.TempDir()
      t.Setenv("HEIMDALL_HOOK_LOG", filepath.Join(tmp, "hooks.log"))
      t.Setenv("HEIMDALL_HOOKS", "1")

      stdin := strings.NewReader(`{"session_id":"sess-short","cwd":"/tmp","prompt":"ok"}`)
      var out, errBuf bytes.Buffer
      _ = HookUserPrompt(config.Config{}, stdin, &out, &errBuf, map[string]string{}, nil, HookUserPromptDeps{})

      data, _ := os.ReadFile(filepath.Join(tmp, "hooks.log"))
      if !strings.Contains(string(data), "session=sess-short") {
          t.Fatalf("expected session=sess-short in log even on skip:\n%s", string(data))
      }
      if !strings.Contains(string(data), "reason=prompt_too_short") {
          t.Fatalf("expected reason=prompt_too_short in log:\n%s", string(data))
      }
  }
  ```

- [ ] **Step 2: Verify it fails**

  Run: `go test ./internal/cli/ -run TestHookUserPrompt_Logs -v`
  Expected: FAIL on missing `session=`.

- [ ] **Step 3: Update `readUserPromptStdin` signature** — in `internal/cli/hook_user_prompt.go`, replace:
  ```go
  // readUserPromptStdin extracts (prompt, absProjectFromCWD, sessionID) from a
  // UserPromptSubmit event JSON on stdin. Empty stdin, malformed JSON, or
  // missing fields all degrade gracefully — the caller decides what to do
  // with empty returns. If promptOverride is non-empty it is used verbatim,
  // stdin is still read to extract sessionID (debug dry-fires supply session
  // via stdin payload even when prompt comes from flag).
  func readUserPromptStdin(stdin io.Reader, promptOverride string) (prompt, projectFromCWD, sessionID string) {
      if stdin == nil {
          return promptOverride, "", ""
      }
      limited := io.LimitReader(stdin, 256*1024)
      data, _ := io.ReadAll(limited)
      var evt struct {
          Prompt    string `json:"prompt"`
          CWD       string `json:"cwd"`
          SessionID string `json:"session_id"`
      }
      if len(data) > 0 {
          _ = json.Unmarshal(data, &evt)
      }
      sessionID = evt.SessionID
      if evt.CWD != "" {
          if abs, err := filepath.Abs(evt.CWD); err == nil {
              projectFromCWD = abs
          } else {
              projectFromCWD = evt.CWD
          }
      }
      if promptOverride != "" {
          return promptOverride, projectFromCWD, sessionID
      }
      return evt.Prompt, projectFromCWD, sessionID
  }
  ```

- [ ] **Step 4: Update `HookUserPrompt` to use sessionID + helper** — in the same file, update the call site:
  ```go
  // Before:
  prompt, stdinProject := readUserPromptStdin(stdin, promptFlag)
  // After:
  prompt, stdinProject, sessionID := readUserPromptStdin(stdin, promptFlag)
  ```
  Then replace every `heimdall.LogHookEvent(level, "user-prompt", kv)` inside `HookUserPrompt` with `logHookEventWithSession(level, "user-prompt", sessionID, kv)`. There are ~14 call sites. Leave the `fs.Parse` error log (line ~97) alone — it fires before stdin is read.

- [ ] **Step 5: Run the new tests**

  Run: `go test ./internal/cli/ -run TestHookUserPrompt -v -race`
  Expected: all PASS including the two new ones.

- [ ] **Step 6: Commit**
  ```bash
  git add internal/cli/hook_user_prompt.go internal/cli/hook_user_prompt_test.go
  git commit -m "feat(hooks): stamp session=<id> on UserPromptSubmit log events"
  ```

### A.4 — PreToolUse: parse session_id + stamp on all log calls

**Files:** Modify `internal/cli/hook_pre_tool_use.go` and `internal/cli/hook_pre_tool_use_test.go`.

- [ ] **Step 1: Write the failing test** — append to `internal/cli/hook_pre_tool_use_test.go`:
  ```go
  func TestHookPreToolUse_LogsSessionID(t *testing.T) {
      tmp := t.TempDir()
      t.Setenv("HEIMDALL_HOOK_LOG", filepath.Join(tmp, "hooks.log"))

      payload := `{"session_id":"sess-abc","tool_name":"Bash","tool_input":{"command":"ls -la"},"cwd":"/tmp"}`
      stdin := strings.NewReader(payload)
      var out, errBuf bytes.Buffer

      rc := HookPreToolUse(config.Config{}, stdin, &out, &errBuf, map[string]string{"HEIMDALL_GUARDRAILS": "shadow"}, nil, HookPreToolUseDeps{})
      if rc != 0 {
          t.Fatalf("expected exit 0 in shadow mode, got %d", rc)
      }

      data, _ := os.ReadFile(filepath.Join(tmp, "hooks.log"))
      if !strings.Contains(string(data), "session=sess-abc") {
          t.Fatalf("expected session=sess-abc:\n%s", string(data))
      }
      if !strings.Contains(string(data), "class=allow") {
          t.Fatalf("expected class=allow:\n%s", string(data))
      }
  }
  ```

- [ ] **Step 2: Verify it fails**

  Run: `go test ./internal/cli/ -run TestHookPreToolUse_LogsSessionID -v`
  Expected: FAIL.

- [ ] **Step 3: Add `SessionID` field to `preToolUseEvent`** — in `internal/cli/hook_pre_tool_use.go`, replace:
  ```go
  type preToolUseEvent struct {
      SessionID string `json:"session_id"`
      ToolName  string `json:"tool_name"`
      ToolInput struct {
          Command string `json:"command"`
      } `json:"tool_input"`
      CWD string `json:"cwd"`
  }
  ```

- [ ] **Step 4: Thread sessionID through all log calls in `HookPreToolUse`** — update every `heimdall.LogHookEvent(level, "pre-tool-use", kv)` inside the handler to `logHookEventWithSession(level, "pre-tool-use", evt.SessionID, kv)`. Exception: the `"stage": "flag_parse"` log (before JSON parse) and `"stage": "panic"` deferred recover (doesn't have evt in scope) — leave those as `heimdall.LogHookEvent`. There are ~6 affected call sites inside `HookPreToolUse`; all fire *after* `json.Unmarshal(data, &evt)` so `evt.SessionID` is in scope.

- [ ] **Step 5: Run the tests**

  Run: `go test ./internal/cli/ -run TestHookPreToolUse -v -race`
  Expected: all PASS including the new one.

- [ ] **Step 6: Commit**
  ```bash
  git add internal/cli/hook_pre_tool_use.go internal/cli/hook_pre_tool_use_test.go
  git commit -m "feat(hooks): stamp session=<id> on PreToolUse log events"
  ```

### A.5 — PostToolUse: parse session_id + stamp on foreground log calls

**Files:** Modify `internal/cli/hook_post_edit.go` and `internal/cli/hook_post_edit_test.go`. (The actor runs detached with no stdin — not touched.)

- [ ] **Step 1: Write the failing test** — append to `internal/cli/hook_post_edit_test.go`:
  ```go
  func TestHookPostEdit_LogsSessionID(t *testing.T) {
      tmp := t.TempDir()
      t.Setenv("HEIMDALL_HOOK_LOG", filepath.Join(tmp, "hooks.log"))

      payload := `{"session_id":"sess-pe","tool_name":"Edit","tool_input":{"file_path":"/nonexistent/path.go"},"cwd":"/tmp"}`
      stdin := strings.NewReader(payload)
      var out, errBuf bytes.Buffer

      deps := PostEditDeps{
          Now:       time.Now,
          Spawn:     func(string) error { return nil },
          UseFlock:  false,
          KillCheck: func(int) bool { return false },
      }
      rc := hookPostEditWithDeps(config.Config{}, stdin, &out, &errBuf, map[string]string{"HEIMDALL_HOOKS": "1"}, []string{"--project", tmp}, deps)
      if rc != 0 {
          t.Fatalf("expected exit 0, got %d", rc)
      }

      data, _ := os.ReadFile(filepath.Join(tmp, "hooks.log"))
      if !strings.Contains(string(data), "session=sess-pe") {
          t.Fatalf("expected session=sess-pe:\n%s", string(data))
      }
  }
  ```

- [ ] **Step 2: Verify it fails**

  Run: `go test ./internal/cli/ -run TestHookPostEdit_LogsSessionID -v`
  Expected: FAIL.

- [ ] **Step 3: Extend `postEditEvent` + add a new extractor** — in `internal/cli/hook_post_edit.go`, replace:
  ```go
  type postEditEvent struct {
      SessionID string `json:"session_id"`
      ToolName  string `json:"tool_name"`
      ToolInput struct {
          FilePath string `json:"file_path"`
      } `json:"tool_input"`
  }

  // extractPostEditStdin parses the PostToolUse event JSON and returns
  // (absolute edited file path, session_id). Either return can be empty when
  // the payload is missing the corresponding field. Kept as a single call so
  // stdin is consumed once.
  func extractPostEditStdin(raw []byte) (filePath, sessionID string) {
      if len(raw) == 0 {
          return "", ""
      }
      var ev postEditEvent
      if err := json.Unmarshal(raw, &ev); err != nil {
          return "", ""
      }
      return strings.TrimSpace(ev.ToolInput.FilePath), strings.TrimSpace(ev.SessionID)
  }
  ```
  **Delete** the old `extractEditedFilePath` function since the new `extractPostEditStdin` subsumes it. Update the one caller inside `hookPostEditWithDeps`:
  ```go
  // Before:
  editedPath := extractEditedFilePath(raw)
  // After:
  editedPath, sessionID := extractPostEditStdin(raw)
  ```

- [ ] **Step 4: Thread sessionID through log calls in `hookPostEditWithDeps`** — update every `heimdall.LogHookEvent(level, "post-edit", kv)` that fires *after* the stdin read to use `logHookEventWithSession(level, "post-edit", sessionID, kv)`. Pre-stdin-read logs (flag parse, no project root) leave as-is. About 6 post-stdin call sites.

- [ ] **Step 5: Run the tests**

  Run: `go test ./internal/cli/ -run TestHookPostEdit -v -race`
  Expected: all PASS including the new one.

- [ ] **Step 6: Commit**
  ```bash
  git add internal/cli/hook_post_edit.go internal/cli/hook_post_edit_test.go
  git commit -m "feat(hooks): stamp session=<id> on PostToolUse log events"
  ```

### A.6 — Full regression + PR

- [ ] **Step 1: Full test sweep**

  Run: `go build ./... && go vet ./... && go test ./... -race -count=1`
  Expected: all PASS. No build or vet errors.

- [ ] **Step 2: Rebuild local binary**

  Run: `go build -o /home/noname/.local/bin/heimdall-mcp ./cmd/heimdall-mcp`

- [ ] **Step 3: Manual dry-fire sanity check**
  ```bash
  tmpdir=$(mktemp -d)
  HEIMDALL_HOOK_LOG=$tmpdir/hooks.log \
    echo '{"session_id":"DRYFIRE-1","cwd":"'"$PWD"'","prompt":"this is a real prompt for dry fire"}' \
    | heimdall-mcp hook user-prompt --source=heimdall --version=1
  grep -c 'session=DRYFIRE-1' $tmpdir/hooks.log
  # Expected: 1 or more
  ```

- [ ] **Step 4: Push branch + open PR**
  ```bash
  git push -u origin feat/session-id-in-hook-logs
  gh pr create --title "feat(hooks): stamp session=<id> on every retrieval hook log line" --body "$(cat <<'EOF'
  ## Summary
  - Extend retrieval-hook payload parsers (SessionStart, UserPromptSubmit, PreToolUse, PostToolUse) to capture `session_id`.
  - Thread it into `hooks.log` via a new `logHookEventWithSession` helper so every fire carries `session=<uuid>`.
  - Unlocks per-session aggregation in the upcoming \`heimdall-mcp sessions report\` CLI (plan: docs/plans/hooks/10-per-session-savings-report.md).

  ## Test plan
  - [ ] \`go test ./... -race\` green
  - [ ] Manual: \`hooks tail --since=10m\` after a real Claude Code turn shows \`session=\` on every line except pre-payload edge cases
  - [ ] \`hooks doctor\` still 12/13 OK (skills-sync warn is unrelated)
  EOF
  )"
  ```

- [ ] **Step 5: Merge PR**

  Wait for CI, squash-merge via `gh pr merge --squash --auto`. Pull main: `git checkout main && git pull`.

### Wave A — Context-reset protocol

Before starting Wave B in a fresh session:
1. Confirm `git log --oneline -1` on `main` shows the Wave A merge.
2. Confirm `go test ./... -race -count=1` is green on main.
3. Confirm `heimdall-mcp hooks tail --since=10m` on a real session shows `session=<uuid>` on post-Wave-A events.
4. **Clear context.** Reopen and re-read this plan from Wave B.

---

## Wave B — Transcript JSONL parser (parallel with Wave C)

**Goal of this wave:** a pure parser `heimdall.ParseTranscript(path) (TranscriptSummary, error)` that reads a Claude Code session transcript and extracts the metrics we need. No CLI wiring.

**Branch:** `feat/transcript-parser` (cut from the post-Wave-A main).

**Files created:** `internal/heimdall/transcript.go`, `internal/heimdall/transcript_test.go`, `internal/heimdall/testdata/transcript_basic.jsonl`, `internal/heimdall/testdata/transcript_heimdall.jsonl`.

### B.1 — Define types + transcript path resolver

- [ ] **Step 1: Write the failing test** — new file `internal/heimdall/transcript_test.go`:
  ```go
  package heimdall

  import (
      "path/filepath"
      "runtime"
      "testing"
  )

  func TestTranscriptPathForSession_BuildsSlug(t *testing.T) {
      got, err := TranscriptPathForSession("/home/alice/Code/proj", "abc-123", "/home/alice")
      if err != nil {
          t.Fatalf("err: %v", err)
      }
      want := filepath.Join("/home/alice", ".claude", "projects", "-home-alice-Code-proj", "abc-123.jsonl")
      if got != want {
          t.Fatalf("got %q want %q", got, want)
      }
  }

  func TestTranscriptPathForSession_EmptySessionErrors(t *testing.T) {
      _, err := TranscriptPathForSession("/home/alice/Code/proj", "", "/home/alice")
      if err == nil {
          t.Fatalf("expected error on empty session id")
      }
  }

  func TestTranscriptPathForSession_WindowsPathIgnoredOnPosix(t *testing.T) {
      if runtime.GOOS == "windows" {
          t.Skip("POSIX-only slug rule")
      }
      got, _ := TranscriptPathForSession("/home/alice/Code/proj", "s", "/home/alice")
      if filepath.Base(filepath.Dir(got)) != "-home-alice-Code-proj" {
          t.Fatalf("unexpected slug dir: %s", got)
      }
  }
  ```

- [ ] **Step 2: Verify it fails**

  Run: `go test ./internal/heimdall/ -run TestTranscriptPath -v`
  Expected: undefined `TranscriptPathForSession`.

- [ ] **Step 3: Create `internal/heimdall/transcript.go`**:
  ```go
  package heimdall

  import (
      "bufio"
      "encoding/json"
      "errors"
      "fmt"
      "io"
      "os"
      "path/filepath"
      "strings"
      "time"
  )

  // TranscriptSummary is the aggregate view of a single Claude Code session
  // transcript. All counts are 0 when no matching events were seen.
  type TranscriptSummary struct {
      SessionID string
      Path      string

      FirstTimestamp time.Time
      LastTimestamp  time.Time

      UserMessages      int
      AssistantMessages int

      TotalInputTokens         int64
      TotalCacheCreationTokens int64
      TotalCacheReadTokens     int64
      TotalOutputTokens        int64

      ToolUseCount      int
      ToolUseByName     map[string]int
      HeimdallToolCalls int // subset: any tool whose name starts with mcp__heimdall__

      HookSuccessBytesByEvent map[string]int64 // e.g. "SessionStart" → 1240
      HookSuccessCountByEvent map[string]int

      ParseErrors int // non-fatal malformed lines
  }

  // TranscriptPathForSession returns the filesystem path Claude Code uses for a
  // transcript file given the working directory and session id. home is injected
  // so tests can exercise the slug rule without depending on os.UserHomeDir.
  func TranscriptPathForSession(cwd, sessionID, home string) (string, error) {
      if sessionID == "" {
          return "", errors.New("empty session id")
      }
      if home == "" {
          h, err := os.UserHomeDir()
          if err != nil {
              return "", fmt.Errorf("home dir: %w", err)
          }
          home = h
      }
      clean := filepath.Clean(cwd)
      slug := strings.ReplaceAll(clean, string(os.PathSeparator), "-")
      return filepath.Join(home, ".claude", "projects", slug, sessionID+".jsonl"), nil
  }
  ```

- [ ] **Step 4: Run tests**

  Run: `go test ./internal/heimdall/ -run TestTranscriptPath -v`
  Expected: PASS.

- [ ] **Step 5: Commit**
  ```bash
  git add internal/heimdall/transcript.go internal/heimdall/transcript_test.go
  git commit -m "feat(transcript): add TranscriptSummary type + path resolver"
  ```

### B.2 — Create basic JSONL fixture

- [ ] **Step 1: Write `internal/heimdall/testdata/transcript_basic.jsonl`** — one event per line (no trailing newline on final line). Each line is valid JSON:
  ```
  {"type":"user","message":{"role":"user","content":"hello world"},"sessionId":"bas-1","timestamp":"2026-01-01T00:00:00Z"}
  {"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi back"}],"usage":{"input_tokens":10,"cache_creation_input_tokens":100,"cache_read_input_tokens":0,"output_tokens":5}},"sessionId":"bas-1","timestamp":"2026-01-01T00:00:01Z"}
  {"type":"user","message":{"role":"user","content":"do thing"},"sessionId":"bas-1","timestamp":"2026-01-01T00:00:02Z"}
  {"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/x"}}],"usage":{"input_tokens":4,"cache_creation_input_tokens":0,"cache_read_input_tokens":200,"output_tokens":8}},"sessionId":"bas-1","timestamp":"2026-01-01T00:00:03Z"}
  {"type":"attachment","attachment":{"type":"hook_success","hookEvent":"SessionStart","stdout":"## Heimdall context\n\n> test bytes"},"sessionId":"bas-1","timestamp":"2026-01-01T00:00:00Z"}
  {"type":"system","subtype":"stop_hook_summary","sessionId":"bas-1"}
  ```

### B.3 — Implement ParseTranscript

- [ ] **Step 1: Write the failing test** — append to `internal/heimdall/transcript_test.go`:
  ```go
  func TestParseTranscript_Basic(t *testing.T) {
      sum, err := ParseTranscript("testdata/transcript_basic.jsonl")
      if err != nil {
          t.Fatalf("parse: %v", err)
      }
      if sum.SessionID != "bas-1" {
          t.Errorf("session id: got %q", sum.SessionID)
      }
      if sum.UserMessages != 2 {
          t.Errorf("user messages: got %d want 2", sum.UserMessages)
      }
      if sum.AssistantMessages != 2 {
          t.Errorf("assistant messages: got %d want 2", sum.AssistantMessages)
      }
      if sum.TotalInputTokens != 14 {
          t.Errorf("input tokens: got %d want 14", sum.TotalInputTokens)
      }
      if sum.TotalCacheCreationTokens != 100 {
          t.Errorf("cache creation: got %d want 100", sum.TotalCacheCreationTokens)
      }
      if sum.TotalCacheReadTokens != 200 {
          t.Errorf("cache read: got %d want 200", sum.TotalCacheReadTokens)
      }
      if sum.TotalOutputTokens != 13 {
          t.Errorf("output: got %d want 13", sum.TotalOutputTokens)
      }
      if sum.ToolUseCount != 1 {
          t.Errorf("tool use count: got %d want 1", sum.ToolUseCount)
      }
      if sum.ToolUseByName["Read"] != 1 {
          t.Errorf("ToolUseByName[Read]: got %d want 1", sum.ToolUseByName["Read"])
      }
      if sum.HookSuccessCountByEvent["SessionStart"] != 1 {
          t.Errorf("hook success count SessionStart: got %d want 1", sum.HookSuccessCountByEvent["SessionStart"])
      }
      wantBytes := int64(len("## Heimdall context\n\n> test bytes"))
      if sum.HookSuccessBytesByEvent["SessionStart"] != wantBytes {
          t.Errorf("hook bytes: got %d want %d", sum.HookSuccessBytesByEvent["SessionStart"], wantBytes)
      }
  }

  func TestParseTranscript_FileMissing(t *testing.T) {
      _, err := ParseTranscript("testdata/does-not-exist.jsonl")
      if err == nil {
          t.Fatalf("expected error")
      }
  }

  func TestParseTranscript_MalformedLineCounted(t *testing.T) {
      // Write a fixture with one valid line and one garbage line to a tempdir.
      tmp := t.TempDir()
      p := filepath.Join(tmp, "t.jsonl")
      content := "{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":\"ok\"},\"sessionId\":\"s\"}\n" +
          "not json at all\n"
      if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
          t.Fatal(err)
      }
      sum, err := ParseTranscript(p)
      if err != nil {
          t.Fatalf("parse returned error for tolerable malformed: %v", err)
      }
      if sum.ParseErrors != 1 {
          t.Errorf("parse errors: got %d want 1", sum.ParseErrors)
      }
      if sum.UserMessages != 1 {
          t.Errorf("user messages: got %d want 1", sum.UserMessages)
      }
  }
  ```

- [ ] **Step 2: Verify it fails**

  Run: `go test ./internal/heimdall/ -run TestParseTranscript -v`
  Expected: undefined `ParseTranscript`.

- [ ] **Step 3: Implement `ParseTranscript`** — append to `internal/heimdall/transcript.go`:
  ```go
  // ParseTranscript streams the Claude Code transcript at path and returns a
  // TranscriptSummary. Unknown event types are ignored. Malformed JSON lines
  // increment ParseErrors but do not abort the parse — the transcript is only
  // ever appended to, so a partial last line is treated as tolerable.
  func ParseTranscript(path string) (TranscriptSummary, error) {
      sum := TranscriptSummary{
          Path:                    path,
          ToolUseByName:           map[string]int{},
          HookSuccessBytesByEvent: map[string]int64{},
          HookSuccessCountByEvent: map[string]int{},
      }

      f, err := os.Open(path)
      if err != nil {
          return sum, fmt.Errorf("open transcript: %w", err)
      }
      defer f.Close()

      r := bufio.NewReaderSize(f, 128*1024)
      for {
          line, rerr := r.ReadBytes('\n')
          if len(line) > 0 {
              applyTranscriptLine(&sum, line)
          }
          if rerr == io.EOF {
              break
          }
          if rerr != nil {
              return sum, fmt.Errorf("read transcript: %w", rerr)
          }
      }
      return sum, nil
  }

  // transcriptEnvelope is the subset of a transcript line we care about.
  type transcriptEnvelope struct {
      Type       string          `json:"type"`
      SessionID  string          `json:"sessionId"`
      Timestamp  string          `json:"timestamp"`
      Message    json.RawMessage `json:"message"`
      Attachment json.RawMessage `json:"attachment"`
  }

  func applyTranscriptLine(sum *TranscriptSummary, raw []byte) {
      trimmed := bytesTrimSpaceTranscript(raw)
      if len(trimmed) == 0 {
          return
      }
      var env transcriptEnvelope
      if err := json.Unmarshal(trimmed, &env); err != nil {
          sum.ParseErrors++
          return
      }
      if sum.SessionID == "" && env.SessionID != "" {
          sum.SessionID = env.SessionID
      }
      if env.Timestamp != "" {
          if ts, err := time.Parse(time.RFC3339, env.Timestamp); err == nil {
              if sum.FirstTimestamp.IsZero() || ts.Before(sum.FirstTimestamp) {
                  sum.FirstTimestamp = ts
              }
              if ts.After(sum.LastTimestamp) {
                  sum.LastTimestamp = ts
              }
          }
      }

      switch env.Type {
      case "user":
          sum.UserMessages++
      case "assistant":
          sum.AssistantMessages++
          applyAssistantMessage(sum, env.Message)
      case "attachment":
          applyAttachment(sum, env.Attachment)
      }
  }

  type assistantMessage struct {
      Content []struct {
          Type string `json:"type"`
          Name string `json:"name"`
      } `json:"content"`
      Usage struct {
          InputTokens              int64 `json:"input_tokens"`
          CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
          CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
          OutputTokens             int64 `json:"output_tokens"`
      } `json:"usage"`
  }

  func applyAssistantMessage(sum *TranscriptSummary, raw json.RawMessage) {
      if len(raw) == 0 {
          return
      }
      var msg assistantMessage
      if err := json.Unmarshal(raw, &msg); err != nil {
          return
      }
      sum.TotalInputTokens += msg.Usage.InputTokens
      sum.TotalCacheCreationTokens += msg.Usage.CacheCreationInputTokens
      sum.TotalCacheReadTokens += msg.Usage.CacheReadInputTokens
      sum.TotalOutputTokens += msg.Usage.OutputTokens
      for _, c := range msg.Content {
          if c.Type != "tool_use" {
              continue
          }
          sum.ToolUseCount++
          name := c.Name
          if name == "" {
              name = "(unnamed)"
          }
          sum.ToolUseByName[name]++
          if strings.HasPrefix(name, "mcp__heimdall__") {
              sum.HeimdallToolCalls++
          }
      }
  }

  type hookSuccessAttachment struct {
      Type      string `json:"type"`
      HookEvent string `json:"hookEvent"`
      Stdout    string `json:"stdout"`
  }

  func applyAttachment(sum *TranscriptSummary, raw json.RawMessage) {
      if len(raw) == 0 {
          return
      }
      var a hookSuccessAttachment
      if err := json.Unmarshal(raw, &a); err != nil {
          return
      }
      if a.Type != "hook_success" || a.HookEvent == "" {
          return
      }
      sum.HookSuccessCountByEvent[a.HookEvent]++
      sum.HookSuccessBytesByEvent[a.HookEvent] += int64(len(a.Stdout))
  }

  // bytesTrimSpace is a tiny byte-level TrimSpace — avoids importing strings
  // for one call site.
  func bytesTrimSpaceTranscript(b []byte) []byte {
      start, end := 0, len(b)
      for start < end && isASCIISpace(b[start]) {
          start++
      }
      for end > start && isASCIISpace(b[end-1]) {
          end--
      }
      return b[start:end]
  }

  func isASCIISpace(c byte) bool {
      return c == ' ' || c == '\t' || c == '\n' || c == '\r'
  }
  ```
  *Note:* there is already a `bytesTrimSpace` in `internal/cli/hook.go`, but that's a different package. This local `bytesTrimSpaceTranscript` keeps `internal/heimdall` self-contained with no cross-package import. Both `applyTranscriptLine` and any other callers in this file must use `bytesTrimSpaceTranscript` consistently.

- [ ] **Step 4: Run tests**

  Run: `go test ./internal/heimdall/ -run TestParseTranscript -v -race`
  Expected: all PASS.

- [ ] **Step 5: Commit**
  ```bash
  git add internal/heimdall/transcript.go internal/heimdall/transcript_test.go internal/heimdall/testdata/transcript_basic.jsonl
  git commit -m "feat(transcript): stream parser for Claude Code transcript JSONL"
  ```

### B.4 — Heimdall-tool fixture + assertions

- [ ] **Step 1: Create `internal/heimdall/testdata/transcript_heimdall.jsonl`**:
  ```
  {"type":"user","message":{"role":"user","content":"explain the hook pipeline"},"sessionId":"hm-1","timestamp":"2026-02-01T00:00:00Z"}
  {"type":"attachment","attachment":{"type":"hook_success","hookEvent":"UserPromptSubmit","stdout":"## Heimdall context\n\nhits from search"},"sessionId":"hm-1","timestamp":"2026-02-01T00:00:00Z"}
  {"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"mcp__heimdall__heimdall_search","input":{"query":"hooks"}},{"type":"tool_use","id":"b","name":"mcp__heimdall__heimdall_recall","input":{"query":"X"}},{"type":"tool_use","id":"c","name":"Read","input":{"file_path":"/x"}}],"usage":{"input_tokens":100,"cache_creation_input_tokens":0,"cache_read_input_tokens":1000,"output_tokens":50}},"sessionId":"hm-1","timestamp":"2026-02-01T00:00:01Z"}
  ```

- [ ] **Step 2: Add test** — append to `internal/heimdall/transcript_test.go`:
  ```go
  func TestParseTranscript_HeimdallToolsCounted(t *testing.T) {
      sum, err := ParseTranscript("testdata/transcript_heimdall.jsonl")
      if err != nil {
          t.Fatalf("parse: %v", err)
      }
      if sum.ToolUseCount != 3 {
          t.Errorf("ToolUseCount: got %d want 3", sum.ToolUseCount)
      }
      if sum.HeimdallToolCalls != 2 {
          t.Errorf("HeimdallToolCalls: got %d want 2", sum.HeimdallToolCalls)
      }
      if sum.ToolUseByName["mcp__heimdall__heimdall_search"] != 1 {
          t.Errorf("heimdall_search: got %d", sum.ToolUseByName["mcp__heimdall__heimdall_search"])
      }
      if sum.HookSuccessBytesByEvent["UserPromptSubmit"] == 0 {
          t.Errorf("expected non-zero UserPromptSubmit bytes")
      }
  }
  ```

- [ ] **Step 3: Run tests**

  Run: `go test ./internal/heimdall/ -run TestParseTranscript -v -race`
  Expected: PASS.

- [ ] **Step 4: Commit**
  ```bash
  git add internal/heimdall/testdata/transcript_heimdall.jsonl internal/heimdall/transcript_test.go
  git commit -m "test(transcript): heimdall-tool + hook_success attachment fixtures"
  ```

### B.5 — Full test sweep + PR

- [ ] **Step 1:** `go build ./... && go vet ./... && go test ./... -race -count=1`
- [ ] **Step 2:** `git push -u origin feat/transcript-parser`
- [ ] **Step 3:** `gh pr create --title "feat(transcript): stream parser for Claude Code transcript JSONL" --body "..."` (body: summary + test plan referencing plan doc).
- [ ] **Step 4:** Merge.

### Wave B — Context-reset protocol

If Wave C is done in parallel by another agent, wait for both to merge before clearing. Otherwise clear after B merges:
1. `git checkout main && git pull` — confirm B merge is present.
2. `go test ./internal/heimdall/... -race -count=1` green on main.
3. **Clear context.** Resume at Wave C (if not yet done) or Wave D.

---

## Wave C — hooks.log reader + aggregator (parallel with Wave B)

**Goal:** `heimdall.ReadHookLog(opts ReadHookLogOpts) ([]HookLogEntry, error)` and `AggregateHookLogBySession([]HookLogEntry) map[string]SessionHookAggregate`.

**Branch:** `feat/hooklog-reader` (cut from post-Wave-A main; **may be developed in parallel with Wave B** — no shared files).

**Files created:** `internal/heimdall/hooklog_reader.go`, `internal/heimdall/hooklog_reader_test.go`.

### C.1 — Parse one line

- [ ] **Step 1: Write the failing test** — new `internal/heimdall/hooklog_reader_test.go`:
  ```go
  package heimdall

  import (
      "strings"
      "testing"
      "time"
  )

  func TestParseHookLogLine_Happy(t *testing.T) {
      line := "2026-04-16T20:56:22Z INFO event=user-prompt bytes=1107 hits=5 model=nomic-embed-text scope= session=abc-xyz skills=3 stage=ok"
      entry, ok := parseHookLogLine(line)
      if !ok {
          t.Fatalf("expected parse ok")
      }
      if entry.Level != "INFO" {
          t.Errorf("level: got %q", entry.Level)
      }
      if entry.Event != "user-prompt" {
          t.Errorf("event: got %q", entry.Event)
      }
      if entry.Session != "abc-xyz" {
          t.Errorf("session: got %q", entry.Session)
      }
      if entry.Fields["stage"] != "ok" {
          t.Errorf("stage: got %q", entry.Fields["stage"])
      }
      want, _ := time.Parse(time.RFC3339, "2026-04-16T20:56:22Z")
      if !entry.Timestamp.Equal(want) {
          t.Errorf("ts: got %v want %v", entry.Timestamp, want)
      }
  }

  func TestParseHookLogLine_NoEventRejected(t *testing.T) {
      _, ok := parseHookLogLine("2026-04-16T20:56:22Z INFO no_event_here=1")
      if ok {
          t.Fatalf("expected reject")
      }
  }

  func TestParseHookLogLine_EmptyString(t *testing.T) {
      _, ok := parseHookLogLine("")
      if ok {
          t.Fatalf("expected reject on empty")
      }
  }

  func TestParseHookLogLine_ValueWithEquals(t *testing.T) {
      line := "2026-04-16T20:56:22Z WARN event=user-prompt err=ollama_embed:_status_400 session=x"
      entry, ok := parseHookLogLine(line)
      if !ok {
          t.Fatalf("parse ok")
      }
      if !strings.Contains(entry.Fields["err"], "ollama_embed") {
          t.Errorf("err: got %q", entry.Fields["err"])
      }
  }
  ```

- [ ] **Step 2: Verify fail**

  Run: `go test ./internal/heimdall/ -run TestParseHookLogLine -v`
  Expected: undefined `parseHookLogLine`.

- [ ] **Step 3: Implement parser** — new `internal/heimdall/hooklog_reader.go`:
  ```go
  package heimdall

  import (
      "bufio"
      "fmt"
      "os"
      "sort"
      "strings"
      "time"
  )

  // HookLogEntry is a parsed line from hooks.log (see formatHookLogLine in
  // hooklog.go for the producer). Missing keys in Fields means the producer
  // did not emit them on that line — callers should treat absence as zero.
  type HookLogEntry struct {
      Timestamp time.Time
      Level     string
      Event     string
      Session   string // convenience mirror of Fields["session"]; empty if absent
      Fields    map[string]string
  }

  // parseHookLogLine parses a single hooks.log line of the form:
  //
  //     <rfc3339> <LEVEL> event=<name> [k=v ...]
  //
  // Returns (_, false) on any parse failure including an empty line or a line
  // missing the `event=` token. The producer (hooklog.go formatHookLogLine)
  // guarantees alphabetically-sorted keys and value sanitization, so a simple
  // space-split on tokens is sufficient — values never contain unescaped
  // whitespace.
  func parseHookLogLine(line string) (HookLogEntry, bool) {
      line = strings.TrimRight(line, "\r\n")
      if line == "" {
          return HookLogEntry{}, false
      }
      parts := strings.SplitN(line, " ", 3)
      if len(parts) < 3 {
          return HookLogEntry{}, false
      }
      ts, err := time.Parse(time.RFC3339, parts[0])
      if err != nil {
          return HookLogEntry{}, false
      }
      entry := HookLogEntry{
          Timestamp: ts,
          Level:     parts[1],
          Fields:    map[string]string{},
      }
      for _, tok := range strings.Split(parts[2], " ") {
          if tok == "" {
              continue
          }
          eq := strings.IndexByte(tok, '=')
          if eq < 0 {
              continue
          }
          k := tok[:eq]
          v := tok[eq+1:]
          if k == "event" && entry.Event == "" {
              entry.Event = v
              continue
          }
          if k == "session" {
              entry.Session = v
          }
          entry.Fields[k] = v
      }
      if entry.Event == "" {
          return HookLogEntry{}, false
      }
      return entry, true
  }
  ```
  *Note:* the regex of "value with equals" in the test is handled here because we use `IndexByte('=')` for the first `=` only.

- [ ] **Step 4: Run tests**

  Run: `go test ./internal/heimdall/ -run TestParseHookLogLine -v`
  Expected: all PASS.

- [ ] **Step 5: Commit**
  ```bash
  git add internal/heimdall/hooklog_reader.go internal/heimdall/hooklog_reader_test.go
  git commit -m "feat(hooklog): line parser"
  ```

### C.2 — Read + filter

- [ ] **Step 1: Add fixture-based test** — append:
  ```go
  func TestReadHookLog_FiltersBySession(t *testing.T) {
      tmp := t.TempDir()
      p := filepath.Join(tmp, "hooks.log")
      content := strings.Join([]string{
          "2026-04-16T20:00:00Z INFO event=session-start session=A stage=ok",
          "2026-04-16T20:00:01Z INFO event=user-prompt session=A stage=ok",
          "2026-04-16T20:00:02Z INFO event=user-prompt session=B stage=ok",
          "2026-04-16T20:00:03Z INFO event=pre-tool-use class=allow mode=shadow session=A",
          "",
      }, "\n")
      if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
          t.Fatal(err)
      }

      entries, err := ReadHookLog(ReadHookLogOpts{Path: p, Session: "A"})
      if err != nil {
          t.Fatal(err)
      }
      if len(entries) != 3 {
          t.Fatalf("got %d want 3 entries for session A", len(entries))
      }

      all, err := ReadHookLog(ReadHookLogOpts{Path: p})
      if err != nil {
          t.Fatal(err)
      }
      if len(all) != 4 {
          t.Fatalf("unfiltered count got %d want 4", len(all))
      }
  }

  func TestReadHookLog_MissingFileReturnsEmpty(t *testing.T) {
      entries, err := ReadHookLog(ReadHookLogOpts{Path: "does-not-exist-xxx.log"})
      if err != nil {
          t.Fatalf("missing file should not error: %v", err)
      }
      if len(entries) != 0 {
          t.Fatalf("expected empty, got %d", len(entries))
      }
  }
  ```
  *Import `path/filepath` and `os` at the top of the test file if not already.*

- [ ] **Step 2: Implement** — append to `internal/heimdall/hooklog_reader.go`:
  ```go
  // ReadHookLogOpts controls ReadHookLog. Empty fields mean "no filter".
  type ReadHookLogOpts struct {
      Path    string
      Session string
      Event   string
      Since   time.Time
      Until   time.Time
  }

  // ReadHookLog reads hooks.log at Path, applies filters, and returns entries
  // in chronological order. A missing path returns an empty slice and no
  // error — hooks.log is created lazily, so callers shouldn't treat absence
  // as a hard failure.
  func ReadHookLog(opts ReadHookLogOpts) ([]HookLogEntry, error) {
      if opts.Path == "" {
          opts.Path = HookLogPath()
      }
      f, err := os.Open(opts.Path)
      if err != nil {
          if os.IsNotExist(err) {
              return nil, nil
          }
          return nil, fmt.Errorf("open hooks.log: %w", err)
      }
      defer f.Close()

      var out []HookLogEntry
      sc := bufio.NewScanner(f)
      sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
      for sc.Scan() {
          entry, ok := parseHookLogLine(sc.Text())
          if !ok {
              continue
          }
          if opts.Session != "" && entry.Session != opts.Session {
              continue
          }
          if opts.Event != "" && entry.Event != opts.Event {
              continue
          }
          if !opts.Since.IsZero() && entry.Timestamp.Before(opts.Since) {
              continue
          }
          if !opts.Until.IsZero() && entry.Timestamp.After(opts.Until) {
              continue
          }
          out = append(out, entry)
      }
      if err := sc.Err(); err != nil {
          return out, fmt.Errorf("scan hooks.log: %w", err)
      }
      return out, nil
  }
  ```

- [ ] **Step 3: Run tests**

  Run: `go test ./internal/heimdall/ -run TestReadHookLog -v -race`
  Expected: PASS.

- [ ] **Step 4: Commit**
  ```bash
  git add internal/heimdall/hooklog_reader.go internal/heimdall/hooklog_reader_test.go
  git commit -m "feat(hooklog): ReadHookLog with filters"
  ```

### C.3 — Aggregate

- [ ] **Step 1: Write test** — append:
  ```go
  func TestAggregateBySession(t *testing.T) {
      entries := []HookLogEntry{
          {Event: "session-start", Session: "A", Fields: map[string]string{"stage": "ok", "bullets": "5"}},
          {Event: "user-prompt", Session: "A", Fields: map[string]string{"stage": "ok", "bytes": "1107", "hits": "5"}},
          {Event: "user-prompt", Session: "A", Fields: map[string]string{"stage": "cache_hit", "bytes": "900"}},
          {Event: "user-prompt", Session: "A", Fields: map[string]string{"stage": "skip", "reason": "prompt_too_short"}},
          {Event: "pre-tool-use", Session: "A", Fields: map[string]string{"class": "allow", "mode": "shadow"}},
          {Event: "pre-tool-use", Session: "A", Fields: map[string]string{"class": "block", "mode": "shadow"}},
          {Event: "post-edit-actor", Session: "A", Fields: map[string]string{"msg": "reindex_ok", "files": "1"}},
          {Event: "stop", Session: "A", Fields: map[string]string{"msg": "buffer_appended", "bytes": "231"}},
          {Event: "user-prompt", Session: "B", Fields: map[string]string{"stage": "ok"}},
      }
      got := AggregateHookLogBySession(entries)
      a := got["A"]
      if a.UserPromptEvents != 3 {
          t.Errorf("UserPromptEvents A: got %d want 3", a.UserPromptEvents)
      }
      if a.UserPromptCacheHits != 1 {
          t.Errorf("cache hits: got %d", a.UserPromptCacheHits)
      }
      if a.UserPromptSkips != 1 {
          t.Errorf("skips: got %d", a.UserPromptSkips)
      }
      if a.GuardrailVerdicts["allow"] != 1 || a.GuardrailVerdicts["block"] != 1 {
          t.Errorf("guardrail verdicts: %+v", a.GuardrailVerdicts)
      }
      if a.UserPromptBytesInjected != 2007 {
          t.Errorf("UserPromptBytesInjected: got %d want 2007", a.UserPromptBytesInjected)
      }
      if a.PostEditReindexes != 1 {
          t.Errorf("PostEditReindexes: got %d", a.PostEditReindexes)
      }
      if got["B"].UserPromptEvents != 1 {
          t.Errorf("B count: got %d", got["B"].UserPromptEvents)
      }
  }
  ```

- [ ] **Step 2: Implement** — append to `hooklog_reader.go`:
  ```go
  // SessionHookAggregate summarizes hook-side metrics for one session.
  type SessionHookAggregate struct {
      Session string

      SessionStartEvents int
      SessionEndEvents   int

      UserPromptEvents        int
      UserPromptCacheHits     int
      UserPromptSkips         int
      UserPromptBytesInjected int64

      PreToolUseEvents  int
      GuardrailVerdicts map[string]int // class -> count, e.g. {"allow": 12, "block": 0}

      PostEditEvents    int
      PostEditReindexes int

      StopEvents int
      StopBytes  int64
  }

  // AggregateHookLogBySession folds a flat list of hooks.log entries into a
  // per-session aggregate. Entries with an empty Session are skipped (they
  // represent pre-payload / doctor dry-fire lines that don't belong to a
  // specific Claude Code session).
  func AggregateHookLogBySession(entries []HookLogEntry) map[string]SessionHookAggregate {
      out := map[string]SessionHookAggregate{}
      for _, e := range entries {
          if e.Session == "" {
              continue
          }
          agg := out[e.Session]
          agg.Session = e.Session
          switch e.Event {
          case "session-start":
              agg.SessionStartEvents++
          case "session-end":
              agg.SessionEndEvents++
          case "user-prompt":
              agg.UserPromptEvents++
              switch e.Fields["stage"] {
              case "cache_hit":
                  agg.UserPromptCacheHits++
              case "skip":
                  agg.UserPromptSkips++
              }
              agg.UserPromptBytesInjected += parseInt64Field(e.Fields["bytes"])
          case "pre-tool-use":
              agg.PreToolUseEvents++
              if agg.GuardrailVerdicts == nil {
                  agg.GuardrailVerdicts = map[string]int{}
              }
              class := e.Fields["class"]
              if class != "" {
                  agg.GuardrailVerdicts[class]++
              }
          case "post-edit":
              agg.PostEditEvents++
          case "post-edit-actor":
              if e.Fields["msg"] == "reindex_ok" {
                  agg.PostEditReindexes++
              }
          case "stop":
              agg.StopEvents++
              agg.StopBytes += parseInt64Field(e.Fields["bytes"])
          }
          out[e.Session] = agg
      }
      return out
  }

  // SortedSessionIDs returns the session IDs from an aggregate map in stable
  // alphabetical order — used by the sessions-list CLI for deterministic output.
  func SortedSessionIDs(agg map[string]SessionHookAggregate) []string {
      ids := make([]string, 0, len(agg))
      for id := range agg {
          ids = append(ids, id)
      }
      sort.Strings(ids)
      return ids
  }

  func parseInt64Field(s string) int64 {
      if s == "" {
          return 0
      }
      var n int64
      for _, r := range s {
          if r < '0' || r > '9' {
              return 0
          }
          n = n*10 + int64(r-'0')
      }
      return n
  }
  ```

- [ ] **Step 3: Run tests**

  Run: `go test ./internal/heimdall/ -run TestAggregateBySession -v -race`
  Expected: PASS.

- [ ] **Step 4: Commit**
  ```bash
  git add internal/heimdall/hooklog_reader.go internal/heimdall/hooklog_reader_test.go
  git commit -m "feat(hooklog): AggregateHookLogBySession"
  ```

### C.4 — Full sweep + PR

- [ ] `go build ./... && go vet ./... && go test ./... -race -count=1`
- [ ] `git push -u origin feat/hooklog-reader`
- [ ] Open PR, merge.

### Wave C — Context-reset protocol

1. Confirm both B and C are merged on main (`git log --oneline -10`).
2. `go test ./internal/heimdall/... -race -count=1` green on main.
3. **Clear context.** Resume at Wave D.

---

## Wave D — `heimdall-mcp sessions` CLI

**Goal:** ship the user-facing CLI that joins the parsers behind two subcommands: `sessions list` and `sessions report`.

**Branch:** `feat/sessions-report-cli` (cut from main after Waves B+C merged).

**Files created:** `internal/cli/sessions.go`, `internal/cli/sessions_test.go`. **Modified:** `internal/cli/cli.go` (one switch case).

### D.1 — Wire subcommand dispatch

- [ ] **Step 1: Locate the dispatch switch** — read `internal/cli/cli.go` and find the main dispatch switch in `RunCLI`. The next PR has a sibling pattern like `case "hook": return DispatchHook(...)`.

- [ ] **Step 2: Add `sessions` case**:
  ```go
  case "sessions":
      return DispatchSessions(cfg, stdin, stdout, stderr, env, rest)
  ```

- [ ] **Step 3: Add help line** — wherever the root CLI prints usage, append `heimdall-mcp sessions list|report [--session-id=X] [--format=text|json]`.

- [ ] **Step 4: Create minimum `sessions.go` to satisfy the compile** — `internal/cli/sessions.go`:
  ```go
  package cli

  import (
      "io"

      "github.com/caio-silva/heimdall-mcp/internal/config"
  )

  // DispatchSessions routes `heimdall-mcp sessions <subcommand>`.
  func DispatchSessions(cfg config.Config, stdin io.Reader, stdout, stderr io.Writer, env map[string]string, args []string) int {
      // Implemented in D.2+ steps.
      _, _, _, _, _, _ = cfg, stdin, stdout, stderr, env, args
      return 0
  }
  ```

- [ ] **Step 5: Build**

  Run: `go build ./...`
  Expected: green.

- [ ] **Step 6: Commit**
  ```bash
  git add internal/cli/cli.go internal/cli/sessions.go
  git commit -m "feat(sessions): stub dispatcher + CLI routing"
  ```

### D.2 — `sessions list`

- [ ] **Step 1: Write the failing test** — new `internal/cli/sessions_test.go`:
  ```go
  package cli

  import (
      "bytes"
      "os"
      "path/filepath"
      "strings"
      "testing"

      "github.com/caio-silva/heimdall-mcp/internal/config"
  )

  func TestSessionsList_EmptyLog(t *testing.T) {
      tmp := t.TempDir()
      t.Setenv("HEIMDALL_HOOK_LOG", filepath.Join(tmp, "hooks.log"))

      var out, errBuf bytes.Buffer
      rc := DispatchSessions(config.Config{}, nil, &out, &errBuf, map[string]string{}, []string{"list"})
      if rc != 0 {
          t.Fatalf("rc: %d", rc)
      }
      if !strings.Contains(out.String(), "no sessions") {
          t.Fatalf("expected no-sessions message, got %q", out.String())
      }
  }

  func TestSessionsList_HappyPath(t *testing.T) {
      tmp := t.TempDir()
      logPath := filepath.Join(tmp, "hooks.log")
      t.Setenv("HEIMDALL_HOOK_LOG", logPath)

      lines := strings.Join([]string{
          "2026-04-16T20:00:00Z INFO event=session-start session=A stage=ok",
          "2026-04-16T20:00:01Z INFO event=user-prompt session=A stage=ok",
          "2026-04-16T20:00:02Z INFO event=user-prompt session=A stage=cache_hit",
          "2026-04-16T20:05:00Z INFO event=session-start session=B stage=ok",
          "",
      }, "\n")
      if err := os.WriteFile(logPath, []byte(lines), 0o600); err != nil {
          t.Fatal(err)
      }

      var out, errBuf bytes.Buffer
      rc := DispatchSessions(config.Config{}, nil, &out, &errBuf, map[string]string{}, []string{"list"})
      if rc != 0 {
          t.Fatalf("rc: %d", rc)
      }
      s := out.String()
      if !strings.Contains(s, "A") || !strings.Contains(s, "B") {
          t.Fatalf("expected both sessions in output:\n%s", s)
      }
      if !strings.Contains(s, "prompts=2") {
          t.Fatalf("expected prompts=2 for A:\n%s", s)
      }
  }
  ```

- [ ] **Step 2: Verify fail**

  Run: `go test ./internal/cli/ -run TestSessionsList -v`
  Expected: FAIL.

- [ ] **Step 3: Implement** — replace stub body in `internal/cli/sessions.go`:
  ```go
  package cli

  import (
      "encoding/json"
      "flag"
      "fmt"
      "io"
      "os"
      "sort"
      "time"

      "github.com/caio-silva/heimdall-mcp/internal/config"
      "github.com/caio-silva/heimdall-mcp/internal/heimdall"
  )

  // DispatchSessions routes `heimdall-mcp sessions <subcommand>`.
  func DispatchSessions(cfg config.Config, stdin io.Reader, stdout, stderr io.Writer, env map[string]string, args []string) int {
      _ = stdin
      _ = env
      if len(args) == 0 {
          fmt.Fprintln(stderr, "Usage: heimdall-mcp sessions <list|report> [flags]")
          return 2
      }
      sub := args[0]
      rest := args[1:]
      switch sub {
      case "list":
          return sessionsList(cfg, stdout, stderr, rest)
      case "report":
          return sessionsReport(cfg, stdout, stderr, rest)
      case "-h", "--help", "help":
          fmt.Fprintln(stdout, "heimdall-mcp sessions — per-session savings metrics")
          fmt.Fprintln(stdout)
          fmt.Fprintln(stdout, "  sessions list                       List recent sessions seen in hooks.log")
          fmt.Fprintln(stdout, "  sessions report --session-id=<id>   Full report for one session")
          fmt.Fprintln(stdout, "                [--format=text|json]")
          fmt.Fprintln(stdout, "                [--cwd=<project-root>] (defaults to current dir)")
          return 0
      default:
          fmt.Fprintf(stderr, "Unknown sessions subcommand: %s\n", sub)
          return 2
      }
  }

  func sessionsList(cfg config.Config, stdout, stderr io.Writer, args []string) int {
      _ = cfg
      _ = stderr

      fs := flag.NewFlagSet("sessions list", flag.ContinueOnError)
      fs.SetOutput(io.Discard)
      var format string
      fs.StringVar(&format, "format", "text", "output format: text|json")
      if err := fs.Parse(args); err != nil {
          fmt.Fprintln(stderr, err)
          return 2
      }

      entries, err := heimdall.ReadHookLog(heimdall.ReadHookLogOpts{})
      if err != nil {
          fmt.Fprintf(stderr, "read hooks.log: %v\n", err)
          return 1
      }
      agg := heimdall.AggregateHookLogBySession(entries)
      ids := heimdall.SortedSessionIDs(agg)
      if len(ids) == 0 {
          fmt.Fprintln(stdout, "no sessions in hooks.log (try starting a Claude Code session first)")
          return 0
      }

      if format == "json" {
          type listRow struct {
              SessionID       string `json:"session_id"`
              PromptEvents    int    `json:"user_prompt_events"`
              CacheHits       int    `json:"user_prompt_cache_hits"`
              GuardrailEvents int    `json:"pre_tool_use_events"`
          }
          rows := make([]listRow, 0, len(ids))
          for _, id := range ids {
              a := agg[id]
              rows = append(rows, listRow{
                  SessionID:       id,
                  PromptEvents:    a.UserPromptEvents,
                  CacheHits:       a.UserPromptCacheHits,
                  GuardrailEvents: a.PreToolUseEvents,
              })
          }
          enc := json.NewEncoder(stdout)
          enc.SetIndent("", "  ")
          _ = enc.Encode(rows)
          return 0
      }

      fmt.Fprintln(stdout, "SESSION                              PROMPTS  CACHE-HITS  GUARDRAIL")
      for _, id := range ids {
          a := agg[id]
          fmt.Fprintf(stdout, "%-36s  prompts=%d  cache_hits=%d  guardrail=%d\n",
              id, a.UserPromptEvents, a.UserPromptCacheHits, a.PreToolUseEvents)
      }
      return 0
  }

  func sessionsReport(cfg config.Config, stdout, stderr io.Writer, args []string) int {
      // Implemented in D.3.
      _ = cfg
      _ = stdout
      _ = stderr
      _ = args
      return 0
  }
  ```

- [ ] **Step 4: Run tests**

  Run: `go test ./internal/cli/ -run TestSessionsList -v -race`
  Expected: PASS.

- [ ] **Step 5: Commit**
  ```bash
  git add internal/cli/sessions.go internal/cli/sessions_test.go
  git commit -m "feat(sessions): list subcommand"
  ```

### D.3 — `sessions report`

- [ ] **Step 1: Write the failing test** — append to `internal/cli/sessions_test.go`:
  ```go
  func TestSessionsReport_TextFormat(t *testing.T) {
      tmp := t.TempDir()
      logPath := filepath.Join(tmp, "hooks.log")
      t.Setenv("HEIMDALL_HOOK_LOG", logPath)

      lines := strings.Join([]string{
          "2026-04-16T20:00:00Z INFO event=session-start session=REPORTME stage=ok",
          "2026-04-16T20:00:01Z INFO event=user-prompt session=REPORTME stage=ok bytes=1100",
          "2026-04-16T20:00:02Z INFO event=user-prompt session=REPORTME stage=cache_hit bytes=900",
          "2026-04-16T20:00:03Z INFO event=pre-tool-use class=allow mode=shadow session=REPORTME",
          "",
      }, "\n")
      _ = os.WriteFile(logPath, []byte(lines), 0o600)

      // A fake transcript in the expected layout.
      home := filepath.Join(tmp, "home")
      slugDir := filepath.Join(home, ".claude", "projects", "-tmp-proj")
      _ = os.MkdirAll(slugDir, 0o755)
      transcriptPath := filepath.Join(slugDir, "REPORTME.jsonl")
      tdata := strings.Join([]string{
          `{"type":"user","message":{"role":"user","content":"hi"},"sessionId":"REPORTME","timestamp":"2026-04-16T20:00:01Z"}`,
          `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":20,"cache_creation_input_tokens":100,"cache_read_input_tokens":500,"output_tokens":10}},"sessionId":"REPORTME","timestamp":"2026-04-16T20:00:02Z"}`,
      }, "\n")
      _ = os.WriteFile(transcriptPath, []byte(tdata), 0o600)

      t.Setenv("HOME", home)
      var out, errBuf bytes.Buffer
      rc := DispatchSessions(config.Config{}, nil, &out, &errBuf, map[string]string{},
          []string{"report", "--session-id=REPORTME", "--cwd=/tmp/proj", "--format=text"})
      if rc != 0 {
          t.Fatalf("rc: %d stderr: %s", rc, errBuf.String())
      }
      s := out.String()
      for _, needle := range []string{"REPORTME", "input_tokens=20", "cache_read=500", "prompts=2", "cache_hits=1", "guardrail=1"} {
          if !strings.Contains(s, needle) {
              t.Errorf("expected %q in report:\n%s", needle, s)
          }
      }
  }

  func TestSessionsReport_JSONFormat(t *testing.T) {
      tmp := t.TempDir()
      logPath := filepath.Join(tmp, "hooks.log")
      t.Setenv("HEIMDALL_HOOK_LOG", logPath)
      _ = os.WriteFile(logPath, []byte("2026-04-16T20:00:01Z INFO event=user-prompt session=J stage=ok bytes=10\n"), 0o600)

      home := filepath.Join(tmp, "home")
      slugDir := filepath.Join(home, ".claude", "projects", "-tmp-proj")
      _ = os.MkdirAll(slugDir, 0o755)
      _ = os.WriteFile(filepath.Join(slugDir, "J.jsonl"),
          []byte(`{"type":"user","message":{"role":"user","content":"hi"},"sessionId":"J"}`+"\n"), 0o600)

      t.Setenv("HOME", home)
      var out, errBuf bytes.Buffer
      rc := DispatchSessions(config.Config{}, nil, &out, &errBuf, map[string]string{},
          []string{"report", "--session-id=J", "--cwd=/tmp/proj", "--format=json"})
      if rc != 0 {
          t.Fatalf("rc: %d", rc)
      }
      var parsed map[string]interface{}
      if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
          t.Fatalf("json: %v\n%s", err, out.String())
      }
      if parsed["session_id"] != "J" {
          t.Errorf("json session id: %v", parsed["session_id"])
      }
  }
  ```
  *Imports to ensure at top of file:* `"encoding/json"`.

- [ ] **Step 2: Verify fail**

  Run: `go test ./internal/cli/ -run TestSessionsReport -v`
  Expected: FAIL (stub returns 0 but prints nothing).

- [ ] **Step 3: Implement** — replace the stub `sessionsReport` in `internal/cli/sessions.go`:
  ```go
  func sessionsReport(cfg config.Config, stdout, stderr io.Writer, args []string) int {
      _ = cfg
      fs := flag.NewFlagSet("sessions report", flag.ContinueOnError)
      fs.SetOutput(io.Discard)
      var (
          sessionID string
          format    string
          cwd       string
          home      string
      )
      fs.StringVar(&sessionID, "session-id", "", "target Claude Code session id (required)")
      fs.StringVar(&format, "format", "text", "output format: text|json")
      fs.StringVar(&cwd, "cwd", "", "project cwd for transcript lookup (default: os.Getwd)")
      fs.StringVar(&home, "home", "", "override home dir for transcript lookup (debug/tests)")
      if err := fs.Parse(args); err != nil {
          fmt.Fprintln(stderr, err)
          return 2
      }
      if sessionID == "" {
          fmt.Fprintln(stderr, "--session-id is required")
          return 2
      }
      if cwd == "" {
          if c, err := os.Getwd(); err == nil {
              cwd = c
          }
      }

      // Hook-side aggregate.
      entries, err := heimdall.ReadHookLog(heimdall.ReadHookLogOpts{Session: sessionID})
      if err != nil {
          fmt.Fprintf(stderr, "read hooks.log: %v\n", err)
          return 1
      }
      aggMap := heimdall.AggregateHookLogBySession(entries)
      hookAgg := aggMap[sessionID]

      // Transcript-side summary.
      transcriptPath, perr := heimdall.TranscriptPathForSession(cwd, sessionID, home)
      var tsum heimdall.TranscriptSummary
      var transcriptErr error
      if perr == nil {
          tsum, transcriptErr = heimdall.ParseTranscript(transcriptPath)
      } else {
          transcriptErr = perr
      }

      report := SessionReport{
          SessionID:  sessionID,
          CWD:        cwd,
          Transcript: tsum,
          Hooks:      hookAgg,
          Duration:   transcriptDuration(tsum),
          Now:        time.Now().UTC(),
      }
      if transcriptErr != nil {
          report.TranscriptError = transcriptErr.Error()
      }

      switch format {
      case "json":
          enc := json.NewEncoder(stdout)
          enc.SetIndent("", "  ")
          _ = enc.Encode(report.toJSON())
          return 0
      case "text":
          renderSessionReportText(stdout, report)
          return 0
      default:
          fmt.Fprintf(stderr, "unknown format: %s\n", format)
          return 2
      }
  }

  // SessionReport is the joined per-session view used by both renderers.
  type SessionReport struct {
      SessionID       string
      CWD             string
      Transcript      heimdall.TranscriptSummary
      Hooks           heimdall.SessionHookAggregate
      Duration        time.Duration
      Now             time.Time
      TranscriptError string
  }

  func transcriptDuration(t heimdall.TranscriptSummary) time.Duration {
      if t.FirstTimestamp.IsZero() || t.LastTimestamp.IsZero() {
          return 0
      }
      return t.LastTimestamp.Sub(t.FirstTimestamp)
  }

  func renderSessionReportText(w io.Writer, r SessionReport) {
      fmt.Fprintf(w, "# Session %s\n\n", r.SessionID)
      fmt.Fprintf(w, "CWD:           %s\n", r.CWD)
      if r.Duration > 0 {
          fmt.Fprintf(w, "Duration:      %s\n", r.Duration.Round(time.Second))
      }
      fmt.Fprintf(w, "\n## Transcript tokens\n")
      fmt.Fprintf(w, "  input_tokens=%d\n", r.Transcript.TotalInputTokens)
      fmt.Fprintf(w, "  cache_creation=%d\n", r.Transcript.TotalCacheCreationTokens)
      fmt.Fprintf(w, "  cache_read=%d\n", r.Transcript.TotalCacheReadTokens)
      fmt.Fprintf(w, "  output_tokens=%d\n", r.Transcript.TotalOutputTokens)
      fmt.Fprintf(w, "\n## Tool use\n")
      fmt.Fprintf(w, "  total=%d heimdall=%d\n", r.Transcript.ToolUseCount, r.Transcript.HeimdallToolCalls)
      names := make([]string, 0, len(r.Transcript.ToolUseByName))
      for n := range r.Transcript.ToolUseByName {
          names = append(names, n)
      }
      sort.Strings(names)
      for _, n := range names {
          fmt.Fprintf(w, "  - %s: %d\n", n, r.Transcript.ToolUseByName[n])
      }
      fmt.Fprintf(w, "\n## Heimdall contribution\n")
      fmt.Fprintf(w, "  SessionStart bytes=%d events=%d\n",
          r.Transcript.HookSuccessBytesByEvent["SessionStart"], r.Transcript.HookSuccessCountByEvent["SessionStart"])
      fmt.Fprintf(w, "  UserPromptSubmit bytes=%d events=%d\n",
          r.Transcript.HookSuccessBytesByEvent["UserPromptSubmit"], r.Transcript.HookSuccessCountByEvent["UserPromptSubmit"])
      fmt.Fprintf(w, "  prompts=%d cache_hits=%d skips=%d bytes_injected=%d\n",
          r.Hooks.UserPromptEvents, r.Hooks.UserPromptCacheHits, r.Hooks.UserPromptSkips, r.Hooks.UserPromptBytesInjected)
      fmt.Fprintf(w, "\n## Guardrails\n")
      fmt.Fprintf(w, "  guardrail=%d verdicts=%v\n", r.Hooks.PreToolUseEvents, r.Hooks.GuardrailVerdicts)
      fmt.Fprintf(w, "\n## Reindexes\n")
      fmt.Fprintf(w, "  post_edit_events=%d reindex_ok=%d\n", r.Hooks.PostEditEvents, r.Hooks.PostEditReindexes)
      if r.TranscriptError != "" {
          fmt.Fprintf(w, "\n(transcript unavailable: %s)\n", r.TranscriptError)
      }
  }

  // toJSON renders a map matching the text sections with stable keys.
  func (r SessionReport) toJSON() map[string]interface{} {
      return map[string]interface{}{
          "session_id":     r.SessionID,
          "cwd":            r.CWD,
          "duration_sec":   int64(r.Duration.Seconds()),
          "tokens": map[string]int64{
              "input":          r.Transcript.TotalInputTokens,
              "cache_creation": r.Transcript.TotalCacheCreationTokens,
              "cache_read":     r.Transcript.TotalCacheReadTokens,
              "output":         r.Transcript.TotalOutputTokens,
          },
          "tool_use": map[string]interface{}{
              "total":     r.Transcript.ToolUseCount,
              "heimdall":  r.Transcript.HeimdallToolCalls,
              "by_name":   r.Transcript.ToolUseByName,
          },
          "heimdall_contribution": map[string]interface{}{
              "session_start_bytes":  r.Transcript.HookSuccessBytesByEvent["SessionStart"],
              "session_start_events": r.Transcript.HookSuccessCountByEvent["SessionStart"],
              "prompt_bytes":         r.Transcript.HookSuccessBytesByEvent["UserPromptSubmit"],
              "prompt_events":        r.Transcript.HookSuccessCountByEvent["UserPromptSubmit"],
              "cache_hits":           r.Hooks.UserPromptCacheHits,
              "skips":                r.Hooks.UserPromptSkips,
              "bytes_injected":       r.Hooks.UserPromptBytesInjected,
          },
          "guardrails": map[string]interface{}{
              "events":   r.Hooks.PreToolUseEvents,
              "verdicts": r.Hooks.GuardrailVerdicts,
          },
          "reindexes": map[string]interface{}{
              "events":     r.Hooks.PostEditEvents,
              "reindex_ok": r.Hooks.PostEditReindexes,
          },
          "transcript_error": r.TranscriptError,
      }
  }
  ```

- [ ] **Step 4: Update transcript path resolver to honor `HOME` env** — the test uses `t.Setenv("HOME", ...)`. `os.UserHomeDir` on Unix reads `HOME` env, so this should already work. If not, pass `home` explicitly via the `--home` flag (the test uses `--cwd=/tmp/proj` and relies on `HOME`).

- [ ] **Step 5: Run tests**

  Run: `go test ./internal/cli/ -run TestSessionsReport -v -race`
  Expected: PASS.

- [ ] **Step 6: Commit**
  ```bash
  git add internal/cli/sessions.go internal/cli/sessions_test.go
  git commit -m "feat(sessions): report subcommand (text + json formats)"
  ```

### D.4 — Full sweep + live dogfood + PR

- [ ] **Step 1:** `go build ./... && go vet ./... && go test ./... -race -count=1`

- [ ] **Step 2: Rebuild binary**

  `go build -o /home/noname/.local/bin/heimdall-mcp ./cmd/heimdall-mcp`

- [ ] **Step 3: Dogfood against real data**
  ```bash
  heimdall-mcp sessions list
  # pick a recent session id from output
  heimdall-mcp sessions report --session-id=<id>
  heimdall-mcp sessions report --session-id=<id> --format=json
  ```
  Expected: sensible numbers for at least one session.

- [ ] **Step 4: Open PR**
  ```bash
  git push -u origin feat/sessions-report-cli
  gh pr create --title "feat(sessions): per-session savings report CLI" --body "..."
  ```
  Body template:
  ```
  ## Summary
  - New `heimdall-mcp sessions list` and `heimdall-mcp sessions report` subcommands.
  - `report` joins Claude Code transcript JSONL (tokens, tool-calls, hook_success attachments) with hooks.log (cache hits, guardrail verdicts, reindex counts) keyed on session_id.
  - Builds on Waves A–C (session_id in logs + pure parsers).

  ## Test plan
  - [ ] `go test ./... -race` green
  - [ ] Dogfood: `heimdall-mcp sessions list` on real data shows recent sessions; `heimdall-mcp sessions report --session-id=<id>` produces a report with non-zero numbers.
  ```

- [ ] **Step 5:** Merge.

### Wave D — Context-reset protocol

1. Confirm D merged on main.
2. `heimdall-mcp sessions list` prints real sessions on a freshly-pulled main.
3. **Clear context.** Resume at Wave E.

---

## Wave E — Docs + handoff update

**Goal:** capture the new feature in README, handoff, and TODO.

**Branch:** `docs/sessions-report`.

### E.1 — README

- [ ] **Step 1:** Read `README.md` and find the existing "Hooks" or similar section.

- [ ] **Step 2:** Append a short subsection (no code block, no screenshots). Example prose:
  ```markdown
  ### Per-session savings

  After a Claude Code session, run:

  ```
  heimdall-mcp sessions list
  heimdall-mcp sessions report --session-id=<id>
  ```

  The `report` command joins the session transcript (tokens, tool calls) with `hooks.log` (cache hits, guardrail verdicts, reindex events) and prints a per-session summary. `--format=json` is available for scripting.
  ```

- [ ] **Step 3:** Commit `git add README.md && git commit -m "docs(readme): add sessions report section"`.

### E.2 — Handoff

- [ ] **Step 1:** Edit `docs/plans/hooks/07-next-session-handoff.md`:
  - Under "What's next", mark item 17 (T21 comparison) as superseded by `heimdall-mcp sessions report`.
  - Add a "Sessions report" one-liner to "First actions on resumption": `heimdall-mcp sessions list && heimdall-mcp sessions report --session-id=<latest>`.

- [ ] **Step 2:** Commit.

### E.3 — TODO + plan doc completion

- [ ] **Step 1:** Edit `TODO.md` to strike the per-session tracking follow-up item (or add it as shipped).

- [ ] **Step 2:** Edit `docs/plans/hooks/10-per-session-savings-report.md` (this file) at the top to insert a SHIPPED badge + PR links.

- [ ] **Step 3:** Commit.

### E.4 — PR + merge

- [ ] `git push -u origin docs/sessions-report`
- [ ] `gh pr create --title "docs: per-session savings report"` — small PR, short body.
- [ ] Merge.

---

## Open decisions

| # | Decision | Recommendation | Why |
|---|---|---|---|
| 1 | Transcript path resolution fallback when `--cwd` is absent | Use `os.Getwd()` | Matches existing hook convention; users are expected to run `sessions report` from the project directory. |
| 2 | JSON schema stability | ✅ SHIPPED | `schema_version: "v1"` emitted on both `sessions list --format=json` and `sessions report --format=json`. See `SessionsReportSchemaVersion` in `internal/cli/sessions.go`. Bump on breaking change only. Additive fields (new keys) **do not** require a bump — e.g. `tool_use.redundant_heimdall_calls` was added post-ship in Wave F without a version change. |
| 3 | Handling rotated `hooks.log.1` | Ignore in v1 | 5 MB cap + rotation means only recent sessions live in the active file. Multi-session history can be a follow-up. |
| 4 | `sessions list` time window | ✅ SHIPPED | `--since=<duration>` filter implemented; keeps sessions whose `LastSeen` falls inside the window. Session aggregates now track `FirstSeen` / `LastSeen`. |
| 5 | Auto-detecting "current" session | Not in v1 | `sessions list` lets the user pick; auto-detect needs ambiguity handling (multiple parallel sessions). |
| 6 | Doctor check #14 | ✅ SHIPPED | Lightweight readability check — reads `hooks.log`, aggregates by session, pass/warn only. Deliberately does not try to render a full report (transcripts may be absent on fresh installs). |

---

## Self-review checklist

After each wave, verify:

- [ ] **Build + vet green:** `go build ./... && go vet ./... && go test ./... -race -count=1`
- [ ] **No placeholder strings:** grep the wave's new files for `TODO`, `FIXME`, `xxx`. Expected: 0 hits.
- [ ] **Type consistency:** The types defined in Wave B (`TranscriptSummary`) and Wave C (`HookLogEntry`, `SessionHookAggregate`) are referenced exactly as defined when used in Wave D.
- [ ] **OQ-5 respected:** Wave A adds `session=` to retrieval-hook logs but never introduces new stderr writes or non-zero exits on the retrieval path. Verify by grep: no new `fmt.Fprintln(stderr, …)` or `return 2` inside `HookSessionStart`/`HookUserPrompt`/`HookPreToolUse`/`HookPostEdit` bodies.
- [ ] **Hooks.log format unchanged:** `formatHookLogLine` not modified. `session=<id>` is just another sorted key.
- [ ] **Real-data dogfood:** after Wave D, `heimdall-mcp sessions report --session-id=<real-id>` shows non-zero tokens and sensible hook counts.

---

## Appendix — example output

Text format (expected shape, numbers are illustrative):

```
# Session b2b55d42-744f-4a11-8a10-120565657ef7

CWD:           /home/noname/Code/heimdall-mcp
Duration:      1h8m

## Transcript tokens
  input_tokens=3412
  cache_creation=20580
  cache_read=1084020
  output_tokens=9821

## Tool use
  total=87 heimdall=14 redundant_heimdall_calls=6
  - Bash: 32
  - Edit: 11
  - Read: 18
  - Write: 4
  - mcp__heimdall__heimdall_recall: 3
  - mcp__heimdall__heimdall_search: 11

## Heimdall contribution
  SessionStart bytes=1240 events=1
  UserPromptSubmit bytes=9872 events=14
  prompts=14 cache_hits=3 skips=1 bytes_injected=13040

## Guardrails
  guardrail=28 verdicts=map[allow:28]

## Reindexes
  post_edit_events=11 reindex_ok=6
```

JSON format: a flat object with the same sections under typed keys, see `SessionReport.toJSON()` in D.3.
