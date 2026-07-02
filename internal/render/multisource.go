// Package render provides multi-source application rendering.
package render

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/rgeraskin/argocdf/internal/cluster"
	"github.com/rgeraskin/argocdf/internal/git"
)

// MultiSourceRenderer handles rendering of applications with multiple sources.
type MultiSourceRenderer struct {
	factory  *Factory
	repoPath string
}

// NewMultiSourceRenderer creates a new MultiSourceRenderer.
func NewMultiSourceRenderer(factory *Factory, repoPath string) *MultiSourceRenderer {
	return &MultiSourceRenderer{
		factory:  factory,
		repoPath: repoPath,
	}
}

// RenderMultiSource renders an application with multiple sources.
// The context can be used to cancel long-running render operations.
// This method is safe for concurrent use - it creates per-request renderers
// instead of mutating shared factory state.
func (r *MultiSourceRenderer) RenderMultiSource(ctx context.Context, app *cluster.Application) ([]byte, error) {
	sources := app.Spec.GetSources()
	if len(sources) == 0 {
		return nil, fmt.Errorf("no sources defined for application")
	}

	// First pass: identify and clone ref sources
	refSources, cleanup, err := r.prepareRefSources(sources)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare ref sources: %w", err)
	}
	defer cleanup()

	// Create per-request renderers with refSources configured
	// This avoids mutating shared factory state and prevents race conditions
	helmOpts := r.factory.helmRenderer.opts
	helmOpts.RefSources = refSources
	helmRenderer := NewHelmRenderer(helmOpts)

	kustomizeOpts := r.factory.kustomizeRenderer.opts
	kustomizeOpts.RefSources = refSources
	kustomizeRenderer := NewKustomizeRenderer(kustomizeOpts)

	// Second pass: render non-ref sources
	var allManifests bytes.Buffer
	for i, source := range sources {
		// Check context before each render
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		// Skip ref sources - they don't produce manifests
		if source.IsRef() {
			continue
		}

		// Select the appropriate renderer for this source
		var renderer Renderer
		if source.IsHelm() || source.Helm != nil {
			renderer = helmRenderer
		} else {
			renderer = kustomizeRenderer
		}

		manifests, err := renderer.Render(ctx, app, &sources[i], r.repoPath)
		if err != nil {
			return nil, fmt.Errorf("failed to render source %d: %w", i, err)
		}

		if allManifests.Len() > 0 && len(manifests) > 0 {
			allManifests.WriteString("---\n")
		}
		allManifests.Write(manifests)
	}

	return allManifests.Bytes(), nil
}

// prepareRefSources clones/checks out ref sources and returns a map of ref name to local path.
func (r *MultiSourceRenderer) prepareRefSources(sources []cluster.ApplicationSource) (map[string]string, func(), error) {
	refSources := make(map[string]string)
	tempDirs := make([]string, 0)

	cleanup := func() {
		for _, dir := range tempDirs {
			_ = SafeRemoveAll(dir)
		}
	}

	for _, source := range sources {
		if !source.IsRef() {
			continue
		}

		// Create a temp directory for this ref source
		tempDir, err := os.MkdirTemp("", "argocdf-ref-")
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("failed to create temp dir: %w", err)
		}
		tempDirs = append(tempDirs, tempDir)

		// Clone the repository using shared git.Clone
		if err := git.Clone(source.RepoURL, source.TargetRevision, tempDir); err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("failed to clone ref source %s: %w", source.Ref, err)
		}

		// If path is specified, adjust the ref path
		refPath := tempDir
		if source.Path != "" {
			refPath = filepath.Join(tempDir, source.Path)
			// Validate that the path stays within the cloned directory
			if err := ValidatePathContainment(tempDir, refPath); err != nil {
				cleanup()
				return nil, nil, fmt.Errorf("invalid ref source path for %s: %w", source.Ref, err)
			}
		}

		refSources[source.Ref] = refPath
	}

	return refSources, cleanup, nil
}
