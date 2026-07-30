// Package render provides manifest rendering functionality.
package render

import (
	"context"

	argoappv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"

	"github.com/rgeraskin/argocdf/internal/types"
)

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

	// ChartCacheDir is the directory under which pulled remote charts (pinned
	// to an immutable version) are cached and reused across runs. Empty
	// disables the chart download cache (e.g. under --no-cache).
	ChartCacheDir string

	// Repository credentials (--repo-creds). Render code is mode-blind:
	// cluster and local modes differ only in what fills these fields, and
	// `none` leaves them empty. The four lists are kept separate so
	// Repos/HelmRepoCreds can be composed per source with ArgoCD's IsOCI
	// gate, exactly as the application controller does.
	HelmRepos     []*argoappv1.Repository
	OCIRepos      []*argoappv1.Repository
	HelmRepoCreds []*argoappv1.RepoCreds
	OCIRepoCreds  []*argoappv1.RepoCreds
	// ResolveRepo returns the configured Repository for a source repo URL
	// (never nil on success — unknown URLs yield a credential-less default).
	// nil means no credential source is configured.
	ResolveRepo func(ctx context.Context, repoURL, project string) (*argoappv1.Repository, error)

	// HelmRegistryConfig is an explicit helm registry config path to expose
	// to ArgoCD's helm executions (--repo-creds=local sets it to the user's
	// own registry config so OCI pulls authenticate with the user's logins,
	// read-only). Empty means the engine owns registry auth itself via a
	// per-run auth file.
	HelmRegistryConfig string

	// registryAuth is the engine's per-run registry auth file, set at engine
	// construction. When non-nil, chart fetching records resolved OCI
	// credentials here and hands ArgoCD's chart client credential-less
	// repositories, so its `helm registry login` (macOS: shared-keychain via
	// ORAS native-store detection) never runs. nil only when
	// HelmRegistryConfig pierces with the user's own registry config instead.
	registryAuth *registryAuthFile
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
