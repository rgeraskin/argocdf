// Package output provides tests for the HTML writer.
package output

import (
	"os"
	"strings"
	"testing"

	"github.com/rgeraskin/argocdf/internal/diff"
)

func TestNewHTMLWriter(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "test-*.html")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	_ = tmpFile.Close()
	defer func() {
		_ = os.Remove(tmpFile.Name())
	}()

	w, err := NewHTMLWriter(tmpFile.Name())
	if err != nil {
		t.Fatalf("NewHTMLWriter() error = %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Errorf("Flush() error = %v", err)
	}
}

func TestHTMLWriterWriteHeader(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "test-*.html")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	_ = tmpFile.Close()
	defer func() {
		_ = os.Remove(tmpFile.Name())
	}()

	w, err := NewHTMLWriter(tmpFile.Name())
	if err != nil {
		t.Fatalf("NewHTMLWriter() error = %v", err)
	}

	title := "Test Diff Report"
	if err := w.WriteHeader(title); err != nil {
		t.Fatalf("WriteHeader() error = %v", err)
	}
	_ = w.Flush()

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
	_ = tmpFile.Close()
	defer func() {
		_ = os.Remove(tmpFile.Name())
	}()

	w, err := NewHTMLWriter(tmpFile.Name())
	if err != nil {
		t.Fatalf("NewHTMLWriter() error = %v", err)
	}

	// Write header first
	_ = w.WriteHeader("Test")

	// Test app with changes
	appDiff := &diff.AppDiff{
		Name:      "test-app",
		Namespace: "test-ns",
		Diff: &diff.ManifestSetDiff{
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
	_ = w.Flush()

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
	_ = tmpFile.Close()
	defer func() {
		_ = os.Remove(tmpFile.Name())
	}()

	w, err := NewHTMLWriter(tmpFile.Name())
	if err != nil {
		t.Fatalf("NewHTMLWriter() error = %v", err)
	}

	_ = w.WriteHeader("Test")

	// Test app with error
	appDiff := &diff.AppDiff{
		Name:      "error-app",
		Namespace: "test-ns",
		Error:     &testError{msg: "render failed"},
	}

	if err := w.WriteAppDiff(appDiff, 0); err != nil {
		t.Fatalf("WriteAppDiff() error = %v", err)
	}
	_ = w.Flush()

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
	_ = tmpFile.Close()
	defer func() {
		_ = os.Remove(tmpFile.Name())
	}()

	w, err := NewHTMLWriter(tmpFile.Name())
	if err != nil {
		t.Fatalf("NewHTMLWriter() error = %v", err)
	}

	_ = w.WriteHeader("Test")

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
	_ = w.Flush()

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
	_ = tmpFile.Close()
	defer func() {
		_ = os.Remove(tmpFile.Name())
	}()

	w, err := NewHTMLWriter(tmpFile.Name())
	if err != nil {
		t.Fatalf("NewHTMLWriter() error = %v", err)
	}

	_ = w.WriteHeader("Test")
	if err := w.WriteFooter(Provenance{}); err != nil {
		t.Fatalf("WriteFooter(Provenance) error = %v", err)
	}
	_ = w.Flush()

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
