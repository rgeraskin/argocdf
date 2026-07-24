package render

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	argoappv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

// registryAuthFile is an argocdf-owned helm registry config (docker config
// JSON) that carries OCI registry credentials INTO ArgoCD's helm executions
// without ever running `helm registry login`.
//
// Why it exists: helm's credential store (ORAS) runs platform native-store
// detection — when the effective registry config has no auth configured and a
// helper binary like docker-credential-osxkeychain is in PATH, `helm registry
// login` writes to the SHARED system keychain instead of the config file.
// ArgoCD's per-command temp helm homes (built for Linux repo-server
// containers, where no helper exists) therefore do not isolate credentials on
// macOS at all: concurrent renders' login→build→logout cycles race on one
// shared keychain item (errSecDuplicateItem -25299 on colliding logins; 401s
// when one render's deferred logout deletes the credential another render's
// dependency build is using).
//
// The fix inverts the flow: argocdf writes the credentials into this file
// itself (in-process, mutex-serialized, atomic rename), strips
// username/password from every OCI-flavored repository handed to ArgoCD's
// code so its login/logout gates never fire, and points HELM_REGISTRY_CONFIG
// here — an explicitly set registry config beats every helm child's
// config-home-derived default, so all pulls authenticate from this read-only
// (to them) file. That also keeps the token off helm's argv (`helm registry
// login --password ...`) and out of the keychain.
//
// The file always contains a placeholder auth entry: a config with at least
// one entry counts as "configured", which is what disables ORAS native-store
// detection — without it, even credential READS for anonymous pulls would
// consult the user's keychain.
//
// Limitation: docker config keys are registry HOSTS, so two repositories on
// the same host with different credentials collapse to whichever was seeded
// last. Upstream's login/logout flow has the same host-granular limitation.
type registryAuthFile struct {
	path string

	mu    sync.Mutex
	auths map[string]registryAuthEntry
}

type registryAuthEntry struct {
	Auth string `json:"auth,omitempty"`
}

// detectionBlocker is the placeholder entry that keeps the config
// "configured" so ORAS never falls back to a platform native credential
// store. The host is syntactically valid but unresolvable by construction
// (.invalid is reserved, RFC 2606).
const detectionBlocker = "argocdf-seed.invalid"

// newRegistryAuthFile creates the per-run auth file (0600, owner-only) in a
// private temp directory and returns it ready to be pointed at by
// HELM_REGISTRY_CONFIG.
func newRegistryAuthFile() (*registryAuthFile, error) {
	dir, err := os.MkdirTemp("", "argocdf-registry-")
	if err != nil {
		return nil, fmt.Errorf("failed to create registry config dir: %w", err)
	}
	f := &registryAuthFile{
		path:  filepath.Join(dir, "config.json"),
		auths: map[string]registryAuthEntry{detectionBlocker: {}},
	}
	if err := f.write(); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return f, nil
}

// Ensure records credentials for the registry hosting repoURL. It mirrors
// ArgoCD's login gate exactly — both username and password non-empty —
// so credential-less repositories degrade to anonymous pulls, the same as
// upstream skipping `helm registry login`. Safe for concurrent use; helm
// children only ever read the file, and writes are atomic (temp + rename),
// so a concurrent reader sees the old or the new config, never a torn one.
func (f *registryAuthFile) Ensure(repoURL, username, password string) error {
	if username == "" || password == "" {
		return nil
	}
	auth := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))

	f.mu.Lock()
	defer f.mu.Unlock()
	changed := false
	for _, key := range registryAuthKeys(repoURL) {
		if f.auths[key].Auth != auth {
			f.auths[key] = registryAuthEntry{Auth: auth}
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return f.write()
}

// write persists the current auth map atomically. Callers hold f.mu (or, at
// construction, exclusive ownership).
func (f *registryAuthFile) write() error {
	data, err := json.Marshal(struct {
		Auths map[string]registryAuthEntry `json:"auths"`
	}{Auths: f.auths})
	if err != nil {
		return fmt.Errorf("failed to encode registry config: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.path), "config-*.json.tmp")
	if err != nil {
		return fmt.Errorf("failed to stage registry config: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("failed to restrict registry config permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("failed to write registry config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("failed to write registry config: %w", err)
	}
	if err := os.Rename(tmp.Name(), f.path); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("failed to publish registry config: %w", err)
	}
	return nil
}

// Remove deletes the auth file and its directory. Best-effort: the file holds
// short-lived tokens, so it should not outlive the run, but a leftover in the
// OS temp dir (0600) is not fatal.
func (f *registryAuthFile) Remove() {
	_ = os.RemoveAll(filepath.Dir(f.path))
}

// inheritedHelmEnvVars are the helm environment variables scrubbed from
// argocdf's own environment before ArgoCD's render code runs. ArgoCD
// isolates its helm executions by APPENDING per-command temp-home variables
// to os.Environ() (util/helm/cmd.go), but Go programs — helm included —
// resolve the FIRST occurrence of a duplicated environment key, so any
// inherited HELM_CONFIG_HOME silently defeats that isolation; and
// HELM_REGISTRY_CONFIG / HELM_REPOSITORY_* are never appended at all, so an
// inherited value redirects registry auth and repository state into the
// user's files unconditionally. The Linux repo-server never has these set;
// a developer machine or CI script often does.
//
// XDG_* variables are deliberately left alone: helm's config resolution is
// fully covered by the appended HELM_CONFIG_HOME once the HELM_* overrides
// are gone, and scrubbing XDG_CONFIG_HOME would change git's config lookup
// for every other child process argocdf spawns.
var inheritedHelmEnvVars = []string{
	"HELM_CONFIG_HOME",
	"HELM_CACHE_HOME",
	"HELM_DATA_HOME",
	"HELM_REPOSITORY_CONFIG",
	"HELM_REPOSITORY_CACHE",
	"HELM_REGISTRY_CONFIG",
	"HELM_PLUGINS",
}

// isolateHelmEnv scrubs the inherited helm environment and installs
// registryConfig (when non-empty) as the process-wide HELM_REGISTRY_CONFIG —
// either argocdf's own auth file, or the user's registry config under
// --repo-creds=local (the documented read-only piercing of the isolated
// helm homes).
func isolateHelmEnv(registryConfig string) {
	for _, v := range inheritedHelmEnvVars {
		_ = os.Unsetenv(v)
	}
	if registryConfig != "" {
		_ = os.Setenv("HELM_REGISTRY_CONFIG", registryConfig)
	}
}

// isOCIRepoEntry reports whether a repository entry is OCI-flavored — the
// entries whose credentials would trigger `helm registry login` inside
// ArgoCD's DependencyBuild or chart client.
func isOCIRepoEntry(enableOCI bool, repoType string) bool {
	return enableOCI || repoType == "oci"
}

// seedAndStripRepos records every OCI-flavored repository's credentials in
// the auth file and returns a list where those entries carry no
// username/password, so ArgoCD's login/logout gates never fire and pulls
// authenticate from the file instead. Non-OCI entries pass through
// untouched: classic repositories are added via `helm repo add
// --username/--password` and never touch the registry config. Stripped
// entries are clones; shared inputs are never mutated.
//
// Note the OCI login's TLS flags (--ca-file/--cert-file) have no file
// equivalent, but they only guarded the login handshake — `helm dependency
// build` has no per-registry TLS flags for the pull either, upstream
// included.
func seedAndStripRepos(auth *registryAuthFile, repos []*argoappv1.Repository) ([]*argoappv1.Repository, error) {
	out := make([]*argoappv1.Repository, len(repos))
	for i, repo := range repos {
		if repo == nil || !isOCIRepoEntry(repo.EnableOCI, repo.Type) || repo.Username == "" || repo.Password == "" {
			out[i] = repo
			continue
		}
		if err := auth.Ensure(repo.Repo, repo.Username, repo.Password); err != nil {
			return nil, fmt.Errorf("failed to record registry credentials for %s: %w", repo.Repo, err)
		}
		stripped := repo.DeepCopy()
		stripped.Username = ""
		stripped.Password = ""
		out[i] = stripped
	}
	return out, nil
}

// seedAndStripRepoCreds is seedAndStripRepos for credential templates
// (RepoCreds), which ArgoCD consults as the fallback for dependencies that
// match no repository entry (getRepoCredential's prefix match).
func seedAndStripRepoCreds(auth *registryAuthFile, creds []*argoappv1.RepoCreds) ([]*argoappv1.RepoCreds, error) {
	out := make([]*argoappv1.RepoCreds, len(creds))
	for i, cred := range creds {
		if cred == nil || !isOCIRepoEntry(cred.EnableOCI, cred.Type) || cred.Username == "" || cred.Password == "" {
			out[i] = cred
			continue
		}
		if err := auth.Ensure(cred.URL, cred.Username, cred.Password); err != nil {
			return nil, fmt.Errorf("failed to record registry credentials for %s: %w", cred.URL, err)
		}
		stripped := cred.DeepCopy()
		stripped.Username = ""
		stripped.Password = ""
		out[i] = stripped
	}
	return out, nil
}

// registryAuthKeys returns the docker-config auth keys for a repository URL.
// Helm resolves credentials by registry host. Docker Hub is special-cased to
// its legacy server address alongside the plain hosts, mirroring how docker
// and ORAS clients store and look up Hub credentials.
func registryAuthKeys(repoURL string) []string {
	host := repoURL
	host = strings.TrimPrefix(host, "oci://")
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	switch host {
	case "docker.io", "index.docker.io", "registry-1.docker.io":
		return []string{"docker.io", "index.docker.io", "registry-1.docker.io", "https://index.docker.io/v1/"}
	}
	return []string{host}
}
