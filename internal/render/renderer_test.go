package render

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rgeraskin/argocdf/internal/cluster"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestGetRenderer(t *testing.T) {
	// Create a temporary directory for testing filesystem-based detection
	tempDir, err := os.MkdirTemp("", "renderer-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(tempDir)
	}()

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
			name: "explicit directory source skips Chart.yaml auto-detection",
			source: &cluster.ApplicationSource{
				Path:      "helm-chart",
				Directory: &cluster.ApplicationSourceDirectory{},
			},
			repoPath:     tempDir,
			wantRenderer: "kustomize",
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

func TestAddHelmOptionsValuesObject(t *testing.T) {
	renderer := NewHelmRenderer(RenderOptions{})

	tests := []struct {
		name          string
		helm          *cluster.ApplicationSourceHelm
		wantValuesArg bool
		wantContains  string // substring expected in the temp values file
	}{
		{
			name: "valuesObject creates values file",
			helm: &cluster.ApplicationSourceHelm{
				ValuesObject: &runtime.RawExtension{
					Raw: []byte(`{"cronjob":{"image":{"tag":"qwe"}}}`),
				},
			},
			wantValuesArg: true,
			wantContains:  "tag: qwe",
		},
		{
			name: "empty valuesObject skipped",
			helm: &cluster.ApplicationSourceHelm{
				ValuesObject: &runtime.RawExtension{Raw: []byte{}},
			},
			wantValuesArg: false,
		},
		{
			name:          "nil valuesObject skipped",
			helm:          &cluster.ApplicationSourceHelm{},
			wantValuesArg: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args, tempFiles, err := renderer.addHelmOptions([]string{}, tt.helm, "/repo", "/repo/chart")
			if err != nil {
				t.Fatalf("addHelmOptions failed: %v", err)
			}
			// Cleanup temp files after test
			defer func() {
				for _, f := range tempFiles {
					_ = os.Remove(f)
				}
			}()

			// Check if --values argument was added
			valuesArgIdx := -1
			for i, arg := range args {
				if arg == "--values" && i+1 < len(args) {
					// Check if this is from valuesObject (contains "values-object" in path)
					if strings.Contains(args[i+1], "values-object") {
						valuesArgIdx = i
						break
					}
				}
			}

			hasValuesArg := valuesArgIdx >= 0
			if hasValuesArg != tt.wantValuesArg {
				t.Errorf("valuesObject handling: got --values=%v, want %v", hasValuesArg, tt.wantValuesArg)
			}

			// If we expect a values file, check its contents
			if tt.wantValuesArg && tt.wantContains != "" && valuesArgIdx >= 0 {
				valuesFile := args[valuesArgIdx+1]
				content, err := os.ReadFile(valuesFile)
				if err != nil {
					t.Errorf("failed to read values file: %v", err)
				} else if !strings.Contains(string(content), tt.wantContains) {
					t.Errorf("values file content = %q, want to contain %q", string(content), tt.wantContains)
				}
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
	defer func() {
		_ = os.RemoveAll(tempDir)
	}()

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
