package output

import (
	"os"
	"strings"
	"testing"

	"github.com/rgeraskin/argocdf/internal/types"
)

// multiLineRenderError is the shape helm actually produces: a first line, then
// continuation lines, one of which starts with "- " — a deletion marker as far as
// any diff parser is concerned.
const multiLineRenderError = "failed to render target branch: values don't meet the specifications of the schema(s) in the following chart(s):\n" +
	"schema-app:\n" +
	"- at '/replicas': got string, want integer"

func TestPrefixLines(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		prefix string
		want   string
	}{
		{"single line", "boom", "# ", "# boom"},
		{"two lines", "a\nb", "# ", "# a\n# b"},
		// A blank line inside the message must keep the prefix - an unprefixed empty
		// line would terminate a markdown blockquote - but without trailing space,
		// so no report carries trailing whitespace.
		{"blank line keeps prefix, trimmed", "a\n\nb", "> ", "> a\n>\n> b"},
		{"blank line keeps prefix, unified", "a\n\nb", "# ", "# a\n#\n# b"},
		// The caller appends its own separator, so a trailing newline must not
		// produce a dangling prefix-only line.
		{"trailing newline trimmed", "a\n", "# ", "# a"},
		{"empty string", "", "# ", "#"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := prefixLines(tt.in, tt.prefix); got != tt.want {
				t.Errorf("prefixLines(%q, %q) = %q, want %q", tt.in, tt.prefix, got, tt.want)
			}
		})
	}
}

// TestUnifiedWriterCommentsEveryErrorLine pins that a render error cannot inject
// raw lines into a file advertised as patch-compatible.
func TestUnifiedWriterCommentsEveryErrorLine(t *testing.T) {
	path := tempReportPath(t, "unified-*.diff")

	w, err := NewUnifiedWriter(path, 3)
	if err != nil {
		t.Fatalf("NewUnifiedWriter() error = %v", err)
	}
	_ = w.WriteHeader("Test")
	if err := w.WriteAppDiff(&types.AppDiff{
		Name:      "schema-app",
		Namespace: "argocd",
		Error:     &testError{msg: multiLineRenderError},
	}, 0); err != nil {
		t.Fatalf("WriteAppDiff() error = %v", err)
	}
	_ = w.Flush()

	for _, line := range nonEmptyLines(t, path) {
		if !strings.HasPrefix(line, "#") {
			t.Errorf("uncommented line in unified output: %q", line)
		}
	}
}

// TestMarkdownWriterQuotesEveryErrorLine pins the same property for markdown,
// where an unprefixed continuation escapes the blockquote (and "- at ..." renders
// as a list item outside it).
func TestMarkdownWriterQuotesEveryErrorLine(t *testing.T) {
	for _, mode := range []struct {
		name   string
		format MarkdownFormat
	}{{"github", MarkdownFormatGitHub}, {"atlantis", MarkdownFormatAtlantis}} {
		t.Run(mode.name, func(t *testing.T) {
			path := tempReportPath(t, "md-*.md")

			w, err := NewMarkdownWriter(path, mode.format, 3)
			if err != nil {
				t.Fatalf("NewMarkdownWriter() error = %v", err)
			}
			_ = w.WriteHeader("Test")
			if err := w.WriteAppDiff(&types.AppDiff{
				Name:      "schema-app",
				Namespace: "argocd",
				Error:     &testError{msg: multiLineRenderError},
			}, 0); err != nil {
				t.Fatalf("WriteAppDiff() error = %v", err)
			}
			_ = w.Flush()

			content := readReport(t, path)
			if !strings.Contains(content, "> schema-app:") {
				t.Errorf("continuation line not quoted:\n%s", content)
			}
			if strings.Contains(content, "\n- at ") {
				t.Errorf("continuation line escaped the blockquote:\n%s", content)
			}
		})
	}
}

func tempReportPath(t *testing.T, pattern string) string {
	t.Helper()
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		t.Fatalf("CreateTemp() error = %v", err)
	}
	_ = f.Close()
	t.Cleanup(func() { _ = os.Remove(f.Name()) })
	return f.Name()
}

func readReport(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	return string(b)
}

func nonEmptyLines(t *testing.T, path string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(readReport(t, path), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}
