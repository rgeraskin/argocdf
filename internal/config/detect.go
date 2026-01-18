// Package config provides auto-detection for configuration values using git binary.
package config

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rgeraskin/argocdf/internal/git"
)

// runGit executes a git command in the specified directory.
func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s failed: %v\nstderr: %s", args[0], err, stderr.String())
	}

	return strings.TrimSpace(stdout.String()), nil
}

// runGitSilent executes a git command and returns whether it succeeded.
func runGitSilent(dir string, args ...string) bool {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	return cmd.Run() == nil
}

// DetectRepoPath attempts to find the git repository root from the current directory.
func DetectRepoPath() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to get working directory: %w", err)
	}

	// Use git rev-parse to find repo root
	cmd := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("not a git repository (or any parent up to root): %s", cwd)
	}

	return strings.TrimSpace(string(output)), nil
}

// DetectBaseBranch detects the default base branch (main or master).
func DetectBaseBranch(repoPath string) (string, error) {
	// Check for common branch names
	branchNames := []string{"main", "master"}

	for _, name := range branchNames {
		if runGitSilent(repoPath, "rev-parse", "--verify", "refs/heads/"+name) {
			return name, nil
		}
	}

	// Try to get default branch from remote
	output, err := runGit(repoPath, "symbolic-ref", "refs/remotes/origin/HEAD")
	if err == nil {
		// Output like: refs/remotes/origin/main
		parts := strings.Split(output, "/")
		if len(parts) > 0 {
			return parts[len(parts)-1], nil
		}
	}

	return "", fmt.Errorf("could not detect base branch (tried main, master)")
}

// DetectTargetBranch returns the current branch name (HEAD).
func DetectTargetBranch(repoPath string) (string, error) {
	// Try to get branch name
	output, err := runGit(repoPath, "symbolic-ref", "--short", "HEAD")
	if err == nil {
		return output, nil
	}

	// Detached HEAD - return short hash
	output, err = runGit(repoPath, "rev-parse", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("failed to get HEAD: %w", err)
	}

	return output, nil
}

// DetectRepoURL attempts to find the remote URL for the repository.
func DetectRepoURL(repoPath string) (string, error) {
	output, err := runGit(repoPath, "remote", "get-url", "origin")
	if err != nil {
		return "", fmt.Errorf("no remote URL found")
	}

	return git.NormalizeRepoURL(output), nil
}

// DetectKubeconfig returns the kubeconfig path.
func DetectKubeconfig() string {
	// Check KUBECONFIG env var first
	if kubeconfig := os.Getenv("KUBECONFIG"); kubeconfig != "" {
		return kubeconfig
	}

	// Fall back to default location
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	return filepath.Join(home, ".kube", "config")
}

// AutoDetect fills in missing configuration values by auto-detection.
func AutoDetect(cfg *Config) error {
	var err error

	// Detect repo path if not set
	if cfg.RepoPath == "" {
		cfg.RepoPath, err = DetectRepoPath()
		if err != nil {
			return fmt.Errorf("auto-detect repo path: %w", err)
		}
	}

	// Detect base branch if not set
	if cfg.BaseBranch == "" {
		cfg.BaseBranch, err = DetectBaseBranch(cfg.RepoPath)
		if err != nil {
			return fmt.Errorf("auto-detect base branch: %w", err)
		}
	}

	// Detect target branch if not set
	if cfg.TargetBranch == "" {
		cfg.TargetBranch, err = DetectTargetBranch(cfg.RepoPath)
		if err != nil {
			return fmt.Errorf("auto-detect target branch: %w", err)
		}
	}

	// Detect repo URL if not set
	if cfg.RepoURL == "" {
		cfg.RepoURL, err = DetectRepoURL(cfg.RepoPath)
		if err != nil {
			// Non-fatal - some operations don't need repo URL
			cfg.RepoURL = ""
		}
	}

	// Detect kubeconfig if not set
	if cfg.KubeconfigPath == "" {
		cfg.KubeconfigPath = DetectKubeconfig()
	}

	return nil
}
