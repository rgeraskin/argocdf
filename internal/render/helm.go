// Package render provides Helm chart rendering.
package render

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rgeraskin/argocdf/internal/cluster"
	"github.com/rgeraskin/argocdf/internal/types"
	"sigs.k8s.io/yaml"
)

// HelmRenderer renders Helm charts using the helm binary.
type HelmRenderer struct {
	opts RenderOptions
}

// NewHelmRenderer creates a new HelmRenderer.
func NewHelmRenderer(opts RenderOptions) *HelmRenderer {
	return &HelmRenderer{opts: opts}
}

// SourceType returns the source type for Helm.
func (r *HelmRenderer) SourceType() types.SourceType {
	return types.SourceTypeHelm
}

// Render renders a Helm chart source.
// The context can be used to cancel long-running helm template operations.
func (r *HelmRenderer) Render(ctx context.Context, app *cluster.Application, source *cluster.ApplicationSource, repoPath string) ([]byte, error) {
	// Check context before starting
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	// Determine chart location and build args
	args, tempDir, err := r.buildArgs(ctx, app, source, repoPath)
	if err != nil {
		return nil, err
	}

	if tempDir != "" {
		defer os.RemoveAll(tempDir)
	}

	// For local charts, ensure dependencies are built
	if source.Path != "" {
		chartPath := filepath.Join(repoPath, source.Path)
		if err := r.ensureDependencies(ctx, chartPath); err != nil {
			return nil, fmt.Errorf("failed to build dependencies: %w", err)
		}
	}

	// Run helm template with context
	cmd := exec.CommandContext(ctx, "helm", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("helm template failed: %v\nstderr: %s", err, stderr.String())
	}

	return stdout.Bytes(), nil
}

// buildArgs builds the helm template command arguments.
func (r *HelmRenderer) buildArgs(ctx context.Context, app *cluster.Application, source *cluster.ApplicationSource, repoPath string) ([]string, string, error) {
	var tempDir string

	// Determine release name
	releaseName := app.ObjectMeta.Name
	if source.Helm != nil && source.Helm.ReleaseName != "" {
		releaseName = source.Helm.ReleaseName
	}

	args := []string{"template", releaseName}

	// Determine chart location
	if source.Chart != "" {
		// Remote chart from repository
		chartRef, tempDirPath, err := r.handleRemoteChart(ctx, source)
		if err != nil {
			return nil, "", err
		}
		tempDir = tempDirPath
		args = append(args, chartRef)
	} else if source.Path != "" {
		// Local chart from repository
		chartPath := filepath.Join(repoPath, source.Path)
		args = append(args, chartPath)
	} else {
		return nil, "", fmt.Errorf("no chart or path specified in source")
	}

	// Add namespace
	namespace := app.Spec.Destination.Namespace
	if namespace != "" {
		args = append(args, "--namespace", namespace)
	}

	// Add Kubernetes version if specified
	if r.opts.KubeVersion != "" {
		args = append(args, "--kube-version", r.opts.KubeVersion)
	}

	// Add Helm-specific options
	if source.Helm != nil {
		chartDir := repoPath
		if source.Path != "" {
			chartDir = filepath.Join(repoPath, source.Path)
		}
		args = r.addHelmOptions(args, source.Helm, repoPath, chartDir)
	}

	return args, tempDir, nil
}

// handleRemoteChart handles fetching a chart from a remote repository.
func (r *HelmRenderer) handleRemoteChart(ctx context.Context, source *cluster.ApplicationSource) (string, string, error) {
	repoURL := source.RepoURL

	if strings.HasPrefix(repoURL, "oci://") {
		// OCI registry - helm can pull directly
		chartRef := repoURL + "/" + source.Chart
		if source.TargetRevision != "" && source.TargetRevision != "HEAD" {
			return chartRef + ":" + source.TargetRevision, "", nil
		}
		return chartRef, "", nil
	}

	// HTTP/HTTPS repo - need to add repo first
	// Create a temp directory for repo operations
	tempDir, err := os.MkdirTemp("", "argocdf-helm-")
	if err != nil {
		return "", "", fmt.Errorf("failed to create temp dir: %w", err)
	}

	// Generate a unique repo name
	repoName := "argocdf-temp-" + source.Chart

	// Add the repo with context
	addArgs := []string{"repo", "add", repoName, repoURL, "--force-update"}
	addCmd := exec.CommandContext(ctx, "helm", addArgs...)
	addCmd.Env = append(os.Environ(),
		"HELM_CACHE_HOME="+filepath.Join(tempDir, "cache"),
		"HELM_CONFIG_HOME="+filepath.Join(tempDir, "config"),
		"HELM_DATA_HOME="+filepath.Join(tempDir, "data"),
	)
	if output, err := addCmd.CombinedOutput(); err != nil {
		os.RemoveAll(tempDir)
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
		return "", "", fmt.Errorf("failed to add helm repo: %v\noutput: %s", err, output)
	}

	// Update the repo with context
	updateArgs := []string{"repo", "update", repoName}
	updateCmd := exec.CommandContext(ctx, "helm", updateArgs...)
	updateCmd.Env = addCmd.Env
	if output, err := updateCmd.CombinedOutput(); err != nil {
		os.RemoveAll(tempDir)
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
		return "", "", fmt.Errorf("failed to update helm repo: %v\noutput: %s", err, output)
	}

	chartRef := repoName + "/" + source.Chart
	return chartRef, tempDir, nil
}

// addHelmOptions adds Helm-specific options to the command arguments.
// repoPath is the repository root, chartDir is the chart directory (for relative path resolution).
func (r *HelmRenderer) addHelmOptions(args []string, helm *cluster.ApplicationSourceHelm, repoPath, chartDir string) []string {
	// Add value files
	for _, valueFile := range helm.ValueFiles {
		// Handle $ref references in value files
		resolvedPath := r.resolveValueFilePath(valueFile, repoPath, chartDir)
		args = append(args, "--values", resolvedPath)
	}

	// Add inline values (string)
	if helm.Values != "" {
		// Write inline values to a temp file
		tmpFile, err := os.CreateTemp("", "values-*.yaml")
		if err == nil {
			tmpFile.WriteString(helm.Values)
			tmpFile.Close()
			args = append(args, "--values", tmpFile.Name())
			// Note: temp file will be cleaned up by OS eventually
		}
	}

	// Add inline values object (structured)
	// ValuesObject is a runtime.RawExtension containing JSON bytes
	if helm.ValuesObject != nil && len(helm.ValuesObject.Raw) > 0 {
		// Convert JSON to YAML and write to a temp file
		valuesYAML, err := yaml.JSONToYAML(helm.ValuesObject.Raw)
		if err == nil {
			tmpFile, err := os.CreateTemp("", "values-object-*.yaml")
			if err == nil {
				tmpFile.Write(valuesYAML)
				tmpFile.Close()
				args = append(args, "--values", tmpFile.Name())
			}
		}
	}

	// Add parameter overrides
	for _, param := range helm.Parameters {
		if param.ForceString {
			args = append(args, "--set-string", fmt.Sprintf("%s=%s", param.Name, param.Value))
		} else {
			args = append(args, "--set", fmt.Sprintf("%s=%s", param.Name, param.Value))
		}
	}

	// Add file parameters
	for _, fileParam := range helm.FileParameters {
		resolvedPath := r.resolveValueFilePath(fileParam.Path, repoPath, chartDir)
		args = append(args, "--set-file", fmt.Sprintf("%s=%s", fileParam.Name, resolvedPath))
	}

	// Add helm version
	if helm.Version != "" {
		args = append(args, "--version", helm.Version)
	}

	return args
}

// resolveValueFilePath resolves a value file path, handling $ref references.
// repoPath is the repository root, chartDir is the chart directory.
// Relative paths are resolved relative to chartDir (matching ArgoCD behavior).
func (r *HelmRenderer) resolveValueFilePath(path, repoPath, chartDir string) string {
	// Check if path uses a $ref reference
	if strings.HasPrefix(path, "$") {
		// Format: $refname/path/to/file.yaml
		parts := strings.SplitN(path, "/", 2)
		if len(parts) == 2 {
			refName := strings.TrimPrefix(parts[0], "$")
			if refPath, ok := r.opts.RefSources[refName]; ok {
				return filepath.Join(refPath, parts[1])
			}
		}
	}

	// Regular path - make absolute if needed
	// Relative paths are resolved relative to the chart directory
	if !filepath.IsAbs(path) {
		return filepath.Join(chartDir, path)
	}
	return path
}

// CanRender checks if the helm binary is available.
func (r *HelmRenderer) CanRender() error {
	cmd := exec.Command("helm", "version", "--short")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("helm binary not found or not working: %w", err)
	}
	return nil
}

// ParseKubeVersion parses a Kubernetes version string.
func ParseKubeVersion(version string) (major, minor string, err error) {
	// Remove v prefix if present
	version = strings.TrimPrefix(version, "v")

	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("invalid kubernetes version: %s", version)
	}

	return parts[0], parts[1], nil
}

// ensureDependencies checks if the chart has dependencies and builds them if needed.
// It runs `helm dependency build` if:
// 1. Chart.yaml exists with a dependencies section
// 2. The charts/ directory is missing or empty
func (r *HelmRenderer) ensureDependencies(ctx context.Context, chartPath string) error {
	// Check if Chart.yaml exists
	chartYamlPath := filepath.Join(chartPath, "Chart.yaml")
	if _, err := os.Stat(chartYamlPath); os.IsNotExist(err) {
		// No Chart.yaml, nothing to do
		return nil
	}

	// Read Chart.yaml to check for dependencies
	chartYaml, err := os.ReadFile(chartYamlPath)
	if err != nil {
		return fmt.Errorf("failed to read Chart.yaml: %w", err)
	}

	// Simple check for dependencies section
	// We look for "dependencies:" at the start of a line
	if !strings.Contains(string(chartYaml), "\ndependencies:") &&
		!strings.HasPrefix(string(chartYaml), "dependencies:") {
		// No dependencies defined
		return nil
	}

	// Check if charts/ directory exists and has content
	chartsDir := filepath.Join(chartPath, "charts")
	if entries, err := os.ReadDir(chartsDir); err == nil && len(entries) > 0 {
		// charts/ exists and has files, dependencies already built
		return nil
	}

	// Run helm dependency build with context
	cmd := exec.CommandContext(ctx, "helm", "dependency", "build", chartPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("helm dependency build failed: %v\nstderr: %s", err, stderr.String())
	}

	return nil
}
