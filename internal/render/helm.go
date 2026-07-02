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
	args, env, tempDir, tempFiles, err := r.buildArgs(ctx, app, source, repoPath)
	if err != nil {
		return nil, err
	}

	// Cleanup temp resources after helm command completes
	if tempDir != "" {
		defer func() {
			_ = SafeRemoveAll(tempDir)
		}()
	}
	for _, f := range tempFiles {
		defer func(path string) {
			_ = os.Remove(path)
		}(f)
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
	if env != nil {
		// Use the isolated environment so helm sees the temp repo config
		// created by handleRemoteChart
		cmd.Env = env
	}
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
// Returns args, env (isolated environment for remote charts, nil otherwise),
// tempDir (for remote chart cleanup), tempFiles (for inline values cleanup), and error.
func (r *HelmRenderer) buildArgs(ctx context.Context, app *cluster.Application, source *cluster.ApplicationSource, repoPath string) ([]string, []string, string, []string, error) {
	var env []string
	var tempDir string
	var tempFiles []string

	// Determine release name
	releaseName := app.Name
	if source.Helm != nil && source.Helm.ReleaseName != "" {
		releaseName = source.Helm.ReleaseName
	}

	args := []string{"template", releaseName}

	// Determine chart location
	if source.Chart != "" {
		// Remote chart from repository
		chartRef, chartEnv, tempDirPath, err := r.handleRemoteChart(ctx, source)
		if err != nil {
			return nil, nil, "", nil, err
		}
		env = chartEnv
		tempDir = tempDirPath
		args = append(args, chartRef)
		// For remote charts, targetRevision is the chart version
		if source.TargetRevision != "" && source.TargetRevision != "HEAD" {
			args = append(args, "--version", source.TargetRevision)
		}
	} else if source.Path != "" {
		// Local chart from repository
		chartPath := filepath.Join(repoPath, source.Path)
		args = append(args, chartPath)
	} else {
		return nil, nil, "", nil, fmt.Errorf("no chart or path specified in source")
	}

	// Add namespace
	namespace := app.Spec.Destination.Namespace
	if namespace != "" {
		args = append(args, "--namespace", namespace)
	}

	// Add Kubernetes version if specified. The version is sanitized to a bare
	// major.minor.patch so vendor suffixes (e.g. "-gke.1091002") don't parse as
	// a semver prerelease and break chart constraints like ">=1.29.5".
	if r.opts.KubeVersion != "" {
		args = append(args, "--kube-version", SanitizeKubeVersion(r.opts.KubeVersion))
	}

	// Add discovered cluster API versions so charts can branch on
	// .Capabilities.APIVersions. Helm accepts repeated --api-versions flags.
	for _, v := range r.opts.APIVersions {
		args = append(args, "--api-versions", v)
	}

	// Add Helm-specific options
	if source.Helm != nil {
		chartDir := repoPath
		if source.Path != "" {
			chartDir = filepath.Join(repoPath, source.Path)
		}
		var err error
		args, tempFiles, err = r.addHelmOptions(args, source.Helm, repoPath, chartDir)
		if err != nil {
			// Cleanup any temp files created before the error
			for _, f := range tempFiles {
				_ = os.Remove(f)
			}
			if tempDir != "" {
				_ = SafeRemoveAll(tempDir)
			}
			return nil, nil, "", nil, err
		}
	}

	return args, env, tempDir, tempFiles, nil
}

// isolatedHelmEnv returns an environment with helm cache/config/data homes
// isolated to tempDir, so repo operations don't touch the user's helm config.
func isolatedHelmEnv(tempDir string) []string {
	return append(os.Environ(),
		"HELM_CACHE_HOME="+filepath.Join(tempDir, "cache"),
		"HELM_CONFIG_HOME="+filepath.Join(tempDir, "config"),
		"HELM_DATA_HOME="+filepath.Join(tempDir, "data"),
	)
}

// handleRemoteChart handles fetching a chart from a remote repository.
// Returns the chart reference, the isolated environment to use for helm
// commands (nil when the default environment suffices), and a temp directory
// to cleanup (empty when none was created).
func (r *HelmRenderer) handleRemoteChart(ctx context.Context, source *cluster.ApplicationSource) (string, []string, string, error) {
	repoURL := source.RepoURL

	if strings.HasPrefix(repoURL, "oci://") {
		// OCI registry - helm can pull directly; the chart version is
		// passed separately via --version
		return repoURL + "/" + source.Chart, nil, "", nil
	}

	// HTTP/HTTPS repo - need to add repo first
	// Create a temp directory for repo operations
	tempDir, err := os.MkdirTemp("", "argocdf-helm-")
	if err != nil {
		return "", nil, "", fmt.Errorf("failed to create temp dir: %w", err)
	}
	env := isolatedHelmEnv(tempDir)

	// Generate a unique repo name
	repoName := "argocdf-temp-" + source.Chart

	// Add the repo with context
	addArgs := []string{"repo", "add", repoName, repoURL, "--force-update"}
	addCmd := exec.CommandContext(ctx, "helm", addArgs...)
	addCmd.Env = env
	if output, err := addCmd.CombinedOutput(); err != nil {
		_ = SafeRemoveAll(tempDir)
		if ctx.Err() != nil {
			return "", nil, "", ctx.Err()
		}
		return "", nil, "", fmt.Errorf("failed to add helm repo: %v\noutput: %s", err, output)
	}

	// Update the repo with context
	updateArgs := []string{"repo", "update", repoName}
	updateCmd := exec.CommandContext(ctx, "helm", updateArgs...)
	updateCmd.Env = env
	if output, err := updateCmd.CombinedOutput(); err != nil {
		_ = SafeRemoveAll(tempDir)
		if ctx.Err() != nil {
			return "", nil, "", ctx.Err()
		}
		return "", nil, "", fmt.Errorf("failed to update helm repo: %v\noutput: %s", err, output)
	}

	chartRef := repoName + "/" + source.Chart
	return chartRef, env, tempDir, nil
}

// addHelmOptions adds Helm-specific options to the command arguments.
// repoPath is the repository root, chartDir is the chart directory (for relative path resolution).
// Returns the updated args, a list of temp files to cleanup, and any error.
func (r *HelmRenderer) addHelmOptions(args []string, helm *cluster.ApplicationSourceHelm, repoPath, chartDir string) ([]string, []string, error) {
	var tempFiles []string

	// Add value files
	for _, valueFile := range helm.ValueFiles {
		// Handle $ref references in value files
		resolvedPath, err := r.resolveValueFilePath(valueFile, repoPath, chartDir)
		if err != nil {
			return nil, tempFiles, fmt.Errorf("failed to resolve value file %q: %w", valueFile, err)
		}
		args = append(args, "--values", resolvedPath)
	}

	// Add inline values (string)
	if helm.Values != "" {
		// Write inline values to a temp file
		tmpFile, err := os.CreateTemp("", "values-*.yaml")
		if err != nil {
			return nil, tempFiles, fmt.Errorf("failed to create temp file for inline values: %w", err)
		}
		if _, err := tmpFile.WriteString(helm.Values); err != nil {
			_ = tmpFile.Close()
			_ = os.Remove(tmpFile.Name())
			return nil, tempFiles, fmt.Errorf("failed to write inline values: %w", err)
		}
		_ = tmpFile.Close()
		tempFiles = append(tempFiles, tmpFile.Name())
		args = append(args, "--values", tmpFile.Name())
	}

	// Add inline values object (structured)
	// ValuesObject is a runtime.RawExtension containing JSON bytes
	if helm.ValuesObject != nil && len(helm.ValuesObject.Raw) > 0 {
		// Convert JSON to YAML and write to a temp file
		valuesYAML, err := yaml.JSONToYAML(helm.ValuesObject.Raw)
		if err != nil {
			return nil, tempFiles, fmt.Errorf("failed to convert values object to YAML: %w", err)
		}
		tmpFile, err := os.CreateTemp("", "values-object-*.yaml")
		if err != nil {
			return nil, tempFiles, fmt.Errorf("failed to create temp file for values object: %w", err)
		}
		if _, err := tmpFile.Write(valuesYAML); err != nil {
			_ = tmpFile.Close()
			_ = os.Remove(tmpFile.Name())
			return nil, tempFiles, fmt.Errorf("failed to write values object: %w", err)
		}
		_ = tmpFile.Close()
		tempFiles = append(tempFiles, tmpFile.Name())
		args = append(args, "--values", tmpFile.Name())
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
		resolvedPath, err := r.resolveValueFilePath(fileParam.Path, repoPath, chartDir)
		if err != nil {
			return nil, tempFiles, fmt.Errorf("failed to resolve file parameter %q: %w", fileParam.Path, err)
		}
		args = append(args, "--set-file", fmt.Sprintf("%s=%s", fileParam.Name, resolvedPath))
	}

	// Note: helm.Version is the Helm binary version to use for templating
	// (e.g. "3"), not a chart version, so it is intentionally not passed
	// as --version. The tool always uses the helm binary on PATH.

	return args, tempFiles, nil
}

// resolveValueFilePath resolves a value file path, handling $ref references.
// repoPath is the repository root, chartDir is the chart directory.
// Relative paths are resolved relative to chartDir (matching ArgoCD behavior).
// Returns an error if the resolved path escapes the allowed directory boundaries.
func (r *HelmRenderer) resolveValueFilePath(path, repoPath, chartDir string) (string, error) {
	// Check if path uses a $ref reference
	if strings.HasPrefix(path, "$") {
		// Format: $refname/path/to/file.yaml
		parts := strings.SplitN(path, "/", 2)
		if len(parts) == 2 {
			refName := strings.TrimPrefix(parts[0], "$")
			if refPath, ok := r.opts.RefSources[refName]; ok {
				resolved := filepath.Join(refPath, parts[1])
				// Validate that the resolved path stays within the ref source directory
				if err := ValidatePathContainment(refPath, resolved); err != nil {
					return "", fmt.Errorf("invalid ref path %q: %w", path, err)
				}
				return resolved, nil
			}
		}
		// If ref not found, return path as-is (let helm handle the error)
		return path, nil
	}

	// Regular path - make absolute if needed
	// Relative paths are resolved relative to the chart directory
	var resolved string
	if !filepath.IsAbs(path) {
		resolved = filepath.Join(chartDir, path)
	} else {
		resolved = path
	}

	// Validate that the resolved path stays within the repository
	if err := ValidatePathContainment(repoPath, resolved); err != nil {
		return "", fmt.Errorf("invalid value file path %q: %w", path, err)
	}

	return resolved, nil
}

// CanRender checks if the helm binary is available.
func (r *HelmRenderer) CanRender() error {
	cmd := exec.Command("helm", "version", "--short")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("helm binary not found or not working: %w", err)
	}
	return nil
}

// SanitizeKubeVersion extracts a bare major.minor.patch version from a raw
// Kubernetes server version. It strips a leading "v" and any vendor suffix
// introduced by "-" (prerelease, e.g. "-gke.1091002", "-eks-abc123") or "+"
// (build metadata). Versions without a patch component (e.g. "1.29") are
// returned unchanged after trimming. If the input has no usable numeric
// prefix, the trimmed input is returned as-is so helm can surface the error.
func SanitizeKubeVersion(version string) string {
	v := strings.TrimSpace(version)
	v = strings.TrimPrefix(v, "v")

	// Strip anything from the first "-" (prerelease) or "+" (build metadata).
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}

	return v
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
// It runs `helm dependency build` if Chart.yaml exists with a dependencies section.
// Helm is smart enough to skip already-fetched dependencies.
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
	// Note: We always run helm dependency build when dependencies are defined
	// because just checking if charts/ has *any* content is not sufficient -
	// some dependencies might be missing while others are present.
	// Helm is smart enough to skip already-fetched dependencies.

	// Run helm dependency build with context
	args := []string{"dependency", "build", chartPath}
	if r.opts.HelmSkipRefresh {
		args = append(args, "--skip-refresh")
	}
	cmd := exec.CommandContext(ctx, "helm", args...)
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
