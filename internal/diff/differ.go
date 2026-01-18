// Package diff provides structured manifest comparison.
package diff

import (
	"bytes"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// DiffResult contains the result of comparing two Kubernetes objects.
type DiffResult struct {
	// Modified indicates if there are differences
	Modified bool

	// Changes contains the list of field-level changes
	Changes []FieldChange
}

// FieldChange represents a change to a specific field.
type FieldChange struct {
	// Path is the field path (e.g., "spec.replicas")
	Path string

	// Type is the change type: added, removed, modified
	Type ChangeType

	// OldValue is the previous value (nil for added)
	OldValue interface{}

	// NewValue is the new value (nil for removed)
	NewValue interface{}
}

// ChangeType represents the type of change.
type ChangeType string

const (
	ChangeTypeAdded    ChangeType = "added"
	ChangeTypeRemoved  ChangeType = "removed"
	ChangeTypeModified ChangeType = "modified"
)

// Differ performs structured comparison of Kubernetes objects.
type Differ struct {
	// IgnoredFields are field paths to ignore during comparison
	IgnoredFields map[string]bool
}

// NewDiffer creates a new Differ with default ignored fields.
func NewDiffer() *Differ {
	return &Differ{
		IgnoredFields: map[string]bool{
			// ArgoCD/Kubernetes metadata fields that change frequently
			"metadata.resourceVersion":   true,
			"metadata.uid":               true,
			"metadata.generation":        true,
			"metadata.creationTimestamp": true,
			"metadata.managedFields":     true,
			"metadata.annotations.kubectl.kubernetes.io/last-applied-configuration": true,
			"status": true,
		},
	}
}

// DiffObjects compares two objects represented as maps.
func (d *Differ) DiffObjects(oldObj, newObj map[string]interface{}) *DiffResult {
	result := &DiffResult{
		Changes: make([]FieldChange, 0),
	}

	d.compareValues("", oldObj, newObj, result)
	result.Modified = len(result.Changes) > 0

	// Sort changes by path for consistent output
	sort.Slice(result.Changes, func(i, j int) bool {
		return result.Changes[i].Path < result.Changes[j].Path
	})

	return result
}

// compareValues recursively compares two values.
func (d *Differ) compareValues(path string, oldVal, newVal interface{}, result *DiffResult) {
	// Skip ignored fields
	if d.IgnoredFields[path] {
		return
	}

	// Handle nil cases
	if oldVal == nil && newVal == nil {
		return
	}
	if oldVal == nil {
		result.Changes = append(result.Changes, FieldChange{
			Path:     path,
			Type:     ChangeTypeAdded,
			NewValue: newVal,
		})
		return
	}
	if newVal == nil {
		result.Changes = append(result.Changes, FieldChange{
			Path:     path,
			Type:     ChangeTypeRemoved,
			OldValue: oldVal,
		})
		return
	}

	// Compare based on type
	oldType := reflect.TypeOf(oldVal)
	newType := reflect.TypeOf(newVal)

	if oldType != newType {
		result.Changes = append(result.Changes, FieldChange{
			Path:     path,
			Type:     ChangeTypeModified,
			OldValue: oldVal,
			NewValue: newVal,
		})
		return
	}

	switch old := oldVal.(type) {
	case map[string]interface{}:
		new := newVal.(map[string]interface{})
		d.compareMaps(path, old, new, result)

	case []interface{}:
		new := newVal.([]interface{})
		d.compareSlices(path, old, new, result)

	default:
		// Scalar comparison
		if !reflect.DeepEqual(oldVal, newVal) {
			result.Changes = append(result.Changes, FieldChange{
				Path:     path,
				Type:     ChangeTypeModified,
				OldValue: oldVal,
				NewValue: newVal,
			})
		}
	}
}

// compareMaps compares two maps.
func (d *Differ) compareMaps(path string, oldMap, newMap map[string]interface{}, result *DiffResult) {
	// Collect all keys
	allKeys := make(map[string]bool)
	for k := range oldMap {
		allKeys[k] = true
	}
	for k := range newMap {
		allKeys[k] = true
	}

	for key := range allKeys {
		fieldPath := key
		if path != "" {
			fieldPath = path + "." + key
		}

		oldVal, oldExists := oldMap[key]
		newVal, newExists := newMap[key]

		if !oldExists {
			d.compareValues(fieldPath, nil, newVal, result)
		} else if !newExists {
			d.compareValues(fieldPath, oldVal, nil, result)
		} else {
			d.compareValues(fieldPath, oldVal, newVal, result)
		}
	}
}

// compareSlices compares two slices.
func (d *Differ) compareSlices(path string, oldSlice, newSlice []interface{}, result *DiffResult) {
	// For simplicity, compare slices by index
	// A more sophisticated approach would match by name/key
	maxLen := len(oldSlice)
	if len(newSlice) > maxLen {
		maxLen = len(newSlice)
	}

	for i := 0; i < maxLen; i++ {
		indexPath := fmt.Sprintf("%s[%d]", path, i)

		var oldVal, newVal interface{}
		if i < len(oldSlice) {
			oldVal = oldSlice[i]
		}
		if i < len(newSlice) {
			newVal = newSlice[i]
		}

		d.compareValues(indexPath, oldVal, newVal, result)
	}
}

// FormatASCIIDiff formats the diff result as a human-readable string.
func FormatASCIIDiff(result *DiffResult) string {
	if !result.Modified {
		return ""
	}

	var buf bytes.Buffer
	for _, change := range result.Changes {
		switch change.Type {
		case ChangeTypeAdded:
			buf.WriteString(fmt.Sprintf("+ %s: %v\n", change.Path, formatValue(change.NewValue)))
		case ChangeTypeRemoved:
			buf.WriteString(fmt.Sprintf("- %s: %v\n", change.Path, formatValue(change.OldValue)))
		case ChangeTypeModified:
			buf.WriteString(fmt.Sprintf("~ %s:\n", change.Path))
			buf.WriteString(fmt.Sprintf("  - %v\n", formatValue(change.OldValue)))
			buf.WriteString(fmt.Sprintf("  + %v\n", formatValue(change.NewValue)))
		}
	}
	return buf.String()
}

// formatValue formats a value for display.
func formatValue(v interface{}) string {
	switch val := v.(type) {
	case string:
		if strings.Contains(val, "\n") {
			return fmt.Sprintf("%q", val)
		}
		return val
	case map[string]interface{}, []interface{}:
		data, _ := yaml.Marshal(val)
		return strings.TrimSpace(string(data))
	default:
		return fmt.Sprintf("%v", val)
	}
}
