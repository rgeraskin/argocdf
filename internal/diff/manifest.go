// Package diff provides manifest parsing and comparison.
package diff

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Manifest represents a parsed Kubernetes manifest.
type Manifest struct {
	// Object is the parsed content as a map
	Object map[string]interface{}

	// Raw is the original YAML content
	Raw string

	// Parsed fields for easy access
	APIVersion string
	Kind       string
	Name       string
	Namespace  string
}

// Key returns a unique identifier for this manifest (namespace/kind/name).
func (m *Manifest) Key() string {
	if m.Namespace != "" {
		return fmt.Sprintf("%s/%s/%s", m.Namespace, m.Kind, m.Name)
	}
	return fmt.Sprintf("%s/%s", m.Kind, m.Name)
}

// ManifestParser parses YAML manifests.
type ManifestParser struct{}

// NewManifestParser creates a new ManifestParser.
func NewManifestParser() *ManifestParser {
	return &ManifestParser{}
}

// ParseManifests parses a multi-document YAML into Manifests.
func (p *ManifestParser) ParseManifests(content string) ([]Manifest, error) {
	decoder := yaml.NewDecoder(strings.NewReader(content))
	var manifests []Manifest

	for {
		var rawObj map[string]interface{}
		err := decoder.Decode(&rawObj)
		if err == io.EOF {
			break
		}
		if err != nil {
			// Skip invalid YAML documents silently.
			// This commonly happens with empty documents or document separators (---).
			// Debug logging would require propagating a logger through the parser.
			continue
		}
		if rawObj == nil {
			// Skip empty/null documents (e.g., just "---" or "---\n---")
			continue
		}

		manifest := Manifest{
			Object: rawObj,
			Raw:    mustMarshalYAML(rawObj),
		}

		// Extract common fields
		manifest.APIVersion = getString(rawObj, "apiVersion")
		manifest.Kind = getString(rawObj, "kind")

		if metadata, ok := rawObj["metadata"].(map[string]interface{}); ok {
			manifest.Name = getString(metadata, "name")
			manifest.Namespace = getString(metadata, "namespace")
		}

		// Skip if not a valid Kubernetes object
		// Require apiVersion, kind, and name to be present
		if manifest.APIVersion == "" || manifest.Kind == "" || manifest.Name == "" {
			continue
		}

		manifests = append(manifests, manifest)
	}

	return manifests, nil
}

// getString safely extracts a string from a map.
func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// mustMarshalYAML marshals to YAML, returning empty string on error.
func mustMarshalYAML(obj interface{}) string {
	data, err := yaml.Marshal(obj)
	if err != nil {
		return ""
	}
	return string(data)
}

// ManifestDiff represents the diff of a single manifest.
type ManifestDiff struct {
	// Key is the manifest identifier (namespace/kind/name)
	Key string

	// Old is the manifest from the base branch (nil if added)
	Old *Manifest

	// New is the manifest from the target branch (nil if removed)
	New *Manifest

	// Diff contains the structured field-level changes
	Diff *DiffResult
}

// ManifestSetDiff contains the full diff between two sets of manifests.
type ManifestSetDiff struct {
	// Added contains manifests only in the new set
	Added []Manifest

	// Removed contains manifests only in the old set
	Removed []Manifest

	// Modified contains manifests that changed
	Modified []ManifestDiff

	// Unchanged count
	UnchangedCount int

	// HasChanges is true if there are any differences
	HasChanges bool
}

// ManifestDiffer compares two sets of manifests.
type ManifestDiffer struct {
	parser *ManifestParser
	differ *Differ
}

// NewManifestDiffer creates a new ManifestDiffer.
func NewManifestDiffer() *ManifestDiffer {
	return &ManifestDiffer{
		parser: NewManifestParser(),
		differ: NewDiffer(),
	}
}

// DiffManifests compares two YAML manifest contents.
func (d *ManifestDiffer) DiffManifests(oldContent, newContent string) (*ManifestSetDiff, error) {
	oldManifests, err := d.parser.ParseManifests(oldContent)
	if err != nil {
		return nil, fmt.Errorf("failed to parse old manifests: %w", err)
	}

	newManifests, err := d.parser.ParseManifests(newContent)
	if err != nil {
		return nil, fmt.Errorf("failed to parse new manifests: %w", err)
	}

	return d.DiffManifestSets(oldManifests, newManifests)
}

// DiffManifestSets compares two slices of manifests.
func (d *ManifestDiffer) DiffManifestSets(oldManifests, newManifests []Manifest) (*ManifestSetDiff, error) {
	result := &ManifestSetDiff{}

	// Build maps for lookup
	oldMap := make(map[string]Manifest)
	for _, m := range oldManifests {
		oldMap[m.Key()] = m
	}

	newMap := make(map[string]Manifest)
	for _, m := range newManifests {
		newMap[m.Key()] = m
	}

	// Find added and modified
	for key, newM := range newMap {
		if oldM, exists := oldMap[key]; exists {
			// Compare using structured differ
			diffResult := d.differ.DiffObjects(oldM.Object, newM.Object)

			if diffResult.Modified {
				result.Modified = append(result.Modified, ManifestDiff{
					Key:  key,
					Old:  &oldM,
					New:  &newM,
					Diff: diffResult,
				})
				result.HasChanges = true
			} else {
				result.UnchangedCount++
			}
		} else {
			// Added
			result.Added = append(result.Added, newM)
			result.HasChanges = true
		}
	}

	// Find removed
	for key, oldM := range oldMap {
		if _, exists := newMap[key]; !exists {
			result.Removed = append(result.Removed, oldM)
			result.HasChanges = true
		}
	}

	// Sort for consistent output
	sort.Slice(result.Added, func(i, j int) bool {
		return result.Added[i].Key() < result.Added[j].Key()
	})
	sort.Slice(result.Removed, func(i, j int) bool {
		return result.Removed[i].Key() < result.Removed[j].Key()
	})
	sort.Slice(result.Modified, func(i, j int) bool {
		return result.Modified[i].Key < result.Modified[j].Key
	})

	return result, nil
}

// ExtractApplications extracts ArgoCD Application manifests from parsed manifests.
func ExtractApplications(manifests []Manifest) []Manifest {
	var apps []Manifest
	for _, m := range manifests {
		if m.Kind == "Application" && strings.Contains(m.APIVersion, "argoproj.io") {
			apps = append(apps, m)
		}
	}
	return apps
}
