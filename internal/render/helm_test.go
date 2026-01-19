// Package render provides tests for Helm rendering functionality.
package render

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveValueFilePath(t *testing.T) {
	// Create temp directories for testing
	tempDir, err := os.MkdirTemp("", "helm-test-")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create subdirectories
	repoPath := filepath.Join(tempDir, "repo")
	chartDir := filepath.Join(repoPath, "charts", "myapp")
	refDir := filepath.Join(tempDir, "refs", "values-repo")

	for _, dir := range []string{repoPath, chartDir, refDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("failed to create dir %s: %v", dir, err)
		}
	}

	// Create test files
	for _, file := range []string{
		filepath.Join(chartDir, "values.yaml"),
		filepath.Join(chartDir, "values-prod.yaml"),
		filepath.Join(refDir, "common-values.yaml"),
	} {
		if err := os.WriteFile(file, []byte("key: value"), 0644); err != nil {
			t.Fatalf("failed to create file %s: %v", file, err)
		}
	}

	r := &HelmRenderer{
		opts: RenderOptions{
			RefSources: map[string]string{
				"values-ref": refDir,
			},
		},
	}

	tests := []struct {
		name       string
		path       string
		repoPath   string
		chartDir   string
		wantErr    bool
		wantSuffix string
	}{
		{
			name:       "relative path in chart dir",
			path:       "values.yaml",
			repoPath:   repoPath,
			chartDir:   chartDir,
			wantErr:    false,
			wantSuffix: "values.yaml",
		},
		{
			name:       "relative path with subdirectory",
			path:       "values-prod.yaml",
			repoPath:   repoPath,
			chartDir:   chartDir,
			wantErr:    false,
			wantSuffix: "values-prod.yaml",
		},
		{
			name:       "ref source path",
			path:       "$values-ref/common-values.yaml",
			repoPath:   repoPath,
			chartDir:   chartDir,
			wantErr:    false,
			wantSuffix: "common-values.yaml",
		},
		{
			name:     "path traversal blocked",
			path:     "../../../etc/passwd",
			repoPath: repoPath,
			chartDir: chartDir,
			wantErr:  true,
		},
		{
			name:     "ref source path traversal blocked",
			path:     "$values-ref/../../../etc/passwd",
			repoPath: repoPath,
			chartDir: chartDir,
			wantErr:  true,
		},
		{
			name:       "unknown ref source returned as-is",
			path:       "$unknown-ref/file.yaml",
			repoPath:   repoPath,
			chartDir:   chartDir,
			wantErr:    false,
			wantSuffix: "$unknown-ref/file.yaml", // Returned unchanged
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolved, err := r.resolveValueFilePath(tt.path, tt.repoPath, tt.chartDir)
			if tt.wantErr {
				if err == nil {
					t.Error("resolveValueFilePath() should return error")
				}
				return
			}
			if err != nil {
				t.Errorf("resolveValueFilePath() unexpected error = %v", err)
				return
			}
			if tt.wantSuffix != "" && !hasSuffix(resolved, tt.wantSuffix) {
				t.Errorf("resolveValueFilePath() = %q, want suffix %q", resolved, tt.wantSuffix)
			}
		})
	}
}

func hasSuffix(path, suffix string) bool {
	normPath := filepath.Clean(path)
	normSuffix := filepath.Clean(suffix)
	return len(normPath) >= len(normSuffix) &&
		normPath[len(normPath)-len(normSuffix):] == normSuffix
}

func TestParseKubeVersion(t *testing.T) {
	tests := []struct {
		name      string
		version   string
		wantMajor string
		wantMinor string
		wantErr   bool
	}{
		{
			name:      "standard version",
			version:   "1.28.0",
			wantMajor: "1",
			wantMinor: "28",
		},
		{
			name:      "version with v prefix",
			version:   "v1.27.5",
			wantMajor: "1",
			wantMinor: "27",
		},
		{
			name:      "short version",
			version:   "1.25",
			wantMajor: "1",
			wantMinor: "25",
		},
		{
			name:    "invalid version - no minor",
			version: "1",
			wantErr: true,
		},
		{
			name:    "empty version",
			version: "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			major, minor, err := ParseKubeVersion(tt.version)
			if tt.wantErr {
				if err == nil {
					t.Error("ParseKubeVersion() should return error")
				}
				return
			}
			if err != nil {
				t.Errorf("ParseKubeVersion() unexpected error = %v", err)
				return
			}
			if major != tt.wantMajor {
				t.Errorf("ParseKubeVersion() major = %q, want %q", major, tt.wantMajor)
			}
			if minor != tt.wantMinor {
				t.Errorf("ParseKubeVersion() minor = %q, want %q", minor, tt.wantMinor)
			}
		})
	}
}

func TestHelmRenderer_CanRender(t *testing.T) {
	r := NewHelmRenderer(RenderOptions{})

	// This test depends on helm being installed
	// In CI, helm should be available
	err := r.CanRender()
	if err != nil {
		t.Skipf("helm not available: %v", err)
	}
}

func TestHelmRenderer_SourceType(t *testing.T) {
	r := NewHelmRenderer(RenderOptions{})
	if r.SourceType() != "helm" {
		t.Errorf("SourceType() = %v, want helm", r.SourceType())
	}
}

func TestNewHelmRenderer(t *testing.T) {
	opts := RenderOptions{
		KubeVersion: "1.28.0",
		RefSources:  map[string]string{"ref1": "/path/to/ref"},
	}

	r := NewHelmRenderer(opts)
	if r == nil {
		t.Fatal("NewHelmRenderer() returned nil")
	}
	if r.opts.KubeVersion != opts.KubeVersion {
		t.Errorf("opts.KubeVersion = %q, want %q", r.opts.KubeVersion, opts.KubeVersion)
	}
	if len(r.opts.RefSources) != len(opts.RefSources) {
		t.Error("opts.RefSources not copied correctly")
	}
}

func TestEnsureDependencies_NoChartYaml(t *testing.T) {
	// Create a temp directory without Chart.yaml
	tempDir, err := os.MkdirTemp("", "helm-deps-test-")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	r := NewHelmRenderer(RenderOptions{})

	// Should succeed silently when Chart.yaml doesn't exist
	err = r.ensureDependencies(context.TODO(), tempDir)
	if err != nil {
		t.Errorf("ensureDependencies() error = %v, want nil", err)
	}
}

func TestEnsureDependencies_NoDependencies(t *testing.T) {
	// Create a temp directory with Chart.yaml but no dependencies
	tempDir, err := os.MkdirTemp("", "helm-deps-test-")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	chartYaml := `
apiVersion: v2
name: test-chart
version: 1.0.0
`
	if err := os.WriteFile(filepath.Join(tempDir, "Chart.yaml"), []byte(chartYaml), 0644); err != nil {
		t.Fatalf("failed to create Chart.yaml: %v", err)
	}

	r := NewHelmRenderer(RenderOptions{})

	// Should succeed silently when no dependencies section
	err = r.ensureDependencies(context.TODO(), tempDir)
	if err != nil {
		t.Errorf("ensureDependencies() error = %v, want nil", err)
	}
}

func TestEnsureDependencies_ChartsExists(t *testing.T) {
	// Create a temp directory with Chart.yaml with dependencies and charts/ dir
	tempDir, err := os.MkdirTemp("", "helm-deps-test-")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	chartYaml := `
apiVersion: v2
name: test-chart
version: 1.0.0
dependencies:
  - name: nginx
    version: 1.0.0
    repository: https://charts.bitnami.com/bitnami
`
	if err := os.WriteFile(filepath.Join(tempDir, "Chart.yaml"), []byte(chartYaml), 0644); err != nil {
		t.Fatalf("failed to create Chart.yaml: %v", err)
	}

	// Create charts/ directory with content
	chartsDir := filepath.Join(tempDir, "charts")
	if err := os.MkdirAll(chartsDir, 0755); err != nil {
		t.Fatalf("failed to create charts dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(chartsDir, "nginx-1.0.0.tgz"), []byte("fake chart"), 0644); err != nil {
		t.Fatalf("failed to create chart file: %v", err)
	}

	r := NewHelmRenderer(RenderOptions{})

	// Should succeed without running helm dependency build (charts already exist)
	err = r.ensureDependencies(context.TODO(), tempDir)
	if err != nil {
		t.Errorf("ensureDependencies() error = %v, want nil", err)
	}
}
