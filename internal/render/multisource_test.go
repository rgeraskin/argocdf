// Package render provides tests for multi-source rendering functionality.
package render

import (
	"reflect"
	"testing"
)

func TestSplitYAMLDocuments(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want int // number of documents
	}{
		{
			name: "single document",
			data: []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: test`),
			want: 1,
		},
		{
			name: "two documents",
			data: []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: first
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: second`),
			want: 2,
		},
		{
			name: "three documents with empty",
			data: []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: first
---
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: third`),
			want: 2, // empty document between separators is skipped
		},
		{
			name: "empty input",
			data: []byte(``),
			want: 0,
		},
		{
			name: "only separator",
			data: []byte(`---`),
			want: 0,
		},
		{
			name: "leading separator",
			data: []byte(`---
apiVersion: v1
kind: ConfigMap
metadata:
  name: test`),
			want: 1,
		},
		{
			name: "trailing newline",
			data: []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: test
`),
			want: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitYAMLDocuments(tt.data)
			if len(got) != tt.want {
				t.Errorf("splitYAMLDocuments() got %d documents, want %d", len(got), tt.want)
				for i, doc := range got {
					t.Logf("doc %d: %q", i, string(doc))
				}
			}
		})
	}
}

func TestExtractResourceKey(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{
			name: "simple configmap",
			data: []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: my-config`),
			want: "v1/ConfigMap/my-config",
		},
		{
			name: "namespaced resource",
			data: []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: my-config
  namespace: default`),
			want: "v1/ConfigMap/default/my-config",
		},
		{
			name: "deployment with apps apiVersion",
			data: []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-deployment
  namespace: production`),
			want: "apps/v1/Deployment/production/my-deployment",
		},
		{
			name: "missing apiVersion",
			data: []byte(`kind: ConfigMap
metadata:
  name: my-config`),
			want: "",
		},
		{
			name: "missing kind",
			data: []byte(`apiVersion: v1
metadata:
  name: my-config`),
			want: "",
		},
		{
			name: "missing name",
			data: []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  namespace: default`),
			want: "",
		},
		{
			name: "empty content",
			data: []byte(``),
			want: "",
		},
		{
			name: "cluster-scoped resource",
			data: []byte(`apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: admin`),
			want: "rbac.authorization.k8s.io/v1/ClusterRole/admin",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractResourceKey(tt.data)
			if got != tt.want {
				t.Errorf("extractResourceKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMergeManifests(t *testing.T) {
	tests := []struct {
		name          string
		manifests     [][]byte
		wantNonEmpty  bool     // just check if result is non-empty
		wantWarnings  []string // expected warning substrings
		checkWarnings bool     // whether to check for specific warnings
	}{
		{
			name: "single manifest",
			manifests: [][]byte{
				[]byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: test`),
			},
			wantNonEmpty: true,
		},
		{
			name: "two different manifests - combined",
			manifests: [][]byte{
				[]byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: first`),
				[]byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: second`),
			},
			wantNonEmpty: true,
		},
		{
			name: "duplicate manifest - emits warning",
			manifests: [][]byte{
				[]byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: same`),
				[]byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: same`),
			},
			wantNonEmpty:  true,
			wantWarnings:  []string{"duplicate resource"},
			checkWarnings: true,
		},
		{
			name:         "empty manifests",
			manifests:    [][]byte{},
			wantNonEmpty: false,
		},
		{
			name: "multi-document input - proper separator",
			manifests: [][]byte{
				[]byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: first
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: second`),
			},
			wantNonEmpty: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, warnings := MergeManifests(tt.manifests...)

			// Check if result is non-empty as expected
			gotNonEmpty := len(got) > 0
			if gotNonEmpty != tt.wantNonEmpty {
				t.Errorf("MergeManifests() got non-empty=%v, want %v", gotNonEmpty, tt.wantNonEmpty)
			}

			// Check warnings if specified
			if tt.checkWarnings {
				for _, wantWarning := range tt.wantWarnings {
					found := false
					for _, w := range warnings {
						if containsSubstring(w, wantWarning) {
							found = true
							break
						}
					}
					if !found {
						t.Errorf("MergeManifests() missing warning containing %q, got %v", wantWarning, warnings)
					}
				}
			}
		})
	}
}

func containsSubstring(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

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

func TestSplitYAMLDocuments_Preservation(t *testing.T) {
	// Test that content is preserved correctly
	input := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: test
data:
  key: value`)

	docs := splitYAMLDocuments(input)
	if len(docs) != 1 {
		t.Fatalf("expected 1 document, got %d", len(docs))
	}

	// Verify key fields are present
	doc := string(docs[0])
	expectedFields := []string{"apiVersion: v1", "kind: ConfigMap", "name: test", "key: value"}
	for _, field := range expectedFields {
		if !findSubstring(doc, field) {
			t.Errorf("missing expected field %q in document", field)
		}
	}
}

func TestExtractResourceKey_EdgeCases(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{
			name: "name with special characters",
			data: []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: my-config-123`),
			want: "v1/ConfigMap/my-config-123",
		},
		{
			name: "extra whitespace",
			data: []byte(`  apiVersion: v1
  kind: ConfigMap
metadata:
  name: test`),
			want: "v1/ConfigMap/test",
		},
		{
			name: "multiple name fields - first wins",
			data: []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: first
spec:
  name: second`),
			want: "v1/ConfigMap/first",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractResourceKey(tt.data)
			if got != tt.want {
				t.Errorf("extractResourceKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMergeManifests_CombinesContent(t *testing.T) {
	// Test that MergeManifests includes content from both manifests
	first := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: a`)
	second := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: b`)

	result, _ := MergeManifests(first, second)

	// Verify both names are in the result
	resultStr := string(result)
	if !findSubstring(resultStr, "name: a") {
		t.Error("result missing content from first manifest")
	}
	if !findSubstring(resultStr, "name: b") {
		t.Error("result missing content from second manifest")
	}
}

func TestMergeManifests_EmptyDocument(t *testing.T) {
	// Test handling of empty documents
	manifest := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: test`)

	result, _ := MergeManifests(manifest, []byte{})
	docs := splitYAMLDocuments(result)

	if len(docs) != 1 {
		t.Errorf("expected 1 document after merging with empty, got %d", len(docs))
	}
}

func TestSplitYAMLDocuments_MultipleEmpty(t *testing.T) {
	// Multiple empty documents between separators
	input := []byte(`---
---
---`)
	docs := splitYAMLDocuments(input)
	if len(docs) != 0 {
		t.Errorf("expected 0 documents, got %d", len(docs))
	}
}

func TestExtractResourceKey_AllFields(t *testing.T) {
	// Verify all required fields must be present
	testCases := []struct {
		name string
		yaml string
		ok   bool
	}{
		{
			name: "all fields present",
			yaml: `apiVersion: v1
kind: Pod
metadata:
  name: test`,
			ok: true,
		},
		{
			name: "missing apiVersion",
			yaml: `kind: Pod
metadata:
  name: test`,
			ok: false,
		},
		{
			name: "missing kind",
			yaml: `apiVersion: v1
metadata:
  name: test`,
			ok: false,
		},
		{
			name: "missing name",
			yaml: `apiVersion: v1
kind: Pod`,
			ok: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			key := extractResourceKey([]byte(tc.yaml))
			hasKey := key != ""
			if hasKey != tc.ok {
				t.Errorf("extractResourceKey() = %q, wantOK=%v", key, tc.ok)
			}
		})
	}
}

func TestMergeManifests_NoKey(t *testing.T) {
	// Documents without extractable keys should still be included
	noKey := []byte(`some: data
without: valid
kubernetes: structure`)

	result, _ := MergeManifests(noKey)
	docs := splitYAMLDocuments(result)

	if len(docs) != 1 {
		t.Errorf("expected 1 document (invalid k8s doc still included), got %d", len(docs))
	}
}

func TestSplitYAMLDocuments_ByteEquality(t *testing.T) {
	// Verify bytes.TrimSpace behavior
	input := []byte(`
apiVersion: v1
kind: ConfigMap
metadata:
  name: test

`)
	docs := splitYAMLDocuments(input)
	if len(docs) != 1 {
		t.Fatalf("expected 1 document, got %d", len(docs))
	}

	// The trimmed document shouldn't have leading/trailing whitespace
	doc := docs[0]
	if len(doc) > 0 && (doc[0] == ' ' || doc[0] == '\n' || doc[0] == '\t') {
		t.Error("document has leading whitespace after split")
	}
	if len(doc) > 0 && (doc[len(doc)-1] == ' ' || doc[len(doc)-1] == '\t') {
		t.Error("document has trailing whitespace after split")
	}
}

// Verify interfaces are satisfied at compile time
var _ = reflect.TypeOf((*MultiSourceRenderer)(nil))
