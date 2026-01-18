// Package git provides git diff functionality using the git binary.
package git

import (
	"strings"
)

// ChangedFiles represents the files changed between two commits.
type ChangedFiles struct {
	Added    []string
	Modified []string
	Deleted  []string
}

// AllPaths returns all changed file paths (added, modified, and deleted).
func (c *ChangedFiles) AllPaths() []string {
	paths := make([]string, 0, len(c.Added)+len(c.Modified)+len(c.Deleted))
	paths = append(paths, c.Added...)
	paths = append(paths, c.Modified...)
	paths = append(paths, c.Deleted...)
	return paths
}

// HasChangesInPath checks if any changed files are within the given directory path.
func (c *ChangedFiles) HasChangesInPath(dirPath string) bool {
	if !strings.HasSuffix(dirPath, "/") {
		dirPath = dirPath + "/"
	}

	for _, path := range c.AllPaths() {
		if strings.HasPrefix(path, dirPath) || path == strings.TrimSuffix(dirPath, "/") {
			return true
		}
	}

	return false
}

// GetDiff returns the changed files between two branches.
func (r *Repository) GetDiff(baseBranch, targetBranch string) (*ChangedFiles, error) {
	// Use git diff --name-status to get changed files
	output, err := r.run("diff", "--name-status", baseBranch+".."+targetBranch)
	if err != nil {
		return nil, err
	}

	return parseNameStatus(output), nil
}

// GetDiffFromMergeBase returns changed files from the merge base of two branches.
func (r *Repository) GetDiffFromMergeBase(baseBranch, targetBranch string) (*ChangedFiles, error) {
	// Find merge base
	mergeBase, err := r.run("merge-base", baseBranch, targetBranch)
	if err != nil {
		return nil, err
	}

	// Get diff from merge base to target
	output, err := r.run("diff", "--name-status", mergeBase+".."+targetBranch)
	if err != nil {
		return nil, err
	}

	return parseNameStatus(output), nil
}

// parseNameStatus parses git diff --name-status output.
// Format: <status>\t<path> or <status>\t<old-path>\t<new-path> for renames
func parseNameStatus(output string) *ChangedFiles {
	changed := &ChangedFiles{
		Added:    make([]string, 0),
		Modified: make([]string, 0),
		Deleted:  make([]string, 0),
	}

	if output == "" {
		return changed
	}

	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			continue
		}

		status := parts[0]
		path := parts[1]

		switch {
		case status == "A":
			changed.Added = append(changed.Added, path)
		case status == "D":
			changed.Deleted = append(changed.Deleted, path)
		case status == "M":
			changed.Modified = append(changed.Modified, path)
		case strings.HasPrefix(status, "R"):
			// Rename: R100\told-path\tnew-path
			if len(parts) >= 3 {
				changed.Deleted = append(changed.Deleted, path)
				changed.Added = append(changed.Added, parts[2])
			}
		case strings.HasPrefix(status, "C"):
			// Copy: C100\told-path\tnew-path
			if len(parts) >= 3 {
				changed.Added = append(changed.Added, parts[2])
			}
		default:
			// Treat unknown status as modified
			changed.Modified = append(changed.Modified, path)
		}
	}

	return changed
}
