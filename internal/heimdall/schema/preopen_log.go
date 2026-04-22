package schema

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// preOpenLogName is the file that records which KindPreOpen migrations
// have already fired for this baseDir/model pair. It lives alongside the
// model's DB dir (under baseDir) so it's easy to locate and doesn't
// require reaching inside the model dir itself.
//
// Format: one integer per line. Whitespace and blank lines are tolerated.
// Duplicates are OK — set semantics. A missing file means "nothing has run
// yet". We intentionally do NOT use an atomic write helper here; the file
// is append-only and a torn write just causes one extra idempotent re-run.
func preOpenLogName(baseDir, model string) string {
	if baseDir == "" {
		return ""
	}
	// Keep the file co-located with `baseDir` and scoped by model name, so
	// two models in the same `.heimdall_db/` don't share state. Sanitize
	// the model to the same rules used for model dir names (see
	// heimdall/dbpath.go:sanitizeModelDirName) — we duplicate the rule
	// locally rather than importing the parent package to avoid an import
	// cycle. Keep the behaviour in sync via the test at
	// TestPreOpenLogName_MatchesDBDirSanitization.
	safe := strings.ReplaceAll(model, ":", "_")
	safe = strings.ReplaceAll(safe, "/", "_")
	if safe == "" {
		safe = "__nomodel__"
	}
	return filepath.Join(baseDir, ".heimdall-schema-preopen-"+safe+".log")
}

// readPreOpenLog returns the set of version numbers recorded in the
// pre-open log for this baseDir+model. Missing file → empty set; malformed
// file → empty set with a warning (treated as "nothing has run" so
// migrations re-fire idempotently).
func readPreOpenLog(baseDir, model string) (map[int]bool, error) {
	path := preOpenLogName(baseDir, model)
	if path == "" {
		return map[int]bool{}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[int]bool{}, nil
		}
		return nil, err
	}
	defer f.Close()

	out := map[int]bool{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		v, err := strconv.Atoi(line)
		if err != nil {
			// Tolerate garbage lines. Treat as "not yet run" for this
			// version — safe because migrations are idempotent.
			continue
		}
		out[v] = true
	}
	return out, scanner.Err()
}

// writePreOpenLog appends one version number to the pre-open log,
// creating the file + parent dir as needed. Idempotent: a duplicate append
// just grows the file by a line, and readPreOpenLog dedupes on load.
func writePreOpenLog(baseDir, model string, version int) error {
	path := preOpenLogName(baseDir, model)
	if path == "" {
		return fmt.Errorf("schema: empty baseDir; cannot record pre-open version")
	}
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "%d\n", version); err != nil {
		return err
	}
	return nil
}
