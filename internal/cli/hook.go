package cli

import (
	"fmt"
	"io"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// DispatchHook routes `heimdall-mcp hook <event>` retrieval calls.
//
// Separate from DispatchHooks (plural), which owns the admin surface
// (`heimdall-mcp hooks tail|cache-clear|cache-stats`). The split keeps the
// foreground retrieval path allocation-free of the admin flag machinery.
//
// All retrieval hooks exit 0 always — per OQ-5 locked in docs/plans/hooks/
// 06-decisions.md, a retrieval hook must never block Claude Code. Errors
// are captured via heimdall.LogHookEvent, never surfaced as exit codes or
// stderr.
func DispatchHook(cfg config.Config, stdin io.Reader, stdout, stderr io.Writer, env map[string]string, args []string) int {
	if len(args) == 0 {
		// No subcommand is still "exit 0" — logging the usage error to
		// stderr would violate the hook contract, and the caller is
		// Claude Code (which never reads a usage message). Log and go.
		heimdall.LogHookEvent("WARN", "hook", map[string]any{"err": "no_subcommand"})
		return 0
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "post-edit":
		return HookPostEdit(cfg, stdin, stdout, stderr, env, rest)
	case "post-edit-actor":
		// Hidden internal shim invoked by the fork+setsid path. Not part
		// of the user-facing surface — `hooks doctor` must skip this.
		return HookPostEditActor(cfg, stdin, stdout, stderr, env, rest)
	case "-h", "--help", "help":
		// Keep help on stdout — this is only reachable from an
		// interactive shell, not from Claude Code.
		fmt.Fprintln(stdout, "heimdall-mcp hook — Claude Code retrieval hooks")
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "  hook post-edit [--source=heimdall --version=1]")
		fmt.Fprintln(stdout, "                                Enqueue an edited file for incremental reindex")
		return 0
	default:
		heimdall.LogHookEvent("WARN", "hook", map[string]any{"err": "unknown_subcommand", "sub": sub})
		return 0
	}
}
