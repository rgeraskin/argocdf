// Package render provides Kustomize rendering.
package render

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/rgeraskin/argocdf/internal/cluster"
	"github.com/rgeraskin/argocdf/internal/types"
)

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
func (r *KustomizeRenderer) Render(app *cluster.Application, source *cluster.ApplicationSource, repoPath string) ([]byte, error) {
	kustomizePath := filepath.Join(repoPath, source.Path)

	// Verify kustomization.yaml exists
	if !r.hasKustomization(kustomizePath) {
		// Fall back to plain YAML files
		return r.renderPlainYAML(kustomizePath)
	}

	args := []string{"build", kustomizePath}

	// Add kustomize-specific options if present
	if source.Kustomize != nil {
		args = r.addKustomizeOptions(args, source.Kustomize)
	}

	cmd := exec.Command("kustomize", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("kustomize build failed: %v\nstderr: %s", err, stderr.String())
	}

	return stdout.Bytes(), nil
}

// hasKustomization checks if a kustomization.yaml exists in the directory.
func (r *KustomizeRenderer) hasKustomization(path string) bool {
	kustomizationFiles := []string{
		"kustomization.yaml",
		"kustomization.yml",
		"Kustomization",
	}

	for _, f := range kustomizationFiles {
		if _, err := os.Stat(filepath.Join(path, f)); err == nil {
			return true
		}
	}
	return false
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

// addKustomizeOptions adds Kustomize-specific options to the build command.
func (r *KustomizeRenderer) addKustomizeOptions(args []string, kustomize *cluster.ApplicationSourceKustomize) []string {
	// Add name prefix
	if kustomize.NamePrefix != "" {
		args = append(args, "--name-prefix", kustomize.NamePrefix)
	}

	// Add name suffix
	if kustomize.NameSuffix != "" {
		args = append(args, "--name-suffix", kustomize.NameSuffix)
	}

	// Note: Images, labels, and annotations are typically handled in the kustomization.yaml
	// or through kustomize edit commands. The kustomize build command doesn't support
	// all these as flags, so we'd need to modify the kustomization.yaml file for those.

	return args
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
