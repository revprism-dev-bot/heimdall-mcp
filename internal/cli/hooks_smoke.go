// hooks_smoke.go — `heimdall-mcp hooks smoke` end-to-end smoke harness.
//
// Motivation: manual "reopen Claude Code and eyeball hooks.log" dogfooding is
// slow and skippable. This command fires one synthesized Claude Code payload
// per installed hook, captures the exit code plus whichever hooks.log line the
// handler produced, and prints a pass/fail summary.
//
// Design:
//   - Each step runs in-process against a tempdir-scoped hooks.log so the user's
//     real log is never touched.
//   - `--fake-ollama` spins an httptest.Server that mimics /api/tags and
//     /api/embed (same shape as the Layer-2 integration tests) and points the
//     retrieval hooks at it via an override config. Without the flag the hooks
//     hit whatever endpoint `heimdall-mcp config` already resolves — useful for
//     verifying a real local Ollama setup.
//   - The per-step contract mirrors plan §5.9 / OQ-5: exit 0 for retrieval
//     hooks, empty stderr, and a recognisable `stage=` / `msg=` token in the
//     per-step log slice.
//
// Not replaced by this harness:
//   - `hooks doctor` still diagnoses the *installed* settings.json and covers
//     the 14-check rollup (PATH lookup, model pulled, index presence, etc.).
//     The smoke harness only proves the hook CLI contracts.
//   - Layer-3 `claude` e2e still validates the real Claude Code binary path.
package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// HooksSmoke is the entry point for `heimdall-mcp hooks smoke`.
//
// Flags:
//   - `--fake-ollama`       spin an in-process fake Ollama and a seeded index
//                           so the retrieval hooks can complete offline.
//   - `--format=text|json`  pick output shape; text is human, json is machine.
//
// Exit codes:
//   - 0 if every step passes.
//   - 1 if any step fails.
//   - 2 on a CLI usage error (bad flag, unknown subcommand).
func HooksSmoke(cfg config.Config, stdin io.Reader, stdout, stderr io.Writer, env map[string]string, args []string) int {
	opts, err := parseSmokeFlags(args)
	if err != nil {
		fmt.Fprintf(stderr, "hooks smoke: %v\n", err)
		return 2
	}

	res, runErr := runSmoke(cfg, env, opts)
	if runErr != nil {
		fmt.Fprintf(stderr, "hooks smoke: %v\n", runErr)
		return 1
	}

	if opts.format == "json" {
		writeJSON(stdout, res)
	} else {
		writeSmokeText(stdout, res)
	}

	if res.Failed > 0 {
		return 1
	}
	return 0
}

// smokeOptions captures the flag surface. Kept as a small struct so tests can
// build one directly without re-parsing strings.
type smokeOptions struct {
	fakeOllama bool
	format     string // "text" or "json"
}

func parseSmokeFlags(args []string) (smokeOptions, error) {
	opts := smokeOptions{format: "text"}
	// Normalize `--flag=value` into `--flag`, `value` for a single switch
	// path — same helper used by `hooks tail`.
	args = splitEqualsFlags(args)
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--fake-ollama":
			opts.fakeOllama = true
		case "--format":
			if i+1 >= len(args) {
				return opts, errors.New("--format requires a value")
			}
			i++
			switch args[i] {
			case "text", "json":
				opts.format = args[i]
			default:
				return opts, fmt.Errorf("invalid --format %q (want text|json)", args[i])
			}
		case "--help", "-h":
			return opts, errors.New("usage: hooks smoke [--fake-ollama] [--format=text|json]")
		default:
			return opts, fmt.Errorf("unknown flag %q", a)
		}
	}
	return opts, nil
}

// SmokeStepResult is the outcome of one hook's synthesized fire.
//
// Exported so the JSON serialization is stable and scripts can consume it.
type SmokeStepResult struct {
	Name        string        `json:"name"`        // human-readable hook label, e.g. "SessionStart"
	Hook        string        `json:"hook"`        // CLI subcommand fired, e.g. "session-start"
	Pass        bool          `json:"pass"`        // true iff every assertion held
	ExitCode    int           `json:"exit_code"`   // code returned by the handler
	Duration    time.Duration `json:"duration_ns"` // wall-time spent in the handler
	LogLine     string        `json:"log_line"`    // captured hooks.log slice (first matching line)
	Stderr      string        `json:"stderr"`      // captured stderr (must be empty on retrieval path)
	Stdout      string        `json:"stdout"`      // captured stdout (informational only)
	Failure     string        `json:"failure,omitempty"`
}

// SmokeReport is the envelope returned to the caller.
type SmokeReport struct {
	Steps    []SmokeStepResult `json:"steps"`
	Total    int               `json:"total"`
	Passed   int               `json:"passed"`
	Failed   int               `json:"failed"`
	Duration time.Duration     `json:"duration_ns"`
	// FakeOllama is true when the harness spun up its own httptest Ollama,
	// so scripts can tell `pass` apart from "pass but trivially faked".
	FakeOllama bool `json:"fake_ollama"`
}

// runSmoke orchestrates the per-hook fires. The env map is snapshotted
// because we set HEIMDALL_HOOK_LOG on the child calls to a tempdir path.
func runSmoke(cfg config.Config, env map[string]string, opts smokeOptions) (SmokeReport, error) {
	// Work dir — holds the scoped hooks.log, config dir, and the seeded
	// project root.
	work, err := os.MkdirTemp("", "heimdall-smoke-*")
	if err != nil {
		return SmokeReport{}, fmt.Errorf("mkdtemp: %w", err)
	}
	defer os.RemoveAll(work)

	hookLog := filepath.Join(work, "hooks.log")
	projectRoot := filepath.Join(work, "project")
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		return SmokeReport{}, fmt.Errorf("mkdir project: %w", err)
	}
	// Fully hermetic: every sub-path resolved via env (HEIMDALL_HOOK_LOG,
	// XDG_CONFIG_HOME, XDG_STATE_HOME) redirected into the work tempdir so
	// we never touch the user's real hooks.log or memory DB. Restored on
	// defer so the harness is re-entrant inside the same process.
	scopedConfigHome := filepath.Join(work, "xdg-config")
	scopedStateHome := filepath.Join(work, "xdg-state")
	_ = os.MkdirAll(scopedConfigHome, 0o755)
	_ = os.MkdirAll(scopedStateHome, 0o755)

	// Copy env so we don't mutate the caller's map.
	scopedEnv := make(map[string]string, len(env)+4)
	for k, v := range env {
		scopedEnv[k] = v
	}
	scopedEnv["HEIMDALL_HOOK_LOG"] = hookLog
	scopedEnv["XDG_CONFIG_HOME"] = scopedConfigHome
	scopedEnv["XDG_STATE_HOME"] = scopedStateHome
	// Force the process-wide env too — HookLogPath() + ResolveMemoryDBPath()
	// read from os.Getenv, not from our env map. Preserve prior values so
	// we can restore after the run (otherwise subsequent CLI calls would
	// leak the tmpdir paths).
	envRestore := snapshotEnv("HEIMDALL_HOOK_LOG", "XDG_CONFIG_HOME", "XDG_STATE_HOME")
	_ = os.Setenv("HEIMDALL_HOOK_LOG", hookLog)
	_ = os.Setenv("XDG_CONFIG_HOME", scopedConfigHome)
	_ = os.Setenv("XDG_STATE_HOME", scopedStateHome)
	defer envRestore()

	// Force `shadow` mode for the pre-tool-use hook so the default run
	// exits 0 on whichever classification the benign `ls` payload produces.
	// Same logic as hooks doctor's classifier check — we verify the wiring
	// compiled, not that `block` mode is correct (that's a unit test).
	scopedEnv["HEIMDALL_GUARDRAILS"] = "shadow"

	// Optionally spin a fake Ollama + seed an index so the retrieval hooks
	// have something to work against.
	var fake *httptest.Server
	if opts.fakeOllama {
		var err error
		fake, err = startSmokeFakeOllama(cfg.Model)
		if err != nil {
			return SmokeReport{}, fmt.Errorf("fake ollama: %w", err)
		}
		defer fake.Close()
		cfg.OllamaEndpoint = fake.URL
		// Seed the project's vector store with the right metadata so
		// VerifyHookIndex passes inside session-start / user-prompt.
		if err := seedSmokeVectorStore(projectRoot, cfg.Model); err != nil {
			return SmokeReport{}, fmt.Errorf("seed vector store: %w", err)
		}
	}

	report := SmokeReport{FakeOllama: opts.fakeOllama}
	started := time.Now()

	steps := buildSmokeSteps(projectRoot)
	for _, step := range steps {
		report.Steps = append(report.Steps, runSmokeStep(cfg, scopedEnv, step, hookLog))
	}

	for _, s := range report.Steps {
		report.Total++
		if s.Pass {
			report.Passed++
		} else {
			report.Failed++
		}
	}
	report.Duration = time.Since(started)
	return report, nil
}

// smokeStep describes one hook fire: which subcommand to call, the payload
// to put on stdin, and how to assert the outcome.
type smokeStep struct {
	name    string
	hook    string // matches `heimdall-mcp hook <hook>` subcommand
	payload map[string]any
	// extraArgs are appended after the subcommand name when dispatching.
	// Most hooks read the project root from stdin `cwd`; post-edit needs
	// `--project <path>` because its payload schema has no cwd field.
	extraArgs []string
	// assert verifies the captured outputs. Returning a non-empty string
	// marks the step as failed with that reason.
	assert func(stdout, stderr, logSlice string, exit int) string
	// expectStderr, when true, suppresses the empty-stderr assertion that
	// every retrieval hook must satisfy. Only flipped for future hooks that
	// deliberately write to stderr (none today — pre-tool-use only does in
	// block mode, and the harness forces shadow).
	expectStderr bool
}

// buildSmokeSteps returns the 6-hook fixture in canonical order. projectRoot
// is the tempdir with a seeded index the payloads point at.
func buildSmokeSteps(projectRoot string) []smokeStep {
	sessionID := "smoke-session-1"
	editedPath := filepath.Join(projectRoot, "example.go")

	return []smokeStep{
		{
			name: "SessionStart",
			hook: "session-start",
			payload: map[string]any{
				"session_id":      sessionID,
				"hook_event_name": "SessionStart",
				"source":          "startup",
				"cwd":             projectRoot,
			},
			assert: func(stdout, stderr, logSlice string, exit int) string {
				if exit != 0 {
					return fmt.Sprintf("exit=%d (want 0)", exit)
				}
				// The happy path emits `stage=ok`. Degraded branches
				// (no index / ollama down) emit `stage=` tokens like
				// `resolve_model`, `ollama_ping`, etc. We accept any
				// `stage=` as proof the handler ran; the pass/fail
				// split for the smoke harness is "did the hook
				// actually execute", not "did the happy path fire".
				if !strings.Contains(logSlice, "event=session-start") {
					return "no event=session-start line in hooks.log"
				}
				if !strings.Contains(logSlice, "stage=") {
					return "no stage= token in session-start log line"
				}
				return ""
			},
		},
		{
			name: "UserPromptSubmit",
			hook: "user-prompt",
			payload: map[string]any{
				"session_id":      sessionID,
				"hook_event_name": "UserPromptSubmit",
				"prompt":          "explain how the hook smoke harness works",
				"cwd":             projectRoot,
			},
			assert: func(stdout, stderr, logSlice string, exit int) string {
				if exit != 0 {
					return fmt.Sprintf("exit=%d (want 0)", exit)
				}
				if !strings.Contains(logSlice, "event=user-prompt") {
					return "no event=user-prompt line in hooks.log"
				}
				if !strings.Contains(logSlice, "stage=") {
					return "no stage= token in user-prompt log line"
				}
				return ""
			},
		},
		{
			name: "PostToolUse(Edit|Write)",
			hook: "post-edit",
			payload: map[string]any{
				"session_id": sessionID,
				"tool_name":  "Edit",
				"tool_input": map[string]any{
					"file_path": editedPath,
				},
			},
			// post-edit schema has no `cwd` field, so project root must
			// come from the flag.
			extraArgs: []string{"--project", projectRoot},
			assert: func(stdout, stderr, logSlice string, exit int) string {
				if exit != 0 {
					return fmt.Sprintf("exit=%d (want 0)", exit)
				}
				if !strings.Contains(logSlice, "event=post-edit") {
					return "no event=post-edit line in hooks.log"
				}
				// Happy path: `msg=actor_spawned`. Skipped path:
				// DEBUG `err=no_file_path`. We require one of the two
				// so a silent no-op (nothing logged) still fails.
				if !strings.Contains(logSlice, "msg=actor_spawned") &&
					!strings.Contains(logSlice, "err=") {
					return "expected msg=actor_spawned or an err= token in post-edit log line"
				}
				return ""
			},
		},
		{
			name: "PreToolUse(Bash)",
			hook: "pre-tool-use",
			payload: map[string]any{
				"session_id": sessionID,
				"tool_name":  "Bash",
				"tool_input": map[string]any{
					"command": "ls -la",
				},
				"cwd": projectRoot,
			},
			assert: func(stdout, stderr, logSlice string, exit int) string {
				// Shadow mode (forced by runSmoke) + allow classification
				// → exit 0, empty stdout/stderr.
				if exit != 0 {
					return fmt.Sprintf("exit=%d (want 0 in shadow+allow)", exit)
				}
				if !strings.Contains(logSlice, "event=pre-tool-use") {
					return "no event=pre-tool-use line in hooks.log"
				}
				if !strings.Contains(logSlice, "stage=classify") {
					return "expected stage=classify in pre-tool-use log line"
				}
				if !strings.Contains(logSlice, "mode=shadow") {
					return "expected mode=shadow in pre-tool-use log line"
				}
				return ""
			},
		},
		{
			name: "Stop",
			hook: "stop",
			payload: map[string]any{
				"session_id":             sessionID,
				"cwd":                    projectRoot,
				"stop_hook_active":       false,
				"last_assistant_message": "synthesized smoke-test last message",
				"hook_event_name":        "Stop",
			},
			assert: func(stdout, stderr, logSlice string, exit int) string {
				if exit != 0 {
					return fmt.Sprintf("exit=%d (want 0)", exit)
				}
				if !strings.Contains(logSlice, "event=stop") {
					return "no event=stop line in hooks.log"
				}
				if !strings.Contains(logSlice, "msg=buffer_appended") {
					return "expected msg=buffer_appended in stop log line"
				}
				return ""
			},
		},
		{
			name: "SessionEnd",
			hook: "session-end",
			payload: map[string]any{
				"session_id":      sessionID,
				"cwd":             projectRoot,
				"hook_event_name": "SessionEnd",
				"reason":          "user_exit",
			},
			assert: func(stdout, stderr, logSlice string, exit int) string {
				if exit != 0 {
					return fmt.Sprintf("exit=%d (want 0)", exit)
				}
				if !strings.Contains(logSlice, "event=session-end") {
					return "no event=session-end line in hooks.log"
				}
				if !strings.Contains(logSlice, "msg=session_ended") {
					return "expected msg=session_ended in session-end log line"
				}
				return ""
			},
		},
	}
}

// runSmokeStep fires one hook subcommand in-process (so the harness stays
// hermetic, skipping the fork+setsid path that the real `hook post-edit`
// would take when exec'd via the binary). Captures stdout/stderr + the bytes
// appended to hooks.log during this step, then runs the step's assertion.
func runSmokeStep(cfg config.Config, env map[string]string, step smokeStep, hookLog string) SmokeStepResult {
	res := SmokeStepResult{Name: step.name, Hook: step.hook}

	stdinBytes, _ := json.Marshal(step.payload)
	stdin := bytes.NewReader(stdinBytes)

	var stdoutBuf, stderrBuf bytes.Buffer

	// Capture the hooks.log slice produced *by this step only* so the
	// per-step log_line attribution is unambiguous. Record the size before
	// and read the delta after.
	beforeSize := fileSize(hookLog)

	started := time.Now()
	exit := dispatchSmokeHook(cfg, stdin, &stdoutBuf, &stderrBuf, env, step)
	res.Duration = time.Since(started)
	res.ExitCode = exit
	res.Stdout = stdoutBuf.String()
	res.Stderr = stderrBuf.String()

	logSlice := readLogSlice(hookLog, beforeSize)
	// Pull out the first line mentioning this hook's event name, if any.
	// That's what the caller typically wants to see.
	res.LogLine = firstLogLineForEvent(logSlice, step.hook)

	// Retrieval-hook contract: no stderr. PreToolUse shadow mode honors this
	// too. buildSmokeSteps doesn't currently flip expectStderr for anyone.
	if !step.expectStderr && strings.TrimSpace(res.Stderr) != "" {
		res.Failure = fmt.Sprintf("unexpected stderr: %q", res.Stderr)
		res.Pass = false
		return res
	}

	if msg := step.assert(res.Stdout, res.Stderr, logSlice, exit); msg != "" {
		res.Failure = msg
		res.Pass = false
		return res
	}
	res.Pass = true
	return res
}

// snapshotEnv returns a closure that restores the given env vars to the
// values they had at snapshot time. Unset → unset; otherwise → setenv back
// to the captured value. Used so the smoke harness can safely toggle
// HEIMDALL_HOOK_LOG and XDG_* without leaking into later CLI calls (the
// Go process stays alive between `heimdall-mcp` subcommands in embedded
// scenarios like unit tests and the MCP stdio loop).
func snapshotEnv(keys ...string) func() {
	type entry struct {
		key string
		val string
		set bool
	}
	entries := make([]entry, 0, len(keys))
	for _, k := range keys {
		v, ok := os.LookupEnv(k)
		entries = append(entries, entry{key: k, val: v, set: ok})
	}
	return func() {
		for _, e := range entries {
			if e.set {
				_ = os.Setenv(e.key, e.val)
			} else {
				_ = os.Unsetenv(e.key)
			}
		}
	}
}

// dispatchSmokeHook is the in-process cousin of DispatchHook: it routes each
// subcommand through the same handler logic, but swaps out the post-edit
// actor fork so we never spawn a background subprocess during a smoke run.
// Anything else (session-start, user-prompt, stop, session-end, pre-tool-use)
// uses the real entry point because those paths are fully in-process already.
func dispatchSmokeHook(cfg config.Config, stdin io.Reader, stdout, stderr io.Writer, env map[string]string, step smokeStep) int {
	args := append([]string{step.hook}, step.extraArgs...)
	if step.hook == "post-edit" {
		// Swap the real spawn for a no-op so the smoke run never forks a
		// detached actor. The foreground path still logs
		// msg=actor_spawned, which is the assertion the smoke step
		// relies on. Tests that need the actor behaviour live in
		// hook_post_edit_test.go.
		deps := defaultPostEditDeps()
		deps.Spawn = func(projectRoot, sessionID string) error { return nil }
		// Strip the leading subcommand — hookPostEditWithDeps expects
		// just the flag portion.
		return hookPostEditWithDeps(cfg, stdin, stdout, stderr, env, args[1:], deps)
	}
	return DispatchHook(cfg, stdin, stdout, stderr, env, args)
}

// fileSize returns 0 if the file is missing or unreadable — which is the
// correct "before" anchor for the very first step.
func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// readLogSlice returns the bytes appended to hookLog since `fromOffset`.
// Empty string on any read error — callers degrade gracefully.
func readLogSlice(hookLog string, fromOffset int64) string {
	f, err := os.Open(hookLog)
	if err != nil {
		return ""
	}
	defer f.Close()
	if fromOffset > 0 {
		if _, err := f.Seek(fromOffset, io.SeekStart); err != nil {
			return ""
		}
	}
	buf, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	return string(buf)
}

// firstLogLineForEvent returns the first line in `slice` whose `event=<name>`
// matches. Empty string if none found.
func firstLogLineForEvent(slice, event string) string {
	needle := "event=" + event
	for _, line := range strings.Split(slice, "\n") {
		if strings.Contains(line, needle) {
			return strings.TrimSpace(line)
		}
	}
	return strings.TrimSpace(slice)
}

// startSmokeFakeOllama mimics the relevant subset of Ollama used by the
// retrieval hooks. Identical shape to internal/cli/integration_test.go's
// fakeOllamaServer, collapsed into a plain httptest.Server so nothing in
// this path depends on testing.T.
func startSmokeFakeOllama(model string) (*httptest.Server, error) {
	if model == "" {
		model = "smoke-model"
	}
	const dim = 4
	var embedCount atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{{"name": model}},
		})
	})
	mux.HandleFunc("/api/embed", func(w http.ResponseWriter, r *http.Request) {
		embedCount.Add(1)
		var body struct {
			Input any `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		vec := make([]float32, dim)
		for i := range vec {
			vec[i] = 1.0 / float32(dim)
		}
		var vectors [][]float32
		switch in := body.Input.(type) {
		case string:
			vectors = [][]float32{vec}
		case []any:
			for range in {
				vectors = append(vectors, vec)
			}
		default:
			vectors = [][]float32{vec}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": vectors})
	})
	return httptest.NewServer(mux), nil
}

// seedSmokeVectorStore stamps the per-project .heimdall_db with an
// `embedding_model` + `embedding_dim` metadata row so VerifyHookIndex inside
// the retrieval hooks passes. Plus one seed chunk so the search path has
// something to return.
func seedSmokeVectorStore(projectRoot, model string) error {
	if model == "" {
		model = "smoke-model"
	}
	const dim = 4
	baseDir := filepath.Join(projectRoot, ".heimdall_db")
	dbDir := heimdall.ModelDBDir(baseDir, model)
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		return fmt.Errorf("mkdir dbdir: %w", err)
	}
	store, err := heimdall.OpenStore(dbDir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	if err := store.SetMetadata("embedding_model", model); err != nil {
		return fmt.Errorf("set model: %w", err)
	}
	if err := store.SetMetadata("embedding_dim", fmt.Sprintf("%d", dim)); err != nil {
		return fmt.Errorf("set dim: %w", err)
	}
	vec := make([]float32, dim)
	for i := range vec {
		vec[i] = 1.0 / float32(dim)
	}
	if err := store.Upsert([]heimdall.VectorRecord{{
		ID:        "smoke-seed-1",
		FilePath:  "seed.go",
		StartLine: 1,
		EndLine:   1,
		Content:   "package seed",
		Kind:      "paragraph",
		Embedding: vec,
	}}); err != nil {
		return fmt.Errorf("upsert seed: %w", err)
	}
	return nil
}

// writeSmokeText renders the human-readable summary. Example:
//
//   heimdall-mcp hooks smoke  (fake-ollama: true)
//
//   [PASS] SessionStart           (session-start)   12.4ms
//           hooks.log: 2026-...Z INFO event=session-start stage=ok ...
//   [FAIL] UserPromptSubmit       (user-prompt)     5ms
//           reason:   expected stage= token in user-prompt log line
//           stderr:   ...
//
//   Summary: 5/6 passed, 1 failed  (total: 34ms)
func writeSmokeText(w io.Writer, r SmokeReport) {
	fmt.Fprintf(w, "heimdall-mcp hooks smoke  (fake-ollama: %v)\n\n", r.FakeOllama)
	nameW := 0
	for _, s := range r.Steps {
		if len(s.Name) > nameW {
			nameW = len(s.Name)
		}
	}
	for _, s := range r.Steps {
		status := "PASS"
		if !s.Pass {
			status = "FAIL"
		}
		pad := strings.Repeat(" ", nameW-len(s.Name))
		fmt.Fprintf(w, "  [%s] %s%s  (%s)  %s\n",
			status, s.Name, pad, s.Hook, s.Duration.Round(time.Millisecond))
		if s.LogLine != "" {
			fmt.Fprintf(w, "         hooks.log: %s\n", s.LogLine)
		}
		if !s.Pass {
			fmt.Fprintf(w, "         reason:    %s\n", s.Failure)
			if strings.TrimSpace(s.Stderr) != "" {
				fmt.Fprintf(w, "         stderr:    %s\n", strings.TrimSpace(s.Stderr))
			}
		}
	}
	fmt.Fprintf(w, "\nSummary: %d/%d passed, %d failed  (total: %s)\n",
		r.Passed, r.Total, r.Failed, r.Duration.Round(time.Millisecond))
}

