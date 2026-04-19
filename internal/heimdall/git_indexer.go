package heimdall

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// GitIndexResult holds statistics from a git commit indexing run.
type GitIndexResult struct {
	CommitsIndexed int
	ChunksCreated  int
}

// gitCommit represents a parsed git commit.
type gitCommit struct {
	Hash      string
	Subject   string
	Body      string
	Timestamp int64
}

// IndexGitCommits indexes recent git commits from a repository for semantic search.
// It runs `git log` to extract commits, embeds them, and upserts into the store.
// depth controls how many commits to index.
//
// Deprecated: pass a sub_project tag via IndexGitCommitsWithSubProject so
// commit rows can be filtered by sub_project consistently with file-code
// rows. This thin wrapper preserves existing callers and stamps
// sub_project="" (outer/wrapper semantics).
func IndexGitCommits(ctx context.Context, repoPath string, depth int, embedder Embedder, store *VectorStore) (*GitIndexResult, error) {
	return IndexGitCommitsWithSubProject(ctx, repoPath, depth, embedder, store, "")
}

// IndexGitCommitsWithSubProject is the explicit-sub-project form of
// IndexGitCommits. Every commit record produced carries subProject in its
// SubProject field so search filters (`sub_project = ?`) return the
// expected rows. Pass "" for outer/wrapper stores; pass the sub-repo name
// (e.g. filepath.Base(sr.Path)) when writing into a sub-repo store.
// Closes handoff Problem #2 for the commit writer.
func IndexGitCommitsWithSubProject(ctx context.Context, repoPath string, depth int, embedder Embedder, store *VectorStore, subProject string) (*GitIndexResult, error) {
	if depth <= 0 {
		depth = 200
	}

	commits, err := parseGitLog(ctx, repoPath, depth)
	if err != nil {
		return nil, fmt.Errorf("git log: %w", err)
	}

	if len(commits) == 0 {
		return &GitIndexResult{}, nil
	}

	// Build the text-per-commit list once so we can feed it to either the
	// batch path or the per-commit fallback without rebuilding strings.
	contents := make([]string, len(commits))
	for i, c := range commits {
		contents[i] = c.Subject
		if c.Body != "" {
			contents[i] += "\n\n" + c.Body
		}
	}

	vectors := make([][]float32, len(commits))
	if batchEmb, ok := embedder.(BatchEmbedder); ok {
		vecs, embedErr := batchEmb.EmbedBatch(ctx, contents)
		if embedErr != nil {
			// Fall through to the per-commit path so a transient batch
			// failure does not abandon the whole run.
			for i, content := range contents {
				vec, err := embedder.Embed(ctx, content)
				if err != nil {
					continue
				}
				vectors[i] = vec
			}
		} else {
			copy(vectors, vecs)
		}
	} else {
		for i, content := range contents {
			vec, err := embedder.Embed(ctx, content)
			if err != nil {
				continue
			}
			vectors[i] = vec
		}
	}

	var records []VectorRecord
	for i, c := range commits {
		if vectors[i] == nil {
			continue
		}
		shortHash := c.Hash
		if len(shortHash) > 12 {
			shortHash = shortHash[:12]
		}
		records = append(records, VectorRecord{
			ID:            fmt.Sprintf("commit:%s", shortHash),
			FilePath:      "git:commit",
			StartLine:     0,
			EndLine:       0,
			Content:       contents[i],
			Kind:          "commit",
			Identifier:    shortHash,
			Embedding:     vectors[i],
			ModTime:       c.Timestamp,
			ContentHash:   "",
			SourceType:    "commit",
			Metadata:      fmt.Sprintf(`{"hash":"%s"}`, c.Hash),
			Relationships: "[]",
			SubProject:    subProject,
		})
	}

	if len(records) > 0 {
		if err := store.Upsert(records); err != nil {
			return nil, fmt.Errorf("upsert commits: %w", err)
		}
	}

	return &GitIndexResult{
		CommitsIndexed: len(commits),
		ChunksCreated:  len(records),
	}, nil
}

// parseGitLog runs git log and parses the output into commits.
func parseGitLog(ctx context.Context, repoPath string, depth int) ([]gitCommit, error) {
	// Use a format that includes hash, subject, body, and timestamp with clear delimiters
	cmd := exec.CommandContext(ctx, "git", "log",
		fmt.Sprintf("--pretty=format:%%H%%n%%at%%n%%s%%n%%b%%n---COMMIT_END---"),
		fmt.Sprintf("-%d", depth),
	)
	cmd.Dir = repoPath

	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	return parseGitLogOutput(string(output)), nil
}

// parseGitLogOutput parses the raw git log output into gitCommit structs.
// Exported for testing.
func parseGitLogOutput(output string) []gitCommit {
	if strings.TrimSpace(output) == "" {
		return nil
	}

	blocks := strings.Split(output, "---COMMIT_END---")
	var commits []gitCommit

	for _, block := range blocks {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}

		lines := strings.SplitN(block, "\n", 4)
		if len(lines) < 3 {
			continue
		}

		hash := strings.TrimSpace(lines[0])
		if hash == "" {
			continue
		}

		timestamp, _ := strconv.ParseInt(strings.TrimSpace(lines[1]), 10, 64)
		if timestamp == 0 {
			timestamp = time.Now().Unix()
		}

		subject := strings.TrimSpace(lines[2])
		if subject == "" {
			continue
		}

		var body string
		if len(lines) > 3 {
			body = strings.TrimSpace(lines[3])
		}

		commits = append(commits, gitCommit{
			Hash:      hash,
			Subject:   subject,
			Body:      body,
			Timestamp: timestamp,
		})
	}

	return commits
}
