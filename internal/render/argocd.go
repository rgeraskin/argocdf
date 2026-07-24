// Package render provides manifest rendering; this file implements the
// --renderer=argocd engine, which renders through ArgoCD's own repo-server
// code (reposerver/repository.GenerateManifests) for exact ArgoCD parity.
//
// What ArgoCD's code takes over here: source-type dispatch (helm / kustomize /
// directory, including .argocd-source*.yaml overrides), the complete
// ApplicationSourceHelm/Kustomize option translation, ARGOCD_APP_* build-env
// substitution, helm's --include-crds default, and dependency building into an
// isolated temp helm home (no user helm config is touched, and dependency
// repos from Chart.yaml are registered there automatically — --helm-add-repos
// is unnecessary with this engine).
//
// What stays argocdf's: worktree management, remote-chart fetching (ArgoCD's
// repo-server fetches charts before calling GenerateManifests, so this file
// reuses the native chart download cache), $ref-source checkout, and the
// render cache (keyed with Options.Renderer so engines never share entries).
package render

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/argoproj/argo-cd/v3/common"
	argoappv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v3/reposerver/apiclient"
	"github.com/argoproj/argo-cd/v3/reposerver/repository"
	argogit "github.com/argoproj/argo-cd/v3/util/git"
	utilio "github.com/argoproj/argo-cd/v3/util/io"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	"github.com/rgeraskin/argocdf/internal/cluster"
	"github.com/rgeraskin/argocdf/internal/git"
	"github.com/rgeraskin/argocdf/internal/types"
)

// maxCombinedDirectoryManifestsSize bounds the combined size of manifest files
// a directory-type source may produce, mirroring the repo-server default
// (ARGOCD_REPO_SERVER_MAX_COMBINED_DIRECTORY_MANIFESTS_SIZE=10M). It must be
// non-zero: a zero quantity makes GenerateManifests reject every file.
var maxCombinedDirectoryManifestsSize = resource.MustParse("10M")

// defaultValuesFileSchemes mirrors ArgoCD's default helm.valuesFileSchemes
// setting (util/settings): value files may be fetched over these URL schemes.
var defaultValuesFileSchemes = []string{"https", "http"}

// ArgoCDRenderer renders applications through ArgoCD's repo-server code.
type ArgoCDRenderer struct {
	opts RenderOptions
	// helm is used only for its remote-chart fetching (persistent chart cache);
	// all templating goes through GenerateManifests.
	helm *HelmRenderer
}

// NewArgoCDRenderer creates a renderer backed by reposerver's GenerateManifests.
func NewArgoCDRenderer(opts RenderOptions) *ArgoCDRenderer {
	// GenerateManifests logs through the process-global logrus logger, which
	// argocdf does not otherwise use. Keep only errors; real failures are
	// returned as errors and surfaced by argocdf's own logging.
	logrus.SetLevel(logrus.ErrorLevel)
	// ArgoCD's exec tracer (util/exec) builds a FRESH logrus logger per
	// command from ARGOCD_LOG_LEVEL/ARGOCD_LOG_FORMAT (default: info + JSON),
	// which would print a JSON "Trace" line for every helm/kustomize run.
	// Default it to errors-only, but respect an explicitly set value so
	// ARGOCD_LOG_LEVEL=info remains available as a render debug channel.
	if os.Getenv(common.EnvLogLevel) == "" {
		_ = os.Setenv(common.EnvLogLevel, "error")
	}
	return &ArgoCDRenderer{opts: opts, helm: NewHelmRenderer(opts)}
}

// RenderApplication renders all sources of an application via ArgoCD's
// GenerateManifests and concatenates the results as multi-doc YAML. revision
// is the commit being rendered; it feeds the ARGOCD_APP_REVISION* build-env
// variables exactly as the repo-server would.
func (r *ArgoCDRenderer) RenderApplication(ctx context.Context, app *cluster.Application, repoPath, revision string) (*RenderResult, error) {
	sources := app.Spec.GetSources()
	if len(sources) == 0 {
		return &RenderResult{SourceType: types.SourceTypeUnknown}, nil
	}

	refSources, tempPaths, cleanup, err := r.prepareRefSources(sources, repoPath)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare ref sources: %w", err)
	}
	defer cleanup()

	var all bytes.Buffer
	sourceType := types.SourceTypeUnknown
	for i := range sources {
		// Pure ref sources produce no manifests (same rule as the native engine).
		if isPureRef(sources[i]) {
			continue
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		manifests, srcType, err := r.renderSource(ctx, app, &sources[i], repoPath, revision, refSources, tempPaths)
		if err != nil {
			rerr := fmt.Errorf("failed to render source %d: %w", i, err)
			return &RenderResult{Error: rerr}, rerr
		}
		if sourceType == types.SourceTypeUnknown {
			sourceType = srcType
		}
		if all.Len() > 0 && len(manifests) > 0 {
			all.WriteString("---\n")
		}
		all.Write(manifests)
	}

	return &RenderResult{Manifests: all.Bytes(), SourceType: sourceType}, nil
}

// renderSource renders a single source through GenerateManifests.
func (r *ArgoCDRenderer) renderSource(
	ctx context.Context,
	app *cluster.Application,
	source *cluster.ApplicationSource,
	repoPath, revision string,
	refSources map[string]*argoappv1.RefTarget,
	tempPaths utilio.TempPaths,
) ([]byte, types.SourceType, error) {
	var appPath, repoRoot string
	if source.Chart != "" {
		// Remote chart: ArgoCD's repo-server fetches charts BEFORE calling
		// GenerateManifests, so argocdf does the same, reusing the native
		// persistent chart cache for pinned versions.
		chartDir, cleanupChart, err := r.ensureRemoteChart(ctx, source)
		if err != nil {
			return nil, "", err
		}
		defer cleanupChart()
		appPath, repoRoot = chartDir, chartDir
	} else {
		appPath = filepath.Join(repoPath, source.Path)
		if err := ValidatePathContainment(repoPath, appPath); err != nil {
			return nil, "", fmt.Errorf("invalid source path %q: %w", source.Path, err)
		}
		repoRoot = repoPath
	}

	q := r.buildManifestRequest(app, source, refSources)

	// Serialize per appPath: GenerateManifests may write into appPath (helm
	// dependency build: charts/, Chart.lock, its skip marker) and restores
	// Chart.lock state after templating. Two apps sharing one chart directory
	// in the same worktree must not interleave those writes. The repo-server
	// never faces this (it locks per repo+revision a level above); argocdf's
	// parallel waves can. Reuses the same per-path mutex as the native engine.
	mu := chartDepMutex(appPath)
	mu.Lock()
	resp, err := repository.GenerateManifests(
		ctx, appPath, repoRoot, revision, q,
		false, // isLocal=false gives the ISOLATED temp helm home (XDG_*, HELM_CONFIG_HOME)
		argogit.NoopCredsStore{},
		maxCombinedDirectoryManifestsSize,
		tempPaths,
	)
	mu.Unlock()
	if err != nil {
		return nil, "", err
	}

	manifests, err := manifestsToYAML(resp.Manifests)
	if err != nil {
		return nil, "", err
	}
	return manifests, mapSourceType(resp.SourceType), nil
}

// buildManifestRequest assembles the per-source ManifestRequest.
// GenerateManifests mutates q.ApplicationSource in memory (merging
// .argocd-source*.yaml overrides), so the source is deep-copied — requests
// must never share an ApplicationSource across goroutines.
func (r *ArgoCDRenderer) buildManifestRequest(
	app *cluster.Application,
	source *cluster.ApplicationSource,
	refSources map[string]*argoappv1.RefTarget,
) *apiclient.ManifestRequest {
	return &apiclient.ManifestRequest{
		// Repo must be non-nil (proxy/creds/env lookups dereference it).
		Repo:               &argoappv1.Repository{Repo: source.RepoURL},
		AppName:            app.Name,
		Namespace:          app.Spec.Destination.Namespace,
		ApplicationSource:  source.DeepCopy(),
		KubeVersion:        r.opts.KubeVersion, // parseKubeVersion handles vendor suffixes (-gke.*)
		ApiVersions:        r.opts.APIVersions,
		KustomizeOptions:   r.kustomizeOptions(),
		HelmOptions:        &argoappv1.HelmOptions{ValuesFileSchemes: defaultValuesFileSchemes},
		RefSources:         refSources,
		HasMultipleSources: len(app.Spec.GetSources()) > 1,
		// AppLabelKey/TrackingMethod are intentionally left empty so no
		// tracking labels are injected — diffs stay comparable to the native
		// engine and to plain chart output.
		//
		// ProjectSourceRepos feeds only a permission-check that rewrites
		// dependency-fetch failures into "repo not permitted in project"
		// errors; argocdf has no AppProject context, so allow everything to
		// keep the real error visible.
		ProjectSourceRepos: []string{"*"},
	}
}

// kustomizeOptions maps argocdf's kustomize flags onto ArgoCD's KustomizeOptions
// build-options string.
func (r *ArgoCDRenderer) kustomizeOptions() *argoappv1.KustomizeOptions {
	var parts []string
	if r.opts.KustomizeBuildOptions != "" {
		parts = append(parts, r.opts.KustomizeBuildOptions)
	}
	if r.opts.KustomizeEnableHelm {
		parts = append(parts, "--enable-helm")
	}
	if r.opts.KustomizeLoadRestrictor != "" {
		parts = append(parts, "--load-restrictor="+r.opts.KustomizeLoadRestrictor)
	}
	if len(parts) == 0 {
		return nil
	}
	return &argoappv1.KustomizeOptions{BuildOptions: strings.Join(parts, " ")}
}

// prepareRefSources builds the RefSources map ("$name" keys, ArgoCD's format)
// and registers each ref repository's checkout in a TempPaths registry keyed by
// the normalized repo URL — exactly where getResolvedRefValueFile looks paths
// up. GenerateManifests never clones; every ref repo must be materialized here:
// the local repo maps to the current worktree, external repos are cloned to
// temp dirs (removed by cleanup).
//
// Note a deliberate semantic difference from the native engine: ArgoCD resolves
// "$ref/some/path" against the ref repository ROOT (RefTarget has no Path
// field), while the native engine joins the ref source's Path first. The
// argocd engine follows ArgoCD.
func (r *ArgoCDRenderer) prepareRefSources(
	sources []cluster.ApplicationSource,
	repoPath string,
) (map[string]*argoappv1.RefTarget, utilio.TempPaths, func(), error) {
	refSources := make(map[string]*argoappv1.RefTarget)
	tempPaths := utilio.NewRandomizedTempPaths(os.TempDir())

	var tempDirs []string
	cleanup := func() {
		for _, dir := range tempDirs {
			_ = SafeRemoveAll(dir)
		}
	}

	for _, source := range sources {
		if source.Ref == "" {
			continue
		}
		refSources["$"+source.Ref] = &argoappv1.RefTarget{
			Repo:           argoappv1.Repository{Repo: source.RepoURL},
			TargetRevision: source.TargetRevision,
			Chart:          source.Chart,
		}

		key := argogit.NormalizeGitURL(source.RepoURL)
		if tempPaths.GetPathIfExists(key) != "" {
			continue // repo already materialized under this key
		}

		// A ref pointing at the repo being diffed resolves to the current
		// worktree, so PR edits to $values files actually produce a diff.
		if r.opts.RepoURL != "" && git.NormalizeRepoURL(source.RepoURL) == git.NormalizeRepoURL(r.opts.RepoURL) {
			tempPaths.Add(key, repoPath)
			continue
		}

		tempDir, err := os.MkdirTemp("", "argocdf-ref-")
		if err != nil {
			cleanup()
			return nil, nil, nil, fmt.Errorf("failed to create temp dir for ref %s: %w", source.Ref, err)
		}
		tempDirs = append(tempDirs, tempDir)
		if err := git.Clone(source.RepoURL, source.TargetRevision, tempDir); err != nil {
			cleanup()
			return nil, nil, nil, fmt.Errorf("failed to clone ref source %s: %w", source.Ref, err)
		}
		tempPaths.Add(key, tempDir)
	}

	return refSources, tempPaths, cleanup, nil
}

// ensureRemoteChart materializes a remote chart as a local directory: from the
// persistent chart cache when the version is pinned and caching is enabled,
// otherwise via a one-shot `helm pull` into a temp dir (also the fallback when
// populating the cache fails, so rendering stays functional).
func (r *ArgoCDRenderer) ensureRemoteChart(ctx context.Context, source *cluster.ApplicationSource) (string, func(), error) {
	cacheDir, chartDir, hit, enabled := chartCacheDecision(
		r.opts.ChartCacheDir, source.RepoURL, source.Chart, source.TargetRevision, dirExists,
	)
	if enabled {
		if hit {
			return chartDir, func() {}, nil
		}
		if err := r.helm.pullChartToCache(ctx, source, cacheDir, chartDir); err == nil {
			return chartDir, func() {}, nil
		}
	}
	return pullChartToTempDir(ctx, source)
}

// pullChartToTempDir pulls and unpacks a chart into a fresh temp directory
// using an isolated helm home. Unlike the cache path it accepts unpinned
// versions (ranges, empty = latest). The returned cleanup removes the temp dir.
func pullChartToTempDir(ctx context.Context, source *cluster.ApplicationSource) (string, func(), error) {
	tmp, err := os.MkdirTemp("", "argocdf-chart-")
	if err != nil {
		return "", nil, fmt.Errorf("failed to create temp chart dir: %w", err)
	}
	cleanup := func() { _ = SafeRemoveAll(tmp) }

	homeTmp, err := os.MkdirTemp("", "argocdf-helmhome-")
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("failed to create temp helm home: %w", err)
	}
	defer func() { _ = SafeRemoveAll(homeTmp) }()

	args := []string{"pull"}
	if isOCIChartRepo(source.RepoURL) {
		args = append(args, ociChartRef(source.RepoURL, source.Chart))
	} else {
		args = append(args, source.Chart, "--repo", source.RepoURL)
	}
	if source.TargetRevision != "" && source.TargetRevision != "HEAD" {
		args = append(args, "--version", source.TargetRevision)
	}
	args = append(args, "--untar", "--untardir", tmp)

	cmd := exec.CommandContext(ctx, "helm", args...)
	cmd.Env = isolatedHelmEnv(homeTmp)
	if output, err := cmd.CombinedOutput(); err != nil {
		cleanup()
		if ctx.Err() != nil {
			return "", nil, ctx.Err()
		}
		return "", nil, fmt.Errorf("failed to pull helm chart: %v\noutput: %s", err, output)
	}

	// helm pull --untar unpacks into a directory named after the chart.
	return filepath.Join(tmp, filepath.Base(source.Chart)), cleanup, nil
}

// manifestsToYAML converts GenerateManifests' output (one JSON document string
// per manifest) into the multi-doc YAML stream the differ consumes.
func manifestsToYAML(manifests []string) ([]byte, error) {
	var buf bytes.Buffer
	for _, m := range manifests {
		y, err := yaml.JSONToYAML([]byte(m))
		if err != nil {
			return nil, fmt.Errorf("failed to convert manifest to YAML: %w", err)
		}
		if buf.Len() > 0 {
			buf.WriteString("---\n")
		}
		buf.Write(y)
	}
	return buf.Bytes(), nil
}

// mapSourceType maps ArgoCD's ApplicationSourceType strings onto argocdf's.
func mapSourceType(s string) types.SourceType {
	switch s {
	case string(argoappv1.ApplicationSourceTypeHelm):
		return types.SourceTypeHelm
	case string(argoappv1.ApplicationSourceTypeKustomize):
		return types.SourceTypeKustomize
	case string(argoappv1.ApplicationSourceTypeDirectory):
		return types.SourceTypePlain
	default:
		return types.SourceTypeUnknown
	}
}
