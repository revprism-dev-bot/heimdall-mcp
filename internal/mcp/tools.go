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

// sanitizeStoreError logs the full error server-side and returns a generic
// message to the client to avoid leaking internal paths.
func sanitizeStoreError(action string, err error) MCPToolResult {
	log.Printf("heimdall: %s error: %v", action, err)
	return ErrResult(action + " error — check server logs for details")
}

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
	client := s.newOllamaClient()
	if err := client.Ping(ctx); err != nil {
		return ollamaSetupError(s.Cfg.OllamaEndpoint, s.Cfg.Model, err)
	}

	// Auto-resolve: find any available index whose model is pulled in Ollama
	dbDir, resolvedModel := s.resolveAnyModelDB(input.Project)
	if dbDir == "" {
		// No usable index — check if any indexes exist at all
		baseDir := s.resolveDBDir(input.Project)
		available := heimdall.ListAvailableModels(baseDir)
		if len(available) > 0 {
			return ErrResult(fmt.Sprintf("Indexes exist for %v but none of those models are pulled in Ollama. Run: ollama pull <model>", available))
		}
		// No index at all — auto-index
		return s.autoIndexOnSearch()
	}

	store, err := heimdall.OpenStore(dbDir)
	if err != nil {
		return sanitizeStoreError("store", err)
	}
	defer store.Close()

	embedder := heimdall.NewOllamaEmbedder(client, resolvedModel)

	if len(input.SubProject) > 255 {
		return ErrResult("sub_project too long (max 255 chars)")
	}

	// If filters are present, use filtered search path
	if input.SourceType != "" || input.SubProject != "" || len(input.MetadataFilter) > 0 || input.Detail != "" || input.Scope != "" {
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

	var notes []string
	if staleNote != "" {
		notes = append(notes, staleNote)
	}

	if len(notes) > 0 {
		response := map[string]any{
			"results":  results,
			"warnings": notes,
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

	var searchOpts []heimdall.SearchOption
	if input.Scope != "" {
		searchOpts = append(searchOpts, heimdall.WithScope(input.Scope))
	}
	if input.Detail != "" {
		searchOpts = append(searchOpts, heimdall.WithDetail(input.Detail))
	}
	searchResults := store.SearchFiltered(ctx, queryVec, input.Limit, input.SourceType, input.SubProject, metaFilter, searchOpts...)

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
			Summary:        r.Record.Summary,
			ContextPath:    r.Record.ContextPath,
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

	return TextResult(fmt.Sprintf("Indexing started in background: %s\nModel: %s\nUse heimdall_status to check progress. Call heimdall_index again when done to get results.", absPath, s.Cfg.Model))
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

	client := s.newOllamaClient()

	if err := client.Ping(ctx); err != nil {
		s.Index.Mu.Lock()
		s.Index.Running = false
		s.Index.Err = fmt.Errorf("Ollama not reachable: %v", err)
		s.Index.Mu.Unlock()
		return
	}

	// Route the write through the registry-first resolver. Pass absPath
	// as both the hint AND as the fallback-constructor source so users
	// who index a directory that isn't yet registered still get
	// baseDir = <absPath>/.heimdall_db (NOT cwd/.heimdall_db). Fixes the
	// handoff Problem #1 symptom where reindex wrote to the MCP server's
	// working directory instead of the target project.
	baseDir := s.resolveRunIndexBaseDir(absPath)
	heimdall.MigrateToModelDir(baseDir, s.Cfg.Model)
	if _, err := heimdall.MigrateLegacyLatestDir(baseDir, s.Cfg.Model); err != nil {
		log.Printf("legacy-latest migration warning for %s: %v", baseDir, err)
	}
	dbDir := heimdall.ModelDBDir(baseDir, s.Cfg.Model)
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
					s.Registry.Register(name, absPath, baseDir)

					// Stamp model metadata so we can detect mismatches later
					store.SetMetadata("embedding_model", s.Cfg.Model)
					if dim := store.GetMetadata("embedding_dim"); dim == "" {
						// Do a test embed to record dimension
						if vec, err := embedder.Embed(ctx, "test"); err == nil {
							store.SetMetadata("embedding_dim", fmt.Sprintf("%d", len(vec)))
						}
					}

					// 4.2: Auto-detect git repo and index commits (outer)
					gitDir := filepath.Join(absPath, ".git")
					if info, err := os.Stat(gitDir); err == nil && info.IsDir() {
						gitResult, gitErr := heimdall.IndexGitCommits(ctx, absPath, 200, embedder, store)
						if gitErr != nil {
							log.Printf("git commit indexing failed: %v", gitErr)
						} else {
							log.Printf("git commit indexing: %d commits, %d chunks", gitResult.CommitsIndexed, gitResult.ChunksCreated)
						}
					}

					// Sub-repo pass: index each immediate sub-repo as its own
					// project, register each, and index its git commits INTO
					// ITS OWN STORE (fixes the tools.go:362 bug where sub-repo
					// commits were being written to the outer store).
					subResults, subErr := indexer.IndexSubRepos(ctx, s.Cfg.Model, heimdall.SubRepoOpts{})
					if subErr != nil {
						log.Printf("sub-repo discovery failed: %v", subErr)
					}
					for _, sr := range subResults {
						if sr.Err != nil || sr.Result == nil {
							log.Printf("sub-repo %s failed: %v", sr.Path, sr.Err)
							continue
						}
						s.Registry.Register(sr.Name, sr.Path, sr.DBPath)

						// Open the sub-repo's OWN store for git-commit indexing.
						subDBDir := heimdall.ModelDBDir(sr.DBPath, sr.Model)
						subStore, err := heimdall.OpenStore(subDBDir)
						if err != nil {
							log.Printf("open sub-repo store for git commits %s: %v", subDBDir, err)
							continue
						}
						subGitDir := filepath.Join(sr.Path, ".git")
						if _, err := os.Stat(subGitDir); err == nil {
							subEmbedder := embedder
							if sr.Model != s.Cfg.Model {
								// Rebuild the embedder for the pinned model so
								// we do not embed with a model whose dims
								// mismatch the sub-repo's store.
								subEmbedder = heimdall.NewOllamaEmbedder(client, sr.Model)
							}
							gitResult, gitErr := heimdall.IndexGitCommits(ctx, sr.Path, 200, subEmbedder, subStore)
							if gitErr != nil {
								log.Printf("git commit indexing for %s failed: %v", sr.Name, gitErr)
							} else if gitResult.CommitsIndexed > 0 {
								log.Printf("git commit indexing for %s: %d commits, %d chunks", sr.Name, gitResult.CommitsIndexed, gitResult.ChunksCreated)
							}
						}
						if closeErr := subStore.Close(); closeErr != nil {
							log.Printf("close sub-repo store %s: %v", subDBDir, closeErr)
						}
					}

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

// staleTimeoutSeconds is the default threshold after which an index is considered stale.
// If the last modified time of the index is older than this, a background re-index is triggered.
const staleTimeoutSeconds = 1800 // 30 minutes

// autoIndexOnSearch triggers auto-indexing when no index exists and a search is attempted.
// Returns a friendly message indicating indexing has started.
//
// Target-path invariant (PR1): this is the single authorized os.Getwd()
// site in the MCP write paths. The Phase 4 behaviour expects auto-index
// to operate on the server's CWD when no project context is available.
// All OTHER MCP write paths (runIndex, toolIndexText, toolStatus) MUST
// resolve through s.resolveDBDir(project). See plan D-17.
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
		"model":   s.Cfg.Model,
		"message": fmt.Sprintf("No index found. Auto-indexing started for %s using model %q. Use heimdall_status to check progress, then retry your search.", absPath, s.Cfg.Model),
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

	// Find the project path from registry.
	// dbDir may be a model-specific subdirectory of the registered DBPath,
	// so check both exact match and parent match.
	var projectPath string
	for _, p := range s.Registry.All() {
		if p.DBPath == dbDir || registry.IsSubpath(dbDir, p.DBPath) {
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

// resolveRunIndexBaseDir returns the baseDir for a write-path tool when
// the caller provided an absolute target path. Registry-first: if the
// target path is already registered EXACTLY (no substring match), reuse
// its DBPath so writes land exactly where the registry already claims
// they do. Otherwise default to <absPath>/.heimdall_db — NEVER
// os.Getwd(). This is the target-path invariant the handoff Problem #1
// fix depends on.
//
// We intentionally DO NOT use Registry.Find(absPath) here because it
// also performs partial substring name matches, which could return the
// wrong project when an absolute filesystem path happens to share a
// substring with a registered project name. Exact path equality is
// required for the invariant to hold.
func (s *Server) resolveRunIndexBaseDir(absPath string) string {
	if absPath != "" {
		for _, p := range s.Registry.All() {
			if p.Path == absPath {
				return p.DBPath
			}
		}
		return filepath.Join(absPath, ".heimdall_db")
	}
	// Last-ditch: fall through to the shared resolver, which ends at
	// <cwd>/.heimdall_db only if nothing else matches.
	return s.resolveDBDir("")
}

// resolveStatusBaseDir returns the baseDir for heimdall_status. It
// mirrors the resolveRunIndexBaseDir exact-match contract for absolute
// paths, fixing PR #73 review HIGH finding: status previously routed
// through resolveDBDir, which uses Registry.Find with case-insensitive
// substring name matching. That meant `heimdall_status path=/srv/auth`
// could silently return stats for a registered project `auth-service`.
//
// Resolution order:
//  1. If input is an absolute directory that exists on disk, return an
//     exact-path registry match OR <input>/.heimdall_db. Substring /
//     name fuzzy matching is bypassed entirely.
//  2. If input is a non-empty string that is NOT an absolute path,
//     treat it as a project name and route through resolveDBDir
//     (registry name/path exact match → substring match → cwd
//     fallback — the existing behavior for name-style input is
//     preserved).
//  3. If input is empty, fall back to resolveDBDir("") — the cwd
//     chain (FindByCWD → <cwd>/.heimdall_db).
func (s *Server) resolveStatusBaseDir(input string) string {
	if input == "" {
		return s.resolveDBDir("")
	}
	if filepath.IsAbs(input) {
		for _, p := range s.Registry.All() {
			if p.Path == input {
				return p.DBPath
			}
		}
		return filepath.Join(input, ".heimdall_db")
	}
	// Name-style input — delegate to the shared resolver. A purely
	// name-based substring match is intentional here (users call
	// `heimdall_status project=my-proj` expecting a loose lookup).
	return s.resolveDBDir(input)
}

// toolStatus serves heimdall_status. In PR1 it gained an optional `path`
// input parameter. Resolution routes through s.resolveStatusBaseDir,
// which mirrors the resolveRunIndexBaseDir exact-match contract for
// absolute paths — so `heimdall_status path=/srv/auth` cannot be
// misrouted to an unrelated registered project named `auth-service` via
// Registry.Find's substring matching (PR #73 review HIGH finding). Does
// NOT use s.Index.Path as fallback because autoIndexOnSearch writes the
// server's cwd into it on unindexed searches (tools.go:464),
// contaminating the signal.
func (s *Server) toolStatus(args json.RawMessage) MCPToolResult {
	var input statusInput
	if len(args) > 0 {
		if err := json.Unmarshal(args, &input); err != nil {
			return ErrResult("invalid arguments: " + err.Error())
		}
	}
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

	baseDir := s.resolveStatusBaseDir(input.Path)
	status["dbPath"] = baseDir

	// Show all available model DBs for this project
	available := heimdall.ListAvailableModels(baseDir)
	if len(available) > 0 {
		status["availableModels"] = available
	}

	// Show stats for the current model's DB
	modelDir := heimdall.ModelDBDir(baseDir, model)
	if _, err := os.Stat(modelDir); err == nil {
		store, err := heimdall.OpenStore(modelDir)
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
			if indexDim := store.GetMetadata("embedding_dim"); indexDim != "" {
				status["indexEmbeddingDim"] = indexDim
			}
		}
	} else {
		status["indexedFiles"] = 0
		status["totalChunks"] = 0
		status["lastIndexed"] = "no index"
	}

	// Registered projects — show `current:true` when CWD is inside the
	// project (best-effort; cwd may be unavailable in some sandboxes).
	cwd, _ := os.Getwd()
	projects := s.Registry.All()
	if len(projects) > 0 {
		var projectList []map[string]string
		for _, p := range projects {
			entry := map[string]string{
				"name":   p.Name,
				"path":   p.Path,
				"dbPath": p.DBPath,
			}
			if cwd != "" && registry.IsSubpath(cwd, p.Path) {
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

func (s *Server) toolExpand(args json.RawMessage) MCPToolResult {
	var input expandInput
	if err := json.Unmarshal(args, &input); err != nil {
		return ErrResult("invalid arguments: " + err.Error())
	}
	if input.ChunkID == "" {
		return ErrResult("chunk_id is required")
	}

	dbDir, _ := s.resolveAnyModelDB(input.Project)
	if dbDir == "" {
		return ErrResult("no index found")
	}

	store, err := heimdall.OpenStore(dbDir)
	if err != nil {
		return ErrResult("store error")
	}
	defer store.Close()

	rec, err := store.ExpandByID(input.ChunkID)
	if err != nil {
		return ErrResult("chunk not found: " + input.ChunkID)
	}

	result := map[string]any{
		"chunkId":     rec.ID,
		"file":        rec.FilePath,
		"startLine":   rec.StartLine,
		"endLine":     rec.EndLine,
		"content":     rec.Content,
		"kind":        rec.Kind,
		"identifier":  rec.Identifier,
		"sourceType":  rec.SourceType,
		"summary":     rec.Summary,
		"contextPath": rec.ContextPath,
	}
	if rec.Metadata != "" && rec.Metadata != "{}" {
		result["metadata"] = json.RawMessage(rec.Metadata)
	}
	if rec.Relationships != "" && rec.Relationships != "[]" {
		result["relationships"] = json.RawMessage(rec.Relationships)
	}

	out, _ := json.MarshalIndent(result, "", "  ")
	return TextResult(string(out))
}

func (s *Server) toolLs(args json.RawMessage) MCPToolResult {
	var input lsInput
	if err := json.Unmarshal(args, &input); err != nil {
		return ErrResult("invalid arguments: " + err.Error())
	}

	dbDir, _ := s.resolveAnyModelDB(input.Project)
	if dbDir == "" {
		return ErrResult("no index found")
	}

	store, err := heimdall.OpenStore(dbDir)
	if err != nil {
		return ErrResult("store error")
	}
	defer store.Close()

	entries := store.ListByContextPath(input.Path)
	if len(entries) == 0 {
		return TextResult("No entries at this path.")
	}

	out, _ := json.MarshalIndent(entries, "", "  ")
	return TextResult(string(out))
}
