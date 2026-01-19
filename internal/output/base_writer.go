// Package output provides base writer functionality.
package output

import (
	"io"
	"os"
)

// baseFileWriter provides common file writing functionality.
// Embed this in concrete writer types to avoid duplicating file operations.
type baseFileWriter struct {
	file *os.File
}

// write is a helper to write strings to the file.
func (b *baseFileWriter) write(s string) {
	io.WriteString(b.file, s)
}

// close closes the underlying file.
func (b *baseFileWriter) close() error {
	return b.file.Close()
}
