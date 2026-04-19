# P0 implementation plan — Claude's self-use of heimdall

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Increase Claude's direct (non-hook-injected) use of heimdall MCP tools by landing four small, independent fixes at existing seams: widened transcript extractor + next-session nag, concrete trigger→action pairs in the MCP instruction block, and a read/write-scoped "unavailable" banner.

**Architecture:** Four units at four existing seams in three files (`internal/cli/hook_stop.go`, `internal/cli/hook.go`, `internal/mcp/server.go`). One new on-disk artifact (`.heimdall_db/hooks/last-session-review.json`) written by SessionEnd and consumed by next SessionStart. No new hook events, no new DB tables, no schema migrations.

**Tech Stack:** Go 1.25+, existing `testing` package, existing `heimdall.LogHookEvent`, existing `IngestSessionSummary`. No new dependencies.

**Spec:** [`01-p0-design.md`](./01-p0-design.md).

---

## Sequencing and parallelism

Four mergeable PRs. Three are independent; the fourth (Unit A+B) sequences within one branch because both changes touch `extractTranscriptSummary` + `HookSessionEnd`.

| Task | Unit | File(s) | Depends on | Parallel-safe? |
|---|---|---|---|---|
| Task 1 | **E** — MCP instruction rewrite | `internal/mcp/server.go` | none | yes |
| Task 2 | **D** — banner scoping | `internal/cli/hook.go` | none | yes |
| Task 3 | **C** — SessionStart nag renderer | `internal/cli/hook.go` | none (gracefully handles missing file) | yes, but same file as Task 2 — sequence after Task 2 or rebase |
| Task 4 | **A+B** — widened extractor + review-record writer | `internal/cli/hook_stop.go` | none (Task 3 renders the artifact, but renderer handles its absence) | yes |
| Task 5 | Acceptance gate — manual two-session end-to-end | (no file changes) | 1, 2, 3, 4 | n/a |

**Merge order guidance:**
- Tasks 1, 2, 4 can land in any order.
- Task 3 shares a file with Task 2; merge Task 2 first or rebase.
- Task 4's artifact is first consumed once Task 3 is merged; either can land first, and the renderer quietly no-ops until the writer is live.
- Task 5 runs once all four are merged.

Each of Tasks 1–4 is one subagent-sized scope. A subagent is handed exactly one Task, its Files list, and its Step list — nothing more.

---

## File structure

**Modified (4):**
- `internal/mcp/server.go` — replace instruction string literal in `handleInitialize`.
- `internal/cli/hook.go` — change `emitTierBNote` wording; add `renderLastSessionReview` helper and call it from `formatSessionStartBlock`.
- `internal/cli/hook_stop.go` — rewrite `extractTranscriptSummary` (new return signature), extend `HookSessionEnd` to write `last-session-review.json`.

**New test files (or extensions):**
- `internal/mcp/server_test.go` — extend with `TestHandleInitialize_InstructionsContainTriggerPairs`.
- `internal/cli/hook_test.go` — extend with banner tests + review-renderer tests.
- `internal/cli/hook_stop_test.go` — extend with extractor + review-writer tests.

**New fixtures:**
- `internal/cli/testdata/transcript_markers.jsonl` — synthetic transcript with each marker class.
- `internal/cli/testdata/transcript_tool_use.jsonl` — synthetic transcript with `heimdall_remember` tool_use blocks, to verify write-count.

**New runtime artifact:**
- `<project>/.heimdall_db/hooks/last-session-review.json` — consume-once, overwrite-on-write, per-project.

---

## Task 1 (Unit E): MCP instruction rewrite

**Goal:** Replace the 240-word prose-numbered instruction string at `internal/mcp/server.go:192-204` with 200-word trigger→action pairs + SessionStart-nag pointer line.

**Files:**
- Modify: `internal/mcp/server.go` (the `handleInitialize` string literal around lines 192–204)
- Test: `internal/mcp/server_test.go` (extend with new test; create the file if it doesn't exist)

- [ ] **Step 1.1: Write the failing test**

The current `handleInitialize` inlines the instructions text in a function body and calls `client.Ping(ctx)` before returning — which means testing via the full initialize round-trip is flaky (takes the fallback branch whenever Ollama is unreachable). Extract the happy-path text into a package-level constant so the test asserts on it directly.

Open `internal/mcp/server_test.go` (check if it exists first with Read; if not, create it with `package mcp`). Add:

```go
func TestHeimdallInstructionsTriggerPairs(t *testing.T) {
	markers := []string{
		"WHEN you're about to Read a file > 200 lines",
		"WHEN the user corrects you",
		"WHEN a WebFetch, Read, or external-MCP call",
		"WHEN starting a task that references past decisions",
		"WHEN search feels wrong",
		"Last session review",
	}
	for _, m := range markers {
		if !strings.Contains(heimdallInstructions, m) {
			t.Errorf("heimdallInstructions missing marker: %q", m)
		}
	}
}
```

If `server_test.go` doesn't exist, include package declaration, `import "strings"`, `import "testing"`. If it exists, make sure `strings` is imported.

- [ ] **Step 1.2: Run test to verify it fails**

Run: `go test ./internal/mcp/ -run TestHeimdallInstructionsTriggerPairs -v`
Expected: FAIL to compile — `heimdallInstructions` undefined. (This is correct; Step 1.3 defines it.)

- [ ] **Step 1.3: Extract instructions to a package-level constant and replace the literal**

In `internal/mcp/server.go`, add a package-level constant (above `handleInitialize`, near other package-level declarations) and replace the inline literal inside `handleInitialize` with a reference to it.

Add at package scope:

```go
// heimdallInstructions is the happy-path instruction block served in the
// MCP initialize response. Extracted as a constant so it can be unit-tested
// without standing up a live Ollama client. The Ollama-unreachable branch
// in handleInitialize substitutes a different string (see below) and is
// unaffected.
const heimdallInstructions = `You have access to Heimdall, a local semantic code + memory store. Use it via these concrete trigger→action pairs, not as a general reminder to "be proactive":

• WHEN you're about to Read a file > 200 lines or Grep/Glob to explore unfamiliar architecture → call heimdall_search FIRST, then read only the ranges that matter.

• WHEN the user corrects you ("actually", "no, do X", "don't"), teaches a non-obvious fact, or you discover a workaround (env quirk, build flag, API gotcha) → call heimdall_remember the same turn, with the rule + the reason.

• WHEN a WebFetch, Read, or external-MCP call (Jira, Slack, GitHub, Confluence, email) returns > 500 chars of content → call heimdall_index_text silently (no user-facing mention). Include the source URL/ID in metadata.

• WHEN starting a task that references past decisions, plans, or prior architecture → call heimdall_recall first to surface what was already decided. If multiple projects are indexed, heimdall_projects lists them.

• WHEN search feels wrong (empty results, surprising ranking) call heimdall_explain; WHEN setup seems off call heimdall_status.

At SessionStart you may see a "Last session review" block listing remember-moments you missed in the previous session — treat it as a task, not status: heimdall_remember the items that still matter before moving on.`
```

Then, inside `handleInitialize`, change the line at ~192:

```go
	instructions := `You have access to Heimdall, ...` // (the old literal block through line 204)
```

to:

```go
	instructions := heimdallInstructions
```

Leave the Ollama-unreachable fallback (`if err := client.Ping(ctx); err != nil { ... instructions = fmt.Sprintf(...) }`) untouched — that branch overwrites `instructions` with the fallback string, which is the correct behavior for that state.

- [ ] **Step 1.4: Run test to verify it passes**

Run: `go test ./internal/mcp/ -run TestHeimdallInstructionsTriggerPairs -v`
Expected: PASS.

Also run the full package to catch any regressions:
Run: `go test ./internal/mcp/ -v`
Expected: PASS for all existing tests.

- [ ] **Step 1.5: Commit**

```bash
git add internal/mcp/server.go internal/mcp/server_test.go
git commit -m "feat(mcp): rewrite instruction block as trigger→action pairs

Replaces the prose 'use X proactively' block with five concrete
WHEN→ACTION rules + a pointer to the SessionStart 'Last session review'
nag. Addresses P0 #2 from docs/plans/claude-heimdall-self-use/."
```

---

## Task 2 (Unit D): banner scoping

**Goal:** Split the single-line "heimdall: unavailable (…)" Tier-B note into a two-line form that names the read path as down and calls out that writes still work. Applies only to the `classifyVerifyErr` branches (index-mismatch / missing / dim). The Ollama-down banner stays as-is.

**Files:**
- Modify: `internal/cli/hook.go` (lines 386–395, `emitTierBNote`)
- Test: `internal/cli/hook_test.go` (extend)

- [ ] **Step 2.1: Write the failing tests**

Add to `internal/cli/hook_test.go`:

```go
func TestEmitTierBNote_NewTwoLineForm(t *testing.T) {
	var buf bytes.Buffer
	// Always-emit suppress stub so the note actually fires.
	alwaysEmit := func(_, _ string, _ time.Duration) bool { return true }
	emitTierBNote(&buf, alwaysEmit, "/some/project", "index_model_mismatch", "index model mismatch")

	out := buf.String()
	if !strings.Contains(out, "> heimdall_search: unavailable (index model mismatch)") {
		t.Errorf("missing scoped search-unavailable line; got:\n%s", out)
	}
	if !strings.Contains(out, "heimdall_remember and heimdall_index_text still work") {
		t.Errorf("missing writes-still-work reassurance line; got:\n%s", out)
	}
	// Old single-line form must be gone.
	if strings.Contains(out, "> heimdall: unavailable") {
		t.Errorf("old unscoped banner leaked; got:\n%s", out)
	}
}

func TestEmitTierBNote_SuppressedWhenOutsideWindow(t *testing.T) {
	var buf bytes.Buffer
	neverEmit := func(_, _ string, _ time.Duration) bool { return false }
	emitTierBNote(&buf, neverEmit, "/p", "index_model_mismatch", "index model mismatch")
	if buf.Len() != 0 {
		t.Errorf("expected empty output when suppressed, got %q", buf.String())
	}
}
```

- [ ] **Step 2.2: Run tests to verify they fail**

Run: `go test ./internal/cli/ -run TestEmitTierBNote_NewTwoLineForm -v`
Expected: FAIL — current banner emits `> heimdall: unavailable` not `> heimdall_search: unavailable`.

Run: `go test ./internal/cli/ -run TestEmitTierBNote_SuppressedWhenOutsideWindow -v`
Expected: PASS already (suppression is preexisting behavior).

- [ ] **Step 2.3: Replace the banner body in `emitTierBNote`**

In `internal/cli/hook.go`, find `emitTierBNote` (~line 389) and replace its `fmt.Fprintf` call:

```go
// emitTierBNote writes a two-line degraded-state note to stdout iff the
// suppression store says this failure code is out of its cooldown window.
// Scopes the "unavailable" claim to the read path (heimdall_search) and
// reassures the model that write-path tools (heimdall_remember,
// heimdall_index_text) are still usable in this branch — which is true
// whenever we reach here: Ollama is up (checked earlier), and the memory
// store is independent of the code-index model-verify gate.
func emitTierBNote(w io.Writer, suppress func(string, string, time.Duration) bool, project, failureCode, humanReason string) {
	if !suppress(project, failureCode, sessionStartSuppressionWindow) {
		return
	}
	fmt.Fprintf(w,
		"## Heimdall context\n\n"+
			"> heimdall_search: unavailable (%s)\n"+
			"> heimdall_remember and heimdall_index_text still work — use them for this session's decisions.\n",
		humanReason,
	)
}
```

- [ ] **Step 2.4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run TestEmitTierBNote -v`
Expected: both tests PASS.

Also run the full package:
Run: `go test ./internal/cli/ -v`
Expected: all existing tests still PASS. If any `TestHookSessionStart_*` golden tests fail because they contained the old banner string, update the expected strings in the test to the new two-line form (not the test logic — only the expected banner text).

- [ ] **Step 2.5: Commit**

```bash
git add internal/cli/hook.go internal/cli/hook_test.go
git commit -m "fix(hooks): scope Tier-B banner to read path, reassure writes work

Splits '> heimdall: unavailable (…)' into two lines — names
heimdall_search as the down tool, tells Claude heimdall_remember and
heimdall_index_text still work. Only applies to the index-verify error
branches; the Ollama-down banner keeps its current wording (writes
genuinely don't work there). Addresses P0 #3 from
docs/plans/claude-heimdall-self-use/."
```

---

## Task 3 (Unit C): SessionStart nag renderer

**Goal:** At SessionStart, if `.heimdall_db/hooks/last-session-review.json` exists (and is <7 days old), render a "### Last session review" section into the context block, then unlink the file. Handles missing file as a no-op — so it can ship before Task 4's writer lands.

**Files:**
- Modify: `internal/cli/hook.go` (extend `formatSessionStartBlock`, add `renderLastSessionReview` helper)
- Test: `internal/cli/hook_test.go` (extend)

- [ ] **Step 3.1: Write the failing tests**

Add to `internal/cli/hook_test.go`:

```go
// writeReviewFile is a local helper for tests; in-repo callers use
// internal/cli/hook_stop.go's writer (landing in Task 4). Defining it here
// keeps this task's tests runnable before Task 4 merges.
func writeReviewFile(t *testing.T, projectDir string, body string) string {
	t.Helper()
	dir := filepath.Join(projectDir, ".heimdall_db", "hooks")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	p := filepath.Join(dir, "last-session-review.json")
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func TestSessionStartBlock_IncludesLastSessionReview(t *testing.T) {
	dir := t.TempDir()
	body := `{
		"session_id": "s-1",
		"ended_at": ` + fmt.Sprintf("%d", time.Now().Unix()-60) + `,
		"candidates": 7,
		"writes": 0,
		"top_markers": ["correction","correction","workaround"],
		"excerpts": [
			"no, the other file — move the check before the loop not after",
			"actually heimdall_remember still works when search is down",
			"binding to 127.0.0.1 instead of localhost fixed macOS ::1"
		]
	}`
	reviewPath := writeReviewFile(t, dir, body)

	out := renderLastSessionReview(dir, time.Now())
	if !strings.Contains(out, "### Last session review") {
		t.Errorf("expected header; got %q", out)
	}
	if !strings.Contains(out, "7 candidate remember-moments") {
		t.Errorf("expected count line; got %q", out)
	}
	if !strings.Contains(out, "move the check before the loop") {
		t.Errorf("expected first excerpt; got %q", out)
	}
	if _, err := os.Stat(reviewPath); !os.IsNotExist(err) {
		t.Errorf("expected review file unlinked after render; err=%v", err)
	}
}

func TestSessionStartBlock_SkipsMissingReviewFile(t *testing.T) {
	dir := t.TempDir()
	out := renderLastSessionReview(dir, time.Now())
	if out != "" {
		t.Errorf("expected empty output when file missing, got %q", out)
	}
}

func TestSessionStartBlock_SkipsStaleReviewFile(t *testing.T) {
	dir := t.TempDir()
	body := `{
		"session_id": "s-old",
		"ended_at": ` + fmt.Sprintf("%d", time.Now().Add(-8*24*time.Hour).Unix()) + `,
		"candidates": 5,
		"writes": 0,
		"top_markers": ["correction"],
		"excerpts": ["stale"]
	}`
	reviewPath := writeReviewFile(t, dir, body)

	out := renderLastSessionReview(dir, time.Now())
	if out != "" {
		t.Errorf("expected empty output for stale file, got %q", out)
	}
	if _, err := os.Stat(reviewPath); !os.IsNotExist(err) {
		t.Errorf("expected stale review file unlinked without render; err=%v", err)
	}
}

func TestSessionStartBlock_SkipsMalformedReviewFile(t *testing.T) {
	dir := t.TempDir()
	_ = writeReviewFile(t, dir, "{ not json")
	out := renderLastSessionReview(dir, time.Now())
	if out != "" {
		t.Errorf("expected empty output on malformed file, got %q", out)
	}
}
```

- [ ] **Step 3.2: Run tests to verify they fail**

Run: `go test ./internal/cli/ -run TestSessionStartBlock_ -v`
Expected: FAIL (undefined: `renderLastSessionReview`).

- [ ] **Step 3.3: Implement `renderLastSessionReview`**

Add to `internal/cli/hook.go` (anywhere below `formatSessionStartBlock`):

```go
// lastSessionReview is the on-disk shape of .heimdall_db/hooks/last-session-review.json.
// Written by HookSessionEnd (see internal/cli/hook_stop.go) and read once by
// renderLastSessionReview at the next SessionStart. Keeping the type local to
// hook.go avoids a package cycle with hook_stop.go — the writer side defines
// its own equivalent; both serialize/deserialize through JSON so structural
// compatibility is what matters, not type identity.
type lastSessionReview struct {
	SessionID   string   `json:"session_id"`
	EndedAt     int64    `json:"ended_at"`
	Candidates  int      `json:"candidates"`
	Writes      int      `json:"writes"`
	TopMarkers  []string `json:"top_markers"`
	Excerpts    []string `json:"excerpts"`
}

const lastSessionReviewMaxAge = 7 * 24 * time.Hour

// renderLastSessionReview returns the "### Last session review" markdown
// section for inclusion in the SessionStart block, or empty string if:
//   - the review file is missing
//   - the file is malformed
//   - the review is older than 7 days
//
// The review file is unlinked in all cases where it exists and is readable
// (including stale and malformed), so a bad file doesn't keep nagging.
// Only the successful-render path writes output; the unlink is side-effect.
func renderLastSessionReview(projectDir string, now time.Time) string {
	path := filepath.Join(projectDir, ".heimdall_db", "hooks", "last-session-review.json")
	data, err := os.ReadFile(path)
	if err != nil {
		// Missing or unreadable — quietly skip. No unlink (nothing to unlink on ENOENT).
		return ""
	}
	// Unlink before rendering: consume-once semantics survive even a render panic.
	_ = os.Remove(path)

	var rev lastSessionReview
	if err := json.Unmarshal(data, &rev); err != nil {
		heimdall.LogHookEvent("WARN", "session-start", map[string]any{"err": "review_json_parse_failed"})
		return ""
	}
	endedAt := time.Unix(rev.EndedAt, 0)
	if now.Sub(endedAt) > lastSessionReviewMaxAge {
		return ""
	}

	// Counts by marker, preserving priority order.
	markerCounts := map[string]int{}
	for _, m := range rev.TopMarkers {
		markerCounts[m]++
	}
	var markerParts []string
	for _, m := range []string{"correction", "workaround", "teaching", "frustration"} {
		if c := markerCounts[m]; c > 0 {
			markerParts = append(markerParts, fmt.Sprintf("%d %s", c, m))
		}
	}
	markerSummary := strings.Join(markerParts, ", ")

	var b strings.Builder
	b.WriteString("\n### Last session review\n")
	fmt.Fprintf(&b, "%d candidate remember-moments", rev.Candidates)
	if markerSummary != "" {
		fmt.Fprintf(&b, " (%s)", markerSummary)
	}
	fmt.Fprintf(&b, " but only %d heimdall_remember / heimdall_index_text calls this session.\n\n", rev.Writes)

	if len(rev.Excerpts) > 0 {
		b.WriteString("Examples:\n")
		for i, ex := range rev.Excerpts {
			if i >= 3 {
				break
			}
			marker := ""
			if i < len(rev.TopMarkers) {
				marker = rev.TopMarkers[i]
			}
			if marker != "" {
				fmt.Fprintf(&b, "- %s: %q\n", marker, ex)
			} else {
				fmt.Fprintf(&b, "- %q\n", ex)
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("If any of these still matter, heimdall_remember them now.\n")
	return b.String()
}
```

- [ ] **Step 3.4: Wire the helper into `formatSessionStartBlock`**

In `internal/cli/hook.go`, edit `formatSessionStartBlock` to call the helper. Replace the existing block (around lines 407–435) — specifically, insert the review section between the memories section and `appendSkillsSection`:

```go
func formatSessionStartBlock(status heimdall.StatusInfo, model, projectRoot string, bullets, skillBullets []string, now time.Time) string {
	var b strings.Builder
	b.Grow(256 + 80*len(bullets) + 80*len(skillBullets))

	projectName := filepath.Base(projectRoot)
	lastIndexed := status.LastIndexed
	if lastIndexed == "" {
		lastIndexed = "unknown"
	}

	b.WriteString("## Heimdall context\n\n")
	fmt.Fprintf(&b, "**Project:** %s  •  **Model:** %s  •  **Chunks:** %d  •  **Last indexed:** %s\n",
		projectName, model, status.TotalChunks, lastIndexed)

	if len(bullets) > 0 {
		b.WriteString("\n### Recent memories\n")
		for _, m := range bullets {
			if m == "" {
				continue
			}
			fmt.Fprintf(&b, "- %s\n", m)
		}
	}

	// NEW: render the last-session review if present. Silent no-op when
	// missing/stale/malformed — so this line is safe to land before the
	// writer in hook_stop.go is live.
	b.WriteString(renderLastSessionReview(projectRoot, now))

	appendSkillsSection(&b, skillBullets)

	b.WriteString("\n_retrieved via heimdall-mcp_\n")
	return b.String()
}
```

Make sure `"encoding/json"` is imported in `hook.go` (check existing imports — it likely already is).

- [ ] **Step 3.5: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run TestSessionStartBlock_ -v`
Expected: all four new tests PASS.

Run: `go test ./internal/cli/ -v`
Expected: all existing tests still PASS.

- [ ] **Step 3.6: Commit**

```bash
git add internal/cli/hook.go internal/cli/hook_test.go
git commit -m "feat(hooks): render last-session-review at SessionStart

Adds renderLastSessionReview helper — reads
.heimdall_db/hooks/last-session-review.json, renders a '### Last session
review' section into the SessionStart context block, unlinks the file.
Handles missing/malformed/stale files as silent no-ops so this can land
before the writer in hook_stop.go. Addresses P0 #1 (part B) from
docs/plans/claude-heimdall-self-use/."
```

---

## Task 4 (Unit A+B): widened extractor + review-record writer

**Goal:** Replace `extractTranscriptSummary` with a marker-based scanner that returns both a richer summary and a candidate list, and extend `HookSessionEnd` to write `last-session-review.json` when the session warrants nagging.

**Files:**
- Modify: `internal/cli/hook_stop.go`
- Test: `internal/cli/hook_stop_test.go` (extend)
- Fixtures: `internal/cli/testdata/transcript_markers.jsonl`, `internal/cli/testdata/transcript_tool_use.jsonl` (new)

- [ ] **Step 4.1: Write the extractor fixture and failing tests**

Create `internal/cli/testdata/transcript_markers.jsonl`:

```jsonl
{"role":"assistant","content":"Here is the fix — I'll change the loop condition to <=."}
{"role":"user","content":"no, actually the loop should use <, not <=. Don't change that."}
{"role":"assistant","content":"Got it, reverting to <."}
{"role":"user","content":"FYI: the build flag `-tags integration` is required for these tests. Heads up, it was missing from the docs."}
{"role":"assistant","content":"Thanks — running with -tags integration now."}
{"role":"user","content":"the workaround for the macOS ::1 binding is to use 127.0.0.1 explicitly"}
{"role":"assistant","content":"Understood, applying that."}
```

Create `internal/cli/testdata/transcript_tool_use.jsonl`:

```jsonl
{"role":"assistant","content":[{"type":"text","text":"I'll remember that."},{"type":"tool_use","name":"heimdall_remember","input":{"content":"use -tags integration for tests"}}]}
{"role":"user","content":"also index that Jira ticket I shared"}
{"role":"assistant","content":[{"type":"tool_use","name":"heimdall_index_text","input":{"text":"..."}}]}
{"role":"assistant","content":"Here's a plain text message, no tool use."}
```

Add to `internal/cli/hook_stop_test.go`:

```go
func TestExtractTranscriptSummary_MarkerWindows(t *testing.T) {
	data, err := os.ReadFile("testdata/transcript_markers.jsonl")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	summary, candidates := extractTranscriptSummary(data)

	if summary == "" {
		t.Fatalf("expected non-empty summary")
	}
	// Every marker class should produce at least one candidate.
	seen := map[string]bool{}
	for _, c := range candidates {
		seen[c.Marker] = true
	}
	for _, want := range []string{"correction", "teaching", "workaround"} {
		if !seen[want] {
			t.Errorf("missing candidate marker %q; got %+v", want, candidates)
		}
	}
	// Excerpts cap at 200 chars.
	for _, c := range candidates {
		if len(c.Excerpt) > 200 {
			t.Errorf("excerpt exceeds 200 chars: %d", len(c.Excerpt))
		}
	}
}

func TestExtractTranscriptSummary_NoMarkers_TailFallback(t *testing.T) {
	// Three plain assistant messages, no user markers.
	data := []byte(`{"role":"assistant","content":"first msg"}
{"role":"assistant","content":"second msg"}
{"role":"assistant","content":"third msg"}
`)
	summary, candidates := extractTranscriptSummary(data)
	if summary == "" {
		t.Errorf("expected non-empty tail-fallback summary; got empty")
	}
	if !strings.Contains(summary, "third msg") {
		t.Errorf("expected tail to include most recent assistant message; got %q", summary)
	}
	if len(candidates) != 0 {
		t.Errorf("expected 0 candidates with no markers, got %d", len(candidates))
	}
}

func TestExtractTranscriptSummary_BoundedOutput(t *testing.T) {
	var big bytes.Buffer
	// ~10 MB of user messages with one correction hit near the start.
	big.WriteString(`{"role":"user","content":"actually stop doing X"}` + "\n")
	msg := `{"role":"assistant","content":"` + strings.Repeat("x", 1000) + `"}` + "\n"
	for big.Len() < 10*1024*1024 {
		big.WriteString(msg)
	}
	start := time.Now()
	summary, _ := extractTranscriptSummary(big.Bytes())
	elapsed := time.Since(start)

	if len(summary) > 20*1024 {
		t.Errorf("summary exceeds 20KB cap: %d bytes", len(summary))
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("extractor took too long on 10MB input: %v", elapsed)
	}
}

func TestCountHeimdallWrites_CountsToolUseBlocks(t *testing.T) {
	data, err := os.ReadFile("testdata/transcript_tool_use.jsonl")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	n := countHeimdallWrites(data)
	if n != 2 {
		t.Errorf("expected 2 write tool_uses (remember + index_text), got %d", n)
	}
}
```

- [ ] **Step 4.2: Run tests to verify they fail**

Run: `go test ./internal/cli/ -run TestExtractTranscriptSummary -v`
Expected: FAIL — signature mismatch (current returns only `string`). Compilation error.

Run: `go test ./internal/cli/ -run TestCountHeimdallWrites -v`
Expected: FAIL — `countHeimdallWrites` undefined.

- [ ] **Step 4.3: Implement `CandidateEvent` type, marker-scanning `extractTranscriptSummary`, and `countHeimdallWrites`**

Replace the current `extractTranscriptSummary` (lines 179–231 of `internal/cli/hook_stop.go`) with:

```go
// CandidateEvent is one remember-worthy moment detected in a transcript scan.
// Excerpt is capped at 200 chars; Marker is one of: correction, frustration,
// teaching, workaround.
type CandidateEvent struct {
	Marker  string
	Excerpt string
}

// Marker priority (highest first) for review-record ranking.
var candidateMarkerPriority = []string{"correction", "workaround", "teaching", "frustration"}

// markerPatterns maps marker class → compiled regex. Regexes scan user
// messages only (assistant messages don't trigger a "user correction").
// Precompiled at package init; the set is small enough that per-message
// linear scan of all regexes is cheap compared to JSON parsing.
var markerPatterns = map[string]*regexp.Regexp{
	"correction":  regexp.MustCompile(`(?i)\bactually\b|\bno,?\s|\bdon['’]?t\b|\bstop\b|\binstead\b|\bwrong\b`),
	"frustration": regexp.MustCompile(`(?i)\bfuck\b|\bwhy\b(?!\s+not)|\bbroken\b`),
	"teaching":    regexp.MustCompile(`(?i)\bturns out\b|\bfyi\b|\bheads up\b|\bfor reference\b`),
	"workaround":  regexp.MustCompile(`(?i)\bworkaround\b|\bhack\b|\btrick\b|\bgotcha\b`),
}

const (
	extractorMaxOutput   = 20 * 1024 // 20 KB summary cap
	extractorExcerptMax  = 200       // per-candidate excerpt cap
	extractorTailCount   = 3         // tail-fallback: last N assistant messages
	extractorTailLen     = 800       // per-tail-message trim
)

// extractTranscriptSummary scans the full JSONL transcript for user-side
// marker hits (correction / frustration / teaching / workaround) and
// returns:
//
//   - summary: concatenated context windows around each hit (user message
//     + preceding assistant message, omitted when no prior assistant turn
//     exists), deduplicated, separated by "\n\n---\n\n", capped at 20 KB.
//     On zero hits, falls back to the last 3 assistant messages trimmed to
//     800 chars each so downstream IngestSessionSummary isn't fed empty.
//
//   - candidates: one entry per hit with marker class + ≤200-char excerpt
//     from the user message. Used by HookSessionEnd to populate
//     last-session-review.json.
//
// Single byte-scan pass; O(len(data)) + O(hits × regex_cost).
func extractTranscriptSummary(data []byte) (string, []CandidateEvent) {
	type parsed struct {
		role    string
		content string
	}
	var msgs []parsed

	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var entry map[string]any
		if json.Unmarshal(line, &entry) != nil {
			continue
		}
		role, _ := entry["role"].(string)
		if role == "" {
			continue
		}
		content := extractContentText(entry["content"])
		if content == "" {
			continue
		}
		msgs = append(msgs, parsed{role: role, content: content})
	}

	var candidates []CandidateEvent
	var windows []string
	seen := map[string]bool{}

	for i, m := range msgs {
		if m.role != "user" {
			continue
		}
		for _, marker := range candidateMarkerPriority {
			if markerPatterns[marker].MatchString(m.content) {
				excerpt := m.content
				if len(excerpt) > extractorExcerptMax {
					excerpt = excerpt[:extractorExcerptMax]
				}
				candidates = append(candidates, CandidateEvent{Marker: marker, Excerpt: excerpt})

				// Window = preceding assistant msg (if any) + this user msg.
				var win strings.Builder
				if i > 0 && msgs[i-1].role == "assistant" {
					win.WriteString("assistant: ")
					win.WriteString(trimTo(msgs[i-1].content, extractorTailLen))
					win.WriteString("\n\n")
				}
				win.WriteString("user: ")
				win.WriteString(trimTo(m.content, extractorTailLen))
				s := win.String()
				if !seen[s] {
					seen[s] = true
					windows = append(windows, s)
				}
				break // one marker class per message; first match wins
			}
		}
	}

	// Tail fallback if no marker hits — last 3 assistant messages.
	if len(windows) == 0 {
		var tail []string
		for i := len(msgs) - 1; i >= 0 && len(tail) < extractorTailCount; i-- {
			if msgs[i].role == "assistant" {
				tail = append([]string{trimTo(msgs[i].content, extractorTailLen)}, tail...)
			}
		}
		windows = tail
	}

	summary := strings.Join(windows, "\n\n---\n\n")
	if len(summary) > extractorMaxOutput {
		summary = summary[:extractorMaxOutput]
	}
	return summary, candidates
}

// extractContentText handles both Claude Code transcript content shapes:
// a bare string, or an array of content blocks with {"type":"text","text":"..."}
// and optionally {"type":"tool_use",...}. Returns the concatenated text
// portion; tool_use blocks are ignored here (counted separately by
// countHeimdallWrites).
func extractContentText(raw any) string {
	switch v := raw.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, block := range v {
			m, ok := block.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := m["type"].(string); t == "text" {
				if text, _ := m["text"].(string); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func trimTo(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// countHeimdallWrites returns the number of tool_use blocks in the
// transcript whose name is heimdall_remember or heimdall_index_text.
// Used by HookSessionEnd to compute the writes field in the review record.
func countHeimdallWrites(data []byte) int {
	n := 0
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var entry map[string]any
		if json.Unmarshal(line, &entry) != nil {
			continue
		}
		raw, ok := entry["content"].([]any)
		if !ok {
			continue
		}
		for _, block := range raw {
			m, ok := block.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := m["type"].(string); t != "tool_use" {
				continue
			}
			name, _ := m["name"].(string)
			if name == "heimdall_remember" || name == "heimdall_index_text" {
				n++
			}
		}
	}
	return n
}
```

Add imports at the top of `hook_stop.go`: `"bytes"`, `"regexp"`, `"strings"` (verify which are already present).

- [ ] **Step 4.4: Run extractor tests to verify they pass**

Run: `go test ./internal/cli/ -run TestExtractTranscriptSummary -v`
Expected: all three PASS.

Run: `go test ./internal/cli/ -run TestCountHeimdallWrites -v`
Expected: PASS.

- [ ] **Step 4.5: Write the review-record writer tests**

Add to `internal/cli/hook_stop_test.go`:

```go
func TestHookSessionEnd_WritesReviewRecord(t *testing.T) {
	dir := t.TempDir()

	// Synthetic transcript with 5 corrections and 0 heimdall tool uses.
	tPath := filepath.Join(dir, "transcript.jsonl")
	lines := []string{
		`{"role":"user","content":"no, actually do it the other way"}`,
		`{"role":"user","content":"don't use that flag"}`,
		`{"role":"user","content":"stop — that's wrong"}`,
		`{"role":"user","content":"instead of X use Y"}`,
		`{"role":"user","content":"actually move the check before the loop"}`,
	}
	if err := os.WriteFile(tPath, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	payload := sessionEndPayload{
		SessionID:      "s-review-1",
		TranscriptPath: tPath,
		CWD:            dir,
		HookEventName:  "SessionEnd",
		Reason:         "user_exit",
	}
	data, _ := json.Marshal(payload)

	cfg := config.DefaultConfig()
	code := HookSessionEnd(cfg, bytes.NewReader(data), &bytes.Buffer{}, &bytes.Buffer{}, map[string]string{}, nil)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}

	reviewPath := filepath.Join(dir, ".heimdall_db", "hooks", "last-session-review.json")
	body, err := os.ReadFile(reviewPath)
	if err != nil {
		t.Fatalf("review file not written: %v", err)
	}
	var rev map[string]any
	if err := json.Unmarshal(body, &rev); err != nil {
		t.Fatalf("review file malformed: %v", err)
	}
	if int(rev["candidates"].(float64)) != 5 {
		t.Errorf("expected 5 candidates, got %v", rev["candidates"])
	}
	if int(rev["writes"].(float64)) != 0 {
		t.Errorf("expected 0 writes, got %v", rev["writes"])
	}
}

func TestHookSessionEnd_SuppressesWhenClean(t *testing.T) {
	dir := t.TempDir()

	// 1 correction + 2 heimdall_remember writes → suppression rule fires.
	tPath := filepath.Join(dir, "transcript.jsonl")
	lines := []string{
		`{"role":"user","content":"actually change the default to 42"}`,
		`{"role":"assistant","content":[{"type":"tool_use","name":"heimdall_remember","input":{}}]}`,
		`{"role":"assistant","content":[{"type":"tool_use","name":"heimdall_remember","input":{}}]}`,
	}
	if err := os.WriteFile(tPath, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	payload := sessionEndPayload{
		SessionID:      "s-clean",
		TranscriptPath: tPath,
		CWD:            dir,
		HookEventName:  "SessionEnd",
	}
	data, _ := json.Marshal(payload)

	code := HookSessionEnd(config.DefaultConfig(), bytes.NewReader(data), &bytes.Buffer{}, &bytes.Buffer{}, map[string]string{}, nil)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}

	reviewPath := filepath.Join(dir, ".heimdall_db", "hooks", "last-session-review.json")
	if _, err := os.Stat(reviewPath); !os.IsNotExist(err) {
		t.Errorf("expected no review file for clean session; err=%v", err)
	}
}

func TestHookSessionEnd_SuppressesOnEmptyTranscript(t *testing.T) {
	dir := t.TempDir()
	tPath := filepath.Join(dir, "transcript.jsonl")
	if err := os.WriteFile(tPath, []byte{}, 0644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	payload := sessionEndPayload{
		SessionID:      "s-empty",
		TranscriptPath: tPath,
		CWD:            dir,
		HookEventName:  "SessionEnd",
	}
	data, _ := json.Marshal(payload)

	code := HookSessionEnd(config.DefaultConfig(), bytes.NewReader(data), &bytes.Buffer{}, &bytes.Buffer{}, map[string]string{}, nil)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}

	reviewPath := filepath.Join(dir, ".heimdall_db", "hooks", "last-session-review.json")
	if _, err := os.Stat(reviewPath); !os.IsNotExist(err) {
		t.Errorf("expected no review file for empty transcript")
	}
}
```

- [ ] **Step 4.6: Run writer tests to verify they fail**

Run: `go test ./internal/cli/ -run TestHookSessionEnd_ -v`
Expected: the two new assertion tests FAIL (HookSessionEnd doesn't write a review file yet). `TestHookSessionEnd_SuppressesOnEmptyTranscript` may PASS incidentally (nothing wrote a review). That's fine.

- [ ] **Step 4.7: Extend `HookSessionEnd` to write the review record**

In `internal/cli/hook_stop.go`, add the new helpers and modify `HookSessionEnd`. Insert the review-writer logic just before the `os.Remove(bufferPath)` call:

```go
// writeLastSessionReview persists a review record to
// .heimdall_db/hooks/last-session-review.json when the session
// warrants nagging the next SessionStart. Suppression rule (from spec
// §4.1): skip when candidates <= 2 AND writes >= 1 (well-behaved session).
// Only the 3 highest-priority excerpts are persisted.
func writeLastSessionReview(projectDir, sessionID string, endedAt time.Time, candidates []CandidateEvent, writes int) error {
	if len(candidates) <= 2 && writes >= 1 {
		return nil
	}

	// Rank by priority, then by position-in-slice (later = more recent).
	priorityIdx := map[string]int{}
	for i, m := range candidateMarkerPriority {
		priorityIdx[m] = i
	}
	ranked := make([]CandidateEvent, len(candidates))
	copy(ranked, candidates)
	sort.SliceStable(ranked, func(i, j int) bool {
		return priorityIdx[ranked[i].Marker] < priorityIdx[ranked[j].Marker]
	})

	topMarkers := make([]string, 0, 3)
	excerpts := make([]string, 0, 3)
	for i, c := range ranked {
		if i >= 3 {
			break
		}
		topMarkers = append(topMarkers, c.Marker)
		excerpts = append(excerpts, c.Excerpt)
	}

	record := map[string]any{
		"session_id":  sessionID,
		"ended_at":    endedAt.Unix(),
		"candidates":  len(candidates),
		"writes":      writes,
		"top_markers": topMarkers,
		"excerpts":    excerpts,
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Join(projectDir, ".heimdall_db", "hooks")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "last-session-review.json"), data, 0600)
}
```

Then in `HookSessionEnd`, replace the existing transcript-handling block (the `if payload.TranscriptPath != "" { … }` section) to also invoke the writer:

```go
	if payload.TranscriptPath != "" {
		if _, err := os.Stat(payload.TranscriptPath); err == nil {
			transcript, err := os.ReadFile(payload.TranscriptPath)
			if err == nil && len(transcript) > 0 {
				summary, candidates := extractTranscriptSummary(transcript)
				writes := countHeimdallWrites(transcript)

				// Existing ingestion path (unchanged call site).
				if summary != "" {
					memDBPath := config.ResolveMemoryDBPath()
					memStore, err := heimdall.OpenMemoryStore(memDBPath)
					if err == nil {
						defer memStore.Close()
						ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
						defer cancel()
						client := newOllamaClient(cfg)
						embedder := heimdall.NewOllamaEmbedder(client, cfg.Model)
						result, err := heimdall.IngestSessionSummary(ctx, summary, "", embedder, memStore)
						if err == nil {
							heimdall.LogHookEvent("INFO", "session-end", map[string]any{
								"session":  payload.SessionID,
								"reason":   payload.Reason,
								"msg":      "ingest_ok",
								"memories": result.MemoriesCreated + result.MemoriesUpdated,
							})
						}
					}
				}

				// New: write the review record. Errors here must not fail the
				// hook — log and continue. OQ-5 rule: always exit 0.
				if err := writeLastSessionReview(projectDir, payload.SessionID, time.Now(), candidates, writes); err != nil {
					heimdall.LogHookEvent("WARN", "session-end", map[string]any{
						"err": "review_write_failed",
						"msg": err.Error(),
					})
				}
			}
		}
	}
```

Add `"sort"` to the imports in `hook_stop.go`.

- [ ] **Step 4.8: Run writer tests to verify they pass**

Run: `go test ./internal/cli/ -run TestHookSessionEnd_ -v`
Expected: all three PASS.

Run full package:
Run: `go test ./internal/cli/ -v`
Expected: all existing tests still PASS.

- [ ] **Step 4.9: Commit**

```bash
git add internal/cli/hook_stop.go internal/cli/hook_stop_test.go internal/cli/testdata/transcript_markers.jsonl internal/cli/testdata/transcript_tool_use.jsonl
git commit -m "feat(hooks): widen transcript extractor + write session review record

Replaces the last-5/500ch extractor with a marker-scanning scan that
returns both the summary fed to IngestSessionSummary and a candidate
list. HookSessionEnd now persists a last-session-review.json record
when the session warrants nagging the next SessionStart (candidates>2
or writes==0). Handles both content-as-string and content-as-blocks
transcript shapes. Addresses P0 #1 (parts A+B) from
docs/plans/claude-heimdall-self-use/."
```

---

## Task 5: Acceptance gate — manual two-session end-to-end

**Goal:** Verify the full round-trip in a real Claude Code session. This can't be unit-tested — it requires Claude Code to actually fire SessionStart/SessionEnd hooks and emit tool_use blocks into a real transcript.

**Prerequisites:**
- Tasks 1–4 merged to `main`.
- `make build` has produced a fresh `heimdall-mcp` binary.
- `heimdall-mcp install-hooks` has been run in this repo.
- Ollama is running and the configured model is pulled.

- [ ] **Step 5.1: Session A — generate transcript with corrections and no heimdall writes**

Open a fresh Claude Code session in this repo. Do the following in sequence, *without* using heimdall tools:

1. Ask a trivial code question.
2. When Claude answers, reply: `actually no, that's wrong — do X instead`.
3. Say: `FYI the build flag is -tags=integration, heads up`.
4. Say: `the workaround for macOS ::1 is to bind to 127.0.0.1`.
5. Say `thanks, bye` and close the session.

Expected on close:
- `hooks.log` shows `session_ended` + `review_write_ok` (or no `review_write_failed`).
- `ls <repo>/.heimdall_db/hooks/last-session-review.json` — file exists.
- `cat` of that file shows `candidates ≥ 3`, `writes == 0`, at least one `correction` and one `teaching` marker, and three excerpts.

- [ ] **Step 5.2: Session B — verify the nag renders and is consumed**

Open a fresh Claude Code session in this repo. The first model turn should see a SessionStart context block that now contains `### Last session review` with the 3 excerpts from Session A.

Expected:
- After the first prompt, `ls <repo>/.heimdall_db/hooks/last-session-review.json` shows the file is **gone** (consumed).
- Ask Claude "have you been using heimdall?" — the response should acknowledge the review block as a task, and Claude should call `heimdall_remember` on at least one of the surfaced items.

- [ ] **Step 5.3: Verify the read/write banner split**

In a scratch directory, pull a *different* Ollama embedding model than what the index was built with (e.g. if the index used `nomic-embed-text`, also pull `all-minilm`). Point `HEIMDALL_EMBED_MODEL` (or equivalent config) at the different model, then start a Claude Code session in that directory.

Expected: SessionStart injection shows the new two-line banner:
```
> heimdall_search: unavailable (index model mismatch)
> heimdall_remember and heimdall_index_text still work — use them for this session's decisions.
```

- [ ] **Step 5.4: Verify the new MCP instruction block is served**

With the MCP server running, check the `initialize` response instructions. One way: start a fresh Claude Code session and run a simple `claude mcp` diagnostic (or just start a session and observe that Claude's internal MCP handshake sees the new text — evidenced by Claude adopting the WHEN→ACTION framing in its tool usage).

Expected: no regressions in normal tool usage; Claude's behavior skews toward same-turn `heimdall_remember` after user corrections.

- [ ] **Step 5.5: Record acceptance**

Append a line to `docs/plans/claude-heimdall-self-use/00-problem-and-fixes.md` under a new `## Acceptance` section: the date, the commits in Tasks 1–4, and a one-sentence pass/fail note for each of 5.1–5.4. If any sub-step fails, that's a bug in the corresponding Task — open a new issue and re-run the affected steps once fixed.

This step produces no code; it closes the loop.

---

## Self-review

1. **Spec coverage:**
   - §3 widened extractor → Task 4 steps 4.1–4.4, 4.9. ✓
   - §4.1 review-record writer → Task 4 steps 4.5–4.8, 4.9. ✓
   - §4.2 SessionStart nag renderer → Task 3. ✓
   - §5 MCP instruction rewrite → Task 1 (text extracted to `heimdallInstructions` constant for testability). ✓
   - §6 banner scoping → Task 2. ✓
   - §7 test groups — all named test functions appear in the corresponding Task. ✓
   - §8 non-goals — none pulled in. ✓
   - §9 acceptance — Task 5 covers each success criterion. ✓

2. **Placeholder scan:** no TBD/TODO/"similar to Task N" references. Every code step contains concrete code, imports, and expected output.

3. **Type consistency:**
   - `CandidateEvent{Marker, Excerpt}` — used in Task 4 (definition), referenced by signature in renderer's JSON (Task 3 via the mirror type `lastSessionReview{TopMarkers, Excerpts}`). The on-disk format carries only strings; the in-process type is only used inside `hook_stop.go`. No cross-package type identity required.
   - `renderLastSessionReview(projectDir string, now time.Time) string` — defined and called with matching signature in Task 3.
   - `writeLastSessionReview(projectDir, sessionID string, endedAt time.Time, candidates []CandidateEvent, writes int) error` — defined and called in Task 4.
   - `extractTranscriptSummary(data []byte) (string, []CandidateEvent)` — new signature; all callers updated in Task 4.7.
   - `countHeimdallWrites(data []byte) int` — new, self-contained.

---

## Execution handoff

Plan complete and saved to `docs/plans/claude-heimdall-self-use/02-p0-plan.md`. Two execution options:

**1. Subagent-Driven (recommended)** — dispatch a fresh subagent per Task (1 through 4), review between tasks, fast iteration. Task 5 runs in the coordinator session because it's a manual two-session test.

**2. Inline Execution** — walk through Tasks 1–4 in this session using superpowers:executing-plans, batch execution with checkpoints before each commit.

Which approach?
