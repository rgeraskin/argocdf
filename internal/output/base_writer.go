// Package output provides base writer functionality.
package output

import (
	"fmt"
	"io"
	"os"
)

// baseFileWriter provides common file writing functionality.
// Embed this in concrete writer types to avoid duplicating file operations.
type baseFileWriter struct {
	file *os.File
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
func (b *baseFileWriter) write(s string) {
	io.WriteString(b.file, s)
}

// close closes the underlying file.
func (b *baseFileWriter) close() error {
	return b.file.Close()
}
