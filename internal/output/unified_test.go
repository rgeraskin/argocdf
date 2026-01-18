package output

import (
	"strings"
	"testing"

	"github.com/rgeraskin/argocdf/internal/diff"
)

func TestGenerateUnifiedDiff(t *testing.T) {
	tests := []struct {
		name     string
		oldYAML  string
		newYAML  string
		filename string
		wantErr  bool
		check    func(string) bool
		desc     string
	}{
		{
			name:     "simple modification",
			oldYAML:  "key: oldvalue\n",
			newYAML:  "key: newvalue\n",
			filename: "test.yaml",
			check: func(d string) bool {
				return strings.Contains(d, "--- base/test.yaml") &&
					strings.Contains(d, "+++ target/test.yaml") &&
					strings.Contains(d, "-key: oldvalue") &&
					strings.Contains(d, "+key: newvalue")
			},
			desc: "should contain diff headers and changes",
		},
		{
			name:     "addition only",
			oldYAML:  "",
			newYAML:  "newkey: newvalue\n",
			filename: "new.yaml",
			check: func(d string) bool {
				return strings.Contains(d, "+newkey: newvalue")
			},
			desc: "should show addition",
		},
		{
			name:     "removal only",
			oldYAML:  "oldkey: oldvalue\n",
			newYAML:  "",
			filename: "removed.yaml",
			check: func(d string) bool {
				return strings.Contains(d, "-oldkey: oldvalue")
			},
			desc: "should show removal",
		},
		{
			name:     "no changes",
			oldYAML:  "same: value\n",
			newYAML:  "same: value\n",
			filename: "same.yaml",
			check: func(d string) bool {
				return d == "" // No diff when content is same
			},
			desc: "should be empty when no changes",
		},
		{
			name: "multiline yaml",
			oldYAML: `apiVersion: v1
kind: ConfigMap
metadata:
  name: test
data:
  key: value1
`,
			newYAML: `apiVersion: v1
kind: ConfigMap
metadata:
  name: test
data:
  key: value2
`,
			filename: "configmap.yaml",
			check: func(d string) bool {
				return strings.Contains(d, "-  key: value1") &&
					strings.Contains(d, "+  key: value2")
			},
			desc: "should show multiline yaml changes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := GenerateUnifiedDiff(tt.oldYAML, tt.newYAML, tt.filename)
			if tt.wantErr {
				if err == nil {
					t.Error("GenerateUnifiedDiff() expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("GenerateUnifiedDiff() unexpected error = %v", err)
				return
			}
			if !tt.check(got) {
				t.Errorf("GenerateUnifiedDiff() check failed: %s\nGot:\n%s", tt.desc, got)
			}
		})
	}
}

func TestGenerateManifestUnifiedDiffs(t *testing.T) {
	tests := []struct {
		name    string
		result  *diff.ManifestSetDiff
		wantErr bool
		check   func(map[string]string) bool
		desc    string
	}{
		{
			name: "modified manifest",
			result: &diff.ManifestSetDiff{
				HasChanges: true,
				Modified: []diff.ManifestDiff{
					{
						Key: "Deployment/default/nginx",
						Old: &diff.Manifest{Raw: "replicas: 1\n"},
						New: &diff.Manifest{Raw: "replicas: 3\n"},
					},
				},
			},
			check: func(diffs map[string]string) bool {
				d, ok := diffs["Deployment/default/nginx"]
				return ok && strings.Contains(d, "-replicas: 1") && strings.Contains(d, "+replicas: 3")
			},
			desc: "should generate diff for modified manifest",
		},
		{
			name: "added manifest",
			result: &diff.ManifestSetDiff{
				HasChanges: true,
				Added: []diff.Manifest{
					{
						Kind:      "ConfigMap",
						Namespace: "default",
						Name:      "newcm",
						Raw:       "data:\n  key: value\n",
					},
				},
			},
			check: func(diffs map[string]string) bool {
				// Key format is namespace/Kind/name
				d, ok := diffs["default/ConfigMap/newcm"]
				return ok && strings.Contains(d, "+data:") && strings.Contains(d, "+  key: value")
			},
			desc: "should generate diff for added manifest",
		},
		{
			name: "removed manifest",
			result: &diff.ManifestSetDiff{
				HasChanges: true,
				Removed: []diff.Manifest{
					{
						Kind:      "Secret",
						Namespace: "default",
						Name:      "oldsecret",
						Raw:       "type: Opaque\n",
					},
				},
			},
			check: func(diffs map[string]string) bool {
				// Key format is namespace/Kind/name
				d, ok := diffs["default/Secret/oldsecret"]
				return ok && strings.Contains(d, "-type: Opaque")
			},
			desc: "should generate diff for removed manifest",
		},
		{
			name: "empty result",
			result: &diff.ManifestSetDiff{
				HasChanges: false,
			},
			check: func(diffs map[string]string) bool {
				return len(diffs) == 0
			},
			desc: "should return empty map for no changes",
		},
		{
			name: "multiple changes",
			result: &diff.ManifestSetDiff{
				HasChanges: true,
				Added: []diff.Manifest{
					{Kind: "ConfigMap", Namespace: "ns1", Name: "cm1", Raw: "data: {}\n"},
				},
				Removed: []diff.Manifest{
					{Kind: "Secret", Namespace: "ns2", Name: "sec1", Raw: "type: Opaque\n"},
				},
				Modified: []diff.ManifestDiff{
					{
						Key: "ns3/Deployment/dep1",
						Old: &diff.Manifest{Raw: "old: data\n"},
						New: &diff.Manifest{Raw: "new: data\n"},
					},
				},
			},
			check: func(diffs map[string]string) bool {
				// Key format is namespace/Kind/name
				return len(diffs) == 3 &&
					diffs["ns1/ConfigMap/cm1"] != "" &&
					diffs["ns2/Secret/sec1"] != "" &&
					diffs["ns3/Deployment/dep1"] != ""
			},
			desc: "should generate diffs for all change types",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := GenerateManifestUnifiedDiffs(tt.result)
			if tt.wantErr {
				if err == nil {
					t.Error("GenerateManifestUnifiedDiffs() expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("GenerateManifestUnifiedDiffs() unexpected error = %v", err)
				return
			}
			if !tt.check(got) {
				t.Errorf("GenerateManifestUnifiedDiffs() check failed: %s\nGot: %v", tt.desc, got)
			}
		})
	}
}

func TestGetSortedKeys(t *testing.T) {
	result := &diff.ManifestSetDiff{
		Added: []diff.Manifest{
			{Kind: "ConfigMap", Namespace: "ns", Name: "added1"},
			{Kind: "ConfigMap", Namespace: "ns", Name: "added2"},
		},
		Removed: []diff.Manifest{
			{Kind: "Secret", Namespace: "ns", Name: "removed1"},
		},
		Modified: []diff.ManifestDiff{
			{Key: "ns/Deployment/modified1"},
			{Key: "ns/Deployment/modified2"},
		},
	}

	keys := GetSortedKeys(result)

	// Check order: added first, then removed, then modified
	// Key format is namespace/Kind/name
	expected := []string{
		"ns/ConfigMap/added1",
		"ns/ConfigMap/added2",
		"ns/Secret/removed1",
		"ns/Deployment/modified1",
		"ns/Deployment/modified2",
	}

	if len(keys) != len(expected) {
		t.Errorf("GetSortedKeys() returned %d keys, want %d", len(keys), len(expected))
		return
	}

	for i, key := range keys {
		if key != expected[i] {
			t.Errorf("GetSortedKeys()[%d] = %s, want %s", i, key, expected[i])
		}
	}
}

func TestCombineUnifiedDiffs(t *testing.T) {
	diffs := map[string]string{
		"key1": "diff1\n",
		"key2": "diff2\n",
		"key3": "diff3\n",
	}

	tests := []struct {
		name string
		keys []string
		want string
	}{
		{
			name: "all keys in order",
			keys: []string{"key1", "key2", "key3"},
			want: "diff1\n\ndiff2\n\ndiff3\n",
		},
		{
			name: "subset of keys",
			keys: []string{"key1", "key3"},
			want: "diff1\n\ndiff3\n",
		},
		{
			name: "missing key ignored",
			keys: []string{"key1", "nonexistent", "key2"},
			want: "diff1\n\ndiff2\n",
		},
		{
			name: "empty keys",
			keys: []string{},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CombineUnifiedDiffs(diffs, tt.keys)
			if got != tt.want {
				t.Errorf("CombineUnifiedDiffs() = %q, want %q", got, tt.want)
			}
		})
	}
}
