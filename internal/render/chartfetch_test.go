package render

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	argoappv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
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
type fakeChartClient struct {
	extractedDir string
	err          error
	calls        []extractCall
	closed       bool
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

	dir, cached, cleanup, err := fetchRemoteChart(context.Background(), &opts, chartTestApp(), source)
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
	_, _, cleanup, err := fetchRemoteChart(context.Background(), &opts, chartTestApp(), chartSource("1.2.3"))
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
	_, _, cleanup, err := fetchRemoteChart(context.Background(), &opts, chartTestApp(), chartSource("1.2.3"))
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
	_, _, cleanup, err := fetchRemoteChart(context.Background(), &opts, chartTestApp(), chartSource("1.2.3"))
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
	_, _, cleanup, err := fetchRemoteChart(context.Background(), &opts, chartTestApp(), chartSource("1.2.3"))
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
	dir, cached, cleanup, err := fetchRemoteChart(context.Background(), &opts, chartTestApp(), source)
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
	dir2, cached2, cleanup2, err := fetchRemoteChart(context.Background(), &opts, chartTestApp(), source)
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

func TestFetchRemoteChart_UnpinnedSkipsCache(t *testing.T) {
	cacheBase := t.TempDir()
	fake := &fakeChartClient{extractedDir: chartFixture(t, "mychart")}
	stubNewChartClient(t, fake, nil, nil)

	opts := RenderOptions{ChartCacheDir: cacheBase}
	dir, cached, cleanup, err := fetchRemoteChart(context.Background(), &opts, chartTestApp(), chartSource("HEAD"))
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
	_, _, _, err := fetchRemoteChart(context.Background(), &opts, chartTestApp(), chartSource("1.2.3"))
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
	dir, cached, cleanup, err := fetchRemoteChart(context.Background(), &opts, chartTestApp(), chartSource("1.2.3"))
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
	_, _, _, err := fetchRemoteChart(ctx, &opts, chartTestApp(), chartSource("1.2.3"))
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
	_, _, _, err := fetchRemoteChart(ctx, &opts, chartTestApp(), chartSource("1.2.3"))
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
	_, _, _, err := fetchRemoteChart(context.Background(), &opts, chartTestApp(), chartSource("1.2.3"))
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
