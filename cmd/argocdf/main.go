// Package main provides the CLI entry point for argocdf.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	argocommon "github.com/argoproj/argo-cd/v3/common"
	"github.com/charmbracelet/log"
	"github.com/go-logr/logr"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"k8s.io/klog/v2"

	"github.com/rgeraskin/argocdf/internal/app"
	"github.com/rgeraskin/argocdf/internal/config"
	"github.com/rgeraskin/argocdf/internal/rendercache"
)

// Build metadata. Defaults are overridden at release time by GoReleaser via
// -ldflags "-X main.Version=... -X main.Commit=... -X main.Date=...". For plain
// `go install`/`go build` (no ldflags) they fall back to the embedded build
// info, so the version is never hardcoded in more than one place.
var (
	Version = "dev"
	Commit  = ""
	Date    = ""

	// Config flags
	kubeconfigPath        string
	kubeContext           string
	argocdNamespace       string
	applicationNamespaces []string
	allNamespaces         bool
	repoPath              string
	repoURL               string
	baseBranch            string
	targetBranch          string
	kubeVersion           string
	stdoutFormat          string
	fileOutputs           []string
	quiet                 bool
	verbose               bool
	noRecursive           bool
	maxDepth              int
	concurrency           int
	unifiedContext        int
	exitCode              bool
	marker                string

	// Kustomize build options
	kustomizeEnableHelm     bool
	kustomizeBuildOptions   string
	kustomizeLoadRestrictor string

	// Helm options
	helmSkipRefresh bool
	helmAddRepos    bool
	noAPIVersions   bool

	// Render engine selection
	renderer string

	// Repository credential source
	repoCreds string

	// Render cache options
	noCache  bool
	cacheDir string

	// Lint options
	lintCommands []string
	lintKyverno  []string
	lintConftest []string
	lintTimeout  time.Duration
)

// envPrefix is prepended to every flag name to form its environment variable,
// e.g. --repo-dir is configurable via ARGOCDF_REPO_DIR.
const envPrefix = "ARGOCDF"

// bindEnv wires every flag to an environment variable through viper's
// AutomaticEnv: the flag name is upper-cased, dashes become underscores, and the
// ARGOCDF_ prefix is added (repo-dir -> ARGOCDF_REPO_DIR). Values are applied
// only to flags the user did not pass explicitly, so precedence stays
// flag > environment > default. Each value is routed through pflag's Set so it
// is parsed by the flag's own type (bool, int, string, ...).
func bindEnv(cmd *cobra.Command) error {
	v := viper.New()
	v.SetEnvPrefix(envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	v.AutomaticEnv() // without BindPFlags, IsSet is true only when the env var is actually set

	var bindErr error
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if bindErr != nil || f.Changed || !v.IsSet(f.Name) {
			return // explicit flag wins; skip flags with no env value
		}
		if err := cmd.Flags().Set(f.Name, fmt.Sprintf("%v", v.Get(f.Name))); err != nil {
			env := envPrefix + "_" + strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
			bindErr = fmt.Errorf("invalid value for %s: %w", env, err)
		}
	})
	return bindErr
}

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

Every flag can also be set via an environment variable named ARGOCDF_<FLAG>,
where <FLAG> is the flag name upper-cased with dashes replaced by underscores
(e.g. --repo-dir -> ARGOCDF_REPO_DIR, --kustomize-enable-helm ->
ARGOCDF_KUSTOMIZE_ENABLE_HELM). An explicit flag always overrides its env var.

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

  # Split oversized markdown into PR-comment-sized parts
  # (pr-comment.md, pr-comment.2.md, ... — each fits GitHub's comment cap)
  argocdf -f md-unified,split:pr-comment.md
  argocdf -f md-fields,split=30000:pr-comment.md

  # Summary only in terminal
  argocdf --stdout summary

  # Lint rendered manifests: each stdout line of the command becomes a warning
  argocdf --lint 'conftest test - --policy policy/ --output json 2>/dev/null | jq -rn "input | .[] | .failures[]?.msg"'

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
	rootCmd.Flags().StringVar(&argocdNamespace, "argocd-namespace", config.DefaultNamespace,
		"ArgoCD control-plane namespace: repository secrets and settings are read here, "+
			"and Applications are listed here unless --application-namespaces is set")
	rootCmd.Flags().StringSliceVar(&applicationNamespaces, "application-namespaces", nil,
		"Comma-separated list of namespaces to scan for Applications (exhaustive when set — "+
			"include the ArgoCD namespace explicitly if wanted); entries may be literal names, "+
			"globs like 'team-*', or /regex/")
	rootCmd.Flags().BoolVarP(&allNamespaces, "all-namespaces", "A", false,
		"Scan all namespaces (same as --application-namespaces='*')")

	// Git flags
	rootCmd.Flags().StringVarP(&repoPath, "repo-dir", "r", "", "Path to git repository (default: current directory)")
	rootCmd.Flags().StringVar(&repoURL, "repo-url", "", "Repository URL for matching ArgoCD apps (overrides auto-detected URL)")
	rootCmd.Flags().StringVar(&baseBranch, "base", "", "Base branch for comparison (default: main or master)")
	rootCmd.Flags().StringVar(&targetBranch, "target", "", "Target branch for comparison (default: HEAD)")

	// Rendering flags
	rootCmd.Flags().StringVar(&kubeVersion, "kube-version", "", "Kubernetes version for rendering (auto-detected)")
	rootCmd.Flags().StringVar(&renderer, "renderer", config.DefaultRenderer,
		"Render engine: 'native' (argocdf's own helm/kustomize pipeline) or 'argocd' "+
			"(ArgoCD's repo-server code, for exact ArgoCD render parity)")
	rootCmd.Flags().StringVar(&repoCreds, "repo-creds", config.DefaultRepoCreds,
		"Repository credential source: 'cluster' (ArgoCD repository secrets from --argocd-namespace; "+
			"read failures are fatal), 'local' (your helm config: registry logins and repositories.yaml "+
			"entries), or 'none' (anonymous)")

	// Kustomize build options
	rootCmd.Flags().BoolVar(&kustomizeEnableHelm, "kustomize-enable-helm", false,
		"Enable Helm chart inflation via kustomize --enable-helm")
	rootCmd.Flags().StringVar(&kustomizeBuildOptions, "kustomize-build-options", "",
		"Additional kustomize build options (space-separated)")
	rootCmd.Flags().StringVar(&kustomizeLoadRestrictor, "kustomize-load-restrictor", "",
		"Load restrictor mode (e.g., 'LoadRestrictionsNone')")

	// Helm options
	rootCmd.Flags().BoolVar(&helmSkipRefresh, "helm-skip-refresh", true,
		"Skip refreshing repository cache during helm dependency build (native renderer only)")
	rootCmd.Flags().BoolVar(&helmAddRepos, "helm-add-repos", false,
		"Make chart dependency repos resolvable before dependency build: refresh a matching existing entry, or helm repo add + update unknown URLs; mutates the local helm config/cache, intended for CI (native renderer only)")
	rootCmd.Flags().BoolVar(&noAPIVersions, "no-api-versions", false,
		"Do not pass cluster-discovered API versions to helm via --api-versions")

	// Output flags
	rootCmd.Flags().StringVar(&stdoutFormat, "stdout", config.DefaultStdoutFormat,
		"Terminal output format: fields, summary, unified, none (set ARGOCDF_EXTERNAL_DIFF for side-by-side)")
	rootCmd.Flags().StringArrayVarP(&fileOutputs, "file", "f", nil,
		"File output in format[,option]:path (can be repeated). Formats: md-fields, html-side-by-side, md-unified, unified. "+
			"Markdown formats accept split[=N] to split the report into PR-comment-sized parts (default 60000 bytes)")
	rootCmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Suppress terminal output (same as --stdout none)")
	rootCmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "Enable debug logging")
	rootCmd.Flags().IntVarP(&unifiedContext, "context-lines", "U", config.DefaultUnifiedContext,
		"Number of context lines in unified diff output (-1 for unlimited)")

	// Lint flags
	rootCmd.Flags().StringArrayVar(&lintCommands, "lint", nil,
		"Shell command that lints rendered manifests (can be repeated): receives an app's rendered "+
			"multi-doc YAML on stdin, each stdout line becomes a report warning; exit != 0 is reported "+
			"as a lint execution error")
	rootCmd.Flags().StringArrayVar(&lintKyverno, "lint-kyverno", nil,
		"Lint rendered manifests with the kyverno policies in `DIR` (can be repeated): runs kyverno "+
			"apply against the cluster being diffed and reports its findings, no shell pipeline or jq "+
			"needed. A relative DIR resolves per side, so each side lints with its own version of "+
			"the policies; an absolute DIR points both sides at one tree")
	rootCmd.Flags().StringArrayVar(&lintConftest, "lint-conftest", nil,
		"Lint rendered manifests with the rego policies in `DIR` (can be repeated): runs conftest test "+
			"and reports its findings, no shell pipeline or jq needed. A relative DIR resolves per "+
			"side, so each side lints with its own version of the policies; an absolute DIR points "+
			"both sides at one tree")
	rootCmd.Flags().DurationVar(&lintTimeout, "lint-timeout", config.DefaultLintTimeout,
		"Timeout for each lint invocation (--lint, --lint-kyverno, --lint-conftest)")

	// CI flags
	rootCmd.Flags().BoolVar(&exitCode, "exit-code", false,
		"Exit 0 if no changes, 1 on error, 2 if changes are present (like `diff`)")
	rootCmd.Flags().StringVar(&marker, "marker", "",
		"Marker id for the markdown PR-comment upsert marker (default: <!-- argocdf-diff -->)")

	// Render cache flags
	rootCmd.Flags().BoolVar(&noCache, "no-cache", false,
		"Disable the persistent render cache")
	rootCmd.Flags().StringVar(&cacheDir, "cache-dir", "",
		"Base cache directory for render and chart caches (default: <user cache dir>/argocdf)")

	// Recursion flags
	rootCmd.Flags().BoolVar(&noRecursive, "no-recursive", false, "Disable apps-of-apps recursion")
	rootCmd.Flags().IntVar(&maxDepth, "max-depth", config.DefaultMaxDepth, "Maximum recursion depth")

	// Concurrency flag
	rootCmd.Flags().IntVar(&concurrency, "concurrency", config.DefaultConcurrency(),
		"Number of applications to render in parallel (1 = sequential)")

	// Version command
	rootCmd.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, args []string) {
			cmd.Printf("argocdf version %s\n", versionString())
		},
	})

	// Cache command
	rootCmd.AddCommand(newCacheCmd())

	err := rootCmd.Execute()
	// Print real errors ourselves (Cobra's printing is silenced above), but keep
	// the ErrChangesPresent sentinel invisible: it only carries the exit code.
	if err != nil && !errors.Is(err, app.ErrChangesPresent) {
		fmt.Fprintln(os.Stderr, "Error:", err)
	}
	os.Exit(app.ExitCodeFor(err))
}

// resolveBaseCacheDir returns the base cache directory, honoring --cache-dir.
func resolveBaseCacheDir(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	return rendercache.BaseDir()
}

// newCacheCmd builds the `cache` command group with `clean` and `info`.
func newCacheCmd() *cobra.Command {
	var dirOverride string

	cacheCmd := &cobra.Command{
		Use:   "cache",
		Short: "Inspect and manage the persistent render/chart cache",
	}
	cacheCmd.PersistentFlags().StringVar(&dirOverride, "cache-dir", "",
		"Base cache directory (default: <user cache dir>/argocdf)")

	cacheCmd.AddCommand(&cobra.Command{
		Use:   "info",
		Short: "Show cache location, entry count, and total size",
		RunE: func(cmd *cobra.Command, args []string) error {
			base, err := resolveBaseCacheDir(dirOverride)
			if err != nil {
				return err
			}
			entries, bytes, err := rendercache.DirStats(base)
			if err != nil {
				return err
			}
			cmd.Printf("Cache dir:   %s\n", base)
			cmd.Printf("Entries:     %d\n", entries)
			cmd.Printf("Total size:  %s\n", humanizeBytes(bytes))
			return nil
		},
	})

	cacheCmd.AddCommand(&cobra.Command{
		Use:   "clean",
		Short: "Remove the entire cache directory (render and chart caches)",
		RunE: func(cmd *cobra.Command, args []string) error {
			base, err := resolveBaseCacheDir(dirOverride)
			if err != nil {
				return err
			}
			if err := os.RemoveAll(base); err != nil {
				return err
			}
			cmd.Printf("Removed cache dir %s\n", base)
			return nil
		},
	})

	return cacheCmd
}

// humanizeBytes formats a byte count with a binary unit suffix.
func humanizeBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// logrusForwarder is a logrus hook that re-emits every record through
// argocdf's own logger, so dependency output carries the same timestamped
// format and a source prefix instead of logrus's own formatting.
type logrusForwarder struct {
	logger *log.Logger
}

func (h *logrusForwarder) Levels() []logrus.Level { return logrus.AllLevels }

func (h *logrusForwarder) Fire(e *logrus.Entry) error {
	keys := make([]string, 0, len(e.Data))
	for k := range e.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	args := make([]any, 0, len(keys)*2)
	for _, k := range keys {
		args = append(args, k, e.Data[k])
	}
	switch e.Level {
	case logrus.PanicLevel, logrus.FatalLevel, logrus.ErrorLevel:
		h.logger.Error(e.Message, args...)
	case logrus.WarnLevel:
		h.logger.Warn(e.Message, args...)
	case logrus.InfoLevel:
		h.logger.Info(e.Message, args...)
	default:
		h.logger.Debug(e.Message, args...)
	}
	return nil
}

// klogHandler adapts klog's slog bridge to argocdf's logger. klog encodes
// verbosity as NEGATIVE slog levels (V(n) → level -n), which charm's level
// map doesn't know — unmapped levels would render as INFO, leaking V(8)
// internals like per-request body hexdumps. Drop records below DEBUG (V > 4)
// and clamp the rest of the sub-info range to DEBUG.
type klogHandler struct{ inner slog.Handler }

func (h klogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if level < slog.LevelDebug {
		return false
	}
	if level < slog.LevelInfo {
		level = slog.LevelDebug
	}
	return h.inner.Enabled(ctx, level)
}

func (h klogHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level < slog.LevelInfo {
		r.Level = slog.LevelDebug
	}
	// client-go reports a watch aborted by its caller's own context
	// cancellation at error severity — for argocdf that cancellation is the
	// deliberate informer shutdown after credential loading (repocreds.go),
	// and whether it fires mid-request is a timing race. Demote that expected
	// case so ERRO stays reserved for real watch failures (RBAC, network).
	if r.Level >= slog.LevelError && recordErrIsContextCanceled(r) {
		r.Level = slog.LevelDebug
	}
	return h.inner.Handle(ctx, r)
}

// recordErrIsContextCanceled reports whether the record's "err" attribute
// carries a context.Canceled, in any wrapping. The fallback for values that
// aren't errors.Is-able (pre-stringified attributes, %v-wrapped chains)
// requires "context canceled" as the TERMINAL cause — the suffix position
// where Go's error-chain rendering puts it — so an error that merely mentions
// the text mid-sentence is never demoted.
func recordErrIsContextCanceled(r slog.Record) bool {
	canceled := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key != "err" {
			return true
		}
		if err, ok := a.Value.Any().(error); ok && errors.Is(err, context.Canceled) {
			canceled = true
		} else if strings.HasSuffix(strings.TrimSpace(fmt.Sprintf("%v", a.Value.Any())), context.Canceled.Error()) {
			canceled = true
		}
		return !canceled
	})
	return canceled
}

func (h klogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return klogHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h klogHandler) WithGroup(name string) slog.Handler {
	return klogHandler{inner: h.inner.WithGroup(name)}
}

// configureDependencyLogging routes the process-global log channels of
// argocdf's libraries through argocdf's own logger, quieted unless --verbose.
// Three independent channels:
//   - ArgoCD logs through the global logrus logger (settings informers,
//     GenerateManifests); argocdf itself does not use logrus. Records are
//     re-emitted via our logger under the "argocd" prefix — errors only,
//     unless --verbose keeps the full stream as a debug channel.
//   - ArgoCD's exec tracer (util/exec) builds a FRESH logrus logger per
//     command from ARGOCD_LOG_LEVEL/ARGOCD_LOG_FORMAT, unreachable by the
//     hook above. BOTH engines reach it — the native engine through ArgoCD's
//     chart client (chartfetch.go). Default it to text (JSON otherwise) and,
//     without --verbose, to errors-only; explicit values are respected either
//     way, so ARGOCD_LOG_LEVEL=info stays available as a render debug channel
//     even in quiet runs.
//   - client-go logs through klog — e.g. the reflector reporting the EXPECTED
//     watch cancellation when ArgoCD's settings informers shut down after
//     credential loading. Real API failures surface as returned errors, so
//     klog output is dropped entirely unless --verbose, where it rides our
//     logger under the "client-go" prefix via klog's slog bridge.
func configureDependencyLogging(logger *log.Logger, debug bool) {
	logrus.SetOutput(io.Discard)
	// Replace, don't stack: a second configuration (tests, library-style
	// reuse) must not leave two forwarders duplicating every argocd line.
	logrus.StandardLogger().ReplaceHooks(make(logrus.LevelHooks))
	logrus.AddHook(&logrusForwarder{logger: logger.WithPrefix("argocd")})
	if !debug {
		logrus.SetLevel(logrus.ErrorLevel)
	}

	if os.Getenv(argocommon.EnvLogFormat) == "" {
		_ = os.Setenv(argocommon.EnvLogFormat, "text")
	}
	if !debug && os.Getenv(argocommon.EnvLogLevel) == "" {
		_ = os.Setenv(argocommon.EnvLogLevel, "error")
	}

	if debug {
		klog.SetSlogLogger(slog.New(klogHandler{inner: logger.WithPrefix("client-go")}))
	} else {
		klog.SetLogger(logr.Discard())
	}
}

func runMain(cmd *cobra.Command, args []string) error {
	// Seed flags from ARGOCDF_* environment variables before anything reads them,
	// so env values behave exactly like flags for the rest of the run.
	if err := bindEnv(cmd); err != nil {
		return err
	}

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
	logLevel := log.InfoLevel
	if verbose {
		logLevel = log.DebugLevel
	}
	logger := log.NewWithOptions(os.Stderr, log.Options{
		ReportTimestamp: true,
		Level:           logLevel,
	})

	configureDependencyLogging(logger, logLevel == log.DebugLevel)

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
		ArgoCDNamespace:         argocdNamespace,
		ApplicationNamespaces:   applicationNamespaces,
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
		Concurrency:             concurrency,
		UnifiedContext:          unifiedContext,
		KustomizeEnableHelm:     kustomizeEnableHelm,
		KustomizeBuildOptions:   kustomizeBuildOptions,
		KustomizeLoadRestrictor: kustomizeLoadRestrictor,
		HelmSkipRefresh:         helmSkipRefresh,
		HelmAddRepos:            helmAddRepos,
		NoAPIVersions:           noAPIVersions,
		Renderer:                renderer,
		RepoCreds:               repoCreds,
		NoCache:                 noCache,
		CacheDir:                cacheDir,
		ExitCode:                exitCode,
		Marker:                  marker,
		Lint:                    lintCommands,
		LintKyverno:             lintKyverno,
		LintConftest:            lintConftest,
		LintTimeout:             lintTimeout,
		Version:                 bareVersion(),
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

	// Explicitly set native-only helm flags do nothing under the argocd
	// renderer; say so instead of silently ignoring them. bindEnv marks
	// env-provided flags as Changed, so env values are covered too.
	for _, warning := range cfg.NativeOnlyFlagWarnings(
		cmd.Flags().Changed("helm-skip-refresh"),
		cmd.Flags().Changed("helm-add-repos"),
	) {
		logger.Warn(warning)
	}

	logger.Info("Using repository URL", "repoURL", cfg.RepoURL)

	logger.Debug("Configuration",
		"repo", cfg.RepoPath,
		"repoURL", cfg.RepoURL,
		"base", cfg.BaseBranch,
		"target", cfg.TargetBranch,
		"context", cfg.Context,
		"argocdNamespace", cfg.ArgoCDNamespace,
		"applicationNamespaces", cfg.ApplicationNamespaces,
	)

	// Create and run the app
	application, err := app.New(cfg, logger)
	if err != nil {
		return err
	}

	return application.Run(ctx)
}
