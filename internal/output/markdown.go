// Package output provides markdown output functionality.
package output

import (
	"fmt"
	"html"
	"io"
	"strings"
	"time"

	"github.com/rgeraskin/argocdf/internal/diff"
	"github.com/rgeraskin/argocdf/internal/types"
)

// MarkdownFormat represents the markdown output format style.
type MarkdownFormat string

const (
	// MarkdownFormatGitHub is the default GitHub-compatible format with collapsible sections.
	MarkdownFormatGitHub MarkdownFormat = "github"
	// MarkdownFormatAtlantis is the Atlantis-style format with summary at top.
	MarkdownFormatAtlantis MarkdownFormat = "atlantis"
)

// MarkdownWriter writes diff output as GitHub-compatible markdown.
type MarkdownWriter struct {
	baseFileWriter
	format       MarkdownFormat
	summaryOnly  bool
	contextLines int // for unified diff context in md-unified format
	// Pre-computed summary for Atlantis format (needs to be written at header)
	pendingSummary *Summary
}

// NewMarkdownWriter creates a new MarkdownWriter.
// contextLines specifies the number of context lines for unified diff format (md-unified).
func NewMarkdownWriter(filePath string, format MarkdownFormat, contextLines int) (*MarkdownWriter, error) {
	base, err := newBaseFileWriter(filePath, "markdown")
	if err != nil {
		return nil, err
	}

	return &MarkdownWriter{
		baseFileWriter: base,
		format:         format,
		contextLines:   contextLines,
	}, nil
}

// WriteHeader writes the markdown header.
func (m *MarkdownWriter) WriteHeader(title string) error {
	if m.format == MarkdownFormatAtlantis {
		// Atlantis format: title only, summary will be written separately
		_, err := io.WriteString(m.file, fmt.Sprintf("## %s\n\n", html.EscapeString(title)))
		return err
	}

	// GitHub format: simple header
	_, err := io.WriteString(m.file, fmt.Sprintf("## %s\n\n", html.EscapeString(title)))
	return err
}

// WriteAppDiff writes the diff for an application.
func (m *MarkdownWriter) WriteAppDiff(appDiff *types.AppDiff, depth int) error {
	if m.format == MarkdownFormatAtlantis {
		return m.writeAppDiffAtlantis(appDiff, depth)
	}
	return m.writeAppDiffGitHub(appDiff, depth)
}

// writeAppDiffGitHub writes app diff in GitHub format.
func (m *MarkdownWriter) writeAppDiffGitHub(appDiff *types.AppDiff, _ int) error {
	appName := appDiff.Name
	if appDiff.Namespace != "" {
		appName += fmt.Sprintf(" (%s)", appDiff.Namespace)
	}

	// Type assert DiffResult
	result, ok := appDiff.DiffResult.(*diff.ManifestSetDiff)

	// Build summary line with emoji badges
	var badges []string
	if appDiff.Error != nil {
		badges = append(badges, "❌ Error")
	} else if ok && result != nil {
		// Show parse errors
		if len(result.ParseErrors) > 0 {
			badges = append(badges, fmt.Sprintf("⚠️ %d parse error(s)", len(result.ParseErrors)))
		}
		// Show changes
		if result.HasChanges {
			if len(result.Added) > 0 {
				badges = append(badges, fmt.Sprintf("🟢+%d", len(result.Added)))
			}
			if len(result.Removed) > 0 {
				badges = append(badges, fmt.Sprintf("🔴-%d", len(result.Removed)))
			}
			if len(result.Modified) > 0 {
				badges = append(badges, fmt.Sprintf("🟡~%d", len(result.Modified)))
			}
		}
	}

	badgeStr := ""
	if len(badges) > 0 {
		badgeStr = " " + strings.Join(badges, " ")
	}

	// Use <details> for collapsible section (supported by GitHub)
	m.write("<details>\n")
	m.write(fmt.Sprintf("<summary><b>%s</b>%s</summary>\n\n", html.EscapeString(appName), badgeStr))

	// Error message
	if appDiff.Error != nil {
		m.write(fmt.Sprintf("> ⚠️ %s\n\n", html.EscapeString(appDiff.Error.Error())))
	} else if !ok || result == nil {
		m.write("_No diff available_\n\n")
	} else {
		// Show parse errors if present
		if len(result.ParseErrors) > 0 {
			m.write(fmt.Sprintf("> ⚠️ **%d YAML parse error(s):**\n", len(result.ParseErrors)))
			for _, err := range result.ParseErrors {
				m.write(fmt.Sprintf("> - %s\n", html.EscapeString(err)))
			}
			m.write("\n")
		}

		// Show changes
		if !result.HasChanges {
			// Don't show "No changes" if there were parse errors
			if len(result.ParseErrors) == 0 {
				m.write("_No changes_\n\n")
			}
		} else if !m.summaryOnly {
			m.writeDetailedDiffGitHub(result)
		}
	}

	m.write("</details>\n\n")
	return nil
}

// writeAppDiffAtlantis writes app diff in Atlantis style.
func (m *MarkdownWriter) writeAppDiffAtlantis(appDiff *types.AppDiff, _ int) error {
	appName := appDiff.Name
	if appDiff.Namespace != "" {
		appName += fmt.Sprintf(" (%s)", appDiff.Namespace)
	}

	// Type assert DiffResult
	result, ok := appDiff.DiffResult.(*diff.ManifestSetDiff)

	// Build summary line with emoji badges
	var badges []string
	if appDiff.Error != nil {
		badges = append(badges, "❌")
	} else if ok && result != nil {
		// Show parse errors
		if len(result.ParseErrors) > 0 {
			badges = append(badges, fmt.Sprintf("⚠️%d", len(result.ParseErrors)))
		}
		// Show changes
		if result.HasChanges {
			if len(result.Added) > 0 {
				badges = append(badges, fmt.Sprintf("🟢+%d", len(result.Added)))
			}
			if len(result.Removed) > 0 {
				badges = append(badges, fmt.Sprintf("🔴-%d", len(result.Removed)))
			}
			if len(result.Modified) > 0 {
				badges = append(badges, fmt.Sprintf("🟡~%d", len(result.Modified)))
			}
		}
	}

	badgeStr := ""
	if len(badges) > 0 {
		badgeStr = " — " + strings.Join(badges, " ")
	}

	// Atlantis style: "Show diff for <b>app-name</b>"
	m.write("<details>\n")
	m.write(fmt.Sprintf("<summary>Show diff for <b>%s</b>%s</summary>\n\n", html.EscapeString(appName), badgeStr))

	// Error message
	if appDiff.Error != nil {
		m.write(fmt.Sprintf("> ⚠️ %s\n\n", html.EscapeString(appDiff.Error.Error())))
	} else if !ok || result == nil {
		m.write("_No diff available_\n\n")
	} else {
		// Show parse errors if present
		if len(result.ParseErrors) > 0 {
			m.write(fmt.Sprintf("> ⚠️ **%d YAML parse error(s):**\n", len(result.ParseErrors)))
			for _, err := range result.ParseErrors {
				m.write(fmt.Sprintf("> - %s\n", html.EscapeString(err)))
			}
			m.write("\n")
		}

		// Show changes
		if !result.HasChanges {
			// Don't show "No changes" if there were parse errors
			if len(result.ParseErrors) == 0 {
				m.write("_No changes_\n\n")
			}
		} else if !m.summaryOnly {
			m.writeDetailedDiffAtlantis(result)
		}
	}

	m.write("</details>\n\n")
	return nil
}

// writeDetailedDiffGitHub writes detailed diff for GitHub using diff code blocks.
func (m *MarkdownWriter) writeDetailedDiffGitHub(result *diff.ManifestSetDiff) {
	// Added resources
	for _, man := range result.Added {
		m.write(fmt.Sprintf("#### ➕ %s\n\n", man.Key()))
		m.write("```yaml\n")
		m.write(man.Raw)
		if !strings.HasSuffix(man.Raw, "\n") {
			m.write("\n")
		}
		m.write("```\n\n")
	}

	// Removed resources
	for _, man := range result.Removed {
		m.write(fmt.Sprintf("#### ➖ %s\n\n", man.Key()))
		m.write("```yaml\n")
		m.write(man.Raw)
		if !strings.HasSuffix(man.Raw, "\n") {
			m.write("\n")
		}
		m.write("```\n\n")
	}

	// Modified resources - show as diff code block
	for _, md := range result.Modified {
		m.write(fmt.Sprintf("#### 📝 %s\n\n", md.Key))
		if md.Diff != nil && len(md.Diff.Changes) > 0 {
			m.write("```diff\n")
			for _, change := range md.Diff.Changes {
				switch change.Type {
				case diff.ChangeTypeAdded:
					m.write(fmt.Sprintf("+ %s: %v\n", change.Path, change.NewValue))
				case diff.ChangeTypeRemoved:
					m.write(fmt.Sprintf("- %s: %v\n", change.Path, change.OldValue))
				case diff.ChangeTypeModified:
					m.write(fmt.Sprintf("- %s: %v\n", change.Path, change.OldValue))
					m.write(fmt.Sprintf("+ %s: %v\n", change.Path, change.NewValue))
				}
			}
			m.write("```\n\n")
		}
	}
}

// writeDetailedDiffAtlantis writes detailed diff in unified diff format.
func (m *MarkdownWriter) writeDetailedDiffAtlantis(result *diff.ManifestSetDiff) {
	diffs, err := GenerateManifestUnifiedDiffs(result, m.contextLines)
	if err != nil {
		m.write(fmt.Sprintf("> Error generating diff: %s\n\n", err.Error()))
		return
	}

	// Added resources
	for _, man := range result.Added {
		m.write(fmt.Sprintf("#### ➕ %s\n\n", man.Key()))
		if d, ok := diffs[man.Key()]; ok && d != "" {
			m.write("```diff\n")
			m.write(d)
			if !strings.HasSuffix(d, "\n") {
				m.write("\n")
			}
			m.write("```\n\n")
		}
	}

	// Removed resources
	for _, man := range result.Removed {
		m.write(fmt.Sprintf("#### ➖ %s\n\n", man.Key()))
		if d, ok := diffs[man.Key()]; ok && d != "" {
			m.write("```diff\n")
			m.write(d)
			if !strings.HasSuffix(d, "\n") {
				m.write("\n")
			}
			m.write("```\n\n")
		}
	}

	// Modified resources
	for _, md := range result.Modified {
		m.write(fmt.Sprintf("#### 📝 %s\n\n", md.Key))
		if d, ok := diffs[md.Key]; ok && d != "" {
			m.write("```diff\n")
			m.write(d)
			if !strings.HasSuffix(d, "\n") {
				m.write("\n")
			}
			m.write("```\n\n")
		}
	}
}

// WriteTree writes the full application tree.
func (m *MarkdownWriter) WriteTree(tree *diff.AppTree) error {
	tree.Walk(func(node *diff.AppTreeNode, depth int) {
		if appDiff, ok := node.AppDiff.(*types.AppDiff); ok {
			m.WriteAppDiff(appDiff, depth)
		}
	})
	return nil
}

// WriteSummary writes the summary.
func (m *MarkdownWriter) WriteSummary(summary Summary) error {
	if m.format == MarkdownFormatAtlantis {
		return m.writeSummaryAtlantis(summary)
	}
	return m.writeSummaryGitHub(summary)
}

// writeSummaryGitHub writes summary in GitHub-compatible markdown.
func (m *MarkdownWriter) writeSummaryGitHub(summary Summary) error {
	m.write("---\n\n")

	// Build inline summary matching Atlantis style
	var parts []string
	parts = append(parts, fmt.Sprintf("%d applications affected", summary.TotalApps))
	parts = append(parts, fmt.Sprintf("%d changed", summary.AppsWithChanges))

	if summary.TotalAdded > 0 || summary.TotalRemoved > 0 || summary.TotalModified > 0 {
		parts = append(parts, fmt.Sprintf("+%d/-%d/~%d resources",
			summary.TotalAdded, summary.TotalRemoved, summary.TotalModified))
	}

	if summary.AppsWithErrors > 0 {
		parts = append(parts, fmt.Sprintf("%d errors", summary.AppsWithErrors))
	}

	m.write(fmt.Sprintf("**Summary:** %s\n", strings.Join(parts, " | ")))
	return nil
}

// writeSummaryAtlantis writes summary in Atlantis style (at the end, with action commands).
func (m *MarkdownWriter) writeSummaryAtlantis(summary Summary) error {
	m.write("---\n\n")

	// Unified summary format
	var parts []string
	parts = append(parts, fmt.Sprintf("%d applications affected", summary.TotalApps))
	parts = append(parts, fmt.Sprintf("%d changed", summary.AppsWithChanges))

	if summary.TotalAdded > 0 || summary.TotalRemoved > 0 || summary.TotalModified > 0 {
		parts = append(parts, fmt.Sprintf("+%d/-%d/~%d resources",
			summary.TotalAdded, summary.TotalRemoved, summary.TotalModified))
	}

	if summary.AppsWithErrors > 0 {
		parts = append(parts, fmt.Sprintf("%d errors", summary.AppsWithErrors))
	}

	m.write(fmt.Sprintf("**Summary:** %s\n", strings.Join(parts, " | ")))

	return nil
}

// WriteFooter writes the footer.
func (m *MarkdownWriter) WriteFooter() error {
	m.write(fmt.Sprintf("\n---\n_Generated at %s by argocdf_\n", time.Now().Format(time.RFC3339)))
	return nil
}

// Flush flushes and closes the file.
func (m *MarkdownWriter) Flush() error {
	return m.close()
}
