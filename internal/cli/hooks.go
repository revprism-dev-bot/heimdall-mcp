package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// HookHandler is the testable shape every hook CLI command handler
// implements, per consolidated-plan §5.4. Handlers MUST NOT call os.Exit;
// they return an int exit code that main() translates.
type HookHandler func(stdin io.Reader, stdout, stderr io.Writer, env map[string]string, args []string) int

// DispatchHooks routes `heimdall-mcp hooks <subcommand>` calls. Returns the
// exit code to surface to the shell. Called by RunCLI via a tiny wrapper.
func DispatchHooks(cfg config.Config, stdin io.Reader, stdout, stderr io.Writer, env map[string]string, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage: heimdall-mcp hooks <tail|cache-clear|cache-stats|doctor|explain-command>")
		return 2
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "tail":
		return HooksTail(stdin, stdout, stderr, env, rest)
	case "cache-clear":
		return HooksCacheClear(cfg, stdin, stdout, stderr, env, rest)
	case "cache-stats":
		return HooksCacheStats(cfg, stdin, stdout, stderr, env, rest)
	case "doctor":
		return HooksDoctor(cfg, stdin, stdout, stderr, env, rest)
	case "explain-command":
		return HooksExplainCommand(stdin, stdout, stderr, env, rest)
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, "heimdall-mcp hooks — operate on the Claude Code hooks subsystem")
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "  hooks tail [flags]                     Tail the hook log file with filters")
		fmt.Fprintln(stdout, "  hooks cache-clear [--project <path>] [--all-models]")
		fmt.Fprintln(stdout, "  hooks cache-stats [--project <path>] [--format text|json] [--all-models]")
		fmt.Fprintln(stdout, "  hooks doctor [--scope=user|project]    Diagnose hooks installation")
		fmt.Fprintln(stdout, "  hooks explain-command \"<cmd>\"          Classify a Bash command (allow/warn/block)")
		return 0
	default:
		fmt.Fprintf(stderr, "Unknown hooks subcommand: %s\n", sub)
		return 2
	}
}

// HooksExplainCommand is the `hooks explain-command "<cmd>"` admin entry.
// Classifies the given Bash command without executing anything and prints
// a one-line summary `class=<allow|warn|block>  rule=<id>  reason=<text>`.
//
// Exit code: 0 unconditionally. This command is purely informational — a
// shell script wanting to gate on classification should parse the printed
// `class=` token itself. Per design doc §4 this is the interactive dry-run
// surface; the hot-path hook lives in HookPreToolUse.
func HooksExplainCommand(stdin io.Reader, stdout, stderr io.Writer, env map[string]string, args []string) int {
	_ = stdin
	_ = env
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage: heimdall-mcp hooks explain-command \"<bash command>\"")
		// Still return 0 — this command is informational; a missing arg
		// just prints usage and moves on. (Matches the task spec: "Exits 0
		// regardless.")
		return 0
	}
	// Join all positional args so users can write the command without quoting
	// each whitespace-separated token. `hooks explain-command rm -rf /` works
	// the same as `hooks explain-command "rm -rf /"`.
	cmd := strings.Join(args, " ")
	class, reason, ruleID := heimdall.ClassifyBashCommand(cmd)
	ruleOut := ruleID
	if ruleOut == "" {
		ruleOut = "-"
	}
	reasonOut := reason
	if reasonOut == "" {
		reasonOut = "-"
	}
	fmt.Fprintf(stdout, "class=%s  rule=%s  reason=%s\n", class.String(), ruleOut, reasonOut)
	return 0
}

// ----- T17: hooks tail -----

type tailFlags struct {
	level    string
	event    string
	since    time.Duration
	project  string
	follow   bool
	hasSince bool
}

// HooksTail implements `heimdall-mcp hooks tail`.
//
// Filters (AND-combined): --level, --event, --since, --project. Default is
// one-shot (print current log and exit). --follow switches to polling-tail.
//
// Exit codes:
//
//	0 — normal (including empty log with no matches)
//	1 — log file not found or unreadable
//	2 — bad flag or argument
func HooksTail(stdin io.Reader, stdout, stderr io.Writer, env map[string]string, args []string) int {
	flags, err := parseTailFlags(args)
	if err != nil {
		fmt.Fprintf(stderr, "hooks tail: %v\n", err)
		return 2
	}

	rc, err := heimdall.OpenHookLog(0, flags.follow)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Empty log — one-shot returns 0 (no content). Follow mode
			// still errors, because there's nothing to watch.
			if !flags.follow {
				return 0
			}
		}
		// Never leak the absolute log path into stderr — it's user data.
		fmt.Fprintln(stderr, "hooks tail: log unavailable")
		return 1
	}
	defer rc.Close()

	now := time.Now()
	var sinceThreshold time.Time
	if flags.hasSince {
		sinceThreshold = now.Add(-flags.since)
	}

	scanner := bufio.NewScanner(rc)
	// Hook log lines are short but allow 64 KB to be safe.
	scanner.Buffer(make([]byte, 0, 8192), 64*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !tailMatches(line, flags, sinceThreshold) {
			continue
		}
		if _, werr := fmt.Fprintln(stdout, line); werr != nil {
			return 0 // broken pipe — normal exit
		}
	}
	if err := scanner.Err(); err != nil {
		// Partial read errors don't prevent a successful return of what we
		// managed to print — but we do surface it for visibility.
		fmt.Fprintln(stderr, "hooks tail: read error")
	}
	return 0
}

func parseTailFlags(args []string) (tailFlags, error) {
	f := tailFlags{}
	// Normalize `--flag=value` into `--flag`, `value` so both forms work.
	args = splitEqualsFlags(args)
	i := 0
	needs := func(flag string) (string, error) {
		i++
		if i >= len(args) {
			return "", fmt.Errorf("%s requires a value", flag)
		}
		return args[i], nil
	}
	for ; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--level":
			v, err := needs("--level")
			if err != nil {
				return f, err
			}
			v = strings.ToUpper(v)
			switch v {
			case "DEBUG", "INFO", "WARN", "ERROR":
				f.level = v
			default:
				return f, fmt.Errorf("invalid --level %q (want DEBUG|INFO|WARN|ERROR)", v)
			}
		case a == "--event":
			v, err := needs("--event")
			if err != nil {
				return f, err
			}
			f.event = v
		case a == "--since":
			v, err := needs("--since")
			if err != nil {
				return f, err
			}
			d, perr := parseTailDuration(v)
			if perr != nil {
				return f, fmt.Errorf("invalid --since %q: %w", v, perr)
			}
			f.since = d
			f.hasSince = true
		case a == "--project":
			v, err := needs("--project")
			if err != nil {
				return f, err
			}
			f.project = v
		case a == "--follow" || a == "-f":
			f.follow = true
		case a == "--help" || a == "-h":
			return f, errors.New("usage: hooks tail [--level] [--event] [--since 5m] [--project <name>] [--follow]")
		default:
			return f, fmt.Errorf("unknown flag %q", a)
		}
	}
	return f, nil
}

// splitEqualsFlags rewrites `--flag=value` tokens into separate `--flag` and
// `value` tokens so a single switch on exact flag names can match both forms.
// Non-flag tokens and bare `--flag` tokens pass through untouched.
func splitEqualsFlags(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if len(a) > 2 && strings.HasPrefix(a, "--") {
			if idx := strings.IndexByte(a, '='); idx > 0 {
				out = append(out, a[:idx], a[idx+1:])
				continue
			}
		}
		out = append(out, a)
	}
	return out
}

// parseTailDuration accepts Go durations plus shorthand "7d" = 168h.
func parseTailDuration(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return 0, err
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// tailMatches applies the filter set to one raw log line.
func tailMatches(line string, f tailFlags, sinceThreshold time.Time) bool {
	if line == "" {
		return false
	}
	// Log line shape: "<RFC3339> LEVEL event=<name> k=v ..."
	// Cheap split — we only need the first two whitespace-separated tokens
	// plus a substring for kv filters.
	sp1 := strings.IndexByte(line, ' ')
	if sp1 < 0 {
		return false
	}
	tsPart := line[:sp1]
	rest := line[sp1+1:]

	if f.hasSince {
		ts, err := time.Parse(time.RFC3339, tsPart)
		if err != nil {
			return false
		}
		if ts.Before(sinceThreshold) {
			return false
		}
	}

	sp2 := strings.IndexByte(rest, ' ')
	if sp2 < 0 {
		// Only timestamp+level, no kv — acceptable if no kv filter is set.
		if f.level != "" && rest != f.level {
			return false
		}
		return f.event == "" && f.project == ""
	}
	lvl := rest[:sp2]
	if f.level != "" && lvl != f.level {
		return false
	}
	tail := rest[sp2+1:]
	if f.event != "" && !strings.Contains(tail, "event="+f.event) {
		return false
	}
	if f.project != "" && !strings.Contains(tail, "project="+f.project) {
		return false
	}
	return true
}

// ----- T18: hooks cache-clear / hooks cache-stats -----

type cacheFlags struct {
	project   string
	format    string
	allModels bool
}

func parseCacheFlags(args []string, wantFormat bool) (cacheFlags, error) {
	f := cacheFlags{format: "text"}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--project":
			if i+1 >= len(args) {
				return f, errors.New("--project requires a value")
			}
			i++
			f.project = args[i]
		case "--format":
			if !wantFormat {
				return f, fmt.Errorf("unknown flag %q", a)
			}
			if i+1 >= len(args) {
				return f, errors.New("--format requires a value")
			}
			i++
			switch args[i] {
			case "text", "json":
				f.format = args[i]
			default:
				return f, fmt.Errorf("invalid --format %q (want text|json)", args[i])
			}
		case "--all-models":
			f.allModels = true
		case "--help", "-h":
			return f, errors.New("usage: hooks cache-clear|cache-stats [--project <path>] [--format text|json] [--all-models]")
		default:
			return f, fmt.Errorf("unknown flag %q", a)
		}
	}
	return f, nil
}

func resolveCacheProject(flagProject string) string {
	if flagProject != "" {
		abs, err := filepath.Abs(flagProject)
		if err == nil {
			return abs
		}
		return flagProject
	}
	cwd, _ := os.Getwd()
	return cwd
}

// HooksCacheClear implements `heimdall-mcp hooks cache-clear`.
//
// Stream B confirmed the final signatures (commit a565a16 on
// feat/wave1-stream-b-retrieval-infra, file internal/heimdall/hook_cache.go):
//
//	func (s *VectorStore) HookCacheClear() error
//	func (s *VectorStore) HookCacheStats() (count, totalBytes, hitCount int64, err error)
//
// Because Stream B's hook_cache.go is not yet on main when this branch was
// cut, calling these methods would not compile here. Task #10 (Wave 1
// close-out) is the merge-time swap-in — the wiring below is the exact
// shape the swap-in needs, with the two real calls replacing the stub
// payload. Nothing else on this handler needs to change.
func HooksCacheClear(cfg config.Config, stdin io.Reader, stdout, stderr io.Writer, env map[string]string, args []string) int {
	flags, err := parseCacheFlags(args, false)
	if err != nil {
		fmt.Fprintf(stderr, "hooks cache-clear: %v\n", err)
		return 2
	}
	project := resolveCacheProject(flags.project)
	baseDir := filepath.Join(project, ".heimdall_db")
	if _, err := os.Stat(baseDir); err != nil {
		// No DB yet — nothing to clear. Still emit a stable JSON payload so
		// callers (tests, scripts) can parse our output unconditionally.
		writeJSON(stdout, map[string]any{"cleared": 0, "reason": "no_db"})
		return 0
	}

	modelDirs := selectCacheModelDirs(baseDir, cfg.Model, flags.allModels)
	var totalCleared int64
	for _, modelDir := range modelDirs {
		store, err := heimdall.OpenStore(modelDir)
		if err != nil {
			fmt.Fprintln(stderr, "hooks cache-clear: store error")
			return 1
		}
		count, _, _, statsErr := store.HookCacheStats()
		if statsErr != nil {
			store.Close()
			fmt.Fprintln(stderr, "hooks cache-clear: stats error")
			return 1
		}
		if clearErr := store.HookCacheClear(); clearErr != nil {
			store.Close()
			fmt.Fprintln(stderr, "hooks cache-clear: clear error")
			return 1
		}
		store.Close()
		totalCleared += count
	}
	writeJSON(stdout, map[string]any{"cleared": totalCleared})
	return 0
}

// selectCacheModelDirs returns the list of per-model DB directories the cache
// admin commands should operate over. In allModels mode it walks every model
// subdir under baseDir; otherwise it returns just the configured model's dir.
// Empty result means "no model index present" and callers should emit the
// equivalent of a no-op success rather than an error.
func selectCacheModelDirs(baseDir, configuredModel string, allModels bool) []string {
	if allModels {
		available := heimdall.ListAvailableModels(baseDir)
		dirs := make([]string, 0, len(available))
		for _, name := range available {
			dirs = append(dirs, filepath.Join(baseDir, name))
		}
		return dirs
	}
	modelDir := heimdall.ModelDBDir(baseDir, configuredModel)
	if _, err := os.Stat(modelDir); err != nil {
		return nil
	}
	return []string{modelDir}
}

// HooksCacheStats implements `heimdall-mcp hooks cache-stats`.
//
// Same Stream B dependency as cache-clear. The payload keys below
// (`entries`, `bytes`, `hit_count`) match Stream B's three-scalar return
// shape from HookCacheStats one-to-one — swap-in is a straight assignment.
func HooksCacheStats(cfg config.Config, stdin io.Reader, stdout, stderr io.Writer, env map[string]string, args []string) int {
	flags, err := parseCacheFlags(args, true)
	if err != nil {
		fmt.Fprintf(stderr, "hooks cache-stats: %v\n", err)
		return 2
	}
	project := resolveCacheProject(flags.project)
	baseDir := filepath.Join(project, ".heimdall_db")
	payload := map[string]any{
		"entries":   int64(0),
		"bytes":     int64(0),
		"hit_count": int64(0),
	}
	if _, err := os.Stat(baseDir); err != nil {
		payload["reason"] = "no_db"
	} else {
		modelDirs := selectCacheModelDirs(baseDir, cfg.Model, flags.allModels)
		if len(modelDirs) == 0 {
			payload["reason"] = "no_model_index"
		} else {
			var entries, bytes, hits int64
			for _, modelDir := range modelDirs {
				store, err := heimdall.OpenStore(modelDir)
				if err != nil {
					fmt.Fprintln(stderr, "hooks cache-stats: store error")
					return 1
				}
				count, totalBytes, hitCount, statsErr := store.HookCacheStats()
				store.Close()
				if statsErr != nil {
					fmt.Fprintln(stderr, "hooks cache-stats: stats error")
					return 1
				}
				entries += count
				bytes += totalBytes
				hits += hitCount
			}
			payload["entries"] = entries
			payload["bytes"] = bytes
			payload["hit_count"] = hits
		}
	}

	if flags.format == "json" {
		writeJSON(stdout, payload)
		return 0
	}
	// Text format: two-column key/value.
	fmt.Fprintf(stdout, "entries  %v\n", payload["entries"])
	fmt.Fprintf(stdout, "bytes    %v\n", payload["bytes"])
	fmt.Fprintf(stdout, "hits     %v\n", payload["hit_count"])
	if r, ok := payload["reason"]; ok {
		fmt.Fprintf(stdout, "reason   %v\n", r)
	}
	return 0
}

func writeJSON(w io.Writer, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		// Fall back to a bare, compact error — never crash the handler.
		_, _ = io.WriteString(w, `{"error":"marshal"}`+"\n")
		return
	}
	b = append(b, '\n')
	_, _ = w.Write(b)
}

// Compile-time assertion: HooksTail matches the HookHandler shape so that
// main() and future wiring in Wave 2 can treat all handlers uniformly.
var _ HookHandler = HooksTail

// envMap snapshots the current process environment into the map shape that
// every hook handler takes. Isolated into a helper so tests can inject a
// fake environment without touching os.Environ.
func envMap() map[string]string {
	e := os.Environ()
	m := make(map[string]string, len(e))
	for _, kv := range e {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		m[kv[:eq]] = kv[eq+1:]
	}
	return m
}
