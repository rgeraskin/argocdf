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
func cloneExternalRepo(ctx context.Context, opts *RenderOptions, project string, source *cluster.ApplicationSource) (string, func(), error) {
	repo, err := resolveRepoOrBare(ctx, opts, project, source.RepoURL)
	if err != nil {
		return "", nil, err
	}

	tempDir, err := os.MkdirTemp("", "argocdf-ext-")
	if err != nil {
		return "", nil, fmt.Errorf("failed to create temp dir for source repo %s: %w", source.RepoURL, err)
	}
	cleanup := func() { _ = SafeRemoveAll(tempDir) }

	if err := git.CloneWithCreds(source.RepoURL, source.TargetRevision, tempDir, cloneCredsFromRepo(repo)); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("failed to clone source repository %s: %w", source.RepoURL, err)
	}
	return tempDir, cleanup, nil
}

// externalRepoSet materializes external renderable-source repositories on
// demand, deduplicated per (URL, revision), and owns their cleanup.
type externalRepoSet struct {
	opts     *RenderOptions
	project  string
	clones   map[externalRepoKey]string
	cleanups []func()
}

func newExternalRepoSet(opts *RenderOptions, project string) *externalRepoSet {
	return &externalRepoSet{opts: opts, project: project, clones: map[externalRepoKey]string{}}
}

// repoPathFor returns the render root for a source: the given local repoPath,
// or a (possibly shared) checkout of the source's external repository. Chart and
// OCI-artifact sources never clone — their content comes from the chart fetch or
// the registry pull, and an oci:// URL is not a git remote, so cloning one fails
// instead of yielding the artifact.
func (s *externalRepoSet) repoPathFor(ctx context.Context, source *cluster.ApplicationSource, localRepoPath string) (string, error) {
	if source.Chart != "" || source.IsOCI() || !isExternalSource(s.opts, source) {
		return localRepoPath, nil
	}
	key := sourceExternalKey(source)
	if path, ok := s.clones[key]; ok {
		return path, nil
	}
	path, cleanup, err := cloneExternalRepo(ctx, s.opts, s.project, source)
	if err != nil {
		return "", err
	}
	s.clones[key] = path
	s.cleanups = append(s.cleanups, cleanup)
	return path, nil
}

// cleanup removes every clone this set created.
func (s *externalRepoSet) cleanup() {
	for _, c := range s.cleanups {
		c()
	}
}
