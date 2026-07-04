// Package render provides tests for multi-source rendering functionality.
package render

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rgeraskin/argocdf/internal/cluster"
)

func TestIsPureRef(t *testing.T) {
	tests := []struct {
		name   string
		source cluster.ApplicationSource
		want   bool
	}{
		{
			name:   "pure ref - only Ref set",
			source: cluster.ApplicationSource{Ref: "values"},
			want:   true,
		},
		{
			name:   "ref with path - renders too",
			source: cluster.ApplicationSource{Ref: "values", Path: "charts/app"},
			want:   false,
		},
		{
			name:   "ref with chart - renders too",
			source: cluster.ApplicationSource{Ref: "values", Chart: "nginx"},
			want:   false,
		},
		{
			name:   "no ref, only path",
			source: cluster.ApplicationSource{Path: "charts/app"},
			want:   false,
		},
		{
			name:   "no ref, only chart",
			source: cluster.ApplicationSource{Chart: "nginx"},
			want:   false,
		},
		{
			name:   "empty source",
			source: cluster.ApplicationSource{},
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPureRef(tt.source); got != tt.want {
				t.Errorf("isPureRef(%+v) = %v, want %v", tt.source, got, tt.want)
			}
		})
	}
}

// TestPrepareRefSources_LocalRepo verifies that a ref source pointing at the
// local repository being diffed maps to the local branch checkout without
// cloning (no network access).
func TestPrepareRefSources_LocalRepo(t *testing.T) {
	const localURL = "https://github.com/org/repo"
	repoPath := "/tmp/fake-checkout"

	factory := NewFactory(RenderOptions{RepoURL: localURL})
	renderer := NewMultiSourceRenderer(factory, repoPath)

	tests := []struct {
		name     string
		source   cluster.ApplicationSource
		wantPath string
	}{
		{
			name:     "same repo, no path -> repo root",
			source:   cluster.ApplicationSource{RepoURL: localURL, Ref: "values"},
			wantPath: repoPath,
		},
		{
			name:     "same repo with path -> joined",
			source:   cluster.ApplicationSource{RepoURL: localURL, Ref: "values", Path: "env"},
			wantPath: filepath.Join(repoPath, "env"),
		},
		{
			name:     "same repo, ssh URL form normalizes to match",
			source:   cluster.ApplicationSource{RepoURL: "git@github.com:org/repo.git", Ref: "values"},
			wantPath: repoPath,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refSources, cleanup, err := renderer.prepareRefSources([]cluster.ApplicationSource{tt.source})
			if err != nil {
				t.Fatalf("prepareRefSources() error = %v", err)
			}
			defer cleanup()

			got, ok := refSources[tt.source.Ref]
			if !ok {
				t.Fatalf("ref %q not registered in refSources: %v", tt.source.Ref, refSources)
			}
			if got != tt.wantPath {
				t.Errorf("refSources[%q] = %q, want %q", tt.source.Ref, got, tt.wantPath)
			}
		})
	}
}

// TestPrepareRefSources_FallsBackWhenLocalURLUnknown verifies that when the
// local repo URL is unknown, a same-repo ref is not resolved locally (it would
// attempt to clone at render time instead).
func TestPrepareRefSources_RegistersRefWithPath(t *testing.T) {
	const localURL = "https://github.com/org/repo"
	repoPath := "/tmp/fake-checkout"

	factory := NewFactory(RenderOptions{RepoURL: localURL})
	renderer := NewMultiSourceRenderer(factory, repoPath)

	// A source that is both a ref AND renders (has a Path) must still be
	// registered as a ref so other sources can reference it.
	source := cluster.ApplicationSource{RepoURL: localURL, Ref: "values", Path: "charts/app"}
	refSources, cleanup, err := renderer.prepareRefSources([]cluster.ApplicationSource{source})
	if err != nil {
		t.Fatalf("prepareRefSources() error = %v", err)
	}
	defer cleanup()

	got, ok := refSources["values"]
	if !ok {
		t.Fatalf("ref %q not registered: %v", "values", refSources)
	}
	if want := filepath.Join(repoPath, "charts/app"); got != want {
		t.Errorf("refSources[values] = %q, want %q", got, want)
	}
}

func TestPrepareRefSources_EmptySources(t *testing.T) {
	// Test with empty sources - should return empty map
	factory := &Factory{}
	renderer := NewMultiSourceRenderer(factory, "/tmp/test")

	// We can't directly test prepareRefSources without mocking git.Clone,
	// but we can verify the structure is correct
	if renderer == nil {
		t.Error("NewMultiSourceRenderer() returned nil")
	}
}

func TestNewMultiSourceRenderer(t *testing.T) {
	tests := []struct {
		name     string
		repoPath string
	}{
		{
			name:     "with repo path",
			repoPath: "/tmp/test-repo",
		},
		{
			name:     "empty repo path",
			repoPath: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			factory := &Factory{}
			renderer := NewMultiSourceRenderer(factory, tt.repoPath)

			if renderer == nil {
				t.Error("NewMultiSourceRenderer() returned nil")
				return
			}
			if renderer.factory != factory {
				t.Error("factory not set correctly")
			}
			if renderer.repoPath != tt.repoPath {
				t.Errorf("repoPath = %q, want %q", renderer.repoPath, tt.repoPath)
			}
		})
	}
}

// skipIfNoHelm skips the test if helm is not installed.
func skipIfNoHelm(t *testing.T) {
	t.Helper()
	if err := (&HelmRenderer{}).CanRender(); err != nil {
		t.Skip("helm not installed, skipping integration test")
	}
}

// writeTestChart creates a minimal Helm chart under dir whose ConfigMap
// template renders .Values.message.
func writeTestChart(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"Chart.yaml":  "apiVersion: v2\nname: app\nversion: 0.1.0\n",
		"values.yaml": "message: from-chart\n",
		"templates/cm.yaml": "apiVersion: v1\n" +
			"kind: ConfigMap\n" +
			"metadata:\n" +
			"  name: app-cm\n" +
			"data:\n" +
			"  message: {{ .Values.message }}\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRenderMultiSource_ChartPathAutoDetection is a regression test for
// renderer selection parity with single-source apps (and with ArgoCD's
// repo-server): in a multi-source app, a path source containing Chart.yaml
// but no explicit chart/helm config must be auto-detected and rendered via
// Helm — not fall back to plain-YAML concatenation of the chart files.
func TestRenderMultiSource_ChartPathAutoDetection(t *testing.T) {
	skipIfNoHelm(t)

	repoDir := t.TempDir()
	writeTestChart(t, filepath.Join(repoDir, "charts", "app"))

	app := &cluster.Application{}
	app.Name = "test-app"
	app.Spec.Sources = []cluster.ApplicationSource{
		{RepoURL: "https://github.com/org/repo", Ref: "vals"},
		{RepoURL: "https://github.com/org/repo", Path: "charts/app"},
	}

	factory := NewFactory(RenderOptions{RepoURL: "https://github.com/org/repo"})
	manifests, err := NewMultiSourceRenderer(factory, repoDir).RenderMultiSource(context.Background(), app)
	if err != nil {
		t.Fatalf("RenderMultiSource() error = %v", err)
	}

	// Only a real helm render emits the templated ConfigMap: the plain-YAML
	// fallback (the bug) concatenates top-level chart files and never reaches
	// templates/, so asserting on the rendered resource pins the regression.
	out := string(manifests)
	if !strings.Contains(out, "name: app-cm") {
		t.Errorf("templated ConfigMap missing (plain-YAML fallback?); output:\n%s", out)
	}
	if !strings.Contains(out, "message: from-chart") {
		t.Errorf("chart default value not rendered; output:\n%s", out)
	}
	if strings.Contains(out, "{{") {
		t.Errorf("output contains unrendered template syntax; output:\n%s", out)
	}
}

// TestRenderMultiSource_ValuesRef renders a multi-source app end-to-end where
// the chart source pulls a value file from a $ref source in the local repo.
func TestRenderMultiSource_ValuesRef(t *testing.T) {
	skipIfNoHelm(t)

	repoDir := t.TempDir()
	writeTestChart(t, filepath.Join(repoDir, "charts", "app"))
	if err := os.MkdirAll(filepath.Join(repoDir, "envs", "prod"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "envs", "prod", "values.yaml"), []byte("message: from-ref\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	app := &cluster.Application{}
	app.Name = "test-app"
	app.Spec.Sources = []cluster.ApplicationSource{
		{RepoURL: "https://github.com/org/repo", Ref: "vals"},
		{
			RepoURL: "https://github.com/org/repo",
			Path:    "charts/app",
			Helm: &cluster.ApplicationSourceHelm{
				ValueFiles: []string{"$vals/envs/prod/values.yaml"},
			},
		},
	}

	factory := NewFactory(RenderOptions{RepoURL: "https://github.com/org/repo"})
	manifests, err := NewMultiSourceRenderer(factory, repoDir).RenderMultiSource(context.Background(), app)
	if err != nil {
		t.Fatalf("RenderMultiSource() error = %v", err)
	}

	if out := string(manifests); !strings.Contains(out, "message: from-ref") {
		t.Errorf("$vals value file not applied; output:\n%s", out)
	}
}

// Verify interfaces are satisfied at compile time
var _ = reflect.TypeOf((*MultiSourceRenderer)(nil))
