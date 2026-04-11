package heimdall

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMigrateToModelDir_LegacyDB(t *testing.T) {
	tmpDir := t.TempDir()
	baseDir := filepath.Join(tmpDir, ".heimdall_db")
	os.MkdirAll(baseDir, 0755)

	// Create a legacy vectors.db with store_metadata
	store, err := OpenStore(baseDir)
	if err != nil {
		t.Fatal(err)
	}
	store.SetMetadata("embedding_model", "nomic-embed-text")
	store.Upsert([]VectorRecord{
		{ID: "test:1", FilePath: "test.go", Content: "hello", Embedding: []float32{1.0}, ModTime: 100},
	})
	store.Close()

	// Verify legacy DB exists
	if _, err := os.Stat(filepath.Join(baseDir, "vectors.db")); err != nil {
		t.Fatal("legacy vectors.db should exist before migration")
	}

	// Migrate with matching model
	MigrateToModelDir(baseDir, "nomic-embed-text")

	// Legacy DB should be gone
	if _, err := os.Stat(filepath.Join(baseDir, "vectors.db")); err == nil {
		t.Error("legacy vectors.db should be removed after migration")
	}

	// Model-specific DB should exist
	modelDir := ModelDBDir(baseDir, "nomic-embed-text")
	if _, err := os.Stat(filepath.Join(modelDir, "vectors.db")); err != nil {
		t.Error("model-specific vectors.db should exist after migration")
	}

	// Verify the migrated DB is usable
	store2, err := OpenStore(modelDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()

	stats := store2.Stats()
	if stats.TotalRecords != 1 {
		t.Errorf("expected 1 record in migrated DB, got %d", stats.TotalRecords)
	}
	if m := store2.GetMetadata("embedding_model"); m != "nomic-embed-text" {
		t.Errorf("expected embedding_model 'nomic-embed-text', got %q", m)
	}
}

func TestMigrateToModelDir_DifferentModel(t *testing.T) {
	tmpDir := t.TempDir()
	baseDir := filepath.Join(tmpDir, ".heimdall_db")
	os.MkdirAll(baseDir, 0755)

	// Create a legacy DB built with "bge-m3"
	store, err := OpenStore(baseDir)
	if err != nil {
		t.Fatal(err)
	}
	store.SetMetadata("embedding_model", "bge-m3")
	store.Upsert([]VectorRecord{
		{ID: "test:1", FilePath: "test.go", Content: "hello", Embedding: []float32{1.0}, ModTime: 100},
	})
	store.Close()

	// Migrate with different model — should move to bge-m3 dir, not nomic
	MigrateToModelDir(baseDir, "nomic-embed-text")

	// Should be in bge-m3 dir (the stored model)
	bgeDir := ModelDBDir(baseDir, "bge-m3")
	if _, err := os.Stat(filepath.Join(bgeDir, "vectors.db")); err != nil {
		t.Error("should migrate to stored model dir (bge-m3)")
	}

	// Should NOT be in nomic dir
	nomicDir := ModelDBDir(baseDir, "nomic-embed-text")
	if _, err := os.Stat(filepath.Join(nomicDir, "vectors.db")); err == nil {
		t.Error("should not create nomic dir when stored model is bge-m3")
	}
}

func TestMigrateToModelDir_NoLegacyDB(t *testing.T) {
	tmpDir := t.TempDir()
	baseDir := filepath.Join(tmpDir, ".heimdall_db")
	os.MkdirAll(baseDir, 0755)

	// No legacy DB — should be a no-op
	MigrateToModelDir(baseDir, "nomic-embed-text")

	// Model dir should NOT be created
	modelDir := ModelDBDir(baseDir, "nomic-embed-text")
	if _, err := os.Stat(modelDir); err == nil {
		t.Error("should not create model dir when no legacy DB exists")
	}
}

func TestMigrateToModelDir_TargetAlreadyExists(t *testing.T) {
	tmpDir := t.TempDir()
	baseDir := filepath.Join(tmpDir, ".heimdall_db")
	os.MkdirAll(baseDir, 0755)

	// Create a legacy DB
	store, err := OpenStore(baseDir)
	if err != nil {
		t.Fatal(err)
	}
	store.SetMetadata("embedding_model", "nomic-embed-text")
	store.Close()

	// Create target dir with an existing vectors.db
	modelDir := ModelDBDir(baseDir, "nomic-embed-text")
	os.MkdirAll(modelDir, 0755)
	os.WriteFile(filepath.Join(modelDir, "vectors.db"), []byte("existing"), 0644)

	// Migration should skip (target already exists)
	MigrateToModelDir(baseDir, "nomic-embed-text")

	// Legacy DB should still be there
	if _, err := os.Stat(filepath.Join(baseDir, "vectors.db")); err != nil {
		t.Error("legacy vectors.db should remain when target already exists")
	}
}

func TestMigrateToModelDir_EmptyModel(t *testing.T) {
	tmpDir := t.TempDir()
	baseDir := filepath.Join(tmpDir, ".heimdall_db")
	os.MkdirAll(baseDir, 0755)

	// Create a legacy DB without embedding_model metadata
	store, err := OpenStore(baseDir)
	if err != nil {
		t.Fatal(err)
	}
	store.Upsert([]VectorRecord{
		{ID: "test:1", FilePath: "test.go", Content: "hello", Embedding: []float32{1.0}, ModTime: 100},
	})
	store.Close()

	// Migrate — should use the current model since stored model is empty
	MigrateToModelDir(baseDir, "nomic-embed-text")

	modelDir := ModelDBDir(baseDir, "nomic-embed-text")
	if _, err := os.Stat(filepath.Join(modelDir, "vectors.db")); err != nil {
		t.Error("should migrate to current model dir when stored model is empty")
	}
}
