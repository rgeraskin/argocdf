// Package git provides git repository operations using the git binary.
package git

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

// run executes a git command and returns stdout.
func (r *Repository) run(args ...string) (string, error) {
	return runGitCommand(r.path, args...)
}

// runSilent executes a git command and returns whether it succeeded.
func (r *Repository) runSilent(args ...string) bool {
	return runGitCommandBool(r.path, args...)
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

// CommitHash returns the hash for a branch or ref.
func (r *Repository) CommitHash(ref string) (string, error) {
	return r.run("rev-parse", ref)
}

// MergeBase returns the best common ancestor commit of two refs.
func (r *Repository) MergeBase(ref1, ref2 string) (string, error) {
	return r.run("merge-base", ref1, ref2)
}

// RemoteRefExists reports whether the given ref resolves to a commit. It is
// intended for remote-tracking refs like "origin/main", but works for any ref.
func (r *Repository) RemoteRefExists(ref string) bool {
	return r.runSilent("rev-parse", "--verify", ref+"^{commit}")
}

// IsAncestor reports whether ancestor is an ancestor of descendant (i.e. the
// history at descendant contains ancestor). Returns false on any git error.
func (r *Repository) IsAncestor(ancestor, descendant string) bool {
	return r.runSilent("merge-base", "--is-ancestor", ancestor, descendant)
}

// CountCommitsBetween returns the number of commits reachable from to but not
// from, i.e. `git rev-list --count from..to`.
func (r *Repository) CountCommitsBetween(from, to string) (int, error) {
	out, err := r.run("rev-list", "--count", from+".."+to)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("unexpected rev-list output %q: %w", out, err)
	}
	return n, nil
}

// TreeHash returns the git object (tree or blob) hash for the given path at the
// specified commit. It reads directly from the object database via
// `git rev-parse <commit>:<path>`, so no checkout is required. Because the hash
// is content-addressed, identical content yields an identical hash across
// branches and commits.
//
// An empty or "." path resolves to the commit's root tree (<commit>^{tree}).
// If the path does not exist at the given commit, an error is returned.
func (r *Repository) TreeHash(commit, path string) (string, error) {
	cleanPath := strings.Trim(strings.TrimSpace(path), "/")
	if cleanPath == "" || cleanPath == "." {
		return r.run("rev-parse", commit+"^{tree}")
	}
	return r.run("rev-parse", commit+":"+cleanPath)
}

// FileContent returns the content of a file at the specified commit, read
// directly from the object database via `git show <commit>:<path>` — no
// checkout required. Trailing whitespace is trimmed (see run), which is
// harmless for the YAML/config probing this supports. An error is returned
// when the path does not exist at the commit.
func (r *Repository) FileContent(commit, path string) (string, error) {
	cleanPath := strings.Trim(strings.TrimSpace(path), "/")
	if cleanPath == "" {
		return "", fmt.Errorf("file path is empty")
	}
	return r.run("show", commit+":"+cleanPath)
}

// AddWorktree creates an ephemeral detached-HEAD git worktree checked out at
// the given ref (branch name or commit hash) under a fresh temp directory.
// It returns the worktree path and a cleanup function that removes the
// worktree via `git worktree remove --force`, falling back to os.RemoveAll plus
// `git worktree prune` if that fails. Callers MUST invoke cleanup to avoid
// leaking temp directories and dangling worktree registrations.
//
// Rendering against a dedicated worktree keeps the user's working tree
// untouched (no checkout churn, no `helm dependency build` litter) and lets
// multiple applications render in parallel from a fixed, committed tree.
func (r *Repository) AddWorktree(ref string) (string, func(), error) {
	// Create a parent temp dir and use a non-existent subpath for the worktree
	// itself: `git worktree add` refuses a path that already exists.
	parent, err := os.MkdirTemp("", "argocdf-worktree-")
	if err != nil {
		return "", nil, fmt.Errorf("failed to create temp dir for worktree: %w", err)
	}
	worktreePath := filepath.Join(parent, "tree")

	if _, err := r.run("worktree", "add", "--detach", worktreePath, ref); err != nil {
		_ = os.RemoveAll(parent)
		return "", nil, fmt.Errorf("failed to add worktree at %q: %w", ref, err)
	}

	cleanup := func() {
		if _, err := r.run("worktree", "remove", "--force", worktreePath); err != nil {
			// Best-effort fallback: drop the files ourselves and let git prune
			// the now-missing worktree registration.
			_ = os.RemoveAll(parent)
			_, _ = r.run("worktree", "prune")
			return
		}
		_ = os.RemoveAll(parent)
	}

	return worktreePath, cleanup, nil
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

// CloneCreds carries optional HTTP(S) credentials for Clone: basic
// auth (username/password) or a bearer token. SSH remotes and other
// credential kinds keep using the ambient git configuration.
type CloneCreds struct {
	Username    string
	Password    string
	BearerToken string
}

// authEnv returns GIT_CONFIG_* environment entries that inject an
// Authorization extraheader for HTTP(S) remotes. Environment-borne config
// keeps credentials out of argv (visible in `ps`) and off disk.
func (c *CloneCreds) authEnv(repoURL string) []string {
	if c == nil || !strings.HasPrefix(repoURL, "http") {
		return nil
	}
	var header string
	switch {
	case c.BearerToken != "":
		header = "Authorization: Bearer " + c.BearerToken
	case c.Username != "" || c.Password != "":
		header = "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(c.Username+":"+c.Password))
	default:
		return nil
	}
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraheader",
		"GIT_CONFIG_VALUE_0=" + header,
	}
}

// Clone clones a repository at revision into destPath, authenticating HTTP(S)
// remotes with creds (nil clones anonymously / with ambient git config). It
// first attempts a shallow clone of the revision; since --branch only accepts
// branch or tag names, it falls back to a full clone followed by a checkout
// when the revision is a commit SHA.
func Clone(repoURL, revision, destPath string, creds *CloneCreds) error {
	env := append(os.Environ(), creds.authEnv(repoURL)...)

	args := []string{"clone", "--depth", "1"}

	if revision != "" && revision != "HEAD" {
		args = append(args, "--branch", revision)
	}

	args = append(args, repoURL, destPath)

	cmd := exec.Command("git", args...)
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}

	// No fallback possible without a revision to checkout
	if revision == "" || revision == "HEAD" {
		return fmt.Errorf("git clone failed: %v\noutput: %s", err, string(output))
	}

	// Clean up any partial clone - cloning into a non-empty directory fails
	if rmErr := os.RemoveAll(destPath); rmErr != nil {
		return fmt.Errorf("git clone failed: %v\noutput: %s\ncleanup failed: %v", err, string(output), rmErr)
	}

	// Full clone, then checkout the revision (works for commit SHAs)
	cmd = exec.Command("git", "clone", repoURL, destPath)
	cmd.Env = env
	if fullOutput, fullErr := cmd.CombinedOutput(); fullErr != nil {
		return fmt.Errorf("git clone failed: %v\noutput: %s", fullErr, string(fullOutput))
	}

	cmd = exec.Command("git", "-C", destPath, "checkout", revision)
	if checkoutOutput, checkoutErr := cmd.CombinedOutput(); checkoutErr != nil {
		return fmt.Errorf("git checkout %s failed: %v\noutput: %s", revision, checkoutErr, string(checkoutOutput))
	}

	return nil
}
