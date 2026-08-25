// Package app provides the main application orchestrator.
package app

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"

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

// CreateRenderer creates the render engine (ArgoCD's own repo-server code,
// for exact render parity). apiVersions is the list of cluster API versions to
// pass to helm; it is ignored when --no-api-versions is set. creds carries
// the repository credentials loaded per --repo-creds (nil in `none` mode, or
// when loading was skipped). credsInstance identifies which INSTANCE of that
// source the run reads (see credentialInstance); it scopes the chart cache one
// level below the mode.
func (f *Factory) CreateRenderer(kubeVersion string, apiVersions []string, creds *cluster.RepoCredentials, credsInstance string) (applicationRenderer, error) {
	if f.config.NoAPIVersions {
		apiVersions = nil
	}
	// Bound the downloaded-chart cache before anything renders into it, under
	// the SAME limits as the render cache (see CreateRenderCache): one pair of
	// bounds governs the whole of <cache dir>, so a user reasons about one age
	// and one size rather than two.
	f.gcChartCache()
	opts := render.RenderOptions{
		RepoPath:                f.config.RepoPath,
		RepoURL:                 f.config.RepoURL,
		ArgoCDNamespace:         f.config.ArgoCDNamespace,
		KubeVersion:             kubeVersion,
		APIVersions:             apiVersions,
		KustomizeEnableHelm:     f.config.KustomizeEnableHelm,
		KustomizeBuildOptions:   f.config.KustomizeBuildOptions,
		KustomizeLoadRestrictor: f.config.KustomizeLoadRestrictor,
		ChartCacheDir:           f.chartCacheDir(credsInstance),
		Logger:                  f.logger,
	}
	if creds != nil {
		opts.HelmRepos = creds.HelmRepos
		opts.OCIRepos = creds.OCIRepos
		opts.HelmRepoCreds = creds.HelmRepoCreds
		opts.OCIRepoCreds = creds.OCIRepoCreds
		opts.ResolveRepo = creds.Resolve
		opts.HelmRegistryConfig = creds.HelmRegistryConfig
	}
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

// baseCacheDir resolves the base argocdf cache directory: the explicit
// --cache-dir when set, otherwise <user cache dir>/argocdf.
func (f *Factory) baseCacheDir() (string, error) {
	if f.config.CacheDir != "" {
		return f.config.CacheDir, nil
	}
	return rendercache.BaseDir()
}

// chartCacheRoot returns the ROOT of the remote chart download cache
// (<cache dir>/charts) — the common parent of every credential-source scope,
// including the legacy unscoped ones — or "" when the chart cache is disabled
// (--no-cache=all|charts) or the base dir cannot be resolved. It is what the
// chart GC sweeps; chartCacheDir picks the scope inside it that this run writes.
func (f *Factory) chartCacheRoot() string {
	if !f.config.ChartCacheEnabled() {
		return ""
	}
	base, err := f.baseCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(base, "charts")
}

// gcChartCache bounds the chart cache best-effort, with the SAME age and size
// limits the render cache uses (rendercache.DefaultMaxAge/DefaultMaxBytes —
// one pair of bounds for both halves of the cache directory). Failures must
// never block rendering, so they are only reported at DEBUG, exactly as
// CreateRenderCache reports the render cache's own GC.
func (f *Factory) gcChartCache() {
	root := f.chartCacheRoot()
	if root == "" {
		return
	}
	removed, err := render.GCChartCache(root, rendercache.DefaultMaxAge, rendercache.DefaultMaxBytes)
	if err != nil {
		if f.logger != nil {
			f.logger.Debug("Chart cache GC failed", "error", err)
		}
		return
	}
	if removed > 0 && f.logger != nil {
		f.logger.Debug("Chart cache GC evicted entries", "removed", removed)
	}
}

// chartCacheDir returns the directory for the remote chart download cache, or
// "" when the chart cache is disabled (--no-cache=all|charts) or the base dir
// cannot be resolved.
//
// The directory is SCOPED BY CREDENTIAL SOURCE, and inside `cluster` mode by the
// source INSTANCE (cluster API server + ArgoCD namespace). A chart already downloaded
// under one --repo-creds source must not satisfy a run whose whole point is checking
// that another source can fetch it: render locally with `local`, re-run with
// `cluster` to see whether ArgoCD's own credentials work, and a shared chart
// directory would serve the chart without contacting the registry - reporting
// success for a registry ArgoCD cannot reach. The instance level exists because the
// same question recurs one step in: two clusters both read with `cluster` are two
// credential sources, and verifying cluster B must not be answered by cluster A's
// download. `local` and `none` have no instance dimension (the helm config is
// machine-global; `none` has nothing to instantiate). The render cache keys on mode
// and instance for the same reason; without this, its miss would still be followed
// by a fetch that never happened.
//
// The cost is a re-download per source instance (and a copy on disk each) for
// charts that are byte-identical - the price of the verification being real.
//
// It IS bounded by garbage collection, under the same age and size limits as the
// render cache: render.GCChartCache runs over the whole charts/ tree from
// gcChartCache above, before anything renders. It finds entries by SHAPE - any
// directory named like a chartCacheKey, at any depth - so the per-source split
// cannot outrun it, and neither can the layouts that predate the split: entries
// written before these scopes existed sit at charts/<sha>/ (and
// charts/cluster/<sha>/), are never read again, and are now swept by the same
// pass rather than accumulating forever. `argocdf cache clean` remains the way
// to remove everything at once; the GC only reclaims what has aged out or what
// overflows the size budget.
func (f *Factory) chartCacheDir(credsInstance string) string {
	root := f.chartCacheRoot()
	if root == "" {
		return ""
	}
	mode := f.config.RepoCreds
	if mode == "" {
		mode = config.DefaultRepoCreds
	}
	dir := filepath.Join(root, mode)
	if credsInstance != "" {
		dir = filepath.Join(dir, instanceSegment(credsInstance))
	}

	return dir
}

// credentialInstance identifies which instance of a credential source a run
// reads: for `cluster` mode the cluster API SERVER plus the ArgoCD namespace
// (the two coordinates that select the repository Secrets), empty for `local`
// and `none`. The server, not the context name: a context name is an alias
// local to one kubeconfig file - two files can both define "prod" pointing at
// different clusters, and one file can repoint a name at a new cluster (kind
// recreations do) - so keying on the name both merged distinct clusters and
// split one cluster reached through two names. The server endpoint has
// neither problem and is the identity ArgoCD itself keys cluster secrets by.
//
// Residual, deliberately not chased: a recreated cluster behind a PINNED
// endpoint (fixed port, same DNS) reuses the scope - the same limitation
// ArgoCD's own server-keyed cluster identity has; closing it would mean
// probing a per-cluster UID on every run. Symmetrically, one cluster reached
// via two DNS names splits the scope, which only costs a re-download.
func credentialInstance(mode, clusterServer, argocdNamespace string) string {
	if mode != config.RepoCredsCluster {
		return ""
	}
	return clusterServer + "\x00" + argocdNamespace
}

// instanceSegment renders a credential-source instance as one filesystem-safe
// path segment: the sanitized human-readable parts (so `cache info` and the
// e2e suite can see WHICH cluster a scope belongs to) plus a short hash of the
// raw value (so sanitization collisions cannot merge two instances).
func instanceSegment(instance string) string {
	sanitize := func(s string) string {
		var b strings.Builder
		for _, r := range s {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
				r == '.', r == '_', r == '-':
				b.WriteRune(r)
			default:
				b.WriteByte('_')
			}
		}
		const maxLen = 40
		out := b.String()
		if len(out) > maxLen {
			out = out[:maxLen]
		}
		return out
	}
	sum := sha256.Sum256([]byte(instance))
	parts := strings.SplitN(instance, "\x00", 2)
	readable := sanitize(parts[0])
	if len(parts) == 2 {
		readable += "_" + sanitize(parts[1])
	}
	return readable + "-" + hex.EncodeToString(sum[:4])
}

// CreateRenderCache creates the persistent render cache, or returns nil when it is
// disabled (--no-cache=all|render). When the cache directory cannot be
// prepared it returns an error; callers degrade to normal rendering. Best-effort
// garbage collection runs inline at creation to bound the cache by age and size.
func (f *Factory) CreateRenderCache() (*rendercache.Cache, error) {
	if !f.config.RenderCacheEnabled() {
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
	if len(f.config.Lint)+len(f.config.LintKyverno)+len(f.config.LintConftest) == 0 {
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
		Kyverno:  f.config.LintKyverno,
		Conftest: f.config.LintConftest,
		// The built-in adapters take these as data instead of reading them back
		// out of Env: the environment variables exist for SHELL commands, which
		// have no other way to learn them.
		KubeContext: kubeContext,
		Kubeconfig:  f.config.KubeconfigPath,
		Timeout:     f.config.LintTimeout,
		Env:         env,
		Logger:      f.logger,
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
			htmlWriter, err := output.NewHTMLWriter(fo.Path)
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
