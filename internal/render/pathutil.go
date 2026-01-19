// Package render provides path validation utilities.
package render

import (
	"errors"
	"fmt"
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
