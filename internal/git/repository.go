// Package git provides git repository operations using the git binary.
package git

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Repository wraps git binary operations for a repository.
type Repository struct {
	path string
}

// Open opens an existing git repository at the given path.
func Open(path string) (*Repository, error) {
	// Verify it's a git repository
	cmd := exec.Command("git", "-C", path, "rev-parse", "--git-dir")
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("not a git repository: %s", path)
	}

	return &Repository{path: path}, nil
}

// Path returns the repository root path.
func (r *Repository) Path() string {
	return r.path
}

// run executes a git command and returns stdout.
func (r *Repository) run(args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", r.path}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s failed: %v\nstderr: %s", args[0], err, stderr.String())
	}

	return strings.TrimSpace(stdout.String()), nil
}

// runSilent executes a git command and returns whether it succeeded.
func (r *Repository) runSilent(args ...string) bool {
	cmd := exec.Command("git", append([]string{"-C", r.path}, args...)...)
	return cmd.Run() == nil
}

// Head returns the current HEAD commit hash.
func (r *Repository) Head() (string, error) {
	return r.run("rev-parse", "HEAD")
}

// HeadBranch returns the current branch name, or empty string if detached HEAD.
func (r *Repository) HeadBranch() (string, error) {
	output, err := r.run("symbolic-ref", "--short", "HEAD")
	if err != nil {
		// Detached HEAD
		return "", nil
	}
	return output, nil
}

// Checkout checks out the specified branch.
func (r *Repository) Checkout(branchName string) error {
	_, err := r.run("checkout", branchName)
	return err
}

// CheckoutHash checks out a specific commit hash.
func (r *Repository) CheckoutHash(hash string) error {
	_, err := r.run("checkout", hash)
	return err
}

// CurrentBranch returns the current branch name, or empty string if detached HEAD.
func (r *Repository) CurrentBranch() (string, error) {
	return r.HeadBranch()
}

// RemoteURL returns the URL of the origin remote.
func (r *Repository) RemoteURL() (string, error) {
	return r.run("remote", "get-url", "origin")
}

// BranchExists checks if a branch exists.
func (r *Repository) BranchExists(branchName string) bool {
	return r.runSilent("rev-parse", "--verify", "refs/heads/"+branchName)
}

// CommitHash returns the hash for a branch or ref.
func (r *Repository) CommitHash(ref string) (string, error) {
	return r.run("rev-parse", ref)
}

// WithBranch executes a function while checked out to the specified branch,
// then restores the original position afterward.
func (r *Repository) WithBranch(branchName string, fn func() error) error {
	// Save current position (branch name or commit hash)
	originalBranch, _ := r.HeadBranch()
	var originalRef string
	if originalBranch == "" {
		// Detached HEAD - save the hash
		hash, err := r.Head()
		if err != nil {
			return fmt.Errorf("failed to get HEAD: %w", err)
		}
		originalRef = hash
	} else {
		originalRef = originalBranch
	}

	// Checkout target branch
	if err := r.Checkout(branchName); err != nil {
		return err
	}

	// Execute function
	fnErr := fn()

	// Restore original position
	restoreErr := r.Checkout(originalRef)
	if restoreErr != nil {
		if fnErr != nil {
			return fmt.Errorf("function error: %v, restore error: %w", fnErr, restoreErr)
		}
		return fmt.Errorf("failed to restore original position: %w", restoreErr)
	}

	return fnErr
}

// FullPath returns the full path for a relative path within the repository.
func (r *Repository) FullPath(relativePath string) string {
	return filepath.Join(r.path, relativePath)
}

// FileExists checks if a file exists in the repository.
func (r *Repository) FileExists(relativePath string) bool {
	fullPath := r.FullPath(relativePath)
	_, err := os.Stat(fullPath)
	return err == nil
}

// ReadFile reads a file from the repository.
func (r *Repository) ReadFile(relativePath string) ([]byte, error) {
	fullPath := r.FullPath(relativePath)
	return os.ReadFile(fullPath)
}

// NormalizeRepoURL normalizes a git URL for comparison.
// It converts various URL formats to a consistent HTTPS format.
func NormalizeRepoURL(url string) string {
	// Remove trailing slash first (before .git check)
	url = strings.TrimSuffix(url, "/")
	// Remove .git suffix
	url = strings.TrimSuffix(url, ".git")

	// Handle ssh://git@hostname/path format
	// e.g., ssh://git@github.com/owner/repo -> https://github.com/owner/repo
	if after, found := strings.CutPrefix(url, "ssh://"); found {
		if _, rest, ok := strings.Cut(after, "@"); ok {
			return "https://" + rest
		}
		return "https://" + after
	}

	// Handle git@hostname:path format
	// e.g., git@github.com:owner/repo -> https://github.com/owner/repo
	if after, found := strings.CutPrefix(url, "git@"); found {
		if host, path, ok := strings.Cut(after, ":"); ok {
			return "https://" + host + "/" + path
		}
		return "https://" + after
	}

	// Already https:// or http:// - return as-is (after suffix removal)
	return url
}

// Clone clones a repository to the specified path.
func Clone(repoURL, revision, destPath string) error {
	args := []string{"clone", "--depth", "1"}

	if revision != "" && revision != "HEAD" {
		args = append(args, "--branch", revision)
	}

	args = append(args, repoURL, destPath)

	cmd := exec.Command("git", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone failed: %v\noutput: %s", err, string(output))
	}

	return nil
}
