// Package render provides tests for path validation utilities.
package render

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestValidatePathContainment(t *testing.T) {
	// Create a temp directory for testing
	tempDir, err := os.MkdirTemp("", "pathutil-test-")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(tempDir)
	}()

	// Create subdirectory
	subDir := filepath.Join(tempDir, "subdir")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("failed to create subdir: %v", err)
	}

	tests := []struct {
		name       string
		base       string
		resolved   string
		wantErr    bool
		errContain string
	}{
		{
			name:     "valid path within base",
			base:     tempDir,
			resolved: filepath.Join(tempDir, "file.txt"),
			wantErr:  false,
		},
		{
			name:     "valid path in subdirectory",
			base:     tempDir,
			resolved: filepath.Join(subDir, "file.txt"),
			wantErr:  false,
		},
		{
			name:     "path equals base",
			base:     tempDir,
			resolved: tempDir,
			wantErr:  false,
		},
		{
			name:       "path escapes via parent traversal",
			base:       tempDir,
			resolved:   filepath.Join(tempDir, "..", "etc", "passwd"),
			wantErr:    true,
			errContain: "path traversal",
		},
		{
			name:       "absolute path outside base",
			base:       tempDir,
			resolved:   "/etc/passwd",
			wantErr:    true,
			errContain: "path traversal",
		},
		{
			name:       "path with multiple parent traversals",
			base:       subDir,
			resolved:   filepath.Join(subDir, "..", "..", "etc", "passwd"),
			wantErr:    true,
			errContain: "path traversal",
		},
		{
			name:     "path with normalized dots staying inside",
			base:     tempDir,
			resolved: filepath.Join(subDir, "..", "other.txt"),
			wantErr:  false,
		},
		{
			name:       "similar prefix but different directory",
			base:       tempDir,
			resolved:   tempDir + "evil", // e.g., /tmp/test -> /tmp/testevil
			wantErr:    true,
			errContain: "path traversal",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePathContainment(tt.base, tt.resolved)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ValidatePathContainment() error = nil, want error containing %q", tt.errContain)
					return
				}
				if !errors.Is(err, ErrPathTraversal) {
					t.Errorf("ValidatePathContainment() error = %v, want ErrPathTraversal", err)
				}
			} else {
				if err != nil {
					t.Errorf("ValidatePathContainment() unexpected error = %v", err)
				}
			}
		})
	}
}

func TestSafeRemoveAll(t *testing.T) {
	t.Run("removes valid temp directory", func(t *testing.T) {
		// Create a directory in temp
		tempDir, err := os.MkdirTemp("", "safe-remove-test-")
		if err != nil {
			t.Fatalf("failed to create temp dir: %v", err)
		}

		// Create a file inside
		testFile := filepath.Join(tempDir, "test.txt")
		if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
			_ = os.RemoveAll(tempDir)
			t.Fatalf("failed to create test file: %v", err)
		}

		// SafeRemoveAll should succeed
		if err := SafeRemoveAll(tempDir); err != nil {
			t.Errorf("SafeRemoveAll() error = %v, want nil", err)
		}

		// Verify it's removed
		if _, err := os.Stat(tempDir); !os.IsNotExist(err) {
			t.Error("SafeRemoveAll() did not remove the directory")
			_ = os.RemoveAll(tempDir) // cleanup
		}
	})

	t.Run("empty path returns nil", func(t *testing.T) {
		if err := SafeRemoveAll(""); err != nil {
			t.Errorf("SafeRemoveAll(\"\") error = %v, want nil", err)
		}
	})

	t.Run("rejects path outside temp directory", func(t *testing.T) {
		// This should fail because /etc is not in temp
		err := SafeRemoveAll("/etc")
		if err == nil {
			t.Error("SafeRemoveAll(/etc) should have failed")
			return
		}
		if !errors.Is(err, ErrInvalidTempDir) {
			t.Errorf("SafeRemoveAll() error = %v, want ErrInvalidTempDir", err)
		}
	})
}

func TestResolveAndValidatePath(t *testing.T) {
	// Create a temp directory for testing
	tempDir, err := os.MkdirTemp("", "resolve-test-")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(tempDir)
	}()

	// Create subdirectory for testing
	subDir := filepath.Join(tempDir, "subdir")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("failed to create subdir: %v", err)
	}

	tests := []struct {
		name       string
		base       string
		path       string
		wantErr    bool
		wantSuffix string // expected suffix of the resolved path
	}{
		{
			name:       "relative path resolved correctly",
			base:       tempDir,
			path:       "subdir/file.txt",
			wantErr:    false,
			wantSuffix: "subdir/file.txt",
		},
		{
			name:       "simple filename",
			base:       tempDir,
			path:       "file.txt",
			wantErr:    false,
			wantSuffix: "file.txt",
		},
		{
			name:    "path traversal blocked",
			base:    tempDir,
			path:    "../../../etc/passwd",
			wantErr: true,
		},
		{
			name:    "absolute path outside base blocked",
			base:    tempDir,
			path:    "/etc/passwd",
			wantErr: true,
		},
		{
			name:       "path with dots that stays inside",
			base:       tempDir,
			path:       "subdir/../other.txt",
			wantErr:    false,
			wantSuffix: "other.txt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolved, err := ResolveAndValidatePath(tt.base, tt.path)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ResolveAndValidatePath() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Errorf("ResolveAndValidatePath() unexpected error = %v", err)
				return
			}
			if tt.wantSuffix != "" && !hasPathSuffix(resolved, tt.wantSuffix) {
				t.Errorf("ResolveAndValidatePath() = %q, want suffix %q", resolved, tt.wantSuffix)
			}
		})
	}
}

// hasPathSuffix checks if path ends with the given suffix (using filepath separator)
func hasPathSuffix(path, suffix string) bool {
	// Normalize both to use the same separator
	normPath := filepath.Clean(path)
	normSuffix := filepath.Clean(suffix)
	return len(normPath) >= len(normSuffix) &&
		normPath[len(normPath)-len(normSuffix):] == normSuffix
}

func TestValidatePathContainment_Symlinks(t *testing.T) {
	// Skip on Windows as symlink behavior differs
	if os.Getenv("SKIP_SYMLINK_TESTS") != "" {
		t.Skip("skipping symlink tests")
	}

	// Create temp directories
	tempDir, err := os.MkdirTemp("", "symlink-test-")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(tempDir)
	}()

	// Create outside directory
	outsideDir, err := os.MkdirTemp("", "outside-test-")
	if err != nil {
		t.Fatalf("failed to create outside dir: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(outsideDir)
	}()

	// Create a file in the outside directory
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0644); err != nil {
		t.Fatalf("failed to create outside file: %v", err)
	}

	// Create a symlink pointing outside
	symlinkPath := filepath.Join(tempDir, "escape-link")
	if err := os.Symlink(outsideDir, symlinkPath); err != nil {
		t.Skipf("cannot create symlink (might be permission issue): %v", err)
	}

	t.Run("symlink escaping base is blocked", func(t *testing.T) {
		resolvedViaSymlink := filepath.Join(symlinkPath, "secret.txt")
		err := ValidatePathContainment(tempDir, resolvedViaSymlink)
		if err == nil {
			t.Error("ValidatePathContainment() should block symlink escape")
		}
	})
}
