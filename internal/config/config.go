// Package config handles configuration for argocdf.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Default values for configuration.
const (
	// DefaultContext is empty to use kubectl's current context by default.
	// This is more portable than hardcoding a specific context name.
	DefaultContext        = ""
	DefaultNamespace      = "argocd"
	DefaultStdoutFormat   = "fields"
	DefaultMaxDepth       = 10
	DefaultUnifiedContext = 3 // Standard unified diff context lines

	// DefaultKubeVersionFallback is the Kubernetes version used for Helm rendering
	// when the cluster version cannot be detected.
	DefaultKubeVersionFallback = "1.29.0"
)

// DefaultConcurrency returns the default number of applications to render in
// parallel: min(4, runtime.NumCPU()), and never less than 1.
func DefaultConcurrency() int {
	n := runtime.NumCPU()
	if n > 4 {
		return 4
	}
	if n < 1 {
		return 1
	}
	return n
}

// FileOutput represents a single file output specification.
type FileOutput struct {
	Format string // "md-fields", "html-side-by-side", "md-unified", "unified"
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

	// Concurrency is the number of applications rendered in parallel.
	// 1 means sequential processing.
	Concurrency int

	// Unified diff settings
	UnifiedContext int // Number of context lines in unified diff output

	// Kustomize build options (CLI defaults)
	KustomizeEnableHelm     bool
	KustomizeBuildOptions   string
	KustomizeLoadRestrictor string

	// Helm options
	HelmSkipRefresh bool

	// Render cache options
	NoCache  bool   // Disable the persistent render cache
	CacheDir string // Cache directory (empty = os.UserCacheDir()/argocdf/render)
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
	case "md-fields", "html-side-by-side", "md-unified", "unified":
		// Valid formats
	default:
		return FileOutput{}, fmt.Errorf("unknown file format: %q (valid: md-fields, html-side-by-side, md-unified, unified)", format)
	}

	if path == "" {
		return FileOutput{}, fmt.Errorf("file path cannot be empty")
	}

	return FileOutput{Format: format, Path: path}, nil
}

// New creates a new Config with default values.
func New() *Config {
	return &Config{
		// Context is left empty to use kubectl's current context
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

	if c.Concurrency < 1 {
		return fmt.Errorf("concurrency must be at least 1")
	}

	return nil
}

// WithDefaults fills in any missing values with sensible defaults.
func (c *Config) WithDefaults() *Config {
	// Note: Context is intentionally not defaulted here.
	// Empty context means kubectl will use the current context from kubeconfig.
	if c.Namespace == "" {
		c.Namespace = DefaultNamespace
	}
	if c.StdoutFormat == "" {
		c.StdoutFormat = DefaultStdoutFormat
	}
	if c.MaxDepth == 0 {
		c.MaxDepth = DefaultMaxDepth
	}
	if c.Concurrency == 0 {
		c.Concurrency = DefaultConcurrency()
	}
	// Note: UnifiedContext is not defaulted here because 0 is a valid value
	// (meaning no context lines). The default is set by the CLI flag.
	return c
}
