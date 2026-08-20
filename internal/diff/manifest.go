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
	// ParseWarnings contains non-fatal issues, and everything else that rides
	// this channel to the report: two documents sharing one manifest identity
	// (DiffManifestSets) and every --lint finding (diff.LabelSide). ParseManifests
	// itself no longer produces any — see its comment.
	ParseWarnings []string
}

// ParseManifests parses a multi-document YAML stream into Manifests.
//
// The only production producer of that stream is argocdf's own render layer:
// ArgoCD's GenerateManifests returns one JSON document per resource, and
// render.manifestsToYAML converts each with yaml.JSONToYAML and joins them with
// "---". That single fact decides how tolerant this parser has to be, so each
// tolerance below says whether the producer can actually exercise it:
//
//   - a document that is not a Kubernetes object (no apiVersion, kind or
//     metadata.name) is SKIPPED. LIVE: gitops-engine's SplitYAML keeps any YAML
//     mapping a chart emits, so a stray "foo: bar" document arrives here as a
//     manifest.
//   - an empty or null document contributes nothing. LIVE ENOUGH to keep:
//     upstream's own UnmarshalToUnstructured guards for a literal "null"
//     manifest, which is where such a document would come from.
//   - a document whose root is not a mapping is skipped and recorded in
//     ParseErrors, and the stream CONTINUES. Unreachable from JSON objects, but
//     the decode error has to be handled either way, and skipping one document
//     beats truncating the stream.
//   - a structural YAML error is recorded in ParseErrors and STOPS the stream:
//     yaml.v3's decoder cannot advance past one, so continuing would spin
//     forever. Unreachable from machine-serialized input.
//
// What is deliberately NOT here: duplicate-map-key resolution. Until 0.5.0
// argocdf parsed raw `helm template` stdout, where a chart could emit duplicate
// keys, and this parser resolved them last-wins with a warning (ffe57f4, three
// weeks before the native pipeline was deleted). ArgoCD's engine hands over JSON
// marshalled from Go maps, so a duplicate key cannot survive to reach here — the
// machinery guarded an input the pipeline can no longer produce, while its tests
// implied coverage of a real scenario. A duplicate key now lands in ParseErrors
// like any other malformed document: reported, and never silently resolved.
//
// The two-step decode (yaml.Node, then Node.Decode into a map) is what keeps
// those last two cases distinguishable — a Node decode never fails on document
// SHAPE, so an error from it is a type problem the stream can survive, while an
// error from the stream decoder is structural and cannot be.
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
