package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// argoSecret builds a Secret with the given argocd.argoproj.io/secret-type
// label ("repository" or "repo-creds") and string data, in the argocd
// namespace — the exact shape ArgoCD's secrets backend parses.
func argoSecret(name, secretType string, data map[string]string) *corev1.Secret {
	byteData := make(map[string][]byte, len(data))
	for k, v := range data {
		byteData[k] = []byte(v)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "argocd",
			Labels:    map[string]string{"argocd.argoproj.io/secret-type": secretType},
		},
		Data: byteData,
	}
}

// seededClientset returns a fake clientset with one helm repository, one OCI
// repository, and one credential template of each type.
func seededClientset() *fake.Clientset {
	return fake.NewClientset(
		argoSecret("repo-acme-charts", "repository", map[string]string{
			"name":     "acme-charts",
			"url":      "https://charts.acme.example",
			"username": "helm-user",
			"password": "helm-pass",
			"type":     "helm",
		}),
		argoSecret("repo-acme-oci", "repository", map[string]string{
			"name":      "acme-oci",
			"url":       "ghcr.io/acme",
			"username":  "oci-user",
			"password":  "oci-pass",
			"type":      "oci",
			"enableOCI": "true",
		}),
		argoSecret("creds-acme-team", "repo-creds", map[string]string{
			"url":      "https://charts.acme.example/team",
			"username": "tpl-user",
			"password": "tpl-pass",
			"type":     "helm",
		}),
		argoSecret("creds-acme-registry", "repo-creds", map[string]string{
			"url":      "registry.acme.example",
			"username": "ocitpl-user",
			"password": "ocitpl-pass",
			"type":     "oci",
		}),
	)
}

func TestLoadRepoCredentials_MapsAllFourLists(t *testing.T) {
	creds, err := loadRepoCredentials(context.Background(), seededClientset(), "argocd")
	if err != nil {
		t.Fatalf("loadRepoCredentials() error: %v", err)
	}

	if len(creds.HelmRepos) != 1 || creds.HelmRepos[0].Repo != "https://charts.acme.example" {
		t.Errorf("HelmRepos = %+v, want the acme-charts helm repository", creds.HelmRepos)
	}
	if creds.HelmRepos[0].Username != "helm-user" || creds.HelmRepos[0].Password != "helm-pass" {
		t.Errorf("HelmRepos[0] creds = %q/%q, want helm-user/helm-pass",
			creds.HelmRepos[0].Username, creds.HelmRepos[0].Password)
	}
	if len(creds.OCIRepos) != 1 || creds.OCIRepos[0].Repo != "ghcr.io/acme" || !creds.OCIRepos[0].EnableOCI {
		t.Errorf("OCIRepos = %+v, want the ghcr.io/acme OCI repository with EnableOCI", creds.OCIRepos)
	}
	if len(creds.HelmRepoCreds) != 1 || creds.HelmRepoCreds[0].URL != "https://charts.acme.example/team" {
		t.Errorf("HelmRepoCreds = %+v, want the /team helm credential template", creds.HelmRepoCreds)
	}
	if len(creds.OCIRepoCreds) != 1 || creds.OCIRepoCreds[0].URL != "registry.acme.example" {
		t.Errorf("OCIRepoCreds = %+v, want the registry.acme.example OCI credential template", creds.OCIRepoCreds)
	}
}

func TestLoadRepoCredentials_EmptyClusterIsNotAnError(t *testing.T) {
	creds, err := loadRepoCredentials(context.Background(), fake.NewClientset(), "argocd")
	if err != nil {
		t.Fatalf("loadRepoCredentials() on an empty cluster: %v", err)
	}
	if len(creds.HelmRepos)+len(creds.OCIRepos)+len(creds.HelmRepoCreds)+len(creds.OCIRepoCreds) != 0 {
		t.Errorf("expected empty lists, got %+v", creds)
	}
}

func TestLoadRepoCredentials_Resolve(t *testing.T) {
	ctx := context.Background()
	creds, err := loadRepoCredentials(ctx, seededClientset(), "argocd")
	if err != nil {
		t.Fatalf("loadRepoCredentials() error: %v", err)
	}

	t.Run("exact repository match wins", func(t *testing.T) {
		repo, err := creds.Resolve(ctx, "https://charts.acme.example", "")
		if err != nil {
			t.Fatalf("Resolve() error: %v", err)
		}
		if repo.Username != "helm-user" {
			t.Errorf("Resolve() username = %q, want helm-user", repo.Username)
		}
	})

	t.Run("credential template matches by prefix when no exact repo exists", func(t *testing.T) {
		repo, err := creds.Resolve(ctx, "https://charts.acme.example/team/app", "")
		if err != nil {
			t.Fatalf("Resolve() error: %v", err)
		}
		if repo.Username != "tpl-user" {
			t.Errorf("Resolve() username = %q, want tpl-user (prefix template)", repo.Username)
		}
	})

	t.Run("unknown URL yields a credential-less default, never nil", func(t *testing.T) {
		repo, err := creds.Resolve(ctx, "https://unknown.example", "")
		if err != nil {
			t.Fatalf("Resolve() error: %v", err)
		}
		if repo == nil {
			t.Fatal("Resolve() returned nil repository")
		}
		if repo.Repo != "https://unknown.example" || repo.Username != "" || repo.Password != "" {
			t.Errorf("Resolve() = %+v, want credential-less default for the URL", repo)
		}
	})

	t.Run("each call returns an independent copy", func(t *testing.T) {
		first, err := creds.Resolve(ctx, "https://charts.acme.example", "")
		if err != nil {
			t.Fatalf("Resolve() error: %v", err)
		}
		first.Username = "mutated"
		second, err := creds.Resolve(ctx, "https://charts.acme.example", "")
		if err != nil {
			t.Fatalf("Resolve() error: %v", err)
		}
		if second.Username != "helm-user" {
			t.Errorf("memoized Resolve() leaked a mutation: username = %q", second.Username)
		}
	})
}

func TestLoadRepoCredentials_ForbiddenFailsFast(t *testing.T) {
	clientset := fake.NewClientset()
	clientset.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Resource: "secrets"}, "", errors.New("RBAC denied"))
	})

	_, err := loadRepoCredentials(context.Background(), clientset, "argocd")
	if err == nil {
		t.Fatal("loadRepoCredentials() = nil error, want Forbidden from the preflight probe")
	}
	if !strings.Contains(err.Error(), "secrets access check") {
		t.Errorf("error %q does not identify the preflight probe", err)
	}
	if !apierrors.IsForbidden(errors.Unwrap(err)) && !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("error %q does not carry the Forbidden cause", err)
	}
}
