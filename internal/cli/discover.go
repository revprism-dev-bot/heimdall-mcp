package cli

import (
	"context"
	"errors"
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

// errNoModelsSelected is returned by resolveIndexModels when the user
// submits the multi-select form with zero selections. Callers should treat
// it as a clean exit (exit code 0, print "Nothing to index", leave the
// resume marker on disk).
var errNoModelsSelected = errors.New("no models selected")

// resolveOpts captures every input resolveIndexModels consults. Grouping
// them in a struct keeps the TTY/non-TTY decision tree legible and makes
// tests easy to write — a bare-struct literal expresses the exact scenario
// under test.
type resolveOpts struct {
	// Discovered is the output of discoverEmbeddingModels — the full list
	// of Ollama models plus their can-embed flag.
	Discovered []discoveredModel
	// ModelFlags is every argument the user passed via --model. Each entry
	// may itself be a comma-separated list (`--model a,b`); repeated flags
	// (`--model a --model b`) stack into this slice. Empty when no flag.
	ModelFlags []string
	// AllModels reflects the --all-models boolean flag. When true the
	// resolver short-circuits to every embeddable model.
	AllModels bool
	// ConfigModel is cfg.Model (persisted via `heimdall-mcp config set
	// model`). Used to pre-select in TTY prompts and to auto-pick in
	// non-TTY mode for back-compat with CI / hooks.
	ConfigModel string
	// MarkerModels is the list from an on-disk resume marker. Empty when
	// no marker or marker was deleted by the caller.
	MarkerModels []string
	// IsTTY gates the interactive path. False means this run cannot prompt
	// the user (piped stdin, CI, background hooks).
	IsTTY bool
}

// promptModelSelectionFunc is the package-level hook that tests replace to
// avoid touching the huh TUI. Kept as a variable rather than an interface
// because there is exactly one caller (resolveIndexModels) and exactly one
// production implementation. Tests invoke `withStubPrompt` to swap it.
//
// Signature:
//
//	embeddable   — the filtered list of CanEmbed=true models
//	preSelected  — canonical-name → annotation (e.g. "configured",
//	               "in progress — resume"). Keys drive which checkboxes are
//	               ticked on open; values drive the trailing "(…)" tag.
var promptModelSelectionFunc = promptModelSelection

// resolveIndexModels picks the models to index with. The resolution order is:
//
//  1. --all-models       → every embeddable model (or error if none).
//  2. --model <flags>    → explicit list, comma-split + repeat-merged. An
//     unknown name fails with the full embeddable list.
//  3. Non-TTY branches (CI, hooks, piped stdin):
//     a. cfg.Model — silent auto-pick, matching the pre-fix CLI behaviour.
//     b. Marker models — resume the interrupted run silently.
//     c. Single embeddable model — auto-pick.
//     d. Otherwise error: nothing to do without a TTY.
//  4. TTY branches:
//     a. Zero embeddable → error (prompt has nothing to show).
//     b. One embeddable  → auto-pick (prompt would be a one-option
//     checkbox — pointless).
//     c. Two or more     → interactive multi-select. cfg.Model and marker
//     models come back pre-checked with annotated labels; the user is
//     free to add or remove. Submitting empty is a clean exit
//     (errNoModelsSelected).
//
// Returned (selected, fromMarker, err): fromMarker=true only when the
// non-TTY path auto-continued using the marker's recorded list — it signals
// the caller to skip re-writing the marker (already correct on disk).
func resolveIndexModels(opts resolveOpts) ([]string, bool, error) {
	// --all-models wins over everything else.
	if opts.AllModels {
		all := embeddableNames(opts.Discovered)
		if len(all) == 0 {
			return nil, false, fmt.Errorf("--all-models requested but no embedding-capable models found.\n\n%s", listEmbeddable(opts.Discovered))
		}
		fmt.Printf("Using all %d embedding models: %v\n\n", len(all), all)
		return all, false, nil
	}

	// Explicit --model takes precedence over config and prompt. CSV and
	// repeated flags are merged here; duplicate names are de-duped by
	// canonical resolution.
	if flagNames := expandModelFlags(opts.ModelFlags); len(flagNames) > 0 {
		resolved := make([]string, 0, len(flagNames))
		seen := make(map[string]struct{}, len(flagNames))
		for _, n := range flagNames {
			m, ok := findEmbeddableModel(opts.Discovered, n)
			if !ok {
				return nil, false, fmt.Errorf("--model %q not found among embedding-capable Ollama models.\n\nAvailable embedding models:\n%s", n, listEmbeddable(opts.Discovered))
			}
			if _, dup := seen[m]; dup {
				continue
			}
			seen[m] = struct{}{}
			resolved = append(resolved, m)
		}
		if len(resolved) == 1 {
			fmt.Printf("Using model: %s\n\n", resolved[0])
		} else {
			fmt.Printf("Using %d models: %v\n\n", len(resolved), resolved)
		}
		return resolved, false, nil
	}

	embeddable := filterEmbeddable(opts.Discovered)
	if len(embeddable) == 0 {
		return nil, false, fmt.Errorf("no embedding-capable models found in Ollama.\n\nPull an embedding model first:\n  ollama pull nomic-embed-text\n\nSee all options: heimdall-mcp models")
	}

	// Non-TTY: no prompt ever. Resolve via config → marker → single.
	if !opts.IsTTY {
		if opts.ConfigModel != "" {
			if m, ok := findEmbeddableModel(opts.Discovered, opts.ConfigModel); ok {
				fmt.Printf("Using configured model: %s\n\n", m)
				return []string{m}, false, nil
			}
			// Config names a model that isn't pulled / isn't embeddable —
			// fall through to marker / single-pick so the run still has a
			// shot, but surface a notice.
			fmt.Printf("Notice: configured model %q is not available; trying other resolution paths.\n", opts.ConfigModel)
		}
		if len(opts.MarkerModels) > 0 {
			fmt.Printf("Resuming with models from interrupted run: %v\n\n", opts.MarkerModels)
			return append([]string{}, opts.MarkerModels...), true, nil
		}
		if len(embeddable) == 1 {
			fmt.Printf("Using model: %s\n\n", embeddable[0].Name)
			return []string{embeddable[0].Name}, false, nil
		}
		return nil, false, fmt.Errorf("non-interactive run with multiple embeddable models and no --model / --all-models / config.Model / resume marker.\n\nAvailable embedding models:\n%s\nPass --model <name>[,<name>...] or --all-models, or set a default via `heimdall-mcp config set model <name>`.", listEmbeddable(opts.Discovered))
	}

	// TTY branches. One embeddable → auto-pick; two+ → always prompt.
	if len(embeddable) == 1 {
		m := embeddable[0]
		fmt.Printf("1 embedding model found:\n")
		if m.Known != nil {
			fmt.Printf("  %s — %s, %s, %dd\n", stripTag(m.Name), m.Known.Origin, m.Known.Params, m.Dimensions)
		} else {
			fmt.Printf("  %s — %dd\n", stripTag(m.Name), m.Dimensions)
		}
		fmt.Println()
		return []string{m.Name}, false, nil
	}

	// Build the pre-selection map, marker wins on collision.
	preSelected := map[string]string{}
	if opts.ConfigModel != "" {
		if m, ok := findEmbeddableModel(opts.Discovered, opts.ConfigModel); ok {
			preSelected[m] = "configured"
		}
	}
	for _, mm := range opts.MarkerModels {
		if m, ok := findEmbeddableModel(opts.Discovered, mm); ok {
			preSelected[m] = "in progress — resume"
		}
	}

	selected, err := promptModelSelectionFunc(embeddable, preSelected)
	if err != nil {
		return nil, false, err
	}
	if len(selected) == 0 {
		return nil, false, errNoModelsSelected
	}
	return selected, false, nil
}

// discoverEmbeddingModels probes Ollama for all pulled models and tests which ones
// can produce embeddings. Returns the list of results.
func discoverEmbeddingModels(cfg config.Config) ([]discoveredModel, error) {
	ctx := context.Background()
	client := newOllamaClient(cfg)

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

// expandModelFlags turns raw --model arguments into a flat list of names.
// Each element is split on "," so both --model a,b and --model a --model b
// funnel into the same shape. Empty strings are skipped.
func expandModelFlags(flags []string) []string {
	if len(flags) == 0 {
		return nil
	}
	var out []string
	for _, f := range flags {
		for _, part := range strings.Split(f, ",") {
			p := strings.TrimSpace(part)
			if p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// filterEmbeddable returns only the discovered models that produce
// embeddings. Order is preserved so the multi-select matches the user's
// `heimdall-mcp models` output.
func filterEmbeddable(models []discoveredModel) []discoveredModel {
	var out []discoveredModel
	for _, m := range models {
		if m.CanEmbed {
			out = append(out, m)
		}
	}
	return out
}

// embeddableNames returns the canonical names of every embeddable model,
// in discovery order.
func embeddableNames(models []discoveredModel) []string {
	var out []string
	for _, m := range models {
		if m.CanEmbed {
			out = append(out, m.Name)
		}
	}
	return out
}

// findEmbeddableModel matches `name` against the discovered list, tolerating
// the `:latest` tag variance (Ollama reports `foo:latest`, users type `foo`).
// Returns the canonical discovered name on hit.
func findEmbeddableModel(discovered []discoveredModel, name string) (string, bool) {
	for _, d := range discovered {
		if !d.CanEmbed {
			continue
		}
		if d.Name == name || stripTag(d.Name) == name || d.Name == name+":latest" {
			return d.Name, true
		}
	}
	return "", false
}

// listEmbeddable renders a bulleted list of embedding-capable model names for
// error output.
func listEmbeddable(discovered []discoveredModel) string {
	var b strings.Builder
	for _, d := range discovered {
		if d.CanEmbed {
			fmt.Fprintf(&b, "  %s\n", d.Name)
		}
	}
	if b.Len() == 0 {
		return "  (none — run `ollama pull nomic-embed-text`)\n"
	}
	return b.String()
}

// promptModelSelection shows an interactive multi-select for embedding models
// and returns the user's picks. preSelected maps canonical-name → annotation
// and drives BOTH the initial checkbox state and the trailing "(…)" suffix
// on each label. An empty map means no pre-checks, no annotations.
//
// Returning (nil, nil) is the "user submitted with zero selected" path — the
// caller (resolveIndexModels) promotes that to errNoModelsSelected so the CLI
// can exit(0) cleanly and preserve any resume marker.
func promptModelSelection(embeddable []discoveredModel, preSelected map[string]string) ([]string, error) {
	options := make([]huh.Option[string], 0, len(embeddable))
	for _, m := range embeddable {
		base := m.Name
		if m.Known != nil {
			base = fmt.Sprintf("%s — %s, %s, %dd", m.Name, m.Known.Origin, m.Known.Params, m.Dimensions)
		} else {
			base = fmt.Sprintf("%s — %dd", m.Name, m.Dimensions)
		}
		if note, ok := preSelected[m.Name]; ok && note != "" {
			base = fmt.Sprintf("%s  (%s)", base, note)
		}
		_, preChecked := preSelected[m.Name]
		options = append(options, huh.NewOption(base, m.Name).Selected(preChecked))
	}

	// Pre-populate the value slice so huh treats pre-checked items as the
	// initial state. huh's MultiSelect mutates this slice in-place.
	selected := make([]string, 0, len(preSelected))
	for _, m := range embeddable {
		if _, ok := preSelected[m.Name]; ok {
			selected = append(selected, m.Name)
		}
	}

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
