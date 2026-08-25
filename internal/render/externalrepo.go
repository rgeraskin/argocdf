package render

import (
	"context"
	"fmt"
	"os"

	argoappv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"

	"github.com/rgeraskin/argocdf/internal/cluster"
	"github.com/rgeraskin/argocdf/internal/git"
)

// externalRepoKey identifies one external checkout: normalized URL + revision.
// Two sources referencing the same repo at the same revision share one clone.
type externalRepoKey struct{ url, revision string }

// sourceExternalKey builds the clone-dedup key for a source.
func sourceExternalKey(source *cluster.ApplicationSource) externalRepoKey {
	return externalRepoKey{url: git.NormalizeRepoURL(source.RepoURL), revision: source.TargetRevision}
}

// isExternalSource reports whether a source lives in a repository other than
// the one being diffed. When the local repo URL is unknown (auto-detection
// failed, or unit tests) everything is treated as local.
func isExternalSource(opts *RenderOptions, source *cluster.ApplicationSource) bool {
	return opts.RepoURL != "" && source.RepoURL != "" &&
		git.NormalizeRepoURL(source.RepoURL) != git.NormalizeRepoURL(opts.RepoURL)
}

// cloneCredsFromRepo maps a resolved Repository onto git clone credentials.
// Only HTTP(S) basic auth and bearer tokens translate; other kinds (SSH keys,
// GitHub App, workload identity) fall back to ambient git configuration.
func cloneCredsFromRepo(repo *argoappv1.Repository) *git.CloneCreds {
	if repo == nil || (repo.Username == "" && repo.Password == "" && repo.BearerToken == "") {
		return nil
	}
	return &git.CloneCreds{
		Username:    repo.Username,
		Password:    repo.Password,
		BearerToken: repo.BearerToken,
	}
}

// cloneExternalRepo materializes an external source repository at its target
// revision in a temp dir, authenticating with credentials resolved through
// --repo-creds. The returned cleanup removes the clone.
//
// revision is the clone's RESOLVED commit, which is what the repo-server renders
// an external source with — and therefore what ARGOCD_APP_REVISION* must report
// for it. The diffed repository's commit describes different content entirely,
// and being the one input guaranteed to differ between the two sides it made a
// cross-repo application diff on every PR (the bug 4aa11f4 fixed for charts and
// artifacts, one source kind further out).
//
// A revision that cannot be read back is NOT fatal: the clone succeeded, so the
// content is there and only its label is unknown. Returning empty lets the caller
// fall back to the diffed commit — the pre-existing behavior — instead of failing
// a render over a name.
func cloneExternalRepo(ctx context.Context, opts *RenderOptions, project string, source *cluster.ApplicationSource) (dir, revision string, cleanup func(), err error) {
	repo, err := resolveRepoOrBare(ctx, opts, project, source.RepoURL)
	if err != nil {
		return "", "", nil, err
	}

	tempDir, err := os.MkdirTemp("", "argocdf-ext-")
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to create temp dir for source repo %s: %w", source.RepoURL, err)
	}
	cleanup = func() { _ = SafeRemoveAll(tempDir) }

	if err := git.Clone(source.RepoURL, source.TargetRevision, tempDir, cloneCredsFromRepo(repo)); err != nil {
		cleanup()
		return "", "", nil, fmt.Errorf("failed to clone source repository %s: %w", source.RepoURL, err)
	}
	return tempDir, clonedRevision(tempDir), cleanup, nil
}

// clonedRevision reads a clone's HEAD commit, or "" when it cannot be read.
func clonedRevision(dir string) string {
	repo, err := git.Open(dir)
	if err != nil {
		return ""
	}
	commit, err := repo.CommitHash("HEAD")
	if err != nil {
		return ""
	}
	return commit
}

// externalClone is one materialized external repository: where it lives and the
// commit it resolved to.
type externalClone struct {
	path     string
	revision string
}

// externalRepoSet materializes external renderable-source repositories on
// demand, deduplicated per (URL, revision), and owns their cleanup.
type externalRepoSet struct {
	opts     *RenderOptions
	project  string
	clones   map[externalRepoKey]externalClone
	cleanups []func()
}

func newExternalRepoSet(opts *RenderOptions, project string) *externalRepoSet {
	return &externalRepoSet{opts: opts, project: project, clones: map[externalRepoKey]externalClone{}}
}

// repoPathFor returns the render root for a source: the given local repoPath,
// or a (possibly shared) checkout of the source's external repository. Chart and
// OCI-artifact sources never clone — their content comes from the chart fetch or
// the registry pull, and an oci:// URL is not a git remote, so cloning one fails
// instead of yielding the artifact.
//
// revision is the external clone's resolved commit, and EMPTY for every source
// rendered from the local worktree — the caller then keeps using the commit it is
// diffing, which for those sources is the right answer.
func (s *externalRepoSet) repoPathFor(ctx context.Context, source *cluster.ApplicationSource, localRepoPath string) (path, revision string, err error) {
	if source.Chart != "" || source.IsOCI() || !isExternalSource(s.opts, source) {
		return localRepoPath, "", nil
	}
	key := sourceExternalKey(source)
	if clone, ok := s.clones[key]; ok {
		return clone.path, clone.revision, nil
	}
	dir, cloneRevision, cleanup, err := cloneExternalRepo(ctx, s.opts, s.project, source)
	if err != nil {
		return "", "", err
	}
	s.clones[key] = externalClone{path: dir, revision: cloneRevision}
	s.cleanups = append(s.cleanups, cleanup)
	return dir, cloneRevision, nil
}

// cleanup removes every clone this set created.
func (s *externalRepoSet) cleanup() {
	for _, c := range s.cleanups {
		c()
	}
}
