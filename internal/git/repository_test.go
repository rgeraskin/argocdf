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

func TestTreeHash(t *testing.T) {
	repoDir := t.TempDir()

	runCmd := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}

	if err := exec.Command("git", "-C", repoDir, "init").Run(); err != nil {
		t.Skip("git not available")
	}
	runCmd("config", "user.email", "test@example.com")
	runCmd("config", "user.name", "Test User")

	// Commit 1: create app/values.yaml and an unrelated file.
	if err := os.MkdirAll(filepath.Join(repoDir, "app"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "app", "values.yaml"), []byte("replicas: 1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "other.txt"), []byte("a\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runCmd("add", ".")
	runCmd("commit", "-m", "c1")

	repo, err := Open(repoDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c1, err := repo.CommitHash("HEAD")
	if err != nil {
		t.Fatalf("CommitHash: %v", err)
	}

	hash1, err := repo.TreeHash(c1, "app")
	if err != nil {
		t.Fatalf("TreeHash(app) at c1: %v", err)
	}

	// Commit 2: change an unrelated file only; app/ content is untouched.
	if err := os.WriteFile(filepath.Join(repoDir, "other.txt"), []byte("b\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runCmd("add", ".")
	runCmd("commit", "-m", "c2")
	c2, err := repo.CommitHash("HEAD")
	if err != nil {
		t.Fatalf("CommitHash: %v", err)
	}

	hash2, err := repo.TreeHash(c2, "app")
	if err != nil {
		t.Fatalf("TreeHash(app) at c2: %v", err)
	}
	if hash1 != hash2 {
		t.Errorf("expected stable tree hash for unchanged path across commits, got %s != %s", hash1, hash2)
	}

	// Commit 3: change app/values.yaml; hash must change.
	if err := os.WriteFile(filepath.Join(repoDir, "app", "values.yaml"), []byte("replicas: 2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runCmd("add", ".")
	runCmd("commit", "-m", "c3")
	c3, err := repo.CommitHash("HEAD")
	if err != nil {
		t.Fatalf("CommitHash: %v", err)
	}

	hash3, err := repo.TreeHash(c3, "app")
	if err != nil {
		t.Fatalf("TreeHash(app) at c3: %v", err)
	}
	if hash3 == hash1 {
		t.Errorf("expected different tree hash after path content changed, got %s", hash3)
	}

	// Root tree via "." and "" should both resolve and be equal.
	rootDot, err := repo.TreeHash(c3, ".")
	if err != nil {
		t.Fatalf("TreeHash(.): %v", err)
	}
	rootEmpty, err := repo.TreeHash(c3, "")
	if err != nil {
		t.Fatalf("TreeHash(\"\"): %v", err)
	}
	if rootDot != rootEmpty {
		t.Errorf("root tree hash mismatch: %q vs %q", rootDot, rootEmpty)
	}

	// Missing path must return an error (caller treats as cache bypass).
	if _, err := repo.TreeHash(c3, "does/not/exist"); err == nil {
		t.Error("expected error for missing path")
	}
}
