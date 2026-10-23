package gitmodules

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/easyp-tech/easyp/internal/adapters/gitcommand"
	moduleconfig "github.com/easyp-tech/easyp/internal/adapters/module_config"
	v1 "github.com/easyp-tech/easyp/internal/config/v1"
)

func v1ModuleCachePath(cacheRoot string, entry v1.LockedModule) string {
	identity := entry.Commit + ":" + entry.Hash
	if roots := v1RootSelectionKey(entry.Roots); roots != "" {
		identity += ":roots:" + roots
	}
	return filepath.Join(cacheRoot, "modules", v1CacheSourceKey(entry.Source), v1CacheSourceKey(identity))
}

// Install installs each locked Git commit, verifying its content hash
// before accepting a downloaded or already cached module.
func (c *Cache) Install(ctx context.Context, lock v1.Lock) error {
	return c.install(ctx, lock, true)
}

// VerifyCached verifies already installed entries without downloading or writing.
// Missing content must be acquired by an explicit mod download first.
func (c *Cache) VerifyCached(ctx context.Context, lock v1.Lock) error {
	return c.install(ctx, lock, false)
}

func (c *Cache) install(ctx context.Context, lock v1.Lock, acquire bool) error {
	if acquire {
		if _, err := gitcommand.Timeout(); err != nil {
			return fmt.Errorf("Timeout: %w", err)
		}
	}
	if err := lock.Validate(); err != nil {
		return fmt.Errorf("Validate: %w", err)
	}
	errs := make([]*error, 0)
	var group errgroup.Group
	group.SetLimit(installConcurrency)
	for _, entry := range lock.Modules {
		var entryErr error
		errs = append(errs, &entryErr)
		group.Go(func() error {
			entryErr = c.installOnce(ctx, entry, acquire)
			return nil
		})
	}
	_ = group.Wait()
	for _, err := range errs {
		if *err != nil {
			return *err
		}
	}
	return nil
}

// installConcurrency bounds parallel module installs, which are mostly network fetches.
const installConcurrency = 8

func (c *Cache) installOnce(ctx context.Context, entry v1.LockedModule, acquire bool) error {
	err := ctx.Err()
	if err != nil {
		return fmt.Errorf("Err: %w", err)
	}
	key := v1ModuleCachePath(c.root, entry) + "\x00" + entry.Version
	if c.installed.contains(key) {
		return nil
	}
	err = c.installEntry(ctx, entry, acquire)
	if err != nil {
		return err
	}
	c.installed.add(key)
	return nil
}

// installedModules records entries installed and verified by one cache
// handle. A nil set records nothing.
type installedModules struct {
	mu      sync.Mutex
	entries map[string]bool
}

func (m *installedModules) contains(key string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.entries[key]
}

func (m *installedModules) add(key string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[key] = true
}

func (c *Cache) installEntry(ctx context.Context, entry v1.LockedModule, acquire bool) (err error) {
	ctx = c.operationContext(ctx, entry.Source, entry.Commit)
	finish := gitcommand.Start(ctx, "install module")
	defer func() { finish(err) }()
	installed := v1ModuleCachePath(c.root, entry)
	info, err := os.Lstat(installed)
	if errors.Is(err, os.ErrNotExist) {
		if !acquire {
			return fmt.Errorf("policy module %s@%s is not cached; run easyp mod download before validate-config: %w", entry.Source, entry.Commit, err)
		}
		if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
			return fmt.Errorf("MkdirAll: %w", err)
		}
		unlock, lockErr := lockObjectRepository(ctx, installed+".install.lock")
		if lockErr != nil {
			return fmt.Errorf("lockObjectRepository: %w", lockErr)
		}
		defer unlock()

		// Another process may have completed the same atomic install while this
		// process waited for the per-snapshot lock. Reuse and verify that result
		// rather than racing a second rename into the same cache directory.
		info, err = os.Lstat(installed)
		if errors.Is(err, os.ErrNotExist) {
			if c.logger != nil {
				c.logger.Info(ctx, "downloading module", slog.String("source", entry.Source), slog.String("version", entry.Version))
			}
			if err := fetchPinnedV1Module(ctx, entry, c.root, installed, c.proxy); err != nil {
				return err
			}
			info, err = os.Lstat(installed)
		}
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s: cached path %q is not a directory; inspect this path and keep protobuf.lock unchanged", entry.Source, installed)
	}
	if err := verifyInstalledV1Module(installed, entry, acquire); err != nil {
		return fmt.Errorf("verify cached %s at %q: %w", entry.Source, installed, err)
	}
	if err := moduleconfig.ValidateLegacyMajor(installed, entry.Source, entry.Version); err != nil {
		return fmt.Errorf("ValidateLegacyMajor: %w", err)
	}
	return nil
}

// RepairPolicySources is reserved for explicit mod download. Ordinary native
// source use does not require objects for unselected producer policy fragments.
func (c *Cache) RepairPolicySources(ctx context.Context, lock v1.Lock) error {
	for _, entry := range lock.Modules {
		if err := c.ensurePinnedSource(c.operationContext(ctx, entry.Source, entry.Commit), entry, true); err != nil {
			return fmt.Errorf("ensurePinnedSource: %w", err)
		}
	}
	// A corrupted shared store may have been quarantined after an earlier pin
	// passed verification. Restore every pin additively after all quarantines.
	for _, entry := range lock.Modules {
		if err := c.ensurePinnedSource(c.operationContext(ctx, entry.Source, entry.Commit), entry, false); err != nil {
			return fmt.Errorf("ensurePinnedSource: %w", err)
		}
	}
	return nil
}
