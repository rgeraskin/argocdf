// Package output provides tests for output functionality.
package output

import (
	"errors"
	"testing"

	"github.com/rgeraskin/argocdf/internal/diff"
	"github.com/rgeraskin/argocdf/internal/types"
)

func TestNullWriter(t *testing.T) {
	w := NewNullWriter()

	// All methods should return nil
	if err := w.WriteHeader("Test"); err != nil {
		t.Errorf("WriteHeader() error = %v, want nil", err)
	}

	if err := w.WriteAppDiff(&types.AppDiff{}, 0); err != nil {
		t.Errorf("WriteAppDiff() error = %v, want nil", err)
	}

	if err := w.WriteTree(&diff.AppTree{}); err != nil {
		t.Errorf("WriteTree() error = %v, want nil", err)
	}

	if err := w.WriteSummary(Summary{}); err != nil {
		t.Errorf("WriteSummary() error = %v, want nil", err)
	}

	if err := w.WriteFooter(); err != nil {
		t.Errorf("WriteFooter() error = %v, want nil", err)
	}

	if err := w.Flush(); err != nil {
		t.Errorf("Flush() error = %v, want nil", err)
	}
}

// mockWriter is a mock implementation of Writer for testing MultiWriter.
type mockWriter struct {
	headerCalled  bool
	appDiffCalled bool
	treeCalled    bool
	summaryCalled bool
	footerCalled  bool
	flushCalled   bool
	shouldError   bool
	lastTitle     string
	lastAppDiff   *types.AppDiff
	lastSummary   Summary
}

func (m *mockWriter) WriteHeader(title string) error {
	m.headerCalled = true
	m.lastTitle = title
	if m.shouldError {
		return errors.New("mock error")
	}
	return nil
}

func (m *mockWriter) WriteAppDiff(appDiff *types.AppDiff, depth int) error {
	m.appDiffCalled = true
	m.lastAppDiff = appDiff
	if m.shouldError {
		return errors.New("mock error")
	}
	return nil
}

func (m *mockWriter) WriteTree(tree *diff.AppTree) error {
	m.treeCalled = true
	if m.shouldError {
		return errors.New("mock error")
	}
	return nil
}

func (m *mockWriter) WriteSummary(summary Summary) error {
	m.summaryCalled = true
	m.lastSummary = summary
	if m.shouldError {
		return errors.New("mock error")
	}
	return nil
}

func (m *mockWriter) WriteFooter() error {
	m.footerCalled = true
	if m.shouldError {
		return errors.New("mock error")
	}
	return nil
}

func (m *mockWriter) Flush() error {
	m.flushCalled = true
	if m.shouldError {
		return errors.New("mock error")
	}
	return nil
}

func TestMultiWriter_FansOutToAllWriters(t *testing.T) {
	w1 := &mockWriter{}
	w2 := &mockWriter{}
	multi := NewMultiWriter(w1, w2)

	// Test WriteHeader
	if err := multi.WriteHeader("Test Title"); err != nil {
		t.Errorf("WriteHeader() error = %v", err)
	}
	if !w1.headerCalled || !w2.headerCalled {
		t.Error("WriteHeader() not called on all writers")
	}
	if w1.lastTitle != "Test Title" || w2.lastTitle != "Test Title" {
		t.Error("WriteHeader() title not passed correctly")
	}

	// Test WriteAppDiff
	appDiff := &types.AppDiff{Name: "test-app"}
	if err := multi.WriteAppDiff(appDiff, 0); err != nil {
		t.Errorf("WriteAppDiff() error = %v", err)
	}
	if !w1.appDiffCalled || !w2.appDiffCalled {
		t.Error("WriteAppDiff() not called on all writers")
	}

	// Test WriteTree
	if err := multi.WriteTree(&diff.AppTree{}); err != nil {
		t.Errorf("WriteTree() error = %v", err)
	}
	if !w1.treeCalled || !w2.treeCalled {
		t.Error("WriteTree() not called on all writers")
	}

	// Test WriteSummary
	summary := Summary{TotalApps: 5, AppsWithChanges: 3}
	if err := multi.WriteSummary(summary); err != nil {
		t.Errorf("WriteSummary() error = %v", err)
	}
	if !w1.summaryCalled || !w2.summaryCalled {
		t.Error("WriteSummary() not called on all writers")
	}

	// Test WriteFooter
	if err := multi.WriteFooter(); err != nil {
		t.Errorf("WriteFooter() error = %v", err)
	}
	if !w1.footerCalled || !w2.footerCalled {
		t.Error("WriteFooter() not called on all writers")
	}

	// Test Flush
	if err := multi.Flush(); err != nil {
		t.Errorf("Flush() error = %v", err)
	}
	if !w1.flushCalled || !w2.flushCalled {
		t.Error("Flush() not called on all writers")
	}
}

func TestMultiWriter_ErrorPropagation(t *testing.T) {
	w1 := &mockWriter{shouldError: true}
	w2 := &mockWriter{}
	multi := NewMultiWriter(w1, w2)

	// Error from first writer should be returned
	if err := multi.WriteHeader("Test"); err == nil {
		t.Error("WriteHeader() should return error from first writer")
	}

	// Second writer should not be called after first error
	if w2.headerCalled {
		t.Error("WriteHeader() should not call second writer after first error")
	}

	// Reset and test with error on second writer
	w1 = &mockWriter{}
	w2 = &mockWriter{shouldError: true}
	multi = NewMultiWriter(w1, w2)

	if err := multi.WriteHeader("Test"); err == nil {
		t.Error("WriteHeader() should return error from second writer")
	}
	if !w1.headerCalled {
		t.Error("First writer should be called")
	}
}

func TestMultiWriter_EmptyWriters(t *testing.T) {
	multi := NewMultiWriter()

	// All methods should succeed with no writers
	if err := multi.WriteHeader("Test"); err != nil {
		t.Errorf("WriteHeader() with no writers error = %v", err)
	}
	if err := multi.WriteAppDiff(&types.AppDiff{}, 0); err != nil {
		t.Errorf("WriteAppDiff() with no writers error = %v", err)
	}
	if err := multi.WriteTree(&diff.AppTree{}); err != nil {
		t.Errorf("WriteTree() with no writers error = %v", err)
	}
	if err := multi.WriteSummary(Summary{}); err != nil {
		t.Errorf("WriteSummary() with no writers error = %v", err)
	}
	if err := multi.WriteFooter(); err != nil {
		t.Errorf("WriteFooter() with no writers error = %v", err)
	}
	if err := multi.Flush(); err != nil {
		t.Errorf("Flush() with no writers error = %v", err)
	}
}

func TestComputeSummary(t *testing.T) {
	tests := []struct {
		name  string
		diffs []*types.AppDiff
		want  Summary
	}{
		{
			name:  "empty diffs",
			diffs: []*types.AppDiff{},
			want:  Summary{TotalApps: 0},
		},
		{
			name: "app with error",
			diffs: []*types.AppDiff{
				{Name: "app1", Error: errors.New("render failed")},
			},
			want: Summary{
				TotalApps:      1,
				AppsWithErrors: 1,
			},
		},
		{
			name: "app with no changes",
			diffs: []*types.AppDiff{
				{
					Name: "app1",
					DiffResult: &diff.ManifestSetDiff{
						HasChanges: false,
					},
				},
			},
			want: Summary{
				TotalApps:       1,
				AppsWithChanges: 0,
			},
		},
		{
			name: "app with changes",
			diffs: []*types.AppDiff{
				{
					Name: "app1",
					DiffResult: &diff.ManifestSetDiff{
						HasChanges: true,
						Added: []diff.Manifest{
							{Kind: "ConfigMap", Name: "cm1"},
							{Kind: "Application", Name: "child-app"}, // New Application CRD
						},
						Removed: []diff.Manifest{
							{Kind: "Secret", Name: "s1"},
						},
						Modified: []diff.ManifestDiff{
							{Key: "Deployment/test"},
						},
					},
				},
			},
			want: Summary{
				TotalApps:       1,
				AppsWithChanges: 1,
				TotalAdded:      2,
				TotalRemoved:    1,
				TotalModified:   1,
				NewApplications: 1,
			},
		},
		{
			name: "multiple apps mixed",
			diffs: []*types.AppDiff{
				{Name: "app1", Error: errors.New("error")},
				{
					Name: "app2",
					DiffResult: &diff.ManifestSetDiff{
						HasChanges: true,
						Added:      []diff.Manifest{{Kind: "ConfigMap"}},
					},
				},
				{
					Name: "app3",
					DiffResult: &diff.ManifestSetDiff{
						HasChanges: false,
					},
				},
			},
			want: Summary{
				TotalApps:       3,
				AppsWithChanges: 1,
				AppsWithErrors:  1,
				TotalAdded:      1,
			},
		},
		{
			name: "nil diff result",
			diffs: []*types.AppDiff{
				{Name: "app1", DiffResult: nil},
			},
			want: Summary{
				TotalApps: 1,
			},
		},
		{
			name: "app with parse errors",
			diffs: []*types.AppDiff{
				{
					Name: "app1",
					DiffResult: &diff.ManifestSetDiff{
						HasChanges:  false,
						ParseErrors: []string{"yaml: duplicate key"},
					},
				},
			},
			want: Summary{
				TotalApps:      1,
				AppsWithErrors: 1,
			},
		},
		{
			name: "app with multiple parse errors counts as one errored app",
			diffs: []*types.AppDiff{
				{
					Name: "app1",
					DiffResult: &diff.ManifestSetDiff{
						HasChanges: false,
						ParseErrors: []string{
							"yaml: duplicate key on line 10",
							"yaml: duplicate key on line 20",
							"yaml: duplicate key on line 30",
						},
					},
				},
			},
			want: Summary{
				TotalApps:      1,
				AppsWithErrors: 1, // Should be 1, not 3
			},
		},
		{
			name: "app with both changes and parse errors",
			diffs: []*types.AppDiff{
				{
					Name: "app1",
					DiffResult: &diff.ManifestSetDiff{
						HasChanges: true,
						Added: []diff.Manifest{
							{Kind: "ConfigMap", Name: "cm1"},
						},
						ParseErrors: []string{"yaml: duplicate key"},
					},
				},
			},
			want: Summary{
				TotalApps:       1,
				AppsWithChanges: 1,
				AppsWithErrors:  1, // Counts in both
				TotalAdded:      1,
			},
		},
		{
			name: "app with parse warnings is NOT counted as errored",
			diffs: []*types.AppDiff{
				{
					Name: "app1",
					DiffResult: &diff.ManifestSetDiff{
						HasChanges: true,
						Added: []diff.Manifest{
							{Kind: "ConfigMap", Name: "cm1"},
						},
						ParseWarnings: []string{
							`resource ConfigMap/cm1: duplicate key "foo" (using last value)`,
						},
					},
				},
			},
			want: Summary{
				TotalApps:       1,
				AppsWithChanges: 1,
				AppsWithErrors:  0, // Warnings must NOT count as errors
				TotalAdded:      1,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeSummary(tt.diffs)
			if got.TotalApps != tt.want.TotalApps {
				t.Errorf("TotalApps = %d, want %d", got.TotalApps, tt.want.TotalApps)
			}
			if got.AppsWithChanges != tt.want.AppsWithChanges {
				t.Errorf("AppsWithChanges = %d, want %d", got.AppsWithChanges, tt.want.AppsWithChanges)
			}
			if got.AppsWithErrors != tt.want.AppsWithErrors {
				t.Errorf("AppsWithErrors = %d, want %d", got.AppsWithErrors, tt.want.AppsWithErrors)
			}
			if got.TotalAdded != tt.want.TotalAdded {
				t.Errorf("TotalAdded = %d, want %d", got.TotalAdded, tt.want.TotalAdded)
			}
			if got.TotalRemoved != tt.want.TotalRemoved {
				t.Errorf("TotalRemoved = %d, want %d", got.TotalRemoved, tt.want.TotalRemoved)
			}
			if got.TotalModified != tt.want.TotalModified {
				t.Errorf("TotalModified = %d, want %d", got.TotalModified, tt.want.TotalModified)
			}
			if got.NewApplications != tt.want.NewApplications {
				t.Errorf("NewApplications = %d, want %d", got.NewApplications, tt.want.NewApplications)
			}
		})
	}
}
