package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func TestAddWorktree(t *testing.T) {
	// AddWorktree must create a detached worktree checked out at the given ref,
	// and cleanup must remove both the worktree files and its registration.
	repoDir := initFixtureRepo(t)
	commitFile(t, repoDir, "second.txt", "second", "second commit")

	repo, err := Open(repoDir)
	if err != nil {
		t.Fatalf("failed to open repo: %v", err)
	}

	// Use the first commit so we can verify the worktree is at the requested ref.
	firstCommit, err := repo.CommitHash("HEAD~1")
	if err != nil {
		t.Fatalf("failed to resolve HEAD~1: %v", err)
	}

	worktreePath, cleanup, err := repo.AddWorktree(firstCommit)
	if err != nil {
		t.Fatalf("AddWorktree failed: %v", err)
	}

	// The worktree directory must exist.
	if info, statErr := os.Stat(worktreePath); statErr != nil || !info.IsDir() {
		t.Fatalf("worktree path %q not a directory: %v", worktreePath, statErr)
	}

	// The worktree HEAD must match the requested commit (detached).
	wtRepo, err := Open(worktreePath)
	if err != nil {
		t.Fatalf("failed to open worktree repo: %v", err)
	}
	head, err := wtRepo.Head()
	if err != nil {
		t.Fatalf("failed to get worktree HEAD: %v", err)
	}
	if head != firstCommit {
		t.Errorf("worktree HEAD = %q, want %q", head, firstCommit)
	}
	if branch, _ := wtRepo.HeadBranch(); branch != "" {
		t.Errorf("expected detached HEAD, got branch %q", branch)
	}

	// The first-commit tree must not contain second.txt.
	if _, statErr := os.Stat(filepath.Join(worktreePath, "second.txt")); !os.IsNotExist(statErr) {
		t.Errorf("second.txt should not exist in worktree at first commit")
	}

	// The worktree must be registered.
	list, err := repo.run("worktree", "list", "--porcelain")
	if err != nil {
		t.Fatalf("worktree list failed: %v", err)
	}
	resolvedWT, _ := filepath.EvalSymlinks(worktreePath)
	if !strings.Contains(list, worktreePath) && !strings.Contains(list, resolvedWT) {
		t.Errorf("worktree %q not registered in list:\n%s", worktreePath, list)
	}

	// Cleanup must remove the worktree directory and its registration.
	cleanup()

	if _, statErr := os.Stat(worktreePath); !os.IsNotExist(statErr) {
		t.Errorf("worktree path still exists after cleanup: %v", statErr)
	}
	list, err = repo.run("worktree", "list", "--porcelain")
	if err != nil {
		t.Fatalf("worktree list after cleanup failed: %v", err)
	}
	if strings.Contains(list, worktreePath) || (resolvedWT != "" && strings.Contains(list, resolvedWT)) {
		t.Errorf("worktree still registered after cleanup:\n%s", list)
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
	if err := Clone(srcDir, sha, destPath, nil); err != nil {
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

func TestRemoteRefAndAncestry(t *testing.T) {
	dir := initFixtureRepo(t)
	gitRun(t, dir, "checkout", "-B", "main")

	repo, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c1, err := repo.CommitHash("HEAD")
	if err != nil {
		t.Fatalf("CommitHash: %v", err)
	}

	// No origin ref yet.
	if repo.RemoteRefExists("origin/main") {
		t.Fatal("RemoteRefExists(origin/main) = true before ref created")
	}

	// Advance main and register origin/main at the new tip.
	commitFile(t, dir, "next.txt", "next", "c2")
	c2, err := repo.CommitHash("HEAD")
	if err != nil {
		t.Fatalf("CommitHash: %v", err)
	}
	gitRun(t, dir, "update-ref", "refs/remotes/origin/main", c2)

	if !repo.RemoteRefExists("origin/main") {
		t.Error("RemoteRefExists(origin/main) = false after ref created")
	}

	// c1 is an ancestor of c2, but not vice versa.
	if !repo.IsAncestor(c1, c2) {
		t.Errorf("IsAncestor(%s, %s) = false, want true", c1, c2)
	}
	if repo.IsAncestor(c2, c1) {
		t.Errorf("IsAncestor(%s, %s) = true, want false", c2, c1)
	}

	// One commit between c1 and c2.
	n, err := repo.CountCommitsBetween(c1, c2)
	if err != nil {
		t.Fatalf("CountCommitsBetween: %v", err)
	}
	if n != 1 {
		t.Errorf("CountCommitsBetween(c1, c2) = %d, want 1", n)
	}
}

func TestCloneCredsAuthEnv(t *testing.T) {
	basic := &CloneCreds{Username: "user", Password: "pass"}
	env := basic.authEnv("https://github.com/org/private.git")
	want := []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraheader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic dXNlcjpwYXNz", // base64(user:pass)
	}
	if len(env) != 3 || env[0] != want[0] || env[1] != want[1] || env[2] != want[2] {
		t.Errorf("authEnv() = %v, want %v", env, want)
	}

	bearer := &CloneCreds{BearerToken: "tok123"}
	env = bearer.authEnv("https://github.com/org/private.git")
	if len(env) != 3 || env[2] != "GIT_CONFIG_VALUE_0=Authorization: Bearer tok123" {
		t.Errorf("authEnv() bearer = %v, want a Bearer extraheader", env)
	}

	// SSH remotes use ambient git configuration, never HTTP headers.
	if env := basic.authEnv("git@github.com:org/private.git"); env != nil {
		t.Errorf("authEnv() for ssh remote = %v, want nil", env)
	}
	// nil and empty creds add nothing.
	var none *CloneCreds
	if env := none.authEnv("https://github.com/org/repo.git"); env != nil {
		t.Errorf("authEnv() on nil creds = %v, want nil", env)
	}
	if env := (&CloneCreds{}).authEnv("https://github.com/org/repo.git"); env != nil {
		t.Errorf("authEnv() on empty creds = %v, want nil", env)
	}
}
