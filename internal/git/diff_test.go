// Package git provides tests for git diff functionality.
package git

import (
	"testing"
)

func TestParseNameStatus(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantAdded    []string
		wantModified []string
		wantDeleted  []string
	}{
		{
			name:         "empty input",
			input:        "",
			wantAdded:    []string{},
			wantModified: []string{},
			wantDeleted:  []string{},
		},
		{
			name:         "single added file",
			input:        "A\tpath/to/file.yaml",
			wantAdded:    []string{"path/to/file.yaml"},
			wantModified: []string{},
			wantDeleted:  []string{},
		},
		{
			name:         "single modified file",
			input:        "M\tpath/to/file.yaml",
			wantAdded:    []string{},
			wantModified: []string{"path/to/file.yaml"},
			wantDeleted:  []string{},
		},
		{
			name:         "single deleted file",
			input:        "D\tpath/to/file.yaml",
			wantAdded:    []string{},
			wantModified: []string{},
			wantDeleted:  []string{"path/to/file.yaml"},
		},
		{
			name:         "multiple files of same type",
			input:        "A\tfile1.yaml\nA\tfile2.yaml\nA\tfile3.yaml",
			wantAdded:    []string{"file1.yaml", "file2.yaml", "file3.yaml"},
			wantModified: []string{},
			wantDeleted:  []string{},
		},
		{
			name:         "mixed statuses",
			input:        "A\tnew.yaml\nM\tchanged.yaml\nD\tremoved.yaml",
			wantAdded:    []string{"new.yaml"},
			wantModified: []string{"changed.yaml"},
			wantDeleted:  []string{"removed.yaml"},
		},
		{
			name:         "rename detection - R100",
			input:        "R100\told-name.yaml\tnew-name.yaml",
			wantAdded:    []string{"new-name.yaml"},
			wantModified: []string{},
			wantDeleted:  []string{"old-name.yaml"},
		},
		{
			name:         "rename detection - partial match R050",
			input:        "R050\told-name.yaml\tnew-name.yaml",
			wantAdded:    []string{"new-name.yaml"},
			wantModified: []string{},
			wantDeleted:  []string{"old-name.yaml"},
		},
		{
			name:         "copy detection - C100",
			input:        "C100\toriginal.yaml\tcopy.yaml",
			wantAdded:    []string{"copy.yaml"},
			wantModified: []string{},
			wantDeleted:  []string{},
		},
		{
			name:         "unknown status treated as modified",
			input:        "U\tunknown-status.yaml",
			wantAdded:    []string{},
			wantModified: []string{"unknown-status.yaml"},
			wantDeleted:  []string{},
		},
		{
			name:         "whitespace handling",
			input:        "  A\tfile.yaml  \n\nM\tanother.yaml\n",
			wantAdded:    []string{"file.yaml"},
			wantModified: []string{"another.yaml"},
			wantDeleted:  []string{},
		},
		{
			name:         "malformed line - no tab",
			input:        "A file-without-tab.yaml",
			wantAdded:    []string{},
			wantModified: []string{},
			wantDeleted:  []string{},
		},
		{
			name:         "rename without new path - ignored",
			input:        "R100\told-only.yaml",
			wantAdded:    []string{},
			wantModified: []string{},
			wantDeleted:  []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseNameStatus(tt.input)

			if !stringSliceEqual(got.Added, tt.wantAdded) {
				t.Errorf("Added = %v, want %v", got.Added, tt.wantAdded)
			}
			if !stringSliceEqual(got.Modified, tt.wantModified) {
				t.Errorf("Modified = %v, want %v", got.Modified, tt.wantModified)
			}
			if !stringSliceEqual(got.Deleted, tt.wantDeleted) {
				t.Errorf("Deleted = %v, want %v", got.Deleted, tt.wantDeleted)
			}
		})
	}
}

func TestChangedFilesAllPaths(t *testing.T) {
	tests := []struct {
		name     string
		added    []string
		modified []string
		deleted  []string
		want     []string
	}{
		{
			name:     "empty",
			added:    []string{},
			modified: []string{},
			deleted:  []string{},
			want:     []string{},
		},
		{
			name:     "only added",
			added:    []string{"a.yaml", "b.yaml"},
			modified: []string{},
			deleted:  []string{},
			want:     []string{"a.yaml", "b.yaml"},
		},
		{
			name:     "only modified",
			added:    []string{},
			modified: []string{"c.yaml"},
			deleted:  []string{},
			want:     []string{"c.yaml"},
		},
		{
			name:     "only deleted",
			added:    []string{},
			modified: []string{},
			deleted:  []string{"d.yaml"},
			want:     []string{"d.yaml"},
		},
		{
			name:     "all types combined",
			added:    []string{"a.yaml"},
			modified: []string{"m.yaml"},
			deleted:  []string{"d.yaml"},
			want:     []string{"a.yaml", "m.yaml", "d.yaml"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cf := &ChangedFiles{
				Added:    tt.added,
				Modified: tt.modified,
				Deleted:  tt.deleted,
			}
			got := cf.AllPaths()

			if !stringSliceEqual(got, tt.want) {
				t.Errorf("AllPaths() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestChangedFilesHasChangesInPath(t *testing.T) {
	cf := &ChangedFiles{
		Added:    []string{"apps/app1/values.yaml", "apps/app2/chart/Chart.yaml"},
		Modified: []string{"base/kustomization.yaml"},
		Deleted:  []string{"old/config.yaml"},
	}

	tests := []struct {
		name    string
		dirPath string
		want    bool
	}{
		{
			name:    "exact directory match - added file",
			dirPath: "apps/app1",
			want:    true,
		},
		{
			name:    "exact directory match - modified file",
			dirPath: "base",
			want:    true,
		},
		{
			name:    "exact directory match - deleted file",
			dirPath: "old",
			want:    true,
		},
		{
			name:    "nested path match",
			dirPath: "apps/app2/chart",
			want:    true,
		},
		{
			name:    "parent path matches child",
			dirPath: "apps",
			want:    true,
		},
		{
			name:    "no match - different path",
			dirPath: "other",
			want:    false,
		},
		{
			name:    "no match - partial name overlap",
			dirPath: "app",
			want:    false,
		},
		{
			name:    "path with trailing slash",
			dirPath: "apps/app1/",
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cf.HasChangesInPath(tt.dirPath)
			if got != tt.want {
				t.Errorf("HasChangesInPath(%q) = %v, want %v", tt.dirPath, got, tt.want)
			}
		})
	}
}

// stringSliceEqual compares two string slices for equality.
func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
