// Package app provides tests for the main application orchestrator.
package app

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/log"

	"github.com/rgeraskin/argocdf/internal/cluster"
	"github.com/rgeraskin/argocdf/internal/config"
	"github.com/rgeraskin/argocdf/internal/git"
	"github.com/rgeraskin/argocdf/internal/testutil"
)

func TestFilterAffectedApps(t *testing.T) {
	tests := []struct {
		name         string
		apps         []cluster.Application
		repoURL      string
		changedFiles *git.ChangedFiles
		wantCount    int
		wantNames    []string
	}{
		{
			name:         "no apps",
			apps:         []cluster.Application{},
			repoURL:      "https://github.com/org/repo",
			changedFiles: testutil.TestChangedFiles([]string{"app/deployment.yaml"}, nil, nil),
			wantCount:    0,
			wantNames:    nil,
		},
		{
			name: "single app with matching path",
			apps: []cluster.Application{
				testutil.TestApp("my-app", "argocd", "https://github.com/org/repo", "app"),
			},
			repoURL:      "https://github.com/org/repo",
			changedFiles: testutil.TestChangedFiles([]string{"app/deployment.yaml"}, nil, nil),
			wantCount:    1,
			wantNames:    []string{"my-app"},
		},
		{
			name: "single app with non-matching path",
			apps: []cluster.Application{
				testutil.TestApp("my-app", "argocd", "https://github.com/org/repo", "app"),
			},
			repoURL:      "https://github.com/org/repo",
			changedFiles: testutil.TestChangedFiles([]string{"other/deployment.yaml"}, nil, nil),
			wantCount:    0,
			wantNames:    nil,
		},
		{
			name: "app with different repo URL",
			apps: []cluster.Application{
				testutil.TestApp("my-app", "argocd", "https://github.com/other/repo", "app"),
			},
			repoURL:      "https://github.com/org/repo",
			changedFiles: testutil.TestChangedFiles([]string{"app/deployment.yaml"}, nil, nil),
			wantCount:    0,
			wantNames:    nil,
		},
		{
			name: "multiple apps - some affected",
			apps: []cluster.Application{
				testutil.TestApp("app1", "argocd", "https://github.com/org/repo", "app1"),
				testutil.TestApp("app2", "argocd", "https://github.com/org/repo", "app2"),
				testutil.TestApp("app3", "argocd", "https://github.com/org/repo", "app3"),
			},
			repoURL:      "https://github.com/org/repo",
			changedFiles: testutil.TestChangedFiles([]string{"app1/values.yaml", "app3/templates/deployment.yaml"}, nil, nil),
			wantCount:    2,
			wantNames:    []string{"app1", "app3"},
		},
		{
			name: "URL normalization - git@ to https",
			apps: []cluster.Application{
				testutil.TestApp("my-app", "argocd", "git@github.com:org/repo.git", "app"),
			},
			repoURL:      "https://github.com/org/repo",
			changedFiles: testutil.TestChangedFiles([]string{"app/deployment.yaml"}, nil, nil),
			wantCount:    1,
			wantNames:    []string{"my-app"},
		},
		{
			name: "URL normalization - ssh:// format",
			apps: []cluster.Application{
				testutil.TestApp("my-app", "argocd", "ssh://git@github.com/org/repo.git", "app"),
			},
			repoURL:      "https://github.com/org/repo",
			changedFiles: testutil.TestChangedFiles([]string{"app/deployment.yaml"}, nil, nil),
			wantCount:    1,
			wantNames:    []string{"my-app"},
		},
		{
			name: "nested path changes",
			apps: []cluster.Application{
				testutil.TestApp("my-app", "argocd", "https://github.com/org/repo", "apps/production/app"),
			},
			repoURL:      "https://github.com/org/repo",
			changedFiles: testutil.TestChangedFiles([]string{"apps/production/app/values.yaml"}, nil, nil),
			wantCount:    1,
			wantNames:    []string{"my-app"},
		},
		{
			name: "modified files affect app",
			apps: []cluster.Application{
				testutil.TestApp("my-app", "argocd", "https://github.com/org/repo", "app"),
			},
			repoURL:      "https://github.com/org/repo",
			changedFiles: testutil.TestChangedFiles(nil, []string{"app/values.yaml"}, nil),
			wantCount:    1,
			wantNames:    []string{"my-app"},
		},
		{
			name: "deleted files affect app",
			apps: []cluster.Application{
				testutil.TestApp("my-app", "argocd", "https://github.com/org/repo", "app"),
			},
			repoURL:      "https://github.com/org/repo",
			changedFiles: testutil.TestChangedFiles(nil, nil, []string{"app/old-config.yaml"}),
			wantCount:    1,
			wantNames:    []string{"my-app"},
		},
		{
			name: "app with empty path - no match",
			apps: []cluster.Application{
				testutil.TestApp("my-app", "argocd", "https://github.com/org/repo", ""),
			},
			repoURL:      "https://github.com/org/repo",
			changedFiles: testutil.TestChangedFiles([]string{"app/deployment.yaml"}, nil, nil),
			wantCount:    0,
			wantNames:    nil,
		},
		{
			name: "path with trailing slash handling",
			apps: []cluster.Application{
				testutil.TestApp("my-app", "argocd", "https://github.com/org/repo", "app/"),
			},
			repoURL:      "https://github.com/org/repo",
			changedFiles: testutil.TestChangedFiles([]string{"app/deployment.yaml"}, nil, nil),
			wantCount:    1,
			wantNames:    []string{"my-app"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a minimal App for testing filterAffectedApps
			cfg := &config.Config{
				RepoURL: tt.repoURL,
			}
			logger := log.New(nil)
			logger.SetLevel(log.FatalLevel) // Suppress log output in tests
			app := &App{
				cfg:    cfg,
				logger: logger,
			}

			got := app.filterAffectedApps(tt.apps, tt.changedFiles)

			if len(got) != tt.wantCount {
				t.Errorf("filterAffectedApps() got %d apps, want %d", len(got), tt.wantCount)
			}

			// Verify specific app names if expected
			if tt.wantNames != nil {
				gotNames := make([]string, len(got))
				for i, a := range got {
					gotNames[i] = a.Name
				}
				for _, wantName := range tt.wantNames {
					found := false
					for _, gotName := range gotNames {
						if gotName == wantName {
							found = true
							break
						}
					}
					if !found {
						t.Errorf("filterAffectedApps() missing expected app %q, got %v", wantName, gotNames)
					}
				}
			}
		})
	}
}

func TestFilterAffectedApps_RefValueFiles(t *testing.T) {
	const localURL = "https://github.com/org/repo"

	// helmSource references a value file in a ref source via $values/... The
	// helm chart itself lives in a different repo than the one being diffed.
	helmSource := func(valueFiles ...string) cluster.ApplicationSource {
		return cluster.ApplicationSource{
			RepoURL: "https://charts.example.com",
			Chart:   "app",
			Helm:    &cluster.ApplicationSourceHelm{ValueFiles: valueFiles},
		}
	}
	refSource := func(refPath string) cluster.ApplicationSource {
		return cluster.ApplicationSource{
			RepoURL: localURL,
			Ref:     "values",
			Path:    refPath,
		}
	}

	tests := []struct {
		name         string
		app          cluster.Application
		changedFiles *git.ChangedFiles
		want         bool
	}{
		{
			name: "ref value file changed - affected",
			app: testutil.TestAppMultiSource("my-app", "argocd", []cluster.ApplicationSource{
				helmSource("$values/env/prod.yaml"),
				refSource(""),
			}),
			changedFiles: testutil.TestChangedFiles(nil, []string{"env/prod.yaml"}, nil),
			want:         true,
		},
		{
			name: "unrelated file changed - not affected",
			app: testutil.TestAppMultiSource("my-app", "argocd", []cluster.ApplicationSource{
				helmSource("$values/env/prod.yaml"),
				refSource(""),
			}),
			changedFiles: testutil.TestChangedFiles(nil, []string{"env/staging.yaml"}, nil),
			want:         false,
		},
		{
			name: "ref source with path prefix - affected",
			app: testutil.TestAppMultiSource("my-app", "argocd", []cluster.ApplicationSource{
				helmSource("$values/env/prod.yaml"),
				refSource("config"),
			}),
			changedFiles: testutil.TestChangedFiles(nil, []string{"config/env/prod.yaml"}, nil),
			want:         true,
		},
		{
			name: "ref value file added - affected",
			app: testutil.TestAppMultiSource("my-app", "argocd", []cluster.ApplicationSource{
				helmSource("$values/env/prod.yaml"),
				refSource(""),
			}),
			changedFiles: testutil.TestChangedFiles([]string{"env/prod.yaml"}, nil, nil),
			want:         true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{RepoURL: localURL}
			logger := log.New(nil)
			logger.SetLevel(log.FatalLevel)
			app := &App{cfg: cfg, logger: logger}

			got := app.filterAffectedApps([]cluster.Application{tt.app}, tt.changedFiles)
			if affected := len(got) == 1; affected != tt.want {
				t.Errorf("filterAffectedApps() affected = %v, want %v", affected, tt.want)
			}
		})
	}
}

func TestSourcePathsExist(t *testing.T) {
	logger := log.New(nil)
	logger.SetLevel(log.FatalLevel)
	a := &App{logger: logger}

	// Create a temp directory with a subdirectory to simulate repo structure
	tmpDir := t.TempDir()
	existingPath := "charts/my-app"
	if err := os.MkdirAll(tmpDir+"/"+existingPath, 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		app  *cluster.Application
		want bool
	}{
		{
			name: "path exists",
			app: &cluster.Application{
				Spec: cluster.ApplicationSpec{
					Source: &cluster.ApplicationSource{
						Path: existingPath,
					},
				},
			},
			want: true,
		},
		{
			name: "path does not exist",
			app: &cluster.Application{
				Spec: cluster.ApplicationSpec{
					Source: &cluster.ApplicationSource{
						Path: "charts/nonexistent",
					},
				},
			},
			want: false,
		},
		{
			name: "remote chart source - no local path needed",
			app: &cluster.Application{
				Spec: cluster.ApplicationSpec{
					Source: &cluster.ApplicationSource{
						Chart:   "nginx",
						RepoURL: "https://charts.bitnami.com/bitnami",
					},
				},
			},
			want: true,
		},
		{
			name: "empty path",
			app: &cluster.Application{
				Spec: cluster.ApplicationSpec{
					Source: &cluster.ApplicationSource{
						Path: "",
					},
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := a.sourcePathsExist(tt.app, tmpDir); got != tt.want {
				t.Errorf("sourcePathsExist() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestChangedFilesHasChangesInPath(t *testing.T) {
	tests := []struct {
		name    string
		files   *git.ChangedFiles
		dirPath string
		want    bool
	}{
		{
			name:    "file directly in path",
			files:   testutil.TestChangedFiles([]string{"app/deployment.yaml"}, nil, nil),
			dirPath: "app",
			want:    true,
		},
		{
			name:    "file in nested path",
			files:   testutil.TestChangedFiles([]string{"app/templates/deployment.yaml"}, nil, nil),
			dirPath: "app",
			want:    true,
		},
		{
			name:    "file not in path",
			files:   testutil.TestChangedFiles([]string{"other/deployment.yaml"}, nil, nil),
			dirPath: "app",
			want:    false,
		},
		{
			name:    "partial path match - should not match",
			files:   testutil.TestChangedFiles([]string{"application/deployment.yaml"}, nil, nil),
			dirPath: "app",
			want:    false,
		},
		{
			name:    "empty path - no match due to trailing slash logic",
			files:   testutil.TestChangedFiles([]string{"app/deployment.yaml"}, nil, nil),
			dirPath: "",
			want:    false, // empty path becomes "/" which doesn't match
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.files.HasChangesInPath(tt.dirPath); got != tt.want {
				t.Errorf("HasChangesInPath(%q) = %v, want %v", tt.dirPath, got, tt.want)
			}
		})
	}
}

func TestExitCodeFor(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"nil means success", nil, 0},
		{"changes present sentinel", ErrChangesPresent, 2},
		{"wrapped changes present", fmt.Errorf("run: %w", ErrChangesPresent), 2},
		{"other error", errors.New("boom"), 1},
		{"wrapped other error", fmt.Errorf("outer: %w", errors.New("boom")), 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExitCodeFor(tt.err); got != tt.want {
				t.Errorf("ExitCodeFor(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

// gitInRepo runs a git command in dir, failing the test on error.
func gitInRepo(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\noutput: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeAndCommit(t *testing.T, dir, file, content, msg string) {
	t.Helper()
	full := filepath.Join(dir, file)
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitInRepo(t, dir, "add", ".")
	gitInRepo(t, dir, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", msg)
}

// setupStaleBaseRepo builds a repo modeling the stale-local-base incident:
//
//	c1 (initial) --- c2 (upstream bump, origin/main) --- cf (feature)
//
// origin/main points at c2, but the local main branch is reset back to c1, so it
// is one commit behind origin/main. The feature branch is cut from c2.
// Returns the repo dir plus the c1 and c2 commit hashes.
func setupStaleBaseRepo(t *testing.T) (dir, c1, c2 string) {
	t.Helper()
	dir = t.TempDir()
	gitInRepo(t, dir, "init")
	writeAndCommit(t, dir, "init.txt", "init\n", "c1")
	// Ensure a local 'main' branch regardless of the default branch name.
	gitInRepo(t, dir, "checkout", "-B", "main")
	c1 = gitInRepo(t, dir, "rev-parse", "HEAD")

	// c2: the upstream bump that landed on origin/main after the PR was cut.
	writeAndCommit(t, dir, "image.txt", "v2\n", "c2 upstream bump")
	c2 = gitInRepo(t, dir, "rev-parse", "HEAD")

	// Simulate the remote-tracking ref locally (no network fetch needed).
	gitInRepo(t, dir, "update-ref", "refs/remotes/origin/main", c2)

	// Feature branch cut from c2.
	gitInRepo(t, dir, "checkout", "-b", "feature")
	writeAndCommit(t, dir, "feature.txt", "work\n", "cf feature work")

	// Reset local main back to c1: now it is 1 commit behind origin/main.
	gitInRepo(t, dir, "branch", "-f", "main", c1)

	return dir, c1, c2
}

func newQuietApp(t *testing.T, dir, base, target string) *App {
	t.Helper()
	repo, err := git.Open(dir)
	if err != nil {
		t.Fatalf("failed to open repo: %v", err)
	}
	logger := log.New(nil)
	logger.SetLevel(log.FatalLevel)
	return &App{
		repo:   repo,
		cfg:    &config.Config{BaseBranch: base, TargetBranch: target},
		logger: logger,
	}
}

func TestResolveBaseRefPrefersOriginWhenLocalStale(t *testing.T) {
	dir, c1, c2 := setupStaleBaseRepo(t)

	app := newQuietApp(t, dir, "main", "feature")
	got := app.resolveBaseRef()

	// With the fix, the base side resolves against origin/main (c2), so the
	// upstream bump is excluded from the PR diff. The stale local main (c1) would
	// have made c2 look like part of the PR.
	if got != c2 {
		t.Errorf("resolveBaseRef() = %q, want origin/main (%q); stale local base c1=%q", got, c2, c1)
	}
}

func TestResolveBaseRefExplicitOriginBase(t *testing.T) {
	dir, _, c2 := setupStaleBaseRepo(t)

	// Passing --base origin/main explicitly must keep working end-to-end.
	app := newQuietApp(t, dir, "origin/main", "feature")
	got := app.resolveBaseRef()
	if got != c2 {
		t.Errorf("resolveBaseRef() with explicit origin/main = %q, want %q", got, c2)
	}
}

func TestResolveBaseRefKeepsLocalWhenAhead(t *testing.T) {
	// Local main ahead of origin/main: keep the local base.
	dir := t.TempDir()
	gitInRepo(t, dir, "init")
	writeAndCommit(t, dir, "init.txt", "init\n", "c1")
	gitInRepo(t, dir, "checkout", "-B", "main")
	c1 := gitInRepo(t, dir, "rev-parse", "HEAD")
	// origin/main stays at c1; local main advances to c2.
	gitInRepo(t, dir, "update-ref", "refs/remotes/origin/main", c1)
	writeAndCommit(t, dir, "local.txt", "local\n", "c2 local ahead")
	c2 := gitInRepo(t, dir, "rev-parse", "HEAD")
	gitInRepo(t, dir, "checkout", "-b", "feature")
	writeAndCommit(t, dir, "feature.txt", "work\n", "cf")

	app := newQuietApp(t, dir, "main", "feature")
	got := app.resolveBaseRef()
	// merge-base(local main=c2, feature) = c2, i.e. the local (ahead) base is used.
	if got != c2 {
		t.Errorf("resolveBaseRef() = %q, want local base c2 (%q); origin was c1 (%q)", got, c2, c1)
	}
}
