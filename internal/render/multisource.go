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
	"github.com/rgeraskin/argocdf/internal/types"
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

// isPureRef reports whether a source is used ONLY as a ref and produces no
// manifests. ArgoCD's IsRef() is simply Ref != "", but a source may legitimately
// both render manifests (via Path/Chart) AND be referenced by other sources. Only
// a source with a Ref and neither Path nor Chart should be skipped from rendering.
func isPureRef(source cluster.ApplicationSource) bool {
	return source.Ref != "" && source.Path == "" && source.Chart == ""
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

		// Skip pure ref sources - they don't produce manifests. Sources that are
		// both a ref and a renderable source (Path/Chart set) are still rendered.
		if isPureRef(source) {
			continue
		}

		// Select the renderer the same way ArgoCD's repo-server does for every
		// source (single- or multi-source alike): explicit tool config wins,
		// otherwise the source path is inspected (Chart.yaml -> Helm). That
		// logic lives in GetRenderer; its decision is mapped onto the
		// per-request renderers carrying this app's RefSources.
		var renderer Renderer = kustomizeRenderer
		if r.factory.GetRenderer(&sources[i], r.repoPath).SourceType() == types.SourceTypeHelm {
			renderer = helmRenderer
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

	// Local repo URL of the repository being diffed (may be empty if auto-detect
	// failed). Both renderers share the same opts, so read from either.
	localRepoURL := r.factory.helmRenderer.opts.RepoURL

	for _, source := range sources {
		// Register ANY source that carries a Ref, including sources that also
		// render manifests (Path/Chart set) and are referenced by others.
		if source.Ref == "" {
			continue
		}

		// If the ref source points at the local repo being diffed, use the
		// local branch checkout (r.repoPath already points at the correct
		// branch at render time) instead of cloning. This ensures edits to
		// $values files in a PR actually produce a diff. Fall back to cloning
		// when the local repo URL is unknown.
		if localRepoURL != "" && git.NormalizeRepoURL(source.RepoURL) == git.NormalizeRepoURL(localRepoURL) {
			refPath := r.repoPath
			if source.Path != "" {
				refPath = filepath.Join(r.repoPath, source.Path)
				// Validate that the path stays within the local checkout
				if err := ValidatePathContainment(r.repoPath, refPath); err != nil {
					cleanup()
					return nil, nil, fmt.Errorf("invalid ref source path for %s: %w", source.Ref, err)
				}
			}
			refSources[source.Ref] = refPath
			continue
		}

		// External repo: create a temp directory and clone it.
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
