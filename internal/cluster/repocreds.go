package cluster

import (
	"context"
	"fmt"
	"sync"
	"time"

	argoappv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v3/util/db"
	"github.com/argoproj/argo-cd/v3/util/settings"
	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// repoCredsPreflightTimeout bounds the direct secrets-List probe that runs
	// before ArgoCD's settings informers are started.
	repoCredsPreflightTimeout = 10 * time.Second
	// repoCredsSyncTimeout bounds the settings manager's informer cache sync.
	// WaitForCacheSync (util/settings) has no internal timeout, so without
	// this bound a slow or misbehaving API server could block indefinitely.
	repoCredsSyncTimeout = 30 * time.Second
)

// RepoCredentials carries ArgoCD repository credentials into rendering. The
// four lists are kept separate (helm vs OCI) so the argocd render engine can
// compose them per source with ArgoCD's own IsOCI gate, exactly as the
// application controller does (controller/state.go).
type RepoCredentials struct {
	HelmRepos     []*argoappv1.Repository // repository secrets with type: helm
	OCIRepos      []*argoappv1.Repository // repository secrets with type: oci
	HelmRepoCreds []*argoappv1.RepoCreds  // repo-creds templates with type: helm
	OCIRepoCreds  []*argoappv1.RepoCreds  // repo-creds templates with type: oci

	// Resolve returns the Repository configured for repoURL (exact-match
	// repository secret, then credential-template prefix fallback — ArgoCD's
	// db.GetRepository). It never returns nil on success: unknown URLs yield
	// a credential-less default Repository. Each call returns a fresh copy,
	// safe to mutate and to hand to GenerateManifests.
	Resolve func(ctx context.Context, repoURL, project string) (*argoappv1.Repository, error)
}

// LoadRepoCredentials reads ArgoCD's repository secrets and credential
// templates from the given control-plane namespace using ArgoCD's own util/db
// machinery — the same code the repo-server and application controller use.
//
// RBAC: list,watch on Secrets and ConfigMaps in the namespace. The settings
// manager's secrets informer is unfiltered, so it lists ALL secrets there
// (same as ArgoCD's own components).
//
// Any failure returns (nil, err); the caller decides how loud to be (fatal in
// --repo-creds=cluster mode). A cluster without repository secrets is NOT a
// failure — the lists come back empty and rendering proceeds credential-less.
func (c *Client) LoadRepoCredentials(ctx context.Context, namespace string) (*RepoCredentials, error) {
	return loadRepoCredentials(ctx, c.clientset, namespace)
}

// loadRepoCredentials is the clientset-generic core of LoadRepoCredentials,
// separated so tests can drive it with a fake clientset.
func loadRepoCredentials(ctx context.Context, clientset kubernetes.Interface, namespace string) (*RepoCredentials, error) {
	// ArgoCD's settings manager logs through the process-global logrus logger
	// at Info level ("Starting configmap/secret informers", ...). argocdf does
	// not use logrus itself; keep only errors. NewArgoCDRenderer sets the same
	// level, but it is constructed after credentials are loaded.
	logrus.SetLevel(logrus.ErrorLevel)

	// Preflight: the settings manager's first use lazily starts secret and
	// configmap informers and waits for their caches with NO internal timeout
	// — with forbidden RBAC that wait blocks until its context dies. A direct
	// one-item List fails fast with the real (Forbidden/NotFound/timeout)
	// error instead.
	preflightCtx, cancelPreflight := context.WithTimeout(ctx, repoCredsPreflightTimeout)
	defer cancelPreflight()
	if _, err := clientset.CoreV1().Secrets(namespace).List(preflightCtx, metav1.ListOptions{Limit: 1}); err != nil {
		return nil, fmt.Errorf("secrets access check in namespace %q failed: %w", namespace, err)
	}

	// The manager context bounds the informer cache sync and stops the
	// informer goroutines when this function returns: after a successful
	// sync the indexer keeps serving reads from its (frozen) snapshot, which
	// is exactly right for a one-shot CLI run — including Resolve calls for
	// child apps discovered later during apps-of-apps recursion.
	mgrCtx, cancelMgr := context.WithTimeout(ctx, repoCredsSyncTimeout)
	defer cancelMgr()
	settingsMgr := settings.NewSettingsManager(mgrCtx, clientset, namespace)
	argoDB := db.NewDB(namespace, settingsMgr, clientset)

	helmRepos, err := argoDB.ListHelmRepositories(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list helm repositories: %w", err)
	}
	ociRepos, err := argoDB.ListOCIRepositories(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list OCI repositories: %w", err)
	}
	helmRepoCreds, err := argoDB.GetAllHelmRepositoryCredentials(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list helm repository credentials: %w", err)
	}
	ociRepoCreds, err := argoDB.GetAllOCIRepositoryCredentials(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list OCI repository credentials: %w", err)
	}

	// Memoize per (url, project): renders are concurrent and child apps may
	// resolve the same repo many times. The memo stores the original; every
	// caller gets a DeepCopy because GenerateManifests mutates its request.
	type resolveKey struct{ url, project string }
	var (
		mu   sync.Mutex
		memo = make(map[resolveKey]*argoappv1.Repository)
	)
	resolve := func(ctx context.Context, repoURL, project string) (*argoappv1.Repository, error) {
		key := resolveKey{url: repoURL, project: project}
		mu.Lock()
		cached, ok := memo[key]
		mu.Unlock()
		if ok {
			return cached.DeepCopy(), nil
		}
		repo, err := argoDB.GetRepository(ctx, repoURL, project)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		memo[key] = repo
		mu.Unlock()
		return repo.DeepCopy(), nil
	}

	return &RepoCredentials{
		HelmRepos:     helmRepos,
		OCIRepos:      ociRepos,
		HelmRepoCreds: helmRepoCreds,
		OCIRepoCreds:  ociRepoCreds,
		Resolve:       resolve,
	}, nil
}
