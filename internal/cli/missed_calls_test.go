package cli

import (
	"os"
	"testing"
)

func TestAnalyzeTranscript_EmptyAndMalformed(t *testing.T) {
	res := AnalyzeTranscript(nil)
	if res.Triggers != 0 || len(res.Misses) != 0 {
		t.Errorf("expected empty result on nil input, got %+v", res)
	}
	res = AnalyzeTranscript([]byte("{ not json\n"))
	if res.Triggers != 0 || len(res.Misses) != 0 {
		t.Errorf("expected empty result on malformed input, got %+v", res)
	}
}

func TestAnalyzeTranscript_MissedRead(t *testing.T) {
	data, err := os.ReadFile("testdata/transcript_missed_read.jsonl")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	res := AnalyzeTranscript(data)

	if res.Triggers < 1 {
		t.Errorf("expected at least 1 trigger for read_large_file, got Triggers=%d", res.Triggers)
	}

	// Find the read_large_file miss.
	var readMiss *MissedCall
	for i := range res.Misses {
		if res.Misses[i].Rule == "read_large_file" {
			readMiss = &res.Misses[i]
			break
		}
	}
	if readMiss == nil {
		t.Fatalf("expected a read_large_file miss, got misses=%+v", res.Misses)
	}
	if readMiss.ExpectedTool != "heimdall_search" {
		t.Errorf("expected ExpectedTool=heimdall_search, got %q", readMiss.ExpectedTool)
	}
	if len(readMiss.TriggerExcerpt) > 200 {
		t.Errorf("TriggerExcerpt exceeds 200 chars: %d", len(readMiss.TriggerExcerpt))
	}
}

func TestAnalyzeTranscript_MissedCorrection(t *testing.T) {
	data, err := os.ReadFile("testdata/transcript_missed_correction.jsonl")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	res := AnalyzeTranscript(data)

	if res.Triggers < 1 {
		t.Errorf("expected at least 1 trigger for user_correction, got Triggers=%d", res.Triggers)
	}

	var corrMiss *MissedCall
	for i := range res.Misses {
		if res.Misses[i].Rule == "user_correction" {
			corrMiss = &res.Misses[i]
			break
		}
	}
	if corrMiss == nil {
		t.Fatalf("expected a user_correction miss, got misses=%+v", res.Misses)
	}
	if corrMiss.ExpectedTool != "heimdall_remember" {
		t.Errorf("expected ExpectedTool=heimdall_remember, got %q", corrMiss.ExpectedTool)
	}
	if len(corrMiss.TriggerExcerpt) > 200 {
		t.Errorf("TriggerExcerpt exceeds 200 chars: %d", len(corrMiss.TriggerExcerpt))
	}
}

func TestAnalyzeTranscript_AllCompliant(t *testing.T) {
	data, err := os.ReadFile("testdata/transcript_all_compliant.jsonl")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	res := AnalyzeTranscript(data)

	if len(res.Misses) != 0 {
		t.Errorf("expected 0 misses in compliant transcript, got %+v", res.Misses)
	}
	if res.Triggers == 0 {
		t.Errorf("expected at least some triggers from compliant transcript, got Triggers=0")
	}
	if res.Followed != res.Triggers {
		t.Errorf("expected Followed==Triggers (%d), got Followed=%d", res.Triggers, res.Followed)
	}
}

func TestAnalyzeTranscript_TriggerExcerptCap(t *testing.T) {
	// Ensure no MissedCall.TriggerExcerpt exceeds 200 chars.
	data, err := os.ReadFile("testdata/transcript_missed_correction.jsonl")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	res := AnalyzeTranscript(data)
	for _, m := range res.Misses {
		if len(m.TriggerExcerpt) > 200 {
			t.Errorf("rule %s: TriggerExcerpt exceeds 200 chars (%d)", m.Rule, len(m.TriggerExcerpt))
		}
	}
}

func TestParseTranscriptMessages_StringContent(t *testing.T) {
	data := []byte(`{"role":"user","content":"hello world"}
{"role":"assistant","content":"hi there"}
`)
	msgs := parseTranscriptMessages(data)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Content != "hello world" {
		t.Errorf("unexpected first message: %+v", msgs[0])
	}
	if msgs[1].Role != "assistant" || msgs[1].Content != "hi there" {
		t.Errorf("unexpected second message: %+v", msgs[1])
	}
}

func TestParseTranscriptMessages_BlockContent(t *testing.T) {
	data := []byte(`{"role":"assistant","content":[{"type":"text","text":"thinking"},{"type":"tool_use","id":"1","name":"Read","input":{}}]}`)
	msgs := parseTranscriptMessages(data)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Content != "thinking" {
		t.Errorf("expected Content='thinking', got %q", msgs[0].Content)
	}
	if len(msgs[0].ContentBlocks) != 2 {
		t.Errorf("expected 2 ContentBlocks, got %d", len(msgs[0].ContentBlocks))
	}
}

func TestEvaluateUserCorrection_NoneWhenFollowed(t *testing.T) {
	// Correction immediately followed by heimdall_remember.
	data := []byte(`{"role":"user","content":"Actually no, do it differently."}
{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"heimdall_remember","input":{"content":"user wants different approach"}}]}
`)
	msgs := parseTranscriptMessages(data)
	triggers, followed, misses := evaluateUserCorrection(msgs)
	if triggers != 1 {
		t.Errorf("expected 1 trigger, got %d", triggers)
	}
	if followed != 1 {
		t.Errorf("expected 1 followed, got %d", followed)
	}
	if len(misses) != 0 {
		t.Errorf("expected 0 misses, got %+v", misses)
	}
}

func TestEvaluateTaskStartRecall_FiresOnFirstMessage(t *testing.T) {
	// First user message mentions architecture; heimdall_recall follows.
	data := []byte(`{"role":"user","content":"We have a task about the architecture of the system from prior decisions."}
{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"heimdall_recall","input":{"query":"architecture"}}]}
`)
	msgs := parseTranscriptMessages(data)
	triggers, followed, misses := evaluateTaskStartRecall(msgs)
	if triggers != 1 {
		t.Errorf("expected 1 trigger, got %d", triggers)
	}
	if followed != 1 {
		t.Errorf("expected 1 followed, got %d", followed)
	}
	if len(misses) != 0 {
		t.Errorf("expected 0 misses, got %+v", misses)
	}
}

func TestEvaluateTaskStartRecall_MissWhenNoRecall(t *testing.T) {
	// First user message triggers; assistant does not call heimdall_recall.
	data := []byte(`{"role":"user","content":"Based on past decisions and the overall architecture plan, what should we do?"}
{"role":"assistant","content":"Let me think about that."}
{"role":"assistant","content":"Here is my answer."}
{"role":"assistant","content":"And some more details."}
`)
	msgs := parseTranscriptMessages(data)
	triggers, followed, misses := evaluateTaskStartRecall(msgs)
	if triggers != 1 {
		t.Errorf("expected 1 trigger, got %d", triggers)
	}
	if followed != 0 {
		t.Errorf("expected 0 followed, got %d", followed)
	}
	if len(misses) != 1 {
		t.Errorf("expected 1 miss, got %d: %+v", len(misses), misses)
	}
}

func TestEvaluateReadLargeFile_MissWhenNoSearch(t *testing.T) {
	data, err := os.ReadFile("testdata/transcript_missed_read.jsonl")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	msgs := parseTranscriptMessages(data)
	triggers, followed, misses := evaluateReadLargeFile(msgs)
	if triggers < 1 {
		t.Errorf("expected >=1 trigger, got %d", triggers)
	}
	if followed != 0 {
		t.Errorf("expected 0 followed (no heimdall_search present), got %d", followed)
	}
	if len(misses) == 0 {
		t.Errorf("expected >=1 miss, got 0")
	}
}

// TestAnalyzeTranscript_RealShape verifies that AnalyzeTranscript correctly
// handles the real Claude Code transcript shape (type discriminator + nested
// message) and recognises mcp__heimdall__* tool names via normalizeToolName.
//
// Fixture transcript_real_shape.jsonl contains:
//   - 1 user correction ("actually no, do X instead") → triggers user_correction
//   - 1 mcp__heimdall__heimdall_remember in the NEXT assistant turn → Followed=1
//   - 1 large Read result (>200 lines) without heimdall_search follow-up → miss
func TestAnalyzeTranscript_RealShape(t *testing.T) {
	data, err := os.ReadFile("testdata/transcript_real_shape.jsonl")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	res := AnalyzeTranscript(data)

	// user_correction should fire exactly once.
	// Count triggers/followed attributed to user_correction.
	corrTriggers, corrFollowed := 0, 0
	for _, m := range res.Misses {
		if m.Rule == "user_correction" {
			corrTriggers++
		}
	}
	// Triggers - misses = followed (for this rule specifically).
	// We'll verify by running the evaluator directly.
	msgs := parseTranscriptMessages(data)
	triggers, followed, misses := evaluateUserCorrection(msgs)

	if triggers != 1 {
		t.Errorf("user_correction: expected 1 trigger, got %d", triggers)
	}
	if followed != 1 {
		t.Errorf("user_correction: expected 1 followed (mcp__heimdall__heimdall_remember should be recognised), got %d", followed)
	}
	if len(misses) != 0 {
		t.Errorf("user_correction: expected 0 misses (correction WAS followed), got %+v", misses)
	}
	_ = corrTriggers
	_ = corrFollowed

	// Overall: at least 1 trigger must be detected.
	if res.Triggers == 0 {
		t.Error("expected at least 1 trigger in real-shape transcript, got 0")
	}
}
