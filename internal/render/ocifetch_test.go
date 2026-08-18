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

// fakeOCIClient records the calls the fetch path makes and serves a pre-made
// directory as the extracted artifact.
type fakeOCIClient struct {
	extractedDir string
	digest       string
	resolveErr   error
	extractErr   error

	resolvedRevisions []string
	resolvedNoCache   []bool
	extractedDigests  []string
	closed            bool
}

func (f *fakeOCIClient) ResolveRevision(_ context.Context, revision string, noCache bool) (string, error) {
	f.resolvedRevisions = append(f.resolvedRevisions, revision)
	f.resolvedNoCache = append(f.resolvedNoCache, noCache)
	if f.resolveErr != nil {
		return "", f.resolveErr
	}
	return f.digest, nil
}

func (f *fakeOCIClient) Extract(_ context.Context, revision string) (string, utilio.Closer, error) {
	f.extractedDigests = append(f.extractedDigests, revision)
	if f.extractErr != nil {
		return "", nil, f.extractErr
	}
	return f.extractedDir, utilio.NewCloser(func() error {
		f.closed = true
		return nil
	}), nil
}

// stubNewOCIClient swaps the OCI-client constructor for the test, optionally
// capturing the repository and the tarball registry it was called with.
func stubNewOCIClient(
	t *testing.T,
	client ociClient,
	gotRepo **argoappv1.Repository,
	gotPaths *utilio.TempPaths,
) {
	t.Helper()
	original := newOCIClient
	newOCIClient = func(repo *argoappv1.Repository, imagePaths utilio.TempPaths) (ociClient, error) {
		if gotRepo != nil {
			*gotRepo = repo
		}
		if gotPaths != nil {
			*gotPaths = imagePaths
		}
		return client, nil
	}
	t.Cleanup(func() { newOCIClient = original })
}

// artifactFixture creates a directory shaped like an extracted OCI artifact.
func artifactFixture(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "artifact")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"),
		[]byte("apiVersion: v2\nname: artifact\nversion: 1.2.3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func ociSource(revision string) *cluster.ApplicationSource {
	return &cluster.ApplicationSource{
		RepoURL:        "oci://ghcr.io/acme/mychart",
		TargetRevision: revision,
	}
}

func ociTestApp() *cluster.Application {
	return &cluster.Application{ObjectMeta: metav1.ObjectMeta{Name: "app"}}
}

func TestFetchOCIArtifactClientInputs(t *testing.T) {
	fake := &fakeOCIClient{extractedDir: artifactFixture(t), digest: "sha256:" + strings.Repeat("a", 64)}
	var gotRepo *argoappv1.Repository
	var gotPaths utilio.TempPaths
	stubNewOCIClient(t, fake, &gotRepo, &gotPaths)

	opts := RenderOptions{
		ResolveRepo: func(_ context.Context, repoURL, _ string) (*argoappv1.Repository, error) {
			return &argoappv1.Repository{
				Repo:     repoURL,
				Username: "resolved-user",
				Password: "resolved-pass",
				Type:     "oci",
			}, nil
		},
	}
	imagePaths := utilio.NewRandomizedTempPaths(t.TempDir())

	dir, cleanup, err := fetchOCIArtifact(context.Background(), &opts, ociTestApp(), ociSource("6.7.0"), imagePaths)
	if err != nil {
		t.Fatalf("fetchOCIArtifact() error: %v", err)
	}

	if gotRepo == nil || gotRepo.Repo != "oci://ghcr.io/acme/mychart" {
		t.Errorf("newOCIClient repo = %+v, want the resolved repository with its oci:// scheme intact", gotRepo)
	}
	// The chart path STRIPS username/password so ArgoCD never runs `helm
	// registry login`; artifact pulls authenticate through ORAS directly, so
	// stripping them here would make every private artifact fetch anonymous.
	if gotRepo.Username != "resolved-user" || gotRepo.Password != "resolved-pass" {
		t.Errorf("newOCIClient repo creds = %q/%q, want the resolved credentials unstripped",
			gotRepo.Username, gotRepo.Password)
	}
	if gotPaths != imagePaths {
		t.Error("newOCIClient imagePaths is not the registry the caller passed; the run's artifact tarballs would not be shared")
	}
	if want := []string{"6.7.0"}; len(fake.resolvedRevisions) != 1 || fake.resolvedRevisions[0] != want[0] {
		t.Errorf("ResolveRevision revisions = %v, want %v", fake.resolvedRevisions, want)
	}
	if len(fake.resolvedNoCache) != 1 || !fake.resolvedNoCache[0] {
		t.Errorf("ResolveRevision noCache = %v, want [true]", fake.resolvedNoCache)
	}
	// Extract must be handed the RESOLVED digest, not the tag: the artifact
	// tarball cache is keyed by what is passed here, so a tag would let two
	// runs of a moved tag collide on one cache entry.
	if len(fake.extractedDigests) != 1 || fake.extractedDigests[0] != fake.digest {
		t.Errorf("Extract revisions = %v, want [%s]", fake.extractedDigests, fake.digest)
	}
	if dir != fake.extractedDir {
		t.Errorf("dir = %q, want the extraction %q", dir, fake.extractedDir)
	}

	if fake.closed {
		t.Error("extraction closed before cleanup")
	}
	cleanup()
	if !fake.closed {
		t.Error("cleanup() did not close the extraction")
	}
}

func TestFetchOCIArtifactBareRepoWithoutCredentialSource(t *testing.T) {
	fake := &fakeOCIClient{extractedDir: artifactFixture(t), digest: "sha256:" + strings.Repeat("b", 64)}
	var gotRepo *argoappv1.Repository
	stubNewOCIClient(t, fake, &gotRepo, nil)

	_, cleanup, err := fetchOCIArtifact(
		context.Background(), &RenderOptions{}, ociTestApp(), ociSource("6.7.0"),
		utilio.NewRandomizedTempPaths(t.TempDir()))
	if err != nil {
		t.Fatalf("fetchOCIArtifact() error: %v", err)
	}
	defer cleanup()

	if gotRepo == nil || gotRepo.Repo != "oci://ghcr.io/acme/mychart" || gotRepo.Username != "" {
		t.Errorf("newOCIClient repo = %+v, want a bare credential-less repository (--repo-creds=none)", gotRepo)
	}
}

func TestFetchOCIArtifactResolutionFailureIsLoud(t *testing.T) {
	stubNewOCIClient(t, &fakeOCIClient{}, nil, nil)

	_, _, err := fetchOCIArtifact(
		context.Background(),
		&RenderOptions{ResolveRepo: func(_ context.Context, _, _ string) (*argoappv1.Repository, error) {
			return nil, errors.New("cluster unreachable")
		}},
		ociTestApp(), ociSource("6.7.0"), utilio.NewRandomizedTempPaths(t.TempDir()))
	if err == nil || !strings.Contains(err.Error(), "cluster unreachable") {
		t.Fatalf("error = %v, want the credential-resolution failure (no anonymous fallback)", err)
	}
}

func TestFetchOCIArtifactErrorsCarryContext(t *testing.T) {
	cause := errors.New("cannot get digest for revision 6.7.0: /tmp/9f8e7d6c-1234-5678-9abc-def012345678/x: MANIFEST_UNKNOWN")

	tests := []struct {
		name   string
		client *fakeOCIClient
	}{
		{"resolve", &fakeOCIClient{resolveErr: cause}},
		{"extract", &fakeOCIClient{digest: "sha256:" + strings.Repeat("c", 64), extractErr: cause}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubNewOCIClient(t, tt.client, nil, nil)

			_, _, err := fetchOCIArtifact(
				context.Background(), &RenderOptions{}, ociTestApp(), ociSource("6.7.0"),
				utilio.NewRandomizedTempPaths(t.TempDir()))
			if err == nil {
				t.Fatal("fetchOCIArtifact() error = nil, want a fetch failure")
			}
			msg := err.Error()
			for _, want := range []string{"oci://ghcr.io/acme/mychart", "6.7.0", "MANIFEST_UNKNOWN"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not name %q", msg, want)
				}
			}
			// Per-run temp paths are redacted: this text reaches PR comments,
			// where a path that no longer exists only causes re-write churn.
			if strings.Contains(msg, "9f8e7d6c-1234-5678-9abc-def012345678") {
				t.Errorf("error %q leaks a per-run temp path", msg)
			}
			if !errors.Is(err, cause) {
				t.Error("errors.Is() cannot reach the cause; the chain was flattened")
			}
		})
	}
}

func TestFetchOCIArtifactClientConstructionFailure(t *testing.T) {
	original := newOCIClient
	newOCIClient = func(*argoappv1.Repository, utilio.TempPaths) (ociClient, error) {
		return nil, errors.New("invalid reference")
	}
	t.Cleanup(func() { newOCIClient = original })

	_, _, err := fetchOCIArtifact(
		context.Background(), &RenderOptions{}, ociTestApp(), ociSource("6.7.0"),
		utilio.NewRandomizedTempPaths(t.TempDir()))
	if err == nil || !strings.Contains(err.Error(), "invalid reference") {
		t.Fatalf("error = %v, want the client-construction failure", err)
	}
}

func TestFetchOCIArtifactCanceledContext(t *testing.T) {
	fake := &fakeOCIClient{extractedDir: artifactFixture(t), digest: "sha256:" + strings.Repeat("d", 64)}
	stubNewOCIClient(t, fake, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := fetchOCIArtifact(ctx, &RenderOptions{}, ociTestApp(), ociSource("6.7.0"),
		utilio.NewRandomizedTempPaths(t.TempDir()))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if len(fake.resolvedRevisions) != 0 {
		t.Error("a canceled context still reached the registry")
	}
}

// TestNewOCIClientNeedsTheSchemeInTheRepoURL pins why the repo URL is handed to
// ArgoCD's OCI client WITH its oci:// prefix, unlike the chart client (which
// takes it scheme-less because `helm pull` re-adds it). NewClientWithLock trims
// the prefix for the repository reference but url.Parse()es the original to find
// the registry host, so trimming here would break every artifact fetch — and the
// failure would look like a registry problem, not a caller bug.
func TestNewOCIClientNeedsTheSchemeInTheRepoURL(t *testing.T) {
	paths := utilio.NewRandomizedTempPaths(t.TempDir())

	if _, err := newOCIClient(&argoappv1.Repository{Repo: "oci://ghcr.io/acme/mychart"}, paths); err != nil {
		t.Fatalf("newOCIClient() with an oci:// URL error = %v, want success", err)
	}
	if _, err := newOCIClient(&argoappv1.Repository{Repo: "ghcr.io/acme/mychart"}, paths); err == nil {
		t.Fatal("newOCIClient() with a scheme-less URL succeeded; the oci:// prefix is no longer load-bearing, re-check the trim")
	}
}

// TestOCIClientEventHandlersArePresent pins the nil-handler trap: ArgoCD's OCI
// client opens ResolveRevision (and Extract, GetTags, DigestMetadata, TestRepo)
// with `inc := c.OnX(c.repoURL)` and no nil check, so a client built without
// EventHandlers panics on its first call — as the first real fetch here did. The
// repo-server always installs metrics handlers, so upstream never meets the case.
//
// The panic precedes any network I/O, so a refused port is enough: what this
// asserts is that the call RETURNS an error instead of crashing the run.
func TestOCIClientEventHandlersArePresent(t *testing.T) {
	client, err := newOCIClient(
		&argoappv1.Repository{Repo: "oci://127.0.0.1:1/acme/artifactchart"},
		utilio.NewRandomizedTempPaths(t.TempDir()))
	if err != nil {
		t.Fatalf("newOCIClient() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := client.ResolveRevision(ctx, "6.7.0", true); err == nil {
		t.Fatal("ResolveRevision() against a refused port succeeded")
	}
}
