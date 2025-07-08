// Package app provides tests for the main application orchestrator.
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/log"

	"github.com/rgeraskin/argocdf/internal/cluster"
	"github.com/rgeraskin/argocdf/internal/config"
	"github.com/rgeraskin/argocdf/internal/diff"
	"github.com/rgeraskin/argocdf/internal/git"
	"github.com/rgeraskin/argocdf/internal/lint"
	"github.com/rgeraskin/argocdf/internal/render"
	"github.com/rgeraskin/argocdf/internal/testutil"
	"github.com/rgeraskin/argocdf/internal/types"
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

// fakeRenderCall records one RenderApplication invocation. start/end are
// samples of a shared monotonic counter, so call intervals can be ordered
// against each other without wall-clock time.
type fakeRenderCall struct {
	app      string
	revision string // TargetRevision of the spec the app was rendered with
	start    int
	end      int
}

// fakeRenderer is an applicationRenderer that simulates a three-level
// apps-of-apps hierarchy without invoking helm/kustomize:
//
//   - "parent" renders a child Application CRD whose spec differs between the
//     base worktree (targetRevision base-values) and the target worktree
//     (target-values) — i.e. the PR modifies the child through the parent.
//   - "child" renders a ConfigMap stamped with the revision it was rendered
//     with; when rendered with the git-derived target spec it additionally
//     emits a grandchild Application CRD.
//   - "grandchild" renders a ConfigMap stamped with its revision.
//
// Every call is recorded with start/end sequence numbers so the test can
// assert wave-barrier ordering. The premature child render (cluster spec) is
// artificially slow so that an implementation without the wave barrier would
// start the corrected render while the stale one is still in flight and fail
// the ordering assertions.
type fakeRenderer struct {
	baseWorktree   string
	targetWorktree string

	mu    sync.Mutex
	seq   int
	calls []fakeRenderCall
}

func appCRD(name, revision string) string {
	return fmt.Sprintf(`apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: %s
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://example.com/org/repo.git
    chart: dummy
    targetRevision: %s
  destination:
    server: https://kubernetes.default.svc
`, name, revision)
}

func revConfigMap(name, revision string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: default
data:
  revision: %s
`, name, revision)
}

func (f *fakeRenderer) RenderApplication(
	_ context.Context,
	app *cluster.Application,
	repoPath string,
) (*render.RenderResult, error) {
	revision := app.Spec.GetSources()[0].TargetRevision

	f.mu.Lock()
	f.seq++
	call := fakeRenderCall{app: app.Name, revision: revision, start: f.seq}
	f.mu.Unlock()

	// Slow down the premature (cluster-spec) child render; see type comment.
	if app.Name == "child" && revision == "cluster" {
		time.Sleep(50 * time.Millisecond)
	}

	var manifests string
	switch app.Name {
	case "parent":
		crdRevision := "base-values"
		if repoPath == f.targetWorktree {
			crdRevision = "target-values"
		}
		manifests = appCRD("child", crdRevision)
	case "child":
		manifests = revConfigMap("child-cm", revision)
		if revision == "target-values" {
			manifests += "---\n" + appCRD("grandchild", "grandchild-values")
		}
	case "grandchild":
		manifests = revConfigMap("grandchild-cm", revision)
	}

	f.mu.Lock()
	f.seq++
	call.end = f.seq
	f.calls = append(f.calls, call)
	f.mu.Unlock()

	return &render.RenderResult{
		Manifests:  []byte(manifests),
		SourceType: types.SourceTypeHelm,
	}, nil
}

// TestProcessApplicationsWaveBarrier pins the wave-barrier invariant of
// processApplications that the concurrency model depends on: renders are
// parallel only WITHIN a wave, and child discovery / requeueing runs strictly
// between waves. Concretely it asserts, for a PR that changes a parent and
// (through it) a child and a grandchild:
//
//  1. A child rendered prematurely with its cluster spec (same wave as the
//     parent) is re-rendered with the git-derived specs extracted from the
//     parent's base/target renders, and that corrected result is what remains.
//  2. The corrected child render starts only after ALL wave-0 renders have
//     finished (the barrier), never concurrently with the stale render.
//  3. A grandchild discovered from the corrected child render runs in a
//     strictly later wave, i.e. chains propagate level by level.
//
// If this test starts failing after a concurrency change (e.g. starting the
// next wave before the current one drains, or running discovery inside render
// goroutines), the requeue/dedup/cycle-guard semantics are broken.
func TestProcessApplicationsWaveBarrier(t *testing.T) {
	logger := log.New(nil)
	logger.SetLevel(log.FatalLevel)

	cfg := &config.Config{Concurrency: 4, MaxDepth: 5}
	fake := &fakeRenderer{
		baseWorktree:   "/fake/base",
		targetWorktree: "/fake/target",
	}

	a := &App{
		factory:        NewFactory(cfg, logger),
		cfg:            cfg,
		logger:         logger,
		renderer:       fake,
		differ:         diff.NewManifestDiffer(),
		discoverer:     diff.NewAppDiscoverer(),
		baseWorktree:   fake.baseWorktree,
		targetWorktree: fake.targetWorktree,
	}

	// Both parent and child are directly affected, so both land in wave 0 and
	// the child initially renders with its cluster spec.
	clusterSpec := func() cluster.ApplicationSpec {
		return cluster.ApplicationSpec{
			Source: &cluster.ApplicationSource{
				RepoURL:        "https://example.com/org/repo.git",
				Chart:          "dummy",
				TargetRevision: "cluster",
			},
		}
	}
	parent := cluster.Application{Spec: clusterSpec()}
	parent.Name = "parent"
	parent.Namespace = "argocd"
	child := cluster.Application{Spec: clusterSpec()}
	child.Name = "child"
	child.Namespace = "argocd"

	diffs, err := a.processApplications(context.Background(), []cluster.Application{parent, child})
	if err != nil {
		t.Fatalf("processApplications() error: %v", err)
	}

	byName := make(map[string]*types.AppDiff, len(diffs))
	for _, d := range diffs {
		if d.Error != nil {
			t.Errorf("app %q finished with error: %v", d.Name, d.Error)
		}
		byName[d.Name] = d
	}
	for _, name := range []string{"parent", "child", "grandchild"} {
		if byName[name] == nil {
			t.Fatalf("missing result for app %q; got %d results", name, len(diffs))
		}
	}
	if len(diffs) != 3 {
		t.Fatalf("got %d results, want 3 (stale child result must be overwritten, not duplicated)", len(diffs))
	}

	// (1) The surviving child result must come from the git-derived specs
	// (base-values -> target-values), not the premature cluster-spec render.
	childDiff := byName["child"]
	if !strings.Contains(childDiff.RenderedOld, "revision: base-values") {
		t.Errorf("child RenderedOld not rendered with parent's base spec:\n%s", childDiff.RenderedOld)
	}
	if !strings.Contains(childDiff.RenderedNew, "revision: target-values") {
		t.Errorf("child RenderedNew not rendered with parent's target spec:\n%s", childDiff.RenderedNew)
	}
	if strings.Contains(childDiff.RenderedOld+childDiff.RenderedNew, "revision: cluster") {
		t.Error("stale cluster-spec render survived as the child's result")
	}
	if childDiff.ParentAppName != "parent" {
		t.Errorf("child ParentAppName = %q, want %q", childDiff.ParentAppName, "parent")
	}
	if byName["grandchild"].ParentAppName != "child" {
		t.Errorf("grandchild ParentAppName = %q, want %q", byName["grandchild"].ParentAppName, "child")
	}

	// Partition the recorded render calls into waves by their meaning:
	// wave 0 = parent renders + premature child renders (cluster spec),
	// wave 1 = corrected child renders (git-derived specs),
	// wave 2 = grandchild renders.
	fake.mu.Lock()
	calls := fake.calls
	fake.mu.Unlock()

	maxEnd := func(match func(fakeRenderCall) bool) int {
		v := -1
		for _, c := range calls {
			if match(c) && c.end > v {
				v = c.end
			}
		}
		return v
	}
	minStart := func(match func(fakeRenderCall) bool) int {
		v := -1
		for _, c := range calls {
			if match(c) && (v == -1 || c.start < v) {
				v = c.start
			}
		}
		return v
	}

	wave0End := maxEnd(func(c fakeRenderCall) bool {
		return c.app == "parent" || (c.app == "child" && c.revision == "cluster")
	})
	childGitStart := minStart(func(c fakeRenderCall) bool {
		return c.app == "child" && c.revision != "cluster"
	})
	childGitEnd := maxEnd(func(c fakeRenderCall) bool {
		return c.app == "child" && c.revision != "cluster"
	})
	grandchildStart := minStart(func(c fakeRenderCall) bool {
		return c.app == "grandchild"
	})

	if childGitStart == -1 {
		t.Fatal("child was never re-rendered with git-derived specs (requeue did not happen)")
	}
	if grandchildStart == -1 {
		t.Fatal("grandchild was never rendered (chain did not propagate)")
	}

	// (2) The wave barrier: the corrected child render must start only after
	// every wave-0 render (including the slow stale child render) has ended.
	if childGitStart < wave0End {
		t.Errorf("wave barrier violated: corrected child render started (seq %d) before wave 0 finished (seq %d)",
			childGitStart, wave0End)
	}

	// (3) Chain propagation: the grandchild renders in a strictly later wave
	// than the corrected child render that discovered it.
	if grandchildStart < childGitEnd {
		t.Errorf("wave barrier violated: grandchild render started (seq %d) before its parent's wave finished (seq %d)",
			grandchildStart, childGitEnd)
	}

	// The child renders exactly twice per side: once prematurely, once corrected.
	childCalls := 0
	for _, c := range calls {
		if c.app == "child" {
			childCalls++
		}
	}
	if childCalls != 4 {
		t.Errorf("child rendered %d times, want 4 (base+target, premature and corrected)", childCalls)
	}
}

// newChildFakeRenderer simulates a parent app that adds a brand-new child
// Application on the target branch. Rendering the new child against the base
// worktree fails hard — mimicking helm erroring on a values file that exists
// only on the target branch (the child's chart directory pre-exists on base,
// so the sourcePathsExist guard would not catch it).
type newChildFakeRenderer struct {
	baseWorktree   string
	targetWorktree string

	mu          sync.Mutex
	baseRenders []string // app names rendered against the base worktree
}

func (f *newChildFakeRenderer) RenderApplication(
	_ context.Context,
	app *cluster.Application,
	repoPath string,
) (*render.RenderResult, error) {
	if repoPath == f.baseWorktree {
		f.mu.Lock()
		f.baseRenders = append(f.baseRenders, app.Name)
		f.mu.Unlock()
	}

	var manifests string
	switch app.Name {
	case "parent":
		// The PR adds the child through the parent: no child CRD on base.
		if repoPath == f.targetWorktree {
			manifests = appCRD("new-child", "v1")
		}
	case "new-child":
		if repoPath == f.baseWorktree {
			return nil, fmt.Errorf(
				"helm template failed: open %s/values-pt1.yaml: no such file or directory", repoPath)
		}
		manifests = revConfigMap("new-child-cm", "v1")
	}

	return &render.RenderResult{
		Manifests:  []byte(manifests),
		SourceType: types.SourceTypeHelm,
	}, nil
}

// TestProcessApplicationsNewChildSkipsBaseRender pins the IsNew contract: a
// child app discovered only on the target branch must never be rendered
// against the base worktree. Rendering it there with the target spec (the only
// spec that exists) can fail hard when the spec references files absent on
// base — e.g. a newly added values file in a pre-existing chart directory —
// and is semantically wrong anyway: a new app has no base state, so its diff
// is everything-added.
func TestProcessApplicationsNewChildSkipsBaseRender(t *testing.T) {
	logger := log.New(nil)
	logger.SetLevel(log.FatalLevel)

	cfg := &config.Config{Concurrency: 4, MaxDepth: 5}
	fake := &newChildFakeRenderer{
		baseWorktree:   "/fake/base",
		targetWorktree: "/fake/target",
	}

	a := &App{
		factory:        NewFactory(cfg, logger),
		cfg:            cfg,
		logger:         logger,
		renderer:       fake,
		differ:         diff.NewManifestDiffer(),
		discoverer:     diff.NewAppDiscoverer(),
		baseWorktree:   fake.baseWorktree,
		targetWorktree: fake.targetWorktree,
	}

	parent := cluster.Application{Spec: cluster.ApplicationSpec{
		Source: &cluster.ApplicationSource{
			RepoURL:        "https://example.com/org/repo.git",
			Chart:          "dummy",
			TargetRevision: "cluster",
		},
	}}
	parent.Name = "parent"
	parent.Namespace = "argocd"

	diffs, err := a.processApplications(context.Background(), []cluster.Application{parent})
	if err != nil {
		t.Fatalf("processApplications() error: %v", err)
	}

	byName := make(map[string]*types.AppDiff, len(diffs))
	for _, d := range diffs {
		byName[d.Name] = d
	}
	childDiff := byName["new-child"]
	if childDiff == nil {
		t.Fatalf("new-child was not discovered; got %d results", len(diffs))
	}

	// The base render must be skipped, not attempted-and-failed.
	if childDiff.Error != nil {
		t.Fatalf("new-child finished with error (base render was attempted?): %v", childDiff.Error)
	}
	fake.mu.Lock()
	baseRenders := fake.baseRenders
	fake.mu.Unlock()
	for _, name := range baseRenders {
		if name == "new-child" {
			t.Error("new-child was rendered against the base worktree; want base render skipped")
		}
	}

	// An empty base side makes the whole app diff as added.
	if childDiff.RenderedOld != "" {
		t.Errorf("new-child RenderedOld = %q, want empty", childDiff.RenderedOld)
	}
	if !strings.Contains(childDiff.RenderedNew, "new-child-cm") {
		t.Errorf("new-child RenderedNew missing expected manifest:\n%s", childDiff.RenderedNew)
	}
	setDiff, ok := childDiff.DiffResult.(*diff.ManifestSetDiff)
	if !ok {
		t.Fatalf("new-child DiffResult is %T, want *diff.ManifestSetDiff", childDiff.DiffResult)
	}
	if len(setDiff.Added) != 1 || len(setDiff.Removed) != 0 || len(setDiff.Modified) != 0 {
		t.Errorf("new-child diff = +%d -%d ~%d, want +1 -0 ~0",
			len(setDiff.Added), len(setDiff.Removed), len(setDiff.Modified))
	}
	if childDiff.SourceType != types.SourceTypeHelm {
		t.Errorf("new-child SourceType = %q, want %q (from the target render)",
			childDiff.SourceType, types.SourceTypeHelm)
	}
}

// sideStampRenderer renders a ConfigMap whose data marks which worktree it was
// rendered from, so lint tests can assert per-side warning attribution.
type sideStampRenderer struct {
	baseWorktree   string
	targetWorktree string
}

func (f *sideStampRenderer) RenderApplication(
	_ context.Context,
	_ *cluster.Application,
	repoPath string,
) (*render.RenderResult, error) {
	side := "base"
	if repoPath == f.targetWorktree {
		side = "target"
	}
	return &render.RenderResult{
		Manifests:  []byte(revConfigMap("stamp-cm", side+"-rev")),
		SourceType: types.SourceTypeHelm,
	}, nil
}

// lintWorktrees creates real base/target worktree directories, each holding a
// policy-note.txt naming its side, so tests can assert that lint commands run
// with the side's worktree as their working directory.
func lintWorktrees(t *testing.T) (base, target string) {
	t.Helper()
	base, target = t.TempDir(), t.TempDir()
	for dir, side := range map[string]string{base: "base", target: "target"} {
		if err := os.WriteFile(filepath.Join(dir, "policy-note.txt"), []byte("policy-of-"+side+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return base, target
}

// TestProcessOneAppLintsBothSides pins the lint contract: each side's rendered
// manifests are piped to the lint command, its stdout lines land in
// ParseWarnings under the side's [base]/[target] label, and the command runs
// with that side's worktree as its working directory (so repo-relative policy
// paths resolve to the side's version of the files).
func TestProcessOneAppLintsBothSides(t *testing.T) {
	logger := log.New(nil)
	logger.SetLevel(log.FatalLevel)

	cfg := &config.Config{Concurrency: 1, MaxDepth: 5}
	baseWT, targetWT := lintWorktrees(t)
	fake := &sideStampRenderer{baseWorktree: baseWT, targetWorktree: targetWT}

	a := &App{
		factory:        NewFactory(cfg, logger),
		cfg:            cfg,
		logger:         logger,
		renderer:       fake,
		differ:         diff.NewManifestDiffer(),
		discoverer:     diff.NewAppDiscoverer(),
		linter:         &lint.Runner{Commands: []string{`grep "revision:"; cat policy-note.txt`}, Timeout: 5 * time.Second},
		baseWorktree:   fake.baseWorktree,
		targetWorktree: fake.targetWorktree,
	}

	spec := cluster.ApplicationSpec{
		Source: &cluster.ApplicationSource{
			RepoURL:        "https://example.com/org/repo.git",
			Chart:          "dummy",
			TargetRevision: "main",
		},
	}
	appDiff, err := a.processOneApp(context.Background(), &diff.QueuedApp{
		Name:      "linted",
		Namespace: "argocd",
		Spec:      &spec,
	})
	if err != nil {
		t.Fatalf("processOneApp() error: %v", err)
	}

	setDiff, ok := appDiff.DiffResult.(*diff.ManifestSetDiff)
	if !ok {
		t.Fatalf("DiffResult is %T, want *diff.ManifestSetDiff", appDiff.DiffResult)
	}
	want := []string{
		"[base] revision: base-rev",
		"[base] policy-of-base",
		"[target] revision: target-rev",
		"[target] policy-of-target",
	}
	if len(setDiff.ParseWarnings) != len(want) {
		t.Fatalf("ParseWarnings = %v, want %v", setDiff.ParseWarnings, want)
	}
	for i, w := range want {
		if setDiff.ParseWarnings[i] != w {
			t.Errorf("ParseWarnings[%d] = %q, want %q", i, setDiff.ParseWarnings[i], w)
		}
	}
}

// TestProcessOneAppLintSkipsEmptyBase verifies that a newly-discovered app
// (no base render) is linted on the target side only.
func TestProcessOneAppLintSkipsEmptyBase(t *testing.T) {
	logger := log.New(nil)
	logger.SetLevel(log.FatalLevel)

	cfg := &config.Config{Concurrency: 1, MaxDepth: 5}
	baseWT, targetWT := lintWorktrees(t)
	fake := &sideStampRenderer{baseWorktree: baseWT, targetWorktree: targetWT}

	a := &App{
		factory:        NewFactory(cfg, logger),
		cfg:            cfg,
		logger:         logger,
		renderer:       fake,
		differ:         diff.NewManifestDiffer(),
		discoverer:     diff.NewAppDiscoverer(),
		linter:         &lint.Runner{Commands: []string{`grep "revision:"`}, Timeout: 5 * time.Second},
		baseWorktree:   fake.baseWorktree,
		targetWorktree: fake.targetWorktree,
	}

	spec := cluster.ApplicationSpec{
		Source: &cluster.ApplicationSource{
			RepoURL:        "https://example.com/org/repo.git",
			Chart:          "dummy",
			TargetRevision: "main",
		},
	}
	appDiff, err := a.processOneApp(context.Background(), &diff.QueuedApp{
		Name:      "new-app",
		Namespace: "argocd",
		Spec:      &spec,
		IsNew:     true,
	})
	if err != nil {
		t.Fatalf("processOneApp() error: %v", err)
	}

	setDiff := appDiff.DiffResult.(*diff.ManifestSetDiff)
	if len(setDiff.ParseWarnings) != 1 || setDiff.ParseWarnings[0] != "[target] revision: target-rev" {
		t.Errorf("ParseWarnings = %v, want only the [target] lint line", setDiff.ParseWarnings)
	}
}

// emptySideRenderer simulates a deleted app: the base render has content, the
// target render is empty.
type emptySideRenderer struct {
	baseWorktree   string
	targetWorktree string
}

func (f *emptySideRenderer) RenderApplication(
	_ context.Context,
	_ *cluster.Application,
	repoPath string,
) (*render.RenderResult, error) {
	if repoPath == f.targetWorktree {
		return &render.RenderResult{Manifests: nil, SourceType: types.SourceTypePlain}, nil
	}
	return &render.RenderResult{
		Manifests:  []byte(revConfigMap("stamp-cm", "base-rev")),
		SourceType: types.SourceTypeHelm,
	}, nil
}

// TestProcessOneAppLintSkipsEmptyTarget verifies that a deleted app (empty
// target render) is linted on the base side only.
func TestProcessOneAppLintSkipsEmptyTarget(t *testing.T) {
	logger := log.New(nil)
	logger.SetLevel(log.FatalLevel)

	cfg := &config.Config{Concurrency: 1, MaxDepth: 5}
	baseWT, targetWT := lintWorktrees(t)
	fake := &emptySideRenderer{baseWorktree: baseWT, targetWorktree: targetWT}

	a := &App{
		factory:        NewFactory(cfg, logger),
		cfg:            cfg,
		logger:         logger,
		renderer:       fake,
		differ:         diff.NewManifestDiffer(),
		discoverer:     diff.NewAppDiscoverer(),
		linter:         &lint.Runner{Commands: []string{`grep "revision:"`}, Timeout: 5 * time.Second},
		baseWorktree:   fake.baseWorktree,
		targetWorktree: fake.targetWorktree,
	}

	spec := cluster.ApplicationSpec{
		Source: &cluster.ApplicationSource{
			RepoURL:        "https://example.com/org/repo.git",
			Chart:          "dummy",
			TargetRevision: "main",
		},
	}
	appDiff, err := a.processOneApp(context.Background(), &diff.QueuedApp{
		Name:      "deleted-app",
		Namespace: "argocd",
		Spec:      &spec,
	})
	if err != nil {
		t.Fatalf("processOneApp() error: %v", err)
	}

	setDiff := appDiff.DiffResult.(*diff.ManifestSetDiff)
	if len(setDiff.ParseWarnings) != 1 || setDiff.ParseWarnings[0] != "[base] revision: base-rev" {
		t.Errorf("ParseWarnings = %v, want only the [base] lint line", setDiff.ParseWarnings)
	}
	if len(setDiff.Removed) != 1 {
		t.Errorf("expected the app's resource to diff as removed, got -%d", len(setDiff.Removed))
	}
}
