package registry

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

// reservedSentinelName matches dunder sentinel names like "__root__" that
// the filter path reserves (see heimdall.SubProjectRoot). Registering a
// project whose Name matches this pattern would create ambiguity between
// the sentinel ("filter to outer/wrapper only") and a sub-repo literally
// named "__root__". Per locked R-v2-1 / plan §12, sub-repo registration
// rejects such names with a WARN log. The pattern is intentionally
// restrictive (lowercase ASCII only) — operational sub-repo names from
// tooling never match.
var reservedSentinelName = regexp.MustCompile(`^__[a-z]+__$`)

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
//
// After a successful load, `repairStaleDBPaths` walks every entry and
// rewrites DBPath to the canonical <path>/.heimdall_db location when:
//   - DBPath != canonical location, AND
//   - the canonical location exists on disk.
//
// This closes the A-L-5 / D-02 repair gap — the symptom seen in the
// handoff where a sub-repo entry's dbPath pointed at its parent wrapper
// despite the sub-repo having its own .heimdall_db/ directory. Repair
// NEVER deletes entries (D-08 I12); it only updates dbPath. Every
// rewrite is logged at WARN so operators can audit the change.
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
		return r
	}
	r.repairStaleDBPaths()
	return r
}

// repairStaleDBPaths normalises in-memory entries whose DBPath does not
// match the canonical <Path>/.heimdall_db location but where the
// canonical location exists on disk. NEVER deletes entries; only
// updates DBPath. Called only from LoadRegistry; holds the registry
// mutex because it mutates r.Projects.
func (r *Registry) repairStaleDBPaths() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, p := range r.Projects {
		if p.Path == "" {
			continue
		}
		canonical := filepath.Clean(filepath.Join(p.Path, ".heimdall_db"))
		if filepath.Clean(p.DBPath) == canonical {
			continue
		}
		info, err := os.Stat(canonical)
		if err != nil || !info.IsDir() {
			continue
		}
		log.Printf("heimdall: repairing stale DBPath for project %q: %s → %s (canonical store exists)", p.Name, p.DBPath, canonical)
		r.Projects[i].DBPath = canonical
	}
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
//
// Dedupe is keyed on the (Name, Path) TUPLE — the pre-fix code used
// `Name OR Path` which meant two distinct projects that happened to share
// a sub-repo basename (e.g. two wrappers each containing a "shared/"
// sub-repo) would clobber each other. The tuple contract is the locked
// A-L-3 / D-03 decision and is guarded by
// TestRegistry_TupleDedupe_AllowsSharedNamesAtDistinctPaths.
//
// Reserved-sentinel names (`^__[a-z]+__$`, e.g. `__root__`) are rejected
// with a WARN log and a no-op — those names are reserved for filter
// sentinels (see heimdall.SubProjectRoot). Rejecting at Register keeps
// downstream search filters unambiguous. Guarded by
// TestRegistry_Register_RejectsReservedSentinelName.
func (r *Registry) Register(name, projectPath, dbPath string) {
	if reservedSentinelName.MatchString(name) {
		log.Printf("heimdall: refusing to register project with reserved sentinel name %q (pattern ^__[a-z]+__$); pick a different name", name)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	for i, p := range r.Projects {
		if p.Name == name && p.Path == projectPath {
			// Tuple match: update in place (usually just refreshes DBPath
			// after a legacy → canonical move).
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
