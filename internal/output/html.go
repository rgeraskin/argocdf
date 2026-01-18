// Package output provides HTML output functionality.
package output

import (
	"fmt"
	"html"
	"io"
	"os"
	"time"

	"github.com/rgeraskin/argocdf/internal/diff"
	"github.com/rgeraskin/argocdf/internal/types"
)

// HTMLWriter writes diff output as an HTML report.
type HTMLWriter struct {
	file    *os.File
	verbose bool
}

// NewHTMLWriter creates a new HTMLWriter.
func NewHTMLWriter(filePath string, verbose bool) (*HTMLWriter, error) {
	file, err := os.Create(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTML file: %w", err)
	}

	return &HTMLWriter{
		file:    file,
		verbose: verbose,
	}, nil
}

// WriteHeader writes the HTML header.
func (h *HTMLWriter) WriteHeader(title string) error {
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
        .container {
            max-width: 1200px;
            margin: 0 auto;
        }
        h1, h2, h3 {
            color: var(--header-color);
        }
        h1 {
            border-bottom: 2px solid var(--border-color);
            padding-bottom: 10px;
        }
        .app-card {
            background-color: var(--code-bg);
            border: 1px solid var(--border-color);
            border-radius: 8px;
            margin: 15px 0;
            padding: 15px;
        }
        .app-header {
            display: flex;
            align-items: center;
            gap: 10px;
            margin-bottom: 10px;
        }
        .app-name {
            font-size: 1.2em;
            font-weight: bold;
            color: var(--header-color);
        }
        .app-namespace {
            color: #888;
            font-size: 0.9em;
        }
        .app-children {
            margin-left: 30px;
            border-left: 2px solid var(--border-color);
            padding-left: 15px;
        }
        .badge {
            padding: 2px 8px;
            border-radius: 4px;
            font-size: 0.8em;
            font-weight: bold;
        }
        .badge-added {
            background-color: rgba(78, 201, 176, 0.2);
            color: var(--added-color);
        }
        .badge-removed {
            background-color: rgba(241, 76, 76, 0.2);
            color: var(--removed-color);
        }
        .badge-modified {
            background-color: rgba(220, 220, 170, 0.2);
            color: var(--modified-color);
        }
        .badge-error {
            background-color: rgba(241, 76, 76, 0.2);
            color: var(--removed-color);
        }
        .diff-container {
            background-color: #1a1a1a;
            border-radius: 4px;
            overflow: hidden;
            margin-top: 10px;
            font-family: 'Monaco', 'Menlo', 'Ubuntu Mono', monospace;
            font-size: 0.85em;
        }
        .diff-line {
            padding: 2px 10px;
            white-space: pre-wrap;
            word-wrap: break-word;
        }
        .diff-add {
            background-color: rgba(78, 201, 176, 0.15);
            color: var(--added-color);
        }
        .diff-del {
            background-color: rgba(241, 76, 76, 0.15);
            color: var(--removed-color);
        }
        .diff-context {
            color: #888;
        }
        .summary {
            background-color: var(--code-bg);
            border: 1px solid var(--border-color);
            border-radius: 8px;
            padding: 20px;
            margin-top: 30px;
        }
        .summary-grid {
            display: grid;
            grid-template-columns: repeat(auto-fit, minmax(150px, 1fr));
            gap: 15px;
            margin-top: 15px;
        }
        .summary-item {
            text-align: center;
        }
        .summary-value {
            font-size: 2em;
            font-weight: bold;
        }
        .summary-label {
            color: #888;
            font-size: 0.9em;
        }
        .error-message {
            color: var(--removed-color);
            padding: 10px;
            background-color: rgba(241, 76, 76, 0.1);
            border-radius: 4px;
        }
        .no-changes {
            color: #888;
            font-style: italic;
        }
        .manifest-key {
            font-family: monospace;
            font-size: 0.9em;
            color: #888;
        }
        .timestamp {
            color: #666;
            font-size: 0.8em;
            margin-top: 30px;
            text-align: center;
        }
        details {
            margin-top: 10px;
        }
        summary {
            cursor: pointer;
            color: var(--header-color);
        }
        summary:hover {
            text-decoration: underline;
        }
    </style>
</head>
<body>
    <div class="container">
        <h1>%s</h1>
`, html.EscapeString(title), html.EscapeString(title)))
	return err
}

// WriteAppDiff writes the diff for an application.
func (h *HTMLWriter) WriteAppDiff(appDiff *types.AppDiff, depth int) error {
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

	// Type assert DiffResult
	result, ok := appDiff.DiffResult.(*diff.ManifestSetDiff)

	// Badges
	if appDiff.Error != nil {
		h.write(`<span class="badge badge-error">Error</span>`)
	} else if ok && result != nil && result.HasChanges {
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
	h.write(`</div>`)

	// Error message
	if appDiff.Error != nil {
		h.write(fmt.Sprintf(`<div class="error-message">%s</div>`, html.EscapeString(appDiff.Error.Error())))
	} else if !ok || result == nil || !result.HasChanges {
		h.write(`<p class="no-changes">No changes</p>`)
	} else {
		h.writeDetailedDiff(result)
	}

	h.write(`</div>`)
	return nil
}

// writeDetailedDiff writes the detailed diff.
func (h *HTMLWriter) writeDetailedDiff(result *diff.ManifestSetDiff) {
	// Added
	if len(result.Added) > 0 {
		h.write(`<details open><summary>Added Resources</summary>`)
		for _, m := range result.Added {
			h.write(fmt.Sprintf(`<div class="manifest-key diff-add">+ %s</div>`, html.EscapeString(m.Key())))
		}
		h.write(`</details>`)
	}

	// Removed
	if len(result.Removed) > 0 {
		h.write(`<details open><summary>Removed Resources</summary>`)
		for _, m := range result.Removed {
			h.write(fmt.Sprintf(`<div class="manifest-key diff-del">- %s</div>`, html.EscapeString(m.Key())))
		}
		h.write(`</details>`)
	}

	// Modified
	if len(result.Modified) > 0 {
		h.write(`<details open><summary>Modified Resources</summary>`)
		for _, md := range result.Modified {
			h.write(fmt.Sprintf(`<details><summary class="manifest-key">~ %s</summary>`, html.EscapeString(md.Key)))
			if md.Diff != nil {
				h.writeFieldChangesHTML(md.Diff)
			}
			h.write(`</details>`)
		}
		h.write(`</details>`)
	}
}

// writeFieldChangesHTML writes field-level changes as HTML.
func (h *HTMLWriter) writeFieldChangesHTML(result *diff.DiffResult) {
	h.write(`<div class="diff-container">`)

	for _, change := range result.Changes {
		switch change.Type {
		case diff.ChangeTypeAdded:
			h.write(fmt.Sprintf(`<div class="diff-line diff-add">+ %s: %v</div>`,
				html.EscapeString(change.Path), change.NewValue))
		case diff.ChangeTypeRemoved:
			h.write(fmt.Sprintf(`<div class="diff-line diff-del">- %s: %v</div>`,
				html.EscapeString(change.Path), change.OldValue))
		case diff.ChangeTypeModified:
			h.write(fmt.Sprintf(`<div class="diff-line diff-context">~ %s:</div>`,
				html.EscapeString(change.Path)))
			h.write(fmt.Sprintf(`<div class="diff-line diff-del">  - %v</div>`, change.OldValue))
			h.write(fmt.Sprintf(`<div class="diff-line diff-add">  + %v</div>`, change.NewValue))
		}
	}

	h.write(`</div>`)
}

// WriteTree writes the full application tree.
func (h *HTMLWriter) WriteTree(tree *diff.AppTree) error {
	tree.Walk(func(node *diff.AppTreeNode, depth int) {
		if appDiff, ok := node.AppDiff.(*types.AppDiff); ok {
			h.WriteAppDiff(appDiff, depth)
		}
	})
	return nil
}

// WriteSummary writes the summary.
func (h *HTMLWriter) WriteSummary(summary Summary) error {
	h.write(`<div class="summary">`)
	h.write(`<h2>Summary</h2>`)
	h.write(`<div class="summary-grid">`)

	h.writeSummaryItem("Total Apps", fmt.Sprintf("%d", summary.TotalApps), "")
	h.writeSummaryItem("Changed", fmt.Sprintf("%d", summary.AppsWithChanges), "modified")

	if summary.AppsWithErrors > 0 {
		h.writeSummaryItem("Errors", fmt.Sprintf("%d", summary.AppsWithErrors), "removed")
	}

	if summary.TotalAdded > 0 {
		h.writeSummaryItem("Added", fmt.Sprintf("+%d", summary.TotalAdded), "added")
	}
	if summary.TotalRemoved > 0 {
		h.writeSummaryItem("Removed", fmt.Sprintf("-%d", summary.TotalRemoved), "removed")
	}
	if summary.TotalModified > 0 {
		h.writeSummaryItem("Modified", fmt.Sprintf("~%d", summary.TotalModified), "modified")
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

// WriteFooter writes the footer.
func (h *HTMLWriter) WriteFooter() error {
	h.write(fmt.Sprintf(`<p class="timestamp">Generated at %s by argocdf</p>`, time.Now().Format(time.RFC3339)))
	h.write(`</div></body></html>`)
	return nil
}

// Flush flushes and closes the file.
func (h *HTMLWriter) Flush() error {
	return h.file.Close()
}

// write is a helper to write strings.
func (h *HTMLWriter) write(s string) {
	io.WriteString(h.file, s)
}
