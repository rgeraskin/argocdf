package render

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/rgeraskin/argocdf/internal/cluster"
	"github.com/rgeraskin/argocdf/internal/types"
)

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
		"Chart.yaml": "apiVersion: v2\nname: testchart\nversion: 0.1.0\n",
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

	r := NewArgoCDRenderer(RenderOptions{})
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
	// must appear (the native engine currently omits it).
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

			r := NewArgoCDRenderer(RenderOptions{})
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

	r := NewArgoCDRenderer(RenderOptions{})
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
	r := NewArgoCDRenderer(RenderOptions{RepoURL: repoURL})
	result, err := r.RenderApplication(context.Background(), app, repoDir, "abcdef1234567890")
	if err != nil {
		t.Fatalf("RenderApplication() error = %v", err)
	}
	if !strings.Contains(string(result.Manifests), "bonjour") {
		t.Errorf("rendered output does not contain value from $vals ref file:\n%s", result.Manifests)
	}
}

// TestArgoCDRenderer_NativeParity renders the same simple chart with both
// engines and requires the same resource set (CRDs excluded — the engines
// intentionally differ there until --include-crds lands in native).
func TestArgoCDRenderer_NativeParity(t *testing.T) {
	requireHelm(t)

	repoDir := t.TempDir()
	writeArgoTestChart(t, repoDir, "chart", nil)
	app := testApp("parity-app", "chart", nil)

	native := NewFactory(RenderOptions{})
	nativeResult, err := native.RenderApplication(context.Background(), app, repoDir, "abcdef1234567890")
	if err != nil {
		t.Fatalf("native RenderApplication() error = %v", err)
	}

	argocd := NewArgoCDRenderer(RenderOptions{})
	argocdResult, err := argocd.RenderApplication(context.Background(), app, repoDir, "abcdef1234567890")
	if err != nil {
		t.Fatalf("argocd RenderApplication() error = %v", err)
	}

	nativeDocs := docsByKindName(t, nativeResult.Manifests)
	argocdDocs := docsByKindName(t, argocdResult.Manifests)
	delete(argocdDocs, "CustomResourceDefinition/widgets.example.com")

	for k := range nativeDocs {
		if !argocdDocs[k] {
			t.Errorf("argocd engine missing resource %s rendered by native", k)
		}
	}
	for k := range argocdDocs {
		if !nativeDocs[k] {
			t.Errorf("argocd engine rendered extra resource %s (beyond CRDs)", k)
		}
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
