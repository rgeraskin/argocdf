// Package main provides the CLI entry point for argocdf.
package main

import (
	"context"
	"errors"
	"fmt"
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
	stdoutFormat   string
	fileOutputs    []string
	quiet          bool
	noRecursive    bool
	maxDepth       int
	unifiedContext int
	exitCode       bool
	marker         string

	// Kustomize build options
	kustomizeEnableHelm     bool
	kustomizeBuildOptions   string
	kustomizeLoadRestrictor string

	// Helm options
	helmSkipRefresh bool
	noAPIVersions   bool

	// Render cache options
	noCache  bool
	cacheDir string
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

  # Generate GitHub markdown for PR comment
  argocdf -q -f md-fields:diff.md

  # Generate multiple outputs
  argocdf -f md-fields:pr-comment.md -f html-side-by-side:report.html

  # Generate unified diff output
  argocdf --stdout unified
  argocdf -f unified:changes.patch

  # Use unified diff format inside markdown
  argocdf --file md-unified:diff.md

  # Summary only in terminal
  argocdf --stdout summary

  # Use external diff tool for side-by-side view
  ARGOCDF_EXTERNAL_DIFF="delta --side-by-side" argocdf`,
		RunE: runMain,
		// We map the Run result to a detailed exit code ourselves (see below), so
		// suppress Cobra's default error printing and usage-on-error behavior for
		// runtime errors. The sentinel ErrChangesPresent must stay invisible.
		SilenceErrors: true,
		SilenceUsage:  true,
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

	// Kustomize build options
	rootCmd.Flags().BoolVar(&kustomizeEnableHelm, "kustomize-enable-helm", false,
		"Enable Helm chart inflation via kustomize --enable-helm")
	rootCmd.Flags().StringVar(&kustomizeBuildOptions, "kustomize-build-options", "",
		"Additional kustomize build options (space-separated)")
	rootCmd.Flags().StringVar(&kustomizeLoadRestrictor, "kustomize-load-restrictor", "",
		"Load restrictor mode (e.g., 'LoadRestrictionsNone')")

	// Helm options
	rootCmd.Flags().BoolVar(&helmSkipRefresh, "helm-skip-refresh", true,
		"Skip refreshing repository cache during helm dependency build")
	rootCmd.Flags().BoolVar(&noAPIVersions, "no-api-versions", false,
		"Do not pass cluster-discovered API versions to helm via --api-versions")

	// Output flags
	rootCmd.Flags().StringVar(&stdoutFormat, "stdout", config.DefaultStdoutFormat,
		"Terminal output format: fields, summary, unified, none (set ARGOCDF_EXTERNAL_DIFF for side-by-side)")
	rootCmd.Flags().StringArrayVarP(&fileOutputs, "file", "f", nil,
		"File output in format:path (can be repeated). Formats: md-fields, html-side-by-side, md-unified, unified")
	rootCmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Suppress terminal output (same as --stdout none)")
	rootCmd.Flags().IntVarP(&unifiedContext, "context-lines", "U", config.DefaultUnifiedContext,
		"Number of context lines in unified diff output (-1 for unlimited)")

	// CI flags
	rootCmd.Flags().BoolVar(&exitCode, "exit-code", false,
		"Exit 0 if no changes, 1 on error, 2 if changes are present (like `diff`)")
	rootCmd.Flags().StringVar(&marker, "marker", "",
		"Marker id for the markdown PR-comment upsert marker (default: <!-- argocdf-diff -->)")

	// Render cache flags
	rootCmd.Flags().BoolVar(&noCache, "no-cache", false,
		"Disable the persistent render cache")
	rootCmd.Flags().StringVar(&cacheDir, "cache-dir", "",
		"Render cache directory (default: <user cache dir>/argocdf/render)")

	// Recursion flags
	rootCmd.Flags().BoolVar(&noRecursive, "no-recursive", false, "Disable apps-of-apps recursion")
	rootCmd.Flags().IntVar(&maxDepth, "max-depth", config.DefaultMaxDepth, "Maximum recursion depth")

	// Version command
	rootCmd.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, args []string) {
			cmd.Printf("argocdf version %s\n", Version)
		},
	})

	err := rootCmd.Execute()
	// Print real errors ourselves (Cobra's printing is silenced above), but keep
	// the ErrChangesPresent sentinel invisible: it only carries the exit code.
	if err != nil && !errors.Is(err, app.ErrChangesPresent) {
		fmt.Fprintln(os.Stderr, "Error:", err)
	}
	os.Exit(app.ExitCodeFor(err))
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

	// Handle quiet flag (alias for --stdout none)
	if quiet {
		stdoutFormat = "none"
	}

	// Parse file outputs
	var parsedFileOutputs []config.FileOutput
	for _, spec := range fileOutputs {
		fo, err := config.ParseFileOutput(spec)
		if err != nil {
			return err
		}
		parsedFileOutputs = append(parsedFileOutputs, fo)
	}

	// Build configuration
	cfg := &config.Config{
		KubeconfigPath:          kubeconfigPath,
		Context:                 kubeContext,
		Namespace:               namespace,
		AllNamespaces:           allNamespaces,
		RepoPath:                repoPath,
		RepoURL:                 repoURL,
		BaseBranch:              baseBranch,
		TargetBranch:            targetBranch,
		KubeVersion:             kubeVersion,
		StdoutFormat:            stdoutFormat,
		FileOutputs:             parsedFileOutputs,
		NoRecursive:             noRecursive,
		MaxDepth:                maxDepth,
		UnifiedContext:          unifiedContext,
		KustomizeEnableHelm:     kustomizeEnableHelm,
		KustomizeBuildOptions:   kustomizeBuildOptions,
		KustomizeLoadRestrictor: kustomizeLoadRestrictor,
		HelmSkipRefresh:         helmSkipRefresh,
		NoAPIVersions:           noAPIVersions,
		NoCache:                 noCache,
		CacheDir:                cacheDir,
		ExitCode:                exitCode,
		Marker:                  marker,
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
