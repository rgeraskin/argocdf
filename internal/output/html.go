// Package output provides HTML output functionality.
package output

import (
	"fmt"
	"html"
	"io"
	"strings"
	"time"

	"github.com/rgeraskin/argocdf/internal/diff"
)

// HTMLWriter writes diff output as an HTML report.
type HTMLWriter struct {
	baseFileWriter
}

// NewHTMLWriter creates a new HTMLWriter writing a side-by-side report to filePath.
func NewHTMLWriter(filePath string) (*HTMLWriter, error) {
	base, err := newBaseFileWriter(filePath, "HTML")
	if err != nil {
		return nil, err
	}
	return &HTMLWriter{baseFileWriter: base}, nil
}

// WriteHeader writes the HTML header.
func (h *HTMLWriter) WriteHeader(title string) error {
	// Full HTML document for standalone viewing
	_, err := io.WriteString(h.file, fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>%s</title>
    <style>
        :root {
            --bg-color: #1e1e1e;
            --text-color: #d4d4d4;
            --header-color: #569cd6;
            --added-color: #4ec9b0;
            --removed-color: #f14c4c;
            --modified-color: #dcdcaa;
            --border-color: #404040;
            --code-bg: #2d2d2d;
        }
        body {
            font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, Oxygen, Ubuntu, sans-serif;
            background-color: var(--bg-color);
            color: var(--text-color);
            margin: 0;
            padding: 20px;
            line-height: 1.6;
        }
        .container { max-width: 1400px; margin: 0 auto; }
        h1, h2, h3 { color: var(--header-color); }
        h1 { border-bottom: 2px solid var(--border-color); padding-bottom: 10px; }
        .app-card { background-color: var(--code-bg); border: 1px solid var(--border-color); border-radius: 8px; margin: 15px 0; padding: 15px; }
        .app-header { display: flex; align-items: center; gap: 10px; margin-bottom: 10px; }
        .app-name { font-size: 1.2em; font-weight: bold; color: var(--header-color); }
        .app-namespace { color: #888; font-size: 0.9em; }
        .app-children { margin-left: 30px; border-left: 2px solid var(--border-color); padding-left: 15px; }
        .badge { padding: 2px 8px; border-radius: 4px; font-size: 0.8em; font-weight: bold; }
        .badge-added { background-color: rgba(78, 201, 176, 0.2); color: var(--added-color); }
        .badge-removed { background-color: rgba(241, 76, 76, 0.2); color: var(--removed-color); }
        .badge-modified { background-color: rgba(220, 220, 170, 0.2); color: var(--modified-color); }
        .badge-error { background-color: rgba(241, 76, 76, 0.2); color: var(--removed-color); }
        .badge-warning { background-color: rgba(220, 220, 170, 0.2); color: var(--modified-color); }
        .summary { background-color: var(--code-bg); border: 1px solid var(--border-color); border-radius: 8px; padding: 20px; margin-top: 30px; }
        .summary-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: 15px; margin-top: 15px; }
        .summary-item { text-align: center; }
        .summary-value { font-size: 2em; font-weight: bold; }
        .summary-label { color: #888; font-size: 0.9em; }
        .error-message { color: var(--removed-color); padding: 10px; background-color: rgba(241, 76, 76, 0.1); border-radius: 4px; }
        .warning-message { color: var(--modified-color); padding: 10px; background-color: rgba(220, 220, 170, 0.1); border-radius: 4px; }
        .no-changes { color: #888; font-style: italic; }
        .timestamp { color: #666; font-size: 0.8em; margin-top: 30px; text-align: center; }
        details { margin-top: 10px; }
        summary { cursor: pointer; color: var(--header-color); }
        summary:hover { text-decoration: underline; }
    </style>
</head>
<body>
    <div class="container">
        <h1>%s</h1>
`, html.EscapeString(title), html.EscapeString(title)))
	return err
}

// WriteAppDiff writes the diff for an application.
func (h *HTMLWriter) WriteAppDiff(appDiff *diff.AppDiff, depth int) error {
	return h.writeAppDiffFull(appDiff, depth)
}

// writeAppDiffFull writes app diff with full HTML/CSS (for standalone file).
func (h *HTMLWriter) writeAppDiffFull(appDiff *diff.AppDiff, depth int) error {
	var class string
	if depth > 0 {
		class = "app-children"
	}

	h.write(fmt.Sprintf(`<div class="app-card %s">`, class))
	h.write(`<div class="app-header">`)
	h.write(fmt.Sprintf(`<span class="app-name">%s</span>`, html.EscapeString(appDiff.Name)))
	if appDiff.Namespace != "" {
		h.write(fmt.Sprintf(`<span class="app-namespace">(%s)</span>`, html.EscapeString(appDiff.Namespace)))
	}

	result := appDiff.Diff

	// Badges
	if appDiff.Error != nil {
		h.write(`<span class="badge badge-error">Error</span>`)
	} else if result != nil {
		// Show parse errors
		if len(result.ParseErrors) > 0 {
			h.write(fmt.Sprintf(`<span class="badge badge-error">⚠ %d parse error(s)</span>`, len(result.ParseErrors)))
		}
		// Show parse warnings
		if len(result.ParseWarnings) > 0 {
			h.write(fmt.Sprintf(`<span class="badge badge-warning">⚠ %d warning(s)</span>`, len(result.ParseWarnings)))
		}
		// Show changes
		if result.HasChanges {
			if len(result.Added) > 0 {
				h.write(fmt.Sprintf(`<span class="badge badge-added">+%d</span>`, len(result.Added)))
			}
			if len(result.Removed) > 0 {
				h.write(fmt.Sprintf(`<span class="badge badge-removed">-%d</span>`, len(result.Removed)))
			}
			if len(result.Modified) > 0 {
				h.write(fmt.Sprintf(`<span class="badge badge-modified">~%d</span>`, len(result.Modified)))
			}
		}
	}
	h.write(`</div>`)

	// Error message
	if appDiff.Error != nil {
		h.write(fmt.Sprintf(`<div class="error-message">%s</div>`, html.EscapeString(appDiff.Error.Error())))
	} else if result == nil {
		h.write(`<p class="no-changes">No diff available</p>`)
	} else {
		// Show parse errors if present
		if len(result.ParseErrors) > 0 {
			h.write(fmt.Sprintf(`<div class="error-message"><strong>⚠ %d YAML parse error(s):</strong><ul>`, len(result.ParseErrors)))
			for _, err := range result.ParseErrors {
				h.write(fmt.Sprintf(`<li>%s</li>`, html.EscapeString(err)))
			}
			h.write(`</ul></div>`)
		}

		// Show parse warnings if present (non-fatal; documents are still diffed)
		if len(result.ParseWarnings) > 0 {
			h.write(fmt.Sprintf(`<div class="warning-message"><strong>⚠ %d warning(s):</strong><ul>`, len(result.ParseWarnings)))
			for _, warn := range result.ParseWarnings {
				h.write(fmt.Sprintf(`<li>%s</li>`, html.EscapeString(warn)))
			}
			h.write(`</ul></div>`)
		}

		// Show changes
		if !result.HasChanges {
			// Don't show "No changes" if there were parse errors
			if len(result.ParseErrors) == 0 {
				h.write(`<p class="no-changes">No changes</p>`)
			}
		} else {
			h.writeDetailedDiffSideBySide(result)
		}
	}

	h.write(`</div>`)
	return nil
}

// writeDetailedDiffSideBySide writes the diff as a pure HTML side-by-side view.
func (h *HTMLWriter) writeDetailedDiffSideBySide(result *diff.ManifestSetDiff) {
	// Show added manifests (empty left side, content on right)
	for _, m := range result.Added {
		h.writeSideBySideDiffBlock(m.Key(), "", m.Raw, "added")
	}

	// Show removed manifests (content on left, empty right side)
	for _, m := range result.Removed {
		h.writeSideBySideDiffBlock(m.Key(), m.Raw, "", "removed")
	}

	// Show modified manifests (old on left, new on right)
	for _, md := range result.Modified {
		oldContent := ""
		newContent := ""
		if md.Old != nil {
			oldContent = md.Old.Raw
		}
		if md.New != nil {
			newContent = md.New.Raw
		}
		h.writeSideBySideDiffBlock(md.Key, oldContent, newContent, "modified")
	}
}

// Inline style constants for side-by-side diff (dark theme)
const (
	// Background colors for diff highlighting (dark theme compatible)
	bgAdded   = "rgba(78, 201, 176, 0.15)"
	bgRemoved = "rgba(241, 76, 76, 0.15)"
	bgEmpty   = "#252525"

	// Text colors
	textAdded   = "#4ec9b0"
	textRemoved = "#f14c4c"
	textNormal  = "#d4d4d4"

	// Badge styles (dark theme)
	badgeAddedStyle   = "background-color:rgba(78,201,176,0.2);color:#4ec9b0;padding:2px 8px;border-radius:4px;font-weight:bold;font-family:monospace;"
	badgeRemovedStyle = "background-color:rgba(241,76,76,0.2);color:#f14c4c;padding:2px 8px;border-radius:4px;font-weight:bold;font-family:monospace;"
	badgeModStyle     = "background-color:rgba(220,220,170,0.2);color:#dcdcaa;padding:2px 8px;border-radius:4px;font-weight:bold;font-family:monospace;"

	// Table styles (dark theme)
	tableStyle      = "width:100%;border-collapse:collapse;font-family:SFMono-Regular,Consolas,Liberation Mono,Menlo,monospace;font-size:12px;background-color:#1a1a1a;"
	lineNumStyle    = "width:40px;padding:0 8px;text-align:right;color:#6a737d;background-color:#252525;border-right:1px solid #404040;user-select:none;vertical-align:top;"
	lineContentBase = "padding:0 8px;white-space:pre-wrap;word-wrap:break-word;vertical-align:top;color:#d4d4d4;"
)

// writeSideBySideDiffBlock writes a single manifest diff with its name header.
func (h *HTMLWriter) writeSideBySideDiffBlock(key, oldContent, newContent, changeType string) {
	// Determine badge style based on change type
	badgeStyle := badgeModStyle
	prefix := "~"
	switch changeType {
	case "added":
		badgeStyle = badgeAddedStyle
		prefix = "+"
	case "removed":
		badgeStyle = badgeRemovedStyle
		prefix = "-"
	}

	h.write(fmt.Sprintf(`<div style="margin:15px 0;overflow-x:auto;">
<div style="margin-bottom:8px;"><span style="%s">%s %s</span></div>
`, badgeStyle, prefix, html.EscapeString(key)))

	// Generate side-by-side table
	h.writeSideBySideTable(oldContent, newContent)

	h.write(`</div>`)
}

// writeSideBySideTable generates a pure HTML side-by-side diff table with inline styles.
func (h *HTMLWriter) writeSideBySideTable(oldContent, newContent string) {
	oldLines := splitLines(oldContent)
	newLines := splitLines(newContent)

	h.write(fmt.Sprintf(`<table style="%s"><tbody>`, tableStyle))

	// Simple line-by-line comparison
	maxLines := len(oldLines)
	if len(newLines) > maxLines {
		maxLines = len(newLines)
	}

	for i := 0; i < maxLines; i++ {
		var oldLine, newLine string
		var oldNum, newNum string
		hasOld := i < len(oldLines)
		hasNew := i < len(newLines)

		if hasOld {
			oldLine = oldLines[i]
			oldNum = fmt.Sprintf("%d", i+1)
		}
		if hasNew {
			newLine = newLines[i]
			newNum = fmt.Sprintf("%d", i+1)
		}

		// Determine styling based on differences
		leftBg := ""
		rightBg := ""
		leftColor := textNormal
		rightColor := textNormal

		if !hasOld && hasNew {
			rightBg = bgAdded
			rightColor = textAdded
		} else if hasOld && !hasNew {
			leftBg = bgRemoved
			leftColor = textRemoved
		} else if oldLine != newLine {
			leftBg = bgRemoved
			rightBg = bgAdded
			leftColor = textRemoved
			rightColor = textAdded
		}

		h.write(`<tr>`)

		// Left side (old) - line number
		leftNumStyle := lineNumStyle
		if leftBg != "" {
			leftNumStyle += fmt.Sprintf("background-color:%s;", leftBg)
		}
		h.write(fmt.Sprintf(`<td style="%s">%s</td>`, leftNumStyle, oldNum))

		// Left side (old) - content
		leftContentStyle := lineContentBase + "border-right:1px solid #404040;"
		if leftBg != "" {
			leftContentStyle += fmt.Sprintf("background-color:%s;", leftBg)
		}
		if !hasOld {
			leftContentStyle += fmt.Sprintf("background-color:%s;", bgEmpty)
		}
		leftContentStyle += fmt.Sprintf("color:%s;", leftColor)
		h.write(fmt.Sprintf(`<td style="%s">%s</td>`, leftContentStyle, html.EscapeString(oldLine)))

		// Right side (new) - line number
		rightNumStyle := lineNumStyle
		if rightBg != "" {
			rightNumStyle += fmt.Sprintf("background-color:%s;", rightBg)
		}
		h.write(fmt.Sprintf(`<td style="%s">%s</td>`, rightNumStyle, newNum))

		// Right side (new) - content
		rightContentStyle := lineContentBase
		if rightBg != "" {
			rightContentStyle += fmt.Sprintf("background-color:%s;", rightBg)
		}
		if !hasNew {
			rightContentStyle += fmt.Sprintf("background-color:%s;", bgEmpty)
		}
		rightContentStyle += fmt.Sprintf("color:%s;", rightColor)
		h.write(fmt.Sprintf(`<td style="%s">%s</td>`, rightContentStyle, html.EscapeString(newLine)))

		h.write(`</tr>`)
	}

	h.write(`</tbody></table>`)
}

// splitLines splits content into lines, handling empty content.
func splitLines(content string) []string {
	if content == "" {
		return []string{}
	}
	lines := strings.Split(content, "\n")
	// Remove trailing empty line if present (from trailing newline)
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// WriteTree writes the full application tree.
func (h *HTMLWriter) WriteTree(tree *diff.AppTree) error {
	tree.Walk(func(node *diff.AppTreeNode, depth int) {
		_ = h.WriteAppDiff(node.AppDiff, depth)
	})
	return nil
}

// WriteSummary writes the summary.
func (h *HTMLWriter) WriteSummary(summary Summary) error {
	h.write(`<div class="summary">`)
	h.write(`<h2>Summary</h2>`)
	h.write(`<div class="summary-grid">`)

	h.writeSummaryItem("Apps affected", fmt.Sprintf("%d", summary.TotalApps), "")
	h.writeSummaryItem("Apps changed", fmt.Sprintf("%d", summary.AppsWithChanges), "modified")

	// Always show resources line if there are any changes
	if summary.TotalAdded > 0 || summary.TotalRemoved > 0 || summary.TotalModified > 0 {
		h.writeSummaryItem("Added", fmt.Sprintf("+%d", summary.TotalAdded), "added")
		h.writeSummaryItem("Removed", fmt.Sprintf("-%d", summary.TotalRemoved), "removed")
		h.writeSummaryItem("Modified", fmt.Sprintf("~%d", summary.TotalModified), "modified")
	}

	if summary.AppsWithErrors > 0 {
		h.writeSummaryItem("Errors", fmt.Sprintf("%d", summary.AppsWithErrors), "removed")
	}

	h.write(`</div>`)
	h.write(`</div>`)
	return nil
}

// writeSummaryItem writes a summary item.
func (h *HTMLWriter) writeSummaryItem(label, value, colorClass string) {
	color := ""
	switch colorClass {
	case "added":
		color = `style="color: var(--added-color)"`
	case "removed":
		color = `style="color: var(--removed-color)"`
	case "modified":
		color = `style="color: var(--modified-color)"`
	}

	h.write(fmt.Sprintf(`<div class="summary-item">
		<div class="summary-value" %s>%s</div>
		<div class="summary-label">%s</div>
	</div>`, color, value, label))
}

// WriteFooter writes the footer, stamping the run's provenance.
func (h *HTMLWriter) WriteFooter(p Provenance) error {
	h.write(fmt.Sprintf(`<p class="timestamp">Generated at %s by <a href="https://github.com/rgeraskin/argocdf">argocdf</a>%s</p>`,
		time.Now().Format(time.RFC3339), html.EscapeString(p.suffix())))
	h.write(`</div></body></html>`)
	return nil
}

// Flush flushes and closes the file.
func (h *HTMLWriter) Flush() error {
	return h.close()
}
