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

// PersistentWriter is implemented by writers that persist their output to a
// file consumed after the run finishes (e.g. a PR-comment markdown file read by
// CI). For these, a "no applications affected" report is still worth emitting so
// the file is self-describing and its upsert marker is present. Ephemeral
// writers (the terminal) deliberately do not implement it: the run already logs
// the empty result to the console, so stdout stays quiet in that case.
//
// The marker method is unexported, so only writers in this package can be
// persistent; callers use the interface solely to classify writers.
type PersistentWriter interface {
	Writer
	persistent()
}

// FileOnly returns a Writer that fans out only to the persistent (file-backed)
// writers within w, unwrapping a MultiWriter. If w has no persistent writers it
// returns a NullWriter, so callers can write unconditionally without producing
// terminal output. It is used for the no-applications-affected report, which
// must reach files (marker + self-describing body) but must not touch stdout.
func FileOnly(w Writer) Writer {
	var persistent []Writer
	if mw, ok := w.(*MultiWriter); ok {
		for _, sub := range mw.writers {
			if _, ok := sub.(PersistentWriter); ok {
				persistent = append(persistent, sub)
			}
		}
	} else if _, ok := w.(PersistentWriter); ok {
		persistent = append(persistent, w)
	}

	switch len(persistent) {
	case 0:
		return NewNullWriter()
	case 1:
		return persistent[0]
	default:
		return NewMultiWriter(persistent...)
	}
}

// Summary contains summary information about the diff.
type Summary struct {
	TotalApps       int
	AppsWithChanges int
	AppsWithErrors  int // Includes app-level errors AND YAML parse errors
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
// It aggregates statistics including:
//   - Total number of applications processed
//   - Number of applications with changes, errors, or no changes
//   - Total resources added, removed, and modified across all apps
//   - Count of new Application CRDs (for apps-of-apps pattern detection)
//
// Applications with errors are counted separately and don't contribute to
// resource change counts.
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

		// Count apps with parse errors (e.g., duplicate YAML keys) as errored apps
		if len(result.ParseErrors) > 0 {
			summary.AppsWithErrors++
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
