// Package output provides output formatting functionality.
package output

import (
	"github.com/rgeraskin/argocdf/internal/diff"
	"github.com/rgeraskin/argocdf/internal/types"
)

// Writer defines the interface for writing diff output.
type Writer interface {
	// WriteHeader writes the output header.
	WriteHeader(title string) error

	// WriteAppDiff writes the diff for a single application.
	WriteAppDiff(appDiff *types.AppDiff, depth int) error

	// WriteTree writes the full application tree.
	WriteTree(tree *diff.AppTree) error

	// WriteSummary writes a summary of all changes.
	WriteSummary(summary Summary) error

	// WriteFooter writes the output footer.
	WriteFooter() error

	// Flush ensures all output is written.
	Flush() error
}

// Summary contains summary information about the diff.
type Summary struct {
	TotalApps       int
	AppsWithChanges int
	AppsWithErrors  int
	TotalAdded      int
	TotalRemoved    int
	TotalModified   int
	NewApplications int
}

// NullWriter is a no-op writer that discards all output.
type NullWriter struct{}

// NewNullWriter creates a new NullWriter.
func NewNullWriter() *NullWriter {
	return &NullWriter{}
}

// WriteHeader is a no-op.
func (n *NullWriter) WriteHeader(title string) error { return nil }

// WriteAppDiff is a no-op.
func (n *NullWriter) WriteAppDiff(appDiff *types.AppDiff, depth int) error { return nil }

// WriteTree is a no-op.
func (n *NullWriter) WriteTree(tree *diff.AppTree) error { return nil }

// WriteSummary is a no-op.
func (n *NullWriter) WriteSummary(summary Summary) error { return nil }

// WriteFooter is a no-op.
func (n *NullWriter) WriteFooter() error { return nil }

// Flush is a no-op.
func (n *NullWriter) Flush() error { return nil }

// MultiWriter writes to multiple outputs simultaneously.
type MultiWriter struct {
	writers []Writer
}

// NewMultiWriter creates a new MultiWriter.
func NewMultiWriter(writers ...Writer) *MultiWriter {
	return &MultiWriter{writers: writers}
}

// WriteHeader writes the header to all writers.
func (m *MultiWriter) WriteHeader(title string) error {
	for _, w := range m.writers {
		if err := w.WriteHeader(title); err != nil {
			return err
		}
	}
	return nil
}

// WriteAppDiff writes the app diff to all writers.
func (m *MultiWriter) WriteAppDiff(appDiff *types.AppDiff, depth int) error {
	for _, w := range m.writers {
		if err := w.WriteAppDiff(appDiff, depth); err != nil {
			return err
		}
	}
	return nil
}

// WriteTree writes the tree to all writers.
func (m *MultiWriter) WriteTree(tree *diff.AppTree) error {
	for _, w := range m.writers {
		if err := w.WriteTree(tree); err != nil {
			return err
		}
	}
	return nil
}

// WriteSummary writes the summary to all writers.
func (m *MultiWriter) WriteSummary(summary Summary) error {
	for _, w := range m.writers {
		if err := w.WriteSummary(summary); err != nil {
			return err
		}
	}
	return nil
}

// WriteFooter writes the footer to all writers.
func (m *MultiWriter) WriteFooter() error {
	for _, w := range m.writers {
		if err := w.WriteFooter(); err != nil {
			return err
		}
	}
	return nil
}

// Flush flushes all writers.
func (m *MultiWriter) Flush() error {
	for _, w := range m.writers {
		if err := w.Flush(); err != nil {
			return err
		}
	}
	return nil
}

// ComputeSummary computes summary information from a list of app diffs.
func ComputeSummary(diffs []*types.AppDiff) Summary {
	summary := Summary{
		TotalApps: len(diffs),
	}

	for _, d := range diffs {
		if d.Error != nil {
			summary.AppsWithErrors++
			continue
		}

		// Type assert DiffResult
		result, ok := d.DiffResult.(*diff.ManifestSetDiff)
		if !ok || result == nil {
			continue
		}

		if result.HasChanges {
			summary.AppsWithChanges++
			summary.TotalAdded += len(result.Added)
			summary.TotalRemoved += len(result.Removed)
			summary.TotalModified += len(result.Modified)

			// Count new Application CRDs
			for _, added := range result.Added {
				if added.Kind == "Application" {
					summary.NewApplications++
				}
			}
		}
	}

	return summary
}
