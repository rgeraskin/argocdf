package render

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	argoappv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"

	"github.com/rgeraskin/argocdf/internal/cluster"
)

func TestExternalRepoSet_DedupAndCleanup(t *testing.T) {
	extRepo := t.TempDir()
	if err := os.WriteFile(filepath.Join(extRepo, "file.txt"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	extURL := initGitRepo(t, extRepo)

	opts := &RenderOptions{RepoURL: "https://github.com/org/repo"}
	set := newExternalRepoSet(opts, "default")
	ctx := context.Background()

	// Two sources, same URL + revision: one clone, shared.
	first, _, err := set.repoPathFor(ctx, &cluster.ApplicationSource{RepoURL: extURL, Path: "a"}, "/local")
	if err != nil {
		t.Fatalf("repoPathFor() error: %v", err)
	}
	second, _, err := set.repoPathFor(ctx, &cluster.ApplicationSource{RepoURL: extURL, Path: "b"}, "/local")
	if err != nil {
		t.Fatalf("repoPathFor() second call error: %v", err)
	}
	if first != second {
		t.Errorf("same URL+revision produced two clones: %q vs %q", first, second)
	}
	if first == "/local" {
		t.Error("external source was served from the local worktree")
	}
	if len(set.cleanups) != 1 {
		t.Errorf("cleanups = %d, want 1 (deduplicated clone)", len(set.cleanups))
	}
	if _, err := os.Stat(filepath.Join(first, "file.txt")); err != nil {
		t.Errorf("clone content missing: %v", err)
	}

	// A different revision is a different checkout.
	third, _, err := set.repoPathFor(ctx, &cluster.ApplicationSource{RepoURL: extURL, Path: "a", TargetRevision: "main"}, "/local")
	if err != nil {
		t.Fatalf("repoPathFor() revision call error: %v", err)
	}
	if third == first {
		t.Error("different revisions must not share a checkout")
	}

	// Local and chart sources never clone.
	local, _, err := set.repoPathFor(ctx, &cluster.ApplicationSource{RepoURL: "https://github.com/org/repo", Path: "x"}, "/local")
	if err != nil || local != "/local" {
		t.Errorf("local source = (%q, %v), want the local worktree", local, err)
	}
	chart, _, err := set.repoPathFor(ctx, &cluster.ApplicationSource{RepoURL: extURL, Chart: "c"}, "/local")
	if err != nil || chart != "/local" {
		t.Errorf("chart source = (%q, %v), want the local worktree (charts come from the chart fetch)", chart, err)
	}

	set.cleanup()
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Error("cleanup did not remove the clone")
	}
}

func TestCloneExternalRepo_ResolveErrorIsLoud(t *testing.T) {
	opts := &RenderOptions{
		RepoURL: "https://github.com/org/repo",
		ResolveRepo: func(context.Context, string, string) (*argoappv1.Repository, error) {
			return nil, errors.New("token exchange failed")
		},
	}
	_, _, _, err := cloneExternalRepo(context.Background(), opts, "default",
		&cluster.ApplicationSource{RepoURL: "https://github.com/org/other", Path: "x"})
	if err == nil || !strings.Contains(err.Error(), "token exchange failed") {
		t.Errorf("cloneExternalRepo() error = %v, want the credential resolution root cause", err)
	}
}
