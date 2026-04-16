package heimdall

// MemoryType constrains the allowed memory types.
type MemoryType string

const (
	MemoryTypePreference MemoryType = "preference"
	MemoryTypeDecision   MemoryType = "decision"
	MemoryTypeFact       MemoryType = "fact"
	MemoryTypeContext    MemoryType = "context"
	MemoryTypeSkill      MemoryType = "skill"
)

// ValidMemoryTypes for validation.
var ValidMemoryTypes = map[MemoryType]bool{
	MemoryTypePreference: true,
	MemoryTypeDecision:   true,
	MemoryTypeFact:       true,
	MemoryTypeContext:    true,
	MemoryTypeSkill:      true,
}

// MemorySource indicates how a memory was created.
type MemorySource string

const (
	MemorySourceExplicit MemorySource = "explicit"
	MemorySourceSession  MemorySource = "session"
)

// Memory represents a single stored memory.
type Memory struct {
	ID          string       `json:"id"`
	Content     string       `json:"content"`
	Type        MemoryType   `json:"type"`
	Tags        []string     `json:"tags,omitempty"`
	Project     string       `json:"project,omitempty"`
	ContextPath string       `json:"contextPath,omitempty"` // slash-separated subpath within Project; used for scope filtering
	Vector      []float32    `json:"-"`
	CreatedAt   int64        `json:"createdAt"`
	UpdatedAt   int64        `json:"updatedAt"`
	Source      MemorySource `json:"source"`
	ContentHash string       `json:"-"`
}

// MemorySearchResult is a memory with its similarity score.
type MemorySearchResult struct {
	Memory     Memory  `json:"memory"`
	Similarity float64 `json:"score"`
}

// MemoryFilter holds optional filters for memory search.
type MemoryFilter struct {
	Type        MemoryType
	Tags        []string
	Project     string
	Source      MemorySource
	ContextPath string // prefix filter on Memory.ContextPath; empty disables the filter
}

// MemoryStoreStats holds memory-specific index statistics.
type MemoryStoreStats struct {
	TotalMemories int            `json:"totalMemories"`
	ByType        map[string]int `json:"byType"`
	BySource      map[string]int `json:"bySource"`
	ByProject     map[string]int `json:"byProject"`
}

// memoryVectorEntry is used for bulk vector loading during dedup.
type memoryVectorEntry struct {
	ID     string
	Vector []float32
}
