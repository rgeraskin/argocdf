// Package app provides the main application orchestrator.
package app

import (
	"path/filepath"

	"github.com/charmbracelet/log"

	"github.com/rgeraskin/argocdf/internal/cluster"
	"github.com/rgeraskin/argocdf/internal/config"
	"github.com/rgeraskin/argocdf/internal/diff"
	"github.com/rgeraskin/argocdf/internal/git"
	"github.com/rgeraskin/argocdf/internal/lint"
	"github.com/rgeraskin/argocdf/internal/output"
	"github.com/rgeraskin/argocdf/internal/render"
	"github.com/rgeraskin/argocdf/internal/rendercache"
)

// Factory creates and configures all dependencies.
type Factory struct {
	config *config.Config
	logger *log.Logger
}

// NewFactory creates a new Factory with the given configuration.
func NewFactory(cfg *config.Config, logger *log.Logger) *Factory {
	return &Factory{
		config: cfg,
		logger: logger,
	}
}

// CreateClusterClient creates a Kubernetes cluster client.
func (f *Factory) CreateClusterClient() (*cluster.Client, error) {
	return cluster.NewClient(f.config.KubeconfigPath, f.config.Context)
}

// CreateAppService creates an ArgoCD application service.
func (f *Factory) CreateAppService(client *cluster.Client) *cluster.ApplicationService {
	return cluster.NewApplicationService(client)
}

// CreateRepository opens the git repository.
func (f *Factory) CreateRepository() (*git.Repository, error) {
	return git.Open(f.config.RepoPath)
}

// CreateRenderFactory creates the render engine selected by --renderer:
// argocdf's native helm/kustomize pipeline, or ArgoCD's own repo-server code
// for exact render parity. apiVersions is the list of cluster API versions to
// pass to helm; it is ignored when --no-api-versions is set. creds carries
// the repository credentials loaded per --repo-creds (nil in `none` mode, or
// when loading was skipped).
func (f *Factory) CreateRenderFactory(kubeVersion string, apiVersions []string, creds *cluster.RepoCredentials) (applicationRenderer, error) {
	if f.config.NoAPIVersions {
		apiVersions = nil
	}
	opts := render.RenderOptions{
		RepoPath:                f.config.RepoPath,
		RepoURL:                 f.config.RepoURL,
		ArgoCDNamespace:         f.config.ArgoCDNamespace,
		KubeVersion:             kubeVersion,
		APIVersions:             apiVersions,
		KustomizeEnableHelm:     f.config.KustomizeEnableHelm,
		KustomizeBuildOptions:   f.config.KustomizeBuildOptions,
		KustomizeLoadRestrictor: f.config.KustomizeLoadRestrictor,
		HelmSkipRefresh:         f.config.HelmSkipRefresh,
		HelmAddRepos:            f.config.HelmAddRepos,
		ChartCacheDir:           f.chartCacheDir(),
	}
	if creds != nil {
		opts.HelmRepos = creds.HelmRepos
		opts.OCIRepos = creds.OCIRepos
		opts.HelmRepoCreds = creds.HelmRepoCreds
		opts.OCIRepoCreds = creds.OCIRepoCreds
		opts.ResolveRepo = creds.Resolve
		opts.HelmRegistryConfig = creds.HelmRegistryConfig
	}
	if f.config.Renderer == config.RendererArgoCD {
		// Explicit nil check instead of returning the call directly: that
		// would convert a typed-nil *ArgoCDRenderer into a NON-nil
		// applicationRenderer on error, and Run's deferred cleanup would then
		// call Cleanup on a nil receiver and panic during error unwinding.
		r, err := render.NewArgoCDRenderer(opts)
		if err != nil {
			return nil, err
		}
		return r, nil
	}
	return render.NewFactory(opts), nil
}

// baseCacheDir resolves the base argocdf cache directory: the explicit
// --cache-dir when set, otherwise <user cache dir>/argocdf.
func (f *Factory) baseCacheDir() (string, error) {
	if f.config.CacheDir != "" {
		return f.config.CacheDir, nil
	}
	return rendercache.BaseDir()
}

// chartCacheDir returns the directory for the remote chart download cache, or
// "" when caching is disabled (--no-cache) or the base dir cannot be resolved.
func (f *Factory) chartCacheDir() string {
	if f.config.NoCache {
		return ""
	}
	base, err := f.baseCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(base, "charts")
}

// CreateRenderCache creates the persistent render cache, or returns nil when
// caching is disabled via --no-cache. When the cache directory cannot be
// prepared it returns an error; callers degrade to normal rendering. Best-effort
// garbage collection runs inline at creation to bound the cache by age and size.
func (f *Factory) CreateRenderCache() (*rendercache.Cache, error) {
	if f.config.NoCache {
		return nil, nil
	}

	base, err := f.baseCacheDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(base, "render")

	cache, err := rendercache.New(dir, f.logger)
	if err != nil {
		return nil, err
	}

	// Bound the cache best-effort; failures here must not block rendering.
	if removed, gcErr := cache.GC(rendercache.DefaultMaxAge, rendercache.DefaultMaxBytes); gcErr != nil {
		if f.logger != nil {
			f.logger.Debug("Render cache GC failed", "error", gcErr)
		}
	} else if removed > 0 && f.logger != nil {
		f.logger.Debug("Render cache GC evicted entries", "removed", removed)
	}

	return cache, nil
}

// CreateManifestDiffer creates a manifest differ.
func (f *Factory) CreateManifestDiffer() *diff.ManifestDiffer {
	return diff.NewManifestDiffer()
}

// Environment variables exported to --lint commands: the EFFECTIVE values of
// argocdf's two cluster-selecting flags, under the names argocdf's
// ARGOCDF_<FLAG> convention already assigns them. A cluster-aware adapter
// (`kyverno apply --cluster`, `kubectl --dry-run=server`) can then target the
// cluster argocdf is diffing instead of whatever the invoking shell points at.
const (
	envLintContext    = "ARGOCDF_CONTEXT"
	envLintKubeconfig = "ARGOCDF_KUBECONFIG"
)

// CreateLintRunner creates the rendered-manifest lint runner, or nil when no
// --lint commands are configured. kubeContext is the RESOLVED context name
// (see cluster.Client.ResolvedContext), so adapters need no fallback logic;
// when it is unknown nothing is exported for that variable — never an empty
// value, which an adapter could not distinguish from a real one.
func (f *Factory) CreateLintRunner(kubeContext string) *lint.Runner {
	if len(f.config.Lint) == 0 {
		return nil
	}

	env := make(map[string]string, 2)
	if kubeContext != "" {
		env[envLintContext] = kubeContext
	}
	// Passed verbatim: KubeconfigPath may be an os.PathListSeparator-joined
	// list, exactly as KUBECONFIG carried it.
	if f.config.KubeconfigPath != "" {
		env[envLintKubeconfig] = f.config.KubeconfigPath
	}

	return &lint.Runner{
		Commands: f.config.Lint,
		Timeout:  f.config.LintTimeout,
		Env:      env,
	}
}

// CreateAppDiscoverer creates an application discoverer.
func (f *Factory) CreateAppDiscoverer() *diff.AppDiscoverer {
	return diff.NewAppDiscoverer()
}

// CreateAppQueue creates an application processing queue.
func (f *Factory) CreateAppQueue() *diff.AppDiffQueue {
	return diff.NewAppDiffQueue(f.config.MaxDepth)
}

// CreateOutputWriter creates the appropriate output writer(s).
func (f *Factory) CreateOutputWriter() (output.Writer, error) {
	var writers []output.Writer

	// Terminal output (unless "none")
	if f.config.StdoutFormat != "none" {
		writers = append(writers, output.NewTerminalWriter(f.config.StdoutFormat, f.config.UnifiedContext))
	}

	// File outputs
	for _, fo := range f.config.FileOutputs {
		switch fo.Format {
		case "md-fields":
			mdWriter, err := output.NewMarkdownWriter(fo.Path, output.MarkdownFormatGitHub, 0)
			if err != nil {
				return nil, err
			}
			mdWriter.SetMarker(f.config.Marker)
			mdWriter.SetSplitMax(fo.SplitMax)
			writers = append(writers, mdWriter)

		case "md-unified":
			mdWriter, err := output.NewMarkdownWriter(fo.Path, output.MarkdownFormatAtlantis, f.config.UnifiedContext)
			if err != nil {
				return nil, err
			}
			mdWriter.SetMarker(f.config.Marker)
			mdWriter.SetSplitMax(fo.SplitMax)
			writers = append(writers, mdWriter)

		case "html-side-by-side":
			htmlWriter, err := output.NewHTMLWriter(fo.Path, true, false, false)
			if err != nil {
				return nil, err
			}
			writers = append(writers, htmlWriter)

		case "unified":
			unifiedWriter, err := output.NewUnifiedWriter(fo.Path, f.config.UnifiedContext)
			if err != nil {
				return nil, err
			}
			writers = append(writers, unifiedWriter)
		}
	}

	// Handle no outputs (shouldn't happen due to validation, but be safe)
	if len(writers) == 0 {
		return output.NewNullWriter(), nil
	}

	if len(writers) == 1 {
		return writers[0], nil
	}

	return output.NewMultiWriter(writers...), nil
}

// Config returns the configuration.
func (f *Factory) Config() *config.Config {
	return f.config
}

// Logger returns the logger.
func (f *Factory) Logger() *log.Logger {
	return f.logger
}
