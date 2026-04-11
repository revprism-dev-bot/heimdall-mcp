package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/huh"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// discoveredModel is a model that was found in Ollama and tested for embedding support.
type discoveredModel struct {
	Name       string
	CanEmbed   bool
	Dimensions int
	Known      *heimdall.KnownEmbeddingModel // nil if not in curated list
}

// discoverEmbeddingModels probes Ollama for all pulled models and tests which ones
// can produce embeddings. Returns the list of results.
func discoverEmbeddingModels(cfg config.Config) ([]discoveredModel, error) {
	ctx := context.Background()
	client := heimdall.NewOllamaClient(cfg.OllamaEndpoint)

	models, err := client.ListModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list Ollama models: %w", err)
	}

	var results []discoveredModel
	for _, m := range models {
		d := discoveredModel{
			Name:  m.Name,
			Known: heimdall.LookupEmbeddingModel(m.Name),
		}

		// Test if the model can produce embeddings
		vec, err := client.Embed(ctx, m.Name, "test")
		if err == nil && len(vec) > 0 {
			d.CanEmbed = true
			d.Dimensions = len(vec)
		}

		results = append(results, d)
	}

	return results, nil
}

// promptModelSelection shows an interactive multi-select for embedding models.
// Returns the selected model names. If only one embedding model exists, auto-selects it.
func promptModelSelection(models []discoveredModel, currentModel string) ([]string, error) {
	var embeddable []discoveredModel
	for _, m := range models {
		if m.CanEmbed {
			embeddable = append(embeddable, m)
		}
	}

	if len(embeddable) == 0 {
		return nil, fmt.Errorf("no embedding-capable models found in Ollama.\n\nPull an embedding model first:\n  ollama pull nomic-embed-text\n\nSee all options: heimdall-mcp models")
	}

	// Single model — show what it is, auto-select
	if len(embeddable) == 1 {
		m := embeddable[0]
		fmt.Printf("1 embedding model found:\n")
		if m.Known != nil {
			fmt.Printf("  %s — %s, %s, %dd\n", stripTag(m.Name), m.Known.Origin, m.Known.Params, m.Dimensions)
		} else {
			fmt.Printf("  %s — %dd\n", stripTag(m.Name), m.Dimensions)
		}
		fmt.Println()
		return []string{m.Name}, nil
	}

	// Build options for multi-select
	options := make([]huh.Option[string], 0, len(embeddable))
	for _, m := range embeddable {
		label := m.Name
		if m.Known != nil {
			label = fmt.Sprintf("%s — %s, %s, %dd", m.Name, m.Known.Origin, m.Known.Params, m.Dimensions)
		} else {
			label = fmt.Sprintf("%s — %dd", m.Name, m.Dimensions)
		}
		options = append(options, huh.NewOption(label, m.Name).Selected(m.Name == currentModel))
	}

	var selected []string
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewMultiSelect[string]().
				Title("Select embedding models to index with").
				Description("Space to toggle, Enter to confirm").
				Options(options...).
				Value(&selected),
		),
	)

	if err := form.Run(); err != nil {
		return nil, err
	}

	if len(selected) == 0 {
		return nil, fmt.Errorf("no models selected")
	}

	return selected, nil
}

// formatDiscoveryResults prints the discovery results to stdout.
func formatDiscoveryResults(models []discoveredModel) {
	for _, m := range models {
		if m.CanEmbed {
			info := ""
			if m.Known != nil {
				info = fmt.Sprintf("(%s, %s, %dd)", m.Known.Origin, m.Known.Params, m.Dimensions)
			} else {
				info = fmt.Sprintf("(%dd)", m.Dimensions)
			}
			fmt.Printf("  ✓ %-30s %s\n", stripTag(m.Name), info)
		} else {
			fmt.Printf("  ✗ %-30s (not an embedding model)\n", stripTag(m.Name))
		}
	}
}

// stripTag removes the :latest or other tag from model name for display.
func stripTag(name string) string {
	if idx := strings.LastIndex(name, ":"); idx > 0 {
		tag := name[idx+1:]
		if tag == "latest" {
			return name[:idx]
		}
	}
	return name
}
