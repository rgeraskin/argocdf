// Package output provides tests for the HTML writer.
package output

import (
	"os"
	"strings"
	"testing"

	"github.com/rgeraskin/argocdf/internal/diff"
	"github.com/rgeraskin/argocdf/internal/types"
)

func TestNewHTMLWriter(t *testing.T) {
	// Create a temp file
	tmpFile, err := os.CreateTemp("", "test-*.html")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	tests := []struct {
		name        string
		sideBySide  bool
		summaryOnly bool
	}{
		{
			name:        "side-by-side mode",
			sideBySide:  true,
			summaryOnly: false,
		},
		{
			name:        "summary only mode",
			sideBySide:  false,
			summaryOnly: true,
		},
		{
			name:        "default mode",
			sideBySide:  false,
			summaryOnly: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, err := NewHTMLWriter(tmpFile.Name(), tt.sideBySide, tt.summaryOnly, false)
			if err != nil {
				t.Fatalf("NewHTMLWriter() error = %v", err)
			}
			defer w.Flush()

			if w.sideBySide != tt.sideBySide {
				t.Errorf("sideBySide = %v, want %v", w.sideBySide, tt.sideBySide)
			}
			if w.summaryOnly != tt.summaryOnly {
				t.Errorf("summaryOnly = %v, want %v", w.summaryOnly, tt.summaryOnly)
			}
		})
	}
}

func TestHTMLWriterWriteHeader(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "test-*.html")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	w, err := NewHTMLWriter(tmpFile.Name(), false, false, false)
	if err != nil {
		t.Fatalf("NewHTMLWriter() error = %v", err)
	}

	title := "Test Diff Report"
	if err := w.WriteHeader(title); err != nil {
		t.Fatalf("WriteHeader() error = %v", err)
	}
	w.Flush()

	// Read and verify content
	content, err := os.ReadFile(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}

	html := string(content)
	if !strings.Contains(html, "<!DOCTYPE html>") {
		t.Error("Expected HTML doctype")
	}
	if !strings.Contains(html, "<title>Test Diff Report</title>") {
		t.Error("Expected title in HTML")
	}
	if !strings.Contains(html, "<h1>Test Diff Report</h1>") {
		t.Error("Expected h1 heading in HTML")
	}
}

func TestHTMLWriterWriteAppDiff(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "test-*.html")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	w, err := NewHTMLWriter(tmpFile.Name(), false, false, false)
	if err != nil {
		t.Fatalf("NewHTMLWriter() error = %v", err)
	}

	// Write header first
	w.WriteHeader("Test")

	// Test app with changes
	appDiff := &types.AppDiff{
		Name:      "test-app",
		Namespace: "test-ns",
		DiffResult: &diff.ManifestSetDiff{
			HasChanges: true,
			Added: []diff.Manifest{
				{Kind: "ConfigMap", Name: "new-config"},
			},
			Removed: []diff.Manifest{
				{Kind: "Secret", Name: "old-secret"},
			},
			Modified: []diff.ManifestDiff{
				{Key: "Deployment/app"},
			},
		},
	}

	if err := w.WriteAppDiff(appDiff, 0); err != nil {
		t.Fatalf("WriteAppDiff() error = %v", err)
	}
	w.Flush()

	// Read and verify content
	content, err := os.ReadFile(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}

	html := string(content)
	if !strings.Contains(html, "test-app") {
		t.Error("Expected app name in HTML")
	}
	if !strings.Contains(html, "test-ns") {
		t.Error("Expected namespace in HTML")
	}
	if !strings.Contains(html, "badge-added") {
		t.Error("Expected added badge in HTML")
	}
	if !strings.Contains(html, "badge-removed") {
		t.Error("Expected removed badge in HTML")
	}
	if !strings.Contains(html, "badge-modified") {
		t.Error("Expected modified badge in HTML")
	}
}

func TestHTMLWriterWriteAppDiffWithError(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "test-*.html")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	w, err := NewHTMLWriter(tmpFile.Name(), false, false, false)
	if err != nil {
		t.Fatalf("NewHTMLWriter() error = %v", err)
	}

	w.WriteHeader("Test")

	// Test app with error
	appDiff := &types.AppDiff{
		Name:      "error-app",
		Namespace: "test-ns",
		Error:     &testError{msg: "render failed"},
	}

	if err := w.WriteAppDiff(appDiff, 0); err != nil {
		t.Fatalf("WriteAppDiff() error = %v", err)
	}
	w.Flush()

	content, err := os.ReadFile(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}

	html := string(content)
	if !strings.Contains(html, "badge-error") {
		t.Error("Expected error badge in HTML")
	}
	if !strings.Contains(html, "render failed") {
		t.Error("Expected error message in HTML")
	}
}

func TestHTMLWriterWriteSummary(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "test-*.html")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	w, err := NewHTMLWriter(tmpFile.Name(), false, false, false)
	if err != nil {
		t.Fatalf("NewHTMLWriter() error = %v", err)
	}

	w.WriteHeader("Test")

	summary := Summary{
		TotalApps:       5,
		AppsWithChanges: 3,
		TotalAdded:      10,
		TotalRemoved:    2,
		TotalModified:   5,
		AppsWithErrors:  1,
	}

	if err := w.WriteSummary(summary); err != nil {
		t.Fatalf("WriteSummary() error = %v", err)
	}
	w.Flush()

	content, err := os.ReadFile(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}

	html := string(content)
	if !strings.Contains(html, "Summary") {
		t.Error("Expected Summary heading in HTML")
	}
	if !strings.Contains(html, "5") { // TotalApps
		t.Error("Expected total apps count in HTML")
	}
}

func TestHTMLWriterWriteFooter(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "test-*.html")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	w, err := NewHTMLWriter(tmpFile.Name(), false, false, false)
	if err != nil {
		t.Fatalf("NewHTMLWriter() error = %v", err)
	}

	w.WriteHeader("Test")
	if err := w.WriteFooter(); err != nil {
		t.Fatalf("WriteFooter() error = %v", err)
	}
	w.Flush()

	content, err := os.ReadFile(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}

	html := string(content)
	if !strings.Contains(html, "argocdf") {
		t.Error("Expected argocdf in footer")
	}
	if !strings.Contains(html, "</body>") {
		t.Error("Expected closing body tag")
	}
	if !strings.Contains(html, "</html>") {
		t.Error("Expected closing html tag")
	}
}

func TestSplitLines(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []string
	}{
		{
			name:    "empty content",
			content: "",
			want:    []string{},
		},
		{
			name:    "single line",
			content: "hello",
			want:    []string{"hello"},
		},
		{
			name:    "multiple lines",
			content: "line1\nline2\nline3",
			want:    []string{"line1", "line2", "line3"},
		},
		{
			name:    "trailing newline",
			content: "line1\nline2\n",
			want:    []string{"line1", "line2"},
		},
		{
			name:    "only newline",
			content: "\n",
			want:    []string{""},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitLines(tt.content)
			if len(got) != len(tt.want) {
				t.Errorf("splitLines() returned %d lines, want %d", len(got), len(tt.want))
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("splitLines()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// Note: testError type is defined in terminal_test.go in this package
