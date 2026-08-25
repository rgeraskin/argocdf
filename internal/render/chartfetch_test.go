package render

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	argoappv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	argohelm "github.com/argoproj/argo-cd/v3/util/helm"
	utilio "github.com/argoproj/argo-cd/v3/util/io"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rgeraskin/argocdf/internal/cluster"
)

type extractCall struct {
	chart           string
	version         string
	passCredentials bool
}

// fakeChartClient records ExtractChart calls and serves a pre-made directory.
// tags/index serve constraint resolution; a test that leaves them empty is
// asserting on a source whose revision needs no resolution.
type fakeChartClient struct {
	extractedDir string
	err          error
	calls        []extractCall
	closed       bool

	tags      []string
	tagsErr   error
	index     *argohelm.Index
	indexErr  error
	tagsCalls int
	idxCalls  int
}

func (f *fakeChartClient) ExtractChart(chart, version string, passCredentials bool, _ int64, _ bool) (string, utilio.Closer, error) {
	f.calls = append(f.calls, extractCall{chart: chart, version: version, passCredentials: passCredentials})
	if f.err != nil {
		return "", nil, f.err
	}
	return f.extractedDir, utilio.NewCloser(func() error {
		f.closed = true
		return nil
	}), nil
}

func (f *fakeChartClient) GetTags(_ string, _ bool) ([]string, error) {
	f.tagsCalls++
	return f.tags, f.tagsErr
}

func (f *fakeChartClient) GetIndex(_ bool, _ int64) (*argohelm.Index, error) {
	f.idxCalls++
	if f.indexErr != nil {
		return nil, f.indexErr
	}
	if f.index != nil {
		return f.index, nil
	}
	return &argohelm.Index{}, nil
}

// stubNewChartClient swaps the chart-client constructor for the test,
// optionally capturing the repo and enableOCI it was called with.
func stubNewChartClient(t *testing.T, client chartClient, gotRepo **argoappv1.Repository, gotEnableOCI *bool) {
	t.Helper()
	original := newChartClient
	newChartClient = func(repo *argoappv1.Repository, enableOCI bool) chartClient {
		if gotRepo != nil {
			*gotRepo = repo
		}
		if gotEnableOCI != nil {
			*gotEnableOCI = enableOCI
		}
		return client
	}
	t.Cleanup(func() { newChartClient = original })
}

// chartFixture creates a directory shaped like an extracted chart.
func chartFixture(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte("apiVersion: v2\nname: "+name+"\nversion: 1.2.3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func chartSource(version string) *cluster.ApplicationSource {
	return &cluster.ApplicationSource{
		RepoURL:        "ghcr.io/acme",
		Chart:          "mychart",
		TargetRevision: version,
	}
}

func chartTestApp() *cluster.Application {
	return &cluster.Application{ObjectMeta: metav1.ObjectMeta{Name: "app"}}
}

func TestFetchRemoteChart_ClientInputs(t *testing.T) {
	fake := &fakeChartClient{extractedDir: chartFixture(t, "mychart")}
	var gotRepo *argoappv1.Repository
	var gotEnableOCI bool
	stubNewChartClient(t, fake, &gotRepo, &gotEnableOCI)

	opts := RenderOptions{
		ResolveRepo: func(_ context.Context, repoURL, _ string) (*argoappv1.Repository, error) {
			return &argoappv1.Repository{
				Repo:      repoURL,
				Username:  "resolved-user",
				Password:  "resolved-pass",
				EnableOCI: true,
			}, nil
		},
	}
	source := chartSource("1.2.3")
	source.Helm = &cluster.ApplicationSourceHelm{PassCredentials: true}

	dir, _, cached, cleanup, err := fetchRemoteChart(context.Background(), &opts, chartTestApp(), source)
	if err != nil {
		t.Fatalf("fetchRemoteChart() error: %v", err)
	}
	defer cleanup()

	if gotRepo == nil || gotRepo.Username != "resolved-user" {
		t.Errorf("newChartClient repo = %+v, want the resolved repository", gotRepo)
	}
	if !gotEnableOCI {
		t.Error("newChartClient enableOCI = false, want true (resolved EnableOCI)")
	}
	if len(fake.calls) != 1 || !fake.calls[0].passCredentials {
		t.Errorf("ExtractChart calls = %+v, want one call with passCredentials from source.Helm", fake.calls)
	}
	if dir != fake.extractedDir || cached {
		t.Errorf("fetchRemoteChart() = (%q, cached=%v), want the extracted dir, uncached", dir, cached)
	}
}

func TestFetchRemoteChart_SchemeLessURLHeuristic(t *testing.T) {
	// Without a credential source, the scheme-less URL heuristic alone must
	// still classify the repo as OCI.
	fake := &fakeChartClient{extractedDir: chartFixture(t, "mychart")}
	var gotEnableOCI bool
	stubNewChartClient(t, fake, nil, &gotEnableOCI)

	opts := RenderOptions{}
	_, _, _, cleanup, err := fetchRemoteChart(context.Background(), &opts, chartTestApp(), chartSource("1.2.3"))
	if err != nil {
		t.Fatalf("fetchRemoteChart() error: %v", err)
	}
	defer cleanup()

	if !gotEnableOCI {
		t.Error("newChartClient enableOCI = false, want true via the scheme-less heuristic")
	}
}

// TestFetchRemoteChart_RegistryAuthFileStripsLogin is the regression test
// for the chart-fetch `helm registry login` keychain collisions (macOS
// errSecDuplicateItem -25299): with an engine-owned auth file (registryAuth
// set), resolved OCI credentials are recorded there and the chart client
// receives a credential-stripped repository, so ArgoCD's login/logout (whose
// helm exec would hit the shared system keychain via ORAS native-store
// detection, and whose argv would carry the token) never runs. Concurrent
// fetches share the file through argocdf-side writes only.
func TestFetchRemoteChart_RegistryAuthFileStripsLogin(t *testing.T) {
	fake := &fakeChartClient{extractedDir: chartFixture(t, "mychart")}
	var gotRepo *argoappv1.Repository
	stubNewChartClient(t, fake, &gotRepo, nil)

	auth, err := newRegistryAuthFile()
	if err != nil {
		t.Fatal(err)
	}
	defer auth.Remove()

	opts := RenderOptions{
		registryAuth: auth,
		ResolveRepo: func(_ context.Context, repoURL, _ string) (*argoappv1.Repository, error) {
			return &argoappv1.Repository{
				Repo: repoURL, EnableOCI: true,
				Username: "fetch-bot", Password: "fetch-tok",
				Proxy: "http://proxy.local", Insecure: true,
			}, nil
		},
	}
	_, _, _, cleanup, err := fetchRemoteChart(context.Background(), &opts, chartTestApp(), chartSource("1.2.3"))
	if err != nil {
		t.Fatalf("fetchRemoteChart() error: %v", err)
	}
	defer cleanup()

	if gotRepo == nil {
		t.Fatal("chart client never constructed")
	}
	if gotRepo.Username != "" || gotRepo.Password != "" {
		t.Error("chart client received credentials — ExtractChart would exec `helm registry login`")
	}
	if gotRepo.Proxy != "http://proxy.local" || !gotRepo.Insecure || !gotRepo.EnableOCI {
		t.Errorf("stripping lost non-credential repo fields: %+v", gotRepo)
	}
	if got := readAuths(t, auth.path)["ghcr.io"].Auth; got == "" {
		t.Error("resolved credentials were not recorded in the registry auth file")
	}
}

// TestFetchRemoteChart_NoAuthFilePassesCredsThrough pins the defensive
// fallback: with neither an engine-owned auth file nor a pierced registry
// config, resolved credentials still reach ArgoCD's chart client (its own
// login flow). The engine always sets one of the two, so this path is only
// reachable when fetching outside an engine.
func TestFetchRemoteChart_NoAuthFilePassesCredsThrough(t *testing.T) {
	fake := &fakeChartClient{extractedDir: chartFixture(t, "mychart")}
	var gotRepo *argoappv1.Repository
	stubNewChartClient(t, fake, &gotRepo, nil)

	opts := RenderOptions{
		ResolveRepo: func(_ context.Context, repoURL, _ string) (*argoappv1.Repository, error) {
			return &argoappv1.Repository{Repo: repoURL, EnableOCI: true, Username: "u", Password: "p"}, nil
		},
	}
	_, _, _, cleanup, err := fetchRemoteChart(context.Background(), &opts, chartTestApp(), chartSource("1.2.3"))
	if err != nil {
		t.Fatalf("fetchRemoteChart() error: %v", err)
	}
	defer cleanup()

	if gotRepo == nil || gotRepo.Username != "u" || gotRepo.Password != "p" {
		t.Errorf("chart client repo = %+v, want credentials passed through", gotRepo)
	}
}

func TestIsOCIChartRepo(t *testing.T) {
	tests := []struct {
		repoURL string
		want    bool
	}{
		{"oci://ghcr.io/org", true},
		{"ghcr.io/org", true}, // scheme-less: ArgoCD's OCI repo secret shape
		{"https://charts.example.com", false},
		{"http://charts.example.com", false},
	}
	for _, tt := range tests {
		if got := isOCIChartRepo(tt.repoURL); got != tt.want {
			t.Errorf("isOCIChartRepo(%q) = %v, want %v", tt.repoURL, got, tt.want)
		}
	}
}

// TestFetchRemoteChart_LocalModeStripsClientLogin pins --repo-creds=local
// under a pierced registry config: there is no owned auth file to seed, but
// inline OCI credentials are still stripped so ExtractChart never execs
// `helm registry login` (the macOS keychain race) — the pull authenticates
// from the user's own registry config instead.
func TestFetchRemoteChart_LocalModeStripsClientLogin(t *testing.T) {
	fake := &fakeChartClient{extractedDir: chartFixture(t, "mychart")}
	var gotRepo *argoappv1.Repository
	stubNewChartClient(t, fake, &gotRepo, nil)

	opts := RenderOptions{
		HelmRegistryConfig: "/user/registry/config.json",
		ResolveRepo: func(_ context.Context, repoURL, _ string) (*argoappv1.Repository, error) {
			return &argoappv1.Repository{Repo: repoURL, EnableOCI: true, Username: "u", Password: "p"}, nil
		},
	}
	_, _, _, cleanup, err := fetchRemoteChart(context.Background(), &opts, chartTestApp(), chartSource("1.2.3"))
	if err != nil {
		t.Fatalf("fetchRemoteChart() error: %v", err)
	}
	defer cleanup()

	if gotRepo == nil {
		t.Fatal("chart client never constructed")
	}
	if gotRepo.Username != "" || gotRepo.Password != "" {
		t.Error("chart client received credentials — ExtractChart would exec `helm registry login`")
	}
}

func TestFetchRemoteChart_CachePublishAndHit(t *testing.T) {
	cacheBase := t.TempDir()
	fake := &fakeChartClient{extractedDir: chartFixture(t, "mychart")}
	stubNewChartClient(t, fake, nil, nil)

	opts := RenderOptions{ChartCacheDir: cacheBase}
	source := chartSource("1.2.3")

	// Miss: fetches, publishes into the cache, closes the extraction.
	dir, _, cached, cleanup, err := fetchRemoteChart(context.Background(), &opts, chartTestApp(), source)
	if err != nil {
		t.Fatalf("fetchRemoteChart() error: %v", err)
	}
	cleanup()
	_, wantChartDir := chartCachePaths(cacheBase, source.RepoURL, source.Chart, source.TargetRevision)
	if dir != wantChartDir || !cached {
		t.Errorf("fetchRemoteChart() = (%q, cached=%v), want cached chart dir %q", dir, cached, wantChartDir)
	}
	if _, err := os.Stat(filepath.Join(dir, "Chart.yaml")); err != nil {
		t.Errorf("cached chart content missing: %v", err)
	}
	if !fake.closed {
		t.Error("extraction closer not closed after publishing to the cache")
	}

	// Hit: served from the cache, the client is never called again.
	dir2, _, cached2, cleanup2, err := fetchRemoteChart(context.Background(), &opts, chartTestApp(), source)
	if err != nil {
		t.Fatalf("fetchRemoteChart() second call error: %v", err)
	}
	cleanup2()
	if dir2 != wantChartDir || !cached2 {
		t.Errorf("cache hit = (%q, cached=%v), want %q, cached", dir2, cached2, wantChartDir)
	}
	if len(fake.calls) != 1 {
		t.Errorf("ExtractChart called %d times, want 1 (second call must be a cache hit)", len(fake.calls))
	}
}

// TestFetchRemoteChart_CredentialScopedDirsDoNotShare is the unit-level gate for the
// per-credential-source chart-cache scope (Factory.chartCacheDir appends
// charts/<mode>): a chart downloaded under one --repo-creds source must NOT satisfy a
// run whose whole purpose is checking that another source can fetch it. Render with
// `local`, re-run with `cluster` to see whether ArgoCD's own credentials work, and a
// shared directory would answer from the first download without contacting the
// registry - success reported for a registry ArgoCD cannot reach.
//
// The path difference alone is pinned in app.TestChartCacheDirScopedByCredentialSource;
// what this adds is the CONSEQUENCE, which is the part that actually protects the
// user: the fetch really is re-attempted. It lives here rather than only in e2e
// because the e2e tripwire (case/private-chart-unauth) needs a sibling case to have
// filled charts/cluster first, so it is order-dependent and rides a live registry;
// this is deterministic, needs no network, and cannot be disarmed by a rename.
func TestFetchRemoteChart_CredentialScopedDirsDoNotShare(t *testing.T) {
	base := t.TempDir()
	fake := &fakeChartClient{extractedDir: chartFixture(t, "mychart")}
	stubNewChartClient(t, fake, nil, nil)

	source := chartSource("1.2.3")
	// The two directories Factory.chartCacheDir produces for two credential sources.
	clusterOpts := RenderOptions{ChartCacheDir: filepath.Join(base, "charts", "cluster")}
	localOpts := RenderOptions{ChartCacheDir: filepath.Join(base, "charts", "local")}

	// Fetched and published under the CLUSTER scope, then the SAME chart and version
	// requested under the LOCAL one. Neither fetch is allowed to fail, but nothing
	// else is asserted until both have run: the load-bearing check is the CALL COUNT,
	// and it should be what speaks when this breaks.
	dir, _, cached, cleanup, err := fetchRemoteChart(context.Background(), &clusterOpts, chartTestApp(), source)
	if err != nil {
		t.Fatalf("cluster-scope fetch: %v", err)
	}
	cleanup()
	dir2, _, cached2, cleanup2, err := fetchRemoteChart(context.Background(), &localOpts, chartTestApp(), source)
	if err != nil {
		t.Fatalf("local-scope fetch: %v", err)
	}
	cleanup2()

	// THE property: the second source had to fetch for itself. Sharing one directory
	// across scopes (the pre-scope layout, or a later "why keep N identical copies"
	// dedupe) makes the second call a HIT and leaves this at 1 - and a --repo-creds
	// switch would then verify nothing.
	if len(fake.calls) != 2 {
		t.Errorf("ExtractChart called %d times, want 2: the chart downloaded under one credential source satisfied the other", len(fake.calls))
	}
	if dir2 == dir {
		t.Errorf("both credential sources served the same chart dir %q", dir2)
	}
	// Each fetch must be served from the directory it was GIVEN (the caller owns the
	// layout - Factory.chartCacheDir builds charts/<mode>, pinned in its own test).
	for _, tc := range []struct {
		mode, dir, base string
		cached          bool
	}{
		{"cluster", dir, clusterOpts.ChartCacheDir, cached},
		{"local", dir2, localOpts.ChartCacheDir, cached2},
	} {
		if !tc.cached || !strings.HasPrefix(tc.dir, tc.base+string(filepath.Separator)) {
			t.Errorf("%s-scope fetch = (%q, cached=%v), want a chart dir under %q", tc.mode, tc.dir, tc.cached, tc.base)
		}
		// Each scope keeps its own copy, so a later run in either mode is still served.
		if _, err := os.Stat(filepath.Join(tc.dir, "Chart.yaml")); err != nil {
			t.Errorf("%s-scope chart content missing under %q: %v", tc.mode, tc.dir, err)
		}
	}
}

func TestFetchRemoteChart_UnpinnedSkipsCache(t *testing.T) {
	cacheBase := t.TempDir()
	fake := &fakeChartClient{extractedDir: chartFixture(t, "mychart")}
	stubNewChartClient(t, fake, nil, nil)

	opts := RenderOptions{ChartCacheDir: cacheBase}
	dir, _, cached, cleanup, err := fetchRemoteChart(context.Background(), &opts, chartTestApp(), chartSource("HEAD"))
	if err != nil {
		t.Fatalf("fetchRemoteChart() error: %v", err)
	}

	if dir != fake.extractedDir || cached {
		t.Errorf("fetchRemoteChart() = (%q, cached=%v), want the extracted dir for an unpinned version", dir, cached)
	}
	if fake.calls[0].version != "" {
		t.Errorf("ExtractChart version = %q, want empty (HEAD means latest)", fake.calls[0].version)
	}
	cleanup()
	if !fake.closed {
		t.Error("cleanup did not close the extraction for an uncached chart")
	}
	if entries, _ := os.ReadDir(cacheBase); len(entries) != 0 {
		t.Errorf("unpinned fetch polluted the cache: %v", entries)
	}
}

func TestFetchRemoteChart_ErrorWrapsChartAndRepo(t *testing.T) {
	fake := &fakeChartClient{err: errors.New("401 unauthorized")}
	stubNewChartClient(t, fake, nil, nil)

	opts := RenderOptions{}
	_, _, _, _, err := fetchRemoteChart(context.Background(), &opts, chartTestApp(), chartSource("1.2.3"))
	if err == nil {
		t.Fatal("fetchRemoteChart() = nil error, want fetch failure")
	}
}

func TestPublishChartToCache_FailureLeavesExtractedIntact(t *testing.T) {
	extracted := chartFixture(t, "mychart")
	// A regular file where the cache parent should be makes MkdirAll fail.
	blocker := filepath.Join(t.TempDir(), "cache")
	if err := os.WriteFile(blocker, []byte("in the way"), 0o644); err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(blocker, "sha")
	chartDir := filepath.Join(cacheDir, "mychart")

	if publishChartToCache(extracted, cacheDir, chartDir) {
		t.Error("publishChartToCache() = true, want false when the cache parent cannot be created")
	}
	if _, err := os.Stat(filepath.Join(extracted, "Chart.yaml")); err != nil {
		t.Errorf("publish failure must leave the extracted chart intact: %v", err)
	}
}

func TestPublishChartToCache_ConcurrentWinnerIsSuccess(t *testing.T) {
	extracted := chartFixture(t, "mychart")
	cacheDir := filepath.Join(t.TempDir(), "sha")
	chartDir := filepath.Join(cacheDir, "mychart")
	// Another renderer already published: the rename into place fails, but
	// the populated cache entry counts as success.
	if err := os.MkdirAll(chartDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if !publishChartToCache(extracted, cacheDir, chartDir) {
		t.Error("publishChartToCache() = false, want true when a concurrent publish already populated the cache")
	}
}

func TestFetchRemoteChart_PublishFailureServesExtracted(t *testing.T) {
	fake := &fakeChartClient{extractedDir: chartFixture(t, "mychart")}
	stubNewChartClient(t, fake, nil, nil)

	// ChartCacheDir points at a regular file: the cache decision still
	// enables caching, but publishing can never succeed.
	blocker := filepath.Join(t.TempDir(), "cache")
	if err := os.WriteFile(blocker, []byte("in the way"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := RenderOptions{ChartCacheDir: blocker}
	dir, _, cached, cleanup, err := fetchRemoteChart(context.Background(), &opts, chartTestApp(), chartSource("1.2.3"))
	if err != nil {
		t.Fatalf("fetchRemoteChart() error: %v", err)
	}
	if dir != fake.extractedDir || cached {
		t.Errorf("fetchRemoteChart() = (%q, cached=%v), want the extracted dir when publishing fails", dir, cached)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "Chart.yaml")); statErr != nil {
		t.Errorf("served chart content missing after publish failure: %v", statErr)
	}
	cleanup()
	if !fake.closed {
		t.Error("cleanup did not close the extraction after a publish failure")
	}
}

func TestFetchRemoteChart_ContextCancelled(t *testing.T) {
	fake := &fakeChartClient{extractedDir: chartFixture(t, "mychart")}
	stubNewChartClient(t, fake, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	opts := RenderOptions{}
	_, _, _, _, err := fetchRemoteChart(ctx, &opts, chartTestApp(), chartSource("1.2.3"))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("fetchRemoteChart() error = %v, want context.Canceled", err)
	}
	if len(fake.calls) != 0 {
		t.Error("chart client must not be called after cancellation")
	}
}

// blockingChartClient blocks inside ExtractChart until released, signalling
// entry — for in-flight cancellation tests.
type blockingChartClient struct {
	entered chan struct{}
	release chan struct{}
	dir     string
	closed  chan struct{}
}

// The resolution methods are unreachable here: the cancellation tests use an
// exact version, which resolves without touching the registry.
func (b *blockingChartClient) GetTags(string, bool) ([]string, error) {
	return nil, errors.New("GetTags must not be called for an exact version")
}

func (b *blockingChartClient) GetIndex(bool, int64) (*argohelm.Index, error) {
	return nil, errors.New("GetIndex must not be called for an exact version")
}

func (b *blockingChartClient) ExtractChart(string, string, bool, int64, bool) (string, utilio.Closer, error) {
	close(b.entered)
	<-b.release
	return b.dir, utilio.NewCloser(func() error {
		close(b.closed)
		return nil
	}), nil
}

func TestFetchRemoteChart_InFlightCancellation(t *testing.T) {
	blocking := &blockingChartClient{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		dir:     chartFixture(t, "mychart"),
		closed:  make(chan struct{}),
	}
	stubNewChartClient(t, blocking, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-blocking.entered // cancel only once the fetch is genuinely in flight
		cancel()
	}()

	opts := RenderOptions{}
	_, _, _, _, err := fetchRemoteChart(ctx, &opts, chartTestApp(), chartSource("1.2.3"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("fetchRemoteChart() error = %v, want context.Canceled while the fetch is in flight", err)
	}

	// The abandoned fetch drains in the background and its extraction is
	// released once it finishes.
	close(blocking.release)
	select {
	case <-blocking.closed:
	case <-time.After(5 * time.Second):
		t.Error("background drain did not close the abandoned extraction")
	}
}

func TestFetchRemoteChart_ResolveErrorIsLoud(t *testing.T) {
	constructed := false
	original := newChartClient
	newChartClient = func(*argoappv1.Repository, bool) chartClient {
		constructed = true
		return &fakeChartClient{}
	}
	t.Cleanup(func() { newChartClient = original })

	opts := RenderOptions{
		ResolveRepo: func(context.Context, string, string) (*argoappv1.Repository, error) {
			return nil, errors.New("token exchange failed")
		},
	}
	_, _, _, _, err := fetchRemoteChart(context.Background(), &opts, chartTestApp(), chartSource("1.2.3"))
	if err == nil || !strings.Contains(err.Error(), "token exchange failed") {
		t.Errorf("fetchRemoteChart() error = %v, want the credential resolution root cause", err)
	}
	if constructed {
		t.Error("chart client must not be constructed when credential resolution fails")
	}
}

func TestCopyChartToTempDir(t *testing.T) {
	src := chartFixture(t, "mychart")
	dst, cleanup, err := copyChartToTempDir(src)
	if err != nil {
		t.Fatalf("copyChartToTempDir() error: %v", err)
	}
	defer cleanup()
	if dst == src {
		t.Fatal("copyChartToTempDir() returned the source directory itself")
	}
	if _, err := os.Stat(filepath.Join(dst, "Chart.yaml")); err != nil {
		t.Errorf("copied chart content missing: %v", err)
	}
	// Mutating the copy must not touch the source (the shared cache).
	if err := os.WriteFile(filepath.Join(dst, "Chart.lock"), []byte("deps"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(src, "Chart.lock")); !os.IsNotExist(err) {
		t.Error("mutating the private copy leaked into the source directory")
	}
}

// TestRedactChartTempPaths: a failed chart pull surfaces helm's argv, including the
// temp destination ArgoCD's client created. That path is gone by the time anyone
// reads the report - and for argocdf a report is usually a PR comment, so leaving it
// in rewrites the comment on every re-run.
func TestRedactChartTempPaths(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "helm pull argv from a real failure",
			in: "failed to get command args to log: `helm pull oci://reg.test/charts/app --version 1.0.0 " +
				"--destination /var/folders/xy/T/3f2b1c4d-1a2b-3c4d-5e6f-7a8b9c0d1e2f` failed exit status 1",
			want: "failed to get command args to log: `helm pull oci://reg.test/charts/app --version 1.0.0 " +
				"--destination (temp dir)` failed exit status 1",
		},
		{
			name: "several paths in one message",
			in: "cp /tmp/3f2b1c4d-1a2b-3c4d-5e6f-7a8b9c0d1e2f/app " +
				"/tmp/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/dst",
			want: "cp (temp dir)/app (temp dir)/dst",
		},
		{
			// Everything actionable must survive: the registry, chart, version and
			// the underlying cause are what a reader needs.
			name: "the diagnosis is untouched",
			in:   `Get "https://reg.test/v2/charts/app/manifests/1.0.0": unauthorized`,
			want: `Get "https://reg.test/v2/charts/app/manifests/1.0.0": unauthorized`,
		},
		{
			// A UUID that is not a path component stays: it could be meaningful.
			name: "bare uuid is not a path",
			in:   "request 3f2b1c4d-1a2b-3c4d-5e6f-7a8b9c0d1e2f failed",
			want: "request 3f2b1c4d-1a2b-3c4d-5e6f-7a8b9c0d1e2f failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redactArgoTempPaths(tt.in); got != tt.want {
				t.Errorf("redactArgoTempPaths()\n got: %s\nwant: %s", got, tt.want)
			}
		})
	}
}

// TestChartFetchErrorKeepsChain: the redaction must not flatten the cause. Wrapping
// with %s would have rendered identically while silently breaking errors.Is/As for
// anything under ExtractChart - a future retry-on-timeout would compile and simply
// never match.
func TestChartFetchErrorKeepsChain(t *testing.T) {
	sentinel := errors.New("boom")
	cause := fmt.Errorf("helm pull --destination /tmp/3f2b1c4d-1a2b-3c4d-5e6f-7a8b9c0d1e2f: %w", sentinel)
	err := &chartFetchError{chart: "app", repoURL: "reg.test/charts", cause: cause}

	if !errors.Is(err, sentinel) {
		t.Error("errors.Is could not reach the cause through the chart-fetch error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "(temp dir)") {
		t.Errorf("temp path not redacted in Error(): %s", msg)
	}
	if strings.Contains(msg, "3f2b1c4d") {
		t.Errorf("Error() still carries the temp path: %s", msg)
	}
	for _, want := range []string{"app", "reg.test/charts", "boom"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Error() dropped %q: %s", want, msg)
		}
	}
}

// TestPublishChartToCache_FailureRemovesStaging: a publish that does not land
// must not leave its staging directory beside the entries. It used to, on every
// failure: the deferred cleanup went through SafeRemoveAll, which refuses paths
// outside os.TempDir(), and the chart cache lives under the user cache dir. Both
// failure shapes are covered - the rename lost to a concurrent publisher (which
// still reports success) and a copy that fails outright.
func TestPublishChartToCache_FailureRemovesStaging(t *testing.T) {
	stagingDirs := func(t *testing.T, parent string) []string {
		t.Helper()
		matches, err := filepath.Glob(filepath.Join(parent, "argocdf-chart-*.tmp"))
		if err != nil {
			t.Fatal(err)
		}
		return matches
	}

	t.Run("rename lost to a concurrent publisher", func(t *testing.T) {
		extracted := chartFixture(t, "mychart")
		parent := t.TempDir()
		cacheDir := filepath.Join(parent, "sha")
		chartDir := filepath.Join(cacheDir, "mychart")
		if err := os.MkdirAll(chartDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if !publishChartToCache(extracted, cacheDir, chartDir) {
			t.Fatal("publishChartToCache() = false, want true (entry already populated)")
		}
		if left := stagingDirs(t, parent); len(left) != 0 {
			t.Errorf("staging directories left behind: %v", left)
		}
	})

	t.Run("copy fails", func(t *testing.T) {
		parent := t.TempDir()
		cacheDir := filepath.Join(parent, "sha")
		chartDir := filepath.Join(cacheDir, "mychart")
		missing := filepath.Join(t.TempDir(), "does-not-exist")
		if publishChartToCache(missing, cacheDir, chartDir) {
			t.Fatal("publishChartToCache() = true, want false when the source cannot be copied")
		}
		if left := stagingDirs(t, parent); len(left) != 0 {
			t.Errorf("staging directories left behind: %v", left)
		}
		if dirExists(cacheDir) {
			t.Errorf("no entry must exist after a failed publish, found %s", cacheDir)
		}
	})
}
