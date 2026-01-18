// Package main provides the CLI entry point for argocdf.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/charmbracelet/log"
	"github.com/spf13/cobra"

	"github.com/rgeraskin/argocdf/internal/app"
	"github.com/rgeraskin/argocdf/internal/config"
)

var (
	// Version is set at build time
	Version = "dev"

	// Config flags
	kubeconfigPath string
	kubeContext    string
	namespace      string
	allNamespaces  bool
	repoPath       string
	repoURL        string
	baseBranch     string
	targetBranch   string
	kubeVersion    string
	outputFormat   string
	htmlFile       string
	noRecursive  bool
	maxDepth     int
	sideBySide   bool
	summaryOnly  bool
	githubCompat bool
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "argocdf",
		Short: "Show diffs for ArgoCD applications affected by PR changes",
		Long: `argocdf analyzes your git repository and ArgoCD cluster to show
what manifest changes will occur when your PR is merged.

It supports:
- Single and multi-source ArgoCD applications
- Helm charts (local and remote)
- Kustomize directories
- Apps-of-apps pattern with recursive discovery

Examples:
  # Basic usage (auto-detects repository and branches)
  argocdf

  # Specify branches explicitly
  argocdf --base main --target feature/new-service

  # Use a different Kubernetes context
  argocdf --context my-cluster

  # Scan all namespaces for ArgoCD applications
  argocdf --all-namespaces

  # Override repo URL (useful when SSH config has custom hostname aliases)
  argocdf --repo-dir /path/to/repo --repo-url https://github.com/org/repo

  # Generate HTML report
  argocdf --output both --html-file report.html`,
		RunE: runMain,
	}

	// Kubernetes flags
	rootCmd.Flags().StringVarP(&kubeconfigPath, "kubeconfig", "k", "", "Path to kubeconfig file")
	rootCmd.Flags().StringVar(&kubeContext, "context", config.DefaultContext, "Kubernetes context to use")
	rootCmd.Flags().StringVarP(&namespace, "namespace", "n", config.DefaultNamespace, "ArgoCD namespace to search")
	rootCmd.Flags().BoolVarP(&allNamespaces, "all-namespaces", "A", false, "Search all namespaces")

	// Git flags
	rootCmd.Flags().StringVarP(&repoPath, "repo-dir", "r", "", "Path to git repository (default: current directory)")
	rootCmd.Flags().StringVar(&repoURL, "repo-url", "", "Repository URL for matching ArgoCD apps (overrides auto-detected URL)")
	rootCmd.Flags().StringVar(&baseBranch, "base", "", "Base branch for comparison (default: main or master)")
	rootCmd.Flags().StringVar(&targetBranch, "target", "", "Target branch for comparison (default: HEAD)")

	// Rendering flags
	rootCmd.Flags().StringVar(&kubeVersion, "kube-version", "", "Kubernetes version for rendering (auto-detected)")

	// Output flags
	rootCmd.Flags().StringVarP(&outputFormat, "output", "o", "terminal", "Output format: terminal, html, or both")
	rootCmd.Flags().StringVar(&htmlFile, "html-file", config.DefaultHTMLFile, "HTML output file path")

	// Recursion flags
	rootCmd.Flags().BoolVar(&noRecursive, "no-recursive", false, "Disable apps-of-apps recursion")
	rootCmd.Flags().IntVar(&maxDepth, "max-depth", config.DefaultMaxDepth, "Maximum recursion depth")

	// Output detail flags
	rootCmd.Flags().BoolVar(&sideBySide, "side-by-side", false, "Show side-by-side YAML diff (uses KUBECTL_EXTERNAL_DIFF for terminal)")
	rootCmd.Flags().BoolVar(&summaryOnly, "summary-only", false, "Show only affected apps without detailed diff")
	rootCmd.Flags().BoolVar(&githubCompat, "github", false, "Output GitHub-compatible HTML (pasteable to PR comments)")

	// Version command
	rootCmd.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, args []string) {
			cmd.Printf("argocdf version %s\n", Version)
		},
	})

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func runMain(cmd *cobra.Command, args []string) error {
	// Setup context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		cancel()
	}()

	// Setup logger
	logger := log.NewWithOptions(os.Stderr, log.Options{
		ReportTimestamp: true,
		Level:           log.InfoLevel,
	})

	// Build configuration
	cfg := &config.Config{
		KubeconfigPath: kubeconfigPath,
		Context:        kubeContext,
		Namespace:      namespace,
		AllNamespaces:  allNamespaces,
		RepoPath:       repoPath,
		RepoURL:        repoURL,
		BaseBranch:     baseBranch,
		TargetBranch:   targetBranch,
		KubeVersion:    kubeVersion,
		OutputFormat:   outputFormat,
		HTMLFilePath:   htmlFile,
		NoRecursive:  noRecursive,
		MaxDepth:     maxDepth,
		SideBySide:   sideBySide,
		SummaryOnly:  summaryOnly,
		GitHubCompat: githubCompat,
	}

	// Auto-detect missing values
	logger.Debug("Auto-detecting configuration...")
	if err := config.AutoDetect(cfg); err != nil {
		return err
	}

	// Apply defaults
	cfg.WithDefaults()

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		return err
	}

	logger.Debug("Configuration",
		"repo", cfg.RepoPath,
		"repoURL", cfg.RepoURL,
		"base", cfg.BaseBranch,
		"target", cfg.TargetBranch,
		"context", cfg.Context,
		"namespace", cfg.Namespace,
	)

	// Create and run the app
	application, err := app.New(cfg, logger)
	if err != nil {
		return err
	}

	return application.Run(ctx)
}
