package cli

import (
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// withStubPrompt swaps promptModelSelectionFunc for the duration of a test.
// The stub observes what the prompt would have been called with and returns
// the caller-specified result without touching the huh TUI.
func withStubPrompt(t *testing.T, stub func(embeddable []discoveredModel, preSelected map[string]string) ([]string, error)) {
	t.Helper()
	orig := promptModelSelectionFunc
	promptModelSelectionFunc = stub
	t.Cleanup(func() { promptModelSelectionFunc = orig })
}

// --- existing contract ---------------------------------------------------

func TestResolveIndexModels_FlagExactMatch(t *testing.T) {
	discovered := []discoveredModel{
		{Name: "nomic-embed-text:latest", CanEmbed: true, Dimensions: 768},
		{Name: "bge-m3:latest", CanEmbed: true, Dimensions: 1024},
	}
	got, fromMarker, err := resolveIndexModels(resolveOpts{
		Discovered: discovered, ModelFlags: []string{"nomic-embed-text:latest"}, IsTTY: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fromMarker {
		t.Error("fromMarker should be false for --model path")
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
	got, _, err := resolveIndexModels(resolveOpts{
		Discovered: discovered, ModelFlags: []string{"nomic-embed-text"}, IsTTY: true,
	})
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
	_, _, err := resolveIndexModels(resolveOpts{
		Discovered: discovered, ModelFlags: []string{"does-not-exist"}, IsTTY: true,
	})
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
	_, _, err := resolveIndexModels(resolveOpts{
		Discovered: discovered, ModelFlags: []string{"llama3"}, IsTTY: true,
	})
	if err == nil {
		t.Fatal("expected error when --model points at a non-embedding model")
	}
}

func TestResolveIndexModels_SingleEmbeddableAutoPicks(t *testing.T) {
	// TTY + single embeddable + no flags → auto-pick (no prompt). The stub
	// flags test failure if invoked.
	withStubPrompt(t, func(_ []discoveredModel, _ map[string]string) ([]string, error) {
		t.Fatal("prompt should not be called when only 1 embeddable exists")
		return nil, nil
	})
	discovered := []discoveredModel{
		{Name: "llama3:latest", CanEmbed: false},
		{Name: "nomic-embed-text:latest", CanEmbed: true, Dimensions: 768},
	}
	got, _, err := resolveIndexModels(resolveOpts{Discovered: discovered, IsTTY: true})
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
	_, _, err := resolveIndexModels(resolveOpts{Discovered: discovered, IsTTY: true})
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

// --- new behaviour: TTY always prompts with 2+ models --------------------

// TestResolveIndexModels_TTYWithConfigAndMultipleShowsPrompt replaces the
// old TTYConfigAutoPickOverMultiple contract. With 2+ embeddable models the
// TTY path MUST always show the multi-select; cfg.Model only pre-checks its
// option.
func TestResolveIndexModels_TTYWithConfigAndMultipleShowsPrompt(t *testing.T) {
	discovered := []discoveredModel{
		{Name: "nomic-embed-text:latest", CanEmbed: true, Dimensions: 768},
		{Name: "bge-m3:latest", CanEmbed: true, Dimensions: 1024},
	}
	var captured map[string]string
	withStubPrompt(t, func(_ []discoveredModel, preSelected map[string]string) ([]string, error) {
		captured = preSelected
		return []string{"nomic-embed-text:latest", "bge-m3:latest"}, nil
	})
	got, _, err := resolveIndexModels(resolveOpts{
		Discovered: discovered, ConfigModel: "nomic-embed-text", IsTTY: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("expected both selections, got %v", got)
	}
	if _, ok := captured["nomic-embed-text:latest"]; !ok {
		t.Errorf("expected cfg.Model to be pre-selected; preSelected=%v", captured)
	}
	if !strings.Contains(captured["nomic-embed-text:latest"], "configured") {
		t.Errorf("expected 'configured' annotation, got %q", captured["nomic-embed-text:latest"])
	}
}

// TestResolveIndexModels_NonTTYWithConfigAutoPicks preserves the CI-friendly
// behaviour: non-TTY + cfg.Model + multiple embeddable → silent auto-pick
// the config model. (Back-compat for hooks, pipes, scripts.)
func TestResolveIndexModels_NonTTYWithConfigAutoPicks(t *testing.T) {
	withStubPrompt(t, func(_ []discoveredModel, _ map[string]string) ([]string, error) {
		t.Fatal("prompt must not be called in non-TTY mode")
		return nil, nil
	})
	discovered := []discoveredModel{
		{Name: "nomic-embed-text:latest", CanEmbed: true, Dimensions: 768},
		{Name: "bge-m3:latest", CanEmbed: true, Dimensions: 1024},
	}
	got, _, err := resolveIndexModels(resolveOpts{
		Discovered: discovered, ConfigModel: "nomic-embed-text", IsTTY: false,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != "nomic-embed-text:latest" {
		t.Fatalf("got %v, want [nomic-embed-text:latest]", got)
	}
}

// TestResolveIndexModels_TTYAlwaysPromptsWithMultipleModels is the headline
// regression: TTY + 2+ embeddable + no flags + no config + no marker → the
// prompt MUST run. Users must never be silently locked into a single model.
func TestResolveIndexModels_TTYAlwaysPromptsWithMultipleModels(t *testing.T) {
	discovered := []discoveredModel{
		{Name: "nomic-embed-text:latest", CanEmbed: true, Dimensions: 768},
		{Name: "bge-m3:latest", CanEmbed: true, Dimensions: 1024},
	}
	called := false
	withStubPrompt(t, func(_ []discoveredModel, preSelected map[string]string) ([]string, error) {
		called = true
		if len(preSelected) != 0 {
			t.Errorf("no pre-selection expected, got %v", preSelected)
		}
		return []string{"bge-m3:latest"}, nil
	})
	got, _, err := resolveIndexModels(resolveOpts{Discovered: discovered, IsTTY: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("prompt should have been invoked for TTY + 2 embeddable models")
	}
	if len(got) != 1 || got[0] != "bge-m3:latest" {
		t.Errorf("got %v, want [bge-m3:latest]", got)
	}
}

// TestResolveIndexModels_TTYPreSelectsConfigAndMarker verifies that when
// BOTH cfg.Model and a marker are present, both pre-checks are reflected in
// preSelected, and distinct annotations are emitted.
func TestResolveIndexModels_TTYPreSelectsConfigAndMarker(t *testing.T) {
	discovered := []discoveredModel{
		{Name: "alpha:latest", CanEmbed: true, Dimensions: 768},
		{Name: "beta:latest", CanEmbed: true, Dimensions: 1024},
		{Name: "gamma:latest", CanEmbed: true, Dimensions: 512},
	}
	var captured map[string]string
	withStubPrompt(t, func(_ []discoveredModel, preSelected map[string]string) ([]string, error) {
		captured = preSelected
		return []string{"alpha:latest", "beta:latest"}, nil
	})
	_, fromMarker, err := resolveIndexModels(resolveOpts{
		Discovered: discovered, ConfigModel: "alpha", MarkerModels: []string{"beta"}, IsTTY: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fromMarker {
		// fromMarker only means "auto-continue with marker's list in non-TTY".
		// A TTY prompt run lets the user control the outcome, so fromMarker
		// must remain false even though the marker shaped pre-selection.
		t.Error("fromMarker should be false when user had the prompt")
	}
	if _, ok := captured["alpha:latest"]; !ok {
		t.Errorf("alpha must be pre-selected (config); captured=%v", captured)
	}
	if _, ok := captured["beta:latest"]; !ok {
		t.Errorf("beta must be pre-selected (marker); captured=%v", captured)
	}
	if !strings.Contains(captured["alpha:latest"], "configured") {
		t.Errorf("alpha should be annotated 'configured', got %q", captured["alpha:latest"])
	}
	if !strings.Contains(captured["beta:latest"], "in progress") {
		t.Errorf("beta should be annotated 'in progress', got %q", captured["beta:latest"])
	}
}

// TestResolveIndexModels_TTYPreSelectConflictPrefersMarkerAnnotation: when
// the same model is BOTH configured AND in the marker, only the "in
// progress" annotation is shown (spec #1: marker wins the label).
func TestResolveIndexModels_TTYPreSelectConflictPrefersMarkerAnnotation(t *testing.T) {
	discovered := []discoveredModel{
		{Name: "alpha:latest", CanEmbed: true, Dimensions: 768},
		{Name: "beta:latest", CanEmbed: true, Dimensions: 1024},
	}
	var captured map[string]string
	withStubPrompt(t, func(_ []discoveredModel, preSelected map[string]string) ([]string, error) {
		captured = preSelected
		return []string{"alpha:latest"}, nil
	})
	_, _, err := resolveIndexModels(resolveOpts{
		Discovered: discovered, ConfigModel: "alpha", MarkerModels: []string{"alpha"}, IsTTY: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	note := captured["alpha:latest"]
	if !strings.Contains(note, "in progress") {
		t.Errorf("conflict: marker annotation should win, got %q", note)
	}
	if strings.Contains(note, "configured") {
		t.Errorf("conflict: 'configured' must NOT appear when marker wins, got %q", note)
	}
}

// TestResolveIndexModels_MultiModelCSV asserts --model a,b returns both.
func TestResolveIndexModels_MultiModelCSV(t *testing.T) {
	discovered := []discoveredModel{
		{Name: "alpha:latest", CanEmbed: true, Dimensions: 1},
		{Name: "beta:latest", CanEmbed: true, Dimensions: 1},
	}
	got, _, err := resolveIndexModels(resolveOpts{
		Discovered: discovered, ModelFlags: []string{"alpha,beta"}, IsTTY: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"alpha:latest", "beta:latest"}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestResolveIndexModels_MultiModelRepeatedFlag asserts --model a --model b returns both.
func TestResolveIndexModels_MultiModelRepeatedFlag(t *testing.T) {
	discovered := []discoveredModel{
		{Name: "alpha:latest", CanEmbed: true, Dimensions: 1},
		{Name: "beta:latest", CanEmbed: true, Dimensions: 1},
	}
	got, _, err := resolveIndexModels(resolveOpts{
		Discovered: discovered, ModelFlags: []string{"alpha", "beta"}, IsTTY: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"alpha:latest", "beta:latest"}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestResolveIndexModels_AllModelsFlag asserts --all-models returns every
// embeddable model and skips non-embeddable ones.
func TestResolveIndexModels_AllModelsFlag(t *testing.T) {
	discovered := []discoveredModel{
		{Name: "alpha:latest", CanEmbed: true, Dimensions: 1},
		{Name: "not-embed:latest", CanEmbed: false},
		{Name: "beta:latest", CanEmbed: true, Dimensions: 1},
	}
	got, _, err := resolveIndexModels(resolveOpts{
		Discovered: discovered, AllModels: true, IsTTY: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"alpha:latest", "beta:latest"}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestResolveIndexModels_NonTTYMarkerAutoContinues asserts the CI/hook path:
// no prompt, no flags, marker present → use marker.models silently.
func TestResolveIndexModels_NonTTYMarkerAutoContinues(t *testing.T) {
	withStubPrompt(t, func(_ []discoveredModel, _ map[string]string) ([]string, error) {
		t.Fatal("prompt must not be called in non-TTY auto-continue path")
		return nil, nil
	})
	discovered := []discoveredModel{
		{Name: "alpha:latest", CanEmbed: true, Dimensions: 1},
		{Name: "beta:latest", CanEmbed: true, Dimensions: 1},
	}
	got, fromMarker, err := resolveIndexModels(resolveOpts{
		Discovered: discovered, MarkerModels: []string{"alpha", "beta"}, IsTTY: false,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fromMarker {
		t.Error("fromMarker should be true when we used the marker's recorded list")
	}
	want := []string{"alpha", "beta"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestResolveIndexModels_MultiModelFlagUnknownModelErrors asserts a CSV or
// repeated flag that references an unknown model surfaces the full list of
// available embedders.
func TestResolveIndexModels_MultiModelFlagUnknownModelErrors(t *testing.T) {
	discovered := []discoveredModel{
		{Name: "alpha:latest", CanEmbed: true, Dimensions: 1},
	}
	_, _, err := resolveIndexModels(resolveOpts{
		Discovered: discovered, ModelFlags: []string{"alpha,unknown"}, IsTTY: true,
	})
	if err == nil {
		t.Fatal("expected error for --model alpha,unknown")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("error should mention the bad model name: %v", err)
	}
}

// TestResolveIndexModels_AllModelsFlagWithZeroEmbeddableErrors asserts
// --all-models fails clearly when no embeddable models are installed.
func TestResolveIndexModels_AllModelsFlagWithZeroEmbeddableErrors(t *testing.T) {
	discovered := []discoveredModel{
		{Name: "llama3:latest", CanEmbed: false},
	}
	_, _, err := resolveIndexModels(resolveOpts{
		Discovered: discovered, AllModels: true, IsTTY: true,
	})
	if err == nil {
		t.Fatal("expected error when --all-models is requested but none are embeddable")
	}
}

// TestResolveIndexModels_ZeroSelectedExitsCleanly asserts that when the user
// opens the multi-select and submits with zero selections, the resolver
// returns errNoModelsSelected so the caller can exit(0) with a friendly
// message (marker preserved by caller).
func TestResolveIndexModels_ZeroSelectedExitsCleanly(t *testing.T) {
	discovered := []discoveredModel{
		{Name: "alpha:latest", CanEmbed: true, Dimensions: 1},
		{Name: "beta:latest", CanEmbed: true, Dimensions: 1},
	}
	withStubPrompt(t, func(_ []discoveredModel, _ map[string]string) ([]string, error) {
		return nil, nil // user submitted empty
	})
	got, _, err := resolveIndexModels(resolveOpts{Discovered: discovered, IsTTY: true})
	if !errors.Is(err, errNoModelsSelected) {
		t.Errorf("err = %v, want errNoModelsSelected", err)
	}
	if got != nil {
		t.Errorf("got = %v, want nil", got)
	}
}
