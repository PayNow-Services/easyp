package modules

import (
	"fmt"
	"path/filepath"
)

// SourceIndex records the selected sources of a root set from a single walk,
// so collision checks and module ownership do not walk the roots again.
type SourceIndex struct {
	roots SourceRoots
	files [][]string
}

type sourceRootKey struct {
	path      string
	module    string
	directory string
}

func (root SourceRoot) key() sourceRootKey {
	return sourceRootKey{path: root.Path, module: root.Module, directory: root.directory}
}

// IndexSources walks every root once with the set's selection and ownership.
func IndexSources(roots SourceRoots) (*SourceIndex, error) {
	index := &SourceIndex{roots: roots}
	allowed := roots.FileAllowed()
	owns := roots.ownsSelectedPhysicalFunc()
	for _, root := range roots {
		var files []string
		err := root.walk(allowed, true, owns, func(path string) error {
			if allowed != nil && !allowed(path) {
				return nil
			}
			files = append(files, path)
			return nil
		})
		if err != nil {
			return nil, err
		}
		index.files = append(index.files, files)
	}
	return index, nil
}

// CheckCollisions respects each module's source selection when comparing import names.
func (index *SourceIndex) CheckCollisions() error {
	seen := make(map[string]string)
	for i, root := range index.roots {
		for _, path := range index.files[i] {
			importPath, err := filepath.Rel(root.Path, path)
			if err != nil {
				return err
			}
			importPath = filepath.ToSlash(importPath)
			if previous, ok := seen[importPath]; ok && previous != path {
				return fmt.Errorf("duplicate import path %q: %s and %s", importPath, previous, path)
			}
			seen[importPath] = path
		}
	}
	return nil
}

// FileModules maps import paths to their owning modules for managed selectors.
// Later roots in order take precedence, and each must belong to the indexed set.
// Callers check import collisions before using this mapping.
func (index *SourceIndex) FileModules(order SourceRoots) (map[string]string, error) {
	positions := make(map[sourceRootKey]int)
	for i, root := range index.roots {
		positions[root.key()] = i
	}
	modules := make(map[string]string)
	for _, root := range order {
		i, ok := positions[root.key()]
		if !ok {
			return nil, fmt.Errorf("source root %s is not indexed", root.Path)
		}
		for _, path := range index.files[i] {
			relative, err := filepath.Rel(root.Path, path)
			if err != nil {
				return nil, err
			}
			modules[filepath.ToSlash(relative)] = root.Module
		}
	}
	return modules, nil
}

// CheckSourceCollisions respects each module's source selection when comparing import names.
func CheckSourceCollisions(roots SourceRoots) error {
	index, err := IndexSources(roots)
	if err != nil {
		return err
	}
	return index.CheckCollisions()
}
