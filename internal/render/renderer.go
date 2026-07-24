// Package render provides manifest rendering functionality.
package render

import (
	"context"
	"os"
	"path/filepath"

	argoappv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"

	"github.com/rgeraskin/argocdf/internal/cluster"
	"github.com/rgeraskin/argocdf/internal/types"
)

// Renderer defines the interface for rendering ArgoCD application manifests.
type Renderer interface {
	// Render renders the manifests for an application source.
	// The context can be used to cancel long-running render operations.
	Render(ctx context.Context, app *cluster.Application, source *cluster.ApplicationSource, repoPath string) ([]byte, error)

	// SourceType returns the type of source this renderer handles.
	SourceType() types.SourceType
}

// RenderOptions contains options for rendering.
type RenderOptions struct {
	// RepoPath is the path to the git repository
	RepoPath string

	// RepoURL is the normalized URL of the local repository being diffed.
	// It is used to detect ref sources that point at the local repo so their
	// files can be read from the local branch checkout instead of a remote clone.
	RepoURL string

	// ArgoCDNamespace is the ArgoCD control-plane namespace, used to compute
	// application instance names the way ArgoCD does: apps living outside it
	// render as "<namespace>_<name>" (feeds ARGOCD_APP_NAME and the default
	// helm release name).
	ArgoCDNamespace string

	// KubeVersion is the Kubernetes version to use for rendering
	KubeVersion string

	// APIVersions is the list of cluster API versions passed to helm via
	// --api-versions so charts can branch on .Capabilities.APIVersions.
	// Empty means no --api-versions flags are added.
	APIVersions []string

	// Namespace is the target namespace for the rendered manifests
	Namespace string

	// RefSources maps ref names to cloned repository paths for multi-source apps
	RefSources map[string]string

	// Kustomize build options (defaults from CLI)
	KustomizeEnableHelm     bool
	KustomizeBuildOptions   string
	KustomizeLoadRestrictor string

	// Helm options
	HelmSkipRefresh bool
	// HelmAddRepos registers chart dependency HTTP(S) repositories in the
	// user-level helm config (`helm repo add` + `helm repo update`) before
	// `helm dependency build`. Off by default because it mutates the user's
	// helm repository config; intended for ephemeral CI runners.
	HelmAddRepos bool

	// ChartCacheDir is the directory under which pulled remote charts (pinned
	// to an immutable version) are cached and reused across runs. Empty
	// disables the chart download cache (e.g. under --no-cache).
	ChartCacheDir string

	// Repository credentials (--repo-creds). Render code is mode-blind:
	// cluster and local modes differ only in what fills these fields, and
	// `none` leaves them empty. The four lists are kept separate so the
	// argocd engine can compose Repos/HelmRepoCreds per source with ArgoCD's
	// IsOCI gate, exactly as the application controller does.
	HelmRepos     []*argoappv1.Repository
	OCIRepos      []*argoappv1.Repository
	HelmRepoCreds []*argoappv1.RepoCreds
	OCIRepoCreds  []*argoappv1.RepoCreds
	// ResolveRepo returns the configured Repository for a source repo URL
	// (never nil on success — unknown URLs yield a credential-less default).
	// nil means no credential source is configured.
	ResolveRepo func(ctx context.Context, repoURL, project string) (*argoappv1.Repository, error)
}

// RenderResult contains the result of rendering an application.
type RenderResult struct {
	// Manifests is the raw YAML output
	Manifests []byte

	// SourceType indicates what type of source was rendered
	SourceType types.SourceType

	// Error holds any error that occurred
	Error error
}

// Factory creates the appropriate renderer for a source.
type Factory struct {
	helmRenderer      *HelmRenderer
	kustomizeRenderer *KustomizeRenderer
}

// NewFactory creates a new renderer factory.
func NewFactory(opts RenderOptions) *Factory {
	return &Factory{
		helmRenderer:      NewHelmRenderer(opts),
		kustomizeRenderer: NewKustomizeRenderer(opts),
	}
}

// GetRenderer returns the appropriate renderer for the given source.
// repoPath is used to detect Helm charts by checking for Chart.yaml in the source path.
func (f *Factory) GetRenderer(source *cluster.ApplicationSource, repoPath string) Renderer {
	// Check for Helm: either Chart field is set or Helm config is present
	if source.IsHelm() || source.Helm != nil {
		return f.helmRenderer
	}
	if source.Kustomize != nil {
		return f.kustomizeRenderer
	}
	// Explicit directory source renders as plain YAML, skipping Chart.yaml
	// auto-detection — ArgoCD's ExplicitType gives tool config precedence
	// over filesystem discovery.
	if source.Directory != nil {
		return f.kustomizeRenderer
	}
	// Check if the path contains a Chart.yaml (ArgoCD auto-detection)
	if source.Path != "" && repoPath != "" {
		chartPath := filepath.Join(repoPath, source.Path, "Chart.yaml")
		if _, err := os.Stat(chartPath); err == nil {
			return f.helmRenderer
		}
	}
	// Default to Kustomize for plain directories (ArgoCD behavior)
	return f.kustomizeRenderer
}

// RenderApplication renders all sources for an application and combines the output.
// The context can be used to cancel long-running render operations. revision is
// accepted for interface compatibility with the argocd engine (which uses it
// for ARGOCD_APP_REVISION* build-env variables); the native engine ignores it.
func (f *Factory) RenderApplication(ctx context.Context, app *cluster.Application, repoPath, _ string) (*RenderResult, error) {
	sources := app.Spec.GetSources()
	if len(sources) == 0 {
		return &RenderResult{
			SourceType: types.SourceTypeUnknown,
		}, nil
	}

	// For single source apps, render directly.
	// A source that is a pure ref (Ref set, but no Path/Chart) produces no
	// manifests, so it must go through the multi-source path instead.
	if len(sources) == 1 && !isPureRef(sources[0]) {
		// Apps-of-apps children may live entirely in another git repository;
		// they render from their own checkout, never from the local worktree.
		srcRepoPath := repoPath
		if sources[0].Chart == "" && isExternalSource(&f.helmRenderer.opts, &sources[0]) {
			clonedPath, cleanupClone, err := cloneExternalRepo(ctx, &f.helmRenderer.opts, app.Spec.Project, &sources[0])
			if err != nil {
				return &RenderResult{Error: err}, err
			}
			defer cleanupClone()
			srcRepoPath = clonedPath
		}
		renderer := f.GetRenderer(&sources[0], srcRepoPath)
		manifests, err := renderer.Render(ctx, app, &sources[0], srcRepoPath)
		return &RenderResult{
			Manifests:  manifests,
			SourceType: renderer.SourceType(),
			Error:      err,
		}, err
	}

	// For multi-source apps, we need to handle ref sources
	msRenderer := NewMultiSourceRenderer(f, repoPath)
	manifests, err := msRenderer.RenderMultiSource(ctx, app)
	return &RenderResult{
		Manifests:  manifests,
		SourceType: types.SourceTypeHelm, // Multi-source typically uses Helm
		Error:      err,
	}, err
}
