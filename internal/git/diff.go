// Package git provides git diff functionality using the git binary.
package git

import (
	"path"
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
// Paths are normalized before comparison (git always outputs forward slashes),
// so values like ".", "./charts/x", or "charts/x/" match as expected.
func (c *ChangedFiles) HasChangesInPath(dirPath string) bool {
	// An unspecified path never matches; a repo-root path is "." explicitly
	if dirPath == "" {
		return false
	}

	dirPath = path.Clean(dirPath)

	// A repo-root source path matches any change
	if dirPath == "." {
		return len(c.AllPaths()) > 0
	}

	prefix := dirPath + "/"
	for _, p := range c.AllPaths() {
		p = path.Clean(p)
		if p == dirPath || strings.HasPrefix(p, prefix) {
			return true
		}
	}

	return false
}

// GetDiff returns the changed files between two refs (branches or commits).
func (r *Repository) GetDiff(baseRef, targetRef string) (*ChangedFiles, error) {
	// Use git diff --name-status to get changed files
	output, err := r.run("diff", "--name-status", baseRef+".."+targetRef)
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
