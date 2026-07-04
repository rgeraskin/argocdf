// Package output provides base writer functionality.
package output

import (
	"fmt"
	"io"
	"os"
)

// baseFileWriter provides common file writing functionality.
// Embed this in concrete writer types to avoid duplicating file operations.
// Write errors are accumulated and returned when Close() is called.
type baseFileWriter struct {
	file     *os.File
	writeErr error // First write error encountered
}

// newBaseFileWriter creates a new baseFileWriter for the given file path.
// fileType is used in the error message to describe what kind of file (e.g., "HTML", "markdown").
func newBaseFileWriter(filePath, fileType string) (baseFileWriter, error) {
	file, err := os.Create(filePath)
	if err != nil {
		return baseFileWriter{}, fmt.Errorf("failed to create %s file: %w", fileType, err)
	}
	return baseFileWriter{file: file}, nil
}

// write is a helper to write strings to the file.
// Errors are accumulated and returned when close() is called.
// This allows callers to write without checking errors on each call.
func (b *baseFileWriter) write(s string) {
	if b.writeErr != nil {
		return // Stop writing after first error
	}
	_, err := io.WriteString(b.file, s)
	if err != nil {
		b.writeErr = err
	}
}

// persistent marks every file-backed writer as a PersistentWriter (see the
// interface in output.go). It carries no behavior; its presence is the signal.
func (b *baseFileWriter) persistent() {}

// close closes the underlying file and returns any write errors that occurred.
// Returns the first write error if any, otherwise returns the close error.
func (b *baseFileWriter) close() error {
	closeErr := b.file.Close()
	if b.writeErr != nil {
		return fmt.Errorf("write error: %w", b.writeErr)
	}
	return closeErr
}
