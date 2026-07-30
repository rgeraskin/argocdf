// Package cluster provides ArgoCD Application operations.
package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	argoapp "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	apppath "github.com/argoproj/argo-cd/v3/util/app/path"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rgeraskin/argocdf/internal/git"
)

// ArgoCD Application GVR (GroupVersionResource).
var ApplicationGVR = schema.GroupVersionResource{
	Group:    "argoproj.io",
	Version:  "v1alpha1",
	Resource: "applications",
}

// Type aliases for ArgoCD types - provides cleaner imports for consumers.
type (
	Application                = argoapp.Application
	ApplicationSpec            = argoapp.ApplicationSpec
	ApplicationSource          = argoapp.ApplicationSource
	ApplicationSourceHelm      = argoapp.ApplicationSourceHelm
	HelmParameter              = argoapp.HelmParameter
	HelmFileParameter          = argoapp.HelmFileParameter
	ApplicationSourceKustomize = argoapp.ApplicationSourceKustomize
	ApplicationSourceDirectory = argoapp.ApplicationSourceDirectory
	ApplicationDestination     = argoapp.ApplicationDestination

	// Kustomize-related types
	KustomizePatch    = argoapp.KustomizePatch
	KustomizePatches  = argoapp.KustomizePatches
	KustomizeReplica  = argoapp.KustomizeReplica
	KustomizeReplicas = argoapp.KustomizeReplicas
	KustomizeImage    = argoapp.KustomizeImage
	KustomizeImages   = argoapp.KustomizeImages
	KustomizeSelector = argoapp.KustomizeSelector

	// Source hydrator (dry source -> hydrated sync source). Aliased so tests can
	// build such an app without importing argo-cd types directly: selection
	// inherits an upstream special case for these apps, see
	// TestManifestGeneratePathsSourceHydratorFollowsUpstream.
	SourceHydrator = argoapp.SourceHydrator
	DrySource      = argoapp.DrySource
	SyncSource     = argoapp.SyncSource
)

// The three helpers below wrap ArgoCD's own manifest-generate-paths resolution.
// They exist so the annotation is interpreted by ArgoCD's code rather than
// reimplemented (its rules are specific: `;` separates entries, a leading `/` is
// repo-root-relative, everything else is joined to the SOURCE's path and cleaned),
// and so the argo-cd import stays inside this package like the type aliases above.

// AnnotationKeyManifestGeneratePaths is ArgoCD's declaration of which repository
// paths generate an application's manifests.
const AnnotationKeyManifestGeneratePaths = argoapp.AnnotationKeyManifestGeneratePaths

// HasManifestGeneratePaths reports whether an Application declares the paths that
// generate its manifests. A present-but-empty annotation counts as absent: it
// declares nothing, and callers then keep their own matching rather than treating
// the app as always affected.
func HasManifestGeneratePaths(app *Application) bool {
	return strings.TrimSpace(app.Annotations[argoapp.AnnotationKeyManifestGeneratePaths]) != ""
}

// ManifestGeneratePaths resolves the declared paths for ONE source, repo-relative.
// ArgoCD resolves per source because relative entries (`.`, `../base`) are joined
// to that source's own path, so a multi-source app can declare one annotation and
// have it mean the right thing for each source.
func ManifestGeneratePaths(app *Application, source ApplicationSource) []string {
	return apppath.GetSourceRefreshPaths(app, source)
}

// ChangedUnderDeclaredPaths reports whether any changed file lies under the
// declared paths.
//
// One DELIBERATE deviation from ArgoCD, and it is the whole reason this wrapper
// guards instead of delegating outright: upstream returns true for an EMPTY change
// list, because a webhook payload that omitted the file list means "unknown, so
// refresh to be safe". argocdf's list never means that - it comes from a git diff
// it computed itself, so empty means there are no changes, and nothing can be
// affected by them. Delegating that case turned `--base X --target X` into "every
// annotated app changed", rendering them all.
//
// Empty declaredPaths are still upstream's problem to answer (it treats them as
// "always refresh"); callers must not reach here with them, which is why
// manifestGeneratePathsAffected skips a source whose declaration resolves to
// nothing.
func ChangedUnderDeclaredPaths(declaredPaths, changedFiles []string) bool {
	if len(changedFiles) == 0 {
		return false
	}

	return apppath.AppFilesHaveChanged(declaredPaths, changedFiles)
}

// ApplicationService provides operations on ArgoCD Applications.
type ApplicationService struct {
	client *Client
}

// NewApplicationService creates a new ApplicationService.
func NewApplicationService(client *Client) *ApplicationService {
	return &ApplicationService{client: client}
}

// List retrieves all ArgoCD Applications from the specified namespace.
func (s *ApplicationService) List(ctx context.Context, namespace string) ([]Application, error) {
	list, err := s.client.dynamicClient.Resource(ApplicationGVR).
		Namespace(namespace).
		List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list applications in namespace %s: %w", namespace, err)
	}

	return s.convertList(list)
}

// ListNamespaces retrieves ArgoCD Applications from each of the given
// namespaces with one namespaced List call per entry, so strictly
// namespace-scoped RBAC suffices (no cluster-wide list). Duplicate entries
// are queried once.
func (s *ApplicationService) ListNamespaces(ctx context.Context, namespaces []string) ([]Application, error) {
	seen := make(map[string]struct{}, len(namespaces))
	var apps []Application
	for _, ns := range namespaces {
		if _, ok := seen[ns]; ok {
			continue
		}
		seen[ns] = struct{}{}
		nsApps, err := s.List(ctx, ns)
		if err != nil {
			return nil, err
		}
		apps = append(apps, nsApps...)
	}
	return apps, nil
}

// ListAllNamespaces retrieves ArgoCD Applications from all namespaces.
func (s *ApplicationService) ListAllNamespaces(ctx context.Context) ([]Application, error) {
	list, err := s.client.dynamicClient.Resource(ApplicationGVR).
		List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list applications across all namespaces: %w", err)
	}

	return s.convertList(list)
}

// Get retrieves a specific ArgoCD Application.
func (s *ApplicationService) Get(ctx context.Context, namespace, name string) (*Application, error) {
	obj, err := s.client.dynamicClient.Resource(ApplicationGVR).
		Namespace(namespace).
		Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get application %s/%s: %w", namespace, name, err)
	}

	return s.convertOne(obj)
}

// convertList converts an unstructured list to typed Applications.
func (s *ApplicationService) convertList(list *unstructured.UnstructuredList) ([]Application, error) {
	apps := make([]Application, 0, len(list.Items))

	for _, item := range list.Items {
		app, err := s.convertOne(&item)
		if err != nil {
			return nil, err
		}
		apps = append(apps, *app)
	}

	return apps, nil
}

// convertOne converts an unstructured object to a typed Application.
// Uses JSON marshaling instead of reflection-based converter to avoid
// Go 1.21+ strict reflection rules on unexported fields.
func (s *ApplicationService) convertOne(obj *unstructured.Unstructured) (*Application, error) {
	// Marshal to JSON and unmarshal to typed struct
	// This avoids reflection issues with unexported fields in ArgoCD types
	data, err := json.Marshal(obj.Object)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal unstructured to JSON: %w", err)
	}

	var app Application
	if err := json.Unmarshal(data, &app); err != nil {
		return nil, fmt.Errorf("failed to unmarshal JSON to Application: %w", err)
	}

	return &app, nil
}

// FilterByRepoURL filters applications that match the given repository URL.
func FilterByRepoURL(apps []Application, repoURL string) []Application {
	filtered := make([]Application, 0)
	normalizedRepoURL := git.NormalizeRepoURL(repoURL)

	for _, app := range apps {
		sources := app.Spec.GetSources()
		for _, source := range sources {
			if git.NormalizeRepoURL(source.RepoURL) == normalizedRepoURL {
				filtered = append(filtered, app)
				break
			}
		}
	}

	return filtered
}
