// Package output provides unified diff file output functionality.
package output

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/rgeraskin/argocdf/internal/diff"
	"github.com/rgeraskin/argocdf/internal/types"
)

// UnifiedWriter writes diff output in unified diff format to a file.
// This format is compatible with patch(1) and can be used with diff viewers.
type UnifiedWriter struct {
	baseFileWriter
	contextLines int // Number of context lines for unified diff
}

// NewUnifiedWriter creates a new UnifiedWriter.
// contextLines specifies the number of unchanged lines to show around changes.
func NewUnifiedWriter(filePath string, contextLines int) (*UnifiedWriter, error) {
	base, err := newBaseFileWriter(filePath, "unified diff")
	if err != nil {
		return nil, err
	}

	return &UnifiedWriter{
		baseFileWriter: base,
		contextLines:   contextLines,
	}, nil
}

// WriteHeader writes the header comment.
func (u *UnifiedWriter) WriteHeader(title string) error {
	_, err := io.WriteString(u.file, fmt.Sprintf("# %s\n", title))
	return err
}

// WriteAppDiff writes the diff for an application in unified diff format.
func (u *UnifiedWriter) WriteAppDiff(appDiff *types.AppDiff, _ int) error {
	// Write app header as comment
	appName := appDiff.Name
	if appDiff.Namespace != "" {
		appName += fmt.Sprintf(" (%s)", appDiff.Namespace)
	}
	u.write(fmt.Sprintf("# Application: %s\n", appName))

	// Handle error
	if appDiff.Error != nil {
		u.write(fmt.Sprintf("# Error: %s\n\n", appDiff.Error.Error()))
		return nil
	}

	// Type assert DiffResult
	result, ok := appDiff.DiffResult.(*diff.ManifestSetDiff)
	if !ok || result == nil {
		u.write("# No diff available\n\n")
		return nil
	}

	// Show parse errors if present
	if len(result.ParseErrors) > 0 {
		u.write(fmt.Sprintf("# ⚠ %d YAML parse error(s):\n", len(result.ParseErrors)))
		for _, err := range result.ParseErrors {
			u.write(fmt.Sprintf("#   • %s\n", err))
		}
	}

	// Show parse warnings if present (non-fatal; documents are still diffed)
	if len(result.ParseWarnings) > 0 {
		u.write(fmt.Sprintf("# ⚠ %d warning(s):\n", len(result.ParseWarnings)))
		for _, warn := range result.ParseWarnings {
			u.write(fmt.Sprintf("#   • %s\n", warn))
		}
	}

	// No changes
	if !result.HasChanges {
		// Don't show "No changes" if there were parse errors
		if len(result.ParseErrors) == 0 {
			u.write("# No changes\n\n")
		} else {
			u.write("\n") // Just add blank line after errors
		}
		return nil
	}

	// Generate unified diffs for all manifests
	diffs, err := GenerateManifestUnifiedDiffs(result, u.contextLines)
	if err != nil {
		u.write(fmt.Sprintf("# Error generating diff: %s\n\n", err.Error()))
		return nil
	}

	keys := GetSortedKeys(result)
	for _, key := range keys {
		if d, ok := diffs[key]; ok && d != "" {
			u.write(d)
			// Add newline between diffs if not already present
			if !strings.HasSuffix(d, "\n") {
				u.write("\n")
			}
		}
	}

	u.write("\n")
	return nil
}

// WriteTree writes the full application tree.
func (u *UnifiedWriter) WriteTree(tree *diff.AppTree) error {
	tree.Walk(func(node *diff.AppTreeNode, depth int) {
		if appDiff, ok := node.AppDiff.(*types.AppDiff); ok {
			_ = u.WriteAppDiff(appDiff, depth)
		}
	})
	return nil
}

// WriteSummary writes a summary comment.
func (u *UnifiedWriter) WriteSummary(summary Summary) error {
	u.write("# Summary\n")
	u.write(fmt.Sprintf("# Applications affected: %d\n", summary.TotalApps))
	u.write(fmt.Sprintf("# Applications changed: %d\n", summary.AppsWithChanges))

	if summary.TotalAdded > 0 || summary.TotalRemoved > 0 || summary.TotalModified > 0 {
		u.write(fmt.Sprintf("# Resources: +%d added, -%d removed, ~%d modified\n",
			summary.TotalAdded, summary.TotalRemoved, summary.TotalModified))
	}

	if summary.AppsWithErrors > 0 {
		u.write(fmt.Sprintf("# Errors: %d\n", summary.AppsWithErrors))
	}

	return nil
}

// WriteFooter writes the footer comment.
func (u *UnifiedWriter) WriteFooter() error {
	_, err := io.WriteString(u.file, fmt.Sprintf("# Generated at %s by argocdf\n", time.Now().Format(time.RFC3339)))
	return err
}

// Flush flushes and closes the file.
func (u *UnifiedWriter) Flush() error {
	return u.close()
}
