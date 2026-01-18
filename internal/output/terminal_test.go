package output

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rgeraskin/argocdf/internal/diff"
	"github.com/rgeraskin/argocdf/internal/types"
)

func TestNewTerminalWriter(t *testing.T) {
	tests := []struct {
		name        string
		format      string
		summaryOnly bool
		unifiedDiff bool
	}{
		{
			name:        "fields format",
			format:      "fields",
			summaryOnly: false,
			unifiedDiff: false,
		},
		{
			name:        "summary format",
			format:      "summary",
			summaryOnly: true,
			unifiedDiff: false,
		},
		{
			name:        "unified format",
			format:      "unified",
			summaryOnly: false,
			unifiedDiff: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := NewTerminalWriter(tt.format, 3)
			if w.summaryOnly != tt.summaryOnly {
				t.Errorf("NewTerminalWriter(%q).summaryOnly = %v, want %v", tt.format, w.summaryOnly, tt.summaryOnly)
			}
			if w.unifiedDiff != tt.unifiedDiff {
				t.Errorf("NewTerminalWriter(%q).unifiedDiff = %v, want %v", tt.format, w.unifiedDiff, tt.unifiedDiff)
			}
		})
	}
}

func TestTerminalWriterWriteHeader(t *testing.T) {
	var buf bytes.Buffer
	w := NewTerminalWriter("fields", 3)
	w.out = &buf

	err := w.WriteHeader("Test Header")
	if err != nil {
		t.Errorf("WriteHeader() error = %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Test Header") {
		t.Errorf("WriteHeader() output should contain title, got: %s", output)
	}
}

func TestTerminalWriterWriteAppDiff(t *testing.T) {
	tests := []struct {
		name       string
		format     string
		appDiff    *types.AppDiff
		wantOutput []string // strings that should be in output
		notOutput  []string // strings that should NOT be in output
	}{
		{
			name:   "app with error",
			format: "fields",
			appDiff: &types.AppDiff{
				Name:      "test-app",
				Namespace: "argocd",
				Error:     &testError{"render failed"},
			},
			wantOutput: []string{"test-app", "argocd", "Error"},
		},
		{
			name:   "app with no changes",
			format: "fields",
			appDiff: &types.AppDiff{
				Name:      "test-app",
				Namespace: "argocd",
				DiffResult: &diff.ManifestSetDiff{
					HasChanges: false,
				},
			},
			wantOutput: []string{"test-app", "No changes"},
		},
		{
			name:   "app with changes in fields format",
			format: "fields",
			appDiff: &types.AppDiff{
				Name:      "test-app",
				Namespace: "argocd",
				DiffResult: &diff.ManifestSetDiff{
					HasChanges: true,
					Added: []diff.Manifest{
						{Kind: "ConfigMap", Namespace: "default", Name: "newcm"},
					},
					Removed: []diff.Manifest{
						{Kind: "Secret", Namespace: "default", Name: "oldsec"},
					},
					Modified: []diff.ManifestDiff{
						{Key: "default/Deployment/app"},
					},
				},
			},
			wantOutput: []string{"test-app", "1 added", "1 removed", "1 modified"},
		},
		{
			name:   "app in summary mode shows only counts",
			format: "summary",
			appDiff: &types.AppDiff{
				Name:      "test-app",
				Namespace: "argocd",
				DiffResult: &diff.ManifestSetDiff{
					HasChanges: true,
					Added: []diff.Manifest{
						{Kind: "ConfigMap", Namespace: "default", Name: "cm1"},
					},
					Modified: []diff.ManifestDiff{
						{
							Key: "default/Deployment/app",
							Diff: &diff.DiffResult{
								Changes: []diff.FieldChange{
									{Type: diff.ChangeTypeModified, Path: ".spec.replicas"},
								},
							},
						},
					},
				},
			},
			wantOutput: []string{"test-app", "1 added", "1 modified"},
			notOutput:  []string{".spec.replicas"}, // field details should not appear in summary
		},
		{
			name:   "app in unified mode shows unified diff",
			format: "unified",
			appDiff: &types.AppDiff{
				Name:      "test-app",
				Namespace: "argocd",
				DiffResult: &diff.ManifestSetDiff{
					HasChanges: true,
					Modified: []diff.ManifestDiff{
						{
							Key: "default/Deployment/app",
							Old: &diff.Manifest{Raw: "replicas: 1\n"},
							New: &diff.Manifest{Raw: "replicas: 3\n"},
						},
					},
				},
			},
			wantOutput: []string{"test-app", "---", "+++", "-replicas: 1", "+replicas: 3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := NewTerminalWriter(tt.format, 3)
			w.out = &buf

			err := w.WriteAppDiff(tt.appDiff, 0)
			if err != nil {
				t.Errorf("WriteAppDiff() error = %v", err)
				return
			}

			output := buf.String()
			for _, want := range tt.wantOutput {
				if !strings.Contains(output, want) {
					t.Errorf("WriteAppDiff() output should contain %q, got:\n%s", want, output)
				}
			}
			for _, notWant := range tt.notOutput {
				if strings.Contains(output, notWant) {
					t.Errorf("WriteAppDiff() output should NOT contain %q, got:\n%s", notWant, output)
				}
			}
		})
	}
}

func TestTerminalWriterWriteSummary(t *testing.T) {
	var buf bytes.Buffer
	w := NewTerminalWriter("fields", 3)
	w.out = &buf

	summary := Summary{
		TotalApps:       10,
		AppsWithChanges: 5,
		AppsWithErrors:  1,
		TotalAdded:      3,
		TotalRemoved:    2,
		TotalModified:   4,
		NewApplications: 1,
	}

	err := w.WriteSummary(summary)
	if err != nil {
		t.Errorf("WriteSummary() error = %v", err)
		return
	}

	output := buf.String()
	checks := []string{"Summary", "10", "5", "1", "3", "2", "4"}
	for _, check := range checks {
		if !strings.Contains(output, check) {
			t.Errorf("WriteSummary() should contain %q, got:\n%s", check, output)
		}
	}
}

func TestTerminalWriterFlush(t *testing.T) {
	w := NewTerminalWriter("fields", 3)
	if err := w.Flush(); err != nil {
		t.Errorf("Flush() error = %v", err)
	}
}

// testError is a simple error implementation for testing
type testError struct {
	msg string
}

func (e *testError) Error() string {
	return e.msg
}
