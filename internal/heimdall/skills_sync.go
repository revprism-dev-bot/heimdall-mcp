// skills_sync.go — disk <-> heimdall sync helpers for Claude Code skills.
//
// Claude Code "skills" are markdown files at <skills-dir>/<name>/SKILL.md
// with a YAML frontmatter block (`---` ... `---`) containing at least
// `name` and `description`. They are always-loaded static instructions for
// Claude Code. Heimdall mirrors them as `MemoryType=skill` rows so they're
// semantically searchable and surfaced by the SessionStart / UserPromptSubmit
// hooks alongside other skill memories (PR #15).
//
// Two directions:
//
//   * inbound  (disk → heimdall): ImportSkillsFromDir walks a skills root,
//     parses every SKILL.md, and upserts a memory per skill. The memory ID is
//     deterministic ("mem:skill:disk:<slug>") so re-imports update the same
//     row instead of creating duplicates.
//
//   * outbound (heimdall → disk): WriteSkillFile materializes a skill memory
//     to <skills-dir>/<slug>/SKILL.md. Opt-in only — never called unless the
//     remember caller passes WriteFile=true. Default OFF.
//
// The frontmatter parser is intentionally minimal: top-level `key: value`
// pairs only, no nested maps, no arrays. This matches every SKILL.md shape
// Claude Code documents today and avoids pulling a YAML dependency into the
// build for one feature.
package heimdall

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ClaudeSkillsDirEnv is the env var that overrides the default
// `~/.claude/skills` location. Tests MUST set this so they never touch a
// real user's skills directory.
const ClaudeSkillsDirEnv = "HEIMDALL_CLAUDE_SKILLS_DIR"

// SkillFile is the parsed content of a SKILL.md file on disk.
type SkillFile struct {
	// Path is the absolute path to the SKILL.md file.
	Path string
	// Dir is the absolute path to the skill's containing directory
	// (the parent of Path). The directory base name is the disk slug.
	Dir string
	// Name is the `name:` value from the frontmatter. If the frontmatter
	// omits it, falls back to filepath.Base(Dir).
	Name string
	// Description is the `description:` value from the frontmatter. May
	// be empty.
	Description string
	// Body is everything after the closing `---` line, with leading and
	// trailing whitespace trimmed.
	Body string
	// Frontmatter holds every parsed top-level key. Used by writers that
	// want to round-trip extra fields (e.g. `model:`, `version:`).
	Frontmatter map[string]string
}

// frontmatterDelim is the YAML frontmatter fence per Claude Code skill docs.
const frontmatterDelim = "---"

// frontmatterKey matches `key: value` lines inside the frontmatter block.
// Captures (key, value) — value is whatever follows the first colon, with
// surrounding whitespace trimmed by the caller. Quoted values keep their
// quotes; the caller strips them when present.
var frontmatterKey = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_-]*)\s*:\s*(.*)$`)

// ParseSkillFile reads a SKILL.md file from disk and returns its parsed
// representation. Returns an error if the file is unreadable or the
// frontmatter block is malformed (missing closing fence). A frontmatter that
// is entirely absent is tolerated — Body becomes the whole file, Name falls
// back to the directory base name, Description is empty.
func ParseSkillFile(path string) (*SkillFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	dir := filepath.Dir(abs)

	sf := &SkillFile{
		Path:        abs,
		Dir:         dir,
		Frontmatter: map[string]string{},
	}

	scanner := bufio.NewScanner(f)
	// SKILL.md files can be large; bump the buffer so a generous body
	// (steps, examples) doesn't trigger bufio.ErrTooLong.
	const maxLine = 1 << 20 // 1 MiB per line
	scanner.Buffer(make([]byte, 64*1024), maxLine)

	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	bodyStart := 0
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == frontmatterDelim {
		// Find closing fence.
		closeIdx := -1
		for i := 1; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == frontmatterDelim {
				closeIdx = i
				break
			}
		}
		if closeIdx < 0 {
			return nil, errors.New("malformed frontmatter: no closing '---' fence")
		}
		for i := 1; i < closeIdx; i++ {
			line := lines[i]
			// Skip blank lines and comments inside the frontmatter.
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			m := frontmatterKey.FindStringSubmatch(line)
			if m == nil {
				continue // ignore unknown shapes (lists/maps)
			}
			key := strings.TrimSpace(m[1])
			val := strings.TrimSpace(m[2])
			val = stripQuotes(val)
			sf.Frontmatter[key] = val
		}
		bodyStart = closeIdx + 1
	}

	if bodyStart < len(lines) {
		body := strings.Join(lines[bodyStart:], "\n")
		sf.Body = strings.TrimSpace(body)
	}

	sf.Name = sf.Frontmatter["name"]
	if sf.Name == "" {
		sf.Name = filepath.Base(dir)
	}
	sf.Description = sf.Frontmatter["description"]

	return sf, nil
}

// stripQuotes removes a single matching pair of surrounding double or single
// quotes from s. Mirrors how minimal YAML parsers treat a flow-scalar string.
func stripQuotes(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// ResolveClaudeSkillsDir returns the directory to scan for skills, resolved
// in this order:
//
//  1. explicit override arg (non-empty wins, used by `--dir <path>`)
//  2. HEIMDALL_CLAUDE_SKILLS_DIR env var (test isolation)
//  3. <home>/.claude/skills
//
// If the home directory cannot be resolved and no override is given, returns
// "" — callers should treat that as "no source available, skip silently".
func ResolveClaudeSkillsDir(override string, env map[string]string) string {
	if override != "" {
		return override
	}
	if env != nil {
		if v := env[ClaudeSkillsDirEnv]; v != "" {
			return v
		}
	}
	// Fall back to os.Getenv so non-CLI callers without an env map still work.
	if v := os.Getenv(ClaudeSkillsDirEnv); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".claude", "skills")
}

// SlugifySkill turns a skill name into a directory-safe slug. Lowercases,
// replaces non-alphanumeric runs with `-`, trims leading/trailing dashes,
// and collapses repeated dashes. Empty input returns "skill" so the
// outbound writer never produces an empty directory name.
func SlugifySkill(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	prevDash := false
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "skill"
	}
	return out
}

// SkillMemoryID returns the deterministic memory ID for a skill imported
// from disk. Re-imports of the same disk skill always land on the same row.
func SkillMemoryID(slug string) string {
	return fmt.Sprintf("mem:skill:disk:%s", slug)
}

// SkillMemoryIDPartPrefix is the prefix used by chunked skill parts
// beyond the first. Example: `mem:skill:disk:<slug>:part-2`. Kept here
// so callers (importer, doctor, tests) share the same constant.
const SkillMemoryIDPartPrefix = ":part-"

// SkillMemoryIDForPart returns the deterministic ID for the n-th part of
// a chunked skill. `n` is 1-based for clarity in stored IDs (part-2 is the
// second chunk; the first chunk uses the non-suffixed SkillMemoryID).
func SkillMemoryIDForPart(slug string, n int) string {
	return fmt.Sprintf("%s%s%d", SkillMemoryID(slug), SkillMemoryIDPartPrefix, n)
}

// SkillBodyChunkThreshold is the maximum body length (in runes) stored in
// a single skill memory row. Bodies exceeding this are split across
// multiple part-N rows by ChunkSkillBody. The threshold is chosen well
// under `nomic-embed-text`'s 8192-token context window (~24KB at 3
// chars/token) so the header + body of every chunk fits with headroom.
const SkillBodyChunkThreshold = 6000

// ChunkSkillBody splits a skill body into substrings, each at most
// maxChars runes long. It prefers to split on H2 (`## `) boundaries, then
// H3 (`### `), then blank-line paragraph breaks, then a hard character
// split as a last resort. The returned slice always has length ≥ 1; an
// empty or whitespace-only body returns `[""]` so the caller can still
// emit one row. Splits preserve markdown formatting on both sides of the
// boundary (the delimiter stays with the chunk that starts with it).
func ChunkSkillBody(body string, maxChars int) []string {
	if maxChars <= 0 {
		maxChars = SkillBodyChunkThreshold
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return []string{""}
	}
	if len([]rune(body)) <= maxChars {
		return []string{body}
	}

	// Try each delimiter in order of coarseness. The first one that splits
	// into ≥2 segments AND produces no oversized segment wins; otherwise
	// we bin-pack the segments from that split and fall back to finer
	// delimiters on any segment still too large.
	for _, delim := range []string{"\n## ", "\n### ", "\n\n"} {
		segs := splitKeepDelim(body, delim)
		if len(segs) < 2 {
			continue
		}
		chunks := binPackSegments(segs, maxChars)
		// If any chunk still exceeds maxChars, a single segment is oversized
		// at this granularity — let the next-finer loop try.
		oversized := false
		for _, c := range chunks {
			if len([]rune(c)) > maxChars {
				oversized = true
				break
			}
		}
		if !oversized {
			return chunks
		}
	}

	// Last-resort hard split on rune boundaries.
	return hardSplitRunes(body, maxChars)
}

// splitKeepDelim splits s around every occurrence of delim but re-attaches
// delim to the start of each segment after the first. This keeps markdown
// headings intact as chunk starters.
func splitKeepDelim(s, delim string) []string {
	if delim == "" {
		return []string{s}
	}
	parts := strings.Split(s, delim)
	if len(parts) < 2 {
		return parts
	}
	out := make([]string, 0, len(parts))
	out = append(out, strings.TrimRight(parts[0], "\n"))
	// Re-attach the leading newline from the delimiter as a natural
	// paragraph break, but strip the leading newline itself from the
	// first segment of each chunk (we'll re-add it only for segment-joins
	// inside binPackSegments).
	leader := strings.TrimLeft(delim, "\n")
	for _, p := range parts[1:] {
		out = append(out, leader+p)
	}
	// Drop any entirely-empty leading segment (happens when the body starts
	// with the delimiter).
	if len(out) > 0 && strings.TrimSpace(out[0]) == "" {
		out = out[1:]
	}
	return out
}

// binPackSegments greedily concatenates consecutive segments into chunks
// of at most maxChars. Segments larger than maxChars become their own
// chunk (the caller then decides whether to re-split them with a finer
// delimiter).
func binPackSegments(segs []string, maxChars int) []string {
	out := make([]string, 0, len(segs))
	var buf strings.Builder
	flush := func() {
		if buf.Len() == 0 {
			return
		}
		out = append(out, strings.TrimSpace(buf.String()))
		buf.Reset()
	}
	for _, seg := range segs {
		seg = strings.TrimRight(seg, "\n")
		if seg == "" {
			continue
		}
		addLen := len([]rune(seg))
		if buf.Len() > 0 {
			addLen += 2 // "\n\n" join
		}
		if len([]rune(buf.String()))+addLen > maxChars && buf.Len() > 0 {
			flush()
		}
		if buf.Len() > 0 {
			buf.WriteString("\n\n")
		}
		buf.WriteString(seg)
	}
	flush()
	if len(out) == 0 {
		return []string{""}
	}
	return out
}

// hardSplitRunes splits s into substrings of exactly maxChars runes (the
// final segment may be shorter). Used only as the last-resort fallback
// when no markdown structure is available.
func hardSplitRunes(s string, maxChars int) []string {
	if maxChars <= 0 {
		return []string{s}
	}
	runes := []rune(s)
	if len(runes) <= maxChars {
		return []string{s}
	}
	out := make([]string, 0, (len(runes)+maxChars-1)/maxChars)
	for i := 0; i < len(runes); i += maxChars {
		end := i + maxChars
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[i:end]))
	}
	return out
}

// SkillMemoryContent is the canonical text body stored in a skill memory.
// Includes a header line so the embedded vector and search results carry
// the skill name and one-line description even when the body is short.
func SkillMemoryContent(name, description, body string) string {
	var b strings.Builder
	b.Grow(len(name) + len(description) + len(body) + 32)
	if name != "" {
		fmt.Fprintf(&b, "skill: %s\n", strings.TrimSpace(name))
	}
	if description != "" {
		fmt.Fprintf(&b, "description: %s\n", strings.TrimSpace(description))
	}
	if body != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(strings.TrimSpace(body))
	}
	return b.String()
}

// SkillImportResult summarizes one ImportSkillsFromDir invocation.
type SkillImportResult struct {
	// ScannedDirs is how many candidate skill subdirectories were inspected
	// (after hidden-dir filtering, before SKILL.md presence check).
	ScannedDirs int
	// FilesFound is how many SKILL.md files actually existed.
	FilesFound int
	// Created counts brand-new memory rows.
	Created int
	// Updated counts memories whose content hash changed since the last
	// import. Re-imports with no change increment Unchanged instead.
	Updated int
	// Unchanged counts memories whose content hash matched what was
	// already on disk — a no-op upsert.
	Unchanged int
	// Skipped counts directories that were silently ignored (hidden,
	// missing SKILL.md, malformed frontmatter, etc.).
	Skipped int
	// Errors lists per-file errors encountered. Import does not abort on
	// the first error; the caller can decide whether to surface them.
	Errors []SkillImportError
}

// SkillImportError pairs an offending path with its error.
type SkillImportError struct {
	Path string
	Err  error
}

// SkillImportOpts customizes ImportSkillsFromDir.
type SkillImportOpts struct {
	// Dir is the skills root to walk. Required (use ResolveClaudeSkillsDir
	// to fill the default).
	Dir string
	// Now overrides the timestamp source for tests. Defaults to time.Now.
	Now func() time.Time
	// Embed turns body text into a vector. Required — there is no way to
	// upsert without an embedding. Errors short-circuit the per-file
	// import (counted as Errors, never aborting the whole run).
	Embed func(content string) ([]float32, error)
	// DryRun skips the actual upsert. Useful for `--dry-run` inspection
	// and for the doctor check that only counts files vs DB rows.
	DryRun bool
}

// ImportSkillsFromDir walks opts.Dir for skill subdirectories and upserts a
// memory per SKILL.md. Hidden directories (name starts with `.`) are
// skipped. Subdirectories without a SKILL.md are skipped. Malformed YAML
// frontmatter is recorded as an error but doesn't stop the rest of the
// walk. The walker only goes one level deep — Claude Code skills are flat.
func ImportSkillsFromDir(store *MemoryStore, opts SkillImportOpts) (SkillImportResult, error) {
	res := SkillImportResult{}
	if opts.Dir == "" {
		return res, errors.New("skills dir is required")
	}
	if !opts.DryRun && store == nil {
		return res, errors.New("memory store is required (use DryRun=true to skip writes)")
	}
	if !opts.DryRun && opts.Embed == nil {
		return res, errors.New("Embed func is required (use DryRun=true to skip writes)")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	entries, err := os.ReadDir(opts.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			// Empty source directory is a valid "no skills to import"
			// state — let the caller decide whether to log it.
			return res, nil
		}
		return res, err
	}

	// Sort for deterministic ordering — easier to reason about in tests
	// and produces stable doctor output.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			res.Skipped++
			continue
		}
		res.ScannedDirs++

		skillPath := filepath.Join(opts.Dir, name, "SKILL.md")
		st, statErr := os.Stat(skillPath)
		if statErr != nil || st.IsDir() {
			res.Skipped++
			continue
		}
		res.FilesFound++

		sf, parseErr := ParseSkillFile(skillPath)
		if parseErr != nil {
			res.Errors = append(res.Errors, SkillImportError{Path: skillPath, Err: parseErr})
			continue
		}

		slug := SlugifySkill(sf.Name)
		if slug == "skill" || slug == "" {
			// Fall back to the directory name when the frontmatter name
			// would slugify to nothing useful.
			slug = SlugifySkill(name)
		}

		// Proactively chunk oversized bodies so large SKILL.md files
		// (notably the default code-improvement-orchestrator and
		// deep-code-review skills) fit inside the embed model's context
		// window. Each chunk still carries the `skill:` + `description:`
		// header for retrieval relevance.
		bodyChunks := ChunkSkillBody(sf.Body, SkillBodyChunkThreshold)
		if len(bodyChunks) == 0 {
			bodyChunks = []string{""}
		}

		importErr := importSkillChunks(store, opts, sf, slug, skillPath, bodyChunks, now, &res)
		if importErr != nil {
			// importSkillChunks records per-chunk errors in res.Errors
			// directly; a non-nil return is reserved for unrecoverable
			// store errors (e.g. DB connection lost) that should abort
			// the whole walk.
			return res, importErr
		}
	}

	return res, nil
}

// importSkillChunks upserts one or more memory rows for a single SKILL.md
// and prunes any stale part-N rows left over from a previous import with
// more chunks. Per-chunk errors are appended to res.Errors and do not
// abort; a non-nil return value indicates a store-level failure that
// should bubble up to the caller.
func importSkillChunks(
	store *MemoryStore,
	opts SkillImportOpts,
	sf *SkillFile,
	slug string,
	skillPath string,
	bodyChunks []string,
	now func() time.Time,
	res *SkillImportResult,
) error {
	// Precompute each chunk's target content + hash + ID so we can dedupe
	// against existing rows and know which orphan parts to prune.
	type chunkSpec struct {
		id      string
		content string
		hash    string
		partN   int // 0 for the primary, 1-based for parts 2..N
	}
	specs := make([]chunkSpec, 0, len(bodyChunks))
	for i, body := range bodyChunks {
		content := SkillMemoryContent(sf.Name, sf.Description, body)
		if len(bodyChunks) > 1 {
			// Tag each chunk with its part index for human inspection.
			// Placed after the header so the body retrieval target isn't
			// obscured by a bookkeeping line.
			content = fmt.Sprintf("%s\n\n(part %d of %d)", content, i+1, len(bodyChunks))
		}
		hash := ContentHash(content)
		var id string
		if i == 0 {
			id = SkillMemoryID(slug)
		} else {
			id = SkillMemoryIDForPart(slug, i+1)
		}
		specs = append(specs, chunkSpec{id: id, content: content, hash: hash, partN: i})
	}

	// DryRun: classify each chunk (created/unchanged/updated) without touching the store.
	if opts.DryRun {
		for _, spec := range specs {
			if store != nil {
				existing, _ := store.GetMemoryByID(spec.id)
				switch {
				case existing == nil:
					res.Created++
				case existing.ContentHash == spec.hash:
					res.Unchanged++
				default:
					res.Updated++
				}
			} else {
				res.Created++
			}
		}
		return nil
	}

	// Prune orphan part-N rows beyond the current chunk count. This keeps
	// the DB consistent when a skill shrinks across re-imports.
	partPrefix := SkillMemoryID(slug) + SkillMemoryIDPartPrefix
	existingParts, listErr := store.ListMemoryIDsByPrefix(partPrefix)
	if listErr != nil {
		res.Errors = append(res.Errors, SkillImportError{Path: skillPath, Err: listErr})
	} else {
		keep := map[string]struct{}{}
		for _, spec := range specs {
			keep[spec.id] = struct{}{}
		}
		for _, old := range existingParts {
			if _, ok := keep[old]; ok {
				continue
			}
			if delErr := store.DeleteMemoryByID(old); delErr != nil {
				res.Errors = append(res.Errors, SkillImportError{Path: skillPath, Err: delErr})
			}
		}
	}

	ts := now().Unix()
	tags := []string{"claude-skill", "imported", slug}
	for _, spec := range specs {
		existing, lookupErr := store.GetMemoryByID(spec.id)
		if lookupErr != nil {
			res.Errors = append(res.Errors, SkillImportError{Path: skillPath, Err: lookupErr})
			continue
		}
		if existing != nil && existing.ContentHash == spec.hash {
			res.Unchanged++
			continue
		}

		vec, embedErr := opts.Embed(spec.content)
		if embedErr != nil {
			res.Errors = append(res.Errors, SkillImportError{Path: skillPath, Err: embedErr})
			continue
		}

		mem := Memory{
			ID:          spec.id,
			Content:     spec.content,
			Type:        MemoryTypeSkill,
			Tags:        tags,
			Vector:      vec,
			CreatedAt:   ts,
			UpdatedAt:   ts,
			Source:      MemorySourceExplicit,
			ContentHash: spec.hash,
		}
		if existing != nil {
			mem.CreatedAt = existing.CreatedAt
			mem.Tags = MergeTags(existing.Tags, tags)
		}

		if upErr := store.UpsertMemory(mem); upErr != nil {
			res.Errors = append(res.Errors, SkillImportError{Path: skillPath, Err: upErr})
			continue
		}
		if existing == nil {
			res.Created++
		} else {
			res.Updated++
		}
	}
	return nil
}

// SkillWriteOpts configures WriteSkillFile.
type SkillWriteOpts struct {
	// Dir is the skills root to write under (the parent of the per-skill
	// directory). Required.
	Dir string
	// Slug overrides the directory name. If empty, derived from Name.
	Slug string
	// Name is the frontmatter `name:` value.
	Name string
	// Description is the frontmatter `description:` value.
	Description string
	// Body is the markdown body written after the frontmatter.
	Body string
	// Overwrite controls behavior when the destination SKILL.md already
	// exists. Default false → return os.ErrExist.
	Overwrite bool
}

// WriteSkillFile writes a SKILL.md to <Dir>/<slug>/SKILL.md. Creates the
// containing directory if missing. Returns the absolute path written.
//
// Outbound sync invariant: this is the ONLY place heimdall touches the
// user's `~/.claude/skills/` directory, and it is opt-in via the caller.
// The MCP `heimdall_remember` tool exposes it behind `write_file=true`.
func WriteSkillFile(opts SkillWriteOpts) (string, error) {
	if opts.Dir == "" {
		return "", errors.New("skills dir is required")
	}
	slug := opts.Slug
	if slug == "" {
		slug = SlugifySkill(opts.Name)
	}
	if slug == "" {
		return "", errors.New("could not derive slug from skill name")
	}
	skillDir := filepath.Join(opts.Dir, slug)
	target := filepath.Join(skillDir, "SKILL.md")

	if !opts.Overwrite {
		if _, err := os.Stat(target); err == nil {
			return target, fmt.Errorf("skill file already exists at %s: %w", target, os.ErrExist)
		} else if !os.IsNotExist(err) {
			return target, err
		}
	}

	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		return target, err
	}

	var b strings.Builder
	b.WriteString("---\n")
	if opts.Name != "" {
		fmt.Fprintf(&b, "name: %s\n", singleLineMeta(opts.Name))
	}
	if opts.Description != "" {
		fmt.Fprintf(&b, "description: %s\n", singleLineMeta(opts.Description))
	}
	b.WriteString("---\n")
	body := strings.TrimSpace(opts.Body)
	if body != "" {
		b.WriteString("\n")
		b.WriteString(body)
		b.WriteString("\n")
	}

	if err := os.WriteFile(target, []byte(b.String()), 0o644); err != nil {
		return target, err
	}
	return target, nil
}

// singleLineMeta collapses any line break in a frontmatter value so the
// emitted YAML stays valid. Quotes the value if it contains a colon (which
// would otherwise look like a nested key) or a leading special char.
func singleLineMeta(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	needsQuote := strings.ContainsAny(s, ":#&*!|>%@`")
	if !needsQuote {
		switch s[0] {
		case '-', '?', '[', ']', '{', '}', ',':
			needsQuote = true
		}
	}
	if needsQuote {
		// Escape embedded double quotes and wrap.
		s = strings.ReplaceAll(s, `"`, `\"`)
		return `"` + s + `"`
	}
	return s
}

// CountDiskSkillFiles counts SKILL.md files under dir, one level deep,
// skipping hidden directories. Used by `hooks doctor` to report
// "X disk skills, Y synced". Returns 0 and a nil error when the directory
// doesn't exist (treated as "no skills available").
func CountDiskSkillFiles(dir string) (int, error) {
	if dir == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		st, err := os.Stat(filepath.Join(dir, e.Name(), "SKILL.md"))
		if err == nil && !st.IsDir() {
			n++
		}
	}
	return n, nil
}

// CountSyncedSkillMemories returns how many *disk-file* skills are synced
// into the memory store. A single disk SKILL.md that was chunked into
// multiple part-N rows still counts as one synced skill — the doctor
// rollup compares disk files to disk-file-equivalents in the store, not
// raw memory rows. Used by `hooks doctor` for the sync rollup row.
func CountSyncedSkillMemories(store *MemoryStore) (int, error) {
	if store == nil {
		return 0, errors.New("store is nil")
	}
	ids, err := store.ListMemoryIDsByPrefix("mem:skill:disk:")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		if !strings.Contains(id, SkillMemoryIDPartPrefix) {
			n++
		}
	}
	return n, nil
}
