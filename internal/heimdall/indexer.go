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

const (
	// MaxFileSize is the maximum file size to index (100KB).
	MaxFileSize = 100 * 1024
	// MaxFiles is the maximum number of files to index.
	MaxFiles = 5000
)

// Indexer scans a project, chunks files, generates embeddings, and stores them.
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

// IndexResult holds statistics from an indexing run.
type IndexResult struct {
	FilesScanned  int
	FilesIndexed  int
	FilesSkipped  int
	ChunksCreated int
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
	for _, walkRoot := range walkRoots {
		if len(files) >= MaxFiles {
			break
		}
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
				if idx.shouldExclude(relPath) {
					return filepath.SkipDir
				}
				// Skip directories that are their own git repos (sub-repos)
				if path != walkRoot {
					gitDir := filepath.Join(path, ".git")
					if info, err := os.Stat(gitDir); err == nil && info.IsDir() {
						return filepath.SkipDir
					}
				}
				return nil
			}

			if idx.shouldExclude(relPath) {
				return nil
			}

			// Skip files over MaxFileSize
			info, statErr := d.Info()
			if statErr != nil {
				return nil
			}
			if info.Size() > MaxFileSize {
				return nil
			}

			// Skip binary files
			if isBinaryFile(path) {
				return nil
			}

			if len(files) >= MaxFiles {
				return filepath.SkipAll
			}

			files = append(files, path)
			return nil
		})
		if err != nil && err != filepath.SkipAll && ctx.Err() == nil {
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
				result.FilesSkipped++
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

// shouldExclude checks if a relative path matches any exclude pattern.
func (idx *Indexer) shouldExclude(relPath string) bool {
	// Always exclude common directories
	defaultExcludes := []string{".git", "node_modules", ".heimdall_db", "vendor", "__pycache__", ".idea"}
	allExcludes := append(defaultExcludes, idx.opts.ExcludeGlobs...)

	parts := strings.Split(filepath.ToSlash(relPath), "/")
	for _, pattern := range allExcludes {
		for _, part := range parts {
			if matched, _ := filepath.Match(pattern, part); matched {
				return true
			}
		}
		if matched, _ := filepath.Match(pattern, filepath.ToSlash(relPath)); matched {
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
