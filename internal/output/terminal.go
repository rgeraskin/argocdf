// Package output provides terminal output functionality.
package output

import (
	"fmt"
	"io"
	"os"
	"os/exec"
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
	out          io.Writer
	summaryOnly  bool
	unifiedDiff  bool   // Show unified diff format
	externalDiff string // External diff command from ARGOCDF_EXTERNAL_DIFF
	contextLines int    // Number of context lines for unified diff
}

// NewTerminalWriter creates a new TerminalWriter.
// Format can be "fields", "summary", or "unified".
// contextLines specifies the number of unchanged lines to show around changes in unified diff.
// If ARGOCDF_EXTERNAL_DIFF environment variable is set, it will be used
// for side-by-side diff display automatically (when format is "fields").
func NewTerminalWriter(format string, contextLines int) *TerminalWriter {
	return &TerminalWriter{
		out:          os.Stdout,
		summaryOnly:  format == "summary",
		unifiedDiff:  format == "unified",
		externalDiff: os.Getenv("ARGOCDF_EXTERNAL_DIFF"),
		contextLines: contextLines,
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
		return nil
	}

	// Type assert DiffResult
	result, ok := appDiff.DiffResult.(*diff.ManifestSetDiff)
	if !ok || result == nil {
		fmt.Fprintf(t.out, "%s  %s\n", indent, dimStyle.Render("No diff available"))
		return nil
	}

	// No changes
	if !result.HasChanges {
		fmt.Fprintf(t.out, "%s  %s\n", indent, dimStyle.Render("No changes"))
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

	// Show detailed diff unless summaryOnly is set
	if !t.summaryOnly {
		if t.unifiedDiff {
			// Use unified diff format
			t.writeUnifiedDiff(result, indent)
		} else if t.externalDiff != "" {
			// Use external diff tool for side-by-side view (ARGOCDF_EXTERNAL_DIFF is set)
			t.writeExternalDiff(appDiff, result, indent)
		} else {
			// Use built-in detailed diff
			t.writeDetailedDiff(result, indent)
		}
	}

	return nil
}

// writeDetailedDiff writes the detailed diff with field-level changes.
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

// writeUnifiedDiff writes unified diff format output.
func (t *TerminalWriter) writeUnifiedDiff(result *diff.ManifestSetDiff, indent string) {
	diffs, err := GenerateManifestUnifiedDiffs(result, t.contextLines)
	if err != nil {
		fmt.Fprintf(t.out, "%s    %s\n", indent, errorStyle.Render("Error generating diff: "+err.Error()))
		return
	}

	keys := GetSortedKeys(result)
	for _, key := range keys {
		if d, ok := diffs[key]; ok && d != "" {
			// Print each line of the unified diff with appropriate coloring
			lines := strings.Split(d, "\n")
			for _, line := range lines {
				if line == "" {
					continue
				}
				switch {
				case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---"):
					fmt.Fprintf(t.out, "%s    %s\n", indent, dimStyle.Render(line))
				case strings.HasPrefix(line, "@@"):
					fmt.Fprintf(t.out, "%s    %s\n", indent, dimStyle.Render(line))
				case strings.HasPrefix(line, "+"):
					fmt.Fprintf(t.out, "%s    %s\n", indent, addedStyle.Render(line))
				case strings.HasPrefix(line, "-"):
					fmt.Fprintf(t.out, "%s    %s\n", indent, removedStyle.Render(line))
				default:
					fmt.Fprintf(t.out, "%s    %s\n", indent, line)
				}
			}
		}
	}
}

// writeExternalDiff uses an external diff tool to display side-by-side diffs.
// Uses the ARGOCDF_EXTERNAL_DIFF environment variable.
func (t *TerminalWriter) writeExternalDiff(_ *types.AppDiff, result *diff.ManifestSetDiff, indent string) {
	// Parse the diff command (already validated to be non-empty)
	parts := strings.Fields(t.externalDiff)
	if len(parts) == 0 {
		t.writeDetailedDiff(result, indent)
		return
	}

	// Show added manifests with their names
	for _, m := range result.Added {
		fmt.Fprintf(t.out, "\n%s    %s\n", indent, addedStyle.Render("+ "+m.Key()))
		t.runExternalDiff(parts, "", m.Raw, indent)
	}

	// Show removed manifests with their names
	for _, m := range result.Removed {
		fmt.Fprintf(t.out, "\n%s    %s\n", indent, removedStyle.Render("- "+m.Key()))
		t.runExternalDiff(parts, m.Raw, "", indent)
	}

	// Show modified manifests with their names
	for _, md := range result.Modified {
		fmt.Fprintf(t.out, "\n%s    %s\n", indent, modifiedStyle.Render("~ "+md.Key))
		if md.Old != nil && md.New != nil {
			t.runExternalDiff(parts, md.Old.Raw, md.New.Raw, indent)
		}
	}
}

// runExternalDiff executes the external diff command for a single manifest.
func (t *TerminalWriter) runExternalDiff(cmdParts []string, oldContent, newContent, indent string) {
	// Create temp files
	oldFile, err := os.CreateTemp("", "argocdf-old-*.yaml")
	if err != nil {
		fmt.Fprintf(t.out, "%s      %s\n", indent, errorStyle.Render("Failed to create temp file: "+err.Error()))
		return
	}
	defer os.Remove(oldFile.Name())

	newFile, err := os.CreateTemp("", "argocdf-new-*.yaml")
	if err != nil {
		fmt.Fprintf(t.out, "%s      %s\n", indent, errorStyle.Render("Failed to create temp file: "+err.Error()))
		return
	}
	defer os.Remove(newFile.Name())

	// Write content
	oldFile.WriteString(oldContent)
	oldFile.Close()

	newFile.WriteString(newContent)
	newFile.Close()

	// Execute the external diff command
	args := append(cmdParts[1:], oldFile.Name(), newFile.Name())
	cmd := exec.Command(cmdParts[0], args...)
	cmd.Stdout = t.out
	cmd.Stderr = os.Stderr

	// Run the command (ignore exit code as diff returns non-zero when files differ)
	cmd.Run()
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
	fmt.Fprintln(t.out, summaryStyle.Render("Summary"))
	fmt.Fprintln(t.out, strings.Repeat("-", 40))

	fmt.Fprintf(t.out, "Applications affected: %d\n", summary.TotalApps)

	if summary.AppsWithChanges > 0 {
		fmt.Fprintf(t.out, "Applications changed: %s\n",
			modifiedStyle.Render(fmt.Sprintf("%d", summary.AppsWithChanges)))
	} else {
		fmt.Fprintln(t.out, "Applications changed: 0")
	}

	// Resources line (always show if there are any changes)
	if summary.TotalAdded > 0 || summary.TotalRemoved > 0 || summary.TotalModified > 0 {
		fmt.Fprintf(t.out, "Resources: %s, %s, %s\n",
			addedStyle.Render(fmt.Sprintf("+%d added", summary.TotalAdded)),
			removedStyle.Render(fmt.Sprintf("-%d removed", summary.TotalRemoved)),
			modifiedStyle.Render(fmt.Sprintf("~%d modified", summary.TotalModified)))
	}

	if summary.AppsWithErrors > 0 {
		fmt.Fprintf(t.out, "Errors: %s\n",
			errorStyle.Render(fmt.Sprintf("%d", summary.AppsWithErrors)))
	}

	if summary.NewApplications > 0 {
		fmt.Fprintf(t.out, "New Application CRDs discovered: %s\n",
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
