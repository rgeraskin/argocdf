// Package helmconfig sources repository credentials from the user's local
// helm configuration, for --repo-creds=local. It produces the same
// RepoCredentials shape as the cluster source, so rendering is mode-blind:
// classic repository entries (whose credentials helm always stores inline in
// repositories.yaml) feed the same lists and Resolve seam, while OCI
// credentials ride the HELM_REGISTRY_CONFIG environment variable so helm
// itself reads its own registry config — including credential helpers
// (e.g. the macOS keychain), which no parsing approach could serve.
package helmconfig

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	argoappv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"gopkg.in/yaml.v3"

	"github.com/rgeraskin/argocdf/internal/cluster"
)

// repoFile mirrors helm's repositories.yaml schema (helm.sh/helm/v3/pkg/repo
// Entry), stable since helm 3.0. TLS client material is stored as file PATHS
// there (certFile/keyFile) while ArgoCD's Repository wants the DATA, so the
// files are read at load time. caFile has no Repository equivalent (ArgoCD
// serves CAs from its certificate database, which a CLI run does not have),
// and pass_credentials_all maps to nothing (ArgoCD's --pass-credentials comes
// from spec.source.helm.passCredentials).
type repoFile struct {
	Repositories []repoEntry `yaml:"repositories"`
}

type repoEntry struct {
	Name                  string `yaml:"name"`
	URL                   string `yaml:"url"`
	Username              string `yaml:"username"`
	Password              string `yaml:"password"`
	CertFile              string `yaml:"certFile"`
	KeyFile               string `yaml:"keyFile"`
	InsecureSkipTLSVerify bool   `yaml:"insecure_skip_tls_verify"`
}

// LoadLocalRepoCredentials builds RepoCredentials from the user's helm
// config. It resolves the config paths the way helm does (`helm env`, which
// honors the user's own HELM_* variables), parses repositories.yaml into
// Repository entries, and points HELM_REGISTRY_CONFIG at the user's registry
// config so every later helm exec — ArgoCD's GenerateManifests and argocdf's
// chart pulls, all built on os.Environ() — authenticates OCI operations with
// the user's own logins. An explicit HELM_REGISTRY_CONFIG beats the isolated
// HELM_CONFIG_HOME-derived default, so this pierces the isolated helm homes
// without giving up any of their write isolation: the registry config is only
// ever read in this flow.
//
// A missing repositories.yaml is not an error (empty lists); an unreadable
// one, or a failing `helm env`, is — the caller treats it as fatal.
//
// NOTE: setting HELM_REGISTRY_CONFIG is a deliberate PROCESS-GLOBAL side
// effect — acceptable for a one-shot CLI where every subsequent helm exec
// should see the user's registry config, but call this once, at startup,
// before renders run.
func LoadLocalRepoCredentials() (*cluster.RepoCredentials, error) {
	registryConfigPath, err := helmEnv("HELM_REGISTRY_CONFIG")
	if err != nil {
		return nil, err
	}
	if err := os.Setenv("HELM_REGISTRY_CONFIG", registryConfigPath); err != nil {
		return nil, fmt.Errorf("failed to set HELM_REGISTRY_CONFIG: %w", err)
	}

	repoConfigPath, err := helmEnv("HELM_REPOSITORY_CONFIG")
	if err != nil {
		return nil, err
	}
	repos, err := parseRepositoriesFile(repoConfigPath)
	if err != nil {
		return nil, err
	}

	resolve := func(_ context.Context, repoURL, _ string) (*argoappv1.Repository, error) {
		for _, repo := range repos {
			if normalizeRepoURL(repo.Repo) == normalizeRepoURL(repoURL) {
				return repo.DeepCopy(), nil
			}
		}
		// Same contract as ArgoCD's db.GetRepository: unknown URLs yield a
		// credential-less default, never nil.
		return &argoappv1.Repository{Repo: repoURL}, nil
	}

	return &cluster.RepoCredentials{
		HelmRepos: repos,
		Resolve:   resolve,
	}, nil
}

// helmEnv returns helm's resolved value for one of its environment variables
// (e.g. `helm env HELM_REGISTRY_CONFIG` prints the effective path, honoring
// the user's HELM_CONFIG_HOME / explicit overrides).
func helmEnv(name string) (string, error) {
	out, err := exec.Command("helm", "env", name).Output()
	if err != nil {
		return "", fmt.Errorf("helm env %s failed: %w", name, err)
	}
	value := strings.TrimSpace(string(out))
	if value == "" {
		return "", fmt.Errorf("helm env %s returned an empty value", name)
	}
	return value, nil
}

// parseRepositoriesFile maps repositories.yaml entries onto ArgoCD Repository
// values. A missing file means "no repos configured" (empty result, nil
// error); anything else unreadable or unparsable is an error.
func parseRepositoriesFile(path string) ([]*argoappv1.Repository, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read helm repository config %s: %w", path, err)
	}

	var file repoFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("failed to parse helm repository config %s: %w", path, err)
	}

	repos := make([]*argoappv1.Repository, 0, len(file.Repositories))
	for _, entry := range file.Repositories {
		if entry.URL == "" {
			continue
		}
		certData, err := readOptionalFile(entry.CertFile)
		if err != nil {
			return nil, fmt.Errorf("repository %q: %w", entry.Name, err)
		}
		keyData, err := readOptionalFile(entry.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("repository %q: %w", entry.Name, err)
		}
		repos = append(repos, &argoappv1.Repository{
			Name:              entry.Name,
			Repo:              entry.URL,
			Username:          entry.Username,
			Password:          entry.Password,
			TLSClientCertData: certData,
			TLSClientCertKey:  keyData,
			Insecure:          entry.InsecureSkipTLSVerify,
			Type:              "helm",
		})
	}
	return repos, nil
}

// readOptionalFile reads a repositories.yaml-referenced TLS file. An empty
// path yields empty data; a configured-but-unreadable file is an error (a
// silently dropped client certificate would surface as a misleading TLS
// handshake failure).
func readOptionalFile(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("failed to read TLS file %s: %w", path, err)
	}
	return string(data), nil
}

// normalizeRepoURL makes repository URL comparison tolerant of trailing
// slashes and case, the two spurious-mismatch sources for chart repo URLs.
func normalizeRepoURL(u string) string {
	return strings.TrimRight(strings.ToLower(u), "/")
}
