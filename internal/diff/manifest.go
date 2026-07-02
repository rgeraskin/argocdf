// Package diff provides manifest parsing and comparison.
package diff

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/charmbracelet/log"
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

// Key returns a unique identifier for this manifest.
// The identifier includes the API group (but not the version) so that resources
// with the same kind and name in different API groups (e.g. two "Certificate"
// resources from different operators) do not collide. Version bumps within a
// single group (e.g. v1beta1 -> v1) still share a key and thus compare as
// modified rather than as add+remove.
// Format: [namespace/][group/]Kind/name — the namespace and group segments are
// omitted when empty (cluster-scoped resources and the core API group).
func (m *Manifest) Key() string {
	parts := make([]string, 0, 4)
	if m.Namespace != "" {
		parts = append(parts, m.Namespace)
	}
	if group := apiGroup(m.APIVersion); group != "" {
		parts = append(parts, group)
	}
	parts = append(parts, m.Kind, m.Name)
	return strings.Join(parts, "/")
}

// apiGroup returns the API group portion of an apiVersion string.
// For "group/version" (e.g. "cert-manager.io/v1") it returns the group; for a
// core-group apiVersion (e.g. "v1") it returns an empty string.
func apiGroup(apiVersion string) string {
	if i := strings.Index(apiVersion, "/"); i >= 0 {
		return apiVersion[:i]
	}
	return ""
}

// ManifestParser parses YAML manifests.
type ManifestParser struct{}

// NewManifestParser creates a new ManifestParser.
func NewManifestParser() *ManifestParser {
	return &ManifestParser{}
}

// ParseResult contains parsed manifests and any parse errors encountered.
type ParseResult struct {
	Manifests   []Manifest
	ParseErrors []string
}

// ParseManifests parses a multi-document YAML into Manifests.
// Invalid YAML documents are skipped (not treated as errors) because:
// - Empty documents and bare separators (---) are common in rendered output
// - Helm/Kustomize may produce documents with only comments
// - Continuing with valid documents provides a better user experience
// Only documents that can be parsed as valid Kubernetes objects (with apiVersion,
// kind, and metadata.name) are included in the result.
// Parse errors (e.g., duplicate YAML keys) are collected and returned in ParseResult.
func (p *ManifestParser) ParseManifests(content string) ParseResult {
	decoder := yaml.NewDecoder(strings.NewReader(content))
	var result ParseResult

	for {
		var rawObj map[string]interface{}
		err := decoder.Decode(&rawObj)
		if err == io.EOF {
			break
		}
		if err != nil {
			// Collect YAML parse errors (e.g., duplicate keys, malformed YAML)
			// These indicate issues in the source templates that should be fixed
			errMsg := strings.ReplaceAll(fmt.Sprintf("%v", err), "\n", " ")
			result.ParseErrors = append(result.ParseErrors, errMsg)
			log.Errorf("Skipping invalid YAML document: %s", errMsg)
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

		result.Manifests = append(result.Manifests, manifest)
	}

	return result
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

	// ParseErrors contains YAML parse errors from both old and new content
	// These indicate issues in the source templates (e.g., duplicate keys)
	ParseErrors []string
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
	oldResult := d.parser.ParseManifests(oldContent)
	newResult := d.parser.ParseManifests(newContent)

	result, err := d.DiffManifestSets(oldResult.Manifests, newResult.Manifests)
	if err != nil {
		return nil, err
	}

	// Collect parse errors from both old and new content
	result.ParseErrors = append(result.ParseErrors, oldResult.ParseErrors...)
	result.ParseErrors = append(result.ParseErrors, newResult.ParseErrors...)

	return result, nil
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
