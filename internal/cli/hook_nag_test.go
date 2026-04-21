package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

//  1. Pure decision: first turn, no direct calls, threshold=5 — counter goes
//     0→1, no emit.
func TestEvaluateNag_FirstTurnBelowThreshold(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	d := evaluateNag(nagState{}, 5, now, false)

	if d.Emit {
		t.Errorf("Emit = true on turn 1 with threshold 5; want false")
	}
	if d.State.Turn != 1 {
		t.Errorf("Turn = %d, want 1", d.State.Turn)
	}
	if d.State.LastNagTurn != 0 {
		t.Errorf("LastNagTurn = %d, want 0", d.State.LastNagTurn)
	}
	if d.State.LastResetUnix != now.Unix() {
		t.Errorf("LastResetUnix = %d, want %d (initialized on first call)", d.State.LastResetUnix, now.Unix())
	}
}

//  2. Pure decision: turn N+1 (with N=5) AND no direct calls → emit + record
//     the nag turn. The next prompt would not re-fire until another N pass.
func TestEvaluateNag_FiresAtThreshold(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	prev := nagState{Turn: 4, LastNagTurn: 0, LastResetUnix: now.Unix() - 600}
	d := evaluateNag(prev, 5, now, false)

	if !d.Emit {
		t.Fatalf("Emit = false at turn 5/threshold 5; want true")
	}
	if d.State.Turn != 5 {
		t.Errorf("Turn = %d, want 5", d.State.Turn)
	}
	if d.State.LastNagTurn != 5 {
		t.Errorf("LastNagTurn = %d, want 5", d.State.LastNagTurn)
	}
}

//  3. Pure decision: after firing, the next turn (turn 6) must NOT emit again
//     — it should wait another full N turns. This is the regression that
//     keeps the nag from being annoying.
func TestEvaluateNag_DoesNotReFireImmediatelyAfterEmit(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	prev := nagState{Turn: 5, LastNagTurn: 5, LastResetUnix: now.Unix() - 1200}
	d := evaluateNag(prev, 5, now, false)

	if d.Emit {
		t.Errorf("Emit = true on turn 6 with LastNagTurn=5 threshold=5; want false (must wait until turn 10)")
	}
	if d.State.Turn != 6 {
		t.Errorf("Turn = %d, want 6", d.State.Turn)
	}
	if d.State.LastNagTurn != 5 {
		t.Errorf("LastNagTurn = %d, want 5 (unchanged)", d.State.LastNagTurn)
	}
}

//  4. Pure decision: a direct call resets the counter, even if we were just
//     about to fire.
func TestEvaluateNag_DirectCallResetsCounter(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	prev := nagState{Turn: 4, LastNagTurn: 0, LastResetUnix: now.Unix() - 600}
	d := evaluateNag(prev, 5, now, true)

	if d.Emit {
		t.Errorf("Emit = true after direct call; want false")
	}
	if d.State.Turn != 0 {
		t.Errorf("Turn = %d, want 0 (reset)", d.State.Turn)
	}
	if d.State.LastNagTurn != 0 {
		t.Errorf("LastNagTurn = %d, want 0 (reset)", d.State.LastNagTurn)
	}
	if d.State.LastResetUnix != now.Unix() {
		t.Errorf("LastResetUnix = %d, want %d (advanced)", d.State.LastResetUnix, now.Unix())
	}
}

// 5. Threshold env override: HEIMDALL_NAG_TURNS=2 takes effect.
func TestNagThresholdFromEnv(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want int
	}{
		{"unset", map[string]string{}, nagDefaultTurns},
		{"explicit", map[string]string{"HEIMDALL_NAG_TURNS": "2"}, 2},
		{"empty string", map[string]string{"HEIMDALL_NAG_TURNS": ""}, nagDefaultTurns},
		{"non-int", map[string]string{"HEIMDALL_NAG_TURNS": "abc"}, nagDefaultTurns},
		{"zero", map[string]string{"HEIMDALL_NAG_TURNS": "0"}, nagDefaultTurns},
		{"negative", map[string]string{"HEIMDALL_NAG_TURNS": "-3"}, nagDefaultTurns},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nagThresholdFromEnv(tc.env); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// 6. State persistence round-trip: save then load returns equal state.
func TestNagState_SaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	sid := "session-abc-123"
	in := nagState{Turn: 7, LastNagTurn: 5, LastResetUnix: 1_700_000_000}

	saveNagState(dir, sid, in)
	got := loadNagState(dir, sid)

	if got != in {
		t.Errorf("round-trip: got %+v, want %+v", got, in)
	}
}

// 7. Loading a missing file returns zero state, no error.
func TestNagState_LoadMissingReturnsZero(t *testing.T) {
	dir := t.TempDir()
	got := loadNagState(dir, "never-saved")
	if (got != nagState{}) {
		t.Errorf("got %+v, want zero", got)
	}
}

// 8. Loading malformed JSON returns zero state (graceful corruption recovery).
func TestNagState_LoadMalformedReturnsZero(t *testing.T) {
	dir := t.TempDir()
	sid := "corrupt"
	if err := os.MkdirAll(filepath.Join(dir, ".heimdall_db", "hooks", "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nagStatePath(dir, sid), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := loadNagState(dir, sid)
	if (got != nagState{}) {
		t.Errorf("got %+v, want zero on malformed", got)
	}
}

//  9. Empty session id is a no-op for save/load (defensive — sessionID may be
//     absent on dry-fires).
func TestNagState_EmptySessionIDNoop(t *testing.T) {
	dir := t.TempDir()
	saveNagState(dir, "", nagState{Turn: 99}) // must not panic, must not write
	got := loadNagState(dir, "")
	if (got != nagState{}) {
		t.Errorf("got %+v, want zero for empty session id", got)
	}
}

// 10. spliceNagBeforeFooter inserts before the footer line.
func TestSpliceNagBeforeFooter_InsertsBeforeFooter(t *testing.T) {
	body := "## Heimdall context\n\nbulk\n_retrieved via heimdall-mcp_\n"
	suffix := "\n### Heimdall nudge\nhi\n"
	got := spliceNagBeforeFooter(body, suffix)

	// Note: spliceNagBeforeFooter splits on "\n_retrieved via …" so the
	// suffix glues directly onto the prior line. The lone newline separating
	// "bulk" from the nudge header comes from the footer's leading "\n".
	wantContains := "bulk\n### Heimdall nudge\nhi\n\n_retrieved via heimdall-mcp_\n"
	if !strings.Contains(got, wantContains) {
		t.Errorf("expected splice before footer, got:\n%s", got)
	}
	if strings.Count(got, "_retrieved via heimdall-mcp_") != 1 {
		t.Errorf("footer count = %d, want 1", strings.Count(got, "_retrieved via heimdall-mcp_"))
	}
}

//  11. spliceNagBeforeFooter falls back to append-at-end when the footer is
//     absent (defensive — should not happen in practice).
func TestSpliceNagBeforeFooter_NoFooterAppends(t *testing.T) {
	body := "## Heimdall context\n\nno footer here\n"
	suffix := "\n### Heimdall nudge\nx\n"
	got := spliceNagBeforeFooter(body, suffix)

	if !strings.HasSuffix(got, suffix) {
		t.Errorf("expected suffix appended at end; got:\n%s", got)
	}
}

// 12. spliceNagBeforeFooter returns body unchanged for empty suffix.
func TestSpliceNagBeforeFooter_EmptySuffixIsNoop(t *testing.T) {
	body := "## Heimdall context\n\n_retrieved via heimdall-mcp_\n"
	if got := spliceNagBeforeFooter(body, ""); got != body {
		t.Errorf("got mutation for empty suffix")
	}
}

//  13. hadDirectHeimdallCallSince — fake reader returns one mcp.tool_call line
//     after the cutoff with tool=heimdall_search → true.
func TestHadDirectHeimdallCallSince_PositiveMatch(t *testing.T) {
	since := time.Unix(1_700_000_000, 0)
	reader := func(opts heimdall.ReadHookLogOpts) ([]heimdall.HookLogEntry, error) {
		// Verify the caller asked for the right slice.
		if opts.Event != "mcp.tool_call" {
			t.Errorf("Event filter = %q, want mcp.tool_call", opts.Event)
		}
		if !opts.Since.Equal(since) {
			t.Errorf("Since filter = %v, want %v", opts.Since, since)
		}
		return []heimdall.HookLogEntry{
			{
				Timestamp: since.Add(time.Minute),
				Event:     "mcp.tool_call",
				Fields:    map[string]string{"tool": "heimdall_search"},
			},
		}, nil
	}
	if !hadDirectHeimdallCallSince(reader, since) {
		t.Errorf("expected true on heimdall_search after cutoff")
	}
}

// 14. hadDirectHeimdallCallSince — only non-direct tool entries → false.
func TestHadDirectHeimdallCallSince_NonDirectIgnored(t *testing.T) {
	since := time.Unix(1_700_000_000, 0)
	reader := func(opts heimdall.ReadHookLogOpts) ([]heimdall.HookLogEntry, error) {
		return []heimdall.HookLogEntry{
			{Event: "mcp.tool_call", Fields: map[string]string{"tool": "heimdall_status"}},
			{Event: "mcp.tool_call", Fields: map[string]string{"tool": "heimdall_projects"}},
			{Event: "mcp.tool_call", Fields: map[string]string{"tool": "heimdall_explain"}},
		}, nil
	}
	if hadDirectHeimdallCallSince(reader, since) {
		t.Errorf("expected false when only non-direct tools were called")
	}
}

//  15. hadDirectHeimdallCallSince — reader error → false (treated as
//     "we don't know", same Tier B principle).
func TestHadDirectHeimdallCallSince_ReadErrorIsFalse(t *testing.T) {
	reader := func(opts heimdall.ReadHookLogOpts) ([]heimdall.HookLogEntry, error) {
		return nil, os.ErrPermission
	}
	if hadDirectHeimdallCallSince(reader, time.Now()) {
		t.Errorf("expected false on reader error")
	}
}

// 16. Each of the four direct tools individually triggers reset.
func TestHadDirectHeimdallCallSince_EachDirectTool(t *testing.T) {
	for _, tool := range []string{"heimdall_search", "heimdall_recall", "heimdall_remember", "heimdall_index_text"} {
		t.Run(tool, func(t *testing.T) {
			reader := func(opts heimdall.ReadHookLogOpts) ([]heimdall.HookLogEntry, error) {
				return []heimdall.HookLogEntry{{Event: "mcp.tool_call", Fields: map[string]string{"tool": tool}}}, nil
			}
			if !hadDirectHeimdallCallSince(reader, time.Now()) {
				t.Errorf("%s should trigger reset", tool)
			}
		})
	}
}

//  17. purgeNagStateIfPresent removes the file when present, ignores when
//     missing. Empty session id is a no-op.
func TestPurgeNagStateIfPresent(t *testing.T) {
	dir := t.TempDir()
	sid := "to-delete"
	saveNagState(dir, sid, nagState{Turn: 3})
	if _, err := os.Stat(nagStatePath(dir, sid)); err != nil {
		t.Fatalf("setup: expected file to exist: %v", err)
	}

	purgeNagStateIfPresent(dir, sid)
	if _, err := os.Stat(nagStatePath(dir, sid)); !os.IsNotExist(err) {
		t.Errorf("expected file removed; stat err = %v", err)
	}

	// Calling again with missing file is a no-op (must not return error).
	purgeNagStateIfPresent(dir, sid)
	// Empty session id is a no-op.
	purgeNagStateIfPresent(dir, "")
}

//  18. Saved JSON has the documented shape (regression trap for schema
//     compatibility — a future centralized-DB migration should not silently
//     change field names).
func TestNagState_OnDiskSchemaShape(t *testing.T) {
	dir := t.TempDir()
	sid := "shape"
	saveNagState(dir, sid, nagState{Turn: 4, LastNagTurn: 3, LastResetUnix: 42})

	raw, err := os.ReadFile(nagStatePath(dir, sid))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var asMap map[string]any
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"turn", "last_nag_turn", "last_reset_unix"} {
		if _, ok := asMap[k]; !ok {
			t.Errorf("missing key %q in serialized form: %s", k, raw)
		}
	}
}
