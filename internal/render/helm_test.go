// Package render provides tests for Helm rendering functionality.
package render

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rgeraskin/argocdf/internal/cluster"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestChartDepMutex verifies the keyed mutex used to serialize
// `helm dependency build` per chart path: the same path returns the same
// mutex, distinct paths return distinct mutexes, and the returned mutex
// actually serializes concurrent access (run with -race).
func TestChartDepMutex(t *testing.T) {
	// Same path -> same mutex instance.
	muA1 := chartDepMutex("/tmp/chart-a")
	muA2 := chartDepMutex("/tmp/chart-a")
	if muA1 != muA2 {
		t.Error("expected identical mutex for identical chart path")
	}
	// Distinct paths -> distinct mutexes.
	muB := chartDepMutex("/tmp/chart-b")
	if muA1 == muB {
		t.Error("expected distinct mutexes for distinct chart paths")
	}

	// The mutex must serialize concurrent access to a shared counter guarded by
	// the mutex for a single path. Under -race this fails if the mutex does not
	// actually protect the critical section.
	const goroutines = 50
	const increments = 200
	counter := 0
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < increments; j++ {
				mu := chartDepMutex("/tmp/shared-chart")
				mu.Lock()
				counter++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if want := goroutines * increments; counter != want {
		t.Errorf("counter = %d, want %d (lost updates indicate a broken mutex)", counter, want)
	}
}

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
func TestSanitizeKubeVersion(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    string
	}{
		{name: "gke suffix", version: "v1.29.5-gke.1091002", want: "1.29.5"},
		{name: "eks suffix", version: "v1.29.13-eks-abc123", want: "1.29.13"},
		{name: "plain", version: "1.28.3", want: "1.28.3"},
		{name: "v prefix", version: "v1.30.0", want: "1.30.0"},
		{name: "no patch", version: "1.29", want: "1.29"},
		{name: "v prefix no patch", version: "v1.29", want: "1.29"},
		{name: "build metadata", version: "1.29.5+build.7", want: "1.29.5"},
		{name: "whitespace", version: "  v1.29.5-gke.1  ", want: "1.29.5"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SanitizeKubeVersion(tt.version); got != tt.want {
				t.Errorf("SanitizeKubeVersion(%q) = %q, want %q", tt.version, got, tt.want)
			}
		})
	}
}

func TestBuildArgs_KubeVersionAndAPIVersions(t *testing.T) {
	app := &cluster.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app"},
	}
	source := &cluster.ApplicationSource{
		RepoURL: "https://github.com/example/repo.git",
		Path:    "charts/myapp",
	}

	opts := RenderOptions{
		KubeVersion: "v1.29.5-gke.1091002",
		APIVersions: []string{"v1", "networking.k8s.io/v1", "networking.k8s.io/v1/Ingress"},
	}
	r := NewHelmRenderer(opts)
	args, _, _, _, err := r.buildArgs(context.TODO(), app, source, t.TempDir())
	if err != nil {
		t.Fatalf("buildArgs() unexpected error = %v", err)
	}

	// --kube-version must be the sanitized bare version.
	if got := argValue(args, "--kube-version"); got != "1.29.5" {
		t.Errorf("buildArgs() --kube-version = %q, want %q", got, "1.29.5")
	}

	// One --api-versions flag per entry.
	if got := countArg(args, "--api-versions"); got != len(opts.APIVersions) {
		t.Errorf("buildArgs() --api-versions count = %d, want %d", got, len(opts.APIVersions))
	}
	for _, want := range opts.APIVersions {
		if countArg(args, want) != 1 {
			t.Errorf("buildArgs() args = %v, missing api-version %q", args, want)
		}
	}
}

func TestBuildArgs_NoAPIVersionsWhenEmpty(t *testing.T) {
	app := &cluster.Application{ObjectMeta: metav1.ObjectMeta{Name: "my-app"}}
	source := &cluster.ApplicationSource{
		RepoURL: "https://github.com/example/repo.git",
		Path:    "charts/myapp",
	}

	r := NewHelmRenderer(RenderOptions{KubeVersion: "1.29.0"})
	args, _, _, _, err := r.buildArgs(context.TODO(), app, source, t.TempDir())
	if err != nil {
		t.Fatalf("buildArgs() unexpected error = %v", err)
	}
	if count := countArg(args, "--api-versions"); count != 0 {
		t.Errorf("buildArgs() --api-versions count = %d, want 0 when list empty", count)
	}
}

func TestBuildArgs_SkipSchemaValidation(t *testing.T) {
	app := &cluster.Application{ObjectMeta: metav1.ObjectMeta{Name: "my-app"}}

	tests := []struct {
		name string
		helm *cluster.ApplicationSourceHelm
		want int
	}{
		{
			name: "set",
			helm: &cluster.ApplicationSourceHelm{SkipSchemaValidation: true},
			want: 1,
		},
		{
			name: "unset",
			helm: &cluster.ApplicationSourceHelm{},
			want: 0,
		},
		{
			name: "no helm block",
			helm: nil,
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := &cluster.ApplicationSource{
				RepoURL: "https://github.com/example/repo.git",
				Path:    "charts/myapp",
				Helm:    tt.helm,
			}
			r := NewHelmRenderer(RenderOptions{})
			args, _, _, _, err := r.buildArgs(context.TODO(), app, source, t.TempDir())
			if err != nil {
				t.Fatalf("buildArgs() unexpected error = %v", err)
			}
			if got := countArg(args, "--skip-schema-validation"); got != tt.want {
				t.Errorf("buildArgs() --skip-schema-validation count = %d, want %d\nargs: %v", got, tt.want, args)
			}
		})
	}
}

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

// TestDependencyRepoURLs verifies that only classic HTTP(S) repositories are
// collected from chart dependencies: oci://, file://, and @alias references
// are excluded, duplicates collapse (including trailing-slash variants of one
// URL), and the result is sorted and slash-normalized.
func TestDependencyRepoURLs(t *testing.T) {
	deps := []chartDependency{
		{Name: "cluster", Repository: "https://cloudnative-pg.github.io/charts"},
		{Name: "again", Repository: "https://cloudnative-pg.github.io/charts"},    // duplicate
		{Name: "slashed", Repository: "https://cloudnative-pg.github.io/charts/"}, // slash variant of the same repo
		{Name: "app", Repository: "oci://ghcr.io/example"},                        // OCI: no index needed
		{Name: "local", Repository: "file://../sibling-chart"},                    // local path
		{Name: "aliased", Repository: "@stable"},                                  // alias: needs an existing entry
		{Name: "insecure", Repository: "http://charts.internal.example"},
		{Name: "spaced", Repository: "  https://charts.example.io  "}, // trimmed
		{Name: "vendored", Repository: ""},                            // chart vendored in charts/
	}

	got := dependencyRepoURLs(deps)
	want := []string{
		"http://charts.internal.example",
		"https://charts.example.io",
		"https://cloudnative-pg.github.io/charts",
	}
	if len(got) != len(want) {
		t.Fatalf("dependencyRepoURLs() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("dependencyRepoURLs()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestDepRepoName verifies the derived repo name is deterministic per URL and
// distinct across URLs, so re-adds are no-ops and user repos can't be clobbered.
func TestDepRepoName(t *testing.T) {
	a1 := depRepoName("https://cloudnative-pg.github.io/charts")
	a2 := depRepoName("https://cloudnative-pg.github.io/charts")
	b := depRepoName("https://charts.example.io")

	if a1 != a2 {
		t.Errorf("depRepoName not deterministic: %q != %q", a1, a2)
	}
	if a1 == b {
		t.Errorf("depRepoName collision for distinct URLs: %q", a1)
	}
	if !strings.HasPrefix(a1, "argocdf-dep-") {
		t.Errorf("depRepoName = %q, want argocdf-dep- prefix", a1)
	}
}

// TestMissingRepoErr covers both helm failure messages for an unregistered
// dependency repository.
func TestMissingRepoErr(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
		want   bool
	}{
		{
			name: "index never downloaded",
			stderr: "Error: no cached repository for helm-manager-b8d1a67b found. " +
				"(try 'helm repo update')",
			want: true,
		},
		{
			name:   "repo not in repositories.yaml",
			stderr: "Error: no repository definition for https://cloudnative-pg.github.io/charts.",
			want:   true,
		},
		{
			name:   "unrelated failure",
			stderr: "Error: can't get a valid version for repositories postgresql",
			want:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := missingRepoErr(tt.stderr); got != tt.want {
				t.Errorf("missingRepoErr(%q) = %v, want %v", tt.stderr, got, tt.want)
			}
		})
	}
}

// TestDependencyBuildError verifies the actionable hint: present (with repo
// URLs and the --helm-add-repos pointer) on a missing-repo failure, absent on
// unrelated failures.
func TestDependencyBuildError(t *testing.T) {
	baseErr := fmt.Errorf("exit status 1")
	repos := []string{"https://cloudnative-pg.github.io/charts"}

	t.Run("missing repo gets hint", func(t *testing.T) {
		err := dependencyBuildError(baseErr,
			"Error: no cached repository for helm-manager-b8d1 found. (try 'helm repo update')",
			repos)
		msg := err.Error()
		for _, want := range []string{
			"helm dependency build failed",
			"hint:",
			"https://cloudnative-pg.github.io/charts",
			"--helm-add-repos",
			"ARGOCDF_HELM_ADD_REPOS",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("error message missing %q:\n%s", want, msg)
			}
		}
	})

	t.Run("missing repo without parsed URLs still hints", func(t *testing.T) {
		err := dependencyBuildError(baseErr,
			"Error: no repository definition for https://charts.example.io.", nil)
		msg := err.Error()
		if !strings.Contains(msg, "hint:") || !strings.Contains(msg, "--helm-add-repos") {
			t.Errorf("expected generic hint, got:\n%s", msg)
		}
	})

	t.Run("unrelated failure gets no hint", func(t *testing.T) {
		err := dependencyBuildError(baseErr, "Error: something else entirely", repos)
		if strings.Contains(err.Error(), "hint:") {
			t.Errorf("unexpected hint on unrelated failure:\n%s", err.Error())
		}
	})
}

// TestEnsureDependencies_AddRepos drives the full missing-repo scenario against
// a real helm binary with an isolated helm home (no network: the dependency
// repo is a local httptest server). It pins both sides of --helm-add-repos:
// without it a fresh environment fails with the actionable hint; with it
// argocdf registers the repo and the build succeeds.
func TestEnsureDependencies_AddRepos(t *testing.T) {
	if err := exec.Command("helm", "version", "--short").Run(); err != nil {
		t.Skip("helm binary not available")
	}

	// Build a dependency chart and a repo index for it, then serve both.
	repoDir := t.TempDir()
	depChartDir := filepath.Join(repoDir, "dep-src")
	if err := os.MkdirAll(depChartDir, 0o755); err != nil {
		t.Fatal(err)
	}
	depChartYaml := `apiVersion: v2
name: depchart
version: 0.1.0
`
	if err := os.WriteFile(filepath.Join(depChartDir, "Chart.yaml"), []byte(depChartYaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("helm", "package", depChartDir, "--destination", repoDir).CombinedOutput(); err != nil {
		t.Fatalf("helm package: %v\n%s", err, out)
	}

	srv := httptest.NewServer(http.FileServer(http.Dir(repoDir)))
	defer srv.Close()

	if out, err := exec.Command("helm", "repo", "index", repoDir, "--url", srv.URL).CombinedOutput(); err != nil {
		t.Fatalf("helm repo index: %v\n%s", err, out)
	}

	// Parent chart depending on the served repo.
	parentDir := t.TempDir()
	parentChartYaml := fmt.Sprintf(`apiVersion: v2
name: parent
version: 0.1.0
dependencies:
  - name: depchart
    version: 0.1.0
    repository: %s
`, srv.URL)
	if err := os.WriteFile(filepath.Join(parentDir, "Chart.yaml"), []byte(parentChartYaml), 0o644); err != nil {
		t.Fatal(err)
	}

	// Isolate helm state so the test neither sees nor touches the user's repos.
	// Child helm processes inherit these via os.Environ().
	helmHome := t.TempDir()
	t.Setenv("HELM_CONFIG_HOME", filepath.Join(helmHome, "config"))
	t.Setenv("HELM_CACHE_HOME", filepath.Join(helmHome, "cache"))
	t.Setenv("HELM_DATA_HOME", filepath.Join(helmHome, "data"))

	// Without --helm-add-repos: fresh environment, repo unknown -> actionable hint.
	r := NewHelmRenderer(RenderOptions{HelmSkipRefresh: true})
	err := r.ensureDependencies(context.TODO(), parentDir)
	if err == nil {
		t.Fatal("expected missing-repo error without HelmAddRepos, got nil")
	}
	for _, want := range []string{"hint:", "--helm-add-repos", srv.URL} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%s", want, err.Error())
		}
	}

	// With --helm-add-repos: argocdf registers the repo, build succeeds.
	r = NewHelmRenderer(RenderOptions{HelmSkipRefresh: true, HelmAddRepos: true})
	if err := r.ensureDependencies(context.TODO(), parentDir); err != nil {
		t.Fatalf("ensureDependencies with HelmAddRepos: %v", err)
	}
	if _, err := os.Stat(filepath.Join(parentDir, "charts", "depchart-0.1.0.tgz")); err != nil {
		t.Errorf("dependency not vendored into charts/: %v", err)
	}
}

// TestFindRepoByURL verifies URL matching against registered repos: exact
// match, trailing-slash insensitivity (both directions), and no match.
func TestFindRepoByURL(t *testing.T) {
	repos := []repoListEntry{
		{Name: "cnpg", URL: "https://cloudnative-pg.github.io/charts"},
		{Name: "vm", URL: "https://victoriametrics.github.io/helm-charts/"},
	}
	tests := []struct {
		name string
		url  string
		want string
	}{
		{name: "exact", url: "https://cloudnative-pg.github.io/charts", want: "cnpg"},
		{name: "query has trailing slash", url: "https://cloudnative-pg.github.io/charts/", want: "cnpg"},
		{name: "entry has trailing slash", url: "https://victoriametrics.github.io/helm-charts", want: "vm"},
		{name: "no match", url: "https://charts.example.io", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := findRepoByURL(repos, tt.url); got != tt.want {
				t.Errorf("findRepoByURL(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

// TestEnsureDependencies_ReusesExistingRepo verifies that --helm-add-repos
// does not pollute repositories.yaml when the dependency's repo URL is already
// registered under a human name: the existing entry is refreshed and reused
// (even across a trailing-slash difference), and no argocdf-dep-* entry is
// added.
func TestEnsureDependencies_ReusesExistingRepo(t *testing.T) {
	if err := exec.Command("helm", "version", "--short").Run(); err != nil {
		t.Skip("helm binary not available")
	}

	// Local chart repo (no network), same setup as TestEnsureDependencies_AddRepos.
	repoDir := t.TempDir()
	depChartDir := filepath.Join(repoDir, "dep-src")
	if err := os.MkdirAll(depChartDir, 0o755); err != nil {
		t.Fatal(err)
	}
	depChartYaml := `apiVersion: v2
name: depchart
version: 0.1.0
`
	if err := os.WriteFile(filepath.Join(depChartDir, "Chart.yaml"), []byte(depChartYaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("helm", "package", depChartDir, "--destination", repoDir).CombinedOutput(); err != nil {
		t.Fatalf("helm package: %v\n%s", err, out)
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(repoDir)))
	defer srv.Close()
	if out, err := exec.Command("helm", "repo", "index", repoDir, "--url", srv.URL).CombinedOutput(); err != nil {
		t.Fatalf("helm repo index: %v\n%s", err, out)
	}

	// The chart references the repo WITH a trailing slash while the user's
	// entry (added below) has none — reuse must match across that difference.
	parentDir := t.TempDir()
	parentChartYaml := fmt.Sprintf(`apiVersion: v2
name: parent
version: 0.1.0
dependencies:
  - name: depchart
    version: 0.1.0
    repository: %s/
`, srv.URL)
	if err := os.WriteFile(filepath.Join(parentDir, "Chart.yaml"), []byte(parentChartYaml), 0o644); err != nil {
		t.Fatal(err)
	}

	// Isolated helm home with the repo pre-registered under a human name,
	// simulating a developer machine.
	helmHome := t.TempDir()
	t.Setenv("HELM_CONFIG_HOME", filepath.Join(helmHome, "config"))
	t.Setenv("HELM_CACHE_HOME", filepath.Join(helmHome, "cache"))
	t.Setenv("HELM_DATA_HOME", filepath.Join(helmHome, "data"))
	if out, err := exec.Command("helm", "repo", "add", "myrepo", srv.URL).CombinedOutput(); err != nil {
		t.Fatalf("helm repo add myrepo: %v\n%s", err, out)
	}

	r := NewHelmRenderer(RenderOptions{HelmSkipRefresh: true, HelmAddRepos: true})
	if err := r.ensureDependencies(context.TODO(), parentDir); err != nil {
		t.Fatalf("ensureDependencies: %v", err)
	}
	if _, err := os.Stat(filepath.Join(parentDir, "charts", "depchart-0.1.0.tgz")); err != nil {
		t.Errorf("dependency not vendored into charts/: %v", err)
	}

	// repositories.yaml must still hold exactly the user's entry: reuse means
	// no argocdf-dep-* pollution.
	out, err := exec.Command("helm", "repo", "list", "-o", "json").Output()
	if err != nil {
		t.Fatalf("helm repo list: %v", err)
	}
	var repos []repoListEntry
	if err := json.Unmarshal(out, &repos); err != nil {
		t.Fatalf("parse repo list: %v", err)
	}
	if len(repos) != 1 || repos[0].Name != "myrepo" {
		t.Errorf("repositories polluted: %+v, want only 'myrepo'", repos)
	}
}

// TestRegisterDependencyRepo_FailurePropagatesAndCaches drives the flag-on
// path against an unreachable repo: ensureDependencies must surface the
// registration failure (not a confusing downstream build error), and a retry
// for the same URL within the process must return the cached error without
// re-attempting (once-per-URL semantics).
func TestRegisterDependencyRepo_FailurePropagatesAndCaches(t *testing.T) {
	if err := exec.Command("helm", "version", "--short").Run(); err != nil {
		t.Skip("helm binary not available")
	}

	// A server that is immediately closed: connection refused, fails fast.
	srv := httptest.NewServer(http.NotFoundHandler())
	deadURL := srv.URL
	srv.Close()

	chartDir := t.TempDir()
	chartYaml := fmt.Sprintf(`apiVersion: v2
name: parent
version: 0.1.0
dependencies:
  - name: depchart
    version: 0.1.0
    repository: %s
`, deadURL)
	if err := os.WriteFile(filepath.Join(chartDir, "Chart.yaml"), []byte(chartYaml), 0o644); err != nil {
		t.Fatal(err)
	}

	helmHome := t.TempDir()
	t.Setenv("HELM_CONFIG_HOME", filepath.Join(helmHome, "config"))
	t.Setenv("HELM_CACHE_HOME", filepath.Join(helmHome, "cache"))
	t.Setenv("HELM_DATA_HOME", filepath.Join(helmHome, "data"))

	r := NewHelmRenderer(RenderOptions{HelmSkipRefresh: true, HelmAddRepos: true})
	err1 := r.ensureDependencies(context.TODO(), chartDir)
	if err1 == nil {
		t.Fatal("expected registration error for unreachable repo, got nil")
	}
	if !strings.Contains(err1.Error(), "failed for dependency repo "+deadURL) {
		t.Errorf("error does not identify the failing repo:\n%s", err1.Error())
	}

	// Same URL again (second chart in the same run): the exact cached error
	// value must come back — pointer identity proves no re-attempt happened
	// (ensureDependencies returns the registration error unwrapped).
	err2 := r.registerDependencyRepo(context.TODO(), deadURL)
	if err2 != err1 { //nolint:errorlint // identity, not equivalence, is the point
		t.Errorf("expected the cached error value, got a different one:\nfirst:  %v\nsecond: %v", err1, err2)
	}
}

// TestEnsureDependencies_MalformedChartYaml verifies the deliberate
// degradation: an unparsable Chart.yaml that still contains a dependencies
// section yields helm's own build error (no panic, no bogus repo hint).
func TestEnsureDependencies_MalformedChartYaml(t *testing.T) {
	if err := exec.Command("helm", "version", "--short").Run(); err != nil {
		t.Skip("helm binary not available")
	}

	chartDir := t.TempDir()
	// Tab indentation is invalid YAML; the "dependencies:" line still trips
	// the quick string check, so dependency build runs and helm reports the
	// parse failure itself.
	chartYaml := "apiVersion: v2\nname: broken\nversion: 0.1.0\ndependencies:\n\t- name: x\n"
	if err := os.WriteFile(filepath.Join(chartDir, "Chart.yaml"), []byte(chartYaml), 0o644); err != nil {
		t.Fatal(err)
	}

	helmHome := t.TempDir()
	t.Setenv("HELM_CONFIG_HOME", filepath.Join(helmHome, "config"))
	t.Setenv("HELM_CACHE_HOME", filepath.Join(helmHome, "cache"))
	t.Setenv("HELM_DATA_HOME", filepath.Join(helmHome, "data"))

	r := NewHelmRenderer(RenderOptions{HelmSkipRefresh: true, HelmAddRepos: true})
	err := r.ensureDependencies(context.TODO(), chartDir)
	if err == nil {
		t.Fatal("expected error for malformed Chart.yaml, got nil")
	}
	if !strings.Contains(err.Error(), "helm dependency build failed") {
		t.Errorf("expected helm's build error, got:\n%s", err.Error())
	}
	if strings.Contains(err.Error(), "hint:") {
		t.Errorf("no repo hint should appear for a parse failure:\n%s", err.Error())
	}
}

// TestEnsureDependencies_ConcurrentDistinctRepos exercises the parallel-render
// scenario the reviews flagged: several charts, each depending on a DIFFERENT
// repository, building dependencies concurrently under one isolated helm home.
// All registrations and builds must succeed and repositories.yaml must end up
// with every repo (helm flocks writers, argocdf's helmRepoConfigMu excludes
// readers from the truncate-write window). Run with -race.
func TestEnsureDependencies_ConcurrentDistinctRepos(t *testing.T) {
	if err := exec.Command("helm", "version", "--short").Run(); err != nil {
		t.Skip("helm binary not available")
	}

	const nRepos = 4

	helmHome := t.TempDir()
	t.Setenv("HELM_CONFIG_HOME", filepath.Join(helmHome, "config"))
	t.Setenv("HELM_CACHE_HOME", filepath.Join(helmHome, "cache"))
	t.Setenv("HELM_DATA_HOME", filepath.Join(helmHome, "data"))

	// One dependency chart + repo index per server, one parent chart per repo.
	parents := make([]string, nRepos)
	for i := 0; i < nRepos; i++ {
		repoDir := t.TempDir()
		depChartDir := filepath.Join(repoDir, "dep-src")
		if err := os.MkdirAll(depChartDir, 0o755); err != nil {
			t.Fatal(err)
		}
		depChartYaml := fmt.Sprintf("apiVersion: v2\nname: depchart%d\nversion: 0.1.0\n", i)
		if err := os.WriteFile(filepath.Join(depChartDir, "Chart.yaml"), []byte(depChartYaml), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("helm", "package", depChartDir, "--destination", repoDir).CombinedOutput(); err != nil {
			t.Fatalf("helm package: %v\n%s", err, out)
		}
		srv := httptest.NewServer(http.FileServer(http.Dir(repoDir)))
		t.Cleanup(srv.Close)
		if out, err := exec.Command("helm", "repo", "index", repoDir, "--url", srv.URL).CombinedOutput(); err != nil {
			t.Fatalf("helm repo index: %v\n%s", err, out)
		}

		parentDir := t.TempDir()
		parentChartYaml := fmt.Sprintf(`apiVersion: v2
name: parent%d
version: 0.1.0
dependencies:
  - name: depchart%d
    version: 0.1.0
    repository: %s
`, i, i, srv.URL)
		if err := os.WriteFile(filepath.Join(parentDir, "Chart.yaml"), []byte(parentChartYaml), 0o644); err != nil {
			t.Fatal(err)
		}
		parents[i] = parentDir
	}

	r := NewHelmRenderer(RenderOptions{HelmSkipRefresh: true, HelmAddRepos: true})
	errs := make([]error, nRepos)
	var wg sync.WaitGroup
	for i := 0; i < nRepos; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = r.ensureDependencies(context.TODO(), parents[i])
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("chart %d: ensureDependencies: %v", i, err)
		}
	}

	out, err := exec.Command("helm", "repo", "list", "-o", "json").Output()
	if err != nil {
		t.Fatalf("helm repo list: %v", err)
	}
	var repos []repoListEntry
	if err := json.Unmarshal(out, &repos); err != nil {
		t.Fatalf("parse repo list: %v", err)
	}
	if len(repos) != nRepos {
		t.Errorf("repositories.yaml has %d entries, want %d: %+v", len(repos), nRepos, repos)
	}
}
