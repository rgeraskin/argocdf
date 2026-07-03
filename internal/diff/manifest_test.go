// Package diff provides tests for manifest parsing.
package diff

import (
	"strings"
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
		{
			name: "core group apiVersion omits group segment",
			manifest: Manifest{
				APIVersion: "v1",
				Namespace:  "default",
				Kind:       "ConfigMap",
				Name:       "my-config",
			},
			wantKey: "default/ConfigMap/my-config",
		},
		{
			name: "namespaced resource with non-core group includes group",
			manifest: Manifest{
				APIVersion: "cert-manager.io/v1",
				Namespace:  "default",
				Kind:       "Certificate",
				Name:       "my-cert",
			},
			wantKey: "default/cert-manager.io/Certificate/my-cert",
		},
		{
			name: "cluster-scoped resource with group",
			manifest: Manifest{
				APIVersion: "rbac.authorization.k8s.io/v1",
				Kind:       "ClusterRole",
				Name:       "admin",
			},
			wantKey: "rbac.authorization.k8s.io/ClusterRole/admin",
		},
		{
			name: "version bump within a group keeps the same key",
			manifest: Manifest{
				APIVersion: "cert-manager.io/v1beta1",
				Namespace:  "default",
				Kind:       "Certificate",
				Name:       "my-cert",
			},
			// group but not version is included, so v1beta1 and v1 share a key.
			wantKey: "default/cert-manager.io/Certificate/my-cert",
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
			result := parser.ParseManifests(tt.content)

			if len(result.Manifests) != tt.wantCount {
				t.Errorf("ParseManifests() returned %d manifests, want %d", len(result.Manifests), tt.wantCount)
				for i, m := range result.Manifests {
					t.Logf("  Manifest %d: %s %s/%s", i, m.APIVersion, m.Kind, m.Name)
				}
			}

			if tt.wantFirst != nil && len(result.Manifests) > 0 {
				got := result.Manifests[0]
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

func TestDiffManifestSets_APIGroupDisambiguation(t *testing.T) {
	differ := NewManifestDiffer()

	t.Run("same kind and name in different groups are distinct manifests", func(t *testing.T) {
		// Two "Certificate" resources with the same name/namespace but from
		// different API groups must be tracked independently. Modifying one must
		// not affect the other, and neither may be dropped from the comparison.
		oldContent := `apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: my-cert
  namespace: default
spec:
  secretName: cm-secret
---
apiVersion: other-operator.io/v1
kind: Certificate
metadata:
  name: my-cert
  namespace: default
spec:
  secretName: oo-secret`
		newContent := `apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: my-cert
  namespace: default
spec:
  secretName: cm-secret-changed
---
apiVersion: other-operator.io/v1
kind: Certificate
metadata:
  name: my-cert
  namespace: default
spec:
  secretName: oo-secret`

		result, err := differ.DiffManifests(oldContent, newContent)
		if err != nil {
			t.Fatalf("DiffManifests() error = %v", err)
		}

		// The cert-manager Certificate changed; the other-operator one did not.
		if len(result.Modified) != 1 {
			t.Errorf("Modified = %d, want 1", len(result.Modified))
		}
		if result.UnchangedCount != 1 {
			t.Errorf("UnchangedCount = %d, want 1", result.UnchangedCount)
		}
		if len(result.Added) != 0 || len(result.Removed) != 0 {
			t.Errorf("Added/Removed = %d/%d, want 0/0", len(result.Added), len(result.Removed))
		}
		if len(result.Modified) == 1 {
			wantKey := "default/cert-manager.io/Certificate/my-cert"
			if result.Modified[0].Key != wantKey {
				t.Errorf("Modified key = %q, want %q", result.Modified[0].Key, wantKey)
			}
		}
	})

	t.Run("version bump within a group is reported as modified", func(t *testing.T) {
		// Bumping v1beta1 -> v1 within the same group must compare as a
		// modification, not as add+remove.
		oldContent := `apiVersion: cert-manager.io/v1beta1
kind: Certificate
metadata:
  name: my-cert
  namespace: default
spec:
  secretName: cm-secret`
		newContent := `apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: my-cert
  namespace: default
spec:
  secretName: cm-secret`

		result, err := differ.DiffManifests(oldContent, newContent)
		if err != nil {
			t.Fatalf("DiffManifests() error = %v", err)
		}

		if len(result.Modified) != 1 {
			t.Errorf("Modified = %d, want 1 (version bump should be a modification)", len(result.Modified))
		}
		if len(result.Added) != 0 || len(result.Removed) != 0 {
			t.Errorf("Added/Removed = %d/%d, want 0/0 (must not be add+remove)", len(result.Added), len(result.Removed))
		}
	})
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

func TestParseManifests_CollectsParseErrors(t *testing.T) {
	parser := NewManifestParser()

	tests := []struct {
		name              string
		content           string
		wantManifests     int
		wantParseErrors   int
		wantParseWarnings int
	}{
		{
			name: "duplicate key resolved with last-wins (warning, doc kept)",
			content: `apiVersion: v1
kind: ConfigMap
metadata:
  name: test
  annotations:
    key: value1
    key: value2`,
			wantManifests:     1, // Duplicate keys are non-fatal now; doc kept
			wantParseErrors:   0,
			wantParseWarnings: 1,
		},
		{
			name: "mixed valid and duplicate-key documents",
			content: `apiVersion: v1
kind: ConfigMap
metadata:
  name: valid-config
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: dup
  labels:
    app: test
    app: duplicate`,
			wantManifests:     2, // Both kept; second has a resolved duplicate
			wantParseErrors:   0,
			wantParseWarnings: 1,
		},
		{
			name: "genuine syntax error still lands in ParseErrors and doc skipped",
			content: `apiVersion: v1
kind: ConfigMap
metadata:
  name: valid-config
---
apiVersion: v1
kind: ConfigMap
data:
  broken: [unclosed, list`,
			wantManifests:     1, // Only the valid doc survives
			wantParseErrors:   1,
			wantParseWarnings: 0,
		},
		{
			name: "no errors or warnings",
			content: `apiVersion: v1
kind: ConfigMap
metadata:
  name: test
data:
  key: value`,
			wantManifests:     1,
			wantParseErrors:   0,
			wantParseWarnings: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parser.ParseManifests(tt.content)

			if len(result.Manifests) != tt.wantManifests {
				t.Errorf("ParseManifests() manifests = %d, want %d", len(result.Manifests), tt.wantManifests)
			}
			if len(result.ParseErrors) != tt.wantParseErrors {
				t.Errorf("ParseManifests() parseErrors = %d, want %d (%v)", len(result.ParseErrors), tt.wantParseErrors, result.ParseErrors)
			}
			if len(result.ParseWarnings) != tt.wantParseWarnings {
				t.Errorf("ParseManifests() parseWarnings = %d, want %d (%v)", len(result.ParseWarnings), tt.wantParseWarnings, result.ParseWarnings)
			}
		})
	}
}

// TestParseManifests_DuplicateKeyLastWins verifies the last-value-wins behavior
// for duplicate map keys and that the surviving document participates in diffs.
func TestParseManifests_DuplicateKeyLastWins(t *testing.T) {
	parser := NewManifestParser()

	content := `apiVersion: v1
kind: ConfigMap
metadata:
  name: test
  namespace: default
data:
  key: first
  key: last`

	result := parser.ParseManifests(content)

	if len(result.Manifests) != 1 {
		t.Fatalf("expected 1 manifest, got %d", len(result.Manifests))
	}
	if len(result.ParseWarnings) != 1 {
		t.Fatalf("expected 1 warning, got %d (%v)", len(result.ParseWarnings), result.ParseWarnings)
	}

	// Warning should reference kind/name and the duplicated key.
	warn := result.ParseWarnings[0]
	for _, want := range []string{"ConfigMap/test", `"key"`, "last value"} {
		if !strings.Contains(warn, want) {
			t.Errorf("warning %q missing %q", warn, want)
		}
	}

	// Last value wins.
	data, ok := result.Manifests[0].Object["data"].(map[string]interface{})
	if !ok {
		t.Fatalf("data is not a map: %T", result.Manifests[0].Object["data"])
	}
	if data["key"] != "last" {
		t.Errorf("data.key = %v, want \"last\" (last-wins)", data["key"])
	}
}

// TestParseManifests_NestedDuplicateKey verifies a duplicate key nested inside
// metadata.annotations is detected and resolved.
func TestParseManifests_NestedDuplicateKey(t *testing.T) {
	parser := NewManifestParser()

	content := `apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: web
  namespace: default
  annotations:
    nginx.ingress.kubernetes.io/service-upstream: "true"
    nginx.ingress.kubernetes.io/service-upstream: "false"`

	result := parser.ParseManifests(content)

	if len(result.Manifests) != 1 {
		t.Fatalf("expected 1 manifest, got %d", len(result.Manifests))
	}
	if len(result.ParseWarnings) != 1 {
		t.Fatalf("expected 1 warning, got %d (%v)", len(result.ParseWarnings), result.ParseWarnings)
	}

	meta := result.Manifests[0].Object["metadata"].(map[string]interface{})
	ann := meta["annotations"].(map[string]interface{})
	if ann["nginx.ingress.kubernetes.io/service-upstream"] != "false" {
		t.Errorf("annotation = %v, want \"false\" (last-wins)", ann["nginx.ingress.kubernetes.io/service-upstream"])
	}
}

// TestDiffManifestSets_DuplicateManifestKey verifies that two documents sharing
// the same manifest identity within one render produce a duplicate-manifest
// warning while the diff still functions.
func TestDiffManifestSets_DuplicateManifestKey(t *testing.T) {
	differ := NewManifestDiffer()

	oldContent := `apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: web
  namespace: default
spec:
  minAvailable: 1`

	// New render emits the PDB twice (e.g. the chart's own template AND a
	// library chart). Last one wins for the diff, but the collision is surfaced.
	newContent := `apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: web
  namespace: default
spec:
  minAvailable: 2
---
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: web
  namespace: default
spec:
  minAvailable: 3`

	result, err := differ.DiffManifests(oldContent, newContent)
	if err != nil {
		t.Fatalf("DiffManifests() error = %v", err)
	}

	// Duplicate-manifest warning surfaced.
	foundDup := false
	for _, w := range result.ParseWarnings {
		if strings.Contains(w, "duplicate manifest") && strings.Contains(w, "PodDisruptionBudget/web") {
			foundDup = true
		}
	}
	if !foundDup {
		t.Errorf("expected duplicate-manifest warning, got %v", result.ParseWarnings)
	}

	// Diff still works: last doc (minAvailable: 3) wins over old (1) => modified.
	if len(result.Modified) != 1 {
		t.Errorf("Modified = %d, want 1", len(result.Modified))
	}
	if !result.HasChanges {
		t.Errorf("HasChanges = false, want true")
	}
}

func TestDiffManifests_SideLabeledWarnings(t *testing.T) {
	differ := NewManifestDiffer()

	clean := `apiVersion: v1
kind: ConfigMap
metadata:
  name: app
data:
  a: "1"`

	// Same ConfigMap but with a duplicate key in data (parse warning).
	dupKey := `apiVersion: v1
kind: ConfigMap
metadata:
  name: app
data:
  a: "1"
  a: "2"`

	// Warning only in the target content is labeled [target].
	result, err := differ.DiffManifests(clean, dupKey)
	if err != nil {
		t.Fatalf("DiffManifests() error = %v", err)
	}
	if len(result.ParseWarnings) == 0 {
		t.Fatal("expected parse warnings, got none")
	}
	for _, w := range result.ParseWarnings {
		if !strings.HasPrefix(w, "[target] ") {
			t.Errorf("warning %q should be labeled [target]", w)
		}
	}

	// The same issue on both sides yields one [base] and one [target] entry.
	result, err = differ.DiffManifests(dupKey, dupKey)
	if err != nil {
		t.Fatalf("DiffManifests() error = %v", err)
	}
	var base, target bool
	for _, w := range result.ParseWarnings {
		if strings.HasPrefix(w, "[base] ") {
			base = true
		}
		if strings.HasPrefix(w, "[target] ") {
			target = true
		}
	}
	if !base || !target {
		t.Errorf("expected both [base] and [target] warnings, got %v", result.ParseWarnings)
	}

	// Duplicate manifests only in the old set are labeled [base].
	twoDocs := clean + "\n---\n" + clean
	result, err = differ.DiffManifests(twoDocs, clean)
	if err != nil {
		t.Fatalf("DiffManifests() error = %v", err)
	}
	foundBaseDup := false
	for _, w := range result.ParseWarnings {
		if strings.HasPrefix(w, "[base] duplicate manifest") {
			foundBaseDup = true
		}
	}
	if !foundBaseDup {
		t.Errorf("expected [base]-labeled duplicate-manifest warning, got %v", result.ParseWarnings)
	}
}
