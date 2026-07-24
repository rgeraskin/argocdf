package helmconfig

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

const fixtureRepositoriesYAML = `apiVersion: ""
generated: "0001-01-01T00:00:00Z"
repositories:
- name: acme
  url: https://charts.acme.example
  username: repo-user
  password: repo-pass
- name: insecure-acme
  url: https://insecure.acme.example/
  insecure_skip_tls_verify: true
  pass_credentials_all: true
- name: public
  url: https://public.example/charts
`

func writeFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "repositories.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write fixture: %v", err)
	}
	return path
}

func TestParseRepositoriesFile(t *testing.T) {
	repos, err := parseRepositoriesFile(writeFixture(t, fixtureRepositoriesYAML))
	if err != nil {
		t.Fatalf("parseRepositoriesFile() error: %v", err)
	}
	if len(repos) != 3 {
		t.Fatalf("parseRepositoriesFile() = %d repos, want 3", len(repos))
	}

	acme := repos[0]
	if acme.Name != "acme" || acme.Repo != "https://charts.acme.example" ||
		acme.Username != "repo-user" || acme.Password != "repo-pass" || acme.Type != "helm" {
		t.Errorf("first entry mapped wrong: %+v", acme)
	}
	insecure := repos[1]
	if !insecure.Insecure {
		t.Errorf("insecure_skip_tls_verify not mapped: %+v", insecure)
	}
	public := repos[2]
	if public.Username != "" || public.Password != "" {
		t.Errorf("credential-less entry grew credentials: %+v", public)
	}
}

func TestParseRepositoriesFile_MissingIsEmpty(t *testing.T) {
	repos, err := parseRepositoriesFile(filepath.Join(t.TempDir(), "nope", "repositories.yaml"))
	if err != nil {
		t.Fatalf("missing repositories.yaml should not be an error, got: %v", err)
	}
	if len(repos) != 0 {
		t.Errorf("missing repositories.yaml should yield no repos, got %d", len(repos))
	}
}

func TestParseRepositoriesFile_UnparsableIsAnError(t *testing.T) {
	if _, err := parseRepositoriesFile(writeFixture(t, "repositories: {not a list}")); err == nil {
		t.Error("unparsable repositories.yaml should be an error")
	}
}

// TestLoadLocalRepoCredentials drives the full path with explicit HELM_*
// variables, which `helm env` echoes back (an explicit value beats the
// HELM_CONFIG_HOME-derived default). Requires the helm binary, like the rest
// of the render test suite.
func TestLoadLocalRepoCredentials(t *testing.T) {
	repoConfig := writeFixture(t, fixtureRepositoriesYAML)
	registryConfig := filepath.Join(t.TempDir(), "registry", "config.json")
	t.Setenv("HELM_REPOSITORY_CONFIG", repoConfig)
	t.Setenv("HELM_REGISTRY_CONFIG", registryConfig)

	creds, err := LoadLocalRepoCredentials()
	if err != nil {
		t.Fatalf("LoadLocalRepoCredentials() error: %v", err)
	}

	if len(creds.HelmRepos) != 3 {
		t.Errorf("HelmRepos = %d entries, want 3", len(creds.HelmRepos))
	}
	if len(creds.OCIRepos)+len(creds.HelmRepoCreds)+len(creds.OCIRepoCreds) != 0 {
		t.Errorf("local mode must leave the OCI/template lists empty, got %+v", creds)
	}
	if got := os.Getenv("HELM_REGISTRY_CONFIG"); got != registryConfig {
		t.Errorf("HELM_REGISTRY_CONFIG = %q, want the resolved path %q", got, registryConfig)
	}

	ctx := context.Background()
	t.Run("resolve matches with URL normalization", func(t *testing.T) {
		// Trailing slash and case differences must not break the match.
		repo, err := creds.Resolve(ctx, "https://CHARTS.acme.example/", "")
		if err != nil {
			t.Fatalf("Resolve() error: %v", err)
		}
		if repo.Username != "repo-user" {
			t.Errorf("Resolve() username = %q, want repo-user", repo.Username)
		}
	})
	t.Run("resolve yields credential-less default for unknown URLs", func(t *testing.T) {
		repo, err := creds.Resolve(ctx, "https://unknown.example", "")
		if err != nil {
			t.Fatalf("Resolve() error: %v", err)
		}
		if repo.Repo != "https://unknown.example" || repo.Username != "" {
			t.Errorf("Resolve() = %+v, want credential-less default", repo)
		}
	})
	t.Run("resolve returns independent copies", func(t *testing.T) {
		first, _ := creds.Resolve(ctx, "https://charts.acme.example", "")
		first.Password = "mutated"
		second, _ := creds.Resolve(ctx, "https://charts.acme.example", "")
		if second.Password != "repo-pass" {
			t.Errorf("Resolve() leaked a mutation: password = %q", second.Password)
		}
	})
}

func TestLoadLocalRepoCredentials_HelmEnvFailureIsFatal(t *testing.T) {
	// No helm binary on PATH: `helm env` cannot run, and local mode must fail
	// loudly rather than silently render without credentials.
	t.Setenv("PATH", t.TempDir())
	if _, err := LoadLocalRepoCredentials(); err == nil {
		t.Error("LoadLocalRepoCredentials() = nil error, want a failure when helm env cannot run")
	}
}

func TestParseRepositoriesFile_TLSClientFiles(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "client.crt")
	keyPath := filepath.Join(dir, "client.key")
	if err := os.WriteFile(certPath, []byte("CERT-DATA"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("KEY-DATA"), 0o600); err != nil {
		t.Fatal(err)
	}

	repos, err := parseRepositoriesFile(writeFixture(t,
		"repositories:\n- name: mtls\n  url: https://mtls.acme.example\n  certFile: "+certPath+"\n  keyFile: "+keyPath+"\n"))
	if err != nil {
		t.Fatalf("parseRepositoriesFile() error: %v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("got %d repos, want 1", len(repos))
	}
	if repos[0].TLSClientCertData != "CERT-DATA" || repos[0].TLSClientCertKey != "KEY-DATA" {
		t.Errorf("TLS client material not read from files: %+v", repos[0])
	}
}

func TestParseRepositoriesFile_UnreadableTLSFileIsAnError(t *testing.T) {
	repos, err := parseRepositoriesFile(writeFixture(t,
		"repositories:\n- name: mtls\n  url: https://mtls.acme.example\n  certFile: /nonexistent/client.crt\n"))
	if err == nil {
		t.Errorf("configured-but-unreadable certFile must be an error, got repos: %+v", repos)
	}
}
