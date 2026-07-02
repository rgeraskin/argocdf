package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestNormalizeRepoURL(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		// HTTPS URLs
		{
			name:     "https URL unchanged",
			input:    "https://github.com/owner/repo",
			expected: "https://github.com/owner/repo",
		},
		{
			name:     "https URL with .git suffix",
			input:    "https://github.com/owner/repo.git",
			expected: "https://github.com/owner/repo",
		},
		{
			name:     "https URL with trailing slash",
			input:    "https://github.com/owner/repo/",
			expected: "https://github.com/owner/repo",
		},
		{
			name:     "https URL with both .git and trailing slash",
			input:    "https://github.com/owner/repo.git/",
			expected: "https://github.com/owner/repo",
		},

		// SSH URLs (git@host:path format)
		{
			name:     "git@ SSH URL converted to https",
			input:    "git@github.com:owner/repo",
			expected: "https://github.com/owner/repo",
		},
		{
			name:     "git@ SSH URL with .git suffix",
			input:    "git@github.com:owner/repo.git",
			expected: "https://github.com/owner/repo",
		},
		{
			name:     "git@ SSH URL with nested path",
			input:    "git@gitlab.com:group/subgroup/repo.git",
			expected: "https://gitlab.com/group/subgroup/repo",
		},

		// SSH URLs (ssh:// format)
		{
			name:     "ssh:// URL converted to https",
			input:    "ssh://git@github.com/owner/repo",
			expected: "https://github.com/owner/repo",
		},
		{
			name:     "ssh:// URL with .git suffix",
			input:    "ssh://git@github.com/owner/repo.git",
			expected: "https://github.com/owner/repo",
		},
		{
			name:     "ssh:// URL without user",
			input:    "ssh://github.com/owner/repo",
			expected: "https://github.com/owner/repo",
		},

		// HTTP URLs
		{
			name:     "http URL unchanged",
			input:    "http://github.com/owner/repo",
			expected: "http://github.com/owner/repo",
		},
		{
			name:     "http URL with .git suffix",
			input:    "http://github.com/owner/repo.git",
			expected: "http://github.com/owner/repo",
		},

		// Edge cases
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "URL with port",
			input:    "https://github.com:443/owner/repo.git",
			expected: "https://github.com:443/owner/repo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := NormalizeRepoURL(tt.input)
			if result != tt.expected {
				t.Errorf("NormalizeRepoURL(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestGetWorktreeForBranch(t *testing.T) {
	// Create a temporary directory for testing
	tmpDir, err := os.MkdirTemp("", "argocdf-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.RemoveAll(tmpDir)
	}()

	// Initialize a git repo
	mainRepo := filepath.Join(tmpDir, "main-repo")
	if err := os.MkdirAll(mainRepo, 0755); err != nil {
		t.Fatal(err)
	}

	runCmd := func(dir string, args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		return cmd.Run()
	}

	// Setup git repo with initial commit
	if err := runCmd(mainRepo, "init"); err != nil {
		t.Skip("git not available")
	}
	if err := runCmd(mainRepo, "config", "user.email", "test@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := runCmd(mainRepo, "config", "user.name", "Test User"); err != nil {
		t.Fatal(err)
	}

	// Create initial commit
	testFile := filepath.Join(mainRepo, "test.txt")
	if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := runCmd(mainRepo, "add", "."); err != nil {
		t.Fatal(err)
	}
	if err := runCmd(mainRepo, "commit", "-m", "initial"); err != nil {
		t.Fatal(err)
	}

	// Create a feature branch
	if err := runCmd(mainRepo, "branch", "feature"); err != nil {
		t.Fatal(err)
	}

	// Create a worktree for the feature branch
	worktreePath := filepath.Join(tmpDir, "feature-worktree")
	if err := runCmd(mainRepo, "worktree", "add", worktreePath, "feature"); err != nil {
		t.Fatal(err)
	}

	// Open the main repository
	repo, err := Open(mainRepo)
	if err != nil {
		t.Fatalf("failed to open repo: %v", err)
	}

	// Helper to resolve symlinks for comparison
	resolvePath := func(p string) string {
		resolved, err := filepath.EvalSymlinks(p)
		if err != nil {
			return p
		}
		return resolved
	}

	// Test 1: Feature branch should be found in the worktree
	path, err := repo.GetWorktreeForBranch("feature")
	if err != nil {
		t.Errorf("GetWorktreeForBranch failed: %v", err)
	}
	if resolvePath(path) != resolvePath(worktreePath) {
		t.Errorf("GetWorktreeForBranch(feature) = %q, want %q", path, worktreePath)
	}

	// Test 2: Main/master branch should be in the main repo
	// First determine the default branch name
	currentBranch, _ := repo.HeadBranch()
	path, err = repo.GetWorktreeForBranch(currentBranch)
	if err != nil {
		t.Errorf("GetWorktreeForBranch failed: %v", err)
	}
	if resolvePath(path) != resolvePath(mainRepo) {
		t.Errorf("GetWorktreeForBranch(%s) = %q, want %q", currentBranch, path, mainRepo)
	}

	// Test 3: Non-existent branch should return empty string
	path, err = repo.GetWorktreeForBranch("nonexistent")
	if err != nil {
		t.Errorf("GetWorktreeForBranch failed: %v", err)
	}
	if path != "" {
		t.Errorf("GetWorktreeForBranch(nonexistent) = %q, want empty string", path)
	}
}

// gitRun runs a git command in dir, failing the test on error.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\noutput: %s", args, err, out)
	}
}

// commitFile writes a file and commits it with test identity.
func commitFile(t *testing.T, dir, file, content, msg string) {
	t.Helper()
	fullPath := filepath.Join(dir, file)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", msg)
}

// initFixtureRepo creates a git repository with an initial commit and returns its path.
func initFixtureRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init")
	commitFile(t, dir, "init.txt", "init", "initial")
	return dir
}

func TestWithBranchPanicRestoresBranch(t *testing.T) {
	// A panic inside fn must not leave the repo checked out to the wrong branch
	repoDir := initFixtureRepo(t)
	gitRun(t, repoDir, "branch", "other")

	repo, err := Open(repoDir)
	if err != nil {
		t.Fatalf("failed to open repo: %v", err)
	}
	originalBranch, err := repo.HeadBranch()
	if err != nil {
		t.Fatalf("failed to get original branch: %v", err)
	}

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic to propagate from WithBranch")
			}
		}()
		_ = repo.WithBranch("other", func() error {
			panic("boom")
		})
	}()

	currentBranch, err := repo.HeadBranch()
	if err != nil {
		t.Fatalf("failed to get current branch: %v", err)
	}
	if currentBranch != originalBranch {
		t.Errorf("branch after panic = %q, want %q", currentBranch, originalBranch)
	}
}

func TestWithBranchPanicRestoresWorktreePath(t *testing.T) {
	// A panic inside fn must not leave the repository path swapped to the worktree
	repoDir := initFixtureRepo(t)
	gitRun(t, repoDir, "branch", "other")
	worktreePath := filepath.Join(t.TempDir(), "other-worktree")
	gitRun(t, repoDir, "worktree", "add", worktreePath, "other")

	repo, err := Open(repoDir)
	if err != nil {
		t.Fatalf("failed to open repo: %v", err)
	}
	originalPath := repo.Path()

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic to propagate from WithBranch")
			}
		}()
		_ = repo.WithBranch("other", func() error {
			panic("boom")
		})
	}()

	if repo.Path() != originalPath {
		t.Errorf("path after panic = %q, want %q", repo.Path(), originalPath)
	}
}

func TestCloneCommitSHA(t *testing.T) {
	// Cloning by commit SHA must fall back to a full clone plus checkout,
	// since git clone --branch only accepts branch or tag names
	srcDir := initFixtureRepo(t)
	commitFile(t, srcDir, "second.txt", "second", "second commit")

	src, err := Open(srcDir)
	if err != nil {
		t.Fatalf("failed to open source repo: %v", err)
	}
	sha, err := src.CommitHash("HEAD~1")
	if err != nil {
		t.Fatalf("failed to resolve HEAD~1: %v", err)
	}

	destPath := filepath.Join(t.TempDir(), "clone")
	if err := Clone(srcDir, sha, destPath); err != nil {
		t.Fatalf("Clone by SHA failed: %v", err)
	}

	cloned, err := Open(destPath)
	if err != nil {
		t.Fatalf("failed to open cloned repo: %v", err)
	}
	head, err := cloned.Head()
	if err != nil {
		t.Fatalf("failed to get cloned HEAD: %v", err)
	}
	if head != sha {
		t.Errorf("cloned HEAD = %q, want %q", head, sha)
	}
}
