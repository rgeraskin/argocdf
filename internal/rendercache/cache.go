// Package rendercache implements a content-addressed persistent cache of
// rendered ArgoCD application manifests. Repeat runs can skip re-rendering (and
// even skip the branch checkout) when nothing relevant to a render changed.
//
// Storage is one JSON file per key under a cache directory (default
// os.UserCacheDir()/argocdf/render). Corrupt or unreadable entries are treated
// as cache misses and best-effort removed. Writes go through a temp file +
// rename for atomicity.
package rendercache

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/charmbracelet/log"
)

// GC defaults, used when the cache is created. They bound the cache both by age
// and by total size; documented here rather than exposed as flags to keep the
// CLI surface small.
const (
	// DefaultMaxAge evicts cache entries not modified within this window.
	DefaultMaxAge = 30 * 24 * time.Hour // 30 days
	// DefaultMaxBytes caps the total size of render cache entries.
	DefaultMaxBytes = int64(1) << 30 // 1 GiB
)

// Entry is a single cached render result.
type Entry struct {
	// Manifests is the raw rendered YAML output.
	Manifests []byte `json:"manifests"`
	// SourceType is the render source type (e.g. "helm", "kustomize").
	SourceType string `json:"sourceType"`
}

// Cache is a persistent, content-addressed store of rendered manifests.
type Cache struct {
	dir    string
	logger *log.Logger
}

// BaseDir returns the base argocdf cache directory: os.UserCacheDir()/argocdf.
// It is the common parent of the render cache and the downloaded-chart cache,
// and the directory removed by `argocdf cache clean`.
func BaseDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("failed to resolve user cache dir: %w", err)
	}
	return filepath.Join(base, "argocdf"), nil
}

// DefaultDir returns the default render cache directory:
// os.UserCacheDir()/argocdf/render.
func DefaultDir() (string, error) {
	base, err := BaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "render"), nil
}

// DirStats walks dir recursively and returns the number of regular files and
// their total size in bytes. A missing directory reports zero, nil.
func DirStats(dir string) (entries int, bytes int64, err error) {
	werr := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		entries++
		bytes += info.Size()
		return nil
	})
	if werr != nil {
		if os.IsNotExist(werr) {
			return 0, 0, nil
		}
		return 0, 0, werr
	}
	return entries, bytes, nil
}

// New creates a Cache rooted at dir, creating the directory if needed.
func New(dir string, logger *log.Logger) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create cache dir %s: %w", dir, err)
	}
	return &Cache{dir: dir, logger: logger}, nil
}

// Dir returns the cache directory.
func (c *Cache) Dir() string {
	return c.dir
}

// entryPath returns the on-disk path for a cache key.
func (c *Cache) entryPath(key string) string {
	return filepath.Join(c.dir, key+".json")
}

// Get returns the cached entry for key. Missing, unreadable, or corrupt entries
// are reported as a miss (ok=false); corrupt entries are best-effort removed.
func (c *Cache) Get(key string) (*Entry, bool) {
	p := c.entryPath(key)
	data, err := os.ReadFile(p)
	if err != nil {
		if !os.IsNotExist(err) && c.logger != nil {
			c.logger.Debug("Render cache read failed", "key", key, "error", err)
		}
		return nil, false
	}

	var e Entry
	if err := json.Unmarshal(data, &e); err != nil {
		if c.logger != nil {
			c.logger.Debug("Render cache entry corrupt, evicting", "key", key, "error", err)
		}
		_ = os.Remove(p)
		return nil, false
	}
	return &e, true
}

// Put stores an entry for key atomically (temp file + rename).
func (c *Cache) Put(key string, e *Entry) error {
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("failed to marshal cache entry: %w", err)
	}

	tmp, err := os.CreateTemp(c.dir, key+".*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp cache file: %w", err)
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("failed to write temp cache file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("failed to close temp cache file: %w", err)
	}

	if err := os.Rename(tmpName, c.entryPath(key)); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("failed to rename cache file into place: %w", err)
	}
	return nil
}

// GC evicts cache entries to bound the cache. It first removes entries whose
// mtime is older than maxAge, then, if the remaining entries still exceed
// maxBytes in total, removes the oldest entries (by mtime) until the total is
// within maxBytes. A non-positive maxAge disables age-based eviction; a
// non-positive maxBytes disables size-based eviction. It returns the number of
// entries removed. GC is best-effort: it only operates on cache entry files
// (*.json) and ignores unrelated files.
func (c *Cache) GC(maxAge time.Duration, maxBytes int64) (removed int, err error) {
	dirEntries, err := os.ReadDir(c.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to read cache dir: %w", err)
	}

	type entry struct {
		path    string
		size    int64
		modTime time.Time
	}

	var entries []entry
	for _, de := range dirEntries {
		if de.IsDir() || filepath.Ext(de.Name()) != ".json" {
			continue
		}
		info, ierr := de.Info()
		if ierr != nil {
			continue
		}
		entries = append(entries, entry{
			path:    filepath.Join(c.dir, de.Name()),
			size:    info.Size(),
			modTime: info.ModTime(),
		})
	}

	// Age-based eviction.
	if maxAge > 0 {
		cutoff := time.Now().Add(-maxAge)
		kept := entries[:0:0]
		for _, e := range entries {
			if e.modTime.Before(cutoff) {
				if rmErr := os.Remove(e.path); rmErr == nil {
					removed++
					continue
				}
			}
			kept = append(kept, e)
		}
		entries = kept
	}

	// Size-based eviction: oldest first until within budget.
	if maxBytes > 0 {
		var total int64
		for _, e := range entries {
			total += e.size
		}
		if total > maxBytes {
			sort.Slice(entries, func(i, j int) bool {
				return entries[i].modTime.Before(entries[j].modTime)
			})
			for _, e := range entries {
				if total <= maxBytes {
					break
				}
				if rmErr := os.Remove(e.path); rmErr == nil {
					removed++
					total -= e.size
				}
			}
		}
	}

	return removed, nil
}
