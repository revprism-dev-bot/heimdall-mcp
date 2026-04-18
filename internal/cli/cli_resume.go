package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// resumeDecision is the outcome of consulting an on-disk resume marker.
// cliIndex routes the run based on this value — either skipping model
// selection (continue), falling through to normal flow (fresh/restart),
// or exiting (cancel).
type resumeDecision int

const (
	// resumeProceedFresh means "no marker, do the normal flow" OR
	// "marker existed but the user chose Restart (we've deleted it)".
	// Either way, the caller runs model selection from scratch.
	resumeProceedFresh resumeDecision = iota
	// resumeContinue means "skip model selection, use the models
	// recorded in the marker". The marker is preserved (it's deleted
	// on clean completion, not at prompt time).
	resumeContinue
	// resumeRestart means "user chose Restart; we've deleted the
	// marker; caller should go through model selection normally".
	// Distinct from resumeProceedFresh for observability (the CLI
	// emits a different informational line before continuing).
	resumeRestart
	// resumeCancel means "user chose Cancel (or gave an unrecognised
	// answer — safer default)". The caller should print a brief
	// message and exit(0). The marker is preserved so a later run can
	// pick up.
	resumeCancel
)

// resolveResume checks for an in-progress marker under baseDir and, if
// found, prompts the user (via stdin) to Continue/Restart/Cancel.
//
// stdin / stdout must be provided so tests can exercise every branch
// without touching os.Stdin / os.Stdout. isTTY is passed in rather than
// sniffed inside so callers can choose their own detection mechanism;
// production cliIndex uses github.com/mattn/go-isatty on os.Stdin, tests
// pass booleans.
//
// When isTTY is false and a marker exists, the function auto-chooses
// Continue — a non-interactive caller (CI, post-commit hook, backgrounded
// run) has nobody to answer the prompt, and blocking forever would be
// worse than just finishing the interrupted work. A notice line is
// printed so observers see the auto-decision.
func resolveResume(baseDir, path string, stdin io.Reader, stdout io.Writer, isTTY bool) (resumeDecision, []string, error) {
	marker, err := heimdall.ReadResumeMarker(baseDir)
	if err != nil {
		// Corrupt marker: surface the error but don't block the run.
		// Caller will treat this the same as no marker.
		fmt.Fprintf(stdout, "Warning: unreadable resume marker at %s: %v (ignoring)\n",
			heimdall.ResumeMarkerPath(baseDir), err)
		_ = heimdall.DeleteResumeMarker(baseDir)
		return resumeProceedFresh, nil, nil
	}
	if marker == nil {
		return resumeProceedFresh, nil, nil
	}

	// Shared header: tell the user what's up regardless of TTY mode.
	fmt.Fprintf(stdout, "\nPrevious indexing of %s with models %v was interrupted on %s.\n",
		path, marker.Models, marker.StartTime.Format("2006-01-02 15:04:05 UTC"))

	if !isTTY {
		fmt.Fprintln(stdout, "stdin is non-TTY — automatically continuing with the recorded models.")
		return resumeContinue, append([]string{}, marker.Models...), nil
	}

	fmt.Fprint(stdout, "Continue with same models [C], restart with new model selection [R], or cancel [Q]? [C/R/Q]: ")

	reader := bufio.NewReader(stdin)
	line, readErr := reader.ReadString('\n')
	// ReadString returns io.EOF when stdin closes without a trailing
	// newline. That's a normal, terminating case — treat it as "user
	// pressed Enter with no content" and fall to the unknown-input
	// Cancel branch. A genuine IO error on stdin is also safest-as-Cancel.
	_ = readErr
	answer := strings.ToUpper(strings.TrimSpace(line))
	switch answer {
	case "C":
		return resumeContinue, append([]string{}, marker.Models...), nil
	case "R":
		if err := heimdall.DeleteResumeMarker(baseDir); err != nil {
			return resumeProceedFresh, nil, fmt.Errorf("delete marker on restart: %w", err)
		}
		return resumeRestart, nil, nil
	case "Q":
		return resumeCancel, nil, nil
	default:
		// Unknown answer is treated as Cancel — refusing to guess is
		// safer than silently wiping the user's pinned model list.
		return resumeCancel, nil, nil
	}
}

// isStdinTTY reports whether os.Stdin is attached to a TTY. Kept as a
// package-level helper so cliIndex calls it directly with no extra
// plumbing — tests call resolveResume with explicit isTTY bools.
func isStdinTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	// A character device with no Setgid/Setuid bits is our proxy for a
	// TTY. Uses the standard library so we don't add a new dependency.
	return (fi.Mode() & os.ModeCharDevice) != 0
}
