// Package render provides multi-source application rendering.
package render

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
func (r *MultiSourceRenderer) RenderMultiSource(app *cluster.Application) ([]byte, error) {
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

	// Update factory options with ref sources
	r.factory.helmRenderer.opts.RefSources = refSources
	r.factory.kustomizeRenderer.opts.RefSources = refSources

	// Second pass: render non-ref sources
	var allManifests bytes.Buffer
	for i, source := range sources {
		// Skip ref sources - they don't produce manifests
		if source.IsRef() {
			continue
		}

		renderer := r.factory.GetRenderer(&sources[i], r.repoPath)
		manifests, err := renderer.Render(app, &sources[i], r.repoPath)
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
			os.RemoveAll(dir)
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
		}

		refSources[source.Ref] = refPath
	}

	return refSources, cleanup, nil
}

// MergeManifests merges multiple manifest outputs, deduplicating by resource key.
// For duplicates, later sources win.
func MergeManifests(manifests ...[]byte) ([]byte, []string) {
	var result bytes.Buffer
	seen := make(map[string]bool)
	var warnings []string

	for _, m := range manifests {
		// Split by YAML document separator
		docs := splitYAMLDocuments(m)

		for _, doc := range docs {
			key := extractResourceKey(doc)
			if key == "" {
				// Can't extract key, just include it
				if result.Len() > 0 {
					result.WriteString("---\n")
				}
				result.Write(doc)
				continue
			}

			if seen[key] {
				warnings = append(warnings, fmt.Sprintf("duplicate resource: %s (using later definition)", key))
			}
			seen[key] = true

			if result.Len() > 0 {
				result.WriteString("---\n")
			}
			result.Write(doc)
		}
	}

	return result.Bytes(), warnings
}

// splitYAMLDocuments splits a multi-document YAML into individual documents.
func splitYAMLDocuments(data []byte) [][]byte {
	var docs [][]byte
	lines := strings.Split(string(data), "\n")
	var current bytes.Buffer

	for _, line := range lines {
		if line == "---" {
			if current.Len() > 0 {
				docs = append(docs, bytes.TrimSpace(current.Bytes()))
				current.Reset()
			}
			continue
		}
		current.WriteString(line)
		current.WriteString("\n")
	}

	if current.Len() > 0 {
		trimmed := bytes.TrimSpace(current.Bytes())
		if len(trimmed) > 0 {
			docs = append(docs, trimmed)
		}
	}

	return docs
}

// extractResourceKey extracts a unique key for a Kubernetes resource from YAML.
func extractResourceKey(data []byte) string {
	// Simple parsing - look for apiVersion, kind, metadata.name, metadata.namespace
	lines := strings.Split(string(data), "\n")

	var apiVersion, kind, name, namespace string

	for _, line := range lines {
		line = strings.TrimSpace(line)

		if val, found := strings.CutPrefix(line, "apiVersion:"); found {
			apiVersion = strings.TrimSpace(val)
		} else if val, found := strings.CutPrefix(line, "kind:"); found {
			kind = strings.TrimSpace(val)
		} else if val, found := strings.CutPrefix(line, "name:"); found && name == "" {
			// First "name:" should be in metadata
			name = strings.TrimSpace(val)
		} else if val, found := strings.CutPrefix(line, "namespace:"); found && namespace == "" {
			namespace = strings.TrimSpace(val)
		}
	}

	if apiVersion == "" || kind == "" || name == "" {
		return ""
	}

	if namespace != "" {
		return fmt.Sprintf("%s/%s/%s/%s", apiVersion, kind, namespace, name)
	}
	return fmt.Sprintf("%s/%s/%s", apiVersion, kind, name)
}
