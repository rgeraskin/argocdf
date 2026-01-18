// Package config handles configuration for argocdf.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Default values for configuration.
const (
	DefaultContext        = "pp-admin-aws"
	DefaultNamespace      = "argocd"
	DefaultStdoutFormat   = "fields"
	DefaultMaxDepth       = 10
	DefaultUnifiedContext = 3 // Standard unified diff context lines
)

// FileOutput represents a single file output specification.
type FileOutput struct {
	Format string // "md", "html-side-by-side", "md-atlantis"
	Path   string // File path
}

// Config holds all configuration for the argocdf tool.
type Config struct {
	// Kubernetes configuration
	KubeconfigPath string
	Context        string
	Namespace      string
	AllNamespaces  bool

	// Git configuration
	RepoPath     string
	BaseBranch   string
	TargetBranch string
	RepoURL      string

	// Kubernetes version for rendering (auto-detected from cluster if empty)
	KubeVersion string

	// Output configuration (new)
	StdoutFormat string       // "fields", "summary", "unified", "none"
	FileOutputs  []FileOutput // Multiple file outputs

	// Recursion settings
	NoRecursive bool
	MaxDepth    int

	// Unified diff settings
	UnifiedContext int // Number of context lines in unified diff output
}

// ParseFileOutput parses a "format:path" string into a FileOutput.
func ParseFileOutput(spec string) (FileOutput, error) {
	parts := strings.SplitN(spec, ":", 2)
	if len(parts) != 2 {
		return FileOutput{}, fmt.Errorf("invalid file output format: %q (expected format:path)", spec)
	}

	format := parts[0]
	path := parts[1]

	// Validate format
	switch format {
	case "md", "html-side-by-side", "md-atlantis", "unified":
		// Valid formats
	default:
		return FileOutput{}, fmt.Errorf("unknown file format: %q (valid: md, html-side-by-side, md-atlantis, unified)", format)
	}

	if path == "" {
		return FileOutput{}, fmt.Errorf("file path cannot be empty")
	}

	return FileOutput{Format: format, Path: path}, nil
}

// New creates a new Config with default values.
func New() *Config {
	return &Config{
		Context:      DefaultContext,
		Namespace:    DefaultNamespace,
		StdoutFormat: DefaultStdoutFormat,
		MaxDepth:     DefaultMaxDepth,
	}
}

// Validate checks if the configuration is valid and complete.
func (c *Config) Validate() error {
	if c.RepoPath == "" {
		return fmt.Errorf("repository path is required")
	}

	// Verify repo path exists
	info, err := os.Stat(c.RepoPath)
	if err != nil {
		return fmt.Errorf("repository path error: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("repository path is not a directory: %s", c.RepoPath)
	}

	// Verify it's a git repository
	gitDir := filepath.Join(c.RepoPath, ".git")
	if _, err := os.Stat(gitDir); err != nil {
		return fmt.Errorf("not a git repository: %s", c.RepoPath)
	}

	// Validate stdout format
	switch c.StdoutFormat {
	case "fields", "summary", "unified", "none":
		// Valid
	default:
		return fmt.Errorf("invalid stdout format: %s (must be fields, summary, unified, or none)", c.StdoutFormat)
	}

	// Warn if no output configured
	if c.StdoutFormat == "none" && len(c.FileOutputs) == 0 {
		return fmt.Errorf("no output configured: --stdout is 'none' and no --file outputs specified")
	}

	if c.MaxDepth < 1 {
		return fmt.Errorf("max depth must be at least 1")
	}

	return nil
}

// WithDefaults fills in any missing values with sensible defaults.
func (c *Config) WithDefaults() *Config {
	if c.Context == "" {
		c.Context = DefaultContext
	}
	if c.Namespace == "" {
		c.Namespace = DefaultNamespace
	}
	if c.StdoutFormat == "" {
		c.StdoutFormat = DefaultStdoutFormat
	}
	if c.MaxDepth == 0 {
		c.MaxDepth = DefaultMaxDepth
	}
	// Note: UnifiedContext is not defaulted here because 0 is a valid value
	// (meaning no context lines). The default is set by the CLI flag.
	return c
}
