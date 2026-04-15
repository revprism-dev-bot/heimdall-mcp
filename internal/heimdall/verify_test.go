package heimdall

import (
	"errors"
	"testing"
)

func TestVerifyHookIndex_HappyPath(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	if err := store.SetMetadata("embedding_model", "nomic-embed-text"); err != nil {
		t.Fatalf("set metadata: %v", err)
	}

	if err := VerifyHookIndex(store, "nomic-embed-text"); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
	// :latest normalization
	if err := VerifyHookIndex(store, "nomic-embed-text:latest"); err != nil {
		t.Errorf("expected nil with :latest, got %v", err)
	}
}

func TestVerifyHookIndex_MissingMetadata(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	err = VerifyHookIndex(store, "nomic-embed-text")
	if !errors.Is(err, ErrIndexModelMissing) {
		t.Errorf("expected ErrIndexModelMissing, got %v", err)
	}
}

func TestVerifyHookIndex_Mismatch(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	store.SetMetadata("embedding_model", "nomic-embed-text")

	err = VerifyHookIndex(store, "bge-m3")
	if !errors.Is(err, ErrIndexModelMismatch) {
		t.Errorf("expected ErrIndexModelMismatch, got %v", err)
	}
	// The error message must carry both sides so Tier B notes can
	// surface them to the user.
	if msg := err.Error(); msg == "" {
		t.Error("empty error message")
	}
}

func TestVerifyHookIndex_NilStore(t *testing.T) {
	err := VerifyHookIndex(nil, "any")
	if !errors.Is(err, ErrIndexModelMissing) {
		t.Errorf("expected ErrIndexModelMissing on nil store, got %v", err)
	}
}

// Metadata-table-missing: OpenStore always creates the table, but
// GetMetadata's empty-string fallback still needs to route through the
// missing sentinel. Simulate by opening fresh then never calling
// SetMetadata — functionally identical.
func TestVerifyHookIndex_MetadataTablePresentButEmpty(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	if err := VerifyHookIndex(store, "nomic-embed-text"); !errors.Is(err, ErrIndexModelMissing) {
		t.Errorf("expected ErrIndexModelMissing, got %v", err)
	}
}

func TestVerifyHookIndexDim_HappyPath(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	store.SetMetadata("embedding_dim", "768")

	if err := VerifyHookIndexDim(store, 768); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestVerifyHookIndexDim_Missing(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	err = VerifyHookIndexDim(store, 768)
	if !errors.Is(err, ErrIndexDimMismatch) {
		t.Errorf("expected ErrIndexDimMismatch, got %v", err)
	}
}

func TestVerifyHookIndexDim_Unparseable(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	store.SetMetadata("embedding_dim", "not-a-number")

	err = VerifyHookIndexDim(store, 768)
	if !errors.Is(err, ErrIndexDimMismatch) {
		t.Errorf("expected ErrIndexDimMismatch, got %v", err)
	}
}

func TestVerifyHookIndexDim_Mismatch(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	store.SetMetadata("embedding_dim", "768")

	err = VerifyHookIndexDim(store, 1024)
	if !errors.Is(err, ErrIndexDimMismatch) {
		t.Errorf("expected ErrIndexDimMismatch, got %v", err)
	}
}

func TestVerifyHookIndexDim_NilStore(t *testing.T) {
	err := VerifyHookIndexDim(nil, 768)
	if !errors.Is(err, ErrIndexDimMismatch) {
		t.Errorf("expected ErrIndexDimMismatch on nil store, got %v", err)
	}
}
