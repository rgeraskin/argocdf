// Package render provides manifest rendering functionality.
package render

import (
	"context"
	"os"
	"path/filepath"

	"github.com/rgeraskin/argocdf/internal/cluster"
	"github.com/rgeraskin/argocdf/internal/types"
)

// Renderer defines the interface for rendering ArgoCD application manifests.
type Renderer interface {
	// Render renders the manifests for an application source.
	// The context can be used to cancel long-running render operations.
	Render(ctx context.Context, app *cluster.Application, source *cluster.ApplicationSource, repoPath string) ([]byte, error)

	// SourceType returns the type of source this renderer handles.
	SourceType() types.SourceType
}

// RenderOptions contains options for rendering.
type RenderOptions struct {
	// RepoPath is the path to the git repository
	RepoPath string

	// KubeVersion is the Kubernetes version to use for rendering
	KubeVersion string

	// Namespace is the target namespace for the rendered manifests
	Namespace string

	// RefSources maps ref names to cloned repository paths for multi-source apps
	RefSources map[string]string

	// Kustomize build options (defaults from CLI)
	KustomizeEnableHelm     bool
	KustomizeBuildOptions   string
	KustomizeLoadRestrictor string
}

// RenderResult contains the result of rendering an application.
type RenderResult struct {
	// Manifests is the raw YAML output
	Manifests []byte

	// SourceType indicates what type of source was rendered
	SourceType types.SourceType

	// Error holds any error that occurred
	Error error
}

// Factory creates the appropriate renderer for a source.
type Factory struct {
	helmRenderer      *HelmRenderer
	kustomizeRenderer *KustomizeRenderer
}

// NewFactory creates a new renderer factory.
func NewFactory(opts RenderOptions) *Factory {
	return &Factory{
		helmRenderer:      NewHelmRenderer(opts),
		kustomizeRenderer: NewKustomizeRenderer(opts),
	}
}

// GetRenderer returns the appropriate renderer for the given source.
// repoPath is used to detect Helm charts by checking for Chart.yaml in the source path.
func (f *Factory) GetRenderer(source *cluster.ApplicationSource, repoPath string) Renderer {
	// Check for Helm: either Chart field is set or Helm config is present
	if source.IsHelm() || source.Helm != nil {
		return f.helmRenderer
	}
	if source.Kustomize != nil {
		return f.kustomizeRenderer
	}
	// Check if the path contains a Chart.yaml (ArgoCD auto-detection)
	if source.Path != "" && repoPath != "" {
		chartPath := filepath.Join(repoPath, source.Path, "Chart.yaml")
		if _, err := os.Stat(chartPath); err == nil {
			return f.helmRenderer
		}
	}
	// Default to Kustomize for plain directories (ArgoCD behavior)
	return f.kustomizeRenderer
}

// RenderApplication renders all sources for an application and combines the output.
// The context can be used to cancel long-running render operations.
func (f *Factory) RenderApplication(ctx context.Context, app *cluster.Application, repoPath string) (*RenderResult, error) {
	sources := app.Spec.GetSources()
	if len(sources) == 0 {
		return &RenderResult{
			SourceType: types.SourceTypeUnknown,
		}, nil
	}

	// For single source apps, render directly
	if len(sources) == 1 && !sources[0].IsRef() {
		renderer := f.GetRenderer(&sources[0], repoPath)
		manifests, err := renderer.Render(ctx, app, &sources[0], repoPath)
		return &RenderResult{
			Manifests:  manifests,
			SourceType: renderer.SourceType(),
			Error:      err,
		}, err
	}

	// For multi-source apps, we need to handle ref sources
	msRenderer := NewMultiSourceRenderer(f, repoPath)
	manifests, err := msRenderer.RenderMultiSource(ctx, app)
	return &RenderResult{
		Manifests:  manifests,
		SourceType: types.SourceTypeHelm, // Multi-source typically uses Helm
		Error:      err,
	}, err
}
