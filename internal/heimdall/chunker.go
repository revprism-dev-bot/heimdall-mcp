package heimdall

// ChunkerOpts controls chunking behavior.
type ChunkerOpts struct {
	MaxChunkSize int      // max characters per chunk (default 1500)
	ContextDepth int      // 0-2: extra surrounding lines to include
	ExcludeGlobs []string // patterns to skip
	IncludePaths []string // directories to index (empty = root only)
}

// Chunk represents a piece of a file suitable for embedding.
type Chunk struct {
	FilePath   string // relative path from project root
	StartLine  int    // 1-based line number where chunk starts
	EndLine    int    // 1-based line number where chunk ends
	Content    string // the actual text
	Kind       string // "function", "type", "paragraph", "file" (for small files)
	Identifier string // function/type name if applicable (empty for paragraphs)
}
