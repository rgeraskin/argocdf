package output

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rgeraskin/argocdf/internal/diff"
	"github.com/rgeraskin/argocdf/internal/types"
)

// splitTestApp builds an app diff with the given number of added resources,
// each carrying a raw manifest of roughly rawSize bytes.
func splitTestApp(name string, resources, rawSize int) *types.AppDiff {
	var added []diff.Manifest
	for i := 0; i < resources; i++ {
		added = append(added, diff.Manifest{
			APIVersion: "v1",
			Kind:       "ConfigMap",
			Name:       fmt.Sprintf("%s-cm-%d", name, i),
			Raw: fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\ndata:\n  blob: %s\n",
				strings.Repeat("x", rawSize)),
		})
	}
	return &types.AppDiff{
		Name:       name,
		DiffResult: &diff.ManifestSetDiff{HasChanges: true, Added: added},
	}
}

// renderSplitReport runs the full WriteHeader→Flush cycle and returns the
// contents of all produced part files, part 1 first.
func renderSplitReport(t *testing.T, path string, format MarkdownFormat, splitMax int, apps []*types.AppDiff) []string {
	t.Helper()

	w, err := NewMarkdownWriter(path, format, 3)
	if err != nil {
		t.Fatalf("NewMarkdownWriter() error = %v", err)
	}
	w.SetSplitMax(splitMax)

	if err := w.WriteHeader("ArgoCD Diff: main → feature"); err != nil {
		t.Fatalf("WriteHeader() error = %v", err)
	}
	for _, a := range apps {
		if err := w.WriteAppDiff(a, 0); err != nil {
			t.Fatalf("WriteAppDiff() error = %v", err)
		}
	}
	if err := w.WriteSummary(ComputeSummary(apps)); err != nil {
		t.Fatalf("WriteSummary() error = %v", err)
	}
	if err := w.WriteFooter(); err != nil {
		t.Fatalf("WriteFooter() error = %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}

	return readParts(t, path)
}

// readParts reads part 1 from path and every contiguous numbered sibling.
func readParts(t *testing.T, path string) []string {
	t.Helper()

	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading part 1: %v", err)
	}
	parts := []string{string(first)}
	for i := 2; ; i++ {
		content, err := os.ReadFile(partPath(path, i))
		if err != nil {
			break
		}
		parts = append(parts, string(content))
	}
	return parts
}

// normalizeTimestamp makes footers from separate runs comparable.
func normalizeTimestamp(s string) string {
	return regexp.MustCompile(`_Generated at \S+ by`).ReplaceAllString(s, "_Generated at TS by")
}

// assertBalanced checks that a part is self-contained markdown: every
// <details> closed and an even number of code fences.
func assertBalanced(t *testing.T, part string) {
	t.Helper()

	opens := strings.Count(part, "<details>")
	closes := strings.Count(part, "</details>")
	if opens != closes {
		t.Errorf("unbalanced <details>: %d opens, %d closes", opens, closes)
	}

	fences := 0
	for _, line := range strings.Split(part, "\n") {
		if strings.HasPrefix(line, "```") {
			fences++
		}
	}
	if fences%2 != 0 {
		t.Errorf("odd number of code fences: %d", fences)
	}
}

// stripPartPreamble removes the marker line and part heading, returning the body.
func stripPartPreamble(t *testing.T, part string) string {
	t.Helper()

	i := strings.Index(part, "\n\n")
	if i < 0 {
		t.Fatalf("part has no heading separator: %q", part[:min(len(part), 80)])
	}
	return part[i+2:]
}

func TestMarkdownWriter_Split_FitsInOnePart(t *testing.T) {
	tempDir := t.TempDir()
	apps := []*types.AppDiff{splitTestApp("app-a", 2, 100), splitTestApp("app-b", 1, 100)}

	unsplit := renderSplitReport(t, filepath.Join(tempDir, "plain.md"), MarkdownFormatGitHub, 0, apps)
	split := renderSplitReport(t, filepath.Join(tempDir, "split.md"), MarkdownFormatGitHub, 60000, apps)

	if len(split) != 1 {
		t.Fatalf("expected 1 part, got %d", len(split))
	}
	if normalizeTimestamp(split[0]) != normalizeTimestamp(unsplit[0]) {
		t.Errorf("split output with fitting report is not identical to unsplit output:\n--- unsplit ---\n%s\n--- split ---\n%s",
			unsplit[0], split[0])
	}
	if strings.Contains(split[0], "(part ") {
		t.Errorf("single-part output must not carry a part heading")
	}
}

func TestMarkdownWriter_Split_MultiPart(t *testing.T) {
	for _, format := range []MarkdownFormat{MarkdownFormatGitHub, MarkdownFormatAtlantis} {
		t.Run(string(format), func(t *testing.T) {
			tempDir := t.TempDir()
			const limit = 3000

			var apps []*types.AppDiff
			for i := 0; i < 8; i++ {
				apps = append(apps, splitTestApp(fmt.Sprintf("app-%d", i), 1, 600))
			}

			path := filepath.Join(tempDir, "pr-comment.md")
			parts := renderSplitReport(t, path, format, limit, apps)

			if len(parts) < 2 {
				t.Fatalf("expected multiple parts, got %d", len(parts))
			}

			marker := CommentMarker("")
			for i, part := range parts {
				if len(part) > limit {
					t.Errorf("part %d exceeds limit: %d > %d", i+1, len(part), limit)
				}
				if !strings.HasPrefix(part, marker+"\n") {
					t.Errorf("part %d does not start with the upsert marker", i+1)
				}
				heading := fmt.Sprintf("(part %d/%d)", i+1, len(parts))
				if !strings.Contains(part, heading) {
					t.Errorf("part %d missing heading %q", i+1, heading)
				}
				assertBalanced(t, part)
			}

			// No app may span two parts, and none may be dropped.
			all := strings.Join(parts, "")
			for i := range apps {
				name := fmt.Sprintf("<b>app-%d</b>", i)
				if got := strings.Count(all, name); got != 1 {
					t.Errorf("app-%d appears %d times across parts, want exactly 1", i, got)
				}
			}

			// Summary and footer land on the last part only.
			for i, part := range parts {
				hasSummary := strings.Contains(part, "**Summary:**")
				if hasSummary != (i == len(parts)-1) {
					t.Errorf("part %d summary presence = %v, want it on the last part only", i+1, hasSummary)
				}
			}

			// Stripping marker and part headings must reassemble the unsplit body.
			unsplit := renderSplitReport(t, filepath.Join(tempDir, "ref.md"), format, 0, apps)
			var reassembled strings.Builder
			for _, part := range parts {
				reassembled.WriteString(stripPartPreamble(t, part))
			}
			want := stripPartPreamble(t, unsplit[0])
			if normalizeTimestamp(reassembled.String()) != normalizeTimestamp(want) {
				t.Errorf("reassembled parts differ from unsplit report")
			}
		})
	}
}

func TestMarkdownWriter_Split_GiantApp(t *testing.T) {
	tempDir := t.TempDir()
	const limit = 3000

	apps := []*types.AppDiff{
		splitTestApp("small-before", 1, 200),
		splitTestApp("giant", 10, 600), // ~6KB section, alone exceeds the limit
		splitTestApp("small-after", 1, 200),
	}

	path := filepath.Join(tempDir, "pr-comment.md")
	parts := renderSplitReport(t, path, MarkdownFormatGitHub, limit, apps)

	if len(parts) < 3 {
		t.Fatalf("expected the giant app to span multiple parts, got %d parts", len(parts))
	}

	all := strings.Join(parts, "")
	for i, part := range parts {
		if len(part) > limit {
			t.Errorf("part %d exceeds limit: %d > %d", i+1, len(part), limit)
		}
		assertBalanced(t, part)
	}

	// Continuation chunks reuse the app header with "(continued)".
	if !strings.Contains(all, "<b>giant (continued)</b>") {
		t.Errorf("expected a continued header for the split app")
	}

	// Every resource block survives intact, exactly once, nothing truncated.
	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("giant-cm-%d", i)
		if got := strings.Count(all, "#### ➕ "+"ConfigMap/"+key); got != 1 {
			t.Errorf("resource %s appears %d times, want exactly 1", key, got)
		}
	}
	if strings.Contains(all, "truncated") {
		t.Errorf("no resource should be truncated when each fits a part")
	}

	// Small neighbors stay whole and un-continued.
	for _, name := range []string{"small-before", "small-after"} {
		if got := strings.Count(all, "<b>"+name+"</b>"); got != 1 {
			t.Errorf("app %s appears %d times, want exactly 1", name, got)
		}
	}
}

func TestMarkdownWriter_Split_GiantResource(t *testing.T) {
	tempDir := t.TempDir()
	const limit = 3000

	apps := []*types.AppDiff{
		splitTestApp("small", 1, 200),
		splitTestApp("huge-res", 1, 5000), // single resource block alone exceeds the limit
	}

	path := filepath.Join(tempDir, "pr-comment.md")
	parts := renderSplitReport(t, path, MarkdownFormatGitHub, limit, apps)

	all := strings.Join(parts, "")
	for i, part := range parts {
		if len(part) > limit {
			t.Errorf("part %d exceeds limit: %d > %d", i+1, len(part), limit)
		}
		assertBalanced(t, part)
	}

	if !strings.Contains(all, "Diff truncated") {
		t.Errorf("expected a truncation note for the oversized resource")
	}
	if got := strings.Count(all, "<b>huge-res</b>"); got != 1 {
		t.Errorf("oversized resource's app appears %d times, want exactly 1 (must not span parts)", got)
	}
}

func TestMarkdownWriter_Split_RemovesStaleParts(t *testing.T) {
	tempDir := t.TempDir()
	path := filepath.Join(tempDir, "pr-comment.md")

	// Leftovers from a previous, larger run.
	for i := 2; i <= 4; i++ {
		if err := os.WriteFile(partPath(path, i), []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	apps := []*types.AppDiff{splitTestApp("app-a", 1, 100)}
	parts := renderSplitReport(t, path, MarkdownFormatGitHub, 60000, apps)

	if len(parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(parts))
	}
	for i := 2; i <= 4; i++ {
		if _, err := os.Stat(partPath(path, i)); !os.IsNotExist(err) {
			t.Errorf("stale part %d was not removed", i)
		}
	}
}

func TestMarkdownWriter_Split_RemovesStalePartsAfterMultiPartRun(t *testing.T) {
	tempDir := t.TempDir()
	path := filepath.Join(tempDir, "pr-comment.md")

	// Leftovers from a previous run that produced 5 parts.
	for i := 2; i <= 5; i++ {
		if err := os.WriteFile(partPath(path, i), []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// This run produces at least 2 parts; parts it writes are overwritten,
	// parts beyond its count are removed.
	var apps []*types.AppDiff
	for i := 0; i < 6; i++ {
		apps = append(apps, splitTestApp(fmt.Sprintf("app-%d", i), 1, 600))
	}
	parts := renderSplitReport(t, path, MarkdownFormatGitHub, 3000, apps)

	if len(parts) < 2 || len(parts) >= 5 {
		t.Fatalf("test needs 2-4 parts to be meaningful, got %d", len(parts))
	}
	for i := 2; i <= len(parts); i++ {
		content, err := os.ReadFile(partPath(path, i))
		if err != nil {
			t.Fatalf("part %d missing: %v", i, err)
		}
		if string(content) == "stale" {
			t.Errorf("part %d was not overwritten", i)
		}
	}
	for i := len(parts) + 1; i <= 5; i++ {
		if _, err := os.Stat(partPath(path, i)); !os.IsNotExist(err) {
			t.Errorf("stale part %d was not removed", i)
		}
	}
}

// fabSection builds an appSection with a notes blob and resource blocks of
// controlled sizes, for exercising the packing functions directly.
func fabSection(name string, notesSize int, resSizes ...int) appSection {
	sec := appSection{
		header:     "<details>\n<summary><b>" + name + "</b></summary>\n\n",
		headerCont: "<details>\n<summary><b>" + name + " (continued)</b></summary>\n\n",
		footer:     "</details>\n\n",
	}
	if notesSize > 0 {
		sec.notes = "> " + strings.Repeat("n", notesSize) + "\n\n"
	}
	for _, s := range resSizes {
		sec.resources = append(sec.resources,
			"#### ➕ "+name+"\n\n```yaml\n"+strings.Repeat("x", s)+"\n```\n\n")
	}
	return sec
}

func TestPackBodies_TailGetsOwnPart(t *testing.T) {
	sec := fabSection("app", 0, 700)
	budget := len(sec.render()) + 50 // no room for the tail after the app

	m := &MarkdownWriter{sections: []appSection{sec}}
	m.summary.WriteString(strings.Repeat("s", 100) + "\n")
	m.footer.WriteString(strings.Repeat("f", 100) + "\n")
	tail := m.summary.String() + m.footer.String()

	bodies := m.packBodies(budget)
	if len(bodies) != 2 {
		t.Fatalf("expected 2 bodies (app, tail), got %d", len(bodies))
	}
	for i, b := range bodies {
		if len(b) > budget {
			t.Errorf("body %d exceeds budget: %d > %d", i, len(b), budget)
		}
	}
	if bodies[1] != tail {
		t.Errorf("last body should be exactly the summary+footer tail")
	}
}

func TestPackBodies_OversizedTailTruncated(t *testing.T) {
	const budget = 400
	sec := fabSection("app", 0, 100)

	m := &MarkdownWriter{sections: []appSection{sec}}
	m.summary.WriteString("---\n\n**Summary:** " + strings.Repeat("s\n", 500))
	m.footer.WriteString("\n---\n_Generated at TS by argocdf_\n")

	bodies := m.packBodies(budget)
	for i, b := range bodies {
		if len(b) > budget {
			t.Errorf("body %d exceeds budget: %d > %d", i, len(b), budget)
		}
	}
	all := strings.Join(bodies, "")
	if !strings.Contains(all, "Diff truncated") {
		t.Errorf("oversized tail should carry a truncation note")
	}
}

func TestSplitAppSection_MultipleGiantResources(t *testing.T) {
	const budget = 500
	sec := fabSection("giant", 0, 800, 800, 800) // every resource alone exceeds the budget

	chunks := splitAppSection(sec, budget)
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks (one truncated resource each), got %d", len(chunks))
	}
	for i, c := range chunks {
		if len(c) > budget {
			t.Errorf("chunk %d exceeds budget: %d > %d", i, len(c), budget)
		}
		if !strings.Contains(c, "Diff truncated") {
			t.Errorf("chunk %d missing truncation note", i)
		}
		assertBalanced(t, c)
		wantHeader := sec.header
		if i > 0 {
			wantHeader = sec.headerCont
		}
		if !strings.HasPrefix(c, wantHeader) {
			t.Errorf("chunk %d has wrong header", i)
		}
	}
}

// Regression: notes nearly filling the first chunk must not squeeze the first
// resource into the leftover sliver (which both truncated it needlessly and
// could overflow the budget when the sliver was smaller than the truncation
// note). The notes-only chunk closes and the resource starts a continuation.
func TestSplitAppSection_NotesThenResource(t *testing.T) {
	const budget = 1000
	sec := fabSection("app", 850, 500) // header+notes ~900, resource block ~530

	chunks := splitAppSection(sec, budget)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks (notes, continued resource), got %d", len(chunks))
	}
	for i, c := range chunks {
		if len(c) > budget {
			t.Errorf("chunk %d exceeds budget: %d > %d", i, len(c), budget)
		}
		assertBalanced(t, c)
	}
	all := strings.Join(chunks, "")
	if strings.Contains(all, "Diff truncated") {
		t.Errorf("resource fits a fresh chunk and must not be truncated")
	}
	if !strings.Contains(chunks[0], sec.notes) {
		t.Errorf("first chunk should carry the notes intact")
	}
	if !strings.Contains(chunks[1], sec.resources[0]) {
		t.Errorf("second chunk should carry the whole resource block")
	}
}

func TestTruncateBlock(t *testing.T) {
	const max = 300
	block := "#### ➕ X\n\n```yaml\n" + strings.Repeat("aaaa\n", 100) + "```\n\n"

	got := truncateBlock(block, max)
	if len(got) > max {
		t.Errorf("truncated block exceeds max: %d > %d", len(got), max)
	}
	if !strings.Contains(got, "Diff truncated") {
		t.Errorf("missing truncation note")
	}
	assertBalanced(t, got)

	// The cut must land on a line boundary: every kept line is one of the
	// original complete lines (or part of the appended note).
	for _, line := range strings.Split(got, "\n") {
		switch line {
		case "#### ➕ X", "", "```yaml", "aaaa", "```":
		default:
			if !strings.HasPrefix(line, "> ⚠️") {
				t.Errorf("truncation cut mid-line: %q", line)
			}
		}
	}
}

func TestTruncateBlock_NeverExceedsMax(t *testing.T) {
	block := "#### ➕ X\n\n```yaml\n" + strings.Repeat("aaaa\n", 100) + "```\n\n"
	for _, max := range []int{0, 5, 20, 25, 60, 111, 200} {
		got := truncateBlock(block, max)
		if len(got) > max {
			t.Errorf("truncateBlock(max=%d) returned %d bytes", max, len(got))
		}
		assertBalanced(t, got)
	}
}

// A header so large it leaves a resource less room than even the truncation
// note (avail < len(note)) must still yield chunks within budget: the resource
// degrades to a short note or is dropped, but the part stays postable.
func TestSplitAppSection_LongHeaderTinyBudget(t *testing.T) {
	const budget = 256
	sec := fabSection(strings.Repeat("n", 180), 0, 300, 300)

	chunks := splitAppSection(sec, budget)
	for i, c := range chunks {
		if len(c) > budget {
			t.Errorf("chunk %d exceeds budget: %d > %d", i, len(c), budget)
		}
		assertBalanced(t, c)
	}
}

func TestMarkdownWriter_Split_MixedAppsAndCustomMarker(t *testing.T) {
	tempDir := t.TempDir()
	const limit = 3000

	apps := []*types.AppDiff{
		{Name: "broken", Error: fmt.Errorf("helm template failed: %s", strings.Repeat("e", 100))},
		splitTestApp("app-a", 1, 600),
		{Name: "unchanged", DiffResult: &diff.ManifestSetDiff{HasChanges: false}},
		splitTestApp("app-b", 1, 600),
		splitTestApp("app-c", 1, 600),
		splitTestApp("app-d", 1, 600),
	}

	path := filepath.Join(tempDir, "pr-comment.md")

	w, err := NewMarkdownWriter(path, MarkdownFormatGitHub, 3)
	if err != nil {
		t.Fatalf("NewMarkdownWriter() error = %v", err)
	}
	w.SetMarker("ci")
	w.SetSplitMax(limit)
	if err := w.WriteHeader("ArgoCD Diff: main → feature"); err != nil {
		t.Fatal(err)
	}
	for _, a := range apps {
		if err := w.WriteAppDiff(a, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.WriteSummary(ComputeSummary(apps)); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteFooter(); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}

	parts := readParts(t, path)
	if len(parts) < 2 {
		t.Fatalf("expected multiple parts, got %d", len(parts))
	}

	for i, part := range parts {
		if len(part) > limit {
			t.Errorf("part %d exceeds limit: %d > %d", i+1, len(part), limit)
		}
		if !strings.HasPrefix(part, CommentMarker("ci")+"\n") {
			t.Errorf("part %d does not start with the scoped marker", i+1)
		}
		assertBalanced(t, part)
	}

	// Error and no-changes apps survive whole, exactly once.
	all := strings.Join(parts, "")
	if got := strings.Count(all, "helm template failed"); got != 1 {
		t.Errorf("error app body appears %d times, want 1", got)
	}
	if got := strings.Count(all, "_No changes_"); got != 1 {
		t.Errorf("no-changes app body appears %d times, want 1", got)
	}
}

func TestPartPath(t *testing.T) {
	tests := []struct {
		path string
		i    int
		want string
	}{
		{"pr-comment.md", 2, "pr-comment.2.md"},
		{"/tmp/out/diff.md", 3, "/tmp/out/diff.3.md"},
		{"report", 2, "report.2"},
	}
	for _, tt := range tests {
		if got := partPath(tt.path, tt.i); got != tt.want {
			t.Errorf("partPath(%q, %d) = %q, want %q", tt.path, tt.i, got, tt.want)
		}
	}
}
