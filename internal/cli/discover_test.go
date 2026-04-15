package cli

import (
	"strings"
	"testing"
)

func TestResolveIndexModels_FlagExactMatch(t *testing.T) {
	discovered := []discoveredModel{
		{Name: "nomic-embed-text:latest", CanEmbed: true, Dimensions: 768},
		{Name: "bge-m3:latest", CanEmbed: true, Dimensions: 1024},
	}
	got, err := resolveIndexModels(discovered, "nomic-embed-text:latest", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != "nomic-embed-text:latest" {
		t.Fatalf("got %v, want [nomic-embed-text:latest]", got)
	}
}

func TestResolveIndexModels_FlagToleratesLatestTag(t *testing.T) {
	discovered := []discoveredModel{
		{Name: "nomic-embed-text:latest", CanEmbed: true, Dimensions: 768},
		{Name: "bge-m3:latest", CanEmbed: true, Dimensions: 1024},
	}
	got, err := resolveIndexModels(discovered, "nomic-embed-text", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != "nomic-embed-text:latest" {
		t.Fatalf("got %v, want [nomic-embed-text:latest]", got)
	}
}

func TestResolveIndexModels_FlagUnknownModel(t *testing.T) {
	discovered := []discoveredModel{
		{Name: "nomic-embed-text:latest", CanEmbed: true, Dimensions: 768},
	}
	_, err := resolveIndexModels(discovered, "does-not-exist", "")
	if err == nil {
		t.Fatal("expected error for unknown model")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Fatalf("error missing model name: %v", err)
	}
}

func TestResolveIndexModels_FlagRejectsNonEmbeddable(t *testing.T) {
	discovered := []discoveredModel{
		{Name: "llama3:latest", CanEmbed: false},
		{Name: "nomic-embed-text:latest", CanEmbed: true, Dimensions: 768},
	}
	_, err := resolveIndexModels(discovered, "llama3", "")
	if err == nil {
		t.Fatal("expected error when --model points at a non-embedding model")
	}
}

func TestResolveIndexModels_ConfigAutoPickOverMultiple(t *testing.T) {
	discovered := []discoveredModel{
		{Name: "nomic-embed-text:latest", CanEmbed: true, Dimensions: 768},
		{Name: "bge-m3:latest", CanEmbed: true, Dimensions: 1024},
	}
	got, err := resolveIndexModels(discovered, "", "nomic-embed-text")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != "nomic-embed-text:latest" {
		t.Fatalf("got %v, want [nomic-embed-text:latest]", got)
	}
}

func TestResolveIndexModels_SingleEmbeddableAutoPicks(t *testing.T) {
	discovered := []discoveredModel{
		{Name: "llama3:latest", CanEmbed: false},
		{Name: "nomic-embed-text:latest", CanEmbed: true, Dimensions: 768},
	}
	got, err := resolveIndexModels(discovered, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != "nomic-embed-text:latest" {
		t.Fatalf("got %v, want [nomic-embed-text:latest]", got)
	}
}

func TestResolveIndexModels_NoEmbeddableReturnsError(t *testing.T) {
	discovered := []discoveredModel{
		{Name: "llama3:latest", CanEmbed: false},
	}
	_, err := resolveIndexModels(discovered, "", "")
	if err == nil {
		t.Fatal("expected error when no embedding-capable models exist")
	}
}

func TestFindEmbeddableModel_SkipsNonEmbeddable(t *testing.T) {
	discovered := []discoveredModel{
		{Name: "foo:latest", CanEmbed: false},
		{Name: "foo-embed:latest", CanEmbed: true, Dimensions: 384},
	}
	if _, ok := findEmbeddableModel(discovered, "foo"); ok {
		t.Fatal("foo is not embeddable, should not have matched")
	}
	if _, ok := findEmbeddableModel(discovered, "foo-embed"); !ok {
		t.Fatal("foo-embed should have matched")
	}
}
