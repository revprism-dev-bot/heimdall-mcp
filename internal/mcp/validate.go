package mcp

import "fmt"

const (
	maxContentSize = 100 * 1024 // 100KB
	maxSummarySize = 50 * 1024  // 50KB
	maxQuerySize   = 10 * 1024  // 10KB
	maxTagCount    = 20
	maxTagLength   = 100
)

func validateContent(content string) error {
	if content == "" {
		return fmt.Errorf("content is required")
	}
	if len(content) > maxContentSize {
		return fmt.Errorf("content exceeds maximum size of %dKB", maxContentSize/1024)
	}
	return nil
}

func validateSummary(summary string) error {
	if summary == "" {
		return fmt.Errorf("summary is required")
	}
	if len(summary) > maxSummarySize {
		return fmt.Errorf("summary exceeds maximum size of %dKB", maxSummarySize/1024)
	}
	return nil
}

func validateQuery(query string) error {
	if query == "" {
		return fmt.Errorf("query is required")
	}
	if len(query) > maxQuerySize {
		return fmt.Errorf("query exceeds maximum size of %dKB", maxQuerySize/1024)
	}
	return nil
}

func validateTags(tags []string) error {
	if len(tags) > maxTagCount {
		return fmt.Errorf("too many tags: max %d allowed", maxTagCount)
	}
	for _, tag := range tags {
		if len(tag) > maxTagLength {
			return fmt.Errorf("tag %q exceeds maximum length of %d characters", tag[:20]+"...", maxTagLength)
		}
	}
	return nil
}
