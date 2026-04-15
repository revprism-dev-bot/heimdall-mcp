package heimdall

import (
	"context"
	"os"
	"time"
)

// StatusInfo is a machine-readable snapshot of heimdall's state for a given
// project base directory. It mirrors the shape of `mcp.toolStatus` so JSON
// output from the CLI and MCP tool is structurally identical.
type StatusInfo struct {
	Endpoint          string             `json:"endpoint"`
	Model             string             `json:"model"`
	ExcludePatterns   []string           `json:"excludePatterns,omitempty"`
	OllamaRunning     bool               `json:"ollamaRunning"`
	ModelAvailable    *bool              `json:"modelAvailable,omitempty"`
	AvailableModels   []string           `json:"availableModels,omitempty"`
	IndexedFiles      int                `json:"indexedFiles"`
	TotalChunks       int                `json:"totalChunks"`
	LastIndexed       string             `json:"lastIndexed"`
	IndexEmbeddingDim string             `json:"indexEmbeddingDim,omitempty"`
	BaseDir           string             `json:"baseDir,omitempty"`
	ModelDBDir        string             `json:"modelDBDir,omitempty"`
	Projects          []StatusProject    `json:"registeredProjects,omitempty"`
}

// StatusProject is a single project entry in StatusInfo.
type StatusProject struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	DBPath  string `json:"dbPath"`
	Current bool   `json:"current,omitempty"`
}

// GatherStatus collects index/model/Ollama status for a project base dir.
// It does not consult the in-memory IndexState or project registry — the
// caller supplies the registered-project list (if any) via StatusProjects.
// This keeps the function pure and testable.
func GatherStatus(ctx context.Context, endpoint, model, baseDir string, excludePatterns []string, client *OllamaClient) StatusInfo {
	info := StatusInfo{
		Endpoint:        endpoint,
		Model:           model,
		ExcludePatterns: excludePatterns,
		BaseDir:         baseDir,
		LastIndexed:     "no index",
	}

	if client != nil {
		if err := client.Ping(ctx); err == nil {
			info.OllamaRunning = true
			if models, err := client.ListModels(ctx); err == nil {
				hasModel := modelPrefixMatch(models, model)
				info.ModelAvailable = &hasModel
			}
		}
	}

	if available := ListAvailableModels(baseDir); len(available) > 0 {
		info.AvailableModels = available
	}

	modelDir := ModelDBDir(baseDir, model)
	info.ModelDBDir = modelDir
	if _, err := os.Stat(modelDir); err == nil {
		store, err := OpenStore(modelDir)
		if err == nil {
			defer store.Close()
			stats := store.Stats()
			info.IndexedFiles = stats.TotalFiles
			info.TotalChunks = stats.TotalRecords
			if stats.LastModified > 0 {
				info.LastIndexed = time.Unix(stats.LastModified, 0).Format("2006-01-02 15:04:05")
			} else {
				info.LastIndexed = "never"
			}
			if dim := store.GetMetadata("embedding_dim"); dim != "" {
				info.IndexEmbeddingDim = dim
			}
		}
	}

	return info
}

func modelPrefixMatch(models []ModelInfo, cfgModel string) bool {
	for _, m := range models {
		if m.Name == cfgModel {
			return true
		}
		if len(m.Name) > len(cfgModel) && m.Name[:len(cfgModel)] == cfgModel {
			return true
		}
	}
	return false
}
