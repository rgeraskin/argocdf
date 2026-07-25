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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	argoappv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v3/reposerver/apiclient"
	"github.com/argoproj/argo-cd/v3/reposerver/repository"
	argogit "github.com/argoproj/argo-cd/v3/util/git"
	utilio "github.com/argoproj/argo-cd/v3/util/io"
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
	// ownedRegistryAuth is the per-run registry auth file when this engine
	// created one (all --repo-creds modes except local, which pierces with the
	// user's own registry config instead). Removed by Cleanup.
	ownedRegistryAuth *registryAuthFile
	// restoreHelmEnv undoes the process-global helm env mutation made at
	// construction (isolateHelmEnv). Run by Cleanup.
	restoreHelmEnv func()
}

// NewArgoCDRenderer creates a renderer backed by reposerver's GenerateManifests.
//
// Construction has process-global side effects that make ArgoCD's helm
// isolation actually hold outside a repo-server container: the inherited helm
// environment is scrubbed (see inheritedHelmEnvVars) and HELM_REGISTRY_CONFIG
// is pointed at either the user's registry config (--repo-creds=local, via
// opts.HelmRegistryConfig) or an argocdf-owned per-run auth file seeded from
// the credential lists — whose OCI entries are then stripped of
// username/password so ArgoCD's DependencyBuild never execs `helm registry
// login`/`logout` (on macOS those land in the shared system keychain via
// ORAS native-store detection and race across concurrent renders).
func NewArgoCDRenderer(opts RenderOptions) (*ArgoCDRenderer, error) {
	r := &ArgoCDRenderer{}
	registryConfig := opts.HelmRegistryConfig
	if registryConfig == "" {
		auth, err := newRegistryAuthFile()
		if err != nil {
			return nil, err
		}
		opts.HelmRepos, err = seedAndStripRepos(auth, opts.HelmRepos)
		if err == nil {
			opts.OCIRepos, err = seedAndStripRepos(auth, opts.OCIRepos)
		}
		if err == nil {
			opts.HelmRepoCreds, err = seedAndStripRepoCreds(auth, opts.HelmRepoCreds)
		}
		if err == nil {
			opts.OCIRepoCreds, err = seedAndStripRepoCreds(auth, opts.OCIRepoCreds)
		}
		if err != nil {
			auth.Remove()
			return nil, err
		}
		opts.registryAuth = auth
		r.ownedRegistryAuth = auth
		registryConfig = auth.path
	}
	r.restoreHelmEnv = isolateHelmEnv(registryConfig)

	r.opts = opts
	r.helm = NewHelmRenderer(opts)
	return r, nil
}

// Cleanup restores the pre-construction helm environment and removes the
// per-run registry auth file (it holds short-lived tokens). Safe to call
// multiple times and on a renderer that owns no auth file. The renderer must
// not render after Cleanup. The env restore is a snapshot of THIS instance's
// construction-time env: the helm env is process-global, so overlapping
// instances must be cleaned up in LIFO order — an out-of-order Cleanup
// rewinds a still-live instance's HELM_REGISTRY_CONFIG.
func (r *ArgoCDRenderer) Cleanup() {
	if r.restoreHelmEnv != nil {
		r.restoreHelmEnv()
		r.restoreHelmEnv = nil
	}
	if r.ownedRegistryAuth != nil {
		r.ownedRegistryAuth.Remove()
	}
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

	refSources, tempPaths, cleanup, err := r.prepareRefSources(ctx, app.Spec.Project, sources, repoPath)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare ref sources: %w", err)
	}
	defer cleanup()

	// Renderable sources from OTHER git repositories (apps-of-apps children
	// and multi-source apps may reference them) render from their own
	// checkout at TargetRevision, never from the local worktree.
	externalRepos := newExternalRepoSet(&r.opts, app.Spec.Project)
	defer externalRepos.cleanup()

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

		srcRepoPath, err := externalRepos.repoPathFor(ctx, &sources[i], repoPath)
		if err != nil {
			rerr := fmt.Errorf("failed to render source %d: %w", i, err)
			return &RenderResult{Error: rerr}, rerr
		}

		manifests, srcType, err := r.renderSource(ctx, app, &sources[i], srcRepoPath, revision, refSources, tempPaths)
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
		// GenerateManifests, so argocdf does the same — through ArgoCD's own
		// chart client, wrapped in the persistent chart cache.
		chartDir, cached, cleanupChart, err := r.helm.fetchRemoteChart(ctx, app, source)
		if err != nil {
			return nil, "", err
		}
		defer cleanupChart()
		if cached {
			// GenerateManifests may build dependencies INTO appPath (charts/,
			// Chart.lock, its skip marker). The persistent cache must stay a
			// pristine shared artifact — and chartDepMutex is process-local,
			// so concurrent argocdf processes could otherwise corrupt each
			// other's cache entries — so cache-backed directories are copied
			// to a private temp dir first. Freshly extracted directories are
			// already private.
			privateDir, cleanupCopy, err := copyChartToTempDir(chartDir)
			if err != nil {
				return nil, "", err
			}
			defer cleanupCopy()
			chartDir = privateDir
		}
		appPath, repoRoot = chartDir, chartDir
	} else {
		appPath = filepath.Join(repoPath, source.Path)
		if err := ValidatePathContainment(repoPath, appPath); err != nil {
			return nil, "", fmt.Errorf("invalid source path %q: %w", source.Path, err)
		}
		repoRoot = repoPath
	}

	q, err := r.buildManifestRequest(ctx, app, source, refSources)
	if err != nil {
		return nil, "", err
	}

	// Serialize per appPath: GenerateManifests may write into appPath (helm
	// dependency build: charts/, Chart.lock, its skip marker) and restores
	// Chart.lock state after templating. Two apps sharing one chart directory
	// in the same worktree must not interleave those writes. The repo-server
	// never faces this (it locks per repo+revision a level above); argocdf's
	// parallel waves can. Reuses the same per-path mutex as the native engine.
	//
	// ArgoCD applies kustomize overrides (namePrefix, images, patches, ...) by
	// rewriting kustomization.yaml IN PLACE (`kustomize edit` plus a direct
	// write for patches — util/kustomize touches no other file). The
	// repo-server renders from managed checkouts so that mutation is invisible
	// there; argocdf renders every app from a per-side SHARED worktree, where
	// it would leak into later renders of the same path (helm's charts/ writes
	// are content-identical for apps pinning the same dependency versions —
	// same-path apps with DIVERGENT dependency pins would still interfere, an
	// accepted residual risk; kustomize edits are app-specific and are not
	// safe to share). Snapshot the kustomization file under the mutex and
	// restore it afterwards — even when the render fails or panics, since the
	// edit may have already happened. A restore failure is an error: a
	// poisoned worktree would silently corrupt every later render of the path.
	// Unlock and restore are deferred so a panic inside GenerateManifests
	// cannot leave the mutex held or the worktree poisoned.
	resp, err := func() (resp *apiclient.ManifestResponse, err error) {
		mu := chartDepMutex(appPath)
		mu.Lock()
		defer mu.Unlock()
		restoreKustomization, err := snapshotKustomization(appPath)
		if err != nil {
			return nil, err
		}
		defer func() { err = errors.Join(err, restoreKustomization()) }()
		return repository.GenerateManifests(
			ctx, appPath, repoRoot, revision, q,
			false, // isLocal=false gives the ISOLATED temp helm home (XDG_*, HELM_CONFIG_HOME)
			argogit.NoopCredsStore{},
			maxCombinedDirectoryManifestsSize,
			tempPaths,
		)
	}()
	if err != nil {
		return nil, "", err
	}

	manifests, err := manifestsToYAML(resp.Manifests)
	if err != nil {
		return nil, "", err
	}
	return manifests, mapSourceType(resp.SourceType), nil
}

// snapshotKustomization captures the kustomization file in dir, if one
// exists, and returns a restore func that writes the original bytes back
// (preserving mode). Kustomize accepts exactly one kustomization file per
// directory; the first match wins, mirroring ArgoCD's findKustomizeFile.
// Directories without one (helm charts, plain manifests) get a no-op restore.
func snapshotKustomization(dir string) (func() error, error) {
	for _, name := range KustomizationNames {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("failed to stat %s: %w", path, err)
		}
		if info.IsDir() {
			continue
		}
		original, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("failed to snapshot %s: %w", path, err)
		}
		restore := func() error {
			if err := os.WriteFile(path, original, info.Mode()); err != nil {
				return fmt.Errorf("failed to restore %s after render: %w", path, err)
			}
			return nil
		}
		return restore, nil
	}
	return func() error { return nil }, nil
}

// buildManifestRequest assembles the per-source ManifestRequest.
// GenerateManifests mutates q.ApplicationSource in memory (merging
// .argocd-source*.yaml overrides), so the source is deep-copied — requests
// must never share an ApplicationSource across goroutines.
func (r *ArgoCDRenderer) buildManifestRequest(
	ctx context.Context,
	app *cluster.Application,
	source *cluster.ApplicationSource,
	refSources map[string]*argoappv1.RefTarget,
) (*apiclient.ManifestRequest, error) {
	// Compose the repository lists per source, mirroring ArgoCD's controller
	// (controller/state.go:300-315): OCI repos and credential templates are
	// offered only for oci:// sources. Intentional parity, including the
	// degradations — an https chart with oci:// dependencies gets no OCI
	// creds, and a scheme-less helm-OCI source URL does not count as IsOCI —
	// exactly like stock ArgoCD.
	repos, repoCreds := r.opts.HelmRepos, r.opts.HelmRepoCreds
	if source.IsOCI() {
		repos = append(slices.Clone(repos), r.opts.OCIRepos...)
		repoCreds = append(slices.Clone(repoCreds), r.opts.OCIRepoCreds...)
	}

	// Repo must be non-nil (proxy/creds/env lookups dereference it). The
	// resolved repo carries creds/proxy/TLS from --repo-creds. Dependency
	// auth flows through Repos/HelmRepoCreds into ArgoCD's DependencyBuild:
	// classic repositories keep their credentials (authenticated `helm repo
	// add`), while OCI entries were stripped at construction — their auth
	// rides the HELM_REGISTRY_CONFIG file instead, so no `helm registry
	// login`/`logout` ever runs (see registryAuthFile).
	repo, err := r.resolveSourceRepo(ctx, app.Spec.Project, source.RepoURL)
	if err != nil {
		return nil, err
	}

	return &apiclient.ManifestRequest{
		Repo:          repo,
		Repos:         repos,
		HelmRepoCreds: repoCreds,
		// AppName is the application INSTANCE name — apps outside the
		// control-plane namespace qualify as "<namespace>_<name>" — exactly
		// what ArgoCD sends: it feeds ARGOCD_APP_NAME and the default helm
		// release name.
		AppName:            app.InstanceName(r.opts.ArgoCDNamespace),
		Namespace:          app.Spec.Destination.Namespace,
		ApplicationSource:  source.DeepCopy(),
		KubeVersion:        r.opts.KubeVersion, // parseKubeVersion handles vendor suffixes (-gke.*)
		ApiVersions:        r.opts.APIVersions,
		KustomizeOptions:   r.kustomizeOptions(),
		HelmOptions:        &argoappv1.HelmOptions{ValuesFileSchemes: defaultValuesFileSchemes},
		RefSources:         refSources,
		HasMultipleSources: len(app.Spec.GetSources()) > 1,
		// ProjectName feeds the ARGOCD_APP_PROJECT_NAME build-env variable.
		ProjectName: app.Spec.Project,
		// AppLabelKey/TrackingMethod are intentionally left empty so no
		// tracking labels are injected — diffs stay comparable to the native
		// engine and to plain chart output.
		//
		// ProjectSourceRepos feeds only a permission-check that rewrites
		// dependency-fetch failures into "repo not permitted in project"
		// errors; argocdf has no AppProject context, so allow everything to
		// keep the real error visible.
		ProjectSourceRepos: []string{"*"},
	}, nil
}

// resolveSourceRepo returns the Repository configured for repoURL — with
// credentials, proxy, and TLS settings from the --repo-creds source — or a
// bare Repository when no credential source is configured. Resolution
// failures are errors (no silent anonymous fallback).
func (r *ArgoCDRenderer) resolveSourceRepo(ctx context.Context, project, repoURL string) (*argoappv1.Repository, error) {
	return resolveRepoOrBare(ctx, &r.opts, project, repoURL)
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
	ctx context.Context,
	project string,
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
		// The resolved repo keeps parity with the request's Repo and carries
		// the credentials the external clone below authenticates with.
		refRepo, err := r.resolveSourceRepo(ctx, project, source.RepoURL)
		if err != nil {
			cleanup()
			return nil, nil, nil, err
		}
		refSources["$"+source.Ref] = &argoappv1.RefTarget{
			Repo:           *refRepo,
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
		if err := git.CloneWithCreds(source.RepoURL, source.TargetRevision, tempDir, cloneCredsFromRepo(refRepo)); err != nil {
			cleanup()
			return nil, nil, nil, fmt.Errorf("failed to clone ref source %s: %w", source.Ref, err)
		}
		tempPaths.Add(key, tempDir)
	}

	return refSources, tempPaths, cleanup, nil
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
