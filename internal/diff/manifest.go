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

// ParseResult contains parsed manifests and any parse issues encountered.
type ParseResult struct {
	Manifests   []Manifest
	ParseErrors []string
	// ParseWarnings contains non-fatal issues (e.g., duplicate YAML map keys
	// resolved with last-wins semantics). The document is still kept.
	ParseWarnings []string
}

// ParseManifests parses a multi-document YAML into Manifests.
// Invalid YAML documents are skipped (not treated as errors) because:
// - Empty documents and bare separators (---) are common in rendered output
// - Helm/Kustomize may produce documents with only comments
// - Continuing with valid documents provides a better user experience
// Only documents that can be parsed as valid Kubernetes objects (with apiVersion,
// kind, and metadata.name) are included in the result.
//
// Each document is first decoded into a yaml.Node (which does NOT error on
// duplicate map keys), then any duplicate keys are resolved with last-wins
// semantics (matching kubectl/ArgoCD apply behavior) and recorded in
// ParseWarnings. Genuine YAML syntax errors are collected in ParseErrors and
// the offending document is skipped.
func (p *ManifestParser) ParseManifests(content string) ParseResult {
	decoder := yaml.NewDecoder(strings.NewReader(content))
	var result ParseResult

	for {
		var node yaml.Node
		err := decoder.Decode(&node)
		if err == io.EOF {
			break
		}
		if err != nil {
			// Genuine YAML syntax error (malformed YAML). Decoding into a
			// yaml.Node does not fail on duplicate keys, so anything that
			// errors here is a real structural problem. yaml.v3's decoder
			// cannot advance past a structural syntax error (it would return
			// the same error indefinitely), so we record it and stop rather
			// than spin forever. Documents parsed before the error are kept.
			errMsg := strings.ReplaceAll(fmt.Sprintf("%v", err), "\n", " ")
			result.ParseErrors = append(result.ParseErrors, errMsg)
			log.Errorf("Skipping invalid YAML document: %s", errMsg)
			break
		}

		// Resolve duplicate map keys (last wins), collecting the duplicated
		// leaf key names so we can attach kind/name context after decode.
		var dupKeys []string
		dedupNode(&node, &dupKeys)

		// Decode the (deduplicated) node into a map.
		var rawObj map[string]interface{}
		if err := node.Decode(&rawObj); err != nil {
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

		// Record duplicate-key warnings now that kind/name are known.
		for _, k := range dupKeys {
			w := fmt.Sprintf("resource %s/%s: duplicate key %q (using last value)", manifest.Kind, manifest.Name, k)
			result.ParseWarnings = append(result.ParseWarnings, w)
			log.Warnf("Duplicate YAML key resolved with last-wins: %s", w)
		}

		result.Manifests = append(result.Manifests, manifest)
	}

	return result
}

// dedupNode walks a yaml.Node tree and, for every mapping node, removes
// duplicate keys keeping the LAST occurrence (matching YAML/kubectl last-wins
// semantics). The leaf name of each duplicated key is appended to dups.
func dedupNode(node *yaml.Node, dups *[]string) {
	if node == nil {
		return
	}
	switch node.Kind {
	case yaml.DocumentNode:
		for _, c := range node.Content {
			dedupNode(c, dups)
		}
	case yaml.MappingNode:
		seen := make(map[string]int) // key value -> index of the key node in newContent
		newContent := make([]*yaml.Node, 0, len(node.Content))
		for i := 0; i+1 < len(node.Content); i += 2 {
			keyNode := node.Content[i]
			valNode := node.Content[i+1]
			if idx, ok := seen[keyNode.Value]; ok {
				// Duplicate: last wins. Replace the previously kept key/value
				// pair in place so the later value takes precedence.
				*dups = append(*dups, keyNode.Value)
				newContent[idx] = keyNode
				newContent[idx+1] = valNode
			} else {
				seen[keyNode.Value] = len(newContent)
				newContent = append(newContent, keyNode, valNode)
			}
			// Recurse into the value node to dedup nested mappings.
			dedupNode(valNode, dups)
		}
		node.Content = newContent
	case yaml.SequenceNode:
		for _, c := range node.Content {
			dedupNode(c, dups)
		}
	}
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

	// ParseErrors contains fatal YAML parse errors from both old and new
	// content (e.g., malformed YAML). The offending document is dropped.
	ParseErrors []string

	// ParseWarnings contains non-fatal issues from both old and new content
	// (e.g., duplicate map keys resolved with last-wins semantics, or multiple
	// rendered documents sharing the same manifest identity). The affected
	// documents are still kept and diffed.
	ParseWarnings []string
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

// Side labels used to attribute parse errors/warnings to the render they came
// from. "base" is the old (merge-base) side, "target" the PR side — matching
// the "base → target" report header. A message appearing only under [target]
// was introduced by the change under review; the same message on both sides
// pre-exists on the base branch. Exported so other packages (e.g. lint)
// attribute their warnings with the same convention.
const (
	SideBase   = "base"
	SideTarget = "target"
)

// LabelSide prefixes each message with its originating side, e.g. "[target] ...".
func LabelSide(side string, msgs []string) []string {
	if len(msgs) == 0 {
		return nil
	}
	labeled := make([]string, len(msgs))
	for i, m := range msgs {
		labeled[i] = "[" + side + "] " + m
	}
	return labeled
}

// DiffManifests compares two YAML manifest contents.
func (d *ManifestDiffer) DiffManifests(oldContent, newContent string) (*ManifestSetDiff, error) {
	oldResult := d.parser.ParseManifests(oldContent)
	newResult := d.parser.ParseManifests(newContent)

	result, err := d.DiffManifestSets(oldResult.Manifests, newResult.Manifests)
	if err != nil {
		return nil, err
	}

	// Collect parse errors from both old and new content, attributed to their side
	result.ParseErrors = append(result.ParseErrors, LabelSide(SideBase, oldResult.ParseErrors)...)
	result.ParseErrors = append(result.ParseErrors, LabelSide(SideTarget, newResult.ParseErrors)...)

	// Collect parse warnings from both old and new content. Duplicate-manifest
	// warnings are already populated (and side-labeled) by DiffManifestSets.
	result.ParseWarnings = append(result.ParseWarnings, LabelSide(SideBase, oldResult.ParseWarnings)...)
	result.ParseWarnings = append(result.ParseWarnings, LabelSide(SideTarget, newResult.ParseWarnings)...)

	return result, nil
}

// DiffManifestSets compares two slices of manifests.
func (d *ManifestDiffer) DiffManifestSets(oldManifests, newManifests []Manifest) (*ManifestSetDiff, error) {
	result := &ManifestSetDiff{}

	// Build maps for lookup. If a render emits multiple documents with the same
	// manifest identity (namespace/group/Kind/name), the map keeps only the last
	// one (matching ArgoCD's apply behavior) but we surface a warning so the
	// collision is visible rather than silently hidden.
	oldMap, oldDupWarnings := buildManifestMap(oldManifests)
	newMap, newDupWarnings := buildManifestMap(newManifests)
	result.ParseWarnings = append(result.ParseWarnings, LabelSide(SideBase, oldDupWarnings)...)
	result.ParseWarnings = append(result.ParseWarnings, LabelSide(SideTarget, newDupWarnings)...)

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

// buildManifestMap indexes manifests by their Key(). When multiple manifests
// share the same key, the last one wins (matching ArgoCD server-side apply,
// which only applies one) and a warning is produced per colliding key.
func buildManifestMap(manifests []Manifest) (map[string]Manifest, []string) {
	m := make(map[string]Manifest, len(manifests))
	counts := make(map[string]int, len(manifests))
	var order []string
	for _, man := range manifests {
		k := man.Key()
		if counts[k] == 0 {
			order = append(order, k)
		}
		counts[k]++
		m[k] = man
	}

	var warnings []string
	for _, k := range order {
		if counts[k] > 1 {
			warnings = append(warnings, fmt.Sprintf(
				"duplicate manifest %s: %d documents share this identity; ArgoCD will only apply one",
				k, counts[k]))
		}
	}
	return m, warnings
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
