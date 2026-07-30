package app

import (
	"testing"

	"github.com/charmbracelet/log"

	"github.com/rgeraskin/argocdf/internal/cluster"
	"github.com/rgeraskin/argocdf/internal/config"
	"github.com/rgeraskin/argocdf/internal/git"
	"github.com/rgeraskin/argocdf/internal/testutil"
)

// TestFilterAffectedApps_LocalHelmFiles covers value files and fileParameters that
// live in the repo being diffed but OUTSIDE the source path - the shared-values-file
// layout (apps/*/chart reading ../shared/vals.yaml). ArgoCD renders those, so a
// change to one must select the app; source-path containment cannot see it.
func TestFilterAffectedApps_LocalHelmFiles(t *testing.T) {
	const localURL = "https://github.com/org/repo"

	// appWith builds a single-source app around the given source.
	appWith := func(source cluster.ApplicationSource) cluster.Application {
		app := testutil.TestApp("my-app", "argocd", localURL, source.Path)
		app.Spec.Source = &source
		return app
	}
	modified := func(paths ...string) *git.ChangedFiles {
		return testutil.TestChangedFiles(nil, paths, nil)
	}
	added := func(paths ...string) *git.ChangedFiles {
		return testutil.TestChangedFiles(paths, nil, nil)
	}
	deleted := func(paths ...string) *git.ChangedFiles {
		return testutil.TestChangedFiles(nil, nil, paths)
	}

	// gitSource is a chart at apps/chart IN the repo being diffed.
	gitSource := func(valueFiles ...string) cluster.ApplicationSource {
		return cluster.ApplicationSource{
			RepoURL: localURL,
			Path:    "apps/chart",
			Helm:    &cluster.ApplicationSourceHelm{ValueFiles: valueFiles},
		}
	}

	tests := []struct {
		name         string
		app          cluster.Application
		changedFiles *git.ChangedFiles
		want         bool
	}{
		{
			// The headline case: only the escaping values file changed.
			name:         "value file escaping the source path changed - affected",
			app:          appWith(gitSource("values.yaml", "../shared/vals.yaml")),
			changedFiles: modified("apps/shared/vals.yaml"),
			want:         true,
		},
		{
			// An absolute entry is repo-root-relative in ArgoCD, not
			// filesystem-absolute.
			name:         "absolute value file resolves against the repo root - affected",
			app:          appWith(gitSource("/config/prod.yaml")),
			changedFiles: modified("config/prod.yaml"),
			want:         true,
		},
		{
			name:         "escaping value file added - affected",
			app:          appWith(gitSource("../shared/vals.yaml")),
			changedFiles: added("apps/shared/vals.yaml"),
			want:         true,
		},
		{
			name:         "escaping value file deleted - affected",
			app:          appWith(gitSource("../shared/vals.yaml")),
			changedFiles: deleted("apps/shared/vals.yaml"),
			want:         true,
		},
		{
			// fileParameters take the same resolution as value files upstream.
			name: "fileParameter escaping the source path changed - affected",
			app: appWith(func() cluster.ApplicationSource {
				s := gitSource()
				s.Helm.FileParameters = []cluster.HelmFileParameter{
					{Name: "image.tag", Path: "../shared/version.txt"},
				}
				return s
			}()),
			changedFiles: modified("apps/shared/version.txt"),
			want:         true,
		},
		{
			// fileParameters take the absolute (repo-root) form too, not just the
			// escaping one.
			name: "absolute fileParameter resolves against the repo root - affected",
			app: appWith(func() cluster.ApplicationSource {
				s := gitSource()
				s.Helm.FileParameters = []cluster.HelmFileParameter{
					{Name: "image.tag", Path: "/config/version.txt"},
				}
				return s
			}()),
			changedFiles: modified("config/version.txt"),
			want:         true,
		},
		{
			// Resolution must not collapse to "any changed file wins": a
			// neighbour of the referenced file is not a match.
			name:         "sibling of the escaping value file changed - not affected",
			app:          appWith(gitSource("../shared/vals.yaml")),
			changedFiles: modified("apps/shared/other.yaml"),
			want:         false,
		},
		{
			// ArgoCD refuses this entry (outside repository root) and so does the
			// matcher - an app that cannot render must not be silently attributed
			// a match either.
			name:         "value file escaping the repository - not affected",
			app:          appWith(gitSource("../../../outside.yaml")),
			changedFiles: modified("outside.yaml"),
			want:         false,
		},
		{
			name:         "remote value file over https - not affected",
			app:          appWith(gitSource("https://example.test/vals.yaml")),
			changedFiles: modified("vals.yaml"),
			want:         false,
		},
		{
			// The same source path in ANOTHER repository: the file changed here
			// is not the file that app reads.
			name: "source in a foreign repository - not affected",
			app: appWith(func() cluster.ApplicationSource {
				s := gitSource("../shared/vals.yaml")
				s.RepoURL = "https://github.com/other/repo"
				return s
			}()),
			changedFiles: modified("apps/shared/vals.yaml"),
			want:         false,
		},
		{
			// manifest-generate-paths REPLACES argocdf's own matching, so it
			// replaces this too - declaring the chart path alone means the
			// escaping values file stops being tracked, exactly as in ArgoCD.
			name: "declared paths replace this matching - not affected",
			app: func() cluster.Application {
				app := appWith(gitSource("../shared/vals.yaml"))
				app.Annotations = map[string]string{
					cluster.AnnotationKeyManifestGeneratePaths: "/apps/chart",
				}
				return app
			}(),
			changedFiles: modified("apps/shared/vals.yaml"),
			want:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := log.New(nil)
			logger.SetLevel(log.FatalLevel)
			a := &App{cfg: &config.Config{RepoURL: localURL}, logger: logger}

			got := a.filterAffectedApps([]cluster.Application{tt.app}, tt.changedFiles)
			if affected := len(got) == 1; affected != tt.want {
				t.Errorf("filterAffectedApps() affected = %v, want %v", affected, tt.want)
			}
		})
	}
}

// TestHelmLocalFilesAffected_RemoteChart exercises the remote-chart exclusion
// directly, because filterAffectedApps cannot reach it: a chart source's RepoURL is
// a helm repository, so the repo-URL check rejects the source first. The guard is
// about the RULE rather than that path - a chart source's relative value files
// resolve inside the EXTRACTED chart, so they are not repository paths at all, and
// with the empty Path such a source carries, "vals.yaml" would otherwise resolve to
// the repository root and match an unrelated file sitting there.
func TestHelmLocalFilesAffected_RemoteChart(t *testing.T) {
	logger := log.New(nil)
	logger.SetLevel(log.FatalLevel)
	a := &App{cfg: &config.Config{}, logger: logger}

	chartSource := cluster.ApplicationSource{
		RepoURL: "ghcr.io/org/charts",
		Chart:   "app",
		Helm:    &cluster.ApplicationSourceHelm{ValueFiles: []string{"vals.yaml"}},
	}
	if a.helmLocalFilesAffected(chartSource, []string{"vals.yaml"}) {
		t.Error("helmLocalFilesAffected() = true for a remote chart source, want false: " +
			"its value files live in the extracted chart, not the repository")
	}

	// Same entry, same empty path, but a git source: now it IS a repository path
	// (the repo root), which is what makes the exclusion above load-bearing.
	gitRootSource := cluster.ApplicationSource{
		RepoURL: "https://github.com/org/repo",
		Helm:    &cluster.ApplicationSourceHelm{ValueFiles: []string{"vals.yaml"}},
	}
	if !a.helmLocalFilesAffected(gitRootSource, []string{"vals.yaml"}) {
		t.Error("helmLocalFilesAffected() = false for a git source at the repo root, want true")
	}
}
