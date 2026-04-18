package heimdall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Indexer scans a project, chunks files, generates embeddings, and stores them.
//
// No per-file or per-project caps live here anymore. Back-pressure comes from:
//   - excludePatterns (.git, node_modules, vendor, .heimdall_db, __pycache__,
//     .idea, .claude/worktrees by default; user-configurable via
//     cfg.ExcludePatterns, CLI --exclude, or the MCP exclude_patterns key)
//   - isBinaryFile NUL-byte sniff (first 512 bytes)
//   - the chunker, which slices every file into MaxChunkSize (~1500-char)
//     pieces — well under nomic-embed-text's 8192-token context window
//
// Nested sub-repos (immediate subdirs with their own .git entry) are skipped
// during the outer walk and recorded in IndexResult.SubRepos so the caller
// can spawn a separate Indexer per sub-repo via IndexSubRepos. That pass
// shares the same exclude rules — a sub-repo whose relative path matches a
// user pattern is skipped at discovery time too.
//
// A pathological multi-GB text file will be read whole via os.ReadFile in
// chunkFile, which is the only remaining memory cliff. If that ever matters
// in practice, exclude the file via excludePatterns or switch chunkFile to
// a streaming reader (flagged as follow-up when this cap was removed).
type Indexer struct {
	embedder Embedder
	store    *VectorStore
	opts     ChunkerOpts
	root     string
}

// NewIndexer creates an indexer for the given project root.
func NewIndexer(root string, embedder Embedder, store *VectorStore, opts ChunkerOpts) *Indexer {
	return &Indexer{
		embedder: embedder,
		store:    store,
		opts:     opts,
		root:     root,
	}
}

// SkipBreakdown classifies skipped files by reason.
//
// UserExcluded counts ONLY user-configured exclusions (cfg.ExcludePatterns
// + CLI --exclude + MCP exclude_patterns). Default hygiene dirs (.git,
// node_modules, .heimdall_db, vendor, __pycache__, .idea) are NOT counted —
// the user cares about what THEY excluded, not about always-on cleanup.
// See plan §G6 for the contract and §10.3 TestExclude_DefaultHygieneNotCounted
// for the regression test.
//
// Invariant: UserExcluded + SubRepo + Binary + Unchanged == IndexResult.FilesSkipped.
type SkipBreakdown struct {
	UserExcluded int `json:"user_excluded"`
	SubRepo      int `json:"sub_repo"`
	Binary       int `json:"binary"`
	Unchanged    int `json:"unchanged"`
}

// IndexResult holds statistics from an indexing run.
type IndexResult struct {
	FilesScanned  int
	FilesIndexed  int
	// FilesSkipped is the sum of Skip.UserExcluded + Skip.SubRepo + Skip.Binary
	// + Skip.Unchanged. Retained for backward compat with existing callers.
	FilesSkipped  int
	// Skip is the per-reason breakdown. New callers should prefer reading this.
	Skip          SkipBreakdown
	ChunksCreated int
	// SubRepos is the list of immediate sub-repo directory names (relative to
	// idx.root) that were discovered during the outer walk and skipped by
	// filepath.SkipDir. These are the sub-repos that the orchestrator (CLI or
	// MCP runIndex) should index separately as their own projects.
	SubRepos      []string
	Duration      time.Duration
	Errors        []string
}

// IndexProgress reports progress from an async indexing run.
type IndexProgress struct {
	Current      int    // files processed so far
	Total        int    // total files discovered
	FilePath     string // current file being indexed
	ChunksSoFar  int    // total chunks created so far
	FileChunks   int    // chunks created for this file
	BytesDone    int64  // bytes processed so far
	BytesTotal   int64  // total bytes to process
	Done         bool   // true when indexing is complete
	Result       *IndexResult // non-nil when Done is true
	Err          error        // non-nil if indexing failed
}

// SubRepoOpts parameterises IndexSubRepos. Currently empty — kept as a
// named struct so we can grow the knob surface (e.g. parallelism, explicit
// model-override) without breaking callers. Exclude patterns are already
// inherited through the indexer's ChunkerOpts.
type SubRepoOpts struct{}

// SubRepoResult describes one sub-repo's indexing outcome.
//
// Invariant: Err != nil implies Result may be nil. Callers MUST gate on
// Err == nil && Result != nil before dereferencing Result.FilesIndexed etc.
// Failures are surfaced per-entry rather than aborting the whole sub-repo
// pass, so one bad sub-repo does not block the rest.
type SubRepoResult struct {
	// Path is the absolute path to the sub-repo root. Always populated.
	Path string
	// Name is filepath.Base(Path). Always populated.
	Name string
	// DBPath is the absolute path to <sub>/.heimdall_db (no model subdir).
	// Always populated so callers can register even partial-failure entries
	// if they so choose.
	DBPath string
	// Model is the effective embedding model used (the pinned model if the
	// sub-repo had a prior store, otherwise the caller's requested model).
	// Populated iff store-open succeeded.
	Model string
	// PinnedModel is true iff Model came from a pre-existing sub-repo store
	// rather than the caller's requested model. The CLI summary surfaces
	// this via `[<model> — pinned]`.
	PinnedModel bool
	// Result is the indexing result. Nil iff Err != nil or store-open failed.
	Result *IndexResult
	// Err is non-nil iff this sub-repo failed (discover, open, or index).
	Err error
}

// IndexSubRepos discovers immediate sub-repos of idx.root and indexes each
// one as a separate project rooted at itself, writing to that sub-repo's
// own `.heimdall_db`.
//
// model is the caller's requested model (typically the outer's). If a
// sub-repo already has a store at <sub>/.heimdall_db/<M>/vectors.db for
// some model M, then M is preserved — the caller's model does NOT overwrite
// a pinned store. See plan §G4 rule 1.
//
// The returned error is non-nil ONLY for discovery-level failures (root
// unreadable). Per-sub-repo failures are reported via out[i].Err and do NOT
// cause this function to return a non-nil error. This preserves the
// "outer-indexed-successfully" contract even if every sub-repo fails.
//
// Sub-repos whose relative path matches a user-configured exclude pattern
// (idx.opts.ExcludeGlobs) are filtered out before indexing. Default
// hygiene excludes do not apply here because DiscoverSubReposAbs never
// descends into them in the first place.
func (idx *Indexer) IndexSubRepos(ctx context.Context, model string, _ SubRepoOpts) ([]SubRepoResult, error) {
	subs, err := DiscoverSubReposAbs(idx.root)
	if err != nil {
		return nil, fmt.Errorf("discover sub-repos: %w", err)
	}
	var out []SubRepoResult
	for _, subAbs := range subs {
		relName, relErr := filepath.Rel(idx.root, subAbs)
		if relErr != nil {
			// Shouldn't happen — idx.root is absolute and subAbs is built
			// via filepath.Join(root, basename). Fail loudly anyway so a
			// pathological path does not get silently dropped.
			out = append(out, SubRepoResult{
				Path:   subAbs,
				Name:   filepath.Base(subAbs),
				DBPath: filepath.Join(subAbs, ".heimdall_db"),
				Err:    fmt.Errorf("relpath %s: %w", subAbs, relErr),
			})
			continue
		}
		if idx.isUserExcluded(relName) {
			// User explicitly excluded this sub-repo — do not index or
			// report it in the results slice (the orchestrator counted it
			// via Skip.UserExcluded during the outer walk; double-counting
			// would confuse the summary renderer).
			continue
		}

		subRes := SubRepoResult{
			Path:   subAbs,
			Name:   filepath.Base(subAbs),
			DBPath: filepath.Join(subAbs, ".heimdall_db"),
		}

		// Resolve model: pinned prior store wins over the caller's request.
		effectiveModel := model
		pinned := false
		if existing := ListAvailableModels(subRes.DBPath); len(existing) > 0 {
			effectiveModel = existing[0]
			pinned = effectiveModel != model
		}
		subRes.Model = effectiveModel
		subRes.PinnedModel = pinned

		// Open / create the sub-repo store. A failure here is captured and
		// the sub-repo moves on — no panics, no partial writes to stale
		// stores.
		subDBDir := ModelDBDir(subRes.DBPath, effectiveModel)
		subStore, err := OpenStore(subDBDir)
		if err != nil {
			subRes.Err = fmt.Errorf("open sub-repo store %s: %w", subDBDir, err)
			// Clear Model to keep the contract "populated iff store-open
			// succeeded" precise.
			subRes.Model = ""
			subRes.PinnedModel = false
			out = append(out, subRes)
			continue
		}

		// Spawn a fresh indexer rooted at the sub-repo, inheriting the
		// parent's ChunkerOpts (exclude patterns in particular) and the
		// same embedder. The embedder choice is the orchestrator's
		// responsibility — if the pinned model differs from the embedder's
		// native model, the caller is expected to rebuild the embedder
		// before calling IndexSubRepos (see CLI integration in cliIndex).
		subIdx := NewIndexer(subAbs, idx.embedder, subStore, idx.opts)
		r, indexErr := subIdx.IndexAll(ctx)
		subRes.Result = r
		subRes.Err = indexErr
		if indexErr == nil {
			subStore.SetMetadata("embedding_model", effectiveModel)
		}
		if closeErr := subStore.Close(); closeErr != nil && subRes.Err == nil {
			subRes.Err = fmt.Errorf("close sub-repo store %s: %w", subDBDir, closeErr)
		}
		out = append(out, subRes)
	}
	return out, nil
}

// IndexAll performs a full re-index of the project synchronously.
func (idx *Indexer) IndexAll(ctx context.Context) (*IndexResult, error) {
	return idx.indexFiles(ctx, false, nil)
}

// IndexIncremental only re-indexes files that have changed since their
// last indexed modTime.
func (idx *Indexer) IndexIncremental(ctx context.Context) (*IndexResult, error) {
	return idx.indexFiles(ctx, true, nil)
}

// IndexProjectAsync runs incremental indexing in a goroutine, sending progress
// updates on the provided channel. Skips files already indexed with the same
// modtime. The channel is closed when indexing completes.
// Respects context cancellation for early termination.
func (idx *Indexer) IndexProjectAsync(ctx context.Context, progress chan<- IndexProgress) {
	go func() {
		defer close(progress)
		result, err := idx.indexFiles(ctx, true, progress)
		progress <- IndexProgress{
			Done:   true,
			Result: result,
			Err:    err,
		}
	}()
}

// indexFiles is the shared implementation for sync and async indexing.
// If progress is non-nil, sends updates on each file processed.
func (idx *Indexer) indexFiles(ctx context.Context, incremental bool, progress chan<- IndexProgress) (*IndexResult, error) {
	start := time.Now()
	result := &IndexResult{}

	// Discover immediate sub-repo directories (dirs with their own .git/)
	// so we can tag files with their sub-project.
	subRepoDirs := DiscoverSubRepos(idx.root)

	// Phase 1: discover all indexable files
	// Determine which directories to walk. If IncludePaths is set, walk
	// each of those (resolved relative to root); otherwise walk root.
	walkRoots := []string{idx.root}
	if len(idx.opts.IncludePaths) > 0 {
		walkRoots = walkRoots[:0]
		for _, p := range idx.opts.IncludePaths {
			abs := p
			if !filepath.IsAbs(p) {
				abs = filepath.Join(idx.root, p)
			}
			walkRoots = append(walkRoots, abs)
		}
	}

	var files []string
	// subRepoSeen dedupes entries in result.SubRepos. The walker only visits
	// each directory once but using a set makes the post-pass loop cheap to
	// reason about and future-proofs against nested walks.
	subRepoSeen := make(map[string]bool)
	for _, walkRoot := range walkRoots {
		err := filepath.WalkDir(walkRoot, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // skip unreadable
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}

			relPath, relErr := filepath.Rel(idx.root, path)
			if relErr != nil {
				return nil
			}

			if d.IsDir() {
				// User-configured excludes are counted and skipped.
				if idx.isUserExcluded(relPath) {
					result.Skip.UserExcluded++
					return filepath.SkipDir
				}
				// Default hygiene is silent (not counted — H1 resolution).
				if idx.isDefaultExcluded(relPath) {
					return filepath.SkipDir
				}
				// Skip directories that are their own git repos (sub-repos).
				// Accept .git as either a directory or a regular file (gitlink
				// for git worktrees) — matches hasRepoMarker in scope.go.
				// Record the sub-repo's relative path so the orchestrator can
				// index it as a separate project after the outer walk.
				if path != walkRoot {
					gitDir := filepath.Join(path, ".git")
					if _, err := os.Stat(gitDir); err == nil {
						if !subRepoSeen[relPath] {
							subRepoSeen[relPath] = true
							result.SubRepos = append(result.SubRepos, relPath)
							result.Skip.SubRepo++
						}
						return filepath.SkipDir
					}
				}
				return nil
			}

			if idx.isUserExcluded(relPath) {
				result.Skip.UserExcluded++
				return nil
			}
			if idx.isDefaultExcluded(relPath) {
				return nil
			}

			// Skip binary files
			if isBinaryFile(path) {
				result.Skip.Binary++
				return nil
			}

			files = append(files, path)
			return nil
		})
		if err != nil && ctx.Err() == nil {
			result.Duration = time.Since(start)
			return result, fmt.Errorf("walking project: %w", err)
		}
	}

	result.FilesScanned = len(files)

	// Calculate total bytes for progress reporting
	fileSizes := make([]int64, len(files))
	var totalBytes int64
	for i, path := range files {
		info, err := os.Stat(path)
		if err == nil {
			fileSizes[i] = info.Size()
			totalBytes += info.Size()
		}
	}

	// Phase 2: index each file
	var bytesDone int64
	for i, path := range files {
		if ctx.Err() != nil {
			result.Duration = time.Since(start)
			return result, ctx.Err()
		}

		relPath, _ := filepath.Rel(idx.root, path)

		bytesDone += fileSizes[i]

		// Incremental: skip unchanged files
		if incremental {
			info, statErr := os.Stat(path)
			if statErr != nil {
				if progress != nil {
					progress <- IndexProgress{
						Current: i + 1, Total: len(files), FilePath: relPath,
						ChunksSoFar: result.ChunksCreated,
						BytesDone: bytesDone, BytesTotal: totalBytes,
					}
				}
				continue
			}
			if idx.isUpToDate(relPath, info.ModTime(), path) {
				result.Skip.Unchanged++
				if progress != nil {
					progress <- IndexProgress{
						Current: i + 1, Total: len(files), FilePath: relPath,
						ChunksSoFar: result.ChunksCreated,
						BytesDone: bytesDone, BytesTotal: totalBytes,
					}
				}
				continue
			}
		}

		// Send progress update before processing
		if progress != nil {
			progress <- IndexProgress{
				Current: i + 1, Total: len(files), FilePath: relPath,
				ChunksSoFar: result.ChunksCreated,
				BytesDone: bytesDone, BytesTotal: totalBytes,
			}
		}

		chunks, chunkErr := idx.chunkFile(path, relPath)
		if chunkErr != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", relPath, chunkErr))
			continue
		}

		if len(chunks) == 0 {
			continue
		}

		// Embed all chunks (batch if supported, fallback to one-at-a-time)
		fileHash := hashFile(path)
		subProject := subProjectForFile(relPath, subRepoDirs)
		info, _ := os.Stat(path)
		var mtime int64
		if info != nil {
			mtime = info.ModTime().Unix()
		}

		var records []VectorRecord
		if batchEmb, ok := idx.embedder.(BatchEmbedder); ok {
			// Batch path: collect texts, embed in one call
			texts := make([]string, len(chunks))
			for ci, chunk := range chunks {
				texts[ci] = chunk.Content
			}
			vecs, embedErr := batchEmb.EmbedBatch(ctx, texts)
			if embedErr != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("%s batch embed: %v", relPath, embedErr))
				continue
			}
			for ci, chunk := range chunks {
				records = append(records, VectorRecord{
					ID:            fmt.Sprintf("%s:%d:%d", relPath, chunk.StartLine, chunk.EndLine),
					FilePath:      relPath,
					StartLine:     chunk.StartLine,
					EndLine:       chunk.EndLine,
					Content:       chunk.Content,
					Kind:          chunk.Kind,
					Identifier:    chunk.Identifier,
					Embedding:     vecs[ci],
					ModTime:       mtime,
					ContentHash:   fileHash,
					SourceType:    "code",
					Metadata:      "{}",
					Relationships: "[]",
					SubProject:    subProject,
				})
			}
		} else {
			// Fallback: embed one chunk at a time
			for ci, chunk := range chunks {
				vec, embedErr := idx.embedder.Embed(ctx, chunk.Content)
				if embedErr != nil {
					result.Errors = append(result.Errors, fmt.Sprintf("%s chunk %d: %v", relPath, ci, embedErr))
					continue
				}
				records = append(records, VectorRecord{
					ID:            fmt.Sprintf("%s:%d:%d", relPath, chunk.StartLine, chunk.EndLine),
					FilePath:      relPath,
					StartLine:     chunk.StartLine,
					EndLine:       chunk.EndLine,
					Content:       chunk.Content,
					Kind:          chunk.Kind,
					Identifier:    chunk.Identifier,
					Embedding:     vec,
					ModTime:       mtime,
					ContentHash:   fileHash,
					SourceType:    "code",
					Metadata:      "{}",
					Relationships: "[]",
					SubProject:    subProject,
				})
			}
		}

		if len(records) > 0 {
			if upsertErr := idx.store.Upsert(records); upsertErr != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("%s upsert: %v", relPath, upsertErr))
				continue
			}
			result.FilesIndexed++
			result.ChunksCreated += len(records)
		}
	}

	// Save store to disk
	if saveErr := idx.store.Save(); saveErr != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("store save: %v", saveErr))
	}

	// Keep the legacy FilesSkipped counter consistent with the breakdown
	// so both old and new readers observe the same totals. Invariant in
	// SkipBreakdown godoc.
	result.FilesSkipped = result.Skip.UserExcluded + result.Skip.SubRepo + result.Skip.Binary + result.Skip.Unchanged

	result.Duration = time.Since(start)
	return result, nil
}

// chunkFile reads a file and splits it into chunks for embedding.
// Uses a simple line-based chunking strategy.
func (idx *Indexer) chunkFile(path, relPath string) ([]Chunk, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	content := string(data)
	if len(content) == 0 {
		return nil, nil
	}

	maxSize := idx.opts.MaxChunkSize
	if maxSize <= 0 {
		maxSize = 1500
	}

	// If the file is small enough, return it as a single chunk.
	if len(content) <= maxSize {
		return []Chunk{{
			FilePath:  relPath,
			StartLine: 1,
			EndLine:   strings.Count(content, "\n") + 1,
			Content:   content,
			Kind:      "file",
		}}, nil
	}

	// Split into chunks by lines, respecting maxSize.
	lines := strings.Split(content, "\n")
	var chunks []Chunk
	var buf strings.Builder
	startLine := 1

	for i, line := range lines {
		lineNum := i + 1
		// If adding this line would exceed maxSize and we have content, flush.
		if buf.Len()+len(line)+1 > maxSize && buf.Len() > 0 {
			chunks = append(chunks, Chunk{
				FilePath:  relPath,
				StartLine: startLine,
				EndLine:   lineNum - 1,
				Content:   buf.String(),
				Kind:      "paragraph",
			})
			buf.Reset()
			startLine = lineNum
		}
		if buf.Len() > 0 {
			buf.WriteByte('\n')
		}
		buf.WriteString(line)
	}

	// Flush remaining content.
	if buf.Len() > 0 {
		chunks = append(chunks, Chunk{
			FilePath:  relPath,
			StartLine: startLine,
			EndLine:   len(lines),
			Content:   buf.String(),
			Kind:      "paragraph",
		})
	}

	return chunks, nil
}

// defaultExcludes names directories that are always skipped for hygiene.
// Hits against this list are NOT counted in Skip.UserExcluded — see
// SkipBreakdown doc and plan §G6 (H1 resolution).
var defaultExcludes = []string{".git", "node_modules", ".heimdall_db", "vendor", "__pycache__", ".idea"}

// isDefaultExcluded reports whether relPath matches one of the always-on
// hygiene patterns. Does NOT consult user config. Hits here are silent —
// the user did not opt into them and should not see them in summary counts.
func (idx *Indexer) isDefaultExcluded(relPath string) bool {
	return matchesAnyPattern(relPath, defaultExcludes)
}

// isUserExcluded reports whether relPath matches a user-configured pattern
// from ChunkerOpts.ExcludeGlobs (sourced from cfg.ExcludePatterns + CLI
// --exclude + MCP exclude_patterns). Hits bump Skip.UserExcluded.
func (idx *Indexer) isUserExcluded(relPath string) bool {
	return matchesAnyPattern(relPath, idx.opts.ExcludeGlobs)
}

// shouldExclude is the union of default and user excludes. Retained for the
// remaining callers (e.g. legacy tests / sub-repo orchestration helpers) so
// behaviour is identical when a counter update is not required.
func (idx *Indexer) shouldExclude(relPath string) bool {
	return idx.isDefaultExcluded(relPath) || idx.isUserExcluded(relPath)
}

// matchesAnyPattern tests relPath against each pattern, matching both each
// path component and the full slash-joined relative path — same semantics as
// the previous shouldExclude body.
func matchesAnyPattern(relPath string, patterns []string) bool {
	if len(patterns) == 0 {
		return false
	}
	slashed := filepath.ToSlash(relPath)
	parts := strings.Split(slashed, "/")
	for _, pattern := range patterns {
		for _, part := range parts {
			if matched, _ := filepath.Match(pattern, part); matched {
				return true
			}
		}
		if matched, _ := filepath.Match(pattern, slashed); matched {
			return true
		}
	}
	return false
}

// isUpToDate checks if a file has been indexed since its last modification.
// Uses a two-tier check: fast modtime comparison first, then content hash
// fallback for copied DBs where local modtimes differ (e.g. fresh git clone).
func (idx *Indexer) isUpToDate(relPath string, mtime time.Time, filePath string) bool {
	maxMod := idx.store.MaxModTimeForFile(relPath)
	if maxMod == 0 {
		return false // no records for this file
	}
	if maxMod >= mtime.Unix() {
		return true // modtime check passes
	}
	// Modtime differs — compare content hash to detect actual changes.
	storedHash := idx.store.ContentHashForFile(relPath)
	if storedHash == "" {
		// DB predates content hashing — fall back to HasFile.
		return idx.store.HasFile(relPath)
	}
	currentHash := hashFile(filePath)
	return currentHash == storedHash
}

// hashFile returns the hex-encoded SHA-256 of the file contents.
func hashFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// subProjectForFile returns the sub-project name for a file's relative path.
// If the first path component matches a known sub-repo directory, that name
// is returned. Otherwise returns empty string.
func subProjectForFile(relPath string, subRepoDirs map[string]bool) string {
	parts := strings.SplitN(filepath.ToSlash(relPath), "/", 2)
	if len(parts) > 0 && subRepoDirs[parts[0]] {
		return parts[0]
	}
	return ""
}

// isBinaryFile checks if a file appears to be binary by looking for
// null bytes in the first 512 bytes.
func isBinaryFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return true // can't read, skip
	}
	defer f.Close()

	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	if n == 0 {
		return false
	}
	for _, b := range buf[:n] {
		if b == 0 {
			return true
		}
	}
	return false
}
