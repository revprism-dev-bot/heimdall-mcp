package registry

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// ProjectEntry maps a project name to its index location.
type ProjectEntry struct {
	Name   string `json:"name"`   // short name (e.g. "payments-analyzer")
	Path   string `json:"path"`   // absolute path to project root
	DBPath string `json:"dbPath"` // absolute path to .heimdall_db directory
}

// Registry manages the list of indexed projects.
type Registry struct {
	mu       sync.Mutex
	Projects []ProjectEntry `json:"projects"`
	filePath string
}

// registryPath returns the path to the registry file.
func registryPath() string {
	xdgConfig := os.Getenv("XDG_CONFIG_HOME")
	if xdgConfig == "" {
		home, _ := os.UserHomeDir()
		xdgConfig = filepath.Join(home, ".config")
	}
	return filepath.Join(xdgConfig, "heimdall-mcp", "projects.json")
}

// LoadRegistry loads the project registry from disk.
//
// A missing file is not an error (returns an empty registry — common on
// first run). A corrupt file IS surfaced via log.Printf so operators
// see the degradation instead of silently losing their registry —
// downstream resolvers (resolveRunIndexBaseDir, resolveStatusBaseDir)
// lean on registry integrity, so a silent reset was masking real state
// loss. Atomic save (a separate PR) will prevent the corruption in the
// first place; this log surfaces the reader side for now.
func LoadRegistry() *Registry {
	r := &Registry{filePath: registryPath()}

	data, err := os.ReadFile(r.filePath)
	if err != nil {
		return r
	}
	if err := json.Unmarshal(data, r); err != nil {
		log.Printf("heimdall: registry file %s is corrupt; starting with empty registry (existing entries will be silently dropped until a Save overwrites it): %v", r.filePath, err)
		// Reset so a partially-parsed registry can't leak into downstream
		// code paths. Any subsequent Register() call will Save() a clean
		// file.
		r.Projects = nil
	}
	return r
}

// Save persists the registry to disk.
func (r *Registry) Save() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	dir := filepath.Dir(r.filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(r.filePath, data, 0644)
}

// Register adds or updates a project in the registry.
func (r *Registry) Register(name, projectPath, dbPath string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for i, p := range r.Projects {
		if p.Name == name || p.Path == projectPath {
			r.Projects[i] = ProjectEntry{Name: name, Path: projectPath, DBPath: dbPath}
			return
		}
	}
	r.Projects = append(r.Projects, ProjectEntry{Name: name, Path: projectPath, DBPath: dbPath})
}

// Find looks up a project by name or path. Returns nil if not found.
// Also checks CWD as a fallback — if CWD is inside a registered project, returns that.
func (r *Registry) Find(query string) *ProjectEntry {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Exact match by name or path
	for _, p := range r.Projects {
		if p.Name == query || p.Path == query {
			return &p
		}
	}

	// Partial name match (case insensitive substring)
	for _, p := range r.Projects {
		if containsIgnoreCase(p.Name, query) {
			return &p
		}
	}

	return nil
}

// FindByCWD returns the project whose path is a prefix of the given directory.
func (r *Registry) FindByCWD(cwd string) *ProjectEntry {
	r.mu.Lock()
	defer r.mu.Unlock()

	var best *ProjectEntry
	bestLen := 0
	for i, p := range r.Projects {
		if len(p.Path) > bestLen && IsSubpath(cwd, p.Path) {
			best = &r.Projects[i]
			bestLen = len(p.Path)
		}
	}
	return best
}

// All returns all registered projects.
func (r *Registry) All() []ProjectEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ProjectEntry, len(r.Projects))
	copy(out, r.Projects)
	return out
}

// IsSubpath reports whether child is under parent (or equal).
func IsSubpath(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (len(rel) > 0 && rel[0] != '.')
}

func containsIgnoreCase(s, substr string) bool {
	if len(substr) > len(s) {
		return false
	}
	for i := 0; i <= len(s)-len(substr); i++ {
		match := true
		for j := 0; j < len(substr); j++ {
			a, b := s[i+j], substr[j]
			if a >= 'A' && a <= 'Z' {
				a += 32
			}
			if b >= 'A' && b <= 'Z' {
				b += 32
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
