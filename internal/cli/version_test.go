package cli

import (
	"strings"
	"testing"
)

func TestBuildVersion_ContainsHeimdallPrefix(t *testing.T) {
	got := buildVersion()
	if !strings.HasPrefix(got, "heimdall-mcp ") {
		t.Fatalf("expected prefix %q, got %q", "heimdall-mcp ", got)
	}
}

func TestBuildVersion_NeverEmpty(t *testing.T) {
	got := buildVersion()
	if strings.TrimSpace(got) == "" {
		t.Fatal("buildVersion() returned empty string")
	}
}
