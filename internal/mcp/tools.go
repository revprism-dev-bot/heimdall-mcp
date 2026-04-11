package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
	"github.com/caio-silva/heimdall-mcp/internal/registry"
)

func (s *Server) toolSearch(args json.RawMessage) MCPToolResult {
	var input searchInput
	if err := json.Unmarshal(args, &input); err != nil {
		return ErrResult("invalid arguments: " + err.Error())
	}
	if input.Query == "" {
		return ErrResult("query is required")
	}
	if input.Limit <= 0 {
		input.Limit = 5
	}

	ctx := context.Background()
	client := heimdall.NewOllamaClient(s.Cfg.OllamaEndpoint)
	if err := client.Ping(ctx); err != nil {
		return ErrResult("Ollama not reachable at " + s.Cfg.OllamaEndpoint + ": " + err.Error())
	}

	// Resolve DB directory (read-only)
	dbDir := s.resolveDBDirForRead(input.Project)
	if dbDir == "" {
		// 4.1: Auto-index on first search
		return s.autoIndexOnSearch()
	}

	store, err := heimdall.OpenStore(dbDir)
	if err != nil {
		return ErrResult("store error: " + err.Error())
	}
	defer store.Close()

	embedder := heimdall.NewOllamaEmbedder(client, s.Cfg.Model)

	// If filters are present, use filtered search path
	if input.SourceType != "" || len(input.MetadataFilter) > 0 {
		return s.toolSearchFiltered(ctx, input, store, embedder)
	}

	// Standard retriever path (with enriched results)
	retriever := heimdall.NewRetriever(embedder, store, input.Limit, s.Cfg.MaxContextTokens)

	blocks, err := retriever.Retrieve(ctx, input.Query)
	if err != nil {
		return ErrResult("search error: " + err.Error())
	}

	if len(blocks) == 0 {
		return TextResult("No relevant results found.")
	}

	// Update last_accessed for matched records (best-effort)
	var matchedIDs []string
	for _, b := range blocks {
		matchedIDs = append(matchedIDs, b.ChunkID)
	}
	if err := store.UpdateLastAccessed(matchedIDs); err != nil {
		log.Printf("last_accessed update error: %v", err)
	}

	// Run lifecycle if throttle window has elapsed (synchronous — just SQL)
	if store.ShouldRunLifecycle() {
		lcCfg := heimdall.LifecycleConfig{ActiveDays: 30, ArchiveDays: 90, MaxChunks: 10000}
		if lcResult, lcErr := heimdall.RunLifecycle(store, lcCfg); lcErr != nil {
			log.Printf("lifecycle error: %v", lcErr)
		} else if lcResult.Archived+lcResult.Pruned+lcResult.Capped > 0 {
			log.Printf("lifecycle: archived=%d pruned=%d capped=%d", lcResult.Archived, lcResult.Pruned, lcResult.Capped)
		}
		store.MarkLifecycleRun()
	}

	var results []SearchResultEnriched
	for _, b := range blocks {
		results = append(results, SearchResultEnriched{
			File:           b.FilePath,
			StartLine:      b.StartLine,
			EndLine:        b.EndLine,
			Content:        b.Content,
			Score:          b.Score,
			Source:         classifySource(b.Kind),
			ChunkID:        b.ChunkID,
			EmbeddingModel: s.Cfg.Model,
		})
	}

	// 4.3: Stale index check — trigger background re-index if stale
	staleNote := s.checkAndTriggerReindex(store, dbDir)

	if staleNote != "" {
		response := map[string]any{
			"results": results,
			"note":    staleNote,
		}
		out, _ := json.MarshalIndent(response, "", "  ")
		return TextResult(string(out))
	}

	out, _ := json.MarshalIndent(results, "", "  ")
	return TextResult(string(out))
}

func (s *Server) toolSearchFiltered(ctx context.Context, input searchInput, store *heimdall.VectorStore, embedder heimdall.Embedder) MCPToolResult {
	queryVec, err := embedder.Embed(ctx, input.Query)
	if err != nil {
		return ErrResult("embedding error: " + err.Error())
	}

	var metaFilter map[string]any
	if len(input.MetadataFilter) > 0 {
		json.Unmarshal(input.MetadataFilter, &metaFilter)
	}

	searchResults := store.SearchFiltered(queryVec, input.Limit, input.SourceType, metaFilter)

	if len(searchResults) == 0 {
		return TextResult("No relevant results found.")
	}

	// Update last_accessed for matched records (best-effort)
	var filteredIDs []string
	for _, r := range searchResults {
		filteredIDs = append(filteredIDs, r.Record.ID)
	}
	if err := store.UpdateLastAccessed(filteredIDs); err != nil {
		log.Printf("last_accessed update error: %v", err)
	}

	// Run lifecycle if throttle window has elapsed (synchronous — just SQL)
	if store.ShouldRunLifecycle() {
		lcCfg := heimdall.LifecycleConfig{ActiveDays: 30, ArchiveDays: 90, MaxChunks: 10000}
		if lcResult, lcErr := heimdall.RunLifecycle(store, lcCfg); lcErr != nil {
			log.Printf("lifecycle error: %v", lcErr)
		} else if lcResult.Archived+lcResult.Pruned+lcResult.Capped > 0 {
			log.Printf("lifecycle: archived=%d pruned=%d capped=%d", lcResult.Archived, lcResult.Pruned, lcResult.Capped)
		}
		store.MarkLifecycleRun()
	}

	var enriched []SearchResultEnriched
	for _, r := range searchResults {
		enriched = append(enriched, SearchResultEnriched{
			File:           r.Record.FilePath,
			StartLine:      r.Record.StartLine,
			EndLine:        r.Record.EndLine,
			Content:        r.Record.Content,
			Score:          r.Similarity,
			Source:         classifySource(r.Record.Kind),
			ChunkID:        r.Record.ID,
			EmbeddingModel: s.Cfg.Model,
		})
	}

	out, _ := json.MarshalIndent(enriched, "", "  ")
	return TextResult(string(out))
}

func (s *Server) toolIndex(args json.RawMessage) MCPToolResult {
	var input indexInput
	if err := json.Unmarshal(args, &input); err != nil {
		return ErrResult("invalid arguments: " + err.Error())
	}
	if input.Path == "" {
		return ErrResult("path is required")
	}

	absPath, err := filepath.Abs(input.Path)
	if err != nil {
		return ErrResult("invalid path: " + err.Error())
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return ErrResult("path does not exist: " + absPath)
	}
	if !info.IsDir() {
		return ErrResult("path is not a directory: " + absPath)
	}

	s.Index.Mu.Lock()
	if s.Index.Running {
		s.Index.Mu.Unlock()
		return TextResult(fmt.Sprintf("Indexing already in progress: %s (%d/%d files)\nUse heimdall_status to check progress.",
			s.Index.Path, s.Index.Current, s.Index.Total))
	}

	if s.Index.Result != nil && s.Index.Path == absPath {
		r := s.Index.Result
		e := s.Index.Err
		s.Index.Result = nil
		s.Index.Err = nil
		s.Index.Mu.Unlock()

		if e != nil {
			return ErrResult("previous indexing failed: " + e.Error())
		}
		out, _ := json.MarshalIndent(map[string]any{
			"filesScanned":  r.FilesScanned,
			"filesIndexed":  r.FilesIndexed,
			"chunksCreated": r.ChunksCreated,
			"duration":      r.Duration.Round(time.Millisecond).String(),
		}, "", "  ")
		return TextResult(string(out))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
	s.Index.Running = true
	s.Index.Path = absPath
	s.Index.Current = 0
	s.Index.Total = 0
	s.Index.StartedAt = time.Now()
	s.Index.LastUpdate = time.Now()
	s.Index.Result = nil
	s.Index.Err = nil
	s.Index.Cancel = cancel
	s.Index.Mu.Unlock()

	go s.runIndex(ctx, absPath)

	return TextResult(fmt.Sprintf("Indexing started in background: %s\nUse heimdall_status to check progress. Call heimdall_index again when done to get results.", absPath))
}

func (s *Server) runIndex(ctx context.Context, absPath string) {
	defer func() {
		s.Index.Mu.Lock()
		if s.Index.Cancel != nil {
			s.Index.Cancel()
			s.Index.Cancel = nil
		}
		s.Index.Mu.Unlock()
	}()

	client := heimdall.NewOllamaClient(s.Cfg.OllamaEndpoint)

	if err := client.Ping(ctx); err != nil {
		s.Index.Mu.Lock()
		s.Index.Running = false
		s.Index.Err = fmt.Errorf("Ollama not reachable: %v", err)
		s.Index.Mu.Unlock()
		return
	}

	cwd, _ := os.Getwd()
	dbDir := filepath.Join(cwd, ".heimdall_db")
	store, err := heimdall.OpenStore(dbDir)
	if err != nil {
		s.Index.Mu.Lock()
		s.Index.Running = false
		s.Index.Err = err
		s.Index.Mu.Unlock()
		return
	}
	defer store.Close()

	embedder := heimdall.NewOllamaEmbedder(client, s.Cfg.Model)
	indexer := heimdall.NewIndexer(absPath, embedder, store, heimdall.ChunkerOpts{
		MaxChunkSize: 1500,
		ContextDepth: s.Cfg.ContextDepth,
		ExcludeGlobs: s.Cfg.ExcludePatterns,
	})

	progressCh := make(chan heimdall.IndexProgress, 64)
	indexer.IndexProjectAsync(ctx, progressCh)

	const stallTimeout = 30 * time.Minute

	for {
		select {
		case p, ok := <-progressCh:
			if !ok {
				return
			}
			s.Index.Mu.Lock()
			s.Index.Current = p.Current
			s.Index.Total = p.Total
			s.Index.LastUpdate = time.Now()
			if p.Done {
				s.Index.Running = false
				s.Index.Result = p.Result
				s.Index.Err = p.Err
			}
			s.Index.Mu.Unlock()
			if p.Done {
				// Register project in registry after successful indexing
				if p.Err == nil {
					name := filepath.Base(absPath)
					s.Registry.Register(name, absPath, dbDir)
					if err := s.Registry.Save(); err != nil {
						log.Printf("failed to save registry: %v", err)
					}

					// 4.2: Auto-detect git repo and index commits
					gitDir := filepath.Join(absPath, ".git")
					if info, err := os.Stat(gitDir); err == nil && info.IsDir() {
						gitResult, gitErr := heimdall.IndexGitCommits(ctx, absPath, 200, embedder, store)
						if gitErr != nil {
							log.Printf("git commit indexing failed: %v", gitErr)
						} else {
							log.Printf("git commit indexing: %d commits, %d chunks", gitResult.CommitsIndexed, gitResult.ChunksCreated)
						}
					}
				}
				return
			}

		case <-time.After(stallTimeout):
			s.Index.Mu.Lock()
			elapsed := time.Since(s.Index.LastUpdate)
			s.Index.Mu.Unlock()

			if elapsed >= stallTimeout {
				log.Printf("indexing stalled for %v, cancelling", elapsed)
				s.Index.Mu.Lock()
				s.Index.Running = false
				s.Index.Err = fmt.Errorf("indexing stalled: no progress for %v at file %d/%d", elapsed, s.Index.Current, s.Index.Total)
				s.Index.Mu.Unlock()
				return
			}

		case <-ctx.Done():
			s.Index.Mu.Lock()
			s.Index.Running = false
			s.Index.Err = fmt.Errorf("indexing timed out after %v", time.Since(s.Index.StartedAt).Round(time.Second))
			s.Index.Mu.Unlock()
			return
		}
	}
}

// staleTimeoutSeconds is the default threshold after which an index is considered stale.
// If the last modified time of the index is older than this, a background re-index is triggered.
const staleTimeoutSeconds = 1800 // 30 minutes

// autoIndexOnSearch triggers auto-indexing when no index exists and a search is attempted.
// Returns a friendly message indicating indexing has started.
func (s *Server) autoIndexOnSearch() MCPToolResult {
	cwd, err := os.Getwd()
	if err != nil {
		return ErrResult("No index found and cannot determine working directory: " + err.Error())
	}

	absPath, err := filepath.Abs(cwd)
	if err != nil {
		return ErrResult("No index found and cannot resolve path: " + err.Error())
	}

	info, err := os.Stat(absPath)
	if err != nil || !info.IsDir() {
		return ErrResult("No index found. Run heimdall_index first.")
	}

	// Check if indexing is already running
	s.Index.Mu.Lock()
	if s.Index.Running {
		s.Index.Mu.Unlock()
		out, _ := json.MarshalIndent(map[string]any{
			"status":  "indexing_in_progress",
			"message": fmt.Sprintf("Indexing already in progress for %s. Use heimdall_status to check progress, then retry your search.", s.Index.Path),
		}, "", "  ")
		return TextResult(string(out))
	}

	// Start background indexing
	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
	s.Index.Running = true
	s.Index.Path = absPath
	s.Index.Current = 0
	s.Index.Total = 0
	s.Index.StartedAt = time.Now()
	s.Index.LastUpdate = time.Now()
	s.Index.Result = nil
	s.Index.Err = nil
	s.Index.Cancel = cancel
	s.Index.Mu.Unlock()

	go s.runIndex(ctx, absPath)

	out, _ := json.MarshalIndent(map[string]any{
		"status":  "indexing_started",
		"message": fmt.Sprintf("No index found. Auto-indexing started for %s. Use heimdall_status to check progress, then retry your search.", absPath),
	}, "", "  ")
	return TextResult(string(out))
}

// checkAndTriggerReindex checks if the index is stale and triggers a background re-index.
// Returns a note string if the index is stale, or empty string if fresh.
func (s *Server) checkAndTriggerReindex(store *heimdall.VectorStore, dbDir string) string {
	stats := store.Stats()
	if stats.LastModified == 0 {
		return ""
	}

	age := time.Now().Unix() - stats.LastModified
	if age <= staleTimeoutSeconds {
		return ""
	}

	// Find the project path from registry
	var projectPath string
	for _, p := range s.Registry.All() {
		if p.DBPath == dbDir {
			projectPath = p.Path
			break
		}
	}
	if projectPath == "" {
		return ""
	}

	// Check if indexing is already in progress
	s.Index.Mu.Lock()
	if s.Index.Running {
		s.Index.Mu.Unlock()
		return fmt.Sprintf("Index is stale (>%d min). Re-index already in progress.", staleTimeoutSeconds/60)
	}

	// Trigger background re-index
	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
	s.Index.Running = true
	s.Index.Path = projectPath
	s.Index.Current = 0
	s.Index.Total = 0
	s.Index.StartedAt = time.Now()
	s.Index.LastUpdate = time.Now()
	s.Index.Result = nil
	s.Index.Err = nil
	s.Index.Cancel = cancel
	s.Index.Mu.Unlock()

	go s.runIndex(ctx, projectPath)

	return fmt.Sprintf("Index is stale (>%d min). Background re-index started.", staleTimeoutSeconds/60)
}

func (s *Server) toolStatus() MCPToolResult {
	ctx := context.Background()
	endpoint := s.Cfg.OllamaEndpoint
	model := s.Cfg.Model

	status := map[string]any{
		"endpoint":        endpoint,
		"model":           model,
		"excludePatterns": s.Cfg.ExcludePatterns,
	}

	client := heimdall.NewOllamaClient(endpoint)
	if err := client.Ping(ctx); err != nil {
		status["ollamaRunning"] = false
	} else {
		status["ollamaRunning"] = true
		models, err := client.ListModels(ctx)
		if err == nil {
			hasModel := false
			for _, m := range models {
				if m.Name == model || len(m.Name) > len(model) && m.Name[:len(model)] == model {
					hasModel = true
					break
				}
			}
			status["modelAvailable"] = hasModel
		}
	}

	cwd, _ := os.Getwd()
	dbDir := filepath.Join(cwd, ".heimdall_db")
	if _, err := os.Stat(dbDir); err == nil {
		store, err := heimdall.OpenStore(dbDir)
		if err == nil {
			defer store.Close()
			stats := store.Stats()
			status["indexedFiles"] = stats.TotalFiles
			status["totalChunks"] = stats.TotalRecords
			if stats.LastModified > 0 {
				status["lastIndexed"] = time.Unix(stats.LastModified, 0).Format("2006-01-02 15:04:05")
			} else {
				status["lastIndexed"] = "never"
			}
		}
	} else {
		status["indexedFiles"] = 0
		status["totalChunks"] = 0
		status["lastIndexed"] = "no index"
	}

	// Registered projects
	projects := s.Registry.All()
	if len(projects) > 0 {
		var projectList []map[string]string
		for _, p := range projects {
			entry := map[string]string{
				"name":   p.Name,
				"path":   p.Path,
				"dbPath": p.DBPath,
			}
			// Highlight if CWD is inside this project
			if registry.IsSubpath(cwd, p.Path) {
				entry["current"] = "true"
			}
			projectList = append(projectList, entry)
		}
		status["registeredProjects"] = projectList
	}

	// Indexing state
	s.Index.Mu.Lock()
	if s.Index.Running {
		status["indexing"] = true
		status["indexingPath"] = s.Index.Path
		status["indexingProgress"] = fmt.Sprintf("%d/%d files", s.Index.Current, s.Index.Total)
		status["indexingElapsed"] = time.Since(s.Index.StartedAt).Round(time.Second).String()
		sinceUpdate := time.Since(s.Index.LastUpdate).Round(time.Second)
		status["timeSinceLastProgress"] = sinceUpdate.String()
		if sinceUpdate > 2*time.Minute {
			status["warning"] = "indexing may be stalled — no progress for " + sinceUpdate.String()
		}
	} else if s.Index.Result != nil {
		status["indexing"] = false
		status["lastIndexResult"] = map[string]any{
			"path":          s.Index.Path,
			"filesScanned":  s.Index.Result.FilesScanned,
			"filesIndexed":  s.Index.Result.FilesIndexed,
			"chunksCreated": s.Index.Result.ChunksCreated,
			"duration":      s.Index.Result.Duration.Round(time.Millisecond).String(),
		}
		if s.Index.Err != nil {
			status["lastIndexError"] = s.Index.Err.Error()
		}
	} else if s.Index.Err != nil {
		status["indexing"] = false
		status["lastIndexError"] = s.Index.Err.Error()
		status["lastIndexPath"] = s.Index.Path
	}
	s.Index.Mu.Unlock()

	out, _ := json.MarshalIndent(status, "", "  ")
	return TextResult(string(out))
}
