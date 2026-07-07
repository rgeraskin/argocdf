// Package config handles configuration for argocdf.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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

	// DefaultSplitMax is the part size (in bytes) used by the `split` file-output
	// option when no explicit value is given. It leaves headroom under GitHub's
	// 65,536-char comment cap for CI-appended footers.
	DefaultSplitMax = 60000

	// MinSplitMax is the smallest accepted `split` value; below this the per-part
	// marker/heading overhead would dominate the budget.
	MinSplitMax = 1024
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
	Format   string // "md-fields", "html-side-by-side", "md-unified", "unified"
	Path     string // File path
	SplitMax int    // Max part size in bytes for markdown outputs (0 = no splitting)
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

	// NoAPIVersions disables passing cluster-discovered API versions to helm
	// via --api-versions (faster; useful for compatibility).
	NoAPIVersions bool

	// Render cache options
	NoCache bool // Disable the persistent render cache
	// CacheDir is the base cache directory holding the render cache (render/)
	// and the downloaded-chart cache (charts/). Empty = os.UserCacheDir()/argocdf.
	CacheDir string

	// CI options
	ExitCode bool   // Exit 2 when changes are present (like `diff`/`terraform plan -detailed-exitcode`)
	Marker   string // Optional marker id for the PR-comment upsert marker (empty = default marker)
}

// ParseFileOutput parses a "format[,option...]:path" string into a FileOutput.
// Options ride on the format segment (before the first colon), so paths
// containing commas or colons stay intact. Supported options:
//
//	split[=N]  split markdown output into parts of at most N bytes
//	           (default 60000); only valid for md-fields and md-unified
func ParseFileOutput(spec string) (FileOutput, error) {
	parts := strings.SplitN(spec, ":", 2)
	if len(parts) != 2 {
		return FileOutput{}, fmt.Errorf("invalid file output format: %q (expected format[,option]:path)", spec)
	}

	segments := strings.Split(parts[0], ",")
	format := segments[0]
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

	fo := FileOutput{Format: format, Path: path}

	// Parse options following the format
	for _, opt := range segments[1:] {
		key, value, hasValue := strings.Cut(opt, "=")
		switch key {
		case "split":
			if format != "md-fields" && format != "md-unified" {
				return FileOutput{}, fmt.Errorf("option %q is only valid for md-fields and md-unified outputs, not %q", key, format)
			}
			fo.SplitMax = DefaultSplitMax
			if hasValue {
				n, err := strconv.Atoi(value)
				if err != nil {
					return FileOutput{}, fmt.Errorf("invalid split value %q: expected an integer number of bytes", value)
				}
				if n < MinSplitMax {
					return FileOutput{}, fmt.Errorf("split value %d is too small (minimum %d bytes)", n, MinSplitMax)
				}
				fo.SplitMax = n
			}
		default:
			return FileOutput{}, fmt.Errorf("unknown file output option: %q (valid: split[=N])", opt)
		}
	}

	return fo, nil
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
