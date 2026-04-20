package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// HooksAnalyzeSession implements `heimdall-mcp hooks analyze-session`.
//
// Reads a JSONL transcript, runs AnalyzeTranscript, and renders a human
// summary to stdout. Also writes a structured JSON blob to hooks.log via
// heimdall.LogHookEvent (event=mcp.compliance) when HEIMDALL_HOOK_LOG is set.
//
// Flags:
//   - --transcript <path>  (required) path to the JSONL transcript file.
//   - --format <text|json> (default text) output format.
//
// Exit codes:
//
//	0 — success (including zero-miss case)
//	1 — transcript file cannot be read or flag error
func HooksAnalyzeSession(cfg config.Config, stdin io.Reader, stdout, stderr io.Writer, env map[string]string, args []string) int {
	_ = cfg
	_ = stdin
	_ = env

	var transcriptPath, format string
	format = "text"

	// Simple flag parsing (no flag package — keep it lightweight per plan §7.5).
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--transcript":
			i++
			if i >= len(args) {
				fmt.Fprintln(stderr, "hooks analyze-session: --transcript requires a value")
				return 1
			}
			transcriptPath = args[i]
		case "--format":
			i++
			if i >= len(args) {
				fmt.Fprintln(stderr, "hooks analyze-session: --format requires a value")
				return 1
			}
			switch args[i] {
			case "text", "json":
				format = args[i]
			default:
				fmt.Fprintf(stderr, "hooks analyze-session: invalid --format %q (want text|json)\n", args[i])
				return 1
			}
		case "-h", "--help":
			fmt.Fprintln(stdout, "Usage: heimdall-mcp hooks analyze-session --transcript <path> [--format text|json]")
			return 0
		default:
			fmt.Fprintf(stderr, "hooks analyze-session: unknown flag %q\n", args[i])
			return 1
		}
	}

	if transcriptPath == "" {
		fmt.Fprintln(stderr, "hooks analyze-session: --transcript is required")
		return 1
	}

	data, err := os.ReadFile(transcriptPath)
	if err != nil {
		fmt.Fprintf(stderr, "hooks analyze-session: cannot read transcript: %v\n", err)
		return 1
	}

	result := AnalyzeTranscript(data)

	// Write structured JSON to hooks.log when HEIMDALL_HOOK_LOG is set.
	// The check is implicit: heimdall.LogHookEvent is a no-op when the
	// env var is unset, so this is always safe to call.
	missEntries := make([]map[string]any, 0, len(result.Misses))
	for _, m := range result.Misses {
		missEntries = append(missEntries, map[string]any{
			"rule":          m.Rule,
			"expected_tool": m.ExpectedTool,
			"trigger":       m.TriggerExcerpt,
			"turn":          m.TurnIndex,
		})
	}
	heimdall.LogHookEvent("INFO", "mcp.compliance", map[string]any{
		"transcript": transcriptPath,
		"triggers":   result.Triggers,
		"followed":   result.Followed,
		"misses":     missEntries,
	})

	if format == "json" {
		writeJSON(stdout, map[string]any{
			"transcript": transcriptPath,
			"triggers":   result.Triggers,
			"followed":   result.Followed,
			"misses":     missEntries,
		})
		return 0
	}

	// Text format.
	fmt.Fprintf(stdout, "Transcript: %s\n", transcriptPath)
	fmt.Fprintf(stdout, "Triggers: %d rules fired\n", result.Triggers)
	if result.Triggers > 0 {
		pct := 0
		if result.Triggers > 0 {
			pct = result.Followed * 100 / result.Triggers
		}
		fmt.Fprintf(stdout, "Followed: %d (%d%%)\n", result.Followed, pct)
	} else {
		fmt.Fprintf(stdout, "Followed: 0\n")
	}

	if len(result.Misses) == 0 {
		fmt.Fprintln(stdout, "Misses: none")
		return 0
	}

	fmt.Fprintln(stdout, "Misses:")
	for _, m := range result.Misses {
		fmt.Fprintf(stdout, "  - %s -> %s (turn %d)\n", m.Rule, m.ExpectedTool, m.TurnIndex)
		if m.TriggerExcerpt != "" {
			fmt.Fprintf(stdout, "    %q\n", m.TriggerExcerpt)
		}
	}

	return 0
}
