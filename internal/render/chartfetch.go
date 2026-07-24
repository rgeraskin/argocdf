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
// credential source is configured or resolution fails (rendering then
// proceeds anonymously and any real failure surfaces downstream with its
// cause).
func resolveRepoOrBare(ctx context.Context, opts *RenderOptions, project, repoURL string) *argoappv1.Repository {
	if opts.ResolveRepo != nil {
		if repo, err := opts.ResolveRepo(ctx, repoURL, project); err == nil && repo != nil {
			return repo
		}
	}
	return &argoappv1.Repository{Repo: repoURL}
}

// fetchRemoteChart materializes a source's remote chart as a local unpacked
// directory through ArgoCD's chart client, wrapped in argocdf's persistent
// chart cache (pinned versions only; a hit skips fetching — and auth —
// entirely). The returned cleanup must be called when the chart directory is
// no longer needed; for cached charts it is a no-op.
func (r *HelmRenderer) fetchRemoteChart(ctx context.Context, app *cluster.Application, source *cluster.ApplicationSource) (string, func(), error) {
	cacheDir, chartDir, hit, cacheEnabled := chartCacheDecision(
		r.opts.ChartCacheDir, source.RepoURL, source.Chart, source.TargetRevision, dirExists,
	)
	if cacheEnabled && hit {
		return chartDir, func() {}, nil
	}

	select {
	case <-ctx.Done():
		return "", nil, ctx.Err()
	default:
	}

	repo := resolveRepoOrBare(ctx, &r.opts, app.Spec.Project, source.RepoURL)
	// The resolved repo's EnableOCI is authoritative; the scheme-less URL
	// heuristic stays as the fallback for unconfigured repos.
	client := newChartClient(repo, repo.EnableOCI || isOCIChartRepo(source.RepoURL))

	version := source.TargetRevision
	if version == "HEAD" {
		version = "" // like empty, HEAD means "latest" for chart sources
	}
	// --pass-credentials comes from the application source, exactly as the
	// repo-server passes it to ExtractChart (reposerver/repository.go:423-427).
	passCredentials := source.Helm != nil && source.Helm.PassCredentials

	extracted, closer, err := client.ExtractChart(source.Chart, version, passCredentials, maxExtractedChartSize, false)
	if err != nil {
		if ctx.Err() != nil {
			return "", nil, ctx.Err()
		}
		return "", nil, fmt.Errorf("failed to fetch helm chart %s from %s: %w", source.Chart, source.RepoURL, err)
	}

	if cacheEnabled && publishChartToCache(extracted, cacheDir, chartDir) {
		_ = closer.Close()
		return chartDir, func() {}, nil
	}
	// Cache disabled (or publishing failed): serve the extracted directory
	// directly so rendering stays functional; cleanup removes it.
	return extracted, func() { _ = closer.Close() }, nil
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
