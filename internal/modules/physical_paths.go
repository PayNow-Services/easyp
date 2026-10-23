package modules

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/easyp-tech/easyp/internal/core/path_helpers"
)

// physicalPaths memoizes symlink resolution and nested-module checks for one
// source filter. Each directory is resolved and listed once, so files sharing
// a directory skip the per-component lookups of filepath.EvalSymlinks, which
// are especially expensive on Windows.
type physicalPaths struct {
	mu          sync.Mutex
	directories map[string]*physicalDirectory
	skips       map[physicalSkipKey]bool
	boundaries  map[string]string
}

type physicalSkipKey struct {
	root string
	path string
}

type physicalDirectory struct {
	once sync.Once
	path string
	ok   bool
	// names maps an entry's on-disk name to its spelling, or "" when the entry
	// must go through EvalSymlinks (aliases, irregular files).
	names map[string]string
	// folded maps lower-cased names to their on-disk spellings on Windows,
	// where EvalSymlinks returns the on-disk case of a differently cased path.
	folded map[string][]string
}

func newPhysicalPaths() *physicalPaths {
	return &physicalPaths{
		directories: make(map[string]*physicalDirectory),
		skips:       make(map[physicalSkipKey]bool),
		boundaries:  make(map[string]string),
	}
}

// boundary memoizes physicalSourcePath for source root boundaries.
func (p *physicalPaths) boundary(path string) string {
	p.mu.Lock()
	physical, ok := p.boundaries[path]
	p.mu.Unlock()
	if ok {
		return physical
	}

	physical = physicalSourcePath(path)
	p.mu.Lock()
	p.boundaries[path] = physical
	p.mu.Unlock()
	return physical
}

// resolve returns the same result as physicalSourcePath.
func (p *physicalPaths) resolve(path string) string {
	clean := filepath.Clean(path)
	name := filepath.Base(clean)
	parent := filepath.Dir(clean)
	if parent == clean || name == "." || name == ".." || strings.Contains(clean, "..") {
		return physicalSourcePath(path)
	}

	directory := p.directory(parent)
	if !directory.ok {
		return clean
	}
	spelling, ok := directory.lookup(name)
	if !ok {
		return physicalSourcePath(path)
	}
	return filepath.Join(directory.path, spelling)
}

// shouldSkip memoizes path_helpers.ShouldSkipV1SourceDir per root and directory.
func (p *physicalPaths) shouldSkip(root, path string) bool {
	key := physicalSkipKey{root: root, path: path}
	p.mu.Lock()
	skip, ok := p.skips[key]
	p.mu.Unlock()
	if ok {
		return skip
	}

	skip = path_helpers.ShouldSkipV1SourceDir(root, path)
	p.mu.Lock()
	p.skips[key] = skip
	p.mu.Unlock()
	return skip
}

func (p *physicalPaths) directory(path string) *physicalDirectory {
	p.mu.Lock()
	directory, ok := p.directories[path]
	if !ok {
		directory = &physicalDirectory{}
		p.directories[path] = directory
	}
	p.mu.Unlock()
	directory.once.Do(func() { directory.load(path) })
	return directory
}

func (d *physicalDirectory) load(path string) {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return
	}
	d.path = canonical
	d.ok = true

	entries, err := os.ReadDir(canonical)
	if err != nil {
		return
	}
	d.names = make(map[string]string)
	if runtime.GOOS == "windows" {
		d.folded = make(map[string][]string)
	}
	for _, entry := range entries {
		spelling := entry.Name()
		if !entry.Type().IsRegular() && !entry.IsDir() {
			spelling = ""
		}
		d.names[entry.Name()] = spelling
		if d.folded != nil {
			folded := strings.ToLower(entry.Name())
			d.folded[folded] = append(d.folded[folded], spelling)
		}
	}
}

// lookup reports the physical spelling of a plain entry, or false when the
// caller must fall back to EvalSymlinks.
func (d *physicalDirectory) lookup(name string) (string, bool) {
	if d.names == nil {
		return "", false
	}
	spelling, ok := d.names[name]
	if ok {
		return spelling, spelling != ""
	}
	candidates := d.folded[strings.ToLower(name)]
	if len(candidates) != 1 || candidates[0] == "" {
		return "", false
	}
	return candidates[0], true
}
