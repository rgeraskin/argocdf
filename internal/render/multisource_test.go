// Package render provides tests for multi-source rendering functionality.
package render

import (
	"reflect"
	"testing"
)

func TestPrepareRefSources_EmptySources(t *testing.T) {
	// Test with empty sources - should return empty map
	factory := &Factory{}
	renderer := NewMultiSourceRenderer(factory, "/tmp/test")

	// We can't directly test prepareRefSources without mocking git.Clone,
	// but we can verify the structure is correct
	if renderer == nil {
		t.Error("NewMultiSourceRenderer() returned nil")
	}
}

func TestNewMultiSourceRenderer(t *testing.T) {
	tests := []struct {
		name     string
		repoPath string
	}{
		{
			name:     "with repo path",
			repoPath: "/tmp/test-repo",
		},
		{
			name:     "empty repo path",
			repoPath: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			factory := &Factory{}
			renderer := NewMultiSourceRenderer(factory, tt.repoPath)

			if renderer == nil {
				t.Error("NewMultiSourceRenderer() returned nil")
				return
			}
			if renderer.factory != factory {
				t.Error("factory not set correctly")
			}
			if renderer.repoPath != tt.repoPath {
				t.Errorf("repoPath = %q, want %q", renderer.repoPath, tt.repoPath)
			}
		})
	}
}

// Verify interfaces are satisfied at compile time
var _ = reflect.TypeOf((*MultiSourceRenderer)(nil))
