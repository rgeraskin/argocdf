// Package cluster provides ArgoCD Application operations.
package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	argoapp "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	apppath "github.com/argoproj/argo-cd/v3/util/app/path"
	pathutil "github.com/argoproj/argo-cd/v3/util/io/path"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

// resolveSyntheticRoot is a repository root that INTENTIONALLY does not exist on
// disk. See ResolveHelmFilePath for why that is the point.
const resolveSyntheticRoot = "/__argocdf_repo_root__"

// ResolveHelmFilePath resolves one helm value-file or fileParameter entry,
// declared by a source whose Path is sourcePath, to a path relative to the
// repository root - so a changed-file list can be matched against it.
//
// The rule is ArgoCD's, and so is the code: relative entries resolve against the
// SOURCE's path, absolute entries against the repository ROOT, entries escaping
// the repository are refused, an entry resolving exactly to the root is refused,
// and a URL is reported as remote when its scheme is allowed. Reimplementing that
// is how argocdf ended up with two divergent copies of the $ref rule, so this
// calls pathutil.ResolveValueFilePathOrUrl - the very function
// reposerver/repository uses while rendering - and derives the repo-relative path
// from its answer.
//
// The resolver takes real paths, but app SELECTION runs before any worktree
// exists (App.Run filters apps, then sets up worktrees). Passing a nonexistent
// root is what bridges that: the resolver's only filesystem call is os.Readlink,
// and a missing path returns a *os.PathError which it treats as "not a symlink",
// so resolution against resolveSyntheticRoot is a pure function of the strings.
// That is an incidental property of upstream, not a contract - if an argo-cd bump
// adds an existence check here, every entry would fail to resolve and this
// matcher would silently stop matching, so TestResolveHelmFilePathParity pins it
// by comparing these answers against a REAL tree.
//
// One consequence of the synthetic root: symlinked value files are not followed
// (there is nothing to readlink). For matching a changed-file list that is the
// right level anyway - git reports the path as it is in the tree, not its target.
func ResolveHelmFilePath(
	sourcePath, entry string,
	allowedURLSchemes []string,
) (relPath string, remote bool, err error) {
	resolved, remote, err := pathutil.ResolveValueFilePathOrUrl(
		filepath.Join(resolveSyntheticRoot, sourcePath),
		resolveSyntheticRoot,
		entry,
		allowedURLSchemes,
	)
	if err != nil || remote {
		return "", remote, err
	}

	rel, err := filepath.Rel(resolveSyntheticRoot, string(resolved))
	if err != nil {
		return "", false, err
	}

	return filepath.ToSlash(rel), false, nil
}

// ResolveRefFilePath resolves a `$<ref>/path` helm entry - a value file or
// fileParameter living in another SOURCE of the same app - to the ref source it
// names and a path relative to THAT repository's root.
//
// The root-relative part is the whole point, and it is what argocdf got wrong twice:
// ArgoCD's getResolvedRefValueFile splits the entry on "/", BLANKS the first
// segment, and hands the remainder to pathutil.ResolveValueFilePathOrUrl with the
// ref repository's checkout as BOTH appPath and repoRoot
// (reposerver/repository/repository.go:1416-1425). RefTarget carries no path at
// all, so the ref source's own Path never participates. Joining it - the native
// engine's old behavior - makes a caller look for a file that does not exist, which
// cost a silent stale cache (fixed in rendercache-v3) and a silently unreported app
// (fixed in selection).
//
// Because appPath and repoRoot are the same directory upstream, resolving the
// remainder against an EMPTY source path is exactly equivalent - so the path half
// delegates to ResolveHelmFilePath and therefore to ArgoCD's own resolver, which
// brings the traversal refusal, the resolve-to-root refusal and the URL-scheme
// handling with it instead of reimplementing them per caller.
//
// The caller still decides whether the ref source is usable to it: selection
// compares the ref source's repo URL with the repo being diffed, the render cache
// asks its SameRepo closure, and only that check differs between them. ok=false
// means the entry is not a resolvable $ref reference at all: not $-prefixed, no
// path segment after the name (`$values`, which upstream resolves to the repo root
// and refuses), an unknown ref name, a traversal out of the ref repository, or a
// remote URL.
func ResolveRefFilePath(
	entry string,
	refSources map[string]ApplicationSource,
	allowedURLSchemes []string,
) (refSource ApplicationSource, relPath string, ok bool) {
	if !strings.HasPrefix(entry, "$") {
		return ApplicationSource{}, "", false
	}

	// "$values/env/prod.yaml" -> ref name "values", remainder "env/prod.yaml".
	refName, remainder, found := strings.Cut(strings.TrimPrefix(entry, "$"), "/")
	if !found || refName == "" {
		return ApplicationSource{}, "", false
	}

	source, found := refSources[refName]
	if !found {
		return ApplicationSource{}, "", false
	}

	rel, remote, err := ResolveHelmFilePath("", remainder, allowedURLSchemes)
	if err != nil || remote {
		return ApplicationSource{}, "", false
	}

	return source, rel, true
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
