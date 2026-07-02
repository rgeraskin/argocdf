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
	"os"
	"path/filepath"

	"github.com/charmbracelet/log"
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

// DefaultDir returns the default cache directory: os.UserCacheDir()/argocdf/render.
func DefaultDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("failed to resolve user cache dir: %w", err)
	}
	return filepath.Join(base, "argocdf", "render"), nil
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
