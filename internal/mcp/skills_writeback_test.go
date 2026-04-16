package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// withSkillsDir overrides HEIMDALL_CLAUDE_SKILLS_DIR for the duration of the
// test so attachSkillFileWrite never touches a real ~/.claude/skills.
func withSkillsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(heimdall.ClaudeSkillsDirEnv, dir)
	return dir
}

// TestAttachSkillFileWrite is a unit test of the helper used by toolRemember
// to materialize a skill memory to disk. Lets us cover the happy path,
// no-op-by-default, overwrite refusal, and error reporting without going
// through the full MCP plumbing (which requires a live embedder).
func TestAttachSkillFileWrite_NoOpWhenWriteFileFalse(t *testing.T) {
	dir := withSkillsDir(t)
	payload := map[string]any{}
	attachSkillFileWrite(payload, rememberInput{
		Content: "body",
		Type:    "skill",
		// WriteFile defaults false
	}, heimdall.MemoryTypeSkill)
	if _, hasFile := payload["skill_file"]; hasFile {
		t.Errorf("write_file=false should not produce skill_file in payload: %v", payload)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("dir must remain empty when write_file=false, got %d entries", len(entries))
	}
}

func TestAttachSkillFileWrite_NoOpWhenNonSkillType(t *testing.T) {
	dir := withSkillsDir(t)
	payload := map[string]any{}
	attachSkillFileWrite(payload, rememberInput{
		Content:   "body",
		Type:      "fact",
		WriteFile: true, // upstream toolRemember would have rejected this
	}, heimdall.MemoryTypeFact)
	if _, hasFile := payload["skill_file"]; hasFile {
		t.Errorf("non-skill type must not write file: %v", payload)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("dir must remain empty for non-skill, got %d entries", len(entries))
	}
}

func TestAttachSkillFileWrite_HappyPath(t *testing.T) {
	dir := withSkillsDir(t)
	payload := map[string]any{}
	attachSkillFileWrite(payload, rememberInput{
		Content:          "Step 1.\nStep 2.",
		Type:             "skill",
		SkillName:        "Run tests",
		SkillDescription: "Always run tests before committing.",
		WriteFile:        true,
	}, heimdall.MemoryTypeSkill)
	got, ok := payload["skill_file"].(string)
	if !ok || got == "" {
		t.Fatalf("skill_file missing in payload: %v", payload)
	}
	wantPath := filepath.Join(dir, "run-tests", "SKILL.md")
	if got != wantPath {
		t.Errorf("skill_file = %q, want %q", got, wantPath)
	}
	data, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("read skill file: %v", err)
	}
	written := string(data)
	if !strings.Contains(written, "name: Run tests") {
		t.Errorf("missing name in frontmatter: %q", written)
	}
	if !strings.Contains(written, "description: Always run tests before committing.") {
		t.Errorf("missing description: %q", written)
	}
	if !strings.Contains(written, "Step 1.") || !strings.Contains(written, "Step 2.") {
		t.Errorf("missing body lines: %q", written)
	}
}

func TestAttachSkillFileWrite_RefuseOverwriteByDefault(t *testing.T) {
	dir := withSkillsDir(t)
	// Pre-create a skill file at the destination.
	target := filepath.Join(dir, "preexisting", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("OLD"), 0o644); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{}
	attachSkillFileWrite(payload, rememberInput{
		Content:   "new body",
		Type:      "skill",
		SkillName: "preexisting",
		WriteFile: true,
	}, heimdall.MemoryTypeSkill)
	if _, ok := payload["skill_file_error"]; !ok {
		t.Errorf("expected skill_file_error in payload, got %v", payload)
	}
	data, _ := os.ReadFile(target)
	if string(data) != "OLD" {
		t.Errorf("file should not be overwritten: %q", data)
	}
}

func TestAttachSkillFileWrite_OverwriteFlag(t *testing.T) {
	dir := withSkillsDir(t)
	target := filepath.Join(dir, "replaceme", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("OLD"), 0o644); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{}
	attachSkillFileWrite(payload, rememberInput{
		Content:            "fresh body",
		Type:               "skill",
		SkillName:          "replaceme",
		WriteFile:          true,
		WriteFileOverwrite: true,
	}, heimdall.MemoryTypeSkill)
	if _, hasErr := payload["skill_file_error"]; hasErr {
		t.Errorf("overwrite=true should not error: %v", payload)
	}
	data, _ := os.ReadFile(target)
	if !strings.Contains(string(data), "fresh body") {
		t.Errorf("file should be overwritten: %q", data)
	}
}

func TestAttachSkillFileWrite_FallsBackToFirstLine(t *testing.T) {
	dir := withSkillsDir(t)
	payload := map[string]any{}
	// No SkillName and no SkillDescription — helper must derive both
	// from the first non-empty line of Content.
	attachSkillFileWrite(payload, rememberInput{
		Content:   "First line skill\n\nMore body",
		Type:      "skill",
		WriteFile: true,
	}, heimdall.MemoryTypeSkill)
	got, ok := payload["skill_file"].(string)
	if !ok || got == "" {
		t.Fatalf("skill_file missing: %v", payload)
	}
	if filepath.Base(filepath.Dir(got)) != "first-line-skill" {
		t.Errorf("slug should derive from first line, got dir %q", filepath.Dir(got))
	}
	data, _ := os.ReadFile(got)
	if !strings.Contains(string(data), "name: First line skill") {
		t.Errorf("name not derived from content: %q", data)
	}
	_ = dir
}

// TestToolRemember_WriteFileRejectedForNonSkill exercises the validation
// added to toolRemember directly so callers see a clear MCP-level error.
func TestToolRemember_WriteFileRejectedForNonSkill(t *testing.T) {
	s := testServerWithMemory(t)
	withSkillsDir(t)
	args, _ := json.Marshal(map[string]any{
		"content":    "not a skill",
		"type":       "fact",
		"write_file": true,
	})
	result := s.toolRemember(args)
	if !result.IsError {
		t.Errorf("expected error rejecting write_file on non-skill type")
	}
	if !strings.Contains(result.Content[0].Text, "write_file=true is only valid when type=\"skill\"") {
		t.Errorf("unexpected error text: %q", result.Content[0].Text)
	}
}

// TestToolRemember_AcceptsSkillType verifies the type validation message
// now mentions "skill" (was a tiny correctness bug — message lied to caller).
func TestToolRemember_RejectsBadTypeMessageMentionsSkill(t *testing.T) {
	s := testServerWithMemory(t)
	args, _ := json.Marshal(map[string]any{
		"content": "anything",
		"type":    "not-a-real-type",
	})
	result := s.toolRemember(args)
	if !result.IsError {
		t.Fatalf("expected error for invalid type")
	}
	if !strings.Contains(result.Content[0].Text, "skill") {
		t.Errorf("error message should now list 'skill' as an option, got %q", result.Content[0].Text)
	}
}
