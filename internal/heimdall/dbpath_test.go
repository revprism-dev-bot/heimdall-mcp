package heimdall

import (
	"os"
	"path/filepath"
	"testing"
)

func TestModelDBDir(t *testing.T) {
	tests := []struct {
		name     string
		baseDir  string
		model    string
		wantSuf  string // expected suffix after baseDir
	}{
		{"simple model", "/tmp/db", "nomic-embed-text", "nomic-embed-text"},
		{"model with latest tag stripped", "/tmp/db", "model:latest", "model"},
		{"model with non-latest tag", "/tmp/db", "model:v2", "model_v2"},
		{"model with slash", "/tmp/db", "org/model", "org_model"},
		{"model with both", "/tmp/db", "org/model:v2", "org_model_v2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ModelDBDir(tt.baseDir, tt.model)
			want := filepath.Join(tt.baseDir, tt.wantSuf)
			if got != want {
				t.Errorf("ModelDBDir(%q, %q) = %q, want %q", tt.baseDir, tt.model, got, want)
			}
		})
	}
}

func TestListAvailableModels(t *testing.T) {
	tmpDir := t.TempDir()

	// Empty dir
	models := ListAvailableModels(tmpDir)
	if len(models) != 0 {
		t.Errorf("expected 0 models, got %d", len(models))
	}

	// Create model dirs with vectors.db
	for _, name := range []string{"nomic-embed-text", "bge-m3"} {
		dir := filepath.Join(tmpDir, name)
		os.MkdirAll(dir, 0755)
		os.WriteFile(filepath.Join(dir, "vectors.db"), []byte("test"), 0644)
	}

	// Create a dir without vectors.db (should be excluded)
	os.MkdirAll(filepath.Join(tmpDir, "empty-dir"), 0755)

	// Create a regular file (should be excluded)
	os.WriteFile(filepath.Join(tmpDir, "not-a-dir"), []byte("test"), 0644)

	models = ListAvailableModels(tmpDir)
	if len(models) != 2 {
		t.Errorf("expected 2 models, got %d: %v", len(models), models)
	}

	// Non-existent dir
	models = ListAvailableModels(filepath.Join(tmpDir, "nonexistent"))
	if models != nil {
		t.Errorf("expected nil for non-existent dir, got %v", models)
	}
}
