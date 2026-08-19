package render

import (
	"context"
	"errors"
	"strings"
	"testing"

	argohelm "github.com/argoproj/argo-cd/v3/util/helm"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rgeraskin/argocdf/internal/cluster"
)

// revisionChart renders whatever ARGOCD_APP_REVISION resolved to, so a render
// can be asked what the build env said. `--set rev=$ARGOCD_APP_REVISION` is
// substituted by ArgoCD's own build-env pass (repository.go:1282).
func revisionChart() map[string]string {
	return map[string]string{
		"Chart.yaml":  "apiVersion: v2\nname: revchart\nversion: 0.1.0\n",
		"values.yaml": "rev: unset\n",
		"templates/cm.yaml": `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}-rev
data:
  rev: {{ .Values.rev | quote }}
`,
	}
}

func revisionApp(source cluster.ApplicationSource) *cluster.Application {
	source.Helm = &cluster.ApplicationSourceHelm{
		Parameters: []cluster.HelmParameter{{Name: "rev", Value: "$ARGOCD_APP_REVISION"}},
	}
	return &cluster.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "eng-app", Namespace: "argocd"},
		Spec: cluster.ApplicationSpec{
			Source:      &source,
			Destination: cluster.ApplicationDestination{Namespace: "apps"},
		},
	}
}

// renderedRevision renders app and returns what ARGOCD_APP_REVISION expanded to.
func renderedRevision(t *testing.T, app *cluster.Application, repoDir string) string {
	t.Helper()
	r := mustNewArgoCDRenderer(t, RenderOptions{ArgoCDNamespace: "argocd"})
	result, err := r.RenderApplication(context.Background(), app, repoDir, testRevision)
	if err != nil {
		t.Fatalf("RenderApplication() error = %v", err)
	}
	doc, ok := parseRendered(t, result.Manifests)["ConfigMap/eng-app-rev"]
	if !ok {
		t.Fatalf("missing ConfigMap/eng-app-rev; rendered:\n%s", result.Manifests)
	}
	got, ok := fieldAt(doc, "data.rev")
	if !ok {
		t.Fatal("data.rev not found")
	}
	return got.(string)
}

// TestBuildEnvRevisionIsPerSource pins WHICH revision each source type reports
// through ARGOCD_APP_REVISION. ArgoCD resolves it per source in
// runRepoOperation — the commit for a git source, the resolved chart version for
// a chart, the artifact digest for an OCI source — and passes that to
// GenerateManifests. argocdf passed the git commit for every source, which both
// mislabelled remote renders and made two sides pulling the SAME pinned chart
// differ, since the commit is the one input guaranteed to differ between them.
func TestBuildEnvRevisionIsPerSource(t *testing.T) {
	requireHelm(t)

	t.Run("git source reports the commit", func(t *testing.T) {
		repoDir := writeTree(t, map[string]string{
			"chart/Chart.yaml":        revisionChart()["Chart.yaml"],
			"chart/values.yaml":       revisionChart()["values.yaml"],
			"chart/templates/cm.yaml": revisionChart()["templates/cm.yaml"],
		})
		app := revisionApp(cluster.ApplicationSource{
			RepoURL: "https://github.com/acme/repo", Path: "chart", TargetRevision: "HEAD",
		})
		if got := renderedRevision(t, app, repoDir); got != testRevision {
			t.Errorf("ARGOCD_APP_REVISION = %q, want the git commit %q", got, testRevision)
		}
	})

	t.Run("chart source reports the chart version", func(t *testing.T) {
		fake := &fakeChartClient{extractedDir: writeTree(t, revisionChart())}
		stubNewChartClient(t, fake, nil, nil)

		app := revisionApp(cluster.ApplicationSource{
			RepoURL: "ghcr.io/acme", Chart: "revchart", TargetRevision: "1.2.3",
		})
		got := renderedRevision(t, app, t.TempDir())
		if got == testRevision {
			t.Fatal("ARGOCD_APP_REVISION is the git commit for a chart source; two sides pulling the same pinned chart would differ")
		}
		if got != "1.2.3" {
			t.Errorf("ARGOCD_APP_REVISION = %q, want the chart version 1.2.3", got)
		}
	})

	t.Run("oci artifact source reports the digest", func(t *testing.T) {
		fake := serveOCIArtifact(t, writeTree(t, revisionChart()))

		app := revisionApp(cluster.ApplicationSource{
			RepoURL: "oci://ghcr.io/acme/revchart", TargetRevision: "6.7.0",
		})
		got := renderedRevision(t, app, t.TempDir())
		if got == testRevision {
			t.Fatal("ARGOCD_APP_REVISION is the git commit for an OCI artifact source")
		}
		if got != fake.digest {
			t.Errorf("ARGOCD_APP_REVISION = %q, want the resolved digest %q", got, fake.digest)
		}
	})
}

// TestResolveChartRevision pins the resolution itself against ArgoCD's
// newHelmClientResolveRevision (repository.go:2618-2647): an exact version is
// returned untouched and costs no registry call, a constraint is resolved to the
// maximum satisfying published version — from the TAG list for an OCI repository
// and from the INDEX for a classic one — and an unresolvable revision errors.
func TestResolveChartRevision(t *testing.T) {
	tags := []string{"1.0.0", "1.2.3", "1.3.0-rc.1", "2.0.0"}

	t.Run("exact version needs no registry", func(t *testing.T) {
		fake := &fakeChartClient{tags: tags}
		got, err := resolveChartRevision(fake, "revchart", "1.2.3", true)
		if err != nil || got != "1.2.3" {
			t.Fatalf("resolveChartRevision() = (%q, %v), want 1.2.3", got, err)
		}
		if fake.tagsCalls != 0 || fake.idxCalls != 0 {
			t.Errorf("an exact version cost %d tag and %d index calls, want none", fake.tagsCalls, fake.idxCalls)
		}
	})

	t.Run("oci constraint resolves from tags", func(t *testing.T) {
		fake := &fakeChartClient{tags: tags}
		got, err := resolveChartRevision(fake, "revchart", "^1.0.0", true)
		if err != nil || got != "1.2.3" {
			t.Fatalf("resolveChartRevision() = (%q, %v), want 1.2.3", got, err)
		}
		if fake.tagsCalls != 1 || fake.idxCalls != 0 {
			t.Errorf("tagsCalls=%d idxCalls=%d, want the TAG list for an OCI repository", fake.tagsCalls, fake.idxCalls)
		}
	})

	t.Run("classic constraint resolves from the index", func(t *testing.T) {
		fake := &fakeChartClient{index: &argohelm.Index{Entries: map[string]argohelm.Entries{
			"revchart": {{Version: "1.0.0"}, {Version: "1.2.3"}, {Version: "2.0.0"}},
		}}}
		got, err := resolveChartRevision(fake, "revchart", "^1.0.0", false)
		if err != nil || got != "1.2.3" {
			t.Fatalf("resolveChartRevision() = (%q, %v), want 1.2.3", got, err)
		}
		if fake.idxCalls != 1 || fake.tagsCalls != 0 {
			t.Errorf("tagsCalls=%d idxCalls=%d, want the INDEX for a classic repository", fake.tagsCalls, fake.idxCalls)
		}
	})

	t.Run("a chart missing from the index errors", func(t *testing.T) {
		fake := &fakeChartClient{index: &argohelm.Index{Entries: map[string]argohelm.Entries{}}}
		if _, err := resolveChartRevision(fake, "revchart", "^1.0.0", false); err == nil {
			t.Fatal("resolveChartRevision() error = nil for a chart absent from the index")
		}
	})

	t.Run("registry failure propagates", func(t *testing.T) {
		fake := &fakeChartClient{tagsErr: errors.New("unauthorized")}
		_, err := resolveChartRevision(fake, "revchart", "^1.0.0", true)
		if err == nil || !strings.Contains(err.Error(), "unauthorized") {
			t.Fatalf("error = %v, want the registry failure", err)
		}
	})

	t.Run("HEAD is neither a version nor a constraint", func(t *testing.T) {
		fake := &fakeChartClient{tags: tags}
		if _, err := resolveChartRevision(fake, "revchart", "HEAD", true); err == nil {
			t.Fatal("resolveChartRevision(HEAD) error = nil; upstream rejects it, and the caller's fallback depends on that")
		}
	})
}

// TestFetchRemoteChartReportsTheResolvedVersion pins what fetchRemoteChart hands
// back as the revision, and that the PULL is pinned to the same string — one
// resolution, as upstream does it, instead of one for the label and another
// inside helm.
func TestFetchRemoteChartReportsTheResolvedVersion(t *testing.T) {
	t.Run("constraint resolves for both the label and the pull", func(t *testing.T) {
		fake := &fakeChartClient{
			extractedDir: chartFixture(t, "mychart"),
			tags:         []string{"1.0.0", "1.2.3", "2.0.0"},
		}
		stubNewChartClient(t, fake, nil, nil)

		source := chartSource("^1.0.0")
		_, revision, _, cleanup, err := fetchRemoteChart(
			context.Background(), &RenderOptions{}, chartTestApp(), source)
		if err != nil {
			t.Fatalf("fetchRemoteChart() error: %v", err)
		}
		defer cleanup()

		if revision != "1.2.3" {
			t.Errorf("revision = %q, want the resolved 1.2.3", revision)
		}
		if len(fake.calls) != 1 || fake.calls[0].version != "1.2.3" {
			t.Errorf("ExtractChart calls = %+v, want the pull pinned to the resolved version", fake.calls)
		}
	})

	t.Run("unresolvable revision keeps the pull and labels what was declared", func(t *testing.T) {
		fake := &fakeChartClient{extractedDir: chartFixture(t, "mychart")}
		stubNewChartClient(t, fake, nil, nil)

		_, revision, _, cleanup, err := fetchRemoteChart(
			context.Background(), &RenderOptions{}, chartTestApp(), chartSource("HEAD"))
		if err != nil {
			t.Fatalf("fetchRemoteChart() error: %v", err)
		}
		defer cleanup()

		// HEAD is argocdf's own "latest" spelling: upstream's resolution rejects
		// it, so the fetch keeps its documented behavior (pull latest, version
		// "") and the label falls back to the DECLARED revision — never to the
		// git commit.
		if revision != "HEAD" {
			t.Errorf("revision = %q, want the declared HEAD", revision)
		}
		if len(fake.calls) != 1 || fake.calls[0].version != "" {
			t.Errorf("ExtractChart calls = %+v, want the unchanged latest-means-empty pull", fake.calls)
		}
	})

	t.Run("cache hit reports the pinned version without a client", func(t *testing.T) {
		cacheBase := t.TempDir()
		fake := &fakeChartClient{extractedDir: chartFixture(t, "mychart")}
		stubNewChartClient(t, fake, nil, nil)
		opts := RenderOptions{ChartCacheDir: cacheBase}
		source := chartSource("1.2.3")

		// Populate the cache, then hit it.
		_, _, _, cleanup, err := fetchRemoteChart(context.Background(), &opts, chartTestApp(), source)
		if err != nil {
			t.Fatalf("fetchRemoteChart() error: %v", err)
		}
		cleanup()

		_, revision, cached, cleanup2, err := fetchRemoteChart(context.Background(), &opts, chartTestApp(), source)
		if err != nil {
			t.Fatalf("fetchRemoteChart() second call error: %v", err)
		}
		defer cleanup2()
		if !cached {
			t.Fatal("second call was not a cache hit")
		}
		// A hit implies the revision passed IsImmutableChartVersion, so ArgoCD's
		// resolution would return it unchanged — no registry call is needed to
		// label the render, and the hit must keep skipping auth entirely.
		if revision != "1.2.3" {
			t.Errorf("revision = %q, want the pinned 1.2.3", revision)
		}
		if fake.tagsCalls != 0 || fake.idxCalls != 0 {
			t.Errorf("a cache hit cost %d tag and %d index calls, want none", fake.tagsCalls, fake.idxCalls)
		}
	})
}

// TestFetchRemoteChartCachesTheResolvedVersion pins the constraint half of the
// download cache. A constraint is not cacheable as written — it resolves against
// the mutable index — but the version it resolves TO is, so the chart is keyed by
// the resolved version and a second run reuses the download while still asking
// the registry what the constraint means now.
func TestFetchRemoteChartCachesTheResolvedVersion(t *testing.T) {
	cacheBase := t.TempDir()
	fake := &fakeChartClient{
		extractedDir: chartFixture(t, "mychart"),
		tags:         []string{"1.0.0", "1.2.3", "2.0.0"},
	}
	stubNewChartClient(t, fake, nil, nil)
	opts := RenderOptions{ChartCacheDir: cacheBase}
	source := chartSource("^1.0.0")

	// Miss: pulls, then publishes under the RESOLVED version's key — not the
	// constraint's, which would be a key whose meaning can change.
	dir, revision, cached, cleanup, err := fetchRemoteChart(
		context.Background(), &opts, chartTestApp(), source)
	if err != nil {
		t.Fatalf("fetchRemoteChart() error: %v", err)
	}
	cleanup()
	_, wantChartDir := chartCachePaths(cacheBase, source.RepoURL, source.Chart, "1.2.3")
	if dir != wantChartDir || !cached || revision != "1.2.3" {
		t.Fatalf("fetchRemoteChart() = (%q, %q, cached=%v), want the cached dir %q at 1.2.3",
			dir, revision, cached, wantChartDir)
	}

	// Hit: no second pull, but the registry IS asked again what the constraint
	// resolves to — that is the mutable half, and skipping it would pin the app
	// to the first version it ever saw.
	dir2, revision2, cached2, cleanup2, err := fetchRemoteChart(
		context.Background(), &opts, chartTestApp(), source)
	if err != nil {
		t.Fatalf("fetchRemoteChart() second call error: %v", err)
	}
	cleanup2()
	if dir2 != wantChartDir || !cached2 || revision2 != "1.2.3" {
		t.Errorf("cache hit = (%q, %q, cached=%v), want the same cached dir at 1.2.3", dir2, revision2, cached2)
	}
	if len(fake.calls) != 1 {
		t.Errorf("ExtractChart called %d times, want 1 — the second run must reuse the download", len(fake.calls))
	}
	if fake.tagsCalls != 2 {
		t.Errorf("tag list fetched %d times, want 2 — a constraint must be re-resolved every run", fake.tagsCalls)
	}

	// A constraint whose maximum has moved resolves elsewhere, so it lands on a
	// different key and pulls: the cache can never serve a stale version.
	fake.tags = append(fake.tags, "1.9.9")
	dir3, revision3, _, cleanup3, err := fetchRemoteChart(
		context.Background(), &opts, chartTestApp(), source)
	if err != nil {
		t.Fatalf("fetchRemoteChart() third call error: %v", err)
	}
	cleanup3()
	if revision3 != "1.9.9" || dir3 == wantChartDir {
		t.Errorf("moved constraint = (%q, %q), want a fresh 1.9.9 entry", dir3, revision3)
	}
	if len(fake.calls) != 2 {
		t.Errorf("ExtractChart called %d times, want 2 — the moved constraint must pull", len(fake.calls))
	}
}

// TestBuildEnvRevisionForExternalGitSource is the third source kind of the
// per-source revision rule. A renderable source in ANOTHER git repository renders
// from a clone of that repository at its own TargetRevision, so the revision it
// reports must be that clone's commit. Reporting the diffed repository's commit
// described unrelated content — and, being the one input guaranteed to differ
// between the two sides, made a cross-repo application diff on every PR while
// both sides rendered identical external content.
func TestBuildEnvRevisionForExternalGitSource(t *testing.T) {
	requireHelm(t)

	extRepo := writeTree(t, map[string]string{
		"chart/Chart.yaml":        revisionChart()["Chart.yaml"],
		"chart/values.yaml":       revisionChart()["values.yaml"],
		"chart/templates/cm.yaml": revisionChart()["templates/cm.yaml"],
	})
	extURL := initGitRepo(t, extRepo)
	wantCommit := clonedRevision(extRepo)
	if wantCommit == "" {
		t.Fatal("could not read the external repo's HEAD")
	}

	app := revisionApp(cluster.ApplicationSource{
		RepoURL: extURL, Path: "chart", TargetRevision: "main",
	})
	// RepoURL differs from the source's, which is what makes it external.
	r := mustNewArgoCDRenderer(t, RenderOptions{
		ArgoCDNamespace: "argocd",
		RepoURL:         "https://github.com/acme/repo",
	})
	result, err := r.RenderApplication(context.Background(), app, t.TempDir(), testRevision)
	if err != nil {
		t.Fatalf("RenderApplication() error = %v", err)
	}
	doc, ok := parseRendered(t, result.Manifests)["ConfigMap/eng-app-rev"]
	if !ok {
		t.Fatalf("missing ConfigMap/eng-app-rev; rendered:\n%s", result.Manifests)
	}
	got, _ := fieldAt(doc, "data.rev")
	if got == testRevision {
		t.Fatal("ARGOCD_APP_REVISION is the DIFFED repo's commit; a cross-repo app would diff on every PR")
	}
	if got != wantCommit {
		t.Errorf("ARGOCD_APP_REVISION = %v, want the external clone's commit %q", got, wantCommit)
	}
}

// TestClonedRevisionOnANonRepo pins the non-fatal contract: a clone whose commit
// cannot be read yields "", and the caller then keeps the diffed commit rather
// than failing a render over a label.
func TestClonedRevisionOnANonRepo(t *testing.T) {
	if got := clonedRevision(t.TempDir()); got != "" {
		t.Errorf("clonedRevision() = %q, want empty for a directory that is not a repository", got)
	}
}
