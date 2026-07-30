package render

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	argoappv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	argohelm "github.com/argoproj/argo-cd/v3/util/helm"
	utilio "github.com/argoproj/argo-cd/v3/util/io"

	"github.com/rgeraskin/argocdf/internal/cluster"
)

// maxExtractedChartSize bounds chart extraction, mirroring the repo-server
// default (ARGOCD_HELM_MANIFEST_MAX_EXTRACTED_SIZE=1G).
const maxExtractedChartSize = 1 << 30

// chartClient is the slice of ArgoCD's util/helm.Client that chart fetching
// uses — an interface seam so tests can stub the network away.
type chartClient interface {
	ExtractChart(chart string, version string, passCredentials bool, manifestMaxExtractedSize int64, disableManifestMaxExtractedSize bool) (string, utilio.Closer, error)
}

// newChartClient builds ArgoCD's chart client — the repo-server's own
// chart-fetch path: OCI-vs-classic dispatch, registry login/logout,
// username/password + TLS + proxy from the repo's creds, an isolated temp
// helm home per helm exec, and per-repo locking. The repo URL is passed
// scheme-less because the client prepends oci:// itself (PullOCI).
// Overridable in tests.
var newChartClient = func(repo *argoappv1.Repository, enableOCI bool) chartClient {
	return argohelm.NewClient(strings.TrimPrefix(repo.Repo, "oci://"), repo.GetHelmCreds(), enableOCI, repo.Proxy, repo.NoProxy)
}

// resolveRepoOrBare returns the Repository configured for repoURL via
// opts.ResolveRepo — with credentials, proxy, and TLS settings from the
// --repo-creds source — or a bare credential-less Repository when no
// credential source is configured. A FAILING resolution is an error, not a
// silent fallback: degrading to an anonymous fetch would bury the root cause
// under a misleading 401/not-found from helm.
func resolveRepoOrBare(ctx context.Context, opts *RenderOptions, project, repoURL string) (*argoappv1.Repository, error) {
	if opts.ResolveRepo == nil {
		return &argoappv1.Repository{Repo: repoURL}, nil
	}
	repo, err := opts.ResolveRepo(ctx, repoURL, project)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve repository credentials for %s: %w", repoURL, err)
	}
	if repo == nil {
		return &argoappv1.Repository{Repo: repoURL}, nil
	}
	return repo, nil
}

// isOCIChartRepo reports whether a chart source's repoURL refers to an OCI
// registry. ArgoCD stores OCI helm repositories scheme-less (e.g.
// "ghcr.io/org", with enableOCI set on the repository secret), so a chart
// repoURL is OCI both with an explicit oci:// scheme and with no scheme at
// all — http(s):// are the only classic chart-repository forms.
func isOCIChartRepo(repoURL string) bool {
	return strings.HasPrefix(repoURL, "oci://") || !strings.Contains(repoURL, "://")
}

// fetchRemoteChart materializes a source's remote chart as a local unpacked
// directory through ArgoCD's chart client, wrapped in argocdf's persistent
// chart cache (pinned versions only; a hit skips fetching — and auth —
// entirely). cached reports whether the returned directory lives in the
// SHARED persistent cache (callers that mutate the chart must copy it first).
// The returned cleanup must be called when the chart directory is no longer
// needed; for cached charts it is a no-op.
func fetchRemoteChart(ctx context.Context, opts *RenderOptions, app *cluster.Application, source *cluster.ApplicationSource) (dir string, cached bool, cleanup func(), err error) {
	cacheDir, chartDir, hit, cacheEnabled := chartCacheDecision(
		opts.ChartCacheDir, source.RepoURL, source.Chart, source.TargetRevision, dirExists,
	)
	if cacheEnabled && hit {
		return chartDir, true, func() {}, nil
	}

	select {
	case <-ctx.Done():
		return "", false, nil, ctx.Err()
	default:
	}

	repo, err := resolveRepoOrBare(ctx, opts, app.Spec.Project, source.RepoURL)
	if err != nil {
		return "", false, nil, err
	}
	// The resolved repo's EnableOCI is authoritative; the scheme-less URL
	// heuristic stays as the fallback for unconfigured repos.
	enableOCI := repo.EnableOCI || isOCIChartRepo(source.RepoURL)
	// OCI credentials ride the engine's registry auth file instead of ArgoCD's
	// `helm registry login` (which on macOS writes to the shared system
	// keychain — see registryAuthFile). Recording them lazily here covers
	// repos known only through ResolveRepo. Under --repo-creds=local there is
	// no owned auth file: OCI auth is defined to ride the user's own registry
	// config (the pierced HELM_REGISTRY_CONFIG), and inline username/password
	// on an OCI-flavored entry — nothing `helm repo add` can produce — would
	// reintroduce the login, so it is stripped the same way without seeding.
	// The stripped copy makes ExtractChart skip its login/logout entirely; the
	// helm pull then authenticates from the effective registry config.
	// Failures are LOUD, per the no-anonymous-fallback contract above.
	if enableOCI && repo.Username != "" && repo.Password != "" {
		auth := opts.registryAuth
		if auth != nil {
			if err := auth.Ensure(repo.Repo, repo.Username, repo.Password); err != nil {
				return "", false, nil, fmt.Errorf("failed to record registry credentials for %s: %w", source.RepoURL, err)
			}
		}
		if auth != nil || opts.HelmRegistryConfig != "" {
			repo = repo.DeepCopy()
			repo.Username = ""
			repo.Password = ""
		}
	}
	client := newChartClient(repo, enableOCI)

	version := source.TargetRevision
	if version == "HEAD" {
		version = "" // like empty, HEAD means "latest" for chart sources
	}
	// --pass-credentials comes from the application source, exactly as the
	// repo-server passes it to ExtractChart (reposerver/repository.go:423-427).
	passCredentials := source.Helm != nil && source.Helm.PassCredentials

	extracted, closer, err := extractChartInterruptible(ctx, client, source.Chart, version, passCredentials)
	if err != nil {
		if ctx.Err() != nil {
			return "", false, nil, ctx.Err()
		}
		return "", false, nil, fmt.Errorf("failed to fetch helm chart %s from %s: %w", source.Chart, source.RepoURL, err)
	}

	if cacheEnabled && publishChartToCache(extracted, cacheDir, chartDir) {
		_ = closer.Close()
		return chartDir, true, func() {}, nil
	}
	// Cache disabled (or publishing failed): serve the extracted directory
	// directly so rendering stays functional; cleanup removes it.
	return extracted, false, func() { _ = closer.Close() }, nil
}

// extractChartInterruptible runs ExtractChart in a goroutine so cancellation
// (SIGINT/SIGTERM) returns promptly: ArgoCD's chart client takes no context —
// its helm exec is bounded by ArgoCD's own exec timeout instead. On
// cancellation the in-flight fetch is left to drain in the background, where
// its extraction closer then releases the temp directory.
func extractChartInterruptible(ctx context.Context, client chartClient, chart, version string, passCredentials bool) (string, utilio.Closer, error) {
	type extractResult struct {
		path   string
		closer utilio.Closer
		err    error
	}
	results := make(chan extractResult, 1)
	go func() {
		path, closer, err := client.ExtractChart(chart, version, passCredentials, maxExtractedChartSize, false)
		results <- extractResult{path: path, closer: closer, err: err}
	}()

	select {
	case <-ctx.Done():
		go func() {
			if res := <-results; res.closer != nil {
				_ = res.closer.Close()
			}
		}()
		return "", nil, ctx.Err()
	case res := <-results:
		return res.path, res.closer, res.err
	}
}

// copyChartToTempDir copies a chart directory into a fresh private temp dir.
// Used before handing a SHARED cache entry to code that may mutate it
// (GenerateManifests builds dependencies into appPath): the process-local
// chartDepMutex cannot protect the persistent cache from concurrent argocdf
// PROCESSES, so shared entries must stay pristine.
func copyChartToTempDir(chartDir string) (string, func(), error) {
	tmp, err := os.MkdirTemp("", "argocdf-chart-")
	if err != nil {
		return "", nil, fmt.Errorf("failed to create temp chart dir: %w", err)
	}
	cleanup := func() { _ = SafeRemoveAll(tmp) }
	dst := filepath.Join(tmp, filepath.Base(chartDir))
	if err := os.MkdirAll(dst, 0o755); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("failed to create private chart dir: %w", err)
	}
	if err := copyDir(chartDir, dst); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("failed to copy cached chart: %w", err)
	}
	return dst, cleanup, nil
}

// publishChartToCache copies an extracted chart directory into the persistent
// cache with an atomic claim: the chart is staged in a sibling directory of
// the cache entry (same filesystem — extraction happens under os.TempDir,
// which may be a different device, so this is a copy, not a rename) and then
// renamed into place, so concurrent renders never observe a partial chart.
// The extracted source directory is left intact; on any failure the caller
// keeps serving it. A concurrent publisher winning the rename race counts as
// success.
func publishChartToCache(extracted, cacheDir, chartDir string) bool {
	parent := filepath.Dir(cacheDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return false
	}
	staging, err := os.MkdirTemp(parent, "argocdf-chart-*.tmp")
	if err != nil {
		return false
	}
	defer func() { _ = SafeRemoveAll(staging) }()

	// chartDir is cacheDir/<chart base name>; recreate that shape in staging.
	// copyDir copies directory CONTENTS, so the destination root is created
	// here first.
	staged := filepath.Join(staging, filepath.Base(chartDir))
	if err := os.MkdirAll(staged, 0o755); err != nil {
		return false
	}
	if err := copyDir(extracted, staged); err != nil {
		return false
	}
	if err := os.Rename(staging, cacheDir); err != nil {
		return dirExists(chartDir)
	}
	return true
}
