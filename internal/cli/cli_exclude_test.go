package cli

import (
	"strings"
	"testing"

	"github.com/caio-silva/heimdall-mcp/internal/config"
)

// TestCLIConfigSet_ExcludePatterns_ValidatesAbsolute asserts the shared
// validator fires via the CLI `config set exclude_patterns` path (M6
// regression — rejected at both surfaces).
func TestCLIConfigSet_ExcludePatterns_ValidatesAbsolute(t *testing.T) {
	cfg := config.DefaultConfig()
	err := setConfigKey(&cfg, "exclude_patterns", `["ok","/etc/foo"]`)
	if err == nil {
		t.Fatal("expected error for absolute path in exclude_patterns")
	}
	if !strings.Contains(err.Error(), "/etc/foo") {
		t.Errorf("error should mention the bad value, got: %v", err)
	}
	// Config must not be mutated on validation failure.
	if len(cfg.ExcludePatterns) != len(config.DefaultConfig().ExcludePatterns) {
		t.Errorf("config mutated on failure: %v", cfg.ExcludePatterns)
	}
}

// TestCLIConfigSet_ExcludePatterns_Accepts asserts a clean JSON array is
// parsed and set correctly.
func TestCLIConfigSet_ExcludePatterns_Accepts(t *testing.T) {
	cfg := config.DefaultConfig()
	if err := setConfigKey(&cfg, "exclude_patterns", `["node_modules","dist","build"]`); err != nil {
		t.Fatal(err)
	}
	want := []string{"node_modules", "dist", "build"}
	if len(cfg.ExcludePatterns) != len(want) {
		t.Fatalf("len = %d, want %d", len(cfg.ExcludePatterns), len(want))
	}
	for i, v := range want {
		if cfg.ExcludePatterns[i] != v {
			t.Errorf("[%d] = %q, want %q", i, cfg.ExcludePatterns[i], v)
		}
	}
}

// TestCLIConfigGet_ExcludePatterns confirms the key is exposed via get.
func TestCLIConfigGet_ExcludePatterns(t *testing.T) {
	cfg := config.DefaultConfig()
	val, ok := getConfigKey(&cfg, "exclude_patterns")
	if !ok {
		t.Fatal("exclude_patterns must be a known config key")
	}
	if _, ok := val.([]string); !ok {
		t.Errorf("expected []string, got %T", val)
	}
}
