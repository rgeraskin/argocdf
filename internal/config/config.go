// Package config handles configuration for argocdf.
package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// Default values for configuration.
const (
	DefaultContext      = "pp-admin-aws"
	DefaultNamespace    = "argocd"
	DefaultOutputFormat = "terminal"
	DefaultHTMLFile     = "argocdf-report.html"
	DefaultMaxDepth     = 10
)

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

	// Output configuration
	OutputFormat string // "terminal", "html", or "both"
	HTMLFilePath string

	// Recursion settings
	NoRecursive bool
	MaxDepth    int

	// Output detail level
	SideBySide  bool // Show side-by-side diff (uses external diff tool for terminal, diff2html for HTML)
	SummaryOnly bool // Show only affected apps without detailed diff
	GitHubCompat bool // Output GitHub-compatible HTML (no document wrapper, pasteable to comments)
}

// New creates a new Config with default values.
func New() *Config {
	return &Config{
		Context:      DefaultContext,
		Namespace:    DefaultNamespace,
		OutputFormat: DefaultOutputFormat,
		HTMLFilePath: DefaultHTMLFile,
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

	// Validate output format
	switch c.OutputFormat {
	case "terminal", "html", "both":
		// Valid
	default:
		return fmt.Errorf("invalid output format: %s (must be terminal, html, or both)", c.OutputFormat)
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
	if c.OutputFormat == "" {
		c.OutputFormat = DefaultOutputFormat
	}
	if c.HTMLFilePath == "" {
		c.HTMLFilePath = DefaultHTMLFile
	}
	if c.MaxDepth == 0 {
		c.MaxDepth = DefaultMaxDepth
	}
	return c
}
