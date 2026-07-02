// Package output provides tests for markdown output functionality.
package output

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rgeraskin/argocdf/internal/diff"
	"github.com/rgeraskin/argocdf/internal/types"
)

func TestNewMarkdownWriter(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "markdown-test-")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(tempDir)
	}()

	t.Run("creates file successfully", func(t *testing.T) {
		filePath := filepath.Join(tempDir, "test.md")
		w, err := NewMarkdownWriter(filePath, MarkdownFormatGitHub, 3)
		if err != nil {
			t.Fatalf("NewMarkdownWriter() error = %v", err)
		}
		defer func() {
			_ = w.Flush()
		}()

		// File should exist
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			t.Error("file was not created")
		}
	})

	t.Run("returns error for invalid path", func(t *testing.T) {
		_, err := NewMarkdownWriter("/nonexistent/path/test.md", MarkdownFormatGitHub, 3)
		if err == nil {
			t.Error("NewMarkdownWriter() should return error for invalid path")
		}
	})
}

func TestMarkdownWriter_WriteHeader(t *testing.T) {
	tests := []struct {
		name     string
		format   MarkdownFormat
		title    string
		contains string
	}{
		{
			name:     "GitHub format",
			format:   MarkdownFormatGitHub,
			title:    "Test Title",
			contains: "## Test Title",
		},
		{
			name:     "Atlantis format",
			format:   MarkdownFormatAtlantis,
			title:    "ArgoCD Diff",
			contains: "## ArgoCD Diff",
		},
		{
			name:     "HTML escaping",
			format:   MarkdownFormatGitHub,
			title:    "<script>alert('xss')</script>",
			contains: "&lt;script&gt;",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempDir, _ := os.MkdirTemp("", "markdown-test-")
			defer func() {
		_ = os.RemoveAll(tempDir)
	}()

			filePath := filepath.Join(tempDir, "test.md")
			w, _ := NewMarkdownWriter(filePath, tt.format, 3)

			if err := w.WriteHeader(tt.title); err != nil {
				t.Errorf("WriteHeader() error = %v", err)
			}
			if err := w.Flush(); err != nil {
				t.Errorf("Flush() error = %v", err)
			}

			content, _ := os.ReadFile(filePath)
			if !strings.Contains(string(content), tt.contains) {
				t.Errorf("WriteHeader() output missing %q, got: %s", tt.contains, content)
			}
		})
	}
}

func TestMarkdownWriter_WriteAppDiff_GitHub(t *testing.T) {
	tests := []struct {
		name     string
		appDiff  *types.AppDiff
		contains []string
	}{
		{
			name: "app with error",
			appDiff: &types.AppDiff{
				Name:      "test-app",
				Namespace: "test-ns",
				Error:     errors.New("render failed"),
			},
			contains: []string{
				"<details>",
				"<summary><b>test-app (test-ns)</b>",
				"❌ Error",
				"render failed",
				"</details>",
			},
		},
		{
			name: "app with no changes",
			appDiff: &types.AppDiff{
				Name: "test-app",
				DiffResult: &diff.ManifestSetDiff{
					HasChanges: false,
				},
			},
			contains: []string{
				"<details>",
				"test-app",
				"_No changes_",
			},
		},
		{
			name: "app with changes",
			appDiff: &types.AppDiff{
				Name: "test-app",
				DiffResult: &diff.ManifestSetDiff{
					HasChanges: true,
					Added: []diff.Manifest{
						{Kind: "ConfigMap", Name: "cm1", Raw: "apiVersion: v1\nkind: ConfigMap"},
					},
					Removed: []diff.Manifest{
						{Kind: "Secret", Name: "s1", Raw: "apiVersion: v1\nkind: Secret"},
					},
					Modified: []diff.ManifestDiff{
						{
							Key: "Deployment/test",
							Diff: &diff.DiffResult{
								Changes: []diff.FieldChange{
									{Type: diff.ChangeTypeModified, Path: "spec.replicas", OldValue: 1, NewValue: 3},
								},
							},
						},
					},
				},
			},
			contains: []string{
				"🟢+1", // Added badge
				"🔴-1", // Removed badge
				"🟡~1", // Modified badge
				"#### ➕",
				"#### ➖",
				"#### 📝",
				"```yaml",
				"```diff",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempDir, _ := os.MkdirTemp("", "markdown-test-")
			defer func() {
		_ = os.RemoveAll(tempDir)
	}()

			filePath := filepath.Join(tempDir, "test.md")
			w, _ := NewMarkdownWriter(filePath, MarkdownFormatGitHub, 3)

			if err := w.WriteAppDiff(tt.appDiff, 0); err != nil {
				t.Errorf("WriteAppDiff() error = %v", err)
			}
			_ = w.Flush()

			content, _ := os.ReadFile(filePath)
			for _, expected := range tt.contains {
				if !strings.Contains(string(content), expected) {
					t.Errorf("WriteAppDiff() output missing %q, got: %s", expected, content)
				}
			}
		})
	}
}

func TestMarkdownWriter_WriteAppDiff_Atlantis(t *testing.T) {
	tempDir, _ := os.MkdirTemp("", "markdown-test-")
	defer func() {
		_ = os.RemoveAll(tempDir)
	}()

	filePath := filepath.Join(tempDir, "test.md")
	w, _ := NewMarkdownWriter(filePath, MarkdownFormatAtlantis, 3)

	appDiff := &types.AppDiff{
		Name:      "test-app",
		Namespace: "test-ns",
		DiffResult: &diff.ManifestSetDiff{
			HasChanges: true,
			Modified: []diff.ManifestDiff{
				{Key: "Deployment/test"},
			},
		},
	}

	if err := w.WriteAppDiff(appDiff, 0); err != nil {
		t.Errorf("WriteAppDiff() error = %v", err)
	}
	_ = w.Flush()

	content, _ := os.ReadFile(filePath)

	// Atlantis format should have "Show diff for" text
	if !strings.Contains(string(content), "Show diff for") {
		t.Errorf("Atlantis format should contain 'Show diff for', got: %s", content)
	}
}

func TestMarkdownWriter_WriteSummary(t *testing.T) {
	tests := []struct {
		name     string
		format   MarkdownFormat
		summary  Summary
		contains []string
	}{
		{
			name:   "GitHub format with changes",
			format: MarkdownFormatGitHub,
			summary: Summary{
				TotalApps:       5,
				AppsWithChanges: 3,
				TotalAdded:      10,
				TotalRemoved:    2,
				TotalModified:   5,
			},
			contains: []string{
				"**Summary:**",
				"5 applications affected",
				"3 changed",
				"+10/-2/~5 resources",
			},
		},
		{
			name:   "GitHub format with errors",
			format: MarkdownFormatGitHub,
			summary: Summary{
				TotalApps:      3,
				AppsWithErrors: 2,
			},
			contains: []string{
				"2 errors",
			},
		},
		{
			name:   "Atlantis format",
			format: MarkdownFormatAtlantis,
			summary: Summary{
				TotalApps:       2,
				AppsWithChanges: 1,
			},
			contains: []string{
				"**Summary:**",
				"2 applications affected",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempDir, _ := os.MkdirTemp("", "markdown-test-")
			defer func() {
		_ = os.RemoveAll(tempDir)
	}()

			filePath := filepath.Join(tempDir, "test.md")
			w, _ := NewMarkdownWriter(filePath, tt.format, 3)

			if err := w.WriteSummary(tt.summary); err != nil {
				t.Errorf("WriteSummary() error = %v", err)
			}
			_ = w.Flush()

			content, _ := os.ReadFile(filePath)
			for _, expected := range tt.contains {
				if !strings.Contains(string(content), expected) {
					t.Errorf("WriteSummary() output missing %q, got: %s", expected, content)
				}
			}
		})
	}
}

func TestMarkdownWriter_WriteFooter(t *testing.T) {
	tempDir, _ := os.MkdirTemp("", "markdown-test-")
	defer func() {
		_ = os.RemoveAll(tempDir)
	}()

	filePath := filepath.Join(tempDir, "test.md")
	w, _ := NewMarkdownWriter(filePath, MarkdownFormatGitHub, 3)

	if err := w.WriteFooter(); err != nil {
		t.Errorf("WriteFooter() error = %v", err)
	}
	_ = w.Flush()

	content, _ := os.ReadFile(filePath)
	if !strings.Contains(string(content), "Generated at") {
		t.Errorf("WriteFooter() missing timestamp, got: %s", content)
	}
	if !strings.Contains(string(content), "argocdf") {
		t.Errorf("WriteFooter() missing argocdf attribution, got: %s", content)
	}
}

func TestMarkdownWriter_WriteTree(t *testing.T) {
	tempDir, _ := os.MkdirTemp("", "markdown-test-")
	defer func() {
		_ = os.RemoveAll(tempDir)
	}()

	filePath := filepath.Join(tempDir, "test.md")
	w, _ := NewMarkdownWriter(filePath, MarkdownFormatGitHub, 3)

	// Create a tree with parent and child apps
	diffs := []*types.AppDiff{
		{
			Name:       "parent-app",
			Namespace:  "argocd",
			DiffResult: &diff.ManifestSetDiff{HasChanges: false},
		},
		{
			Name:          "child-app",
			Namespace:     "default",
			ParentAppName: "parent-app",
			DiffResult:    &diff.ManifestSetDiff{HasChanges: false},
		},
	}

	tree := diff.NewAppTree(diffs)
	if err := w.WriteTree(tree); err != nil {
		t.Errorf("WriteTree() error = %v", err)
	}
	_ = w.Flush()

	content, _ := os.ReadFile(filePath)
	if !strings.Contains(string(content), "parent-app") {
		t.Errorf("WriteTree() missing parent-app, got: %s", content)
	}
	if !strings.Contains(string(content), "child-app") {
		t.Errorf("WriteTree() missing child-app, got: %s", content)
	}
}

func TestMarkdownWriter_HTMLEscaping(t *testing.T) {
	tempDir, _ := os.MkdirTemp("", "markdown-test-")
	defer func() {
		_ = os.RemoveAll(tempDir)
	}()

	filePath := filepath.Join(tempDir, "test.md")
	w, _ := NewMarkdownWriter(filePath, MarkdownFormatGitHub, 3)

	// App name with special characters
	appDiff := &types.AppDiff{
		Name:  "<script>alert('xss')</script>",
		Error: errors.New("<malicious> error & message"),
	}

	_ = w.WriteAppDiff(appDiff, 0)
	_ = w.Flush()

	content, _ := os.ReadFile(filePath)
	contentStr := string(content)

	// Should not contain raw HTML tags
	if strings.Contains(contentStr, "<script>") {
		t.Error("WriteAppDiff() should escape <script> tags")
	}
	if strings.Contains(contentStr, "<malicious>") {
		t.Error("WriteAppDiff() should escape error message HTML")
	}

	// Should contain escaped versions
	if !strings.Contains(contentStr, "&lt;script&gt;") {
		t.Error("WriteAppDiff() should contain escaped script tag")
	}
}

func TestMarkdownFormat_Values(t *testing.T) {
	// Verify format constants
	if MarkdownFormatGitHub != "github" {
		t.Errorf("MarkdownFormatGitHub = %q, want %q", MarkdownFormatGitHub, "github")
	}
	if MarkdownFormatAtlantis != "atlantis" {
		t.Errorf("MarkdownFormatAtlantis = %q, want %q", MarkdownFormatAtlantis, "atlantis")
	}
}
