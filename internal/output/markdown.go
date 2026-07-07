// Package output provides markdown output functionality.
package output

import (
	"fmt"
	"html"
	"os"
	"path/filepath"
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

// markerBase is the invisible HTML comment marker emitted as the first line of
// markdown output. CI can match on it to find and upsert its own PR comment.
const markerBase = "argocdf-diff"

// CommentMarker returns the invisible HTML comment marker that identifies an
// argocdf markdown report. An empty id yields the default marker
// ("<!-- argocdf-diff -->"); a non-empty id scopes it ("<!-- argocdf-diff:<id> -->")
// so multiple independent reports can coexist on a single PR.
func CommentMarker(id string) string {
	if id == "" {
		return "<!-- " + markerBase + " -->"
	}
	return fmt.Sprintf("<!-- %s:%s -->", markerBase, id)
}

// MarkdownWriter writes diff output as GitHub-compatible markdown.
//
// The report is buffered in memory as structured sections and only written to
// disk by Flush. With a split limit set (see SetSplitMax), Flush packs whole
// app sections into size-limited part files; without one it writes a single
// file, byte-identical to the streaming behavior it replaced. Buffering costs
// O(report size) even when splitting is off, but that is dwarfed by the
// rendered manifests of both branches argocdf already holds in memory to
// compute the diff, so a separate streaming path for the unsplit case isn't
// worth the dual code paths. Write errors surface at Flush.
type MarkdownWriter struct {
	baseFileWriter
	path         string
	format       MarkdownFormat
	summaryOnly  bool
	contextLines int    // for unified diff context in md-unified format
	markerID     string // marker id for the PR-comment upsert marker (empty = default)
	splitMax     int    // max part size in bytes (0 = single file, no splitting)

	title    string
	sections []appSection
	summary  strings.Builder
	footer   strings.Builder
	buf      *strings.Builder // current target of write()
}

// appSection is one application's report, kept structured so Flush can pack
// whole apps into size-limited parts and, when a single app alone exceeds the
// part budget, split it at resource-block boundaries.
type appSection struct {
	header     string   // "<details>\n<summary>...</summary>\n\n"
	headerCont string   // header variant used when the app continues in a following part
	notes      string   // error/warning/no-changes lines between the summary and the resource blocks
	resources  []string // one whole "#### <emoji> <key>" block per resource
	footer     string   // "</details>\n\n"
}

// render returns the section as it appears in an unsplit report.
func (s appSection) render() string {
	var b strings.Builder
	b.WriteString(s.header)
	b.WriteString(s.notes)
	for _, r := range s.resources {
		b.WriteString(r)
	}
	b.WriteString(s.footer)
	return b.String()
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
		path:           filePath,
		format:         format,
		contextLines:   contextLines,
		buf:            &strings.Builder{},
	}, nil
}

// SetMarker sets the marker id used for the PR-comment upsert marker. An empty
// id keeps the default marker.
func (m *MarkdownWriter) SetMarker(id string) {
	m.markerID = id
}

// SetSplitMax sets the maximum part size in bytes. When the report exceeds it,
// Flush splits the output into multiple part files (<path>, <base>.2.<ext>, ...),
// each a self-contained markdown document fitting a PR comment. 0 disables
// splitting.
func (m *MarkdownWriter) SetSplitMax(n int) {
	m.splitMax = n
}

// write appends to the current section buffer. It intentionally shadows
// baseFileWriter.write: report content is buffered and only written to disk by
// Flush, which needs the full report to decide part boundaries.
func (m *MarkdownWriter) write(s string) {
	m.buf.WriteString(s)
}

// WriteHeader records the report title; the marker and heading are rendered
// per part by Flush.
func (m *MarkdownWriter) WriteHeader(title string) error {
	m.title = title
	return nil
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
		// Show parse warnings
		if len(result.ParseWarnings) > 0 {
			badges = append(badges, fmt.Sprintf("⚠️ %d warning(s)", len(result.ParseWarnings)))
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
	sec := appSection{
		header: fmt.Sprintf("<details>\n<summary><b>%s</b>%s</summary>\n\n",
			html.EscapeString(appName), badgeStr),
		headerCont: fmt.Sprintf("<details>\n<summary><b>%s (continued)</b>%s</summary>\n\n",
			html.EscapeString(appName), badgeStr),
		footer: "</details>\n\n",
	}

	var notes strings.Builder
	m.buf = &notes

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

		// Show parse warnings if present (non-fatal; documents are still diffed)
		if len(result.ParseWarnings) > 0 {
			m.write(fmt.Sprintf("> ⚠️ **%d warning(s):**\n", len(result.ParseWarnings)))
			for _, warn := range result.ParseWarnings {
				m.write(fmt.Sprintf("> - %s\n", html.EscapeString(warn)))
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
			sec.resources = m.detailedDiffBlocksGitHub(result)
		}
	}

	sec.notes = notes.String()
	m.sections = append(m.sections, sec)
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
		// Show parse warnings
		if len(result.ParseWarnings) > 0 {
			badges = append(badges, fmt.Sprintf("⚠️%d", len(result.ParseWarnings)))
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
	sec := appSection{
		header: fmt.Sprintf("<details>\n<summary>Show diff for <b>%s</b>%s</summary>\n\n",
			html.EscapeString(appName), badgeStr),
		headerCont: fmt.Sprintf("<details>\n<summary>Show diff for <b>%s (continued)</b>%s</summary>\n\n",
			html.EscapeString(appName), badgeStr),
		footer: "</details>\n\n",
	}

	var notes strings.Builder
	m.buf = &notes

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

		// Show parse warnings if present (non-fatal; documents are still diffed)
		if len(result.ParseWarnings) > 0 {
			m.write(fmt.Sprintf("> ⚠️ **%d warning(s):**\n", len(result.ParseWarnings)))
			for _, warn := range result.ParseWarnings {
				m.write(fmt.Sprintf("> - %s\n", html.EscapeString(warn)))
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
			sec.resources = m.detailedDiffBlocksAtlantis(result)
		}
	}

	sec.notes = notes.String()
	m.sections = append(m.sections, sec)
	return nil
}

// detailedDiffBlocksGitHub renders the detailed diff for GitHub as one block
// per resource, using diff code blocks.
func (m *MarkdownWriter) detailedDiffBlocksGitHub(result *diff.ManifestSetDiff) []string {
	var blocks []string

	// Added resources
	for _, man := range result.Added {
		var b strings.Builder
		m.buf = &b
		m.write(fmt.Sprintf("#### ➕ %s\n\n", man.Key()))
		m.write("```yaml\n")
		m.write(man.Raw)
		if !strings.HasSuffix(man.Raw, "\n") {
			m.write("\n")
		}
		m.write("```\n\n")
		blocks = append(blocks, b.String())
	}

	// Removed resources
	for _, man := range result.Removed {
		var b strings.Builder
		m.buf = &b
		m.write(fmt.Sprintf("#### ➖ %s\n\n", man.Key()))
		m.write("```yaml\n")
		m.write(man.Raw)
		if !strings.HasSuffix(man.Raw, "\n") {
			m.write("\n")
		}
		m.write("```\n\n")
		blocks = append(blocks, b.String())
	}

	// Modified resources - show as diff code block
	for _, md := range result.Modified {
		var b strings.Builder
		m.buf = &b
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
		blocks = append(blocks, b.String())
	}

	return blocks
}

// detailedDiffBlocksAtlantis renders the detailed diff as one block per
// resource, in unified diff format.
func (m *MarkdownWriter) detailedDiffBlocksAtlantis(result *diff.ManifestSetDiff) []string {
	diffs, err := GenerateManifestUnifiedDiffs(result, m.contextLines)
	if err != nil {
		return []string{fmt.Sprintf("> Error generating diff: %s\n\n", err.Error())}
	}

	var blocks []string

	writeBlock := func(prefix, key string) {
		var b strings.Builder
		m.buf = &b
		m.write(fmt.Sprintf("#### %s %s\n\n", prefix, key))
		if d, ok := diffs[key]; ok && d != "" {
			m.write("```diff\n")
			m.write(d)
			if !strings.HasSuffix(d, "\n") {
				m.write("\n")
			}
			m.write("```\n\n")
		}
		blocks = append(blocks, b.String())
	}

	// Added resources
	for _, man := range result.Added {
		writeBlock("➕", man.Key())
	}

	// Removed resources
	for _, man := range result.Removed {
		writeBlock("➖", man.Key())
	}

	// Modified resources
	for _, md := range result.Modified {
		writeBlock("📝", md.Key)
	}

	return blocks
}

// WriteTree writes the full application tree.
func (m *MarkdownWriter) WriteTree(tree *diff.AppTree) error {
	tree.Walk(func(node *diff.AppTreeNode, depth int) {
		if appDiff, ok := node.AppDiff.(*types.AppDiff); ok {
			_ = m.WriteAppDiff(appDiff, depth)
		}
	})
	return nil
}

// WriteSummary writes the summary.
func (m *MarkdownWriter) WriteSummary(summary Summary) error {
	m.buf = &m.summary
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
	m.buf = &m.footer
	m.write(fmt.Sprintf("\n---\n_Generated at %s by [argocdf](https://github.com/rgeraskin/argocdf)_\n", time.Now().Format(time.RFC3339)))
	return nil
}

// Flush assembles the buffered report into one or more part files and closes
// the writer. Part 1 goes to the configured path; parts 2+ become numbered
// siblings (<base>.2.<ext>, ...). Stale part files from a previous, larger run
// are removed so consumers never pick up outdated content.
func (m *MarkdownWriter) Flush() error {
	parts := m.assembleParts()

	// Part 1 goes into the eagerly-created file.
	m.baseFileWriter.write(parts[0])
	err := m.close()

	for i := 1; i < len(parts); i++ {
		if werr := os.WriteFile(partPath(m.path, i+1), []byte(parts[i]), 0o644); werr != nil && err == nil {
			err = fmt.Errorf("failed to write markdown part file: %w", werr)
		}
	}

	removeStaleParts(m.path, len(parts))

	return err
}

// assembleParts renders the buffered report. Without a split limit — or when
// everything fits in one part — it returns a single document identical to the
// pre-split output format. Otherwise each part is a self-contained document:
// upsert marker, "## <title> (part i/N)" heading, whole app sections (all
// <details> and code fences closed), with the summary and footer on the last
// part.
func (m *MarkdownWriter) assembleParts() []string {
	marker := CommentMarker(m.markerID) + "\n"
	title := html.EscapeString(m.title)
	heading := "## " + title + "\n\n"

	var whole strings.Builder
	whole.WriteString(marker)
	whole.WriteString(heading)
	for _, sec := range m.sections {
		whole.WriteString(sec.render())
	}
	whole.WriteString(m.summary.String())
	whole.WriteString(m.footer.String())

	if m.splitMax <= 0 || whole.Len() <= m.splitMax {
		return []string{whole.String()}
	}

	// Per-part body budget: reserve the marker and a part heading (digits
	// sized generously so the final part count never overflows the reserve).
	overhead := len(marker) + len(fmt.Sprintf("## %s (part %d/%d)\n\n", title, 9999, 9999))
	budget := m.splitMax - overhead
	if budget < 256 {
		budget = 256 // safety net for programmatic tiny limits
	}

	bodies := m.packBodies(budget)

	if len(bodies) == 0 {
		// Unreachable with real reports (an oversized report always packs into
		// at least one body), but Flush indexes parts[0]; never return empty.
		return []string{whole.String()}
	}

	if len(bodies) == 1 {
		// Truncation shrank the report into a single part after all; emit it
		// without a part heading.
		return []string{marker + heading + bodies[0]}
	}

	parts := make([]string, len(bodies))
	for i, body := range bodies {
		parts[i] = marker + fmt.Sprintf("## %s (part %d/%d)\n\n", title, i+1, len(bodies)) + body
	}
	return parts
}

// packBodies packs app sections into part bodies of at most budget bytes,
// keeping each app whole. An app that alone exceeds the budget gets its own
// part(s), split at resource-block boundaries. The summary and footer land on
// the last part, or on their own final part if they don't fit.
func (m *MarkdownWriter) packBodies(budget int) []string {
	var bodies []string
	cur := ""

	flush := func() {
		if cur != "" {
			bodies = append(bodies, cur)
			cur = ""
		}
	}

	for _, sec := range m.sections {
		full := sec.render()
		switch {
		case len(cur)+len(full) <= budget:
			cur += full
		case len(full) <= budget:
			flush()
			cur = full
		default:
			// The app alone exceeds a whole part: split it at resource-block
			// boundaries. Its last chunk stays open so following small apps
			// can share the part.
			flush()
			chunks := splitAppSection(sec, budget)
			bodies = append(bodies, chunks[:len(chunks)-1]...)
			cur = chunks[len(chunks)-1]
		}
	}

	tail := m.summary.String() + m.footer.String()
	if len(tail) > budget {
		// Unreachable with the writer's own bounded summary/footer, but keep
		// the every-body-fits contract local instead of relying on that.
		tail = truncateBlock(tail, budget)
	}
	if len(cur)+len(tail) <= budget {
		cur += tail
		flush()
	} else {
		flush()
		if tail != "" {
			bodies = append(bodies, tail)
		}
	}

	return bodies
}

// splitAppSection splits one oversized app section into part bodies at
// resource-block boundaries. Every chunk is a complete <details> block;
// continuation chunks reuse the header with "(continued)" in the app name and
// skip the notes. A single resource block that alone exceeds the budget is
// truncated in place rather than spread across parts.
func splitAppSection(sec appSection, budget int) []string {
	var chunks []string
	cur := sec.header + sec.notes
	// Notes count as closable payload too: when they nearly fill the chunk,
	// the first resource must move to a fresh continuation chunk instead of
	// being truncated against the leftover sliver.
	payload := sec.notes != ""

	// Degenerate case: the header and notes alone blow the budget (e.g. huge
	// parse-error lists). Cut the notes down so the chunk stays postable.
	if len(cur)+len(sec.footer) > budget {
		cur = truncateBlock(cur, budget-len(sec.footer))
	}

	for _, res := range sec.resources {
		if len(cur)+len(res)+len(sec.footer) > budget {
			if payload {
				chunks = append(chunks, cur+sec.footer)
				cur = sec.headerCont
			}
			if avail := budget - len(cur) - len(sec.footer); len(res) > avail {
				// One resource larger than a whole part: truncate it rather
				// than spreading a single resource across comments.
				res = truncateBlock(res, avail)
			}
		}
		cur += res
		payload = true
	}

	chunks = append(chunks, cur+sec.footer)
	return chunks
}

// truncateBlock hard-cuts a markdown block to at most max bytes at a line
// boundary, closes a code fence left open by the cut, and appends a truncation
// note, so the result stays valid standalone markdown. The result never
// exceeds max: when max is too small for the full note, a short note is used,
// and when even that doesn't fit, the block is dropped entirely.
func truncateBlock(block string, max int) string {
	const note = "\n> ⚠️ Diff truncated: it exceeds the size limit of a single part. Run argocdf locally for the full diff.\n\n"
	const shortNote = "\n> ⚠️ truncated\n\n"
	const fenceClose = "```\n"

	if max < len(shortNote) {
		return ""
	}
	n := note
	if max < len(n)+len(fenceClose) {
		n = shortNote
	}

	keep := max - len(n) - len(fenceClose)
	if keep < 0 {
		keep = 0
	}
	if keep > len(block) {
		keep = len(block)
	}

	kept := block[:keep]
	if i := strings.LastIndexByte(kept, '\n'); i >= 0 {
		kept = kept[:i+1]
	} else {
		kept = ""
	}

	// Generated fences always sit at column 0; an odd count means the cut
	// left one open.
	fences := 0
	for _, line := range strings.Split(kept, "\n") {
		if strings.HasPrefix(line, "```") {
			fences++
		}
	}
	if fences%2 == 1 {
		kept += fenceClose
	}

	return kept + n
}

// partPath returns the file path for part i (i >= 2): report.md -> report.2.md.
func partPath(path string, i int) string {
	ext := filepath.Ext(path)
	return fmt.Sprintf("%s.%d%s", strings.TrimSuffix(path, ext), i, ext)
}

// removeStaleParts deletes part files above keep left over from a previous,
// larger run. Best-effort: parts are always contiguous, so it stops at the
// first missing file.
func removeStaleParts(path string, keep int) {
	i := keep + 1
	if i < 2 {
		i = 2
	}
	for ; ; i++ {
		if err := os.Remove(partPath(path, i)); err != nil {
			return
		}
	}
}
