package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// seedSkillMemories inserts n skill-type memories into an isolated memory
// store and returns it. All skills share the same vector so they all rank
// identically under cosine similarity; their order after RunRecall is
// therefore store-insertion-order, which is deterministic.
func seedSkillMemories(t *testing.T, n int, vec []float32) *heimdall.MemoryStore {
	t.Helper()
	dir := t.TempDir()
	store, err := heimdall.OpenMemoryStore(filepath.Join(dir, "skills.db"))
	if err != nil {
		t.Fatalf("open mem: %v", err)
	}
	now := time.Now().Unix()
	for i := 0; i < n; i++ {
		content := fmt.Sprintf("skill %d: always run tests before committing", i+1)
		if err := store.UpsertMemory(heimdall.Memory{
			ID:          fmt.Sprintf("sk:%d", i+1),
			Content:     content,
			Type:        heimdall.MemoryTypeSkill,
			Vector:      vec,
			CreatedAt:   now,
			UpdatedAt:   now,
			Source:      heimdall.MemorySourceExplicit,
			ContentHash: heimdall.ContentHash(content),
		}); err != nil {
			t.Fatalf("upsert skill %d: %v", i, err)
		}
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// seedMixedMemories inserts `facts` fact-memories + `skills` skill-memories.
// Used to prove the type filter actually excludes non-skill rows.
func seedMixedMemories(t *testing.T, facts, skills int, vec []float32) *heimdall.MemoryStore {
	t.Helper()
	dir := t.TempDir()
	store, err := heimdall.OpenMemoryStore(filepath.Join(dir, "mixed.db"))
	if err != nil {
		t.Fatalf("open mem: %v", err)
	}
	now := time.Now().Unix()
	for i := 0; i < facts; i++ {
		c := fmt.Sprintf("fact %d: the sky is blue", i+1)
		_ = store.UpsertMemory(heimdall.Memory{
			ID: fmt.Sprintf("ft:%d", i+1), Content: c, Type: heimdall.MemoryTypeFact,
			Vector: vec, CreatedAt: now, UpdatedAt: now,
			Source: heimdall.MemorySourceExplicit, ContentHash: heimdall.ContentHash(c),
		})
	}
	for i := 0; i < skills; i++ {
		c := fmt.Sprintf("skill %d: run gofmt on save", i+1)
		_ = store.UpsertMemory(heimdall.Memory{
			ID: fmt.Sprintf("sk:%d", i+1), Content: c, Type: heimdall.MemoryTypeSkill,
			Vector: vec, CreatedAt: now, UpdatedAt: now,
			Source: heimdall.MemorySourceExplicit, ContentHash: heimdall.ContentHash(c),
		})
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// seedChunkedSkill inserts one "head" skill id plus N-1 `:part-<n>` chunk
// rows for the same logical skill, mirroring what
// internal/heimdall/skills_sync.go ChunkSkillBody produces when a SKILL.md
// exceeds SkillBodyChunkThreshold.
func seedChunkedSkill(t *testing.T, slug string, parts int, vec []float32) *heimdall.MemoryStore {
	t.Helper()
	dir := t.TempDir()
	store, err := heimdall.OpenMemoryStore(filepath.Join(dir, "chunked.db"))
	if err != nil {
		t.Fatalf("open mem: %v", err)
	}
	now := time.Now().Unix()
	head := fmt.Sprintf("skill: %s\ndescription: chunked skill\n\nbody part 1", slug)
	_ = store.UpsertMemory(heimdall.Memory{
		ID: fmt.Sprintf("mem:skill:disk:%s", slug), Content: head, Type: heimdall.MemoryTypeSkill,
		Vector: vec, CreatedAt: now, UpdatedAt: now,
		Source: heimdall.MemorySourceExplicit, ContentHash: heimdall.ContentHash(head),
	})
	for i := 2; i <= parts; i++ {
		c := fmt.Sprintf("skill: %s\ndescription: chunked skill\n\nbody part %d", slug, i)
		_ = store.UpsertMemory(heimdall.Memory{
			ID: fmt.Sprintf("mem:skill:disk:%s:part-%d", slug, i), Content: c, Type: heimdall.MemoryTypeSkill,
			Vector: vec, CreatedAt: now, UpdatedAt: now,
			Source: heimdall.MemorySourceExplicit, ContentHash: heimdall.ContentHash(c),
		})
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// stubSkillEmbedder returns a fixed vector for every Embed call. Used so
// the skills helper doesn't need a real Ollama to score similarity.
type stubSkillEmbedder struct {
	vec []float32
	err error
}

func (s *stubSkillEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.vec, nil
}

// -----------------------------------------------------------------------------
// 1. Pure helper — no skills in store → nil.
// -----------------------------------------------------------------------------

func TestSurfaceRelevantSkills_NoSkillsReturnsNil(t *testing.T) {
	store := seedSkillMemories(t, 0, []float32{1, 0, 0})
	got := surfaceRelevantSkills(context.Background(), "anything", 3,
		&stubSkillEmbedder{vec: []float32{1, 0, 0}}, store)
	if got != nil {
		t.Errorf("want nil bullets, got %v", got)
	}
}

// -----------------------------------------------------------------------------
// 2. Pure helper — skills present → top-N bullets returned, length capped.
// -----------------------------------------------------------------------------

func TestSurfaceRelevantSkills_ReturnsTopN(t *testing.T) {
	store := seedSkillMemories(t, 5, []float32{1, 0, 0})
	got := surfaceRelevantSkills(context.Background(), "testing workflow", 3,
		&stubSkillEmbedder{vec: []float32{1, 0, 0}}, store)
	if len(got) != 3 {
		t.Fatalf("want 3 bullets, got %d: %v", len(got), got)
	}
	for _, b := range got {
		if b == "" {
			t.Errorf("empty bullet in result")
		}
		// Skills content is one line after singleLine; bullet is safe to embed.
		if strings.ContainsAny(b, "\n\r") {
			t.Errorf("bullet contains newline: %q", b)
		}
	}
}

// -----------------------------------------------------------------------------
// 3. Pure helper — type filter excludes non-skill memories.
// -----------------------------------------------------------------------------

func TestSurfaceRelevantSkills_OnlyReturnsSkillType(t *testing.T) {
	store := seedMixedMemories(t, 4 /*facts*/, 2 /*skills*/, []float32{1, 0, 0})
	got := surfaceRelevantSkills(context.Background(), "anything", 5,
		&stubSkillEmbedder{vec: []float32{1, 0, 0}}, store)
	if len(got) != 2 {
		t.Fatalf("want 2 skill bullets (facts excluded), got %d: %v", len(got), got)
	}
	for _, b := range got {
		if !strings.Contains(b, "skill") {
			t.Errorf("bullet %q looks like a non-skill memory", b)
		}
	}
}

// -----------------------------------------------------------------------------
// 4. Pure helper — cancelled ctx → nil (no DB hit, no bullets).
// -----------------------------------------------------------------------------

func TestSurfaceRelevantSkills_CancelledCtxReturnsNil(t *testing.T) {
	store := seedSkillMemories(t, 5, []float32{1, 0, 0})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := surfaceRelevantSkills(ctx, "anything", 3,
		&stubSkillEmbedder{vec: []float32{1, 0, 0}}, store)
	if got != nil {
		t.Errorf("want nil on cancelled ctx, got %v", got)
	}
}

// -----------------------------------------------------------------------------
// 5. Pure helper — embedder error → nil, no panic, section omitted.
// -----------------------------------------------------------------------------

func TestSurfaceRelevantSkills_EmbedderErrorReturnsNil(t *testing.T) {
	store := seedSkillMemories(t, 3, []float32{1, 0, 0})
	got := surfaceRelevantSkills(context.Background(), "anything", 3,
		&stubSkillEmbedder{err: fmt.Errorf("embed blew up")}, store)
	if got != nil {
		t.Errorf("want nil on embed error, got %v", got)
	}
}

// -----------------------------------------------------------------------------
// 6. Pure helper — nil store / nil embedder / empty query → nil.
// -----------------------------------------------------------------------------

func TestSurfaceRelevantSkills_DefensiveNilHandling(t *testing.T) {
	store := seedSkillMemories(t, 3, []float32{1, 0, 0})
	if got := surfaceRelevantSkills(context.Background(), "q", 3, nil, store); got != nil {
		t.Errorf("nil embedder should return nil, got %v", got)
	}
	if got := surfaceRelevantSkills(context.Background(), "q", 3, &stubSkillEmbedder{vec: []float32{1, 0, 0}}, nil); got != nil {
		t.Errorf("nil store should return nil, got %v", got)
	}
	if got := surfaceRelevantSkills(context.Background(), "   ", 3, &stubSkillEmbedder{vec: []float32{1, 0, 0}}, store); got != nil {
		t.Errorf("blank query should return nil, got %v", got)
	}
}

// -----------------------------------------------------------------------------
// 7. appendSkillsSection renders the markdown with header + bullets.
// -----------------------------------------------------------------------------

func TestAppendSkillsSection_RendersSectionWhenBullets(t *testing.T) {
	var b strings.Builder
	appendSkillsSection(&b, []string{"first skill", "second skill"})
	got := b.String()
	if !strings.Contains(got, "### Relevant skills") {
		t.Errorf("missing header in %q", got)
	}
	if !strings.Contains(got, "- first skill") || !strings.Contains(got, "- second skill") {
		t.Errorf("missing bullets in %q", got)
	}
}

// -----------------------------------------------------------------------------
// 8. appendSkillsSection with empty bullets writes nothing.
// -----------------------------------------------------------------------------

func TestAppendSkillsSection_EmptyBulletsWritesNothing(t *testing.T) {
	var b strings.Builder
	appendSkillsSection(&b, nil)
	if b.Len() != 0 {
		t.Errorf("wrote %q, want nothing", b.String())
	}
	appendSkillsSection(&b, []string{})
	if b.Len() != 0 {
		t.Errorf("wrote %q on empty slice, want nothing", b.String())
	}
}

// -----------------------------------------------------------------------------
// 9. insertSkillsSection splices before the footer.
// -----------------------------------------------------------------------------

func TestInsertSkillsSection_SplicesBeforeFooter(t *testing.T) {
	body := "## Heimdall context\n\nsome stuff\n\n### Relevant code\n- a.go:1-10 — foo\n\n_retrieved via heimdall-mcp_\n"
	got := insertSkillsSection(body, []string{"the skill"})
	if !strings.Contains(got, "### Relevant skills") {
		t.Errorf("missing skills section: %q", got)
	}
	skillsIdx := strings.Index(got, "### Relevant skills")
	footerIdx := strings.Index(got, "_retrieved via heimdall-mcp_")
	if skillsIdx < 0 || footerIdx < 0 || skillsIdx >= footerIdx {
		t.Errorf("skills section should precede footer; skillsIdx=%d footerIdx=%d\n%s", skillsIdx, footerIdx, got)
	}
	codeIdx := strings.Index(got, "### Relevant code")
	if codeIdx < 0 || codeIdx >= skillsIdx {
		t.Errorf("Relevant code should precede Relevant skills; codeIdx=%d skillsIdx=%d", codeIdx, skillsIdx)
	}
}

// -----------------------------------------------------------------------------
// 10. insertSkillsSection with no bullets returns body unchanged.
// -----------------------------------------------------------------------------

func TestInsertSkillsSection_NoBulletsUnchanged(t *testing.T) {
	body := "## foo\n\n_retrieved via heimdall-mcp_\n"
	if got := insertSkillsSection(body, nil); got != body {
		t.Errorf("nil bullets must leave body unchanged; got %q", got)
	}
	if got := insertSkillsSection(body, []string{}); got != body {
		t.Errorf("empty bullets must leave body unchanged; got %q", got)
	}
}

// -----------------------------------------------------------------------------
// 11. HookSessionStart renders "### Relevant skills" when the memory store
//     has skills matching the project query.
// -----------------------------------------------------------------------------

func TestHookSessionStart_RendersSkillsSection(t *testing.T) {
	const model = "test-model"
	const dim = 3

	fake := newFakeOllama(t, model, dim)
	cfg := config.Config{OllamaEndpoint: fake.server.URL, Model: model}

	projectRoot := t.TempDir()
	seedVectorStore(t, projectRoot, model, dim, time.Now().Unix())

	// Build a memory store that has both Fact (for "Recent memories" section)
	// and Skill (for the new "Relevant skills" section) rows. Same vector
	// means RunRecall + the skills helper both find them on any query.
	mem := seedMixedMemories(t, 3 /*facts*/, 2 /*skills*/, fake.embedVec)

	stdout, stderr, code := runHookSessionStart(t, runOpts{
		stdin:       fmt.Sprintf(`{"cwd":%q}`, projectRoot),
		cfg:         cfg,
		memoryStore: mem,
	})
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if stderr != "" {
		t.Errorf("stderr should be empty, got %q", stderr)
	}
	if !strings.Contains(stdout, "### Relevant skills") {
		t.Errorf("expected skills section, got:\n%s", stdout)
	}
	// The fact bullets still render under Recent memories.
	if !strings.Contains(stdout, "### Recent memories") {
		t.Errorf("expected memories section, got:\n%s", stdout)
	}
	// Section ordering: memories before skills before footer.
	mem1 := strings.Index(stdout, "### Recent memories")
	sk1 := strings.Index(stdout, "### Relevant skills")
	footer := strings.Index(stdout, "_retrieved via heimdall-mcp_")
	if !(mem1 < sk1 && sk1 < footer) {
		t.Errorf("section ordering wrong: memories=%d skills=%d footer=%d\n%s",
			mem1, sk1, footer, stdout)
	}
}

// -----------------------------------------------------------------------------
// 12. HookSessionStart omits the skills section when no skill memories exist.
// -----------------------------------------------------------------------------

func TestHookSessionStart_NoSkillsOmitsSection(t *testing.T) {
	const model = "test-model"
	const dim = 3
	fake := newFakeOllama(t, model, dim)
	cfg := config.Config{OllamaEndpoint: fake.server.URL, Model: model}

	projectRoot := t.TempDir()
	seedVectorStore(t, projectRoot, model, dim, time.Now().Unix())

	// Only fact memories, no skills → the section must be omitted entirely.
	mem := seedMixedMemories(t, 3, 0, fake.embedVec)

	stdout, _, code := runHookSessionStart(t, runOpts{
		stdin:       fmt.Sprintf(`{"cwd":%q}`, projectRoot),
		cfg:         cfg,
		memoryStore: mem,
	})
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if strings.Contains(stdout, "### Relevant skills") {
		t.Errorf("should not render empty skills section, got:\n%s", stdout)
	}
	// Recent memories still renders.
	if !strings.Contains(stdout, "### Recent memories") {
		t.Errorf("expected memories section, got:\n%s", stdout)
	}
}

// -----------------------------------------------------------------------------
// 13. Budget timeout before the skill query → hook still emits the main body
//     (or empty on strict budget), skills section simply not present. The key
//     invariant: no panic, exit 0, and the skills path never stalls the hook.
// -----------------------------------------------------------------------------

func TestHookSessionStart_SkillsSkippedOnBudgetTimeout(t *testing.T) {
	const model = "test-model"
	const dim = 3
	fake := newFakeOllama(t, model, dim)
	// A slow embed forces the ctx to fire before the recall completes, which
	// means surfaceRelevantSkills sees a cancelled ctx and returns nil.
	fake.embedDelay = 500 * time.Millisecond
	cfg := config.Config{OllamaEndpoint: fake.server.URL, Model: model}

	projectRoot := t.TempDir()
	seedVectorStore(t, projectRoot, model, dim, time.Now().Unix())
	mem := seedSkillMemories(t, 3, fake.embedVec)

	stdout, stderr, code := runHookSessionStart(t, runOpts{
		stdin:       fmt.Sprintf(`{"cwd":%q}`, projectRoot),
		args:        []string{"--budget-ms=40"},
		cfg:         cfg,
		memoryStore: mem,
	})
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if stderr != "" {
		t.Errorf("stderr must stay empty on retrieval path, got %q", stderr)
	}
	// Either the header-only body rendered (no bullets, no skills) or the
	// full block bailed before write. Critically: the skills section must
	// NOT appear — it would imply we ran the skill query after the budget
	// had already fired.
	if strings.Contains(stdout, "### Relevant skills") {
		t.Errorf("skills should be skipped when ctx deadline fired, got:\n%s", stdout)
	}
}

// -----------------------------------------------------------------------------
// 14. HookUserPrompt renders "### Relevant skills" section on the happy path.
// -----------------------------------------------------------------------------

func TestHookUserPrompt_RendersSkillsSection(t *testing.T) {
	const model = "test-model"
	const dim = 3
	project := t.TempDir()
	baseDir := seedVectorStore(t, project, model, dim, time.Now().Unix())
	dbDir := heimdall.ModelDBDir(baseDir, model)
	recs := []heimdall.VectorRecord{
		{ID: "1", FilePath: "a.go", StartLine: 1, EndLine: 10, Content: "package a", Embedding: []float32{1, 0, 0}},
	}
	seedStoreWithRecords(t, dbDir, recs)
	fake := newFakeOllama(t, model, dim)
	mem := seedSkillMemories(t, 2, fake.embedVec)

	out, _, code := runHookUserPrompt(t, runUPOpts{
		stdin:       `{"prompt":"explain the post-edit flow","cwd":"` + project + `"}`,
		cfg:         baseCfg(fake.server.URL, model),
		memoryStore: mem,
	})
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if !strings.Contains(out, "### Relevant skills") {
		t.Errorf("expected skills section, got: %q", out)
	}
	// Relevant code comes from the vector search; must remain present.
	if !strings.Contains(out, "### Relevant code") {
		t.Errorf("expected relevant code section alongside skills: %q", out)
	}
	codeIdx := strings.Index(out, "### Relevant code")
	skIdx := strings.Index(out, "### Relevant skills")
	footerIdx := strings.Index(out, "_retrieved via heimdall-mcp_")
	if !(codeIdx < skIdx && skIdx < footerIdx) {
		t.Errorf("ordering: code=%d skills=%d footer=%d\n%s", codeIdx, skIdx, footerIdx, out)
	}
}

// -----------------------------------------------------------------------------
// 15. HookUserPrompt — no skills → section omitted.
// -----------------------------------------------------------------------------

func TestHookUserPrompt_NoSkillsOmitsSection(t *testing.T) {
	const model = "test-model"
	const dim = 3
	project := t.TempDir()
	baseDir := seedVectorStore(t, project, model, dim, time.Now().Unix())
	dbDir := heimdall.ModelDBDir(baseDir, model)
	seedStoreWithRecords(t, dbDir, []heimdall.VectorRecord{
		{ID: "1", FilePath: "a.go", StartLine: 1, EndLine: 10, Content: "package a", Embedding: []float32{1, 0, 0}},
	})
	fake := newFakeOllama(t, model, dim)
	// No skill memories; only facts.
	mem := seedMixedMemories(t, 3, 0, fake.embedVec)

	out, _, code := runHookUserPrompt(t, runUPOpts{
		stdin:       `{"prompt":"explain the post-edit flow","cwd":"` + project + `"}`,
		cfg:         baseCfg(fake.server.URL, model),
		memoryStore: mem,
	})
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if strings.Contains(out, "### Relevant skills") {
		t.Errorf("should not render empty skills section, got: %q", out)
	}
	if !strings.Contains(out, "### Relevant code") {
		t.Errorf("relevant code section still required: %q", out)
	}
}

// -----------------------------------------------------------------------------
// 16. HookUserPrompt — on budget-timeout during the search/skills step, the
//     skills section must NOT appear, and the hook still exits 0.
// -----------------------------------------------------------------------------

func TestHookUserPrompt_SkillsSkippedOnBudgetTimeout(t *testing.T) {
	const model = "test-model"
	const dim = 3
	project := t.TempDir()
	_ = seedVectorStore(t, project, model, dim, time.Now().Unix())
	fake := newFakeOllama(t, model, dim)
	fake.embedDelay = 500 * time.Millisecond
	mem := seedSkillMemories(t, 3, fake.embedVec)

	out, _, code := runHookUserPrompt(t, runUPOpts{
		stdin:       `{"prompt":"explain the post-edit flow","cwd":"` + project + `"}`,
		args:        []string{"--budget-ms", "40"},
		cfg:         baseCfg(fake.server.URL, model),
		suppress:    func(string, string, time.Duration) bool { return false },
		memoryStore: mem,
	})
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if strings.Contains(out, "### Relevant skills") {
		t.Errorf("skills section must not render after budget timeout: %q", out)
	}
}

// -----------------------------------------------------------------------------
// Chunked-skill dedup (part-* rows from ChunkSkillBody).
// -----------------------------------------------------------------------------

func TestBaseSkillID(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"mem:skill:disk:deep-code-review", "mem:skill:disk:deep-code-review"},
		{"mem:skill:disk:deep-code-review:part-2", "mem:skill:disk:deep-code-review"},
		{"mem:skill:disk:deep-code-review:part-10", "mem:skill:disk:deep-code-review"},
		{"mem:skill:disk:foo:part-", "mem:skill:disk:foo:part-"},          // empty suffix — not a chunk
		{"mem:skill:disk:foo:part-abc", "mem:skill:disk:foo:part-abc"},    // non-digit suffix
		{"mem:skill:disk:foo:part-2a", "mem:skill:disk:foo:part-2a"},      // mixed → not a chunk
		{"mem:fact:part-1", "mem:fact"},                                   // suffix match works anywhere
		{"", ""},
	}
	for _, c := range cases {
		if got := baseSkillID(c.in); got != c.want {
			t.Errorf("baseSkillID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSurfaceRelevantSkills_DedupsPartChunks(t *testing.T) {
	store := seedChunkedSkill(t, "deep-code-review", 4, []float32{1, 0, 0})
	got := surfaceRelevantSkills(context.Background(), "review quality", 3,
		&stubSkillEmbedder{vec: []float32{1, 0, 0}}, store)
	if len(got) != 1 {
		t.Fatalf("want 1 bullet (4 chunks of 1 skill collapse), got %d: %v", len(got), got)
	}
	// All chunks share the same `skill: deep-code-review` header first line,
	// so the single surviving bullet should reference the slug.
	if !strings.Contains(got[0], "deep-code-review") {
		t.Errorf("dedup bullet missing skill slug: %q", got[0])
	}
}

func TestSurfaceRelevantSkills_DedupAcrossMultipleChunkedSkills(t *testing.T) {
	dir := t.TempDir()
	store, err := heimdall.OpenMemoryStore(filepath.Join(dir, "multi.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	vec := []float32{1, 0, 0}
	now := time.Now().Unix()
	for _, slug := range []string{"skill-a", "skill-b"} {
		for part := 1; part <= 3; part++ {
			id := fmt.Sprintf("mem:skill:disk:%s", slug)
			if part > 1 {
				id = fmt.Sprintf("%s:part-%d", id, part)
			}
			c := fmt.Sprintf("skill: %s\ndescription: bullet-%s\n\npart %d", slug, slug, part)
			_ = store.UpsertMemory(heimdall.Memory{
				ID: id, Content: c, Type: heimdall.MemoryTypeSkill, Vector: vec,
				CreatedAt: now, UpdatedAt: now,
				Source: heimdall.MemorySourceExplicit, ContentHash: heimdall.ContentHash(c),
			})
		}
	}
	got := surfaceRelevantSkills(context.Background(), "x", 3,
		&stubSkillEmbedder{vec: vec}, store)
	if len(got) != 2 {
		t.Fatalf("want 2 deduped bullets, got %d: %v", len(got), got)
	}
}
