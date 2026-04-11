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

	// Resolve DB directory using registry
	// 1. If project param given, look up in registry
	// 2. If no param, try registry.FindByCWD(cwd)
	// 3. Fallback: cwd/.heimdall_db/
	cwd, _ := os.Getwd()
	dbDir := ""

	if input.Project != "" {
		if entry := s.Registry.Find(input.Project); entry != nil {
			dbDir = entry.DBPath
		} else {
			return ErrResult(fmt.Sprintf("Project %q not found in registry. Run list_projects to see registered projects.", input.Project))
		}
	}

	if dbDir == "" {
		if entry := s.Registry.FindByCWD(cwd); entry != nil {
			dbDir = entry.DBPath
		}
	}

	if dbDir == "" {
		dbDir = filepath.Join(cwd, ".heimdall_db")
	}

	if _, err := os.Stat(dbDir); err != nil {
		return ErrResult("No index found. Run index_project first.")
	}

	store, err := heimdall.OpenStore(dbDir)
	if err != nil {
		return ErrResult("store error: " + err.Error())
	}
	defer store.Close()

	embedder := heimdall.NewOllamaEmbedder(client, s.Cfg.Model)
	retriever := heimdall.NewRetriever(embedder, store, input.Limit, s.Cfg.MaxContextTokens)

	blocks, err := retriever.Retrieve(ctx, input.Query)
	if err != nil {
		return ErrResult("search error: " + err.Error())
	}

	if len(blocks) == 0 {
		return TextResult("No relevant results found.")
	}

	type result struct {
		File      string  `json:"file"`
		StartLine int     `json:"startLine"`
		EndLine   int     `json:"endLine"`
		Content   string  `json:"content"`
		Score     float64 `json:"score"`
	}
	var results []result
	for _, b := range blocks {
		results = append(results, result{
			File:      b.FilePath,
			StartLine: b.StartLine,
			EndLine:   b.EndLine,
			Content:   b.Content,
			Score:     b.Score,
		})
	}

	out, _ := json.MarshalIndent(results, "", "  ")
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
