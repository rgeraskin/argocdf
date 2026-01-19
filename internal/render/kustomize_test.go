package render

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rgeraskin/argocdf/internal/cluster"
	"github.com/rgeraskin/argocdf/internal/types"
)

func TestMapToEditAddArgs(t *testing.T) {
	tests := []struct {
		name     string
		input    map[string]string
		expected []string
	}{
		{
			name:     "single entry",
			input:    map[string]string{"app": "myapp"},
			expected: []string{"app:myapp"},
		},
		{
			name:     "multiple entries sorted",
			input:    map[string]string{"zebra": "z", "alpha": "a", "beta": "b"},
			expected: []string{"alpha:a", "beta:b", "zebra:z"},
		},
		{
			name:     "empty map",
			input:    map[string]string{},
			expected: []string{},
		},
		{
			name:     "values with special characters",
			input:    map[string]string{"key": "value-with-dashes", "num": "123"},
			expected: []string{"key:value-with-dashes", "num:123"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mapToEditAddArgs(tt.input)

			if len(result) != len(tt.expected) {
				t.Errorf("mapToEditAddArgs() returned %d args, want %d", len(result), len(tt.expected))
				return
			}

			for i, arg := range result {
				if arg != tt.expected[i] {
					t.Errorf("mapToEditAddArgs()[%d] = %q, want %q", i, arg, tt.expected[i])
				}
			}
		})
	}
}

func TestIsYAMLFile(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		expected bool
	}{
		{"yaml extension", "deployment.yaml", true},
		{"yml extension", "service.yml", true},
		{"json extension", "config.json", false},
		{"no extension", "Makefile", false},
		{"yaml in middle", "chart.yaml.bak", false},
		{"uppercase YAML", "CONFIG.YAML", false}, // ext is case-sensitive
		{"hidden yaml file", ".hidden.yaml", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isYAMLFile(tt.filename)
			if result != tt.expected {
				t.Errorf("isYAMLFile(%q) = %v, want %v", tt.filename, result, tt.expected)
			}
		})
	}
}

func TestHasKustomization(t *testing.T) {
	renderer := &KustomizeRenderer{}

	// Create temp directory
	tempDir, err := os.MkdirTemp("", "kustomize-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Test directory without kustomization
	if renderer.hasKustomization(tempDir) {
		t.Error("hasKustomization() returned true for directory without kustomization")
	}

	// Test with kustomization.yaml
	kustomization := filepath.Join(tempDir, "kustomization.yaml")
	if err := os.WriteFile(kustomization, []byte("resources:\n- deployment.yaml\n"), 0644); err != nil {
		t.Fatalf("failed to write kustomization.yaml: %v", err)
	}

	if !renderer.hasKustomization(tempDir) {
		t.Error("hasKustomization() returned false for directory with kustomization.yaml")
	}
}

func TestFindKustomizationFile(t *testing.T) {
	renderer := &KustomizeRenderer{}

	tests := []struct {
		name     string
		files    []string
		expected string
	}{
		{
			name:     "kustomization.yaml",
			files:    []string{"kustomization.yaml"},
			expected: "kustomization.yaml",
		},
		{
			name:     "kustomization.yml",
			files:    []string{"kustomization.yml"},
			expected: "kustomization.yml",
		},
		{
			name:     "Kustomization",
			files:    []string{"Kustomization"},
			expected: "Kustomization",
		},
		{
			name:     "prefers kustomization.yaml over others",
			files:    []string{"Kustomization", "kustomization.yml", "kustomization.yaml"},
			expected: "kustomization.yaml",
		},
		{
			name:     "no kustomization file",
			files:    []string{"deployment.yaml", "service.yaml"},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempDir, err := os.MkdirTemp("", "kustomize-find-*")
			if err != nil {
				t.Fatalf("failed to create temp dir: %v", err)
			}
			defer os.RemoveAll(tempDir)

			// Create the test files
			for _, file := range tt.files {
				path := filepath.Join(tempDir, file)
				if err := os.WriteFile(path, []byte("test"), 0644); err != nil {
					t.Fatalf("failed to write %s: %v", file, err)
				}
			}

			result := renderer.findKustomizationFile(tempDir)
			if result != tt.expected {
				t.Errorf("findKustomizationFile() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestKustomizationNames(t *testing.T) {
	// Verify the expected kustomization file names
	expected := []string{"kustomization.yaml", "kustomization.yml", "Kustomization"}

	if len(KustomizationNames) != len(expected) {
		t.Errorf("KustomizationNames has %d entries, want %d", len(KustomizationNames), len(expected))
	}

	for i, name := range expected {
		if i >= len(KustomizationNames) {
			break
		}
		if KustomizationNames[i] != name {
			t.Errorf("KustomizationNames[%d] = %q, want %q", i, KustomizationNames[i], name)
		}
	}
}

func TestIsKustomizeDirectory(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "kustomize-dir-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Without kustomization file
	if IsKustomizeDirectory(tempDir) {
		t.Error("IsKustomizeDirectory() returned true for non-kustomize directory")
	}

	// With kustomization file
	if err := os.WriteFile(filepath.Join(tempDir, "kustomization.yaml"), []byte("resources: []"), 0644); err != nil {
		t.Fatalf("failed to write kustomization.yaml: %v", err)
	}

	if !IsKustomizeDirectory(tempDir) {
		t.Error("IsKustomizeDirectory() returned false for kustomize directory")
	}
}

func TestRenderPlainYAML(t *testing.T) {
	renderer := &KustomizeRenderer{}

	tempDir, err := os.MkdirTemp("", "plain-yaml-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create some YAML files
	yaml1 := `apiVersion: v1
kind: ConfigMap
metadata:
  name: cm1`

	yaml2 := `apiVersion: v1
kind: ConfigMap
metadata:
  name: cm2`

	if err := os.WriteFile(filepath.Join(tempDir, "a-configmap.yaml"), []byte(yaml1), 0644); err != nil {
		t.Fatalf("failed to write yaml1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tempDir, "b-configmap.yaml"), []byte(yaml2), 0644); err != nil {
		t.Fatalf("failed to write yaml2: %v", err)
	}
	// Create a non-yaml file that should be ignored
	if err := os.WriteFile(filepath.Join(tempDir, "README.md"), []byte("# Test"), 0644); err != nil {
		t.Fatalf("failed to write README: %v", err)
	}

	result, err := renderer.renderPlainYAML(tempDir)
	if err != nil {
		t.Fatalf("renderPlainYAML() error = %v", err)
	}

	content := string(result)

	// Should contain both ConfigMaps
	if !strings.Contains(content, "name: cm1") {
		t.Error("renderPlainYAML() missing cm1")
	}
	if !strings.Contains(content, "name: cm2") {
		t.Error("renderPlainYAML() missing cm2")
	}

	// Should have document separator between them
	if !strings.Contains(content, "---") {
		t.Error("renderPlainYAML() missing document separator")
	}

	// Should NOT contain README content
	if strings.Contains(content, "# Test") {
		t.Error("renderPlainYAML() included non-YAML file")
	}
}

func TestSourceType(t *testing.T) {
	renderer := NewKustomizeRenderer(RenderOptions{})
	if renderer.SourceType() != types.SourceTypeKustomize {
		t.Errorf("SourceType() = %v, want %v", renderer.SourceType(), types.SourceTypeKustomize)
	}
}

func TestApplyPatches(t *testing.T) {
	renderer := &KustomizeRenderer{}

	tests := []struct {
		name            string
		initialContent  string
		patches         []cluster.KustomizePatch
		wantInResult    []string
		wantNotInResult []string
	}{
		{
			name: "add patch to empty patches list",
			initialContent: `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- deployment.yaml
`,
			patches: []cluster.KustomizePatch{
				{
					Patch: `- op: replace
  path: /spec/replicas
  value: 3`,
				},
			},
			wantInResult: []string{"patches:", "op: replace", "path: /spec/replicas"},
		},
		{
			name: "append to existing patches",
			initialContent: `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- deployment.yaml
patches:
- patch: existing-patch
  target:
    kind: Service
`,
			patches: []cluster.KustomizePatch{
				{
					Patch: "new-patch",
				},
			},
			wantInResult: []string{"existing-patch", "new-patch", "kind: Service"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempDir, err := os.MkdirTemp("", "kustomize-patches-*")
			if err != nil {
				t.Fatalf("failed to create temp dir: %v", err)
			}
			defer os.RemoveAll(tempDir)

			// Write initial kustomization.yaml
			kustFile := filepath.Join(tempDir, "kustomization.yaml")
			if err := os.WriteFile(kustFile, []byte(tt.initialContent), 0644); err != nil {
				t.Fatalf("failed to write kustomization.yaml: %v", err)
			}

			// Apply patches
			err = renderer.applyPatches(tempDir, tt.patches)
			if err != nil {
				t.Fatalf("applyPatches() error = %v", err)
			}

			// Read result
			result, err := os.ReadFile(kustFile)
			if err != nil {
				t.Fatalf("failed to read result: %v", err)
			}
			content := string(result)

			for _, want := range tt.wantInResult {
				if !strings.Contains(content, want) {
					t.Errorf("result missing %q, got:\n%s", want, content)
				}
			}

			for _, notWant := range tt.wantNotInResult {
				if strings.Contains(content, notWant) {
					t.Errorf("result should not contain %q, got:\n%s", notWant, content)
				}
			}
		})
	}
}

func TestApplyPatchesNoKustomization(t *testing.T) {
	renderer := &KustomizeRenderer{}

	tempDir, err := os.MkdirTemp("", "kustomize-no-file-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Try to apply patches without kustomization file
	err = renderer.applyPatches(tempDir, []cluster.KustomizePatch{})
	if err == nil {
		t.Error("applyPatches() should fail when no kustomization file exists")
	}
}

// skipIfNoKustomize skips the test if kustomize is not installed
func skipIfNoKustomize(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("kustomize"); err != nil {
		t.Skip("kustomize not installed, skipping integration test")
	}
}

func TestCanRender(t *testing.T) {
	skipIfNoKustomize(t)

	renderer := NewKustomizeRenderer(RenderOptions{})
	if err := renderer.CanRender(); err != nil {
		t.Errorf("CanRender() error = %v", err)
	}
}

func TestKustomizeEditNamePrefix(t *testing.T) {
	skipIfNoKustomize(t)

	renderer := &KustomizeRenderer{}

	tempDir, err := os.MkdirTemp("", "kustomize-nameprefix-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create kustomization.yaml
	kustContent := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources: []
`
	if err := os.WriteFile(filepath.Join(tempDir, "kustomization.yaml"), []byte(kustContent), 0644); err != nil {
		t.Fatalf("failed to write kustomization.yaml: %v", err)
	}

	// Run kustomize edit set nameprefix
	err = renderer.runKustomizeEdit(tempDir, "set", "nameprefix", "--", "test-")
	if err != nil {
		t.Fatalf("runKustomizeEdit() error = %v", err)
	}

	// Read and verify
	result, err := os.ReadFile(filepath.Join(tempDir, "kustomization.yaml"))
	if err != nil {
		t.Fatalf("failed to read result: %v", err)
	}

	if !strings.Contains(string(result), "namePrefix: test-") {
		t.Errorf("kustomization.yaml missing namePrefix, got:\n%s", result)
	}
}

func TestKustomizeEditImages(t *testing.T) {
	skipIfNoKustomize(t)

	renderer := &KustomizeRenderer{}

	tempDir, err := os.MkdirTemp("", "kustomize-images-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create kustomization.yaml
	kustContent := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources: []
`
	if err := os.WriteFile(filepath.Join(tempDir, "kustomization.yaml"), []byte(kustContent), 0644); err != nil {
		t.Fatalf("failed to write kustomization.yaml: %v", err)
	}

	// Run kustomize edit set image
	err = renderer.runKustomizeEdit(tempDir, "set", "image", "nginx=nginx:1.21")
	if err != nil {
		t.Fatalf("runKustomizeEdit() error = %v", err)
	}

	// Read and verify
	result, err := os.ReadFile(filepath.Join(tempDir, "kustomization.yaml"))
	if err != nil {
		t.Fatalf("failed to read result: %v", err)
	}

	if !strings.Contains(string(result), "images:") || !strings.Contains(string(result), "newTag: \"1.21\"") {
		t.Errorf("kustomization.yaml missing images, got:\n%s", result)
	}
}

func TestKustomizeEditLabels(t *testing.T) {
	skipIfNoKustomize(t)

	renderer := &KustomizeRenderer{}

	tempDir, err := os.MkdirTemp("", "kustomize-labels-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create kustomization.yaml
	kustContent := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources: []
`
	if err := os.WriteFile(filepath.Join(tempDir, "kustomization.yaml"), []byte(kustContent), 0644); err != nil {
		t.Fatalf("failed to write kustomization.yaml: %v", err)
	}

	// Run kustomize edit add label
	err = renderer.runKustomizeEdit(tempDir, "add", "label", "app:myapp")
	if err != nil {
		t.Fatalf("runKustomizeEdit() error = %v", err)
	}

	// Read and verify
	result, err := os.ReadFile(filepath.Join(tempDir, "kustomization.yaml"))
	if err != nil {
		t.Fatalf("failed to read result: %v", err)
	}

	if !strings.Contains(string(result), "commonLabels:") || !strings.Contains(string(result), "app: myapp") {
		t.Errorf("kustomization.yaml missing labels, got:\n%s", result)
	}
}

func TestKustomizeEditNamespace(t *testing.T) {
	skipIfNoKustomize(t)

	renderer := &KustomizeRenderer{}

	tempDir, err := os.MkdirTemp("", "kustomize-namespace-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create kustomization.yaml
	kustContent := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources: []
`
	if err := os.WriteFile(filepath.Join(tempDir, "kustomization.yaml"), []byte(kustContent), 0644); err != nil {
		t.Fatalf("failed to write kustomization.yaml: %v", err)
	}

	// Run kustomize edit set namespace
	err = renderer.runKustomizeEdit(tempDir, "set", "namespace", "--", "production")
	if err != nil {
		t.Fatalf("runKustomizeEdit() error = %v", err)
	}

	// Read and verify
	result, err := os.ReadFile(filepath.Join(tempDir, "kustomization.yaml"))
	if err != nil {
		t.Fatalf("failed to read result: %v", err)
	}

	if !strings.Contains(string(result), "namespace: production") {
		t.Errorf("kustomization.yaml missing namespace, got:\n%s", result)
	}
}

func TestKustomizeRenderWithOptions(t *testing.T) {
	skipIfNoKustomize(t)

	renderer := NewKustomizeRenderer(RenderOptions{})

	tempDir, err := os.MkdirTemp("", "kustomize-render-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create a simple kustomization with a deployment
	kustContent := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- deployment.yaml
`
	deployContent := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: myapp
spec:
  replicas: 1
  selector:
    matchLabels:
      app: myapp
  template:
    metadata:
      labels:
        app: myapp
    spec:
      containers:
      - name: myapp
        image: nginx:latest
`

	if err := os.WriteFile(filepath.Join(tempDir, "kustomization.yaml"), []byte(kustContent), 0644); err != nil {
		t.Fatalf("failed to write kustomization.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tempDir, "deployment.yaml"), []byte(deployContent), 0644); err != nil {
		t.Fatalf("failed to write deployment.yaml: %v", err)
	}

	// Create Application with Kustomize options
	app := &cluster.Application{}
	app.Name = "test-app"

	source := &cluster.ApplicationSource{
		Path: ".",
		Kustomize: &cluster.ApplicationSourceKustomize{
			NamePrefix: "prod-",
			CommonLabels: map[string]string{
				"env": "production",
			},
		},
	}

	// Render
	result, err := renderer.Render(app, source, tempDir)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	content := string(result)

	// Verify namePrefix was applied
	if !strings.Contains(content, "name: prod-myapp") {
		t.Errorf("namePrefix not applied, got:\n%s", content)
	}

	// Verify labels were applied
	if !strings.Contains(content, "env: production") {
		t.Errorf("labels not applied, got:\n%s", content)
	}
}

func TestBuildKustomizeArgs(t *testing.T) {
	tests := []struct {
		name     string
		opts     RenderOptions
		path     string
		expected []string
	}{
		{
			name:     "no options",
			opts:     RenderOptions{},
			path:     "/path/to/kustomize",
			expected: []string{"build", "/path/to/kustomize"},
		},
		{
			name: "enable helm",
			opts: RenderOptions{
				KustomizeEnableHelm: true,
			},
			path:     "/path/to/kustomize",
			expected: []string{"build", "/path/to/kustomize", "--enable-helm"},
		},
		{
			name: "load restrictor",
			opts: RenderOptions{
				KustomizeLoadRestrictor: "LoadRestrictionsNone",
			},
			path:     "/path/to/kustomize",
			expected: []string{"build", "/path/to/kustomize", "--load-restrictor", "LoadRestrictionsNone"},
		},
		{
			name: "single build option",
			opts: RenderOptions{
				KustomizeBuildOptions: "--reorder none",
			},
			path:     "/path/to/kustomize",
			expected: []string{"build", "/path/to/kustomize", "--reorder", "none"},
		},
		{
			name: "multiple build options",
			opts: RenderOptions{
				KustomizeBuildOptions: "--reorder none --enable-alpha-plugins",
			},
			path:     "/path/to/kustomize",
			expected: []string{"build", "/path/to/kustomize", "--reorder", "none", "--enable-alpha-plugins"},
		},
		{
			name: "all options combined",
			opts: RenderOptions{
				KustomizeEnableHelm:     true,
				KustomizeLoadRestrictor: "LoadRestrictionsNone",
				KustomizeBuildOptions:   "--reorder none",
			},
			path:     "/path/to/kustomize",
			expected: []string{"build", "/path/to/kustomize", "--enable-helm", "--load-restrictor", "LoadRestrictionsNone", "--reorder", "none"},
		},
		{
			name: "build options with extra spaces",
			opts: RenderOptions{
				KustomizeBuildOptions: "  --reorder   none  ",
			},
			path:     "/path/to/kustomize",
			expected: []string{"build", "/path/to/kustomize", "--reorder", "none"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			renderer := NewKustomizeRenderer(tt.opts)
			result := renderer.buildKustomizeArgs(tt.path)

			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("buildKustomizeArgs() = %v, want %v", result, tt.expected)
			}
		})
	}
}
