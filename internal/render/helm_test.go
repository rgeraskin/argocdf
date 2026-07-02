// Package render provides tests for Helm rendering functionality.
package render

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rgeraskin/argocdf/internal/cluster"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestResolveValueFilePath(t *testing.T) {
	// Create temp directories for testing
	tempDir, err := os.MkdirTemp("", "helm-test-")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(tempDir)
	}()

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
	defer func() {
		_ = os.RemoveAll(tempDir)
	}()

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
	defer func() {
		_ = os.RemoveAll(tempDir)
	}()

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

// Note: TestEnsureDependencies_ChartsExists was removed because it tested the old
// behavior where helm dependency build was skipped if charts/ had any content.
// This was a bug - partial dependency presence caused failures.
// Now we always run helm dependency build when dependencies are defined.

func TestHelmSkipRefresh(t *testing.T) {
	tests := []struct {
		name            string
		helmSkipRefresh bool
		wantSkipRefresh bool
	}{
		{
			name:            "skip refresh enabled",
			helmSkipRefresh: true,
			wantSkipRefresh: true,
		},
		{
			name:            "skip refresh disabled",
			helmSkipRefresh: false,
			wantSkipRefresh: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewHelmRenderer(RenderOptions{
				HelmSkipRefresh: tt.helmSkipRefresh,
			})

			if r.opts.HelmSkipRefresh != tt.wantSkipRefresh {
				t.Errorf("HelmSkipRefresh = %v, want %v", r.opts.HelmSkipRefresh, tt.wantSkipRefresh)
			}
		})
	}
}

// countArg counts how many times an argument appears in args.
func countArg(args []string, arg string) int {
	count := 0
	for _, a := range args {
		if a == arg {
			count++
		}
	}
	return count
}

// argValue returns the value following the first occurrence of flag in args,
// or "" if the flag is not present.
func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestBuildArgs_LocalChartIgnoresHelmVersion(t *testing.T) {
	// helm.Version is the Helm binary version ("3"), not a chart version,
	// so it must not be passed as --version.
	app := &cluster.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app"},
	}
	source := &cluster.ApplicationSource{
		RepoURL: "https://github.com/example/repo.git",
		Path:    "charts/myapp",
		Helm: &cluster.ApplicationSourceHelm{
			Version: "3",
		},
	}

	r := NewHelmRenderer(RenderOptions{})
	args, env, tempDir, tempFiles, err := r.buildArgs(context.TODO(), app, source, t.TempDir())
	if err != nil {
		t.Fatalf("buildArgs() unexpected error = %v", err)
	}
	if count := countArg(args, "--version"); count != 0 {
		t.Errorf("buildArgs() args = %v, want no --version flag, got %d", args, count)
	}
	if env != nil {
		t.Errorf("buildArgs() env = %v, want nil for local charts", env)
	}
	if tempDir != "" {
		t.Errorf("buildArgs() tempDir = %q, want empty for local charts", tempDir)
	}
	if len(tempFiles) != 0 {
		t.Errorf("buildArgs() tempFiles = %v, want none", tempFiles)
	}
}

func TestBuildArgs_OCIChart(t *testing.T) {
	tests := []struct {
		name           string
		targetRevision string
		helmVersion    string
		wantVersion    string
	}{
		{
			name:           "pinned revision becomes --version",
			targetRevision: "1.2.3",
			wantVersion:    "1.2.3",
		},
		{
			name:           "HEAD revision omits --version",
			targetRevision: "HEAD",
			wantVersion:    "",
		},
		{
			name:           "empty revision omits --version",
			targetRevision: "",
			wantVersion:    "",
		},
		{
			name:           "helm.Version does not produce a second --version",
			targetRevision: "1.2.3",
			helmVersion:    "3",
			wantVersion:    "1.2.3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := &cluster.Application{
				ObjectMeta: metav1.ObjectMeta{Name: "my-app"},
			}
			source := &cluster.ApplicationSource{
				RepoURL:        "oci://registry.example.com/charts",
				Chart:          "mychart",
				TargetRevision: tt.targetRevision,
			}
			if tt.helmVersion != "" {
				source.Helm = &cluster.ApplicationSourceHelm{Version: tt.helmVersion}
			}

			r := NewHelmRenderer(RenderOptions{})
			args, env, tempDir, _, err := r.buildArgs(context.TODO(), app, source, "")
			if err != nil {
				t.Fatalf("buildArgs() unexpected error = %v", err)
			}

			// Chart ref must be untagged; the version goes via --version
			wantRef := "oci://registry.example.com/charts/mychart"
			if countArg(args, wantRef) != 1 {
				t.Errorf("buildArgs() args = %v, want untagged chart ref %q", args, wantRef)
			}
			for _, a := range args {
				if strings.HasPrefix(a, wantRef+":") {
					t.Errorf("buildArgs() args = %v, chart ref must not include a tag", args)
				}
			}

			wantCount := 0
			if tt.wantVersion != "" {
				wantCount = 1
			}
			if count := countArg(args, "--version"); count != wantCount {
				t.Errorf("buildArgs() args = %v, want %d --version flag(s), got %d", args, wantCount, count)
			}
			if got := argValue(args, "--version"); got != tt.wantVersion {
				t.Errorf("buildArgs() --version = %q, want %q", got, tt.wantVersion)
			}

			// OCI charts don't need an isolated environment or temp dir
			if env != nil {
				t.Errorf("buildArgs() env = %v, want nil for OCI charts", env)
			}
			if tempDir != "" {
				t.Errorf("buildArgs() tempDir = %q, want empty for OCI charts", tempDir)
			}
		})
	}
}

func TestIsolatedHelmEnv(t *testing.T) {
	tempDir := t.TempDir()
	env := isolatedHelmEnv(tempDir)

	wantVars := map[string]string{
		"HELM_CACHE_HOME":  filepath.Join(tempDir, "cache"),
		"HELM_CONFIG_HOME": filepath.Join(tempDir, "config"),
		"HELM_DATA_HOME":   filepath.Join(tempDir, "data"),
	}
	for name, wantValue := range wantVars {
		found := false
		for _, entry := range env {
			if entry == name+"="+wantValue {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("isolatedHelmEnv() missing %s=%s in %v", name, wantValue, env)
		}
	}

	// The isolated env must extend the process environment, not replace it,
	// so helm still finds PATH, HOME, etc.
	if len(env) < len(os.Environ()) {
		t.Errorf("isolatedHelmEnv() len = %d, want at least %d (os.Environ())", len(env), len(os.Environ()))
	}
}

func TestHelmSkipRefresh_CommandArgs(t *testing.T) {
	// This test verifies that the HelmSkipRefresh option affects
	// the command arguments in ensureDependencies.
	// We can't easily test the actual command execution without mocking,
	// but we can verify the option is properly stored and would be used.

	tests := []struct {
		name            string
		helmSkipRefresh bool
	}{
		{
			name:            "option stored when true",
			helmSkipRefresh: true,
		},
		{
			name:            "option stored when false",
			helmSkipRefresh: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := RenderOptions{
				HelmSkipRefresh: tt.helmSkipRefresh,
			}
			r := NewHelmRenderer(opts)

			// Verify the option is properly stored
			if r.opts.HelmSkipRefresh != tt.helmSkipRefresh {
				t.Errorf("HelmSkipRefresh not properly stored: got %v, want %v",
					r.opts.HelmSkipRefresh, tt.helmSkipRefresh)
			}
		})
	}
}
