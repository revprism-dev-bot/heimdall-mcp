package heimdall

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// normalizeHookModelName strips Ollama's implicit ":latest" tag so stored
// and requested model names compare equal regardless of who typed the tag.
// Kept package-private to avoid colliding with a future NormalizeModelName
// helper landing on main via a sibling stream.
func normalizeHookModelName(m string) string {
	return strings.TrimSuffix(m, ":latest")
}

// Sentinel errors returned by VerifyHookIndex / VerifyHookIndexDim.
//
// Callers on the hook path use errors.Is to distinguish the three failure
// modes so each can map to a distinct Tier B note. These errors are
// intentionally separate from ResolveUsableModelDB's fuzzy-match path: the
// hook boundary must refuse a mismatched index rather than silently serve
// cosine-nonsense from a different embedding space.
var (
	ErrIndexModelMissing  = errors.New("heimdall: index has no embedding_model metadata")
	ErrIndexModelMismatch = errors.New("heimdall: index embedding_model does not match requested model")
	ErrIndexDimMismatch   = errors.New("heimdall: index embedding_dim does not match requested dim")
)

// VerifyHookIndex checks that the given store was built with the requested
// embedding model. It never calls Ollama and never fuzzy-matches against
// other indexes on disk — that is deliberate (see docs/plans/hooks §5.1).
//
// Returns nil when the stored embedding_model (normalized) equals the
// normalized requestedModel. Otherwise wraps one of the sentinel errors
// above; callers should use errors.Is to branch.
func VerifyHookIndex(store *VectorStore, requestedModel string) error {
	if store == nil {
		return fmt.Errorf("%w: store is nil", ErrIndexModelMissing)
	}
	indexed := store.GetMetadata("embedding_model")
	if indexed == "" {
		return fmt.Errorf("%w", ErrIndexModelMissing)
	}
	if normalizeHookModelName(indexed) != normalizeHookModelName(requestedModel) {
		return fmt.Errorf("%w: indexed=%s requested=%s",
			ErrIndexModelMismatch, indexed, requestedModel)
	}
	return nil
}

// VerifyHookIndexDim is the separate dim gate. Callers invoke it only when
// they already have the current model's output dimension (e.g. from a live
// probe) and want to refuse retrieval against a store whose vectors are a
// different length. Missing or unparseable embedding_dim metadata also
// triggers ErrIndexDimMismatch — an index without dim metadata cannot be
// trusted for the hook path.
func VerifyHookIndexDim(store *VectorStore, wantDim int) error {
	if store == nil {
		return fmt.Errorf("%w: store is nil", ErrIndexDimMismatch)
	}
	raw := store.GetMetadata("embedding_dim")
	if raw == "" {
		return fmt.Errorf("%w: embedding_dim metadata missing", ErrIndexDimMismatch)
	}
	got, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("%w: embedding_dim %q unparseable: %v",
			ErrIndexDimMismatch, raw, err)
	}
	if got != wantDim {
		return fmt.Errorf("%w: indexed_dim=%d requested_dim=%d",
			ErrIndexDimMismatch, got, wantDim)
	}
	return nil
}
