package modules

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	v1 "github.com/easyp-tech/easyp/internal/config/v1"
	"github.com/easyp-tech/easyp/internal/core/path_helpers"
	"github.com/easyp-tech/easyp/internal/sourceview"
)

// SourceRoot associates a physical import root with its module identity.
type SourceRoot struct {
	Path             string
	Module           string
	fileAllowed      func(string) bool
	directoryAllowed func(string) bool
	directory        string
}

// SourceRoots preserves import precedence and module ownership.
type SourceRoots []SourceRoot

// Paths returns a new slice in the same order as the source roots.
func (roots SourceRoots) Paths() []string {
	paths := make([]string, 0, len(roots))
	for _, root := range roots {
		paths = append(paths, root.Path)
	}
	return paths
}

// Walk visits only proto files selected by the module's source metadata.
func (root SourceRoot) Walk(visit func(string) error) error { return root.WalkSelected(nil, visit) }

// WalkSelected applies logical selection before reading or reporting file alias errors.
func (root SourceRoot) WalkSelected(selected func(string) bool, visit func(string) error) error {
	return root.walk(selected, false, nil, visit)
}

// WalkSelected walks one root with the graph's logical and target ownership.
func (roots SourceRoots) WalkSelected(root SourceRoot, selected func(string) bool, visit func(string) error) error {
	return root.walk(selected, false, roots.ownsSelectedPhysical, visit)
}

func (root SourceRoot) walk(selected func(string) bool, namesOnly bool, targetAllowed func(string) bool, visit func(string) error) error {
	boundary := root.boundary()
	relative, err := filepath.Rel(boundary, root.Path)
	if err != nil {
		return fmt.Errorf("Rel: %w", err)
	}
	physicalRoot, err := sourceview.ResolveLocal(context.Background(), boundary, relative)
	if err != nil {
		return fmt.Errorf("ResolveLocal: %w", err)
	}
	physicalBoundary := physicalSourcePath(boundary)
	physicalRootPath := filepath.Join(physicalBoundary, filepath.FromSlash(physicalRoot.Path))
	return sourceview.WalkLocal(context.Background(), boundary, relative, func(logical string, resolved sourceview.Resolution, walkErr error) error {
		path := filepath.Join(boundary, filepath.FromSlash(logical))
		if filepath.Ext(path) == ".proto" && root.fileAllowed != nil && !root.fileAllowed(path) {
			return nil
		}
		if errors.Is(walkErr, sourceview.ErrCycle) && resolved.Info != nil && resolved.Info.IsDir() {
			if path_helpers.HiddenOrVendorSourcePath(root.Path, path) || (root.directoryAllowed != nil && !root.directoryAllowed(path)) {
				return nil
			}
			return walkErr
		}
		if root.directoryAllowed != nil && resolved.Info != nil && resolved.Info.IsDir() && !root.directoryAllowed(path) {
			return fs.SkipDir
		}
		if path_helpers.ShouldSkipV1SourceDir(root.Path, path) {
			return fs.SkipDir
		}
		if filepath.Ext(path) == ".proto" && selected != nil && !selected(path) {
			return nil
		}
		if errors.Is(walkErr, sourceview.ErrNestedRepository) && targetAllowed != nil && targetAllowed(filepath.Join(physicalBoundary, filepath.FromSlash(resolved.Path))) {
			walkErr = nil
		}
		if walkErr != nil && namesOnly && filepath.Ext(path) == ".proto" {
			return visit(path)
		}
		if walkErr != nil {
			if filepath.Ext(path) != ".proto" && path != root.Path {
				return nil
			}
			return walkErr
		}
		physical := filepath.Join(physicalBoundary, filepath.FromSlash(resolved.Path))
		if !resolved.Info.IsDir() && root.fileAllowed != nil && !root.fileAllowed(physical) {
			return nil
		}
		if resolved.Info.IsDir() {
			if path_helpers.ShouldSkipV1SourceDir(physicalRootPath, physical) {
				return fs.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".proto" {
			return nil
		}
		return visit(path)
	})
}

func (root SourceRoot) boundary() string {
	if root.directory != "" {
		return root.directory
	}
	return root.Path
}

// OpenSourceFile opens a source at its logical location through its owning root.
func (roots SourceRoots) OpenSourceFile(path string) (io.ReadCloser, error) {
	return roots.SourceFileOpener()(path)
}

// SourceFileOpener returns OpenSourceFile with the roots' selection and
// ownership resolved once, for repeated opens against an unchanged root set.
func (roots SourceRoots) SourceFileOpener() func(string) (io.ReadCloser, error) {
	allowed := roots.FileAllowed()
	owns := roots.ownsSelectedPhysicalFunc()
	opener := sourceview.NewLocalOpener()
	physical := newPhysicalPaths()
	return func(path string) (io.ReadCloser, error) {
		return roots.openSourceFile(path, allowed, owns, opener, physical)
	}
}

func (r SourceRoots) openSourceFile(path string, allowed, owns func(string) bool, opener *sourceview.LocalOpener, physical *physicalPaths) (io.ReadCloser, error) {
	if allowed != nil && !allowed(path) {
		return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrNotExist}
	}
	for _, root := range r {
		if !sourcePathWithin(path, root.Path) {
			continue
		}
		relative, err := filepath.Rel(root.boundary(), path)
		if err != nil {
			return nil, fmt.Errorf("Rel: %w", err)
		}
		physicalPath := func(resolved sourceview.Resolution) string {
			return filepath.Join(physical.boundary(root.boundary()), filepath.FromSlash(resolved.Path))
		}
		file, err := opener.OpenSelected(context.Background(), root.boundary(), relative, func(resolved sourceview.Resolution) bool { return allowed == nil || allowed(physicalPath(resolved)) }, func(resolved sourceview.Resolution) bool { return owns(physicalPath(resolved)) })
		if err != nil {
			return nil, fmt.Errorf("OpenLocal: %w", err)
		}
		return file, nil
	}
	return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrNotExist}
}

// ReadSourceFile reads a selected logical source through its owning root.
func (roots SourceRoots) ReadSourceFile(path string) (_ []byte, resultErr error) {
	file, err := roots.OpenSourceFile(path)
	if err != nil {
		return nil, fmt.Errorf("OpenSourceFile: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("ReadAll: %w", err)
	}
	return data, nil
}

func (root SourceRoot) allows(path, importRoot string) bool {
	return root.allowsPath(filepath.Clean(path), filepath.Clean(importRoot)) &&
		root.allowsPath(physicalSourcePath(path), physicalSourcePath(importRoot))
}

func (root SourceRoot) allowsPath(path, importRoot string) bool {
	return root.allowsPathWith(path, importRoot, path_helpers.ShouldSkipV1SourceDir)
}

func (r SourceRoot) allowsPathWith(path, importRoot string, shouldSkip func(root, path string) bool) bool {
	if r.fileAllowed != nil && !r.fileAllowed(path) {
		return false
	}
	// A containing consumer root does not own files inside a nested module.
	// Import lookup must use the same boundaries as source discovery.
	for directory := filepath.Dir(path); sourcePathWithin(directory, importRoot) && filepath.Clean(directory) != filepath.Clean(importRoot); directory = filepath.Dir(directory) {
		if shouldSkip(importRoot, directory) {
			return false
		}
	}
	return true
}

// FileAllowed returns an immutable source filter for physical import lookup.
// Paths outside these roots remain available to the consumer's own inputs.
// Overlapping roots select the union of their allowed files. Both the import's
// spelling and its physical target must pass their respective owning selections.
func (roots SourceRoots) FileAllowed() func(string) bool {

	type selection struct {
		path string
		root SourceRoot
	}
	physical := newPhysicalPaths()
	selections := make([]selection, 0, len(roots))
	for _, root := range roots {
		selections = append(selections, selection{path: filepath.Clean(root.Path), root: root})
		canonical, err := filepath.EvalSymlinks(root.Path)
		if err == nil && canonical != root.Path {
			selections = append(selections, selection{path: canonical, root: root})
		}
	}
	allowedPath := func(path string) bool {
		inside := false
		for _, root := range selections {
			if !sourcePathWithin(path, root.path) {
				continue
			}
			inside = true
			if root.root.allowsPathWith(path, root.path, physical.shouldSkip) {
				return true
			}
		}
		return !inside
	}
	return func(path string) bool {
		return allowedPath(filepath.Clean(path)) && allowedPath(physical.resolve(path))
	}
}

func sourcePathWithin(path, directory string) bool {
	relative, err := filepath.Rel(directory, path)
	return err == nil && filepath.IsLocal(relative)
}

func physicalSourcePath(path string) string {
	canonical, err := filepath.EvalSymlinks(path)
	if err == nil {
		return canonical
	}
	return filepath.Clean(path)
}

func moduleFileSelection(directory string, filters []v1.ProtoFileFilter, includeAncestors bool) func(string) bool {
	canonical, err := filepath.EvalSymlinks(directory)
	if err != nil {
		canonical = directory
	}
	// Metadata is immutable for the lifetime of an effective graph.
	filters = slices.Clone(filters)
	for i, filter := range filters {
		filters[i].Includes = slices.Clone(filter.Includes)
		filters[i].Excludes = slices.Clone(filter.Excludes)
	}
	// Root aliases preserve relative Buf selection in their physical namespace.
	// Selection leaves stay literal, so excluding an alias does not exclude its target.
	logicalFilters := slices.Clone(filters)
	for _, filter := range logicalFilters {
		resolved, err := sourceview.ResolveLocal(context.Background(), directory, filter.Root)
		if err != nil || len(resolved.Links) == 0 {
			continue
		}
		physical := filter
		physical.Root = filepath.FromSlash(resolved.Path)
		translate := func(names []string) []string {
			translated := make([]string, 0, len(names))
			for _, name := range names {
				relative, err := filepath.Rel(filter.Root, name)
				if err != nil || !filepath.IsLocal(relative) {
					continue
				}
				translated = append(translated, filepath.Join(physical.Root, relative))
			}
			return translated
		}
		physical.Includes, physical.Excludes = translate(filter.Includes), translate(filter.Excludes)
		filters = append(filters, physical)
	}
	return func(path string) bool {
		base := directory
		if !sourcePathWithin(path, base) {
			base = canonical
		}
		relative, err := filepath.Rel(base, path)
		if err != nil || !filepath.IsLocal(relative) {
			return true
		}
		name := filepath.ToSlash(relative)
		inside := false
		within := func(directory string) bool {
			directory = filepath.ToSlash(directory)
			return directory == "." || name == directory || strings.HasPrefix(name, directory+"/")
		}
		for _, filter := range filters {
			inScope := within(filter.Root) || (includeAncestors && v1.PathSelectorMatches(name, filepath.ToSlash(filter.Root)))
			if !inScope {
				continue
			}
			inside = true
			included := len(filter.Includes) == 0 || slices.ContainsFunc(filter.Includes, func(included string) bool {
				return within(included) || (includeAncestors && v1.PathSelectorMatches(name, filepath.ToSlash(included)))
			})
			if included && !slices.ContainsFunc(filter.Excludes, within) {
				return true
			}
		}
		return !inside
	}
}

// FileModules maps import paths to their owning modules for managed selectors.
// Callers check import collisions before using this mapping.
func (roots SourceRoots) FileModules() (map[string]string, error) {
	index, err := IndexSources(roots)
	if err != nil {
		return nil, err
	}
	return index.FileModules(roots)
}

// ModuleSources validates module roots and resolves them relative to its directory.
func ModuleSources(directory string, module v1.Module) (SourceRoots, error) {
	roots := make(SourceRoots, 0, len(module.Roots))
	var allowed, dirAllowed func(string) bool
	if len(module.ProtoFilters) > 0 {
		allowed = moduleFileSelection(directory, module.ProtoFilters, false)
		dirAllowed = moduleFileSelection(directory, module.ProtoFilters, true)
	}
	for _, root := range module.Roots {
		path := filepath.Join(directory, root)
		resolved, err := sourceview.ResolveLocal(context.Background(), directory, root)
		info := resolved.Info
		if err != nil {
			return nil, fmt.Errorf("Stat: %w", err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("module %s has invalid root %q: not a directory", module.Name, root)
		}
		boundary := directory
		if filepath.Clean(path) == filepath.Clean(directory) {
			boundary = ""
		}
		roots = append(roots, SourceRoot{Path: path, Module: module.Name, fileAllowed: allowed, directoryAllowed: dirAllowed, directory: boundary})
	}
	return roots, nil
}

func (roots SourceRoots) ownsSelectedPhysical(path string) bool {
	return roots.ownsSelectedPhysicalFunc()(path)
}

// ownsSelectedPhysicalFunc resolves each root's physical path once for repeated checks.
func (roots SourceRoots) ownsSelectedPhysicalFunc() func(string) bool {
	physicalRoots := make([]string, 0)
	for _, root := range roots {
		physicalRoots = append(physicalRoots, physicalSourcePath(root.Path))
	}
	return func(path string) bool {
		for i, root := range roots {
			if sourcePathWithin(path, physicalRoots[i]) && root.allowsPath(path, physicalRoots[i]) {
				return true
			}
		}
		return false
	}
}
