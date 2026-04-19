package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/config"
)

func TestHookStop_AppendsBuffer(t *testing.T) {
	dir := t.TempDir()
	payload := stopPayload{
		SessionID:            "test-session-1",
		CWD:                  dir,
		LastAssistantMessage: "Here is the implementation.",
		StopHookActive:       true,
		HookEventName:        "Stop",
	}
	data, _ := json.Marshal(payload)

	cfg := config.DefaultConfig()
	code := HookStop(cfg, bytes.NewReader(data), &bytes.Buffer{}, &bytes.Buffer{}, map[string]string{}, nil)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}

	bufPath := filepath.Join(dir, ".heimdall_db", "hooks", "sessions", "test-session-1.jsonl")
	content, err := os.ReadFile(bufPath)
	if err != nil {
		t.Fatalf("buffer file not created: %v", err)
	}
	if !strings.Contains(string(content), "Here is the implementation.") {
		t.Fatalf("buffer doesn't contain message: %s", content)
	}
}

func TestHookStop_EmptySession(t *testing.T) {
	payload := stopPayload{SessionID: "", LastAssistantMessage: "msg"}
	data, _ := json.Marshal(payload)
	code := HookStop(config.DefaultConfig(), bytes.NewReader(data), &bytes.Buffer{}, &bytes.Buffer{}, map[string]string{}, nil)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestHookStop_EmptyMessage(t *testing.T) {
	payload := stopPayload{SessionID: "s1", LastAssistantMessage: ""}
	data, _ := json.Marshal(payload)
	code := HookStop(config.DefaultConfig(), bytes.NewReader(data), &bytes.Buffer{}, &bytes.Buffer{}, map[string]string{}, nil)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestHookStop_MalformedJSON(t *testing.T) {
	code := HookStop(config.DefaultConfig(), strings.NewReader("{bad json"), &bytes.Buffer{}, &bytes.Buffer{}, map[string]string{}, nil)
	if code != 0 {
		t.Fatalf("expected exit 0 on malformed JSON, got %d", code)
	}
}

func TestHookStop_Disabled(t *testing.T) {
	payload := stopPayload{SessionID: "s1", LastAssistantMessage: "msg"}
	data, _ := json.Marshal(payload)
	env := map[string]string{"HEIMDALL_HOOKS": "0"}
	code := HookStop(config.DefaultConfig(), bytes.NewReader(data), &bytes.Buffer{}, &bytes.Buffer{}, env, nil)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestHookStop_MultipleAppends(t *testing.T) {
	dir := t.TempDir()
	cfg := config.DefaultConfig()

	for i := 0; i < 3; i++ {
		payload := stopPayload{
			SessionID:            "multi-session",
			CWD:                  dir,
			LastAssistantMessage: "message " + string(rune('A'+i)),
			StopHookActive:       true,
		}
		data, _ := json.Marshal(payload)
		code := HookStop(cfg, bytes.NewReader(data), &bytes.Buffer{}, &bytes.Buffer{}, map[string]string{}, nil)
		if code != 0 {
			t.Fatalf("append %d: expected exit 0, got %d", i, code)
		}
	}

	bufPath := filepath.Join(dir, ".heimdall_db", "hooks", "sessions", "multi-session.jsonl")
	content, err := os.ReadFile(bufPath)
	if err != nil {
		t.Fatalf("buffer file not created: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %s", len(lines), content)
	}
}

func TestHookSessionEnd_CleansUpBuffer(t *testing.T) {
	dir := t.TempDir()
	cfg := config.DefaultConfig()

	// First create a buffer file via Stop
	stopData, _ := json.Marshal(stopPayload{
		SessionID:            "cleanup-session",
		CWD:                  dir,
		LastAssistantMessage: "test msg",
	})
	HookStop(cfg, bytes.NewReader(stopData), &bytes.Buffer{}, &bytes.Buffer{}, map[string]string{}, nil)

	bufPath := filepath.Join(dir, ".heimdall_db", "hooks", "sessions", "cleanup-session.jsonl")
	if _, err := os.Stat(bufPath); err != nil {
		t.Fatalf("buffer should exist before session-end: %v", err)
	}

	// Now fire SessionEnd — it should clean up the buffer
	endData, _ := json.Marshal(sessionEndPayload{
		SessionID:     "cleanup-session",
		CWD:           dir,
		HookEventName: "SessionEnd",
		Reason:        "clear",
	})
	code := HookSessionEnd(cfg, bytes.NewReader(endData), &bytes.Buffer{}, &bytes.Buffer{}, map[string]string{}, nil)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}

	if _, err := os.Stat(bufPath); !os.IsNotExist(err) {
		t.Fatal("buffer file should be removed after session-end")
	}
}

func TestHookSessionEnd_EmptySession(t *testing.T) {
	data, _ := json.Marshal(sessionEndPayload{SessionID: ""})
	code := HookSessionEnd(config.DefaultConfig(), bytes.NewReader(data), &bytes.Buffer{}, &bytes.Buffer{}, map[string]string{}, nil)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestHookSessionEnd_MalformedJSON(t *testing.T) {
	code := HookSessionEnd(config.DefaultConfig(), strings.NewReader("nope"), &bytes.Buffer{}, &bytes.Buffer{}, map[string]string{}, nil)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

// TestHookStop_LogsSingleEventStamp is a regression guard: the Stop hook handler
// must not pass "event" in its kv map to LogHookEvent — the logger already
// stamps `event=<name>` from its own `event` parameter. Passing it in kv too
// produced a duplicate token in the log line (e.g. `event=stop ... event=stop`),
// breaking grep-friendly parsing and making the output look malformed.
func TestHookStop_LogsSingleEventStamp(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HEIMDALL_HOOK_LOG", filepath.Join(tmp, "hooks.log"))
	t.Setenv("HEIMDALL_HOOKS", "1")

	projectDir := t.TempDir()
	payload := stopPayload{
		SessionID:            "single-stamp-session",
		CWD:                  projectDir,
		LastAssistantMessage: "msg",
		StopHookActive:       true,
	}
	data, _ := json.Marshal(payload)
	code := HookStop(config.DefaultConfig(), bytes.NewReader(data), &bytes.Buffer{}, &bytes.Buffer{}, map[string]string{}, nil)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}

	logBytes, err := os.ReadFile(filepath.Join(tmp, "hooks.log"))
	if err != nil {
		t.Fatalf("hook log not written: %v", err)
	}
	logStr := string(logBytes)
	// Find the buffer_appended line — that's the one with the dup before the fix.
	var target string
	for _, line := range strings.Split(strings.TrimSpace(logStr), "\n") {
		if strings.Contains(line, "buffer_appended") {
			target = line
			break
		}
	}
	if target == "" {
		t.Fatalf("no buffer_appended line in hook log:\n%s", logStr)
	}
	if n := strings.Count(target, "event=stop"); n != 1 {
		t.Fatalf("expected exactly 1 `event=stop` token on buffer_appended line, got %d:\n%s", n, target)
	}
}

// TestHookSessionEnd_LogsSingleEventStamp is the SessionEnd counterpart to the
// Stop regression above. Same bug (`"event": "session-end"` in the kv map on
// top of the logger's own stamp), same fix.
func TestHookSessionEnd_LogsSingleEventStamp(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HEIMDALL_HOOK_LOG", filepath.Join(tmp, "hooks.log"))
	t.Setenv("HEIMDALL_HOOKS", "1")

	projectDir := t.TempDir()
	// No buffer file needed; HookSessionEnd will still log session_ended.
	payload := sessionEndPayload{
		SessionID:     "single-stamp-session-end",
		CWD:           projectDir,
		HookEventName: "SessionEnd",
		Reason:        "clear",
	}
	data, _ := json.Marshal(payload)
	code := HookSessionEnd(config.DefaultConfig(), bytes.NewReader(data), &bytes.Buffer{}, &bytes.Buffer{}, map[string]string{}, nil)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}

	logBytes, err := os.ReadFile(filepath.Join(tmp, "hooks.log"))
	if err != nil {
		t.Fatalf("hook log not written: %v", err)
	}
	logStr := string(logBytes)
	var target string
	for _, line := range strings.Split(strings.TrimSpace(logStr), "\n") {
		if strings.Contains(line, "session_ended") {
			target = line
			break
		}
	}
	if target == "" {
		t.Fatalf("no session_ended line in hook log:\n%s", logStr)
	}
	if n := strings.Count(target, "event=session-end"); n != 1 {
		t.Fatalf("expected exactly 1 `event=session-end` token on session_ended line, got %d:\n%s", n, target)
	}
}

func TestExtractTranscriptSummary_Empty(t *testing.T) {
	got, _ := extractTranscriptSummary(nil)
	if got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestExtractTranscriptSummary_AssistantMessages(t *testing.T) {
	lines := []string{
		`{"role":"user","content":"hello"}`,
		`{"role":"assistant","content":"Hi there!"}`,
		`{"role":"user","content":"what time is it?"}`,
		`{"role":"assistant","content":"I don't have access to a clock."}`,
	}
	data := []byte(strings.Join(lines, "\n"))
	got, _ := extractTranscriptSummary(data)
	if !strings.Contains(got, "Hi there!") {
		t.Fatalf("expected assistant message, got %q", got)
	}
	if !strings.Contains(got, "don't have access") {
		t.Fatalf("expected second assistant message, got %q", got)
	}
}

func TestExtractTranscriptSummary_TakesLast5(t *testing.T) {
	var lines []string
	for i := 0; i < 10; i++ {
		lines = append(lines, `{"role":"assistant","content":"msg`+string(rune('A'+i))+`"}`)
	}
	data := []byte(strings.Join(lines, "\n"))
	got, _ := extractTranscriptSummary(data)
	// Should only contain the last 5 (F through J)
	if strings.Contains(got, "msgA") {
		t.Fatal("should not contain first messages")
	}
}

func TestExtractTranscriptSummary_MarkerWindows(t *testing.T) {
	data, err := os.ReadFile("testdata/transcript_markers.jsonl")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	summary, candidates := extractTranscriptSummary(data)

	if summary == "" {
		t.Fatalf("expected non-empty summary")
	}
	seen := map[string]bool{}
	for _, c := range candidates {
		seen[c.Marker] = true
	}
	for _, want := range []string{"correction", "teaching", "workaround"} {
		if !seen[want] {
			t.Errorf("missing candidate marker %q; got %+v", want, candidates)
		}
	}
	for _, c := range candidates {
		if len(c.Excerpt) > 200 {
			t.Errorf("excerpt exceeds 200 chars: %d", len(c.Excerpt))
		}
	}
}

func TestExtractTranscriptSummary_NoMarkers_TailFallback(t *testing.T) {
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

func TestHookSessionEnd_WritesReviewRecord(t *testing.T) {
	dir := t.TempDir()

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

// TestHookSessionEnd_RecordsMissesInReview verifies that when the transcript
// contains a user correction not followed by heimdall_remember, the review
// record includes a misses entry with the correct rule and expected tool.
func TestHookSessionEnd_RecordsMissesInReview(t *testing.T) {
	dir := t.TempDir()

	// Transcript: correction without a following heimdall_remember.
	// 5 corrections to exceed the suppression threshold (candidates > 2).
	tPath := filepath.Join(dir, "transcript.jsonl")
	lines := []string{
		`{"role":"assistant","content":"I will use global state."}`,
		`{"role":"user","content":"Actually no, do not use global variables."}`,
		`{"role":"assistant","content":"Understood."}`,
		`{"role":"user","content":"no, that approach is wrong too"}`,
		`{"role":"assistant","content":"Let me try again."}`,
		`{"role":"user","content":"stop — this is still broken"}`,
		`{"role":"assistant","content":"OK."}`,
	}
	if err := os.WriteFile(tPath, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	payload := sessionEndPayload{
		SessionID:      "s-misses-1",
		TranscriptPath: tPath,
		CWD:            dir,
		HookEventName:  "SessionEnd",
		Reason:         "user_exit",
	}
	data, _ := json.Marshal(payload)

	code := HookSessionEnd(config.DefaultConfig(), bytes.NewReader(data), &bytes.Buffer{}, &bytes.Buffer{}, map[string]string{}, nil)
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

	// Verify misses field is present and non-empty.
	missesRaw, ok := rev["misses"]
	if !ok {
		t.Fatalf("review file missing 'misses' field; keys: %v", rev)
	}
	misses, ok := missesRaw.([]any)
	if !ok || len(misses) == 0 {
		t.Fatalf("expected non-empty misses array, got %T: %v", missesRaw, missesRaw)
	}

	// Verify the first miss has the right rule and expected_tool.
	firstMiss, ok := misses[0].(map[string]any)
	if !ok {
		t.Fatalf("expected miss to be a map, got %T", misses[0])
	}
	if rule, _ := firstMiss["rule"].(string); rule != "user_correction" {
		t.Errorf("expected rule=user_correction, got %q", rule)
	}
	if et, _ := firstMiss["expected_tool"].(string); et != "heimdall_remember" {
		t.Errorf("expected expected_tool=heimdall_remember, got %q", et)
	}

	// Verify triggers and followed fields are present.
	if _, ok := rev["triggers"]; !ok {
		t.Errorf("review file missing 'triggers' field")
	}
	if _, ok := rev["followed"]; !ok {
		t.Errorf("review file missing 'followed' field")
	}
}
