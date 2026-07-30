package render

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	"github.com/rgeraskin/argocdf/internal/cluster"
)

// resourceCheck asserts a rendered resource exists and, optionally, that
// specific fields have specific values. Assertions are SEMANTIC (parsed
// documents, not raw text) because the engine re-serializes YAML from parsed
// objects (quoting/key order differ from raw helm output).
type resourceCheck struct {
	// kindName identifies the resource as "Kind/name".
	kindName string
	// fields maps dotted paths (numeric segments index into lists) to
	// expected values, compared via fmt.Sprint.
	fields map[string]string
}

// renderScenario is one application-rendering behavior. This suite pins the
// engine's application-level behavior (helm/kustomize/directory dispatch,
// option translation, multi-source concatenation): any silent change in what
// a spec renders to fails here.
type renderScenario struct {
	name           string
	needsHelm      bool
	needsKustomize bool
	files          map[string]string
	app            func() *cluster.Application
	opts           RenderOptions
	wantErr        bool
	// rawNotContains is a raw-text ban for non-manifest content that has no
	// resource identity.
	want           []resourceCheck
	absent         []string
	rawNotContains []string
}

const testRevision = "0123456789abcdef0123456789abcdef01234567"

// helmApp builds a single-source app at path "chart".
func helmApp(helm *cluster.ApplicationSourceHelm) func() *cluster.Application {
	return func() *cluster.Application {
		return &cluster.Application{
			ObjectMeta: metav1.ObjectMeta{Name: "eng-app"},
			Spec: cluster.ApplicationSpec{
				Source: &cluster.ApplicationSource{
					RepoURL: "https://github.com/example/repo.git",
					Path:    "chart",
					Helm:    helm,
				},
				Destination: cluster.ApplicationDestination{Namespace: "default"},
			},
		}
	}
}

// kustomizeApp builds a single-source app at path "overlay" with the given
// kustomize overrides.
func kustomizeApp(k *cluster.ApplicationSourceKustomize) func() *cluster.Application {
	return func() *cluster.Application {
		return &cluster.Application{
			ObjectMeta: metav1.ObjectMeta{Name: "eng-app"},
			Spec: cluster.ApplicationSpec{
				Source: &cluster.ApplicationSource{
					RepoURL:   "https://github.com/example/repo.git",
					Path:      "overlay",
					Kustomize: k,
				},
				Destination: cluster.ApplicationDestination{Namespace: "default"},
			},
		}
	}
}

// dirApp builds a single-source app at the given path with an explicit
// (or nil) directory config.
func dirApp(path string, dir *cluster.ApplicationSourceDirectory) func() *cluster.Application {
	return func() *cluster.Application {
		return &cluster.Application{
			ObjectMeta: metav1.ObjectMeta{Name: "eng-app"},
			Spec: cluster.ApplicationSpec{
				Source: &cluster.ApplicationSource{
					RepoURL:   "https://github.com/example/repo.git",
					Path:      path,
					Directory: dir,
				},
				Destination: cluster.ApplicationDestination{Namespace: "default"},
			},
		}
	}
}

// minimalChart returns chart files emitting the named values through a
// ConfigMap; extra files are merged in (overwriting on collision).
func minimalChart(extra map[string]string) map[string]string {
	files := map[string]string{
		"chart/Chart.yaml":  "apiVersion: v2\nname: engchart\nversion: 0.1.0\n",
		"chart/values.yaml": "greeting: from-values\nnum: fallback\n",
		"chart/templates/cm.yaml": `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}-cm
data:
  greeting: {{ .Values.greeting | quote }}
  num: {{ .Values.num | quote }}
  ns: {{ .Release.Namespace | quote }}
`,
	}
	for k, v := range extra {
		files[k] = v
	}
	return files
}

// kustomizeOverlay returns a kustomization with one ConfigMap and a Deployment
// (for image override scenarios).
func kustomizeOverlay() map[string]string {
	return map[string]string{
		"overlay/kustomization.yaml": "resources:\n  - cm.yaml\n  - deploy.yaml\n",
		"overlay/cm.yaml":            "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: kust-cm\ndata:\n  a: b\n",
		"overlay/deploy.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: kust-deploy
spec:
  selector:
    matchLabels:
      app: kust
  template:
    metadata:
      labels:
        app: kust
    spec:
      containers:
        - name: main
          image: nginx:1.20
`,
	}
}

func renderScenarios() []renderScenario {
	return []renderScenario{
		{
			name:      "helm/values-yaml",
			needsHelm: true,
			files:     minimalChart(nil),
			app:       helmApp(nil),
			want: []resourceCheck{{
				kindName: "ConfigMap/eng-app-cm",
				fields:   map[string]string{"data.greeting": "from-values"},
			}},
		},
		{
			name:      "helm/inline-values",
			needsHelm: true,
			files:     minimalChart(nil),
			app:       helmApp(&cluster.ApplicationSourceHelm{Values: "greeting: from-inline\n"}),
			want: []resourceCheck{{
				kindName: "ConfigMap/eng-app-cm",
				fields:   map[string]string{"data.greeting": "from-inline"},
			}},
		},
		{
			name:      "helm/values-object",
			needsHelm: true,
			files:     minimalChart(nil),
			app: helmApp(&cluster.ApplicationSourceHelm{
				ValuesObject: &runtime.RawExtension{Raw: []byte(`{"greeting":"from-object"}`)},
			}),
			want: []resourceCheck{{
				kindName: "ConfigMap/eng-app-cm",
				fields:   map[string]string{"data.greeting": "from-object"},
			}},
		},
		{
			name:      "helm/parameters-set",
			needsHelm: true,
			files:     minimalChart(nil),
			app: helmApp(&cluster.ApplicationSourceHelm{
				Parameters: []cluster.HelmParameter{{Name: "greeting", Value: "from-param"}},
			}),
			want: []resourceCheck{{
				kindName: "ConfigMap/eng-app-cm",
				fields:   map[string]string{"data.greeting": "from-param"},
			}},
		},
		{
			name:      "helm/parameters-force-string",
			needsHelm: true,
			files:     minimalChart(nil),
			app: helmApp(&cluster.ApplicationSourceHelm{
				Parameters: []cluster.HelmParameter{{Name: "num", Value: "0123", ForceString: true}},
			}),
			// --set would coerce 0123 to the number 123; --set-string must keep it.
			want: []resourceCheck{{
				kindName: "ConfigMap/eng-app-cm",
				fields:   map[string]string{"data.num": "0123"},
			}},
		},
		{
			name:      "helm/release-name-override",
			needsHelm: true,
			files:     minimalChart(nil),
			app:       helmApp(&cluster.ApplicationSourceHelm{ReleaseName: "custom-rel"}),
			want:      []resourceCheck{{kindName: "ConfigMap/custom-rel-cm"}},
			absent:    []string{"ConfigMap/eng-app-cm"},
		},
		{
			name:      "helm/value-files-relative-to-chart",
			needsHelm: true,
			files: minimalChart(map[string]string{
				"chart/overrides.yaml": "greeting: from-file\n",
			}),
			app: helmApp(&cluster.ApplicationSourceHelm{ValueFiles: []string{"overrides.yaml"}}),
			want: []resourceCheck{{
				kindName: "ConfigMap/eng-app-cm",
				fields:   map[string]string{"data.greeting": "from-file"},
			}},
		},
		{
			name:      "helm/destination-namespace",
			needsHelm: true,
			files:     minimalChart(nil),
			app:       helmApp(nil),
			want: []resourceCheck{{
				kindName: "ConfigMap/eng-app-cm",
				fields:   map[string]string{"data.ns": "default"},
			}},
		},
		{
			name:      "helm/kube-version-sanitized",
			needsHelm: true,
			files: minimalChart(map[string]string{
				"chart/templates/kv.yaml": `apiVersion: v1
kind: ConfigMap
metadata:
  name: kv-cm
data:
  kv: {{ .Capabilities.KubeVersion.Version | quote }}
`,
			}),
			app:  helmApp(nil),
			opts: RenderOptions{KubeVersion: "v1.30.2-gke.1091002"},
			want: []resourceCheck{{
				kindName: "ConfigMap/kv-cm",
				fields:   map[string]string{"data.kv": "v1.30.2"},
			}},
		},
		{
			name:      "helm/api-versions",
			needsHelm: true,
			files: minimalChart(map[string]string{
				"chart/templates/capa.yaml": `apiVersion: v1
kind: ConfigMap
metadata:
  name: capa-cm
data:
  has: {{ .Capabilities.APIVersions.Has "myapi.example.com/v1" | quote }}
`,
			}),
			app:  helmApp(nil),
			opts: RenderOptions{APIVersions: []string{"myapi.example.com/v1"}},
			want: []resourceCheck{{
				kindName: "ConfigMap/capa-cm",
				fields:   map[string]string{"data.has": "true"},
			}},
		},
		{
			name:      "helm/chart-autodetected-without-helm-block",
			needsHelm: true,
			files:     minimalChart(nil),
			app:       helmApp(nil), // no Helm block; Chart.yaml presence must trigger helm
			want: []resourceCheck{{
				kindName: "ConfigMap/eng-app-cm",
				fields:   map[string]string{"data.greeting": "from-values"},
			}},
		},
		{
			name:      "helm/schema-violation-fails",
			needsHelm: true,
			files: minimalChart(map[string]string{
				"chart/values.schema.json": `{"type": "object", "properties": {"greeting": {"type": "integer"}}}`,
			}),
			app:     helmApp(nil),
			wantErr: true,
		},
		{
			name:      "helm/skip-schema-validation",
			needsHelm: true,
			files: minimalChart(map[string]string{
				"chart/values.schema.json": `{"type": "object", "properties": {"greeting": {"type": "integer"}}}`,
			}),
			app: helmApp(&cluster.ApplicationSourceHelm{SkipSchemaValidation: true}),
			want: []resourceCheck{{
				kindName: "ConfigMap/eng-app-cm",
				fields:   map[string]string{"data.greeting": "from-values"},
			}},
		},
		{
			name:      "helm/crds-directory",
			needsHelm: true,
			files: minimalChart(map[string]string{
				"chart/crds/widget.yaml": `apiVersion: apiextensions.k8s.io/v1
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
			}),
			app: helmApp(nil),
			// ArgoCD templates with --include-crds: crds/ content must appear.
			want: []resourceCheck{
				{kindName: "ConfigMap/eng-app-cm"},
				{kindName: "CustomResourceDefinition/widgets.example.com"},
			},
		},
		{
			name:           "kustomize/basic",
			needsKustomize: true,
			files:          kustomizeOverlay(),
			app:            kustomizeApp(nil),
			want: []resourceCheck{
				{kindName: "ConfigMap/kust-cm"},
				{kindName: "Deployment/kust-deploy"},
			},
		},
		{
			name:           "kustomize/name-prefix",
			needsKustomize: true,
			files:          kustomizeOverlay(),
			app:            kustomizeApp(&cluster.ApplicationSourceKustomize{NamePrefix: "pre-"}),
			want: []resourceCheck{
				{kindName: "ConfigMap/pre-kust-cm"},
				{kindName: "Deployment/pre-kust-deploy"},
			},
			absent: []string{"ConfigMap/kust-cm", "Deployment/kust-deploy"},
		},
		{
			name:           "kustomize/namespace",
			needsKustomize: true,
			files:          kustomizeOverlay(),
			app:            kustomizeApp(&cluster.ApplicationSourceKustomize{Namespace: "production"}),
			want: []resourceCheck{{
				kindName: "ConfigMap/kust-cm",
				fields:   map[string]string{"metadata.namespace": "production"},
			}},
		},
		{
			name:           "kustomize/images-override",
			needsKustomize: true,
			files:          kustomizeOverlay(),
			app: kustomizeApp(&cluster.ApplicationSourceKustomize{
				Images: cluster.KustomizeImages{"nginx=nginx:1.21"},
			}),
			want: []resourceCheck{{
				kindName: "Deployment/kust-deploy",
				fields:   map[string]string{"spec.template.spec.containers.0.image": "nginx:1.21"},
			}},
		},
		{
			name:           "kustomize/common-labels",
			needsKustomize: true,
			files:          kustomizeOverlay(),
			app: kustomizeApp(&cluster.ApplicationSourceKustomize{
				CommonLabels: map[string]string{"team": "platform"},
			}),
			want: []resourceCheck{{
				kindName: "ConfigMap/kust-cm",
				fields:   map[string]string{"metadata.labels.team": "platform"},
			}},
		},
		{
			name:           "kustomize/overlay-with-relative-base",
			needsKustomize: true,
			files: map[string]string{
				"base/kustomization.yaml":    "resources:\n  - cm.yaml\n",
				"base/cm.yaml":               "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: base-cm\ndata:\n  a: b\n",
				"overlay/kustomization.yaml": "resources:\n  - ../base\nnamePrefix: ovl-\n",
			},
			app:  kustomizeApp(nil),
			want: []resourceCheck{{kindName: "ConfigMap/ovl-base-cm"}},
		},
		{
			// An EXPLICIT directory source wins over kustomization.yaml
			// auto-detection (ArgoCD's ExplicitType precedence): the overlay
			// renders as plain files — no namePrefix applied — and the
			// kustomization document itself passes through as a rendered doc.
			name: "directory/explicit-directory-overrides-kustomization",
			files: map[string]string{
				"overlay/kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nnamePrefix: pre-\nresources:\n  - cm.yaml\n",
				"overlay/cm.yaml":            "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: kust-cm\ndata:\n  a: b\n",
			},
			app:    dirApp("overlay", &cluster.ApplicationSourceDirectory{}),
			want:   []resourceCheck{{kindName: "ConfigMap/kust-cm"}},
			absent: []string{"ConfigMap/pre-kust-cm"},
		},
		{
			name: "directory/plain-yaml-non-recursive",
			files: map[string]string{
				"manifests/a-cm.yaml":          "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm1\n",
				"manifests/b-cm.yaml":          "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm2\n",
				"manifests/nested/nested.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: nested-cm\n",
			},
			app: dirApp("manifests", nil),
			want: []resourceCheck{
				{kindName: "ConfigMap/cm1"},
				{kindName: "ConfigMap/cm2"},
			},
			absent: []string{"ConfigMap/nested-cm"},
		},
		{
			name: "directory/recursive-with-json",
			files: map[string]string{
				"manifests/top.yaml":           "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: top-cm\n",
				"manifests/nested/secret.json": `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"nested-secret"}}`,
				"manifests/nested/notes.txt":   "ignore me",
			},
			app: dirApp("manifests", &cluster.ApplicationSourceDirectory{Recurse: true}),
			want: []resourceCheck{
				{kindName: "ConfigMap/top-cm"},
				{kindName: "Secret/nested-secret"},
			},
			rawNotContains: []string{"ignore me"},
		},
		{
			name: "directory/hidden-dir",
			files: map[string]string{
				"manifests/top.yaml":          "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: top-cm\n",
				"manifests/.hidden/skip.yaml": "apiVersion: v1\nkind: Pod\nmetadata:\n  name: skip-pod\n",
			},
			app: dirApp("manifests", &cluster.ApplicationSourceDirectory{Recurse: true}),
			// ArgoCD's recursive directory walk (getPotentiallyValidManifests)
			// does NOT skip hidden directories; the engine matches real ArgoCD
			// (the hidden-dir skip at repository.go:3009 is GetGitDirectories, a
			// different code path).
			want: []resourceCheck{
				{kindName: "ConfigMap/top-cm"},
				{kindName: "Pod/skip-pod"},
			},
		},
		{
			name: "multisource/concatenates-sources",
			files: map[string]string{
				"chart/Chart.yaml":  "apiVersion: v2\nname: engchart\nversion: 0.1.0\n",
				"chart/values.yaml": "greeting: multi\n",
				"chart/templates/cm.yaml": `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}-chart-cm
data:
  greeting: {{ .Values.greeting | quote }}
`,
				"plain/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: plain-cm\ndata:\n  a: b\n",
			},
			needsHelm: true,
			app: func() *cluster.Application {
				return &cluster.Application{
					ObjectMeta: metav1.ObjectMeta{Name: "eng-app"},
					Spec: cluster.ApplicationSpec{
						Sources: []cluster.ApplicationSource{
							{RepoURL: "https://github.com/example/repo.git", Path: "chart"},
							{RepoURL: "https://github.com/example/repo.git", Path: "plain"},
						},
						Destination: cluster.ApplicationDestination{Namespace: "default"},
					},
				}
			},
			want: []resourceCheck{
				{
					kindName: "ConfigMap/eng-app-chart-cm",
					fields:   map[string]string{"data.greeting": "multi"},
				},
				{kindName: "ConfigMap/plain-cm"},
			},
		},
		{
			name:      "multisource/values-ref",
			needsHelm: true,
			files: map[string]string{
				"chart/Chart.yaml":  "apiVersion: v2\nname: engchart\nversion: 0.1.0\n",
				"chart/values.yaml": "greeting: default\n",
				"chart/templates/cm.yaml": `apiVersion: v1
kind: ConfigMap
metadata:
  name: ref-cm
data:
  greeting: {{ .Values.greeting | quote }}
`,
				"prod-values.yaml": "greeting: from-ref\n",
			},
			opts: RenderOptions{RepoURL: "https://github.com/example/repo.git"},
			app: func() *cluster.Application {
				return &cluster.Application{
					ObjectMeta: metav1.ObjectMeta{Name: "eng-app"},
					Spec: cluster.ApplicationSpec{
						Sources: []cluster.ApplicationSource{
							{
								RepoURL: "https://github.com/example/repo.git",
								Path:    "chart",
								Helm: &cluster.ApplicationSourceHelm{
									ValueFiles: []string{"$vals/prod-values.yaml"},
								},
							},
							// Ref source: $vals/... resolves against the ref
							// repository ROOT (ArgoCD semantics).
							{RepoURL: "https://github.com/example/repo.git", Ref: "vals"},
						},
						Destination: cluster.ApplicationDestination{Namespace: "default"},
					},
				}
			},
			want: []resourceCheck{{
				kindName: "ConfigMap/ref-cm",
				fields:   map[string]string{"data.greeting": "from-ref"},
			}},
		},
	}
}

// parseRendered parses multi-doc YAML into documents keyed by "Kind/name".
func parseRendered(t *testing.T, manifests []byte) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, doc := range strings.Split(string(manifests), "\n---") {
		doc = strings.TrimSpace(strings.TrimPrefix(doc, "---"))
		if doc == "" {
			continue
		}
		var m map[string]any
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
			t.Fatalf("unmarshal rendered doc: %v\ndoc:\n%s", err, doc)
		}
		kind, _ := m["kind"].(string)
		if kind == "" {
			continue
		}
		name := ""
		if md, ok := m["metadata"].(map[string]any); ok {
			name, _ = md["name"].(string)
		}
		out[kind+"/"+name] = m
	}
	return out
}

// fieldAt walks a dotted path through nested maps and lists (numeric
// segments index into lists).
func fieldAt(doc any, path string) (any, bool) {
	cur := doc
	for _, seg := range strings.Split(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[seg]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(node) {
				return nil, false
			}
			cur = node[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// TestRenderBehavior runs every behavioral scenario against the render
// engine, pinning what a spec renders to end-to-end: any silent behavior
// change in dispatch, option translation, or multi-source handling fails here.
func TestRenderBehavior(t *testing.T) {
	for _, sc := range renderScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			if sc.needsHelm {
				requireHelm(t)
			}
			if sc.needsKustomize {
				requireKustomize(t)
			}

			// Fresh repo per scenario: renders may mutate the tree
			// (helm dependency build, kustomize edit).
			repoDir := t.TempDir()
			for name, content := range sc.files {
				p := filepath.Join(repoDir, name)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
					t.Fatalf("write %s: %v", name, err)
				}
			}

			engine := mustNewArgoCDRenderer(t, sc.opts)
			result, err := engine.RenderApplication(context.Background(), sc.app(), repoDir, testRevision)

			if sc.wantErr {
				if err == nil {
					t.Fatalf("RenderApplication() expected error, got nil\noutput:\n%s", result.Manifests)
				}
				return
			}
			if err != nil {
				t.Fatalf("RenderApplication() error = %v", err)
			}

			docs := parseRendered(t, result.Manifests)

			for _, check := range sc.want {
				doc, ok := docs[check.kindName]
				if !ok {
					t.Errorf("missing resource %s; rendered: %v", check.kindName, keysOf(docs))
					continue
				}
				for path, wantVal := range check.fields {
					got, ok := fieldAt(doc, path)
					if !ok {
						t.Errorf("%s: field %s not found", check.kindName, path)
						continue
					}
					if fmt.Sprint(got) != wantVal {
						t.Errorf("%s: field %s = %v, want %v", check.kindName, path, got, wantVal)
					}
				}
			}
			for _, banned := range sc.absent {
				if _, ok := docs[banned]; ok {
					t.Errorf("resource %s must not be rendered", banned)
				}
			}
			for _, banned := range sc.rawNotContains {
				if strings.Contains(string(result.Manifests), banned) {
					t.Errorf("output must not contain %q", banned)
				}
			}
		})
	}
}

// keysOf returns the sorted-ish key list for error messages.
func keysOf(m map[string]map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
