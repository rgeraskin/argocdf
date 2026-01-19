// Package diff provides tests for manifest parsing.
package diff

import (
	"testing"
)

func TestManifestKey(t *testing.T) {
	tests := []struct {
		name     string
		manifest Manifest
		wantKey  string
	}{
		{
			name: "namespaced resource",
			manifest: Manifest{
				Namespace: "default",
				Kind:      "ConfigMap",
				Name:      "my-config",
			},
			wantKey: "default/ConfigMap/my-config",
		},
		{
			name: "cluster-scoped resource",
			manifest: Manifest{
				Namespace: "",
				Kind:      "ClusterRole",
				Name:      "admin",
			},
			wantKey: "ClusterRole/admin",
		},
		{
			name: "deployment with namespace",
			manifest: Manifest{
				Namespace: "kube-system",
				Kind:      "Deployment",
				Name:      "coredns",
			},
			wantKey: "kube-system/Deployment/coredns",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.manifest.Key()
			if got != tt.wantKey {
				t.Errorf("Key() = %q, want %q", got, tt.wantKey)
			}
		})
	}
}

func TestParseManifests(t *testing.T) {
	parser := NewManifestParser()

	tests := []struct {
		name      string
		content   string
		wantCount int
		wantFirst *Manifest // nil to skip checking
	}{
		{
			name:      "empty content",
			content:   "",
			wantCount: 0,
		},
		{
			name: "single valid manifest",
			content: `apiVersion: v1
kind: ConfigMap
metadata:
  name: test-config
  namespace: default
data:
  key: value`,
			wantCount: 1,
			wantFirst: &Manifest{
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Name:       "test-config",
				Namespace:  "default",
			},
		},
		{
			name: "multiple manifests separated by ---",
			content: `apiVersion: v1
kind: ConfigMap
metadata:
  name: config1
  namespace: ns1
---
apiVersion: v1
kind: Secret
metadata:
  name: secret1
  namespace: ns1`,
			wantCount: 2,
		},
		{
			name: "manifest with leading document separator",
			content: `---
apiVersion: v1
kind: ConfigMap
metadata:
  name: test
  namespace: default`,
			wantCount: 1,
		},
		{
			name: "skip manifest without apiVersion",
			content: `kind: ConfigMap
metadata:
  name: test`,
			wantCount: 0,
		},
		{
			name: "skip manifest without kind",
			content: `apiVersion: v1
metadata:
  name: test`,
			wantCount: 0,
		},
		{
			name: "skip manifest without name",
			content: `apiVersion: v1
kind: ConfigMap
metadata:
  namespace: default`,
			wantCount: 0,
		},
		{
			name: "empty documents between separators",
			content: `---
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: test
  namespace: default
---
---`,
			wantCount: 1,
		},
		{
			name: "cluster-scoped resource without namespace",
			content: `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: admin-role`,
			wantCount: 1,
			wantFirst: &Manifest{
				APIVersion: "rbac.authorization.k8s.io/v1",
				Kind:       "ClusterRole",
				Name:       "admin-role",
				Namespace:  "",
			},
		},
		{
			name: "apps/v1 Deployment",
			content: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
  namespace: production
spec:
  replicas: 3`,
			wantCount: 1,
			wantFirst: &Manifest{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       "my-app",
				Namespace:  "production",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifests, err := parser.ParseManifests(tt.content)
			if err != nil {
				t.Fatalf("ParseManifests() error = %v", err)
			}

			if len(manifests) != tt.wantCount {
				t.Errorf("ParseManifests() returned %d manifests, want %d", len(manifests), tt.wantCount)
				for i, m := range manifests {
					t.Logf("  Manifest %d: %s %s/%s", i, m.APIVersion, m.Kind, m.Name)
				}
			}

			if tt.wantFirst != nil && len(manifests) > 0 {
				got := manifests[0]
				if got.APIVersion != tt.wantFirst.APIVersion {
					t.Errorf("First manifest APIVersion = %q, want %q", got.APIVersion, tt.wantFirst.APIVersion)
				}
				if got.Kind != tt.wantFirst.Kind {
					t.Errorf("First manifest Kind = %q, want %q", got.Kind, tt.wantFirst.Kind)
				}
				if got.Name != tt.wantFirst.Name {
					t.Errorf("First manifest Name = %q, want %q", got.Name, tt.wantFirst.Name)
				}
				if got.Namespace != tt.wantFirst.Namespace {
					t.Errorf("First manifest Namespace = %q, want %q", got.Namespace, tt.wantFirst.Namespace)
				}
			}
		})
	}
}

func TestManifestDifferDiffManifests(t *testing.T) {
	differ := NewManifestDiffer()

	tests := []struct {
		name           string
		oldContent     string
		newContent     string
		wantAdded      int
		wantRemoved    int
		wantModified   int
		wantUnchanged  int
		wantHasChanges bool
	}{
		{
			name:           "identical manifests - no changes",
			oldContent:     "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test\n  namespace: default\ndata:\n  key: value",
			newContent:     "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test\n  namespace: default\ndata:\n  key: value",
			wantAdded:      0,
			wantRemoved:    0,
			wantModified:   0,
			wantUnchanged:  1,
			wantHasChanges: false,
		},
		{
			name:           "new manifest added",
			oldContent:     "",
			newContent:     "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test\n  namespace: default",
			wantAdded:      1,
			wantRemoved:    0,
			wantModified:   0,
			wantUnchanged:  0,
			wantHasChanges: true,
		},
		{
			name:           "manifest removed",
			oldContent:     "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test\n  namespace: default",
			newContent:     "",
			wantAdded:      0,
			wantRemoved:    1,
			wantModified:   0,
			wantUnchanged:  0,
			wantHasChanges: true,
		},
		{
			name:           "manifest modified",
			oldContent:     "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test\n  namespace: default\ndata:\n  key: old",
			newContent:     "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test\n  namespace: default\ndata:\n  key: new",
			wantAdded:      0,
			wantRemoved:    0,
			wantModified:   1,
			wantUnchanged:  0,
			wantHasChanges: true,
		},
		{
			name: "mixed changes",
			oldContent: `apiVersion: v1
kind: ConfigMap
metadata:
  name: keep-unchanged
  namespace: default
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: to-be-removed
  namespace: default
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: to-be-modified
  namespace: default
data:
  key: old`,
			newContent: `apiVersion: v1
kind: ConfigMap
metadata:
  name: keep-unchanged
  namespace: default
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: newly-added
  namespace: default
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: to-be-modified
  namespace: default
data:
  key: new`,
			wantAdded:      1, // newly-added
			wantRemoved:    1, // to-be-removed
			wantModified:   1, // to-be-modified
			wantUnchanged:  1, // keep-unchanged
			wantHasChanges: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := differ.DiffManifests(tt.oldContent, tt.newContent)
			if err != nil {
				t.Fatalf("DiffManifests() error = %v", err)
			}

			if len(result.Added) != tt.wantAdded {
				t.Errorf("Added = %d, want %d", len(result.Added), tt.wantAdded)
			}
			if len(result.Removed) != tt.wantRemoved {
				t.Errorf("Removed = %d, want %d", len(result.Removed), tt.wantRemoved)
			}
			if len(result.Modified) != tt.wantModified {
				t.Errorf("Modified = %d, want %d", len(result.Modified), tt.wantModified)
			}
			if result.UnchangedCount != tt.wantUnchanged {
				t.Errorf("UnchangedCount = %d, want %d", result.UnchangedCount, tt.wantUnchanged)
			}
			if result.HasChanges != tt.wantHasChanges {
				t.Errorf("HasChanges = %v, want %v", result.HasChanges, tt.wantHasChanges)
			}
		})
	}
}

func TestExtractApplications(t *testing.T) {
	manifests := []Manifest{
		{APIVersion: "argoproj.io/v1alpha1", Kind: "Application", Name: "app1"},
		{APIVersion: "v1", Kind: "ConfigMap", Name: "config1"},
		{APIVersion: "argoproj.io/v1alpha1", Kind: "Application", Name: "app2"},
		{APIVersion: "apps/v1", Kind: "Deployment", Name: "deploy1"},
		{APIVersion: "argoproj.io/v1alpha1", Kind: "AppProject", Name: "project1"},
	}

	apps := ExtractApplications(manifests)

	if len(apps) != 2 {
		t.Errorf("ExtractApplications() returned %d, want 2", len(apps))
	}

	// Verify only Application kind with argoproj.io apiVersion are returned
	for _, app := range apps {
		if app.Kind != "Application" {
			t.Errorf("Expected Kind=Application, got %s", app.Kind)
		}
		if app.APIVersion != "argoproj.io/v1alpha1" {
			t.Errorf("Expected APIVersion=argoproj.io/v1alpha1, got %s", app.APIVersion)
		}
	}
}

func TestGetString(t *testing.T) {
	m := map[string]interface{}{
		"string": "value",
		"number": 123,
		"bool":   true,
		"nil":    nil,
		"nested": map[string]interface{}{"key": "value"},
	}

	tests := []struct {
		key  string
		want string
	}{
		{"string", "value"},
		{"number", ""},  // not a string
		{"bool", ""},    // not a string
		{"nil", ""},     // nil
		{"missing", ""}, // doesn't exist
		{"nested", ""},  // not a string (is a map)
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			got := getString(m, tt.key)
			if got != tt.want {
				t.Errorf("getString(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}
