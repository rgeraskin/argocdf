// Package git provides git command execution utilities.
package git

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// runGitCommand executes a git command in the specified directory and returns stdout.
// This is used internally by the git package.
func runGitCommand(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s failed: %v\nstderr: %s", args[0], err, stderr.String())
	}

	return strings.TrimSpace(stdout.String()), nil
}

// runGitCommandBool executes a git command and returns whether it succeeded.
// This is used internally by the git package.
func runGitCommandBool(dir string, args ...string) bool {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	return cmd.Run() == nil
}

// RunGitCommand executes a git command in the specified directory and returns stdout.
// This is the exported version for use by other packages.
func RunGitCommand(dir string, args ...string) (string, error) {
	return runGitCommand(dir, args...)
}

// RunGitCommandBool executes a git command and returns whether it succeeded.
// This is the exported version for use by other packages.
func RunGitCommandBool(dir string, args ...string) bool {
	return runGitCommandBool(dir, args...)
}
