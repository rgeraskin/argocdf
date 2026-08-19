package render

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	argoappv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rgeraskin/argocdf/internal/cluster"
)

// writeTree materializes a name→content map under a fresh directory.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "artifact")
	for name, content := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// artifactChart is a helm chart at the artifact ROOT — the shape `helm push`
// produces, and the one an OCI-artifact source renders without a chart: field.
func artifactChart() map[string]string {
	return map[string]string{
		"Chart.yaml":  "apiVersion: v2\nname: artifactchart\nversion: 6.7.0\n",
		"values.yaml": "greeting: from-artifact\n",
		"templates/cm.yaml": `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}-cm
data:
  greeting: {{ .Values.greeting | quote }}
`,
		// A second, non-chart tree so a source.Path can select INSIDE the
		// artifact instead of at its root.
		"manifests/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: from-subpath\ndata:\n  a: b\n",
	}
}

// serveOCIArtifact stubs the OCI client so it serves dir for any revision, and
// returns the client so callers can assert on what was requested.
func serveOCIArtifact(t *testing.T, dir string) *fakeOCIClient {
	t.Helper()
	fake := &fakeOCIClient{extractedDir: dir, digest: "sha256:" + strings.Repeat("e", 64)}
	stubNewOCIClient(t, fake, nil, nil)
	return fake
}

// failIfChartFetched makes any remote-CHART fetch a test failure, so a test can
// prove a source took the artifact path and not the helm-chart path.
func failIfChartFetched(t *testing.T) {
	t.Helper()
	original := newChartClient
	newChartClient = func(*argoappv1.Repository, bool) chartClient {
		t.Error("the helm chart client was constructed for an oci:// source; the source-type dispatch regressed to IsHelm-before-IsOCI")
		return &fakeChartClient{err: os.ErrNotExist}
	}
	t.Cleanup(func() { newChartClient = original })
}

func ociArtifactApp(source cluster.ApplicationSource) *cluster.Application {
	return &cluster.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "eng-app", Namespace: "argocd"},
		Spec: cluster.ApplicationSpec{
			Source:      &source,
			Destination: cluster.ApplicationDestination{Namespace: "apps"},
		},
	}
}

// TestRenderOCIArtifactSourceRendersTheArtifact is the OCI-artifact spelling
// end to end: repoURL carries the oci:// scheme AND the chart name, there is no
// chart: field, and the whole artifact is the app.
func TestRenderOCIArtifactSourceRendersTheArtifact(t *testing.T) {
	requireHelm(t)
	fake := serveOCIArtifact(t, writeTree(t, artifactChart()))

	app := ociArtifactApp(cluster.ApplicationSource{
		RepoURL:        "oci://ghcr.io/acme/artifactchart",
		TargetRevision: "6.7.0",
	})
	r := mustNewArgoCDRenderer(t, RenderOptions{ArgoCDNamespace: "argocd"})

	result, err := r.RenderApplication(context.Background(), app, t.TempDir(), testRevision)
	if err != nil {
		t.Fatalf("RenderApplication() error = %v", err)
	}

	docs := parseRendered(t, result.Manifests)
	doc, ok := docs["ConfigMap/eng-app-cm"]
	if !ok {
		t.Fatalf("missing ConfigMap/eng-app-cm; rendered: %v", keysOf(docs))
	}
	if got, _ := fieldAt(doc, "data.greeting"); got != "from-artifact" {
		t.Errorf("data.greeting = %v, want from-artifact", got)
	}
	if len(fake.resolvedRevisions) != 1 || fake.resolvedRevisions[0] != "6.7.0" {
		t.Errorf("ResolveRevision revisions = %v, want [6.7.0]", fake.resolvedRevisions)
	}
}

// TestRenderOCIArtifactSourceIgnoresTheChartField pins ArgoCD's dispatch ORDER
// (repository.go:342 tests IsOCI before IsHelm): an oci:// repoURL that also
// carries chart: renders the ARTIFACT at that URL and never reads chart:.
//
// This is the shape a PR produces when someone "adds the missing scheme" to a
// working scheme-less helm-OCI source. It changes the source TYPE, and ArgoCD
// then resolves the tag against the repoURL itself. argocdf used to normalize
// the prefix away (its chart client trims oci:// because `helm pull` re-adds it),
// so both spellings rendered the same chart and the report said "no changes"
// about a change that breaks the app.
func TestRenderOCIArtifactSourceIgnoresTheChartField(t *testing.T) {
	requireHelm(t)
	serveOCIArtifact(t, writeTree(t, artifactChart()))
	failIfChartFetched(t)

	app := ociArtifactApp(cluster.ApplicationSource{
		RepoURL:        "oci://ghcr.io/acme",
		Chart:          "artifactchart",
		TargetRevision: "6.7.0",
	})
	r := mustNewArgoCDRenderer(t, RenderOptions{ArgoCDNamespace: "argocd"})

	result, err := r.RenderApplication(context.Background(), app, t.TempDir(), testRevision)
	if err != nil {
		t.Fatalf("RenderApplication() error = %v", err)
	}
	if _, ok := parseRendered(t, result.Manifests)["ConfigMap/eng-app-cm"]; !ok {
		t.Error("the artifact at oci://ghcr.io/acme did not render; chart: must not divert an oci:// source")
	}
}

// TestRenderOCIArtifactSourcePathIsInsideTheArtifact pins that source.Path
// selects a directory in the EXTRACTED artifact, not in the local worktree.
func TestRenderOCIArtifactSourcePathIsInsideTheArtifact(t *testing.T) {
	artifactDir := writeTree(t, artifactChart())
	serveOCIArtifact(t, artifactDir)

	// The local worktree holds a DIFFERENT manifests/ tree: if the path were
	// joined to the worktree, this is what would render.
	worktree := writeTree(t, map[string]string{
		"manifests/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: from-worktree\ndata:\n  a: b\n",
	})

	app := ociArtifactApp(cluster.ApplicationSource{
		RepoURL:        "oci://ghcr.io/acme/artifactchart",
		TargetRevision: "6.7.0",
		Path:           "manifests",
	})
	r := mustNewArgoCDRenderer(t, RenderOptions{ArgoCDNamespace: "argocd"})

	result, err := r.RenderApplication(context.Background(), app, worktree, testRevision)
	if err != nil {
		t.Fatalf("RenderApplication() error = %v", err)
	}
	docs := parseRendered(t, result.Manifests)
	if _, ok := docs["ConfigMap/from-subpath"]; !ok {
		t.Errorf("missing ConfigMap/from-subpath; rendered: %v", keysOf(docs))
	}
	if _, ok := docs["ConfigMap/from-worktree"]; ok {
		t.Error("source.Path resolved against the local worktree instead of the extracted artifact")
	}
}

// TestRenderOCIArtifactSourcePathCannotEscapeTheArtifact keeps the containment
// guard the git branch has: a malicious or broken artifact path must not reach
// the host filesystem.
func TestRenderOCIArtifactSourcePathCannotEscapeTheArtifact(t *testing.T) {
	serveOCIArtifact(t, writeTree(t, artifactChart()))

	app := ociArtifactApp(cluster.ApplicationSource{
		RepoURL:        "oci://ghcr.io/acme/artifactchart",
		TargetRevision: "6.7.0",
		Path:           "../../etc",
	})
	r := mustNewArgoCDRenderer(t, RenderOptions{ArgoCDNamespace: "argocd"})

	_, err := r.RenderApplication(context.Background(), app, t.TempDir(), testRevision)
	if err == nil {
		t.Fatal("RenderApplication() error = nil, want a path-containment failure")
	}
	if !strings.Contains(err.Error(), "invalid source path") {
		t.Errorf("error = %v, want the containment failure", err)
	}
}

// TestOCISourceIsNotAPureRef pins upstream's carve-out (repository.go:591): an
// OCI-artifact source renders from the artifact root, so an empty Path is normal
// and must not make a source that is ALSO a $ref render nothing.
func TestOCISourceIsNotAPureRef(t *testing.T) {
	tests := []struct {
		name   string
		source cluster.ApplicationSource
		want   bool
	}{
		{"oci ref with no path renders", cluster.ApplicationSource{
			RepoURL: "oci://ghcr.io/acme/chart", Ref: "artifact", TargetRevision: "6.7.0"}, false},
		{"git ref with no path is pure", cluster.ApplicationSource{
			RepoURL: "https://github.com/acme/repo", Ref: "values"}, true},
		{"git ref with a path renders", cluster.ApplicationSource{
			RepoURL: "https://github.com/acme/repo", Ref: "values", Path: "chart"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPureRef(tt.source); got != tt.want {
				t.Errorf("isPureRef() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestOCISourceIsNeverGitCloned pins that an oci:// URL does not reach the
// external-repository clone: it is not a git remote, so the clone would fail and
// the artifact would never be fetched.
func TestOCISourceIsNeverGitCloned(t *testing.T) {
	opts := RenderOptions{RepoURL: "https://github.com/acme/repo"}
	set := newExternalRepoSet(&opts, "default")
	t.Cleanup(set.cleanup)

	local := t.TempDir()
	source := &cluster.ApplicationSource{
		RepoURL:        "oci://ghcr.io/acme/artifactchart",
		TargetRevision: "6.7.0",
	}
	path, err := set.repoPathFor(context.Background(), source, local)
	if err != nil {
		t.Fatalf("repoPathFor() error = %v, want no clone attempt", err)
	}
	if path != local {
		t.Errorf("repoPathFor() = %q, want the local path %q (the artifact fetch provides the content)", path, local)
	}
}

// TestOCIArtifactTarballsAreSharedForTheRun pins that every client of one run
// gets the SAME tarball registry, so the two sides of a diff pull an artifact
// once — and that Cleanup removes it.
func TestOCIArtifactTarballsAreSharedForTheRun(t *testing.T) {
	r, err := NewArgoCDRenderer(RenderOptions{})
	if err != nil {
		t.Fatalf("NewArgoCDRenderer() error = %v", err)
	}
	root := r.ociPathsRoot
	if root == "" || r.ociPaths == nil {
		t.Fatal("NewArgoCDRenderer() left the artifact tarball registry unset")
	}
	if r.ociImagePaths() != r.ociPaths {
		t.Error("ociImagePaths() returned a fresh registry; artifacts would be pulled once per source")
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("artifact tarball dir missing: %v", err)
	}

	r.Cleanup()
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("Cleanup() left %q behind", root)
	}
	// A zero-value renderer (unit tests that never construct one) must still
	// hand out a usable registry rather than a nil interface.
	var zero ArgoCDRenderer
	if paths := zero.ociImagePaths(); paths == nil {
		t.Error("ociImagePaths() on a zero-value renderer = nil")
	}
}

// TestOCIRefSourceIsNotClonedAndRenders is the other half of the isPureRef
// carve-out: the shape it unlocks — a source that is BOTH an oci:// artifact and
// a $ref — has to survive ref preparation to reach its own branch. argocdf
// materializes ref repositories eagerly, so it used to `git clone` the oci:// URL
// and fail the whole application before rendering anything. Upstream materializes
// lazily (resolveReferencedSources walks the value files, not the sources), so an
// unreferenced ref costs nothing there.
func TestOCIRefSourceIsNotClonedAndRenders(t *testing.T) {
	requireHelm(t)
	serveOCIArtifact(t, writeTree(t, artifactChart()))

	app := &cluster.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "eng-app", Namespace: "argocd"},
		Spec: cluster.ApplicationSpec{
			Sources: []cluster.ApplicationSource{{
				RepoURL:        "oci://ghcr.io/acme/artifactchart",
				TargetRevision: "6.7.0",
				Ref:            "artifact",
			}},
			Destination: cluster.ApplicationDestination{Namespace: "apps"},
		},
	}
	// RepoURL set so the ref URL classifies as external — the state that used to
	// send it to git clone.
	r := mustNewArgoCDRenderer(t, RenderOptions{
		ArgoCDNamespace: "argocd",
		RepoURL:         "https://github.com/acme/repo",
	})

	result, err := r.RenderApplication(context.Background(), app, t.TempDir(), testRevision)
	if err != nil {
		t.Fatalf("RenderApplication() error = %v (an oci:// ref source must not be git-cloned)", err)
	}
	if _, ok := parseRendered(t, result.Manifests)["ConfigMap/eng-app-cm"]; !ok {
		t.Errorf("the artifact did not render; got:\n%s", result.Manifests)
	}
}

// TestChartRefSourceIsNotCloned covers the same family: a chart: source carrying
// a ref is a chart REPOSITORY URL, not a git remote.
func TestChartRefSourceIsNotCloned(t *testing.T) {
	requireHelm(t)
	fake := &fakeChartClient{extractedDir: writeTree(t, artifactChart())}
	stubNewChartClient(t, fake, nil, nil)

	app := &cluster.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "eng-app", Namespace: "argocd"},
		Spec: cluster.ApplicationSpec{
			Sources: []cluster.ApplicationSource{{
				RepoURL:        "ghcr.io/acme",
				Chart:          "artifactchart",
				TargetRevision: "6.7.0",
				Ref:            "chart",
			}},
			Destination: cluster.ApplicationDestination{Namespace: "apps"},
		},
	}
	r := mustNewArgoCDRenderer(t, RenderOptions{
		ArgoCDNamespace: "argocd",
		RepoURL:         "https://github.com/acme/repo",
	})

	if _, err := r.RenderApplication(context.Background(), app, t.TempDir(), testRevision); err != nil {
		t.Fatalf("RenderApplication() error = %v (a chart ref source must not be git-cloned)", err)
	}
}

// TestReferencedOCIRefFailsInGenerateManifests pins the other outcome: a value
// file that actually NAMES the unmaterialized ref fails where upstream fails, in
// GenerateManifests' own resolution, rather than being silently resolved against
// something else.
func TestReferencedOCIRefFailsInGenerateManifests(t *testing.T) {
	requireHelm(t)
	serveOCIArtifact(t, writeTree(t, artifactChart()))

	repoDir := writeTree(t, map[string]string{
		"chart/Chart.yaml":        "apiVersion: v2\nname: local\nversion: 0.1.0\n",
		"chart/templates/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: local-cm\n",
	})
	app := &cluster.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "eng-app", Namespace: "argocd"},
		Spec: cluster.ApplicationSpec{
			Sources: []cluster.ApplicationSource{
				{
					RepoURL: "https://github.com/acme/repo",
					Path:    "chart",
					Helm:    &cluster.ApplicationSourceHelm{ValueFiles: []string{"$artifact/values.yaml"}},
				},
				{
					RepoURL:        "oci://ghcr.io/acme/artifactchart",
					TargetRevision: "6.7.0",
					Ref:            "artifact",
				},
			},
			Destination: cluster.ApplicationDestination{Namespace: "apps"},
		},
	}
	r := mustNewArgoCDRenderer(t, RenderOptions{
		ArgoCDNamespace: "argocd",
		RepoURL:         "https://github.com/acme/repo",
	})

	_, err := r.RenderApplication(context.Background(), app, repoDir, testRevision)
	if err == nil {
		t.Fatal("RenderApplication() error = nil; a $ref value file naming an unmaterialized ref must fail")
	}
	if !strings.Contains(err.Error(), "failed to find repo") {
		t.Errorf("error = %v, want GenerateManifests' own `failed to find repo`", err)
	}
}
