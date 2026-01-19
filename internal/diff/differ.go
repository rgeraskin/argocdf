// Package diff provides structured manifest comparison.
package diff

import (
	"fmt"
	"reflect"
	"sort"
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

// compareSlices compares two slices using name-based matching when possible.
// This produces better diffs for Kubernetes resources where list items often have
// a "name" field (containers, volumes, env vars, ports, etc.).
func (d *Differ) compareSlices(path string, oldSlice, newSlice []interface{}, result *DiffResult) {
	// Try name-based matching first
	oldByName, oldOrder := d.indexSliceByName(oldSlice)
	newByName, newOrder := d.indexSliceByName(newSlice)

	// If both slices have named items (at least some), use name-based comparison
	if len(oldByName) > 0 || len(newByName) > 0 {
		d.compareSlicesByName(path, oldByName, oldOrder, newByName, newOrder, result)
		return
	}

	// Fall back to index-based comparison for slices without named items
	d.compareSlicesByIndex(path, oldSlice, newSlice, result)
}

// indexSliceByName extracts items with a "name" field and returns them indexed by name.
// Also returns the order of names for consistent output.
// Items without names are not included in the map.
func (d *Differ) indexSliceByName(slice []interface{}) (map[string]interface{}, []string) {
	byName := make(map[string]interface{})
	var order []string

	for _, item := range slice {
		if m, ok := item.(map[string]interface{}); ok {
			if name, ok := d.extractItemName(m); ok {
				byName[name] = item
				order = append(order, name)
			}
		}
	}

	return byName, order
}

// extractItemName tries to extract a unique identifier from a slice item.
// It looks for common Kubernetes naming patterns.
func (d *Differ) extractItemName(item map[string]interface{}) (string, bool) {
	// Try common name fields in order of preference
	nameFields := []string{"name", "containerPort", "port", "key"}

	for _, field := range nameFields {
		if val, ok := item[field]; ok {
			switch v := val.(type) {
			case string:
				if v != "" {
					return v, true
				}
			case int, int64, float64:
				// For numeric fields like containerPort
				return fmt.Sprintf("%v", v), true
			}
		}
	}

	return "", false
}

// compareSlicesByName compares slices using name-based matching.
func (d *Differ) compareSlicesByName(path string, oldByName map[string]interface{}, oldOrder []string,
	newByName map[string]interface{}, newOrder []string, result *DiffResult) {

	// Track which names we've processed
	processed := make(map[string]bool)

	// First, process items in the old slice order
	for _, name := range oldOrder {
		processed[name] = true
		oldItem := oldByName[name]
		newItem, existsInNew := newByName[name]

		itemPath := fmt.Sprintf("%s[name=%s]", path, name)

		if !existsInNew {
			// Item was removed
			d.compareValues(itemPath, oldItem, nil, result)
		} else {
			// Item exists in both - compare them
			d.compareValues(itemPath, oldItem, newItem, result)
		}
	}

	// Then, process items that only exist in the new slice
	for _, name := range newOrder {
		if processed[name] {
			continue
		}
		newItem := newByName[name]
		itemPath := fmt.Sprintf("%s[name=%s]", path, name)
		// Item was added
		d.compareValues(itemPath, nil, newItem, result)
	}
}

// compareSlicesByIndex compares slices element by element using index positions.
// This is the fallback when items don't have identifiable names.
func (d *Differ) compareSlicesByIndex(path string, oldSlice, newSlice []interface{}, result *DiffResult) {
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
