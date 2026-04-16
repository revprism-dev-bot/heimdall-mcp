package heimdall

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeFile is a tiny test helper so each fixture write fits on one line.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// fakeEmbed returns a deterministic 3-d vector that varies with the input
// length. Lets tests confirm that Embed was called for new content but not
// for unchanged content (where the hash-equality path short-circuits).
func fakeEmbed(content string) ([]float32, error) {
	return []float32{1, float32(len(content)), 0}, nil
}

func TestParseSkillFile_Happy(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "SKILL.md")
	writeFile(t, path, `---
name: test-skill
description: A simple skill for tests.
---

# Body header

Some content.
`)
	sf, err := ParseSkillFile(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sf.Name != "test-skill" {
		t.Errorf("name = %q, want test-skill", sf.Name)
	}
	if sf.Description != "A simple skill for tests." {
		t.Errorf("description = %q", sf.Description)
	}
	if !strings.Contains(sf.Body, "# Body header") {
		t.Errorf("body missing header: %q", sf.Body)
	}
	if !strings.Contains(sf.Body, "Some content.") {
		t.Errorf("body missing content: %q", sf.Body)
	}
}

func TestParseSkillFile_NoFrontmatter(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "SKILL.md")
	writeFile(t, path, "Just a body, no frontmatter at all.\n")
	sf, err := ParseSkillFile(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sf.Name != filepath.Base(filepath.Dir(path)) {
		t.Errorf("name fallback should be parent dir base: got %q", sf.Name)
	}
	if sf.Description != "" {
		t.Errorf("description should be empty, got %q", sf.Description)
	}
	if !strings.Contains(sf.Body, "Just a body") {
		t.Errorf("body lost: %q", sf.Body)
	}
}

func TestParseSkillFile_QuotedValues(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "SKILL.md")
	writeFile(t, path, `---
name: "quoted-name"
description: 'single quotes work too'
---
body
`)
	sf, err := ParseSkillFile(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sf.Name != "quoted-name" {
		t.Errorf("quoted name not stripped: %q", sf.Name)
	}
	if sf.Description != "single quotes work too" {
		t.Errorf("quoted description not stripped: %q", sf.Description)
	}
}

func TestParseSkillFile_MalformedFrontmatter(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "SKILL.md")
	writeFile(t, path, "---\nname: oops\nno closing fence\n")
	if _, err := ParseSkillFile(path); err == nil {
		t.Errorf("expected error for missing closing fence")
	}
}

func TestParseSkillFile_NonexistentReturnsErr(t *testing.T) {
	if _, err := ParseSkillFile(filepath.Join(t.TempDir(), "missing.md")); err == nil {
		t.Errorf("expected error for missing file")
	}
}

func TestSlugifySkill_Cases(t *testing.T) {
	cases := map[string]string{
		"simple":              "simple",
		"With Spaces":         "with-spaces",
		"UPPER_case":          "upper-case",
		"weird !@# chars":     "weird-chars",
		"-leading-dashes-":    "leading-dashes",
		"":                    "skill",
		"!!!":                 "skill",
		"a/b/c":               "a-b-c",
		"name.with.dots":      "name-with-dots",
		"already-good":        "already-good",
		"123-numbers":         "123-numbers",
	}
	for in, want := range cases {
		if got := SlugifySkill(in); got != want {
			t.Errorf("SlugifySkill(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveClaudeSkillsDir_Precedence(t *testing.T) {
	// Override always wins.
	if got := ResolveClaudeSkillsDir("/explicit", map[string]string{ClaudeSkillsDirEnv: "/env"}); got != "/explicit" {
		t.Errorf("override should win, got %q", got)
	}
	// Env wins when no override.
	if got := ResolveClaudeSkillsDir("", map[string]string{ClaudeSkillsDirEnv: "/from-env"}); got != "/from-env" {
		t.Errorf("env should win, got %q", got)
	}
	// Without override or env (and clearing the process env so the real one
	// doesn't leak in), falls back to ~/.claude/skills.
	t.Setenv(ClaudeSkillsDirEnv, "")
	got := ResolveClaudeSkillsDir("", nil)
	if got == "" {
		t.Skip("home dir unresolvable on this test runner; default path skipped")
	}
	if !strings.HasSuffix(got, filepath.Join(".claude", "skills")) {
		t.Errorf("default should end with .claude/skills, got %q", got)
	}
}

func TestSkillMemoryContent_Format(t *testing.T) {
	got := SkillMemoryContent("Foo", "bar", "body lines")
	if !strings.Contains(got, "skill: Foo") {
		t.Errorf("missing name header: %q", got)
	}
	if !strings.Contains(got, "description: bar") {
		t.Errorf("missing description: %q", got)
	}
	if !strings.HasSuffix(got, "body lines") {
		t.Errorf("body should be last: %q", got)
	}
}

func TestSkillMemoryID_Stable(t *testing.T) {
	if SkillMemoryID("foo") != SkillMemoryID("foo") {
		t.Errorf("memory ID should be stable")
	}
	if SkillMemoryID("foo") == SkillMemoryID("bar") {
		t.Errorf("different slugs should yield different IDs")
	}
}

// importTestStore builds a memory store and returns it; cleanup is wired.
func importTestStore(t *testing.T) *MemoryStore {
	t.Helper()
	store, err := OpenMemoryStore(filepath.Join(t.TempDir(), "memories.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func writeSampleSkillsTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "alpha", "SKILL.md"),
		"---\nname: alpha\ndescription: First skill.\n---\nAlpha body\n")
	writeFile(t, filepath.Join(root, "beta", "SKILL.md"),
		"---\nname: beta\ndescription: Second skill.\n---\nBeta body\n")
	writeFile(t, filepath.Join(root, ".hidden", "SKILL.md"),
		"---\nname: hidden\n---\nshould skip\n")
	// directory without SKILL.md: must be skipped without error.
	if err := os.MkdirAll(filepath.Join(root, "no-skill-md"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return root
}

func TestImportSkillsFromDir_HappyPath(t *testing.T) {
	store := importTestStore(t)
	root := writeSampleSkillsTree(t)

	res, err := ImportSkillsFromDir(store, SkillImportOpts{
		Dir:   root,
		Embed: fakeEmbed,
		Now:   func() time.Time { return time.Unix(1234567890, 0) },
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Created != 2 {
		t.Errorf("Created = %d, want 2", res.Created)
	}
	if res.Updated != 0 {
		t.Errorf("Updated = %d, want 0", res.Updated)
	}
	if res.Unchanged != 0 {
		t.Errorf("Unchanged = %d, want 0", res.Unchanged)
	}
	if res.Skipped < 1 {
		t.Errorf("expected at least one skipped (.hidden, no-skill-md), got %d", res.Skipped)
	}
	if len(res.Errors) != 0 {
		t.Errorf("unexpected errors: %v", res.Errors)
	}
	// Hidden skill must NOT be imported.
	if got, _ := store.GetMemoryByID(SkillMemoryID("hidden")); got != nil {
		t.Errorf("hidden skill should be skipped, got %+v", got)
	}
	// Confirm one of the imports landed with the right content + tags.
	alpha, err := store.GetMemoryByID(SkillMemoryID("alpha"))
	if err != nil || alpha == nil {
		t.Fatalf("alpha not stored: err=%v mem=%v", err, alpha)
	}
	if alpha.Type != MemoryTypeSkill {
		t.Errorf("alpha.Type = %q, want skill", alpha.Type)
	}
	hasClaudeSkillTag := false
	for _, tag := range alpha.Tags {
		if tag == "claude-skill" {
			hasClaudeSkillTag = true
		}
	}
	if !hasClaudeSkillTag {
		t.Errorf("alpha missing claude-skill tag, got %v", alpha.Tags)
	}
}

func TestImportSkillsFromDir_Idempotent(t *testing.T) {
	store := importTestStore(t)
	root := writeSampleSkillsTree(t)

	first, err := ImportSkillsFromDir(store, SkillImportOpts{Dir: root, Embed: fakeEmbed})
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	if first.Created != 2 {
		t.Errorf("first Created = %d, want 2", first.Created)
	}

	// Re-running with no changes must be a clean Unchanged result.
	embedCalls := 0
	second, err := ImportSkillsFromDir(store, SkillImportOpts{
		Dir: root,
		Embed: func(s string) ([]float32, error) {
			embedCalls++
			return fakeEmbed(s)
		},
	})
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if second.Created != 0 {
		t.Errorf("second Created = %d, want 0 (idempotency)", second.Created)
	}
	if second.Updated != 0 {
		t.Errorf("second Updated = %d, want 0", second.Updated)
	}
	if second.Unchanged != 2 {
		t.Errorf("second Unchanged = %d, want 2", second.Unchanged)
	}
	if embedCalls != 0 {
		t.Errorf("Embed should not be called on Unchanged path, got %d calls", embedCalls)
	}
}

func TestImportSkillsFromDir_DetectsContentChange(t *testing.T) {
	store := importTestStore(t)
	root := writeSampleSkillsTree(t)
	if _, err := ImportSkillsFromDir(store, SkillImportOpts{Dir: root, Embed: fakeEmbed}); err != nil {
		t.Fatalf("seed import: %v", err)
	}
	// Mutate alpha's body.
	writeFile(t, filepath.Join(root, "alpha", "SKILL.md"),
		"---\nname: alpha\ndescription: First skill v2.\n---\nNEW body\n")

	res, err := ImportSkillsFromDir(store, SkillImportOpts{Dir: root, Embed: fakeEmbed})
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if res.Updated != 1 {
		t.Errorf("Updated = %d, want 1", res.Updated)
	}
	if res.Unchanged != 1 {
		t.Errorf("Unchanged = %d, want 1", res.Unchanged)
	}
	alpha, _ := store.GetMemoryByID(SkillMemoryID("alpha"))
	if !strings.Contains(alpha.Content, "NEW body") {
		t.Errorf("alpha content not updated: %q", alpha.Content)
	}
}

func TestImportSkillsFromDir_DryRunMakesNoChange(t *testing.T) {
	store := importTestStore(t)
	root := writeSampleSkillsTree(t)

	res, err := ImportSkillsFromDir(store, SkillImportOpts{Dir: root, DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if res.Created != 2 {
		t.Errorf("dry run should report Created = 2, got %d", res.Created)
	}
	if got := store.MemoryCount(); got != 0 {
		t.Errorf("dry run wrote rows: count = %d", got)
	}
}

func TestImportSkillsFromDir_MissingDirIsSilent(t *testing.T) {
	store := importTestStore(t)
	res, err := ImportSkillsFromDir(store, SkillImportOpts{
		Dir:   filepath.Join(t.TempDir(), "does-not-exist"),
		Embed: fakeEmbed,
	})
	if err != nil {
		t.Errorf("missing dir should not error, got %v", err)
	}
	if res.Created != 0 || res.Updated != 0 {
		t.Errorf("missing dir should produce empty result: %+v", res)
	}
}

func TestImportSkillsFromDir_RequiresDir(t *testing.T) {
	store := importTestStore(t)
	if _, err := ImportSkillsFromDir(store, SkillImportOpts{Embed: fakeEmbed}); err == nil {
		t.Errorf("expected error for missing Dir")
	}
}

func TestImportSkillsFromDir_RequiresEmbedWhenNotDryRun(t *testing.T) {
	store := importTestStore(t)
	root := writeSampleSkillsTree(t)
	if _, err := ImportSkillsFromDir(store, SkillImportOpts{Dir: root}); err == nil {
		t.Errorf("expected error when Embed is nil and DryRun is false")
	}
}

func TestImportSkillsFromDir_EmbedErrorRecordedNotFatal(t *testing.T) {
	store := importTestStore(t)
	root := writeSampleSkillsTree(t)
	res, err := ImportSkillsFromDir(store, SkillImportOpts{
		Dir: root,
		Embed: func(string) ([]float32, error) {
			return nil, errors.New("boom")
		},
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(res.Errors) != 2 {
		t.Errorf("want 2 errors (one per skill), got %d: %v", len(res.Errors), res.Errors)
	}
	if res.Created != 0 {
		t.Errorf("Created should be 0 when embed fails, got %d", res.Created)
	}
}

func TestWriteSkillFile_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteSkillFile(SkillWriteOpts{
		Dir:         dir,
		Name:        "Test Skill",
		Description: "Wrote me to disk.",
		Body:        "Step one.\nStep two.",
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	want := filepath.Join(dir, "test-skill", "SKILL.md")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	got := string(data)
	if !strings.HasPrefix(got, "---\n") {
		t.Errorf("missing opening fence: %q", got)
	}
	if !strings.Contains(got, "name: Test Skill") {
		t.Errorf("missing name: %q", got)
	}
	if !strings.Contains(got, "description: Wrote me to disk.") {
		t.Errorf("missing description: %q", got)
	}
	if !strings.Contains(got, "Step one.") || !strings.Contains(got, "Step two.") {
		t.Errorf("missing body: %q", got)
	}
}

func TestWriteSkillFile_RefusesOverwriteByDefault(t *testing.T) {
	dir := t.TempDir()
	_, err := WriteSkillFile(SkillWriteOpts{Dir: dir, Name: "x", Body: "v1"})
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	_, err = WriteSkillFile(SkillWriteOpts{Dir: dir, Name: "x", Body: "v2"})
	if err == nil {
		t.Errorf("expected error on overwrite without Overwrite=true")
	}
	if !errors.Is(err, os.ErrExist) {
		t.Errorf("error should wrap os.ErrExist, got %v", err)
	}
}

func TestWriteSkillFile_OverwriteFlag(t *testing.T) {
	dir := t.TempDir()
	if _, err := WriteSkillFile(SkillWriteOpts{Dir: dir, Name: "x", Body: "v1"}); err != nil {
		t.Fatal(err)
	}
	path, err := WriteSkillFile(SkillWriteOpts{Dir: dir, Name: "x", Body: "v2", Overwrite: true})
	if err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "v2") {
		t.Errorf("overwrite did not replace body: %q", data)
	}
}

func TestWriteSkillFile_QuotesValuesWithSpecialChars(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteSkillFile(SkillWriteOpts{
		Dir:         dir,
		Name:        "with-colon",
		Description: "before: after",
		Body:        "body",
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	data, _ := os.ReadFile(path)
	got := string(data)
	if !strings.Contains(got, `description: "before: after"`) {
		t.Errorf("expected quoted description, got: %q", got)
	}
	// Round-trip parse must still recover the value.
	sf, err := ParseSkillFile(path)
	if err != nil {
		t.Fatalf("round-trip parse: %v", err)
	}
	if sf.Description != "before: after" {
		t.Errorf("round-trip description = %q", sf.Description)
	}
}

func TestWriteSkillFile_EmptyDirRejected(t *testing.T) {
	if _, err := WriteSkillFile(SkillWriteOpts{Name: "x"}); err == nil {
		t.Errorf("expected error for empty Dir")
	}
}

func TestCountDiskSkillFiles_Counts(t *testing.T) {
	root := writeSampleSkillsTree(t)
	got, err := CountDiskSkillFiles(root)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != 2 {
		t.Errorf("got %d, want 2 (.hidden and no-skill-md must not count)", got)
	}
}

func TestCountDiskSkillFiles_MissingDir(t *testing.T) {
	got, err := CountDiskSkillFiles(filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Errorf("missing dir should not error, got %v", err)
	}
	if got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}

func TestCountSyncedSkillMemories_AfterImport(t *testing.T) {
	store := importTestStore(t)
	root := writeSampleSkillsTree(t)
	if _, err := ImportSkillsFromDir(store, SkillImportOpts{Dir: root, Embed: fakeEmbed}); err != nil {
		t.Fatal(err)
	}
	got, err := CountSyncedSkillMemories(store)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != 2 {
		t.Errorf("got %d synced, want 2", got)
	}
}

func TestCountSyncedSkillMemories_NilStore(t *testing.T) {
	if _, err := CountSyncedSkillMemories(nil); err == nil {
		t.Errorf("expected error on nil store")
	}
}
