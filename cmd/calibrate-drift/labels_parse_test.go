package main

import (
	"strings"
	"testing"
)

// TestParseLabels_Valid covers the happy path: three well-formed JSONL
// lines (one per label bucket) round-trip into LabeledTurn values with
// all fields preserved.
func TestParseLabels_Valid(t *testing.T) {
	input := strings.Join([]string{
		`{"session_id":"s1","turn_idx":0,"prompt":"hi","injected_hit_ids":["a","b"],"heimdall_search_queries":["q1"],"heimdall_tools_used":["heimdall_search"],"v1_flagged":true,"label":"truly_redundant"}`,
		`{"session_id":"s1","turn_idx":1,"prompt":"bye","injected_hit_ids":[],"heimdall_search_queries":[],"heimdall_tools_used":[],"v1_flagged":false,"label":"truly_missed"}`,
		`{"session_id":"s2","turn_idx":0,"prompt":"maybe","injected_hit_ids":["c"],"heimdall_search_queries":["q2"],"heimdall_tools_used":["heimdall_recall"],"v1_flagged":false,"label":"neither"}`,
	}, "\n")

	turns, err := parseLabels(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parseLabels: %v", err)
	}
	if got, want := len(turns), 3; got != want {
		t.Fatalf("got %d turns, want %d", got, want)
	}
	if turns[0].Label != LabelTrulyRedundant {
		t.Errorf("turn 0 label = %q, want truly_redundant", turns[0].Label)
	}
	if !turns[0].V1Flagged {
		t.Errorf("turn 0 v1_flagged = false, want true")
	}
	if got, want := turns[1].TurnIdx, 1; got != want {
		t.Errorf("turn 1 idx = %d, want %d", got, want)
	}
	if got, want := len(turns[2].InjectedHitIDs), 1; got != want {
		t.Errorf("turn 2 hit ids len = %d, want %d", got, want)
	}
}

// TestParseLabels_Malformed_JSON asserts that a line the JSON parser
// cannot decode (stray trailing character) is a hard error that names
// the line number — the operator needs to jump straight to the bad
// row and fix it rather than silently dropping a label.
func TestParseLabels_Malformed_JSON(t *testing.T) {
	input := `{"session_id":"s1","turn_idx":0,"prompt":"ok","label":"neither"}` + "\n" +
		`{"session_id":"s2","turn_idx":0,"label":"neither"}garbage` + "\n"
	_, err := parseLabels(strings.NewReader(input))
	if err == nil {
		t.Fatalf("want error, got nil")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error %q should include line number 2", err.Error())
	}
}

// TestParseLabels_UnknownLabel protects the precision/recall math —
// an unknown label value must fail loudly instead of silently landing
// in the TN bucket.
func TestParseLabels_UnknownLabel(t *testing.T) {
	input := `{"session_id":"s1","turn_idx":0,"prompt":"x","label":"kinda-redundant"}` + "\n"
	_, err := parseLabels(strings.NewReader(input))
	if err == nil {
		t.Fatalf("want error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown label") {
		t.Errorf("error %q should mention unknown label", err.Error())
	}
}

// TestParseLabels_MissingFields checks that required fields
// (session_id, label) produce line-numbered errors. The harness
// leans on these so an accidental truncation during labeling doesn't
// get counted as a zero-filled turn.
func TestParseLabels_MissingFields(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "no session id",
			input: `{"turn_idx":0,"prompt":"x","label":"neither"}`,
			want:  "session_id",
		},
		{
			name:  "no label",
			input: `{"session_id":"s","turn_idx":0,"prompt":"x"}`,
			want:  "label is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseLabels(strings.NewReader(tc.input))
			if err == nil {
				t.Fatalf("want error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

// TestParseLabels_ExtraKeys documents that the parser is forward-compat
// — unknown JSON fields (that plan-12a §3.2 hasn't defined yet) are
// silently dropped so a future label schema revision can land without
// coordinating with this harness.
func TestParseLabels_ExtraKeys(t *testing.T) {
	input := `{"session_id":"s1","turn_idx":2,"prompt":"p","label":"neither","future_key":"ignore me","nested":{"also":"dropped"}}` + "\n"
	turns, err := parseLabels(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parseLabels: %v", err)
	}
	if got, want := len(turns), 1; got != want {
		t.Fatalf("got %d turns, want %d", got, want)
	}
	if turns[0].Label != LabelNeither {
		t.Errorf("turn 0 label = %q, want neither", turns[0].Label)
	}
}

// TestParseLabels_EmptyInput + blank lines — the parser should skip
// blank lines (operators will sometimes append a trailing newline)
// and return (nil, nil) for an empty file. Empty file is not an error
// here; the caller decides whether to fail on NTurnsTotal==0.
func TestParseLabels_EmptyAndBlanks(t *testing.T) {
	turns, err := parseLabels(strings.NewReader("\n\n   \n"))
	if err != nil {
		t.Fatalf("parseLabels: %v", err)
	}
	if len(turns) != 0 {
		t.Fatalf("got %d turns, want 0", len(turns))
	}
}
