// Package render provides Kustomize rendering.
package render

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/rgeraskin/argocdf/internal/cluster"
	"github.com/rgeraskin/argocdf/internal/types"
)

// KustomizationNames contains the known kustomization file names.
var KustomizationNames = []string{"kustomization.yaml", "kustomization.yml", "Kustomization"}

// KustomizeRenderer renders Kustomize directories using the kustomize binary.
type KustomizeRenderer struct {
	opts RenderOptions
}

// NewKustomizeRenderer creates a new KustomizeRenderer.
func NewKustomizeRenderer(opts RenderOptions) *KustomizeRenderer {
	return &KustomizeRenderer{opts: opts}
}

// SourceType returns the source type for Kustomize.
func (r *KustomizeRenderer) SourceType() types.SourceType {
	return types.SourceTypeKustomize
}

// Render renders a Kustomize directory.
// Following ArgoCD's approach, this uses `kustomize edit` commands to apply
// Application-level overrides before running `kustomize build`.
// The context can be used to cancel long-running kustomize operations.
// When Kustomize edits are needed, the directory is copied to a temp location
// to prevent race conditions when multiple renders run concurrently.
func (r *KustomizeRenderer) Render(ctx context.Context, app *cluster.Application, source *cluster.ApplicationSource, repoPath string) ([]byte, error) {
	// Check context before starting
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	kustomizePath := filepath.Join(repoPath, source.Path)

	// Verify kustomization.yaml exists
	if !r.hasKustomization(kustomizePath) {
		// Fall back to plain YAML files
		return r.renderPlainYAML(kustomizePath)
	}

	// If we need to apply kustomize edits, copy to a temp directory first
	// This prevents race conditions when multiple goroutines render the same directory
	workDir := kustomizePath
	if r.needsKustomizeEdits(source.Kustomize) {
		tempDir, err := r.copyToTemp(kustomizePath)
		if err != nil {
			return nil, fmt.Errorf("failed to copy kustomize directory: %w", err)
		}
		defer os.RemoveAll(tempDir)
		workDir = tempDir

		// Apply kustomize-specific options using edit commands
		if err := r.applyKustomizeEdits(ctx, workDir, source.Kustomize); err != nil {
			return nil, fmt.Errorf("failed to apply kustomize edits: %w", err)
		}
	}

	// Run kustomize build with context
	cmd := exec.CommandContext(ctx, "kustomize", r.buildKustomizeArgs(workDir)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("kustomize build failed: %v\nstderr: %s", err, stderr.String())
	}

	return stdout.Bytes(), nil
}

// applyKustomizeEdits applies Application-level kustomize options using `kustomize edit` commands.
// This modifies the kustomization.yaml in place (changes are uncommitted and will be
// discarded on the next git operation).
func (r *KustomizeRenderer) applyKustomizeEdits(ctx context.Context, kustomizePath string, opts *cluster.ApplicationSourceKustomize) error {
	// NamePrefix
	if opts.NamePrefix != "" {
		if err := r.runKustomizeEdit(ctx, kustomizePath, "set", "nameprefix", "--", opts.NamePrefix); err != nil {
			return fmt.Errorf("failed to set nameprefix: %w", err)
		}
	}

	// NameSuffix
	if opts.NameSuffix != "" {
		if err := r.runKustomizeEdit(ctx, kustomizePath, "set", "namesuffix", "--", opts.NameSuffix); err != nil {
			return fmt.Errorf("failed to set namesuffix: %w", err)
		}
	}

	// Images
	if len(opts.Images) > 0 {
		args := []string{"set", "image"}
		for _, img := range opts.Images {
			args = append(args, string(img))
		}
		if err := r.runKustomizeEdit(ctx, kustomizePath, args...); err != nil {
			return fmt.Errorf("failed to set images: %w", err)
		}
	}

	// Replicas
	if len(opts.Replicas) > 0 {
		args := []string{"set", "replicas"}
		for _, replica := range opts.Replicas {
			count, err := replica.GetIntCount()
			if err != nil {
				return fmt.Errorf("failed to get replica count for %s: %w", replica.Name, err)
			}
			args = append(args, fmt.Sprintf("%s=%d", replica.Name, count))
		}
		if err := r.runKustomizeEdit(ctx, kustomizePath, args...); err != nil {
			return fmt.Errorf("failed to set replicas: %w", err)
		}
	}

	// CommonLabels
	if len(opts.CommonLabels) > 0 {
		args := []string{"add", "label"}
		if opts.ForceCommonLabels {
			args = append(args, "--force")
		}
		if opts.LabelWithoutSelector {
			args = append(args, "--without-selector")
		}
		args = append(args, mapToEditAddArgs(opts.CommonLabels)...)
		if err := r.runKustomizeEdit(ctx, kustomizePath, args...); err != nil {
			return fmt.Errorf("failed to add labels: %w", err)
		}
	}

	// CommonAnnotations
	if len(opts.CommonAnnotations) > 0 {
		args := []string{"add", "annotation"}
		if opts.ForceCommonAnnotations {
			args = append(args, "--force")
		}
		args = append(args, mapToEditAddArgs(opts.CommonAnnotations)...)
		if err := r.runKustomizeEdit(ctx, kustomizePath, args...); err != nil {
			return fmt.Errorf("failed to add annotations: %w", err)
		}
	}

	// Namespace
	if opts.Namespace != "" {
		if err := r.runKustomizeEdit(ctx, kustomizePath, "set", "namespace", "--", opts.Namespace); err != nil {
			return fmt.Errorf("failed to set namespace: %w", err)
		}
	}

	// Components
	if len(opts.Components) > 0 {
		args := []string{"add", "component"}
		args = append(args, opts.Components...)
		if err := r.runKustomizeEdit(ctx, kustomizePath, args...); err != nil {
			return fmt.Errorf("failed to add components: %w", err)
		}
	}

	// Patches - requires direct kustomization.yaml modification (no kustomize edit command)
	if len(opts.Patches) > 0 {
		if err := r.applyPatches(kustomizePath, opts.Patches); err != nil {
			return fmt.Errorf("failed to apply patches: %w", err)
		}
	}

	return nil
}

// buildKustomizeArgs constructs the arguments for kustomize build.
func (r *KustomizeRenderer) buildKustomizeArgs(path string) []string {
	args := []string{"build", path}

	if r.opts.KustomizeEnableHelm {
		args = append(args, "--enable-helm")
	}
	if r.opts.KustomizeLoadRestrictor != "" {
		args = append(args, "--load-restrictor", r.opts.KustomizeLoadRestrictor)
	}
	if r.opts.KustomizeBuildOptions != "" {
		args = append(args, strings.Fields(r.opts.KustomizeBuildOptions)...)
	}

	return args
}

// runKustomizeEdit runs a `kustomize edit` command in the given directory.
func (r *KustomizeRenderer) runKustomizeEdit(ctx context.Context, dir string, args ...string) error {
	cmdArgs := append([]string{"edit"}, args...)
	cmd := exec.CommandContext(ctx, "kustomize", cmdArgs...)
	cmd.Dir = dir

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("kustomize edit %v failed: %v\nstderr: %s", args, err, stderr.String())
	}
	return nil
}

// mapToEditAddArgs converts a map to kustomize edit add arguments.
// Format: key:value for each entry.
func mapToEditAddArgs(m map[string]string) []string {
	// Sort keys for deterministic ordering
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	args := make([]string, 0, len(m))
	for _, k := range keys {
		args = append(args, fmt.Sprintf("%s:%s", k, m[k]))
	}
	return args
}

// applyPatches adds patches to the kustomization.yaml file.
// This is done by reading, modifying, and writing the file directly
// since there's no `kustomize edit` command for patches.
func (r *KustomizeRenderer) applyPatches(kustomizePath string, patches []cluster.KustomizePatch) error {
	kustFile := r.findKustomizationFile(kustomizePath)
	if kustFile == "" {
		return fmt.Errorf("kustomization file not found")
	}

	kustomizationFilePath := filepath.Join(kustomizePath, kustFile)

	// Read existing kustomization
	content, err := os.ReadFile(kustomizationFilePath)
	if err != nil {
		return fmt.Errorf("failed to read kustomization file: %w", err)
	}

	var kustomization map[string]interface{}
	if err := yaml.Unmarshal(content, &kustomization); err != nil {
		return fmt.Errorf("failed to parse kustomization file: %w", err)
	}

	// Convert patches to interface slice for YAML
	patchesInterface := make([]interface{}, len(patches))
	for i, p := range patches {
		patchesInterface[i] = p
	}

	// Append to existing patches or create new
	if existingPatches, ok := kustomization["patches"]; ok {
		if patchesList, ok := existingPatches.([]interface{}); ok {
			kustomization["patches"] = append(patchesList, patchesInterface...)
		} else {
			kustomization["patches"] = patchesInterface
		}
	} else {
		kustomization["patches"] = patchesInterface
	}

	// Write back
	updatedContent, err := yaml.Marshal(kustomization)
	if err != nil {
		return fmt.Errorf("failed to marshal kustomization: %w", err)
	}

	info, err := os.Stat(kustomizationFilePath)
	if err != nil {
		return fmt.Errorf("failed to stat kustomization file: %w", err)
	}

	if err := os.WriteFile(kustomizationFilePath, updatedContent, info.Mode()); err != nil {
		return fmt.Errorf("failed to write kustomization file: %w", err)
	}

	return nil
}

// findKustomizationFile returns the name of the kustomization file in the directory.
func (r *KustomizeRenderer) findKustomizationFile(dir string) string {
	for _, name := range KustomizationNames {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return name
		}
	}
	return ""
}

// hasKustomization checks if a kustomization.yaml exists in the directory.
func (r *KustomizeRenderer) hasKustomization(path string) bool {
	return r.findKustomizationFile(path) != ""
}

// renderPlainYAML renders plain YAML files when no kustomization exists.
func (r *KustomizeRenderer) renderPlainYAML(dirPath string) ([]byte, error) {
	var result bytes.Buffer

	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		if !isYAMLFile(name) {
			continue
		}

		content, err := os.ReadFile(filepath.Join(dirPath, name))
		if err != nil {
			return nil, fmt.Errorf("failed to read file %s: %w", name, err)
		}

		if result.Len() > 0 {
			result.WriteString("---\n")
		}
		result.Write(content)
		result.WriteString("\n")
	}

	return result.Bytes(), nil
}

// isYAMLFile checks if a filename has a YAML extension.
func isYAMLFile(name string) bool {
	ext := filepath.Ext(name)
	return ext == ".yaml" || ext == ".yml"
}

// CanRender checks if the kustomize binary is available.
func (r *KustomizeRenderer) CanRender() error {
	cmd := exec.Command("kustomize", "version")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kustomize binary not found or not working: %w", err)
	}
	return nil
}

// IsKustomizeDirectory checks if a directory contains a kustomization file.
func IsKustomizeDirectory(path string) bool {
	r := &KustomizeRenderer{}
	return r.hasKustomization(path)
}

// needsKustomizeEdits returns true if the Kustomize options require modifying kustomization.yaml.
func (r *KustomizeRenderer) needsKustomizeEdits(opts *cluster.ApplicationSourceKustomize) bool {
	if opts == nil {
		return false
	}
	return opts.NamePrefix != "" ||
		opts.NameSuffix != "" ||
		len(opts.Images) > 0 ||
		len(opts.Replicas) > 0 ||
		len(opts.CommonLabels) > 0 ||
		len(opts.CommonAnnotations) > 0 ||
		opts.Namespace != "" ||
		len(opts.Components) > 0 ||
		len(opts.Patches) > 0
}

// copyToTemp copies the kustomize directory to a temp location.
// This is used to prevent race conditions when modifying kustomization.yaml.
func (r *KustomizeRenderer) copyToTemp(srcDir string) (string, error) {
	tempDir, err := os.MkdirTemp("", "argocdf-kustomize-")
	if err != nil {
		return "", fmt.Errorf("failed to create temp directory: %w", err)
	}

	if err := copyDir(srcDir, tempDir); err != nil {
		os.RemoveAll(tempDir)
		return "", err
	}

	return tempDir, nil
}

// copyDir recursively copies a directory tree.
func copyDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("failed to read directory %s: %w", src, err)
	}

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		if entry.IsDir() {
			// Create the directory
			info, err := entry.Info()
			if err != nil {
				return fmt.Errorf("failed to get info for %s: %w", srcPath, err)
			}
			if err := os.MkdirAll(dstPath, info.Mode()); err != nil {
				return fmt.Errorf("failed to create directory %s: %w", dstPath, err)
			}
			// Recursively copy contents
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			// Copy the file
			if err := copyFile(srcPath, dstPath); err != nil {
				return err
			}
		}
	}

	return nil
}

// copyFile copies a single file.
func copyFile(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open source file %s: %w", src, err)
	}
	defer srcFile.Close()

	srcInfo, err := srcFile.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat source file %s: %w", src, err)
	}

	dstFile, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, srcInfo.Mode())
	if err != nil {
		return fmt.Errorf("failed to create destination file %s: %w", dst, err)
	}
	defer dstFile.Close()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return fmt.Errorf("failed to copy file content: %w", err)
	}

	return nil
}
