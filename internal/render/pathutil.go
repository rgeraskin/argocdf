// Package render provides path validation utilities.
package render

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrPathTraversal indicates an attempt to access a path outside the allowed directory.
var ErrPathTraversal = errors.New("path traversal detected: resolved path escapes allowed directory")

// ErrInvalidTempDir indicates the path is not within the system temp directory.
var ErrInvalidTempDir = errors.New("path is not within the system temp directory")

// ValidatePathContainment ensures that a resolved path stays within the base directory.
// It resolves symlinks and normalizes both paths before comparison.
// Returns an error if the resolved path escapes the base directory.
func ValidatePathContainment(base, resolved string) error {
	// Get absolute paths
	absBase, err := filepath.Abs(base)
	if err != nil {
		return fmt.Errorf("failed to get absolute path for base: %w", err)
	}

	absResolved, err := filepath.Abs(resolved)
	if err != nil {
		return fmt.Errorf("failed to get absolute path for resolved: %w", err)
	}

	// Clean both paths to normalize . and .. components
	cleanBase := filepath.Clean(absBase)
	cleanResolved := filepath.Clean(absResolved)

	// If both paths exist, resolve symlinks to handle cases like /var -> /private/var on macOS
	baseExists := false
	if _, err := os.Stat(cleanBase); err == nil {
		baseExists = true
		if evaled, err := filepath.EvalSymlinks(cleanBase); err == nil {
			cleanBase = evaled
		}
	}

	if _, err := os.Stat(cleanResolved); err == nil {
		if evaled, err := filepath.EvalSymlinks(cleanResolved); err == nil {
			cleanResolved = evaled
		}
	} else if baseExists {
		// If resolved path doesn't exist but base does, try to resolve the parent
		// This handles the case where we're checking a non-existent file within an existing directory
		// On macOS, /var/folders is symlinked to /private/var/folders
		parent := filepath.Dir(cleanResolved)
		if evaled, err := filepath.EvalSymlinks(parent); err == nil {
			cleanResolved = filepath.Join(evaled, filepath.Base(cleanResolved))
		}
	}

	// Ensure base ends with separator for proper prefix checking
	// This prevents /base/path matching /base/pathevil
	if !strings.HasSuffix(cleanBase, string(filepath.Separator)) {
		cleanBase += string(filepath.Separator)
	}

	// Check if resolved path starts with base path or equals the base directory
	// (after removing the trailing separator we added for prefix checking)
	baseWithoutSep := strings.TrimSuffix(cleanBase, string(filepath.Separator))
	if cleanResolved != baseWithoutSep && !strings.HasPrefix(cleanResolved, cleanBase) {
		return fmt.Errorf("%w: %q is outside %q", ErrPathTraversal, resolved, base)
	}

	return nil
}

// SafeRemoveAll removes a directory only if it is within the system temp directory.
// This prevents accidental removal of directories outside temp due to symlinks or path manipulation.
func SafeRemoveAll(path string) error {
	if path == "" {
		return nil
	}

	// Get the system temp directory
	tempDir := os.TempDir()

	// Validate that the path is within temp directory
	if err := ValidatePathContainment(tempDir, path); err != nil {
		return fmt.Errorf("%w: refusing to remove %q (temp dir: %q)", ErrInvalidTempDir, path, tempDir)
	}

	return os.RemoveAll(path)
}

// ResolveAndValidatePath resolves a path relative to base and validates it stays within allowed directory.
// Returns the resolved absolute path or an error if validation fails.
func ResolveAndValidatePath(allowedBase, pathToResolve string) (string, error) {
	// If the path is already absolute, just validate it
	var resolved string
	if filepath.IsAbs(pathToResolve) {
		resolved = pathToResolve
	} else {
		resolved = filepath.Join(allowedBase, pathToResolve)
	}

	// Validate the path stays within the allowed directory
	if err := ValidatePathContainment(allowedBase, resolved); err != nil {
		return "", err
	}

	return resolved, nil
}

// dirExists reports whether p exists and is a directory.
func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// copyDir recursively copies a directory tree.
// The .git directory is skipped (it can be large and is not needed for
// rendering). Symlinks are recreated as symlinks; if a symlink cannot be read
// or recreated it is skipped rather than failing the whole copy.
func copyDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("failed to read directory %s: %w", src, err)
	}

	for _, entry := range entries {
		// Skip the git metadata directory: it can be huge and is irrelevant
		// to rendering manifests.
		if entry.IsDir() && entry.Name() == ".git" {
			continue
		}

		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		// Handle symlinks explicitly: entry.IsDir() is false for a symlink to
		// a directory, so without this branch copyFile would try to io.Copy a
		// directory. Recreate the link when possible, otherwise skip it.
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(srcPath)
			if err != nil {
				// Skip unreadable symlink rather than failing the whole copy.
				continue
			}
			if err := os.Symlink(target, dstPath); err != nil {
				// Skip symlink we cannot recreate rather than failing.
				continue
			}
			continue
		}

		if entry.IsDir() {
			// Create the directory
			info, err := entry.Info()
			if err != nil {
				return fmt.Errorf("failed to get info for %s: %w", srcPath, err)
			}
			if err := os.MkdirAll(dstPath, info.Mode()); err != nil {
				return fmt.Errorf("failed to create directory %s: %w", dstPath, err)
			}
			// Recursively copy contents
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			// Copy the file
			if err := copyFile(srcPath, dstPath); err != nil {
				return err
			}
		}
	}

	return nil
}

// copyFile copies a single file.
func copyFile(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open source file %s: %w", src, err)
	}
	defer func() {
		_ = srcFile.Close()
	}()

	srcInfo, err := srcFile.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat source file %s: %w", src, err)
	}

	dstFile, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, srcInfo.Mode())
	if err != nil {
		return fmt.Errorf("failed to create destination file %s: %w", dst, err)
	}
	defer func() {
		_ = dstFile.Close()
	}()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return fmt.Errorf("failed to copy file content: %w", err)
	}

	return nil
}
