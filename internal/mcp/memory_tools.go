package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// deriveMemoryContextPath returns the Memory.ContextPath to persist for a
// heimdall_remember call. Priority:
//  1. explicit input.ContextPath (whitespace-trimmed, leading "./" stripped)
//  2. auto-detect from cwd: if cwd is inside an indexed project (findable
//     via heimdall.FindRepoRoot), use ComputeScope(cwd, root).
//  3. empty string — memory is stored project-global.
//
// Extracted as a pure function so the server-side tests can exercise
// resolution without touching embedding or store layers.
func deriveMemoryContextPath(explicit, cwd string) string {
	explicit = strings.TrimSpace(explicit)
	if explicit != "" {
		explicit = strings.TrimPrefix(explicit, "./")
		return explicit
	}
	if cwd == "" {
		return ""
	}
	root := heimdall.FindRepoRoot(cwd)
	if root == "" {
		return ""
	}
	return heimdall.ComputeScope(cwd, root)
}

func (s *Server) toolRemember(args json.RawMessage) MCPToolResult {
	var input rememberInput
	if err := json.Unmarshal(args, &input); err != nil {
		return ErrResult("invalid arguments: " + err.Error())
	}

	// Validate inputs
	if err := validateContent(input.Content); err != nil {
		return ErrResult(err.Error())
	}
	if err := validateTags(input.Tags); err != nil {
		return ErrResult(err.Error())
	}

	// Sanitize: strip null bytes
	input.Content = strings.ReplaceAll(input.Content, "\x00", "")

	// Auto-detect context_path: explicit input wins; otherwise derive from
	// the MCP server's CWD (the project Claude Code is running in) relative
	// to the nearest repo root. Enables later scope= filtering of memories
	// the same way code chunks are filtered.
	cwd, _ := os.Getwd()
	contextPath := deriveMemoryContextPath(input.ContextPath, cwd)

	// Validate memory type
	memType := heimdall.MemoryTypeFact
	if input.Type != "" {
		mt := heimdall.MemoryType(input.Type)
		if !heimdall.ValidMemoryTypes[mt] {
			return ErrResult(fmt.Sprintf("invalid memory type %q: must be one of preference, decision, fact, context, skill", input.Type))
		}
		memType = mt
	}

	// WriteFile is opt-in and only meaningful for type=skill. Reject
	// loud-misuse early so callers see the constraint instead of silently
	// having the flag ignored.
	if input.WriteFile && memType != heimdall.MemoryTypeSkill {
		return ErrResult("write_file=true is only valid when type=\"skill\"")
	}

	if s.MemoryStore == nil {
		return ErrResult("memory store not initialized")
	}

	hash := heimdall.ContentHash(input.Content)

	// Tier 1: Check for exact content hash duplicate
	existing, err := s.MemoryStore.GetMemoryByHash(hash)
	if err != nil {
		return ErrResult("memory lookup error: " + err.Error())
	}
	if existing != nil {
		// Update existing: merge tags, update type and timestamp. Preserve
		// the existing ContextPath unless the caller explicitly overrode it
		// (auto-detect only fills blanks — never overwrites).
		existing.Tags = heimdall.MergeTags(existing.Tags, input.Tags)
		existing.Type = memType
		existing.UpdatedAt = time.Now().Unix()
		if strings.TrimSpace(input.ContextPath) != "" {
			existing.ContextPath = contextPath
		} else if existing.ContextPath == "" && contextPath != "" {
			existing.ContextPath = contextPath
		}
		if err := s.MemoryStore.UpsertMemory(*existing); err != nil {
			return ErrResult("memory update error: " + err.Error())
		}
		payload := map[string]any{
			"status":  "updated",
			"id":      existing.ID,
			"message": "Existing memory updated (exact match).",
		}
		attachSkillFileWrite(payload, input, memType)
		out, _ := json.MarshalIndent(payload, "", "  ")
		return TextResult(string(out))
	}

	// Embed the content
	ctx := context.Background()
	client := heimdall.NewOllamaClient(s.Cfg.OllamaEndpoint)
	if err := client.Ping(ctx); err != nil {
		return ollamaSetupError(s.Cfg.OllamaEndpoint, s.Cfg.Model, err)
	}
	embedder := heimdall.NewOllamaEmbedder(client, s.Cfg.Model)

	vec, err := embedder.Embed(ctx, input.Content)
	if err != nil {
		return ErrResult("embedding error: " + err.Error())
	}

	// Tier 2: Semantic dedup
	similar, sim, err := s.MemoryStore.FindSimilarMemory(vec, 0.92)
	if err != nil {
		return ErrResult("similarity check error: " + err.Error())
	}
	if similar != nil {
		// Near-duplicate: explicit always overwrites
		if similar.Source == heimdall.MemorySourceSession {
			// Session -> explicit: overwrite entirely
			similar.Content = input.Content
			similar.Vector = vec
			similar.ContentHash = hash
		}
		similar.Tags = heimdall.MergeTags(similar.Tags, input.Tags)
		similar.Type = memType
		similar.Source = heimdall.MemorySourceExplicit
		similar.UpdatedAt = time.Now().Unix()
		// Preserve existing ContextPath; fill if empty. Explicit overrides.
		if strings.TrimSpace(input.ContextPath) != "" {
			similar.ContextPath = contextPath
		} else if similar.ContextPath == "" && contextPath != "" {
			similar.ContextPath = contextPath
		}
		if err := s.MemoryStore.UpsertMemory(*similar); err != nil {
			return ErrResult("memory update error: " + err.Error())
		}
		payload := map[string]any{
			"status":     "updated",
			"id":         similar.ID,
			"similarity": fmt.Sprintf("%.2f", sim),
			"message":    "Similar memory updated (semantic match).",
		}
		attachSkillFileWrite(payload, input, memType)
		out, _ := json.MarshalIndent(payload, "", "  ")
		return TextResult(string(out))
	}

	// New memory
	id := fmt.Sprintf("mem:explicit:%s", hash)
	now := time.Now().Unix()
	m := heimdall.Memory{
		ID:          id,
		Content:     input.Content,
		Type:        memType,
		Tags:        input.Tags,
		Project:     input.Project,
		ContextPath: contextPath,
		Vector:      vec,
		CreatedAt:   now,
		UpdatedAt:   now,
		Source:      heimdall.MemorySourceExplicit,
		ContentHash: hash,
	}
	if err := s.MemoryStore.UpsertMemory(m); err != nil {
		return ErrResult("memory store error: " + err.Error())
	}

	payload := map[string]any{
		"status":  "created",
		"id":      id,
		"type":    string(memType),
		"message": "Memory stored successfully.",
	}
	attachSkillFileWrite(payload, input, memType)
	out, _ := json.MarshalIndent(payload, "", "  ")
	return TextResult(string(out))
}

// attachSkillFileWrite, when input.WriteFile is true and Type=="skill",
// writes a SKILL.md to the configured Claude Code skills directory and
// records the result under the "skill_file" key in payload. Errors are
// captured under "skill_file_error" — they do NOT fail the memory store
// operation, which is the load-bearing side-effect for callers.
//
// This is the ONLY place the MCP server touches ~/.claude/skills/.
// The write is opt-in per call (default false), and overwrite refuses
// silently unless write_file_overwrite is also true.
func attachSkillFileWrite(payload map[string]any, input rememberInput, memType heimdall.MemoryType) {
	if !input.WriteFile || memType != heimdall.MemoryTypeSkill {
		return
	}
	skillsDir := heimdall.ResolveClaudeSkillsDir("", nil)
	if skillsDir == "" {
		payload["skill_file_error"] = "could not resolve skills directory; set HEIMDALL_CLAUDE_SKILLS_DIR or HOME"
		return
	}
	name := input.SkillName
	if name == "" {
		name = firstLine(input.Content)
	}
	desc := input.SkillDescription
	if desc == "" {
		desc = firstLine(input.Content)
	}
	body := input.Content
	path, err := heimdall.WriteSkillFile(heimdall.SkillWriteOpts{
		Dir:         skillsDir,
		Name:        name,
		Description: desc,
		Body:        body,
		Overwrite:   input.WriteFileOverwrite,
	})
	if err != nil {
		payload["skill_file_error"] = err.Error()
		if path != "" {
			payload["skill_file"] = path
		}
		return
	}
	payload["skill_file"] = path
}

// firstLine returns the first non-empty trimmed line of s, or "" if none.
// Used as a defensive fallback for the skill-file frontmatter when the
// caller doesn't pass an explicit skill_name / skill_description.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if t != "" {
			return t
		}
	}
	return ""
}

func (s *Server) toolRecall(args json.RawMessage) MCPToolResult {
	var input recallInput
	if err := json.Unmarshal(args, &input); err != nil {
		return ErrResult("invalid arguments: " + err.Error())
	}

	if err := validateQuery(input.Query); err != nil {
		return ErrResult(err.Error())
	}
	if err := validateTags(input.Tags); err != nil {
		return ErrResult(err.Error())
	}

	if s.MemoryStore == nil {
		return ErrResult("memory store not initialized")
	}

	ctx := context.Background()
	client := heimdall.NewOllamaClient(s.Cfg.OllamaEndpoint)
	if err := client.Ping(ctx); err != nil {
		return ollamaSetupError(s.Cfg.OllamaEndpoint, s.Cfg.Model, err)
	}
	embedder := heimdall.NewOllamaEmbedder(client, s.Cfg.Model)

	hits, err := heimdall.RunRecall(ctx, heimdall.RecallParams{
		Query:   input.Query,
		Type:    input.Type,
		Tags:    input.Tags,
		Project: input.Project,
		Limit:   input.Limit,
		Scope:   input.Scope,
	}, embedder, s.MemoryStore)
	if err != nil {
		return ErrResult(err.Error())
	}

	if len(hits) == 0 {
		return TextResult("No memories found matching your query.")
	}

	data, _ := json.MarshalIndent(hits, "", "  ")
	return TextResult(string(data))
}

func (s *Server) toolIngestSession(args json.RawMessage) MCPToolResult {
	var input ingestSessionInput
	if err := json.Unmarshal(args, &input); err != nil {
		return ErrResult("invalid arguments: " + err.Error())
	}

	if err := validateSummary(input.Summary); err != nil {
		return ErrResult(err.Error())
	}

	if s.MemoryStore == nil {
		return ErrResult("memory store not initialized")
	}

	ctx := context.Background()
	client := heimdall.NewOllamaClient(s.Cfg.OllamaEndpoint)
	if err := client.Ping(ctx); err != nil {
		return ollamaSetupError(s.Cfg.OllamaEndpoint, s.Cfg.Model, err)
	}
	embedder := heimdall.NewOllamaEmbedder(client, s.Cfg.Model)

	result, err := heimdall.IngestSessionSummary(ctx, input.Summary, input.Project, embedder, s.MemoryStore)
	if err != nil {
		return ErrResult("ingestion error: " + err.Error())
	}

	// Include memory stats
	stats := s.MemoryStore.MemoryStats()

	out, _ := json.MarshalIndent(map[string]any{
		"chunksProcessed": result.ChunksProcessed,
		"memoriesCreated": result.MemoriesCreated,
		"memoriesUpdated": result.MemoriesUpdated,
		"duplicates":      result.Duplicates,
		"tagsExtracted":   result.Tags,
		"totalMemories":   stats.TotalMemories,
	}, "", "  ")
	return TextResult(string(out))
}
