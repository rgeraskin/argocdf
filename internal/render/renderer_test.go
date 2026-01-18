package render

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rgeraskin/argocdf/internal/cluster"
)

func TestGetRenderer(t *testing.T) {
	// Create a temporary directory for testing filesystem-based detection
	tempDir, err := os.MkdirTemp("", "renderer-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create a subdirectory with Chart.yaml (simulates a Helm chart)
	helmChartDir := filepath.Join(tempDir, "helm-chart")
	if err := os.MkdirAll(helmChartDir, 0755); err != nil {
		t.Fatalf("failed to create helm chart dir: %v", err)
	}
	chartYaml := filepath.Join(helmChartDir, "Chart.yaml")
	if err := os.WriteFile(chartYaml, []byte("name: test-chart\nversion: 1.0.0\n"), 0644); err != nil {
		t.Fatalf("failed to create Chart.yaml: %v", err)
	}

	// Create a subdirectory without Chart.yaml (plain directory)
	plainDir := filepath.Join(tempDir, "plain-dir")
	if err := os.MkdirAll(plainDir, 0755); err != nil {
		t.Fatalf("failed to create plain dir: %v", err)
	}

	factory := NewFactory(RenderOptions{})

	tests := []struct {
		name         string
		source       *cluster.ApplicationSource
		repoPath     string
		wantRenderer string // "helm" or "kustomize"
	}{
		{
			name: "explicit helm config with Chart field",
			source: &cluster.ApplicationSource{
				Chart:   "nginx",
				RepoURL: "https://charts.bitnami.com/bitnami",
			},
			repoPath:     tempDir,
			wantRenderer: "helm",
		},
		{
			name: "explicit helm config with Helm field",
			source: &cluster.ApplicationSource{
				Path: "some/path",
				Helm: &cluster.ApplicationSourceHelm{
					ReleaseName: "my-release",
				},
			},
			repoPath:     tempDir,
			wantRenderer: "helm",
		},
		{
			name: "explicit kustomize config",
			source: &cluster.ApplicationSource{
				Path: "some/path",
				Kustomize: &cluster.ApplicationSourceKustomize{
					NamePrefix: "test-",
				},
			},
			repoPath:     tempDir,
			wantRenderer: "kustomize",
		},
		{
			name: "path with Chart.yaml detected as helm",
			source: &cluster.ApplicationSource{
				Path: "helm-chart",
			},
			repoPath:     tempDir,
			wantRenderer: "helm",
		},
		{
			name: "path without Chart.yaml defaults to kustomize",
			source: &cluster.ApplicationSource{
				Path: "plain-dir",
			},
			repoPath:     tempDir,
			wantRenderer: "kustomize",
		},
		{
			name: "empty path defaults to kustomize",
			source: &cluster.ApplicationSource{
				RepoURL: "https://github.com/example/repo",
			},
			repoPath:     tempDir,
			wantRenderer: "kustomize",
		},
		{
			name: "empty repoPath skips filesystem check",
			source: &cluster.ApplicationSource{
				Path: "helm-chart",
			},
			repoPath:     "",
			wantRenderer: "kustomize",
		},
		{
			name: "nonexistent path defaults to kustomize",
			source: &cluster.ApplicationSource{
				Path: "nonexistent-path",
			},
			repoPath:     tempDir,
			wantRenderer: "kustomize",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			renderer := factory.GetRenderer(tt.source, tt.repoPath)

			var gotRenderer string
			switch renderer.(type) {
			case *HelmRenderer:
				gotRenderer = "helm"
			case *KustomizeRenderer:
				gotRenderer = "kustomize"
			default:
				t.Fatalf("unexpected renderer type: %T", renderer)
			}

			if gotRenderer != tt.wantRenderer {
				t.Errorf("GetRenderer() = %s, want %s", gotRenderer, tt.wantRenderer)
			}
		})
	}
}

func TestGetRendererPrecedence(t *testing.T) {
	// Test that explicit config takes precedence over filesystem detection
	tempDir, err := os.MkdirTemp("", "renderer-precedence-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create a directory with both Chart.yaml and set Kustomize config
	// Kustomize config should win since it's explicit
	mixedDir := filepath.Join(tempDir, "mixed")
	if err := os.MkdirAll(mixedDir, 0755); err != nil {
		t.Fatalf("failed to create mixed dir: %v", err)
	}
	chartYaml := filepath.Join(mixedDir, "Chart.yaml")
	if err := os.WriteFile(chartYaml, []byte("name: test\n"), 0644); err != nil {
		t.Fatalf("failed to create Chart.yaml: %v", err)
	}

	factory := NewFactory(RenderOptions{})

	// Explicit Kustomize config should take precedence over Chart.yaml
	source := &cluster.ApplicationSource{
		Path: "mixed",
		Kustomize: &cluster.ApplicationSourceKustomize{
			NamePrefix: "test-",
		},
	}

	renderer := factory.GetRenderer(source, tempDir)
	if _, ok := renderer.(*KustomizeRenderer); !ok {
		t.Errorf("expected KustomizeRenderer when Kustomize config is explicit, got %T", renderer)
	}
}
