// Package output provides terminal output functionality.
package output

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/rgeraskin/argocdf/internal/diff"
	"github.com/rgeraskin/argocdf/internal/types"
)

// Colors and styles for terminal output.
var (
	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("39")).
			MarginBottom(1)

	appNameStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("212"))

	addedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("42"))

	removedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196"))

	modifiedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("214"))

	errorStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("196"))

	dimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240"))

	summaryStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("39")).
			MarginTop(1)
)

// TerminalWriter writes diff output to the terminal with colors.
type TerminalWriter struct {
	out         io.Writer
	verbose     bool
	contextLine int
}

// NewTerminalWriter creates a new TerminalWriter.
func NewTerminalWriter(verbose bool) *TerminalWriter {
	return &TerminalWriter{
		out:         os.Stdout,
		verbose:     verbose,
		contextLine: 3,
	}
}

// WriteHeader writes the header.
func (t *TerminalWriter) WriteHeader(title string) error {
	fmt.Fprintln(t.out, headerStyle.Render(title))
	fmt.Fprintln(t.out, strings.Repeat("=", len(title)))
	fmt.Fprintln(t.out)
	return nil
}

// WriteAppDiff writes the diff for an application.
func (t *TerminalWriter) WriteAppDiff(appDiff *types.AppDiff, depth int) error {
	indent := strings.Repeat("  ", depth)

	// Write app name with tree indicator
	prefix := "├─"
	if depth == 0 {
		prefix = "●"
	}

	appLine := fmt.Sprintf("%s%s %s", indent, prefix, appNameStyle.Render(appDiff.Name))
	if appDiff.Namespace != "" {
		appLine += dimStyle.Render(fmt.Sprintf(" (%s)", appDiff.Namespace))
	}
	fmt.Fprintln(t.out, appLine)

	// Handle error
	if appDiff.Error != nil {
		fmt.Fprintf(t.out, "%s  %s\n", indent, errorStyle.Render("Error: "+appDiff.Error.Error()))
		fmt.Fprintln(t.out)
		return nil
	}

	// Type assert DiffResult
	result, ok := appDiff.DiffResult.(*diff.ManifestSetDiff)
	if !ok || result == nil {
		fmt.Fprintf(t.out, "%s  %s\n", indent, dimStyle.Render("No diff available"))
		fmt.Fprintln(t.out)
		return nil
	}

	// No changes
	if !result.HasChanges {
		fmt.Fprintf(t.out, "%s  %s\n", indent, dimStyle.Render("No changes"))
		fmt.Fprintln(t.out)
		return nil
	}

	// Write summary counts
	if len(result.Added) > 0 {
		fmt.Fprintf(t.out, "%s  %s\n", indent, addedStyle.Render(fmt.Sprintf("+ %d added", len(result.Added))))
	}
	if len(result.Removed) > 0 {
		fmt.Fprintf(t.out, "%s  %s\n", indent, removedStyle.Render(fmt.Sprintf("- %d removed", len(result.Removed))))
	}
	if len(result.Modified) > 0 {
		fmt.Fprintf(t.out, "%s  %s\n", indent, modifiedStyle.Render(fmt.Sprintf("~ %d modified", len(result.Modified))))
	}

	if t.verbose {
		t.writeDetailedDiff(result, indent)
	}

	fmt.Fprintln(t.out)
	return nil
}

// writeDetailedDiff writes the detailed diff for verbose mode.
func (t *TerminalWriter) writeDetailedDiff(result *diff.ManifestSetDiff, indent string) {
	// Added manifests
	for _, m := range result.Added {
		fmt.Fprintf(t.out, "%s    %s\n", indent, addedStyle.Render("+ "+m.Key()))
	}

	// Removed manifests
	for _, m := range result.Removed {
		fmt.Fprintf(t.out, "%s    %s\n", indent, removedStyle.Render("- "+m.Key()))
	}

	// Modified manifests with field-level changes
	for _, md := range result.Modified {
		fmt.Fprintf(t.out, "%s    %s\n", indent, modifiedStyle.Render("~ "+md.Key))
		if md.Diff != nil {
			t.writeFieldChanges(md.Diff, indent+"      ")
		}
	}
}

// writeFieldChanges writes field-level changes with coloring.
func (t *TerminalWriter) writeFieldChanges(result *diff.DiffResult, indent string) {
	for _, change := range result.Changes {
		switch change.Type {
		case diff.ChangeTypeAdded:
			fmt.Fprintf(t.out, "%s%s\n", indent, addedStyle.Render(fmt.Sprintf("+ %s: %v", change.Path, change.NewValue)))
		case diff.ChangeTypeRemoved:
			fmt.Fprintf(t.out, "%s%s\n", indent, removedStyle.Render(fmt.Sprintf("- %s: %v", change.Path, change.OldValue)))
		case diff.ChangeTypeModified:
			fmt.Fprintf(t.out, "%s%s\n", indent, modifiedStyle.Render(fmt.Sprintf("~ %s:", change.Path)))
			fmt.Fprintf(t.out, "%s  %s\n", indent, removedStyle.Render(fmt.Sprintf("- %v", change.OldValue)))
			fmt.Fprintf(t.out, "%s  %s\n", indent, addedStyle.Render(fmt.Sprintf("+ %v", change.NewValue)))
		}
	}
}

// WriteTree writes the full application tree.
func (t *TerminalWriter) WriteTree(tree *diff.AppTree) error {
	tree.Walk(func(node *diff.AppTreeNode, depth int) {
		if appDiff, ok := node.AppDiff.(*types.AppDiff); ok {
			t.WriteAppDiff(appDiff, depth)
		}
	})
	return nil
}

// WriteSummary writes the summary.
func (t *TerminalWriter) WriteSummary(summary Summary) error {
	fmt.Fprintln(t.out)
	fmt.Fprintln(t.out, summaryStyle.Render("Summary"))
	fmt.Fprintln(t.out, strings.Repeat("-", 40))

	fmt.Fprintf(t.out, "Applications analyzed: %d\n", summary.TotalApps)

	if summary.AppsWithChanges > 0 {
		fmt.Fprintf(t.out, "Applications with changes: %s\n",
			modifiedStyle.Render(fmt.Sprintf("%d", summary.AppsWithChanges)))
	} else {
		fmt.Fprintln(t.out, "Applications with changes: 0")
	}

	if summary.AppsWithErrors > 0 {
		fmt.Fprintf(t.out, "Applications with errors: %s\n",
			errorStyle.Render(fmt.Sprintf("%d", summary.AppsWithErrors)))
	}

	if summary.TotalAdded > 0 || summary.TotalRemoved > 0 || summary.TotalModified > 0 {
		fmt.Fprintln(t.out)
		fmt.Fprintln(t.out, "Resource changes:")
		if summary.TotalAdded > 0 {
			fmt.Fprintf(t.out, "  %s\n", addedStyle.Render(fmt.Sprintf("+%d added", summary.TotalAdded)))
		}
		if summary.TotalRemoved > 0 {
			fmt.Fprintf(t.out, "  %s\n", removedStyle.Render(fmt.Sprintf("-%d removed", summary.TotalRemoved)))
		}
		if summary.TotalModified > 0 {
			fmt.Fprintf(t.out, "  %s\n", modifiedStyle.Render(fmt.Sprintf("~%d modified", summary.TotalModified)))
		}
	}

	if summary.NewApplications > 0 {
		fmt.Fprintf(t.out, "\nNew Application CRDs discovered: %s\n",
			addedStyle.Render(fmt.Sprintf("%d", summary.NewApplications)))
	}

	return nil
}

// WriteFooter writes the footer.
func (t *TerminalWriter) WriteFooter() error {
	fmt.Fprintln(t.out)
	return nil
}

// Flush flushes the output.
func (t *TerminalWriter) Flush() error {
	return nil
}
