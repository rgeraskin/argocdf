// Package diff provides tests for the Differ.
package diff

import (
	"testing"
)

func TestDiffObjects(t *testing.T) {
	tests := []struct {
		name         string
		oldObj       map[string]interface{}
		newObj       map[string]interface{}
		wantModified bool
		wantCount    int // number of changes
	}{
		{
			name:         "identical objects - no changes",
			oldObj:       map[string]interface{}{"key": "value"},
			newObj:       map[string]interface{}{"key": "value"},
			wantModified: false,
			wantCount:    0,
		},
		{
			name:         "added field",
			oldObj:       map[string]interface{}{},
			newObj:       map[string]interface{}{"newKey": "newValue"},
			wantModified: true,
			wantCount:    1,
		},
		{
			name:         "removed field",
			oldObj:       map[string]interface{}{"oldKey": "oldValue"},
			newObj:       map[string]interface{}{},
			wantModified: true,
			wantCount:    1,
		},
		{
			name:         "modified field",
			oldObj:       map[string]interface{}{"key": "oldValue"},
			newObj:       map[string]interface{}{"key": "newValue"},
			wantModified: true,
			wantCount:    1,
		},
		{
			name:         "nested map - no changes",
			oldObj:       map[string]interface{}{"spec": map[string]interface{}{"replicas": 3}},
			newObj:       map[string]interface{}{"spec": map[string]interface{}{"replicas": 3}},
			wantModified: false,
			wantCount:    0,
		},
		{
			name:         "nested map - field changed",
			oldObj:       map[string]interface{}{"spec": map[string]interface{}{"replicas": 3}},
			newObj:       map[string]interface{}{"spec": map[string]interface{}{"replicas": 5}},
			wantModified: true,
			wantCount:    1,
		},
		{
			name:         "nested map - field added",
			oldObj:       map[string]interface{}{"spec": map[string]interface{}{}},
			newObj:       map[string]interface{}{"spec": map[string]interface{}{"replicas": 3}},
			wantModified: true,
			wantCount:    1,
		},
		{
			name:         "slice - no changes",
			oldObj:       map[string]interface{}{"items": []interface{}{"a", "b"}},
			newObj:       map[string]interface{}{"items": []interface{}{"a", "b"}},
			wantModified: false,
			wantCount:    0,
		},
		{
			name:         "slice - item changed",
			oldObj:       map[string]interface{}{"items": []interface{}{"a", "b"}},
			newObj:       map[string]interface{}{"items": []interface{}{"a", "c"}},
			wantModified: true,
			wantCount:    1,
		},
		{
			name:         "slice - item added",
			oldObj:       map[string]interface{}{"items": []interface{}{"a"}},
			newObj:       map[string]interface{}{"items": []interface{}{"a", "b"}},
			wantModified: true,
			wantCount:    1,
		},
		{
			name:         "slice - item removed",
			oldObj:       map[string]interface{}{"items": []interface{}{"a", "b"}},
			newObj:       map[string]interface{}{"items": []interface{}{"a"}},
			wantModified: true,
			wantCount:    1,
		},
		{
			name:         "type change - string to int",
			oldObj:       map[string]interface{}{"value": "123"},
			newObj:       map[string]interface{}{"value": 123},
			wantModified: true,
			wantCount:    1,
		},
		{
			name:         "nil to value",
			oldObj:       map[string]interface{}{"key": nil},
			newObj:       map[string]interface{}{"key": "value"},
			wantModified: true,
			wantCount:    1,
		},
		{
			name:         "value to nil",
			oldObj:       map[string]interface{}{"key": "value"},
			newObj:       map[string]interface{}{"key": nil},
			wantModified: true,
			wantCount:    1,
		},
		{
			name:         "multiple changes",
			oldObj:       map[string]interface{}{"a": 1, "b": 2, "c": 3},
			newObj:       map[string]interface{}{"a": 1, "b": 20, "d": 4},
			wantModified: true,
			wantCount:    3, // b modified, c removed, d added
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := NewDiffer()
			result := d.DiffObjects(tt.oldObj, tt.newObj)

			if result.Modified != tt.wantModified {
				t.Errorf("Modified = %v, want %v", result.Modified, tt.wantModified)
			}
			if len(result.Changes) != tt.wantCount {
				t.Errorf("len(Changes) = %d, want %d", len(result.Changes), tt.wantCount)
				for _, c := range result.Changes {
					t.Logf("  Change: %s (%s)", c.Path, c.Type)
				}
			}
		})
	}
}

func TestDiffObjectsIgnoredFields(t *testing.T) {
	d := NewDiffer()

	// These fields should be ignored by default
	oldObj := map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":              "test",
			"resourceVersion":   "12345",
			"uid":               "abc-123",
			"generation":        int64(1),
			"creationTimestamp": "2024-01-01T00:00:00Z",
		},
		"status": map[string]interface{}{
			"phase": "Running",
		},
	}

	newObj := map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":              "test",
			"resourceVersion":   "67890",                // changed - should be ignored
			"uid":               "xyz-789",              // changed - should be ignored
			"generation":        int64(2),               // changed - should be ignored
			"creationTimestamp": "2024-02-01T00:00:00Z", // changed - should be ignored
		},
		"status": map[string]interface{}{
			"phase": "Completed", // changed - should be ignored
		},
	}

	result := d.DiffObjects(oldObj, newObj)

	if result.Modified {
		t.Errorf("Expected no changes (all changed fields should be ignored), got %d changes", len(result.Changes))
		for _, c := range result.Changes {
			t.Logf("  Unexpected change: %s", c.Path)
		}
	}
}

func TestDiffObjectsChangeTypes(t *testing.T) {
	d := NewDiffer()

	oldObj := map[string]interface{}{
		"removed":   "old",
		"modified":  "old",
		"unchanged": "same",
	}

	newObj := map[string]interface{}{
		"added":     "new",
		"modified":  "new",
		"unchanged": "same",
	}

	result := d.DiffObjects(oldObj, newObj)

	// Verify each change type
	changesByPath := make(map[string]FieldChange)
	for _, c := range result.Changes {
		changesByPath[c.Path] = c
	}

	// Check added
	if added, ok := changesByPath["added"]; !ok {
		t.Error("Expected 'added' change not found")
	} else if added.Type != ChangeTypeAdded {
		t.Errorf("'added' Type = %s, want %s", added.Type, ChangeTypeAdded)
	}

	// Check removed
	if removed, ok := changesByPath["removed"]; !ok {
		t.Error("Expected 'removed' change not found")
	} else if removed.Type != ChangeTypeRemoved {
		t.Errorf("'removed' Type = %s, want %s", removed.Type, ChangeTypeRemoved)
	}

	// Check modified
	if modified, ok := changesByPath["modified"]; !ok {
		t.Error("Expected 'modified' change not found")
	} else if modified.Type != ChangeTypeModified {
		t.Errorf("'modified' Type = %s, want %s", modified.Type, ChangeTypeModified)
	}
}

func TestCompareSlicesOrdering_Scalars(t *testing.T) {
	d := NewDiffer()

	// Slices of scalars (no name field) are compared by index
	oldObj := map[string]interface{}{
		"items": []interface{}{"first", "second", "third"},
	}

	newObj := map[string]interface{}{
		"items": []interface{}{"second", "first", "third"},
	}

	result := d.DiffObjects(oldObj, newObj)

	// Items at index 0 and 1 should be detected as modified (index-based comparison)
	if !result.Modified {
		t.Error("Expected changes when slice elements are reordered")
	}
	if len(result.Changes) != 2 {
		t.Errorf("Expected 2 changes for reordered elements, got %d", len(result.Changes))
	}
}

func TestCompareSlices_NameBasedMatching(t *testing.T) {
	d := NewDiffer()

	t.Run("reordered named items - no changes", func(t *testing.T) {
		// Items with "name" field should be matched by name, not index
		oldObj := map[string]interface{}{
			"containers": []interface{}{
				map[string]interface{}{"name": "app", "image": "app:v1"},
				map[string]interface{}{"name": "sidecar", "image": "sidecar:v1"},
			},
		}

		newObj := map[string]interface{}{
			"containers": []interface{}{
				map[string]interface{}{"name": "sidecar", "image": "sidecar:v1"}, // reordered
				map[string]interface{}{"name": "app", "image": "app:v1"},
			},
		}

		result := d.DiffObjects(oldObj, newObj)

		// No actual changes - just reordering
		if result.Modified {
			t.Error("Expected no changes when named items are just reordered")
			for _, c := range result.Changes {
				t.Logf("  Unexpected change: %s (%s)", c.Path, c.Type)
			}
		}
	})

	t.Run("named item modified", func(t *testing.T) {
		oldObj := map[string]interface{}{
			"containers": []interface{}{
				map[string]interface{}{"name": "app", "image": "app:v1"},
			},
		}

		newObj := map[string]interface{}{
			"containers": []interface{}{
				map[string]interface{}{"name": "app", "image": "app:v2"}, // image changed
			},
		}

		result := d.DiffObjects(oldObj, newObj)

		if !result.Modified {
			t.Error("Expected changes when named item is modified")
		}
		if len(result.Changes) != 1 {
			t.Errorf("Expected 1 change, got %d", len(result.Changes))
		}
		// Check path uses name-based addressing
		if len(result.Changes) > 0 && result.Changes[0].Path != "containers[name=app].image" {
			t.Errorf("Expected path 'containers[name=app].image', got '%s'", result.Changes[0].Path)
		}
	})

	t.Run("named item added", func(t *testing.T) {
		oldObj := map[string]interface{}{
			"containers": []interface{}{
				map[string]interface{}{"name": "app", "image": "app:v1"},
			},
		}

		newObj := map[string]interface{}{
			"containers": []interface{}{
				map[string]interface{}{"name": "app", "image": "app:v1"},
				map[string]interface{}{"name": "sidecar", "image": "sidecar:v1"},
			},
		}

		result := d.DiffObjects(oldObj, newObj)

		if !result.Modified {
			t.Error("Expected changes when named item is added")
		}
		// Should detect the new container
		found := false
		for _, c := range result.Changes {
			if c.Path == "containers[name=sidecar]" && c.Type == ChangeTypeAdded {
				found = true
			}
		}
		if !found {
			t.Error("Expected to find added container")
			for _, c := range result.Changes {
				t.Logf("  Change: %s (%s)", c.Path, c.Type)
			}
		}
	})

	t.Run("named item removed", func(t *testing.T) {
		oldObj := map[string]interface{}{
			"containers": []interface{}{
				map[string]interface{}{"name": "app", "image": "app:v1"},
				map[string]interface{}{"name": "sidecar", "image": "sidecar:v1"},
			},
		}

		newObj := map[string]interface{}{
			"containers": []interface{}{
				map[string]interface{}{"name": "app", "image": "app:v1"},
			},
		}

		result := d.DiffObjects(oldObj, newObj)

		if !result.Modified {
			t.Error("Expected changes when named item is removed")
		}
		// Should detect the removed container
		found := false
		for _, c := range result.Changes {
			if c.Path == "containers[name=sidecar]" && c.Type == ChangeTypeRemoved {
				found = true
			}
		}
		if !found {
			t.Error("Expected to find removed container")
			for _, c := range result.Changes {
				t.Logf("  Change: %s (%s)", c.Path, c.Type)
			}
		}
	})

	t.Run("port-based matching", func(t *testing.T) {
		oldObj := map[string]interface{}{
			"ports": []interface{}{
				map[string]interface{}{"containerPort": 8080, "protocol": "TCP"},
				map[string]interface{}{"containerPort": 9090, "protocol": "TCP"},
			},
		}

		newObj := map[string]interface{}{
			"ports": []interface{}{
				map[string]interface{}{"containerPort": 9090, "protocol": "TCP"}, // reordered
				map[string]interface{}{"containerPort": 8080, "protocol": "UDP"}, // protocol changed
			},
		}

		result := d.DiffObjects(oldObj, newObj)

		// Should detect only the protocol change on port 8080
		if !result.Modified {
			t.Error("Expected changes")
		}
		if len(result.Changes) != 1 {
			t.Errorf("Expected 1 change (protocol), got %d", len(result.Changes))
			for _, c := range result.Changes {
				t.Logf("  Change: %s (%s)", c.Path, c.Type)
			}
		}
	})
}

// hasChange reports whether result contains a change with the given path and type.
func hasChange(result *DiffResult, path string, changeType ChangeType) bool {
	for _, c := range result.Changes {
		if c.Path == path && c.Type == changeType {
			return true
		}
	}
	return false
}

// TestCompareSlices_SoundMatching verifies that name-based matching is only used
// when it is fully sound for both slices, and that the tool falls back to
// index-based comparison otherwise (so no list items are silently dropped or
// double-reported).
func TestCompareSlices_SoundMatching(t *testing.T) {
	d := NewDiffer()

	t.Run("mixed tolerations - change to unnamed item is not dropped", func(t *testing.T) {
		// One toleration has no "key" (only operator: Exists). Because the slice
		// mixes named and unnamed items, comparison must fall back to index-based
		// matching so a change to the unnamed item is still reported.
		oldObj := map[string]interface{}{
			"tolerations": []interface{}{
				map[string]interface{}{"key": "node-role", "operator": "Equal", "value": "true"},
				map[string]interface{}{"operator": "Exists"},
			},
		}
		newObj := map[string]interface{}{
			"tolerations": []interface{}{
				map[string]interface{}{"key": "node-role", "operator": "Equal", "value": "true"},
				map[string]interface{}{"operator": "Exists", "effect": "NoSchedule"}, // changed
			},
		}

		result := d.DiffObjects(oldObj, newObj)

		if !result.Modified {
			t.Fatal("Expected the change to the unnamed toleration to be detected")
		}
		if !hasChange(result, "tolerations[1].effect", ChangeTypeAdded) {
			t.Error("Expected added change at tolerations[1].effect")
			for _, c := range result.Changes {
				t.Logf("  Change: %s (%s)", c.Path, c.Type)
			}
		}
	})

	t.Run("duplicate extracted names - no item lost", func(t *testing.T) {
		// Two ports both extract to name "80" (differing only by protocol). Because
		// names are not unique, comparison must fall back to index-based matching
		// so removing one is detected and neither entry collapses or is lost.
		oldObj := map[string]interface{}{
			"ports": []interface{}{
				map[string]interface{}{"containerPort": 80, "protocol": "TCP"},
				map[string]interface{}{"containerPort": 80, "protocol": "UDP"},
			},
		}
		newObj := map[string]interface{}{
			"ports": []interface{}{
				map[string]interface{}{"containerPort": 80, "protocol": "TCP"},
			},
		}

		result := d.DiffObjects(oldObj, newObj)

		if !result.Modified {
			t.Fatal("Expected removal of the UDP port to be detected")
		}
		if len(result.Changes) != 1 {
			t.Errorf("Expected exactly 1 change, got %d", len(result.Changes))
			for _, c := range result.Changes {
				t.Logf("  Change: %s (%s)", c.Path, c.Type)
			}
		}
		if !hasChange(result, "ports[1]", ChangeTypeRemoved) {
			t.Error("Expected removed change at ports[1]")
		}
	})

	t.Run("fully-named unique items still use name-based paths", func(t *testing.T) {
		oldObj := map[string]interface{}{
			"containers": []interface{}{
				map[string]interface{}{"name": "app", "image": "app:v1"},
				map[string]interface{}{"name": "sidecar", "image": "sidecar:v1"},
			},
		}
		newObj := map[string]interface{}{
			"containers": []interface{}{
				map[string]interface{}{"name": "app", "image": "app:v2"}, // changed
				map[string]interface{}{"name": "sidecar", "image": "sidecar:v1"},
			},
		}

		result := d.DiffObjects(oldObj, newObj)

		if !result.Modified {
			t.Fatal("Expected the image change to be detected")
		}
		if !hasChange(result, "containers[name=app].image", ChangeTypeModified) {
			t.Error("Expected name-based path containers[name=app].image")
			for _, c := range result.Changes {
				t.Logf("  Change: %s (%s)", c.Path, c.Type)
			}
		}
	})
}

func TestDiffObjectsNestedSliceOfMaps(t *testing.T) {
	d := NewDiffer()

	oldObj := map[string]interface{}{
		"containers": []interface{}{
			map[string]interface{}{"name": "app", "image": "app:v1"},
		},
	}

	newObj := map[string]interface{}{
		"containers": []interface{}{
			map[string]interface{}{"name": "app", "image": "app:v2"},
		},
	}

	result := d.DiffObjects(oldObj, newObj)

	if !result.Modified {
		t.Error("Expected changes when nested map in slice is modified")
	}

	// Should detect the image change using name-based addressing
	found := false
	for _, c := range result.Changes {
		// With name-based matching, path will be containers[name=app].image
		if c.Path == "containers[name=app].image" && c.Type == ChangeTypeModified {
			found = true
			if c.OldValue != "app:v1" || c.NewValue != "app:v2" {
				t.Errorf("Expected image change v1->v2, got %v->%v", c.OldValue, c.NewValue)
			}
		}
	}
	if !found {
		t.Error("Expected to find containers[name=app].image change")
		for _, c := range result.Changes {
			t.Logf("  Found: %s (%s)", c.Path, c.Type)
		}
	}
}
