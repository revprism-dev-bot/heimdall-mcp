package config

import "testing"

func TestValidateExcludePatterns_AcceptsGlobsAndRelative(t *testing.T) {
	ok := []string{"node_modules", "dist", "build/**", "src/generated"}
	if err := ValidateExcludePatterns(ok); err != nil {
		t.Errorf("unexpected error for %v: %v", ok, err)
	}
	if err := ValidateExcludePatterns(nil); err != nil {
		t.Errorf("nil slice should be valid, got %v", err)
	}
}

func TestValidateExcludePatterns_RejectsAbsolutePath(t *testing.T) {
	bad := []string{"ok", "/etc/foo"}
	err := ValidateExcludePatterns(bad)
	if err == nil {
		t.Fatal("expected error for absolute path")
	}
	// Message should reference the bad path for debuggability.
	got := err.Error()
	if !containsAll(got, "exclude_patterns", "/etc/foo") {
		t.Errorf("error %q must mention key + bad value", got)
	}
}

func TestValidateExcludePatterns_RejectsEmpty(t *testing.T) {
	if err := ValidateExcludePatterns([]string{""}); err == nil {
		t.Error("expected error for empty pattern")
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
