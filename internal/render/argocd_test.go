package render

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	argoappv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	argogit "github.com/argoproj/argo-cd/v3/util/git"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/rgeraskin/argocdf/internal/cluster"
	"github.com/rgeraskin/argocdf/internal/types"
)

// mustNewArgoCDRenderer builds the argocd engine and registers its cleanup
// (the per-run registry auth file). Construction failures are test-fatal.
func mustNewArgoCDRenderer(t *testing.T, opts RenderOptions) *ArgoCDRenderer {
	t.Helper()
	r, err := NewArgoCDRenderer(opts)
	if err != nil {
		t.Fatalf("NewArgoCDRenderer() error = %v", err)
	}
	t.Cleanup(r.Cleanup)
	return r
}

// requireHelm skips the test when the helm binary is unavailable.
func requireHelm(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed, skipping integration test")
	}
}

// requireKustomize skips the test when the kustomize binary is unavailable.
func requireKustomize(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("kustomize"); err != nil {
		t.Skip("kustomize not installed, skipping integration test")
	}
}

// writeTestChart writes a minimal helm chart under dir/<rel> with one
// ConfigMap template, a CRD in crds/, and optional extra files.
func writeArgoTestChart(t *testing.T, dir, rel string, extra map[string]string) string {
	t.Helper()
	chartDir := filepath.Join(dir, rel)
	files := map[string]string{
		"Chart.yaml":  "apiVersion: v2\nname: testchart\nversion: 0.1.0\n",
		"values.yaml": "greeting: hello\n",
		"templates/cm.yaml": `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}-cm
data:
  greeting: {{ .Values.greeting | quote }}
`,
		"crds/testcrd.yaml": `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.example.com
spec:
  group: example.com
  names:
    kind: Widget
    plural: widgets
  scope: Namespaced
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
`,
	}
	for k, v := range extra {
		files[k] = v
	}
	for name, content := range files {
		p := filepath.Join(chartDir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return chartDir
}

// testApp builds an Application with a single path source rooted at rel.
func testApp(name, rel string, helm *cluster.ApplicationSourceHelm) *cluster.Application {
	app := &cluster.Application{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: cluster.ApplicationSpec{
			Source: &cluster.ApplicationSource{
				RepoURL: "https://github.com/example/repo.git",
				Path:    rel,
				Helm:    helm,
			},
			Destination: cluster.ApplicationDestination{Namespace: "default"},
		},
	}
	return app
}

// docsByKindName parses multi-doc YAML into a set of "Kind/name" keys.
func docsByKindName(t *testing.T, manifests []byte) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, doc := range strings.Split(string(manifests), "\n---\n") {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}
		var m struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
			t.Fatalf("unmarshal doc: %v\ndoc:\n%s", err, doc)
		}
		if m.Kind != "" {
			out[m.Kind+"/"+m.Metadata.Name] = true
		}
	}
	return out
}

func TestArgoCDRenderer_LocalHelmChart_IncludesCRDs(t *testing.T) {
	requireHelm(t)

	repoDir := t.TempDir()
	writeArgoTestChart(t, repoDir, "charts/testchart", nil)

	r := mustNewArgoCDRenderer(t, RenderOptions{})
	app := testApp("my-app", "charts/testchart", nil)

	result, err := r.RenderApplication(context.Background(), app, repoDir, "abcdef1234567890")
	if err != nil {
		t.Fatalf("RenderApplication() error = %v", err)
	}
	if result.SourceType != types.SourceTypeHelm {
		t.Errorf("SourceType = %q, want %q", result.SourceType, types.SourceTypeHelm)
	}

	docs := docsByKindName(t, result.Manifests)
	if !docs["ConfigMap/my-app-cm"] {
		t.Errorf("rendered output missing ConfigMap/my-app-cm; got %v", docs)
	}
	// ArgoCD parity: helm template runs with --include-crds, so crds/ content
	// must appear.
	if !docs["CustomResourceDefinition/widgets.example.com"] {
		t.Errorf("rendered output missing CRD from crds/ dir (ArgoCD --include-crds parity); got %v", docs)
	}
}

func TestArgoCDRenderer_SkipSchemaValidation(t *testing.T) {
	requireHelm(t)

	schema := `{"$schema": "https://json-schema.org/draft-07/schema#", "type": "object", "properties": {"greeting": {"type": "integer"}}}`

	tests := []struct {
		name    string
		helm    *cluster.ApplicationSourceHelm
		wantErr bool
	}{
		{
			name:    "violating values fail by default",
			helm:    nil,
			wantErr: true,
		},
		{
			name:    "skipSchemaValidation renders",
			helm:    &cluster.ApplicationSourceHelm{SkipSchemaValidation: true},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoDir := t.TempDir()
			// values.yaml sets greeting to a string; the schema demands integer.
			writeArgoTestChart(t, repoDir, "chart", map[string]string{
				"values.schema.json": schema,
			})

			r := mustNewArgoCDRenderer(t, RenderOptions{})
			app := testApp("schema-app", "chart", tt.helm)

			_, err := r.RenderApplication(context.Background(), app, repoDir, "abcdef1234567890")
			if tt.wantErr && err == nil {
				t.Fatal("RenderApplication() expected schema validation error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("RenderApplication() unexpected error = %v", err)
			}
		})
	}
}

func TestArgoCDRenderer_Kustomize(t *testing.T) {
	requireKustomize(t)

	repoDir := t.TempDir()
	base := filepath.Join(repoDir, "overlay")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "kustomization.yaml"),
		[]byte("resources:\n  - cm.yaml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "cm.yaml"),
		[]byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: kust-cm\ndata:\n  a: b\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	app := &cluster.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "kust-app"},
		Spec: cluster.ApplicationSpec{
			Source: &cluster.ApplicationSource{
				RepoURL: "https://github.com/example/repo.git",
				Path:    "overlay",
			},
			Destination: cluster.ApplicationDestination{Namespace: "default"},
		},
	}

	r := mustNewArgoCDRenderer(t, RenderOptions{})
	result, err := r.RenderApplication(context.Background(), app, repoDir, "abcdef1234567890")
	if err != nil {
		t.Fatalf("RenderApplication() error = %v", err)
	}
	if result.SourceType != types.SourceTypeKustomize {
		t.Errorf("SourceType = %q, want %q", result.SourceType, types.SourceTypeKustomize)
	}
	docs := docsByKindName(t, result.Manifests)
	if !docs["ConfigMap/kust-cm"] {
		t.Errorf("rendered output missing ConfigMap/kust-cm; got %v", docs)
	}
}

// TestArgoCDRenderer_SharedPathKustomizeOverridesDoNotLeak is the regression
// test for the shared-worktree kustomize leak: ArgoCD applies spec kustomize
// overrides via `kustomize edit`, which rewrites kustomization.yaml in the
// source directory. Two apps pointing at the SAME path must not see each
// other's overrides — the engine snapshots and restores the kustomization
// file around each render.
func TestArgoCDRenderer_SharedPathKustomizeOverridesDoNotLeak(t *testing.T) {
	requireKustomize(t)

	repoDir := t.TempDir()
	appDir := filepath.Join(repoDir, "kustomize-app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	kustomization := []byte("resources:\n  - cm.yaml\n")
	if err := os.WriteFile(filepath.Join(appDir, "kustomization.yaml"), kustomization, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "cm.yaml"),
		[]byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: kust-cm\ndata:\n  a: b\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	newApp := func(name string, kust *cluster.ApplicationSourceKustomize) *cluster.Application {
		return &cluster.Application{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: cluster.ApplicationSpec{
				Source: &cluster.ApplicationSource{
					RepoURL:   "https://github.com/example/repo.git",
					Path:      "kustomize-app",
					Kustomize: kust,
				},
				Destination: cluster.ApplicationDestination{Namespace: "default"},
			},
		}
	}

	r := mustNewArgoCDRenderer(t, RenderOptions{})

	// The overrides app renders first and must get its prefix.
	withOverrides := newApp("kust-overrides", &cluster.ApplicationSourceKustomize{NamePrefix: "base-"})
	result, err := r.RenderApplication(context.Background(), withOverrides, repoDir, "abcdef1234567890")
	if err != nil {
		t.Fatalf("RenderApplication(overrides) error = %v", err)
	}
	docs := docsByKindName(t, result.Manifests)
	if !docs["ConfigMap/base-kust-cm"] {
		t.Errorf("overrides app missing ConfigMap/base-kust-cm; got %v", docs)
	}

	// The kustomization file must be byte-identical after the render — the
	// `kustomize edit set nameprefix` mutation may not outlive it.
	after, err := os.ReadFile(filepath.Join(appDir, "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(kustomization) {
		t.Errorf("kustomization.yaml mutated after render:\n%s", after)
	}

	// A later render of the SAME path without overrides must not inherit the
	// prefix (pre-fix, this yielded ConfigMap/base-kust-cm).
	plain := newApp("kust-plain", nil)
	result, err = r.RenderApplication(context.Background(), plain, repoDir, "abcdef1234567890")
	if err != nil {
		t.Fatalf("RenderApplication(plain) error = %v", err)
	}
	docs = docsByKindName(t, result.Manifests)
	if !docs["ConfigMap/kust-cm"] {
		t.Errorf("plain app missing ConfigMap/kust-cm; got %v", docs)
	}
	if docs["ConfigMap/base-kust-cm"] {
		t.Errorf("plain app leaked the other app's namePrefix: got %v", docs)
	}
}

func TestArgoCDRenderer_MultiSourceValuesRef(t *testing.T) {
	requireHelm(t)

	repoURL := "https://github.com/example/repo.git"
	repoDir := t.TempDir()
	writeArgoTestChart(t, repoDir, "chart", nil)
	// Value file living OUTSIDE the chart, referenced via $vals. ArgoCD
	// resolves the path against the ref repo ROOT.
	if err := os.WriteFile(filepath.Join(repoDir, "prod-values.yaml"),
		[]byte("greeting: bonjour\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	app := &cluster.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "ms-app"},
		Spec: cluster.ApplicationSpec{
			Sources: []cluster.ApplicationSource{
				{
					RepoURL: repoURL,
					Path:    "chart",
					Helm: &cluster.ApplicationSourceHelm{
						ValueFiles: []string{"$vals/prod-values.yaml"},
					},
				},
				{
					RepoURL: repoURL,
					Ref:     "vals",
				},
			},
			Destination: cluster.ApplicationDestination{Namespace: "default"},
		},
	}

	// RepoURL matching makes the ref resolve to the local worktree (repoDir).
	r := mustNewArgoCDRenderer(t, RenderOptions{RepoURL: repoURL})
	result, err := r.RenderApplication(context.Background(), app, repoDir, "abcdef1234567890")
	if err != nil {
		t.Fatalf("RenderApplication() error = %v", err)
	}
	if !strings.Contains(string(result.Manifests), "bonjour") {
		t.Errorf("rendered output does not contain value from $vals ref file:\n%s", result.Manifests)
	}
}

// TestChartDepMutex pins the per-path mutex registry: the same path yields
// the same mutex (so chart-directory writes serialize), different paths yield
// different mutexes (so unrelated renders don't contend).
func TestChartDepMutex(t *testing.T) {
	a1 := chartDepMutex("/repo/charts/a")
	a2 := chartDepMutex("/repo/charts/a")
	b := chartDepMutex("/repo/charts/b")
	if a1 != a2 {
		t.Error("chartDepMutex returned different mutexes for the same path")
	}
	if a1 == b {
		t.Error("chartDepMutex returned the same mutex for different paths")
	}
}

func TestIsPureRef(t *testing.T) {
	tests := []struct {
		name   string
		source cluster.ApplicationSource
		want   bool
	}{
		{"ref only", cluster.ApplicationSource{Ref: "values"}, true},
		{"ref with path renders", cluster.ApplicationSource{Ref: "values", Path: "chart"}, false},
		{"ref with chart renders", cluster.ApplicationSource{Ref: "values", Chart: "app"}, false},
		{"no ref", cluster.ApplicationSource{Path: "chart"}, false},
		{"empty source", cluster.ApplicationSource{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPureRef(tt.source); got != tt.want {
				t.Errorf("isPureRef() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMapSourceType(t *testing.T) {
	tests := []struct {
		in   string
		want types.SourceType
	}{
		{"Helm", types.SourceTypeHelm},
		{"Kustomize", types.SourceTypeKustomize},
		{"Directory", types.SourceTypePlain},
		{"Plugin", types.SourceTypeUnknown},
		{"", types.SourceTypeUnknown},
	}
	for _, tt := range tests {
		if got := mapSourceType(tt.in); got != tt.want {
			t.Errorf("mapSourceType(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestManifestsToYAML(t *testing.T) {
	out, err := manifestsToYAML([]string{
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"a"}}`,
		`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"b"}}`,
	})
	if err != nil {
		t.Fatalf("manifestsToYAML() error = %v", err)
	}
	docs := strings.Split(string(out), "\n---\n")
	if len(docs) != 2 {
		t.Fatalf("expected 2 YAML docs, got %d:\n%s", len(docs), out)
	}
	if !strings.Contains(docs[0], "kind: ConfigMap") || !strings.Contains(docs[1], "kind: Secret") {
		t.Errorf("unexpected doc contents:\n%s", out)
	}

	if _, err := manifestsToYAML([]string{"{not-json"}); err == nil {
		t.Error("expected error for invalid JSON input")
	}
}

func TestBuildManifestRequest_RepoCreds(t *testing.T) {
	helmRepo := &argoappv1.Repository{Repo: "https://charts.acme.example", Name: "acme", Username: "helm-user", Type: "helm"}
	ociRepo := &argoappv1.Repository{Repo: "ghcr.io/acme", Username: "oci-user", Type: "oci", EnableOCI: true}
	helmTpl := &argoappv1.RepoCreds{URL: "https://charts.acme.example/team", Type: "helm"}
	ociTpl := &argoappv1.RepoCreds{URL: "registry.acme.example", Type: "oci"}

	opts := RenderOptions{
		HelmRepos:     []*argoappv1.Repository{helmRepo},
		OCIRepos:      []*argoappv1.Repository{ociRepo},
		HelmRepoCreds: []*argoappv1.RepoCreds{helmTpl},
		OCIRepoCreds:  []*argoappv1.RepoCreds{ociTpl},
		ResolveRepo: func(_ context.Context, repoURL, _ string) (*argoappv1.Repository, error) {
			return &argoappv1.Repository{
				Repo:     repoURL,
				Username: "resolved-user",
				Password: "resolved-pass",
				Proxy:    "http://proxy.local",
			}, nil
		},
	}
	r := mustNewArgoCDRenderer(t, opts)
	app := testApp("creds-app", "chart", nil)
	ctx := context.Background()

	t.Run("non-OCI source carries only the helm halves", func(t *testing.T) {
		q, err := r.buildManifestRequest(ctx, app, app.Spec.Source, nil)
		if err != nil {
			t.Fatalf("buildManifestRequest() error: %v", err)
		}
		if len(q.Repos) != 1 || q.Repos[0].Repo != helmRepo.Repo {
			t.Errorf("Repos = %+v, want only the helm repositories", q.Repos)
		}
		if len(q.HelmRepoCreds) != 1 || q.HelmRepoCreds[0].URL != helmTpl.URL {
			t.Errorf("HelmRepoCreds = %+v, want only the helm templates", q.HelmRepoCreds)
		}
		if q.Repo.Username != "resolved-user" || q.Repo.Proxy != "http://proxy.local" {
			t.Errorf("Repo = %+v, want the resolved repository (creds + proxy)", q.Repo)
		}
	})

	t.Run("oci source appends the OCI halves without mutating the options", func(t *testing.T) {
		source := &cluster.ApplicationSource{RepoURL: "oci://ghcr.io/acme", Chart: "app", TargetRevision: "1.0.0"}
		q, err := r.buildManifestRequest(ctx, app, source, nil)
		if err != nil {
			t.Fatalf("buildManifestRequest() error: %v", err)
		}
		if len(q.Repos) != 2 || q.Repos[1].Repo != ociRepo.Repo {
			t.Errorf("Repos = %+v, want helm + OCI repositories", q.Repos)
		}
		if len(q.HelmRepoCreds) != 2 || q.HelmRepoCreds[1].URL != ociTpl.URL {
			t.Errorf("HelmRepoCreds = %+v, want helm + OCI templates", q.HelmRepoCreds)
		}
		if len(r.opts.HelmRepos) != 1 || len(r.opts.HelmRepoCreds) != 1 {
			t.Error("per-source composition mutated the shared option slices")
		}
	})

	t.Run("scheme-less helm-OCI source URL is not IsOCI (ArgoCD parity)", func(t *testing.T) {
		source := &cluster.ApplicationSource{RepoURL: "ghcr.io/acme", Chart: "app", TargetRevision: "1.0.0"}
		q, err := r.buildManifestRequest(ctx, app, source, nil)
		if err != nil {
			t.Fatalf("buildManifestRequest() error: %v", err)
		}
		if len(q.Repos) != 1 {
			t.Errorf("Repos = %+v, want only the helm half for a scheme-less URL", q.Repos)
		}
	})

	t.Run("nil ResolveRepo falls back to a bare repo", func(t *testing.T) {
		bare := mustNewArgoCDRenderer(t, RenderOptions{})
		q, err := bare.buildManifestRequest(ctx, app, app.Spec.Source, nil)
		if err != nil {
			t.Fatalf("buildManifestRequest() error: %v", err)
		}
		if q.Repo == nil || q.Repo.Repo != app.Spec.Source.RepoURL || q.Repo.Username != "" {
			t.Errorf("Repo = %+v, want bare credential-less repository", q.Repo)
		}
	})

	t.Run("resolve errors fail the request with the root cause", func(t *testing.T) {
		failing := mustNewArgoCDRenderer(t, RenderOptions{
			ResolveRepo: func(context.Context, string, string) (*argoappv1.Repository, error) {
				return nil, context.DeadlineExceeded
			},
		})
		if _, err := failing.buildManifestRequest(ctx, app, app.Spec.Source, nil); err == nil {
			t.Error("buildManifestRequest() = nil error, want the credential resolution failure surfaced")
		}
	})
}

// TestBuildManifestRequest_GitSourceCarriesOCIDependencyCreds is the
// regression test for the 401s on git-path charts with private OCI
// dependencies: an enableOCI HELM-type repository (the classic
// `type: helm` + `enableOCI: true` secret shape) must reach the request's
// Repos for a plain git source — it rides the helm list unconditionally, NOT
// the IsOCI-gated OCI list — with its credentials seeded into the engine's
// registry auth file and stripped from the entry itself, so `helm dependency
// build` authenticates from HELM_REGISTRY_CONFIG without ever running
// `helm registry login` (macOS: shared-keychain writes and login/logout
// races across concurrent renders).
func TestBuildManifestRequest_GitSourceCarriesOCIDependencyCreds(t *testing.T) {
	opts := RenderOptions{
		HelmRepos: []*argoappv1.Repository{{
			Repo: "ghcr.io/acme", Name: "acme/app-template", Type: "helm",
			EnableOCI: true, Username: "bot", Password: "tok",
		}},
		HelmRepoCreds: []*argoappv1.RepoCreds{{
			URL: "registry.example.com/templates", Type: "helm", EnableOCI: true,
			Username: "tpl-bot", Password: "tpl-tok",
		}},
	}
	r := mustNewArgoCDRenderer(t, opts)

	app := testApp("git-app", "charts/umbrella", nil) // git path source, not OCI
	q, err := r.buildManifestRequest(context.Background(), app, app.Spec.Source, nil)
	if err != nil {
		t.Fatalf("buildManifestRequest() error: %v", err)
	}

	if len(q.Repos) != 1 || q.Repos[0].Repo != "ghcr.io/acme" || !q.Repos[0].EnableOCI {
		t.Fatalf("Repos = %+v, want the enableOCI helm repository offered to a git source (its OCI dependencies need it)", q.Repos)
	}
	if q.Repos[0].Username != "" || q.Repos[0].Password != "" {
		t.Error("request repo kept its credentials — DependencyBuild would exec `helm registry login`/`logout`")
	}
	if len(q.HelmRepoCreds) != 1 || q.HelmRepoCreds[0].Username != "" {
		t.Errorf("HelmRepoCreds = %+v, want the template offered, credential-stripped", q.HelmRepoCreds)
	}

	// The stripped credentials must be waiting in the engine-owned registry
	// config helm children read via HELM_REGISTRY_CONFIG.
	authFile := os.Getenv("HELM_REGISTRY_CONFIG")
	if authFile == "" || r.ownedRegistryAuth == nil || authFile != r.ownedRegistryAuth.path {
		t.Fatalf("HELM_REGISTRY_CONFIG = %q, want the engine-owned auth file", authFile)
	}
	auths := readAuths(t, authFile)
	if want := base64.StdEncoding.EncodeToString([]byte("bot:tok")); auths["ghcr.io"].Auth != want {
		t.Errorf("auth file ghcr.io = %q, want the repository secret credentials", auths["ghcr.io"].Auth)
	}
	if want := base64.StdEncoding.EncodeToString([]byte("tpl-bot:tpl-tok")); auths["registry.example.com"].Auth != want {
		t.Errorf("auth file registry.example.com = %q, want the credential template credentials", auths["registry.example.com"].Auth)
	}
}

// TestNewArgoCDRendererIsolatesHelmEnv pins the engine's environment
// contract: inherited helm variables are scrubbed (a first-occurrence
// duplicate would defeat ArgoCD's appended per-command temp homes), and
// HELM_REGISTRY_CONFIG points at the engine-owned per-run auth file — the
// mechanism that keeps registry logins out of the user's helm state and the
// macOS keychain. Concurrent constructions each own a distinct file.
func TestNewArgoCDRendererIsolatesHelmEnv(t *testing.T) {
	for _, v := range inheritedHelmEnvVars {
		t.Setenv(v, "/inherited")
	}

	r := mustNewArgoCDRenderer(t, RenderOptions{})
	for _, v := range inheritedHelmEnvVars {
		if v == "HELM_REGISTRY_CONFIG" {
			continue
		}
		if got, set := os.LookupEnv(v); set {
			t.Errorf("%s = %q survived engine construction", v, got)
		}
	}
	got := os.Getenv("HELM_REGISTRY_CONFIG")
	if r.ownedRegistryAuth == nil || got != r.ownedRegistryAuth.path {
		t.Fatalf("HELM_REGISTRY_CONFIG = %q, want the engine-owned auth file %v", got, r.ownedRegistryAuth)
	}
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("owned auth file missing: %v", err)
	}

	second := mustNewArgoCDRenderer(t, RenderOptions{})
	if second.ownedRegistryAuth.path == r.ownedRegistryAuth.path {
		t.Error("two engines share one auth file")
	}

	// Overlapping instances snapshot process-global env at construction and
	// must clean up LIFO (see Cleanup): second's restore rewinds to r's env,
	// r's to the inherited one.
	second.Cleanup()
	if got := os.Getenv("HELM_REGISTRY_CONFIG"); got != r.ownedRegistryAuth.path {
		t.Errorf("HELM_REGISTRY_CONFIG = %q after LIFO Cleanup of the second engine, want the first engine's auth file", got)
	}
	r.Cleanup()
	if _, err := os.Stat(r.ownedRegistryAuth.path); !os.IsNotExist(err) {
		t.Error("Cleanup() left the auth file (it holds tokens) behind")
	}
	if got := os.Getenv("HELM_REGISTRY_CONFIG"); got != "/inherited" {
		t.Errorf("HELM_REGISTRY_CONFIG = %q after the full unwind, want the inherited value", got)
	}
}

// TestArgoCDRendererCleanupRestoresHelmEnv pins the other half of the env
// contract: Cleanup undoes the process-global scrub, so repeated engine use
// in one process (tests, library-style reuse) never leaves the environment
// scrubbed with HELM_REGISTRY_CONFIG pointing at a deleted auth file.
func TestArgoCDRendererCleanupRestoresHelmEnv(t *testing.T) {
	t.Setenv("HELM_CONFIG_HOME", "/inherited/helm")
	t.Setenv("HELM_REGISTRY_CONFIG", "/inherited/registry.json")
	// Present-but-unset vars must stay unset after the restore; t.Setenv
	// records the original value for post-test restoration.
	t.Setenv("HELM_CACHE_HOME", "placeholder")
	_ = os.Unsetenv("HELM_CACHE_HOME")

	r, err := NewArgoCDRenderer(RenderOptions{})
	if err != nil {
		t.Fatalf("NewArgoCDRenderer() error = %v", err)
	}
	owned := r.ownedRegistryAuth.path

	r.Cleanup()
	if got := os.Getenv("HELM_CONFIG_HOME"); got != "/inherited/helm" {
		t.Errorf("HELM_CONFIG_HOME = %q after Cleanup, want the inherited value restored", got)
	}
	if got := os.Getenv("HELM_REGISTRY_CONFIG"); got != "/inherited/registry.json" {
		t.Errorf("HELM_REGISTRY_CONFIG = %q after Cleanup, want the inherited value restored", got)
	}
	if got, set := os.LookupEnv("HELM_CACHE_HOME"); set {
		t.Errorf("HELM_CACHE_HOME = %q after Cleanup, want it back to unset", got)
	}
	if _, statErr := os.Stat(owned); !os.IsNotExist(statErr) {
		t.Error("Cleanup() left the auth file behind")
	}

	// Cleanup is idempotent: a second call must not re-run the restore over
	// an environment that legitimately changed since.
	t.Setenv("HELM_REGISTRY_CONFIG", "/changed/after.json")
	r.Cleanup()
	if got := os.Getenv("HELM_REGISTRY_CONFIG"); got != "/changed/after.json" {
		t.Errorf("second Cleanup() re-ran the env restore; HELM_REGISTRY_CONFIG = %q", got)
	}
}

// TestNewArgoCDRendererPreservesLocalRegistryConfig pins the
// --repo-creds=local piercing: the user's own registry config survives the
// environment scrub and no engine-owned file is created, so OCI pulls
// authenticate with the user's own logins, read-only.
func TestNewArgoCDRendererPreservesLocalRegistryConfig(t *testing.T) {
	t.Setenv("HELM_REGISTRY_CONFIG", "/user/original.json")
	t.Setenv("HELM_CONFIG_HOME", "/user/helm")

	r := mustNewArgoCDRenderer(t, RenderOptions{
		HelmRegistryConfig: "/user/registry/config.json",
		HelmRepos: []*argoappv1.Repository{{
			Repo: "ghcr.io/acme", Type: "helm", EnableOCI: true, Username: "u", Password: "p",
		}},
	})

	if got := os.Getenv("HELM_REGISTRY_CONFIG"); got != "/user/registry/config.json" {
		t.Errorf("HELM_REGISTRY_CONFIG = %q, want the local-mode registry config", got)
	}
	if _, set := os.LookupEnv("HELM_CONFIG_HOME"); set {
		t.Error("HELM_CONFIG_HOME survived — helm children would resolve the user config over the isolated home")
	}
	if r.ownedRegistryAuth != nil {
		t.Error("local mode built an engine-owned auth file; it must read the user's config instead")
	}
	// Without an owned file there is nowhere safe to strip credentials to;
	// the lists must pass through untouched.
	if r.opts.HelmRepos[0].Username != "u" {
		t.Error("local-mode repositories were credential-stripped")
	}
}

func TestBuildManifestRequest_InstanceNameAndProject(t *testing.T) {
	r := mustNewArgoCDRenderer(t, RenderOptions{ArgoCDNamespace: "argocd"})
	ctx := context.Background()

	app := testApp("my-app", "chart", nil)
	app.Namespace = "team-a"
	app.Spec.Project = "payments"

	q, err := r.buildManifestRequest(ctx, app, app.Spec.Source, nil)
	if err != nil {
		t.Fatalf("buildManifestRequest() error: %v", err)
	}
	if q.AppName != "team-a_my-app" {
		t.Errorf("AppName = %q, want the instance name team-a_my-app (feeds ARGOCD_APP_NAME and the release name)", q.AppName)
	}
	if q.ProjectName != "payments" {
		t.Errorf("ProjectName = %q, want payments (feeds ARGOCD_APP_PROJECT_NAME)", q.ProjectName)
	}

	// In the control-plane namespace the instance name is the plain name.
	app.Namespace = "argocd"
	q, err = r.buildManifestRequest(ctx, app, app.Spec.Source, nil)
	if err != nil {
		t.Fatalf("buildManifestRequest() error: %v", err)
	}
	if q.AppName != "my-app" {
		t.Errorf("AppName = %q, want the plain name inside the control-plane namespace", q.AppName)
	}
}

func TestPrepareRefSources_ResolvedRepo(t *testing.T) {
	repoURL := "https://github.com/example/repo.git"
	r := mustNewArgoCDRenderer(t, RenderOptions{
		RepoURL: repoURL,
		ResolveRepo: func(_ context.Context, url, _ string) (*argoappv1.Repository, error) {
			return &argoappv1.Repository{Repo: url, Username: "ref-user", Password: "ref-pass"}, nil
		},
	})
	repoPath := t.TempDir()
	sources := []cluster.ApplicationSource{
		{RepoURL: repoURL, Ref: "values"},
		{RepoURL: repoURL, Path: "chart"},
	}

	refSources, tempPaths, cleanup, err := r.prepareRefSources(context.Background(), "default", sources, repoPath)
	if err != nil {
		t.Fatalf("prepareRefSources() error: %v", err)
	}
	defer cleanup()

	target := refSources["$values"]
	if target == nil {
		t.Fatal("missing $values ref target")
	}
	if target.Repo.Username != "ref-user" || target.Repo.Password != "ref-pass" {
		t.Errorf("RefTarget.Repo = %+v, want the resolved repository credentials", target.Repo)
	}
	// The local repo must still map to the current worktree, not a clone.
	if got := tempPaths.GetPathIfExists(argogit.NormalizeGitURL(repoURL)); got != repoPath {
		t.Errorf("ref repo path = %q, want the local worktree %q", got, repoPath)
	}
}

// initGitRepo turns dir into a committed git repository and returns its
// file:// URL, for external-source clone tests without a network.
func initGitRepo(t *testing.T, dir string) string {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"add", "."},
		{"-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "-q", "-m", "init"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	return "file://" + dir
}

// TestArgoCDRenderer_ExternalRenderableSource pins that a renderable source
// living in ANOTHER git repository renders from its own checkout at
// TargetRevision — never from the local worktree (which may contain an
// identically named path, or nothing at all).
func TestArgoCDRenderer_ExternalRenderableSource(t *testing.T) {
	requireHelm(t)

	// The external repo carries the chart; the local worktree does NOT.
	extRepo := t.TempDir()
	writeArgoTestChart(t, extRepo, "chart", map[string]string{
		"values.yaml": "greeting: from-external-repo\n",
	})
	extURL := initGitRepo(t, extRepo)

	localWorktree := t.TempDir() // deliberately empty

	app := testApp("ext-app", "chart", nil)
	app.Spec.Source.RepoURL = extURL

	r := mustNewArgoCDRenderer(t, RenderOptions{RepoURL: "https://github.com/example/local-repo.git"})
	result, err := r.RenderApplication(context.Background(), app, localWorktree, "rev")
	if err != nil {
		t.Fatalf("RenderApplication() error: %v", err)
	}

	if !strings.Contains(string(result.Manifests), "from-external-repo") {
		t.Errorf("manifests do not contain the external repo's values:\n%s", result.Manifests)
	}
}

// TestArgoCDRenderer_ExternalValuesRef pins the external-$ref clone path: a
// values file referenced from ANOTHER git repository must be cloned and
// resolved, while the chart itself renders from the local worktree.
func TestArgoCDRenderer_ExternalValuesRef(t *testing.T) {
	requireHelm(t)

	refRepo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(refRepo, "envs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(refRepo, "envs", "prod.yaml"), []byte("greeting: from-external-ref\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	refURL := initGitRepo(t, refRepo)

	localRepo := t.TempDir()
	writeArgoTestChart(t, localRepo, "chart", nil)

	app := &cluster.Application{ObjectMeta: metav1.ObjectMeta{Name: "ref-app"}}
	app.Spec.Sources = []cluster.ApplicationSource{
		{
			RepoURL: "https://github.com/example/repo.git",
			Path:    "chart",
			Helm:    &cluster.ApplicationSourceHelm{ValueFiles: []string{"$vals/envs/prod.yaml"}},
		},
		{RepoURL: refURL, Ref: "vals"},
	}
	app.Spec.Destination = cluster.ApplicationDestination{Namespace: "default"}

	r := mustNewArgoCDRenderer(t, RenderOptions{RepoURL: "https://github.com/example/repo.git"})
	result, err := r.RenderApplication(context.Background(), app, localRepo, "rev")
	if err != nil {
		t.Fatalf("RenderApplication() error: %v", err)
	}
	if !strings.Contains(string(result.Manifests), "from-external-ref") {
		t.Errorf("manifests do not carry the external ref values:\n%s", result.Manifests)
	}
}
