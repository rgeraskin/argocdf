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

func TestCopyDirSymlink(t *testing.T) {
	srcDir := t.TempDir()

	// A regular file.
	if err := os.WriteFile(filepath.Join(srcDir, "real.txt"), []byte("hello"), 0644); err != nil {
		t.Fatalf("failed to write real.txt: %v", err)
	}

	// A subdirectory with a file, used as a symlink-to-directory target.
	targetDir := filepath.Join(srcDir, "targetdir")
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatalf("failed to create targetdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "inside.txt"), []byte("nested"), 0644); err != nil {
		t.Fatalf("failed to write inside.txt: %v", err)
	}

	// Symlink to a directory (the case that previously broke copyFile).
	if err := os.Symlink("targetdir", filepath.Join(srcDir, "dirlink")); err != nil {
		t.Fatalf("failed to create dir symlink: %v", err)
	}
	// Symlink to a file.
	if err := os.Symlink("real.txt", filepath.Join(srcDir, "filelink")); err != nil {
		t.Fatalf("failed to create file symlink: %v", err)
	}
	// Dangling symlink (target does not exist) must not fail the copy.
	if err := os.Symlink("does-not-exist", filepath.Join(srcDir, "dangling")); err != nil {
		t.Fatalf("failed to create dangling symlink: %v", err)
	}

	dstDir := t.TempDir()
	if err := copyDir(srcDir, dstDir); err != nil {
		t.Fatalf("copyDir() error = %v", err)
	}

	// Regular file copied.
	if data, err := os.ReadFile(filepath.Join(dstDir, "real.txt")); err != nil || string(data) != "hello" {
		t.Errorf("real.txt not copied correctly: data=%q err=%v", data, err)
	}

	// Directory symlink recreated as a symlink pointing to the same target.
	info, err := os.Lstat(filepath.Join(dstDir, "dirlink"))
	if err != nil {
		t.Fatalf("dirlink not present: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("dirlink should be a symlink, got mode %v", info.Mode())
	}
	if target, err := os.Readlink(filepath.Join(dstDir, "dirlink")); err != nil || target != "targetdir" {
		t.Errorf("dirlink target = %q err=%v, want %q", target, err, "targetdir")
	}

	// File symlink recreated.
	if target, err := os.Readlink(filepath.Join(dstDir, "filelink")); err != nil || target != "real.txt" {
		t.Errorf("filelink target = %q err=%v, want %q", target, err, "real.txt")
	}
}

func TestCopyDirSkipsGit(t *testing.T) {
	srcDir := t.TempDir()

	// .git directory with content that must be skipped.
	gitDir := filepath.Join(srcDir, ".git")
	if err := os.MkdirAll(gitDir, 0755); err != nil {
		t.Fatalf("failed to create .git dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main"), 0644); err != nil {
		t.Fatalf("failed to write .git/HEAD: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "keep.txt"), []byte("keep"), 0644); err != nil {
		t.Fatalf("failed to write keep.txt: %v", err)
	}

	dstDir := t.TempDir()
	if err := copyDir(srcDir, dstDir); err != nil {
		t.Fatalf("copyDir() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(dstDir, ".git")); !os.IsNotExist(err) {
		t.Errorf(".git directory should have been skipped, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "keep.txt")); err != nil {
		t.Errorf("keep.txt should have been copied: %v", err)
	}
}
