package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/registry"
)

func TestClassifySource(t *testing.T) {
	tests := []struct {
		kind string
		want string
	}{
		{"external", "external"},
		{"memory", "memory"},
		{"file", "code"},
		{"function", "code"},
		{"type", "code"},
		{"paragraph", "code"},
		{"", "code"},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			got := classifySource(tt.kind)
			if got != tt.want {
				t.Errorf("classifySource(%q) = %q, want %q", tt.kind, got, tt.want)
			}
		})
	}
}

func TestValidateSourceType(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", "custom"},
		{"ticket", "ticket"},
		{"doc", "doc"},
		{"pr", "pr"},
		{"message", "message"},
		{"changelog", "changelog"},
		{"note", "note"},
		{"custom", "custom"},
		{"invalid", ""},
		{"CODE", ""},
		{"Ticket", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := validateSourceType(tt.input)
			if got != tt.want {
				t.Errorf("validateSourceType(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestValidateSource(t *testing.T) {
	tests := []struct {
		name    string
		source  string
		wantErr bool
	}{
		{"valid simple", "JIRA-123", false},
		{"valid with colon", "confluence:My Page", false},
		{"valid with hash", "slack:#channel", false},
		{"valid with slash", "github/repo", false},
		{"valid with at", "user@domain", false},
		{"path traversal", "../../../etc/passwd", true},
		{"null byte", "source\x00evil", true},
		{"newline", "source\nevil", true},
		{"too long", string(make([]byte, 501)), true},
		{"empty special chars", "source;drop table", true},
		{"backtick", "source`cmd`", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSource(tt.source)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateSource(%q) error = %v, wantErr %v", tt.source, err, tt.wantErr)
			}
		})
	}
}

func TestResolveDBDir(t *testing.T) {
	reg := &registry.Registry{}
	srv := &Server{
		Cfg:      config.DefaultConfig(),
		Registry: reg,
	}

	// With no projects registered, should fall back to cwd/.heimdall_db
	result := srv.resolveDBDir("")
	cwd, _ := os.Getwd()
	expected := filepath.Join(cwd, ".heimdall_db")
	if result != expected {
		t.Errorf("resolveDBDir('') = %q, want %q", result, expected)
	}

	// Register a project
	reg.Register("testproj", "/tmp/testproj", "/tmp/testproj/.heimdall_db")

	// With project name
	result = srv.resolveDBDir("testproj")
	if result != "/tmp/testproj/.heimdall_db" {
		t.Errorf("resolveDBDir('testproj') = %q, want %q", result, "/tmp/testproj/.heimdall_db")
	}

	// Non-existent project falls back
	result = srv.resolveDBDir("nonexistent")
	if result != expected {
		t.Errorf("resolveDBDir('nonexistent') = %q, want %q", result, expected)
	}
}

func TestResolveDBDirForRead(t *testing.T) {
	reg := &registry.Registry{}
	srv := &Server{
		Cfg:      config.DefaultConfig(),
		Registry: reg,
	}

	// Non-existent directory returns empty
	result := srv.resolveDBDirForRead("")
	if result != "" {
		t.Errorf("resolveDBDirForRead with non-existent dir = %q, want empty", result)
	}

	// Create a temp dir and register it
	tmpDir := t.TempDir()
	dbDir := filepath.Join(tmpDir, ".heimdall_db")
	os.MkdirAll(dbDir, 0755)
	reg.Register("existing", tmpDir, dbDir)

	result = srv.resolveDBDirForRead("existing")
	if result != dbDir {
		t.Errorf("resolveDBDirForRead('existing') = %q, want %q", result, dbDir)
	}
}
