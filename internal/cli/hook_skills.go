package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// skillsTopNDefault is the cap on how many top-ranked skill memories each
// retrieval hook surfaces. Three is a deliberate pick:
//   - one skill is too narrow and trains the model to over-weight it;
//   - five or more crowds out the Recent memories / Relevant code sections
//     under the ~1500-token hook budget (plan §3.1, §3.2);
//   - three matches the "small bullet list" shape of the existing sections
//     so Claude treats them as peers, not as a headline.
//
// Adjustable via the topN arg on surfaceRelevantSkills, but the two hook
// call sites both pass this default.
const skillsTopNDefault = 3

// skillsMaxBulletRunes bounds the rendered length of a single skill bullet.
// Matches the per-hit formatting used in formatSearchHookMD's Relevant code
// section (120 chars). Long procedures get a "..." suffix.
const skillsMaxBulletRunes = 120

// surfaceRelevantSkills queries the memory store for the top-N skill
// memories (type=skill) ranked by semantic similarity to query, and returns
// them as pre-formatted single-line bullets suitable for a markdown "- %s"
// list. Empty return → no skills matched; the caller should omit the
// section header entirely.
//
// Failure modes all collapse to "return nil": store nil, embed error,
// memory store error, context cancelled, no results. Skills are additive
// context — a failure here must never block the rest of the hook output.
// The caller has already validated budget via ctx; we re-check ctx.Err()
// before the DB round-trip as a cheap defensive guard.
func surfaceRelevantSkills(ctx context.Context, query string, topN int, embedder heimdall.Embedder, memStore *heimdall.MemoryStore) []string {
	if memStore == nil || embedder == nil {
		return nil
	}
	if topN <= 0 {
		topN = skillsTopNDefault
	}
	if strings.TrimSpace(query) == "" {
		// A blank query would produce a random ranking — better to skip.
		return nil
	}
	if err := ctx.Err(); err != nil {
		return nil
	}

	// Over-fetch by a factor to compensate for part-* chunks of the same
	// skill collapsing to one bullet below. 5× topN covers skills split into
	// up to ~5 chunks (deep-code-review currently splits into 4), with
	// headroom. The memory store is bounded so over-fetch is cheap.
	hits, err := heimdall.RunRecall(ctx, heimdall.RecallParams{
		Query: query,
		Type:  string(heimdall.MemoryTypeSkill),
		Limit: topN * 5,
	}, embedder, memStore)
	if err != nil || len(hits) == 0 {
		return nil
	}

	bullets := make([]string, 0, topN)
	seen := make(map[string]struct{}, topN)
	for _, h := range hits {
		base := baseSkillID(h.ID)
		if _, dup := seen[base]; dup {
			continue
		}
		seen[base] = struct{}{}
		line := singleLine(h.Content)
		if line == "" {
			continue
		}
		line = capRunes(line, skillsMaxBulletRunes)
		bullets = append(bullets, line)
		if len(bullets) >= topN {
			break
		}
	}
	return bullets
}

// baseSkillID strips a trailing `:part-<digits>` suffix from a chunked skill
// memory id so that every chunk of the same disk-sourced skill collapses to
// one dedup key. Ids without the suffix (non-chunked skills, unrelated
// memories) are returned unchanged. A suffix with non-digit content (e.g.
// `:part-a`) is treated as not-a-chunk and left intact — the importer only
// produces numeric suffixes (see ChunkSkillBody in internal/heimdall/skills_sync.go).
func baseSkillID(id string) string {
	const marker = ":part-"
	idx := strings.LastIndex(id, marker)
	if idx < 0 {
		return id
	}
	suffix := id[idx+len(marker):]
	if suffix == "" {
		return id
	}
	for _, r := range suffix {
		if r < '0' || r > '9' {
			return id
		}
	}
	return id[:idx]
}

// appendSkillsSection writes the "### Relevant skills" markdown block to b
// if bullets is non-empty, otherwise writes nothing. Intended to be called
// after the main body is built and before the trailing "_retrieved via
// heimdall-mcp_" footer, matching the shape of formatSessionStartBlock's
// "### Recent memories" and formatSearchHookMD's "### Relevant code".
func appendSkillsSection(b *strings.Builder, bullets []string) {
	if len(bullets) == 0 {
		return
	}
	b.WriteString("\n### Relevant skills\n")
	for _, s := range bullets {
		if s == "" {
			continue
		}
		fmt.Fprintf(b, "- %s\n", s)
	}
}

// insertSkillsSection splices the "### Relevant skills" block into an
// already-rendered body, just before the trailing footer line
// "_retrieved via heimdall-mcp_". If bullets is empty or the footer is
// absent, body is returned unchanged. This keeps formatSearchHookMD (which
// renders the whole body in one shot) separate from the skills query path.
func insertSkillsSection(body string, bullets []string) string {
	if len(bullets) == 0 {
		return body
	}
	const footer = "\n_retrieved via heimdall-mcp_\n"
	idx := strings.LastIndex(body, footer)
	if idx < 0 {
		// No footer to splice before — append at end. Callers should have
		// emitted a formatSessionStartBlock / formatSearchHookMD body so
		// this branch is unreachable in practice, but keeping it safe.
		var b strings.Builder
		b.WriteString(body)
		appendSkillsSection(&b, bullets)
		return b.String()
	}
	var b strings.Builder
	b.Grow(len(body) + 32 + 80*len(bullets))
	b.WriteString(body[:idx])
	appendSkillsSection(&b, bullets)
	b.WriteString(body[idx:])
	return b.String()
}
