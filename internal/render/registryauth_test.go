package render

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	argoappv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

// readAuthConfig parses the auth file into its raw JSON shape so tests can
// assert on exactly what helm children will read.
func readAuthConfig(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("auth file is not valid JSON: %v\n%s", err, data)
	}
	return raw
}

func readAuths(t *testing.T, path string) map[string]struct {
	Auth string `json:"auth"`
} {
	t.Helper()
	raw := readAuthConfig(t, path)
	var auths map[string]struct {
		Auth string `json:"auth"`
	}
	if err := json.Unmarshal(raw["auths"], &auths); err != nil {
		t.Fatalf("auths section: %v", err)
	}
	return auths
}

func TestNewRegistryAuthFile(t *testing.T) {
	f, err := newRegistryAuthFile()
	if err != nil {
		t.Fatalf("newRegistryAuthFile() error = %v", err)
	}
	defer f.Remove()

	info, err := os.Stat(f.path)
	if err != nil {
		t.Fatalf("auth file not created: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("auth file permissions = %o, want 0600 (it will hold tokens)", perm)
	}

	raw := readAuthConfig(t, f.path)
	// The whole point of the file: helm's ORAS credential store must treat it
	// as "configured" (placeholder entry present) and must never be told to
	// use a native credential store.
	if _, ok := raw["credsStore"]; ok {
		t.Error("auth file contains credsStore — logins would hit the system keychain")
	}
	auths := readAuths(t, f.path)
	if _, ok := auths[detectionBlocker]; !ok {
		t.Errorf("auth file lacks the %s placeholder that disables ORAS native-store detection; auths = %v", detectionBlocker, auths)
	}

	f.Remove()
	if _, err := os.Stat(filepath.Dir(f.path)); !os.IsNotExist(err) {
		t.Error("Remove() left the auth directory behind")
	}
}

func TestRegistryAuthFileEnsure(t *testing.T) {
	f, err := newRegistryAuthFile()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Remove()

	if err := f.Ensure("oci://ghcr.io/acme", "bot", "s3cret"); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	auths := readAuths(t, f.path)
	want := base64.StdEncoding.EncodeToString([]byte("bot:s3cret"))
	if auths["ghcr.io"].Auth != want {
		t.Errorf("auths[ghcr.io] = %q, want inline base64 user:pass", auths["ghcr.io"].Auth)
	}

	// The login gate mirror: credential-less repos stay anonymous.
	if err := f.Ensure("ghcr.io/other", "user-only", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.Ensure("ghcr.io/other", "", "pass-only"); err != nil {
		t.Fatal(err)
	}
	if got := len(readAuths(t, f.path)); got != 2 { // placeholder + ghcr.io
		t.Errorf("partial credentials were recorded; auths = %v", readAuths(t, f.path))
	}

	// Same-host overwrite: last writer wins (documented host-granularity).
	if err := f.Ensure("ghcr.io/acme2", "bot2", "s3cret2"); err != nil {
		t.Fatal(err)
	}
	want2 := base64.StdEncoding.EncodeToString([]byte("bot2:s3cret2"))
	if got := readAuths(t, f.path)["ghcr.io"].Auth; got != want2 {
		t.Errorf("auths[ghcr.io] = %q after overwrite, want %q", got, want2)
	}
}

func TestRegistryAuthFileEnsureConcurrent(t *testing.T) {
	f, err := newRegistryAuthFile()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Remove()

	var wg sync.WaitGroup
	hosts := []string{"a.example", "b.example", "c.example", "d.example"}
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = f.Ensure(hosts[i%len(hosts)]+"/org", "u", "p")
		}(i)
	}
	wg.Wait()

	auths := readAuths(t, f.path) // also asserts the JSON is never torn
	for _, h := range hosts {
		if auths[h].Auth == "" {
			t.Errorf("auths[%s] missing after concurrent Ensure", h)
		}
	}
}

func TestRegistryAuthKeys(t *testing.T) {
	tests := []struct {
		url  string
		want []string
	}{
		{"oci://ghcr.io/acme", []string{"ghcr.io"}},
		{"ghcr.io/acme/sub", []string{"ghcr.io"}},
		{"ghcr.io", []string{"ghcr.io"}},
		{"https://charts.example.com/repo", []string{"charts.example.com"}},
		{"registry.example.com:5000/acme", []string{"registry.example.com:5000"}},
		{"oci://docker.io/acme", []string{"docker.io", "index.docker.io", "registry-1.docker.io", "https://index.docker.io/v1/"}},
		// URL userinfo is not part of the docker-config key.
		{"https://user:pass@ghcr.io/acme", []string{"ghcr.io"}},
		{"oci://token@registry.example.com/acme", []string{"registry.example.com"}},
		// Pathological URLs yield no keys instead of auths[""].
		{"", nil},
		{"oci://", nil},
	}
	for _, tt := range tests {
		if got := registryAuthKeys(tt.url); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("registryAuthKeys(%q) = %v, want %v", tt.url, got, tt.want)
		}
	}
}

func TestSeedAndStripRepos(t *testing.T) {
	f, err := newRegistryAuthFile()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Remove()

	ociRepo := &argoappv1.Repository{
		Repo: "ghcr.io/acme", Name: "acme", Type: "helm", EnableOCI: true,
		Username: "bot", Password: "tok",
	}
	classic := &argoappv1.Repository{
		Repo: "https://charts.example.com", Name: "classic", Type: "helm",
		Username: "cu", Password: "cp",
	}
	typedOCI := &argoappv1.Repository{
		Repo: "oci://registry.example.com/other", Type: "oci",
		Username: "ou", Password: "op",
	}
	credless := &argoappv1.Repository{Repo: "ghcr.io/public", Type: "helm", EnableOCI: true}

	in := []*argoappv1.Repository{ociRepo, classic, typedOCI, credless}
	out, err := seedAndStripRepos(f, in)
	if err != nil {
		t.Fatalf("seedAndStripRepos() error = %v", err)
	}

	// OCI-flavored entries: stripped in the output, credentials seeded into
	// the file, original untouched.
	if out[0].Username != "" || out[0].Password != "" {
		t.Error("enableOCI repo kept its credentials — DependencyBuild would exec `helm registry login`")
	}
	if out[0].Repo != ociRepo.Repo || out[0].Name != ociRepo.Name || !out[0].EnableOCI {
		t.Errorf("stripped repo lost non-credential fields: %+v", out[0])
	}
	if ociRepo.Username != "bot" || ociRepo.Password != "tok" {
		t.Error("seedAndStripRepos mutated the shared input entry")
	}
	if out[2].Username != "" || out[2].Password != "" {
		t.Error("type=oci repo kept its credentials")
	}

	// Classic and credential-less entries pass through unchanged (same pointer).
	if out[1] != classic {
		t.Error("classic repository was copied/stripped — its `helm repo add` credentials must survive")
	}
	if out[3] != credless {
		t.Error("credential-less repo was copied for no reason")
	}

	auths := readAuths(t, f.path)
	if auths["ghcr.io"].Auth != base64.StdEncoding.EncodeToString([]byte("bot:tok")) {
		t.Errorf("ghcr.io auth not seeded: %v", auths)
	}
	if _, ok := auths["charts.example.com"]; ok {
		t.Error("classic repository credentials leaked into the registry auth file")
	}
}

func TestSeedAndStripRepoCreds(t *testing.T) {
	f, err := newRegistryAuthFile()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Remove()

	ociTemplate := &argoappv1.RepoCreds{URL: "ghcr.io/acme", EnableOCI: true, Username: "tu", Password: "tp"}
	classicTemplate := &argoappv1.RepoCreds{URL: "https://charts.example.com", Username: "cu", Password: "cp"}

	out, err := seedAndStripRepoCreds(f, []*argoappv1.RepoCreds{ociTemplate, classicTemplate})
	if err != nil {
		t.Fatalf("seedAndStripRepoCreds() error = %v", err)
	}
	if out[0].Username != "" || out[0].Password != "" {
		t.Error("OCI credential template kept its credentials")
	}
	if ociTemplate.Username != "tu" {
		t.Error("seedAndStripRepoCreds mutated the shared input entry")
	}
	if out[1] != classicTemplate {
		t.Error("classic credential template was copied/stripped")
	}
	if got := readAuths(t, f.path)["ghcr.io"].Auth; got != base64.StdEncoding.EncodeToString([]byte("tu:tp")) {
		t.Errorf("template credentials not seeded: %q", got)
	}
}

func TestIsolateHelmEnv(t *testing.T) {
	for _, v := range inheritedHelmEnvVars {
		t.Setenv(v, "/inherited/"+strings.ToLower(v))
	}

	isolateHelmEnv("/run/config.json")

	for _, v := range inheritedHelmEnvVars {
		if v == "HELM_REGISTRY_CONFIG" {
			continue
		}
		if got, set := os.LookupEnv(v); set {
			t.Errorf("%s survived isolation as %q — a helm child would resolve it over ArgoCD's appended temp home (first duplicate wins)", v, got)
		}
	}
	if got := os.Getenv("HELM_REGISTRY_CONFIG"); got != "/run/config.json" {
		t.Errorf("HELM_REGISTRY_CONFIG = %q, want the explicit config", got)
	}

	isolateHelmEnv("")
	if got, set := os.LookupEnv("HELM_REGISTRY_CONFIG"); set {
		t.Errorf("HELM_REGISTRY_CONFIG = %q after isolation without an explicit config, want unset", got)
	}
}
