// doctor.go — implements T16 `heimdall-mcp hooks doctor`.
//
// Fifteen checks are run in a fixed order and rendered as a small ASCII table.
// Each check returns a status (pass/warn/fail) plus a short message. Any
// `fail` row makes the overall command exit 1; `warn`-only or all-pass rows
// exit 0. ASCII markers are used unconditionally — `NO_COLOR` is honored by
// the absence of any color codes anywhere in the output.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// doctorStatus is a small enum for check outcomes.
type doctorStatus int

const (
	statusPass doctorStatus = iota
	statusWarn
	statusFail
)

func (s doctorStatus) marker() string {
	switch s {
	case statusPass:
		return "[OK]"
	case statusWarn:
		return "[!!]"
	case statusFail:
		return "[XX]"
	}
	return "[??]"
}

// doctorCheck is one row of the doctor table.
type doctorCheck struct {
	name    string
	status  doctorStatus
	message string
}

// doctorDeps holds the injectable dependencies the checks need so that tests
// can swap any of them for fakes. Production callers pass an empty struct
// and the runner fills in real ones from cfg/env.
type doctorDeps struct {
	// settingsPath overrides scope detection for tests.
	settingsPath string
	// lookupPath stubs exec.LookPath. Defaults to exec.LookPath.
	lookupPath func(string) (string, error)
	// runVersion runs `<bin> --version` and returns nil on success. Defaults
	// to a real exec call with a 5s timeout.
	runVersion func(bin string) error
	// pingOllama checks Ollama reachability. Defaults to a Ping using the
	// configured endpoint.
	pingOllama func(ctx context.Context, endpoint string) error
	// listModels returns the locally pulled Ollama model names. Defaults to a
	// real ListModels call using the configured endpoint.
	listModels func(ctx context.Context, endpoint string) ([]string, error)
	// projectRoot overrides project detection for tests.
	projectRoot string
	// dryFire invokes the hook command end-to-end (with a canned event JSON
	// on stdin) and returns nil on exit 0. Defaults to a real exec call.
	dryFire func(command string) error
	// hookLogPath overrides HookLogPath for tests.
	hookLogPath string
	// nowOnDisk is the canned event-JSON payload used by dryFire.
	dryFireStdin []byte
	// skillsDir overrides ResolveClaudeSkillsDir for the skills-sync check.
	// Empty string means "use the env-resolved default".
	skillsDir string
	// memoryDBPath overrides config.ResolveMemoryDBPath for the
	// skills-sync check so tests don't touch the user's real memory DB.
	memoryDBPath string
}

// HooksDoctor implements the §5.4 handler shape. Wired into DispatchHooks
// under the `hooks doctor` subcommand. Returns 0 on green/yellow, 1 on red.
func HooksDoctor(cfg config.Config, stdin io.Reader, stdout, stderr io.Writer, env map[string]string, args []string) int {
	_ = stdin
	scopeFlag, err := parseDoctorFlags(args)
	if err != nil {
		fmt.Fprintf(stderr, "hooks doctor: %v\n", err)
		return 2
	}
	scope, settingsPath, scopeErr := resolveScope(scopeFlag, env)
	if scopeErr != nil {
		fmt.Fprintf(stderr, "hooks doctor: %v\n", scopeErr)
		return 1
	}
	deps := doctorDeps{
		settingsPath: settingsPath,
	}
	checks := runDoctorChecks(cfg, env, deps)
	fmt.Fprintf(stdout, "scope=%s\nsettings=%s\n\n", scope, settingsPath)
	renderDoctorTable(stdout, checks)
	for _, c := range checks {
		if c.status == statusFail {
			return 1
		}
	}
	return 0
}

func parseDoctorFlags(args []string) (string, error) {
	scope := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--scope":
			if i+1 >= len(args) {
				return "", errors.New("--scope requires a value")
			}
			i++
			scope = args[i]
		case strings.HasPrefix(a, "--scope="):
			scope = strings.TrimPrefix(a, "--scope=")
		case a == "-h" || a == "--help":
			return "", errors.New("usage: hooks doctor [--scope=user|project]")
		default:
			return "", fmt.Errorf("unknown flag %q", a)
		}
	}
	if scope != "" && scope != "user" && scope != "project" {
		return "", fmt.Errorf("invalid --scope %q (want user|project)", scope)
	}
	return scope, nil
}

// renderDoctorTable prints a fixed-width table to w.
func renderDoctorTable(w io.Writer, checks []doctorCheck) {
	maxName := 0
	for _, c := range checks {
		if len(c.name) > maxName {
			maxName = len(c.name)
		}
	}
	for _, c := range checks {
		pad := strings.Repeat(" ", maxName-len(c.name))
		fmt.Fprintf(w, "  %s %s%s  %s\n", c.status.marker(), c.name, pad, c.message)
	}
}

// runDoctorChecks executes all 15 checks in order. Pure-ish: the only side
// effects are file reads on the chosen settings.json path, exec calls via
// deps, and HTTP calls via deps. Tests inject doctorDeps to bypass exec/net.
func runDoctorChecks(cfg config.Config, env map[string]string, deps doctorDeps) []doctorCheck {
	deps = fillDoctorDeps(cfg, deps)
	checks := make([]doctorCheck, 0, 15)

	// 1 — settings.json exists at the chosen scope?
	settingsBytes, settingsErr := os.ReadFile(deps.settingsPath)
	switch {
	case settingsErr == nil:
		checks = append(checks, doctorCheck{
			name: "settings.json present", status: statusPass,
			message: deps.settingsPath,
		})
	case errors.Is(settingsErr, os.ErrNotExist):
		checks = append(checks, doctorCheck{
			name: "settings.json present", status: statusFail,
			message: "missing: " + deps.settingsPath,
		})
	default:
		checks = append(checks, doctorCheck{
			name: "settings.json present", status: statusFail,
			message: settingsErr.Error(),
		})
	}

	// 2 — does it parse as JSON?
	var parsed map[string]any
	parseOK := false
	if settingsErr == nil {
		if perr := json.Unmarshal(settingsBytes, &parsed); perr == nil {
			parseOK = true
			checks = append(checks, doctorCheck{
				name: "settings.json parses", status: statusPass, message: "valid JSON",
			})
		} else {
			checks = append(checks, doctorCheck{
				name: "settings.json parses", status: statusFail,
				message: "invalid JSON: " + perr.Error(),
			})
		}
	} else {
		checks = append(checks, doctorCheck{
			name: "settings.json parses", status: statusFail,
			message: "skipped (file missing)",
		})
	}

	// 3 — any heimdall hooks installed?
	installedEntries := []map[string]any{}
	if parseOK {
		installedEntries = collectHeimdallEntries(parsed)
	}
	switch {
	case !parseOK:
		checks = append(checks, doctorCheck{
			name: "heimdall hooks installed", status: statusFail, message: "skipped (no parse)",
		})
	case len(installedEntries) == 0:
		checks = append(checks, doctorCheck{
			name: "heimdall hooks installed", status: statusWarn,
			message: "no entries found — run 'heimdall-mcp install-hooks'",
		})
	default:
		checks = append(checks, doctorCheck{
			name: "heimdall hooks installed", status: statusPass,
			message: fmt.Sprintf("%d entries", len(installedEntries)),
		})
	}

	// 4 — does the command's binary resolve on PATH?
	binary := "heimdall-mcp"
	resolvedBin, lookErr := deps.lookupPath(binary)
	if lookErr != nil {
		checks = append(checks, doctorCheck{
			name: "heimdall-mcp on PATH", status: statusFail,
			message: lookErr.Error(),
		})
	} else {
		checks = append(checks, doctorCheck{
			name: "heimdall-mcp on PATH", status: statusPass, message: resolvedBin,
		})
	}

	// 5 — does `heimdall-mcp --version` run cleanly?
	if lookErr != nil {
		checks = append(checks, doctorCheck{
			name: "heimdall-mcp --version", status: statusFail, message: "binary missing",
		})
	} else if vErr := deps.runVersion(resolvedBin); vErr != nil {
		checks = append(checks, doctorCheck{
			name: "heimdall-mcp --version", status: statusFail, message: vErr.Error(),
		})
	} else {
		checks = append(checks, doctorCheck{
			name: "heimdall-mcp --version", status: statusPass, message: "ok",
		})
	}

	// 6 — Ollama reachable?
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	pingErr := deps.pingOllama(ctx, cfg.OllamaEndpoint)
	cancel()
	if pingErr != nil {
		checks = append(checks, doctorCheck{
			name: "ollama reachable", status: statusWarn,
			message: pingErr.Error() + " (degraded but not fatal)",
		})
	} else {
		checks = append(checks, doctorCheck{
			name: "ollama reachable", status: statusPass, message: cfg.OllamaEndpoint,
		})
	}

	// 7 — configured model pulled?
	if pingErr != nil {
		checks = append(checks, doctorCheck{
			name: "model pulled", status: statusWarn, message: "ollama down — skipped",
		})
	} else {
		ctxL, cancelL := context.WithTimeout(context.Background(), 3*time.Second)
		models, listErr := deps.listModels(ctxL, cfg.OllamaEndpoint)
		cancelL()
		if listErr != nil {
			checks = append(checks, doctorCheck{
				name: "model pulled", status: statusWarn, message: listErr.Error(),
			})
		} else if !modelInList(models, cfg.Model) {
			checks = append(checks, doctorCheck{
				name: "model pulled", status: statusWarn,
				message: fmt.Sprintf("%s not pulled — run: ollama pull %s", cfg.Model, cfg.Model),
			})
		} else {
			checks = append(checks, doctorCheck{
				name: "model pulled", status: statusPass, message: cfg.Model,
			})
		}
	}

	// 8 — LLM classifier model pulled (plan 11 §5.5 F5 error event, deferred
	// from #57 to hooks doctor). Only relevant when the opt-in fallback is
	// actually armed: HEIMDALL_LLM_CLASSIFIER=1 AND cfg.LLMClassifierModel is
	// non-empty. If either gate is off, we skip with a clear explanation so
	// operators running `hooks doctor` on a default-off box don't see a
	// spurious warning. If Ollama itself is down (pingErr from check #6),
	// skip silently — the model-pulled check above already warned about that,
	// and a second warn on the same root cause would be noise.
	llmEnabled := env["HEIMDALL_LLM_CLASSIFIER"] == "1"
	llmModel := cfg.LLMClassifierModel
	switch {
	case !llmEnabled:
		checks = append(checks, doctorCheck{
			name: "llm classifier model", status: statusPass,
			message: "disabled (HEIMDALL_LLM_CLASSIFIER != 1)",
		})
	case llmModel == "":
		checks = append(checks, doctorCheck{
			name: "llm classifier model", status: statusWarn,
			message: "HEIMDALL_LLM_CLASSIFIER=1 but cfg.llmClassifierModel is empty — run: heimdall-mcp configure --llm-classifier-model=<model>",
		})
	case pingErr != nil:
		checks = append(checks, doctorCheck{
			name: "llm classifier model", status: statusWarn, message: "ollama down — skipped",
		})
	default:
		ctxL, cancelL := context.WithTimeout(context.Background(), 3*time.Second)
		models, listErr := deps.listModels(ctxL, cfg.OllamaEndpoint)
		cancelL()
		switch {
		case listErr != nil:
			checks = append(checks, doctorCheck{
				name: "llm classifier model", status: statusWarn, message: listErr.Error(),
			})
		case !modelInList(models, llmModel):
			// Plan 11 §5.5 F5 `llm.classifier.model_missing`: surface a clean
			// remediation hint before the hook ever fires. The hook itself
			// still logs `unreachable`/`bad_response` at call time, but this
			// proactive check catches the common "enabled the flag, forgot to
			// pull the model" misconfiguration.
			checks = append(checks, doctorCheck{
				name: "llm classifier model", status: statusWarn,
				message: fmt.Sprintf("%s not pulled — run: ollama pull %s", llmModel, llmModel),
			})
		default:
			checks = append(checks, doctorCheck{
				name: "llm classifier model", status: statusPass, message: llmModel,
			})
		}
	}

	// 9 — does the current project have an index?
	projectRoot := deps.projectRoot
	if projectRoot == "" {
		projectRoot = findProjectRoot(env)
	}
	dbBaseDir := ""
	if projectRoot != "" {
		dbBaseDir = filepath.Join(projectRoot, ".heimdall_db")
	}
	hasIndex := false
	if dbBaseDir != "" {
		if fi, err := os.Stat(dbBaseDir); err == nil && fi.IsDir() {
			hasIndex = len(heimdall.ListAvailableModels(dbBaseDir)) > 0
		}
	}
	switch {
	case projectRoot == "":
		checks = append(checks, doctorCheck{
			name: "project index present", status: statusWarn,
			message: "no project detected (run 'heimdall-mcp index <path>')",
		})
	case !hasIndex:
		checks = append(checks, doctorCheck{
			name: "project index present", status: statusWarn,
			message: "no .heimdall_db index — run 'heimdall-mcp index'",
		})
	default:
		checks = append(checks, doctorCheck{
			name: "project index present", status: statusPass, message: dbBaseDir,
		})
	}

	// 10 — index matches configured model?
	if hasIndex {
		modelDir := heimdall.ModelDBDir(dbBaseDir, cfg.Model)
		if _, err := os.Stat(modelDir); err == nil {
			checks = append(checks, doctorCheck{
				name: "index matches model", status: statusPass, message: cfg.Model,
			})
		} else {
			available := heimdall.ListAvailableModels(dbBaseDir)
			checks = append(checks, doctorCheck{
				name: "index matches model", status: statusFail,
				message: fmt.Sprintf("configured=%s have=%v", cfg.Model, available),
			})
		}
	} else {
		checks = append(checks, doctorCheck{
			name: "index matches model", status: statusWarn, message: "no index — skipped",
		})
	}

	// 11 — dry-fire each installed hook.
	if len(installedEntries) == 0 {
		checks = append(checks, doctorCheck{
			name: "hook dry-fire", status: statusWarn, message: "no hooks installed",
		})
	} else {
		failures := []string{}
		fired := 0
		for _, entry := range installedEntries {
			cmds := entryCommands(entry)
			for _, cmd := range cmds {
				if isInternalHookSubcommand(cmd) {
					continue
				}
				fired++
				if err := deps.dryFire(cmd); err != nil {
					failures = append(failures, cmd+": "+err.Error())
				}
			}
		}
		switch {
		case fired == 0:
			checks = append(checks, doctorCheck{
				name: "hook dry-fire", status: statusWarn, message: "no user-visible hooks to dry-fire",
			})
		case len(failures) > 0:
			checks = append(checks, doctorCheck{
				name: "hook dry-fire", status: statusFail,
				message: strings.Join(failures, "; "),
			})
		default:
			checks = append(checks, doctorCheck{
				name: "hook dry-fire", status: statusPass, message: fmt.Sprintf("%d ok", fired),
			})
		}
	}

	// 12 — hook log file writable?
	logPath := deps.hookLogPath
	if logPath == "" {
		logPath = heimdall.HookLogPath()
	}
	if logPath == "" {
		checks = append(checks, doctorCheck{
			name: "hook log writable", status: statusFail,
			message: "log path unresolved (set XDG_STATE_HOME or HOME)",
		})
	} else if err := canWrite(logPath); err != nil {
		checks = append(checks, doctorCheck{
			name: "hook log writable", status: statusFail, message: err.Error(),
		})
	} else {
		checks = append(checks, doctorCheck{
			name: "hook log writable", status: statusPass, message: logPath,
		})
	}

	// 13 — Claude Code skills sync rollup (informational; warn on drift).
	// Compares the count of SKILL.md files under the configured skills dir
	// against the count of memory rows whose ID matches the disk-skill prefix.
	// Drift means either: a skill file exists that has not been imported, or
	// a synced row's source file has been deleted. Either way, the user
	// should run `heimdall-mcp skills import` to reconcile.
	skillsDir := deps.skillsDir
	if skillsDir == "" {
		skillsDir = heimdall.ResolveClaudeSkillsDir("", env)
	}
	memDBPath := deps.memoryDBPath
	if memDBPath == "" {
		memDBPath = config.ResolveMemoryDBPath()
	}
	if skillsDir == "" {
		checks = append(checks, doctorCheck{
			name: "skills sync", status: statusWarn,
			message: "skills dir unresolved (set HEIMDALL_CLAUDE_SKILLS_DIR or HOME)",
		})
	} else {
		diskN, diskErr := heimdall.CountDiskSkillFiles(skillsDir)
		if diskErr != nil {
			checks = append(checks, doctorCheck{
				name: "skills sync", status: statusWarn,
				message: fmt.Sprintf("count error: %v", diskErr),
			})
		} else {
			memStore, openErr := heimdall.OpenMemoryStore(memDBPath)
			if openErr != nil {
				checks = append(checks, doctorCheck{
					name: "skills sync", status: statusWarn,
					message: fmt.Sprintf("memory store unavailable: %v", openErr),
				})
			} else {
				syncedN, _ := heimdall.CountSyncedSkillMemories(memStore)
				memStore.Close()
				switch {
				case diskN == 0:
					checks = append(checks, doctorCheck{
						name: "skills sync", status: statusPass,
						message: "0 disk skills (nothing to sync)",
					})
				case diskN == syncedN:
					checks = append(checks, doctorCheck{
						name: "skills sync", status: statusPass,
						message: fmt.Sprintf("%d/%d in sync", syncedN, diskN),
					})
				default:
					checks = append(checks, doctorCheck{
						name: "skills sync", status: statusWarn,
						message: fmt.Sprintf("%d/%d synced — run 'heimdall-mcp skills import'", syncedN, diskN),
					})
				}
			}
		}
	}

	// 14 — PreToolUse guardrail dry-fire (in-process). Runs a known-block
	// command (`rm -rf /`) through the exact same classifier the installed
	// hook uses; a ClassBlock return proves the rule table compiled and
	// the handler wiring is intact. (The generic hook dry-fire at check
	// #10 already exec's every installed entry, but the payload it sends
	// is event-agnostic — for PreToolUse that means `tool_name` is
	// missing and the handler exits silently without actually classifying.)
	//
	// Note: we used to fire `ls -la` here and expect ClassAllow, but after
	// the plan 11 §2.1 ClassUnknown split, unrecognized-but-safe commands
	// now return ClassUnknown (not ClassAllow). Firing a known-block
	// command is a stronger check anyway — it exercises the regex engine
	// against real patterns instead of just the default fall-through.
	{
		class, _, ruleID := heimdall.ClassifyBashCommand("rm -rf /")
		if class != heimdall.ClassBlock || ruleID != "RM_RF_ROOT" {
			checks = append(checks, doctorCheck{
				name: "guardrail classifier", status: statusFail,
				message: fmt.Sprintf("`rm -rf /` classified as %s (rule=%s); expected block/RM_RF_ROOT", class, ruleID),
			})
		} else {
			checks = append(checks, doctorCheck{
				name: "guardrail classifier", status: statusPass,
				message: fmt.Sprintf("%d rules loaded", heimdall.DestructiveRuleCount()),
			})
		}
	}

	// 15 — sessions report pipeline self-check. Reads hooks.log, folds it
	// into per-session aggregates, and confirms that `heimdall-mcp sessions
	// list` / `sessions report` can see at least one session. A missing or
	// empty log warns (system is fresh, nothing to report yet); a read
	// error fails. We deliberately do not try to render a full report —
	// that requires an actual Claude Code transcript on disk, which would
	// false-fail on any machine that hasn't run a session yet. See
	// docs/plans/hooks/10-per-session-savings-report.md §Open decisions #6.
	{
		entries, err := heimdall.ReadHookLog(heimdall.ReadHookLogOpts{Path: logPath})
		switch {
		case err != nil:
			checks = append(checks, doctorCheck{
				name: "sessions pipeline", status: statusFail,
				message: fmt.Sprintf("hooks.log unreadable: %v", err),
			})
		default:
			agg := heimdall.AggregateHookLogBySession(entries)
			n := len(agg)
			switch {
			case n == 0:
				checks = append(checks, doctorCheck{
					name: "sessions pipeline", status: statusWarn,
					message: "no sessions in hooks.log yet (expected on fresh install)",
				})
			default:
				checks = append(checks, doctorCheck{
					name: "sessions pipeline", status: statusPass,
					message: fmt.Sprintf("%d session(s) available for `sessions list`", n),
				})
			}
		}
	}

	return checks
}

// fillDoctorDeps hydrates any nil function fields in deps with their
// production defaults. Test callers can leave individual fields nil to use
// the real impl or set them explicitly to inject a fake.
func fillDoctorDeps(cfg config.Config, deps doctorDeps) doctorDeps {
	if deps.lookupPath == nil {
		deps.lookupPath = exec.LookPath
	}
	if deps.runVersion == nil {
		deps.runVersion = func(bin string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin, "--version")
			return cmd.Run()
		}
	}
	if deps.pingOllama == nil {
		deps.pingOllama = func(ctx context.Context, endpoint string) error {
			c := heimdall.NewOllamaClient(endpoint)
			return c.Ping(ctx)
		}
	}
	if deps.listModels == nil {
		deps.listModels = func(ctx context.Context, endpoint string) ([]string, error) {
			c := heimdall.NewOllamaClient(endpoint)
			models, err := c.ListModels(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]string, 0, len(models))
			for _, m := range models {
				out = append(out, m.Name)
			}
			return out, nil
		}
	}
	if deps.dryFire == nil {
		deps.dryFire = func(command string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			parts := strings.Fields(command)
			if len(parts) == 0 {
				return errors.New("empty command")
			}
			cmd := exec.CommandContext(ctx, parts[0], parts[1:]...)
			payload := deps.dryFireStdin
			if len(payload) == 0 {
				payload = []byte(`{"event":"doctor-dry-fire"}`)
			}
			cmd.Stdin = strings.NewReader(string(payload))
			return cmd.Run()
		}
	}
	_ = cfg
	return deps
}

// collectHeimdallEntries walks `settings.hooks.<event>` arrays and returns
// every entry that satisfies isHeimdallEntry.
func collectHeimdallEntries(settings map[string]any) []map[string]any {
	hooksAny, ok := settings["hooks"]
	if !ok || hooksAny == nil {
		return nil
	}
	hooksMap, ok := hooksAny.(map[string]any)
	if !ok {
		return nil
	}
	out := []map[string]any{}
	for _, list := range hooksMap {
		arr, ok := list.([]any)
		if !ok {
			continue
		}
		for _, raw := range arr {
			obj, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if isHeimdallEntry(obj) {
				out = append(out, obj)
			}
		}
	}
	return out
}

// entryCommands returns every `hooks[].command` string on a hook entry.
func entryCommands(entry map[string]any) []string {
	raw, ok := entry["hooks"].([]any)
	if !ok {
		return nil
	}
	out := []string{}
	for _, h := range raw {
		hmap, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if cmd, _ := hmap["command"].(string); cmd != "" {
			out = append(out, cmd)
		}
	}
	return out
}

// isInternalHookSubcommand returns true for hook subcommands that are not
// user-visible (e.g. Stream E's `hook post-edit-actor`). Doctor skips these
// when dry-firing because a user wouldn't have installed them directly.
func isInternalHookSubcommand(command string) bool {
	return strings.Contains(command, " hook post-edit-actor")
}

// modelInList returns true if `name` matches any item in `models`, accounting
// for Ollama's implicit `:latest` tag.
func modelInList(models []string, name string) bool {
	want := heimdall.NormalizeModelName(name)
	for _, m := range models {
		if heimdall.NormalizeModelName(m) == want {
			return true
		}
	}
	return false
}

// canWrite verifies that we can append to the given log file path without
// actually mutating its contents. Creates parent dirs if missing, then opens
// the file in append mode and immediately closes it.
func canWrite(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}
