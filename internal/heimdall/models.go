package heimdall

// KnownEmbeddingModel holds metadata about a recognized embedding model.
type KnownEmbeddingModel struct {
	Name       string // Ollama model name (without tag)
	Origin     string // Company/org and country
	Params     string // Parameter count
	Dimensions int    // Output vector dimensions
	License    string // License type
	Notes      string // Brief description
}

// KnownEmbeddingModels is the curated list of models known to produce embeddings.
// This is used to help users pick a model — any model that can embed via Ollama
// will work, but these are the ones we can give guidance on.
var KnownEmbeddingModels = []KnownEmbeddingModel{
	{Name: "nomic-embed-text", Origin: "Nomic AI (US)", Params: "137M", Dimensions: 768, License: "Apache 2.0", Notes: "Default. Best quality/size balance."},
	{Name: "snowflake-arctic-embed:s", Origin: "Snowflake (US)", Params: "33M", Dimensions: 384, License: "Apache 2.0", Notes: "Tiny, fast."},
	{Name: "snowflake-arctic-embed", Origin: "Snowflake (US)", Params: "110M", Dimensions: 768, License: "Apache 2.0", Notes: "Medium footprint."},
	{Name: "snowflake-arctic-embed:l", Origin: "Snowflake (US)", Params: "335M", Dimensions: 1024, License: "Apache 2.0", Notes: "High quality."},
	{Name: "all-minilm", Origin: "Microsoft (US)", Params: "33M", Dimensions: 384, License: "Apache 2.0", Notes: "Smallest, fastest, lower quality."},
	{Name: "mxbai-embed-large", Origin: "Mixedbread (DE)", Params: "335M", Dimensions: 1024, License: "Apache 2.0", Notes: "High quality, heavier."},
	{Name: "bge-m3", Origin: "BAAI (CN)", Params: "567M", Dimensions: 1024, License: "MIT", Notes: "Multilingual, heaviest."},
	{Name: "bge-large-en-v1.5", Origin: "BAAI (CN)", Params: "335M", Dimensions: 1024, License: "MIT", Notes: "English-focused."},
}

// knownModelSet is a lookup set for fast membership checks.
var knownModelSet map[string]bool

func init() {
	knownModelSet = make(map[string]bool, len(KnownEmbeddingModels))
	for _, m := range KnownEmbeddingModels {
		knownModelSet[m.Name] = true
	}
}

// IsKnownEmbeddingModel returns true if the model name (with or without tag)
// matches a known embedding model.
func IsKnownEmbeddingModel(name string) bool {
	if knownModelSet[name] {
		return true
	}
	// Strip tag (e.g. "nomic-embed-text:latest" → "nomic-embed-text")
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == ':' {
			return knownModelSet[name[:i]]
		}
	}
	return false
}

// LookupEmbeddingModel returns metadata for a known model, or nil.
func LookupEmbeddingModel(name string) *KnownEmbeddingModel {
	for i := range KnownEmbeddingModels {
		m := &KnownEmbeddingModels[i]
		if m.Name == name {
			return m
		}
	}
	// Try stripping tag
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == ':' {
			return LookupEmbeddingModel(name[:i])
		}
	}
	return nil
}
