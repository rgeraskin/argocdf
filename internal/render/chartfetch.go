package render

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	argoappv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	argohelm "github.com/argoproj/argo-cd/v3/util/helm"
	utilio "github.com/argoproj/argo-cd/v3/util/io"
	argoversions "github.com/argoproj/argo-cd/v3/util/versions"

	"github.com/rgeraskin/argocdf/internal/cluster"
)

// maxExtractedChartSize bounds chart extraction, mirroring the repo-server
// default (ARGOCD_HELM_MANIFEST_MAX_EXTRACTED_SIZE=1G).
const maxExtractedChartSize = 1 << 30

// maxHelmIndexSize bounds a helm repository index download, mirroring the
// repo-server default (ARGOCD_REPO_SERVER_HELM_MANIFEST_MAX_INDEX_SIZE=1G). It
// is only read when a chart source's target revision is a CONSTRAINT, which is
// the only case that needs the index at all.
const maxHelmIndexSize = 1 << 30

// chartClient is the slice of ArgoCD's util/helm.Client that chart fetching
// uses — an interface seam so tests can stub the network away.
type chartClient interface {
	ExtractChart(chart string, version string, passCredentials bool, manifestMaxExtractedSize int64, disableManifestMaxExtractedSize bool) (string, utilio.Closer, error)
	// GetTags and GetIndex serve constraint resolution (resolveChartRevision):
	// an OCI chart repository lists tags, a classic one serves an index.
	GetTags(chart string, noCache bool) ([]string, error)
	GetIndex(noCache bool, maxIndexSize int64) (*argohelm.Index, error)
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

// resolveChartRevision returns the chart version ArgoCD's repo-server would
// resolve a chart source's target revision to: the revision itself when it names
// a version, otherwise the maximum published version satisfying it as a semver
// constraint. It mirrors newHelmClientResolveRevision's revision half
// (repository.go:2618-2647) and delegates both decisions to ArgoCD's own
// util/versions, so "is this a version" and "which version wins" cannot drift.
//
// The resolved version is what feeds ARGOCD_APP_REVISION* and what the pull is
// pinned to — one resolution, as upstream does it, rather than one for the label
// and another inside helm.
//
// noCache=true because argocdf wires no index cache; the flag says so rather
// than implying one.
func resolveChartRevision(client chartClient, chart, revision string, enableOCI bool) (string, error) {
	if argoversions.IsVersion(revision) {
		return revision, nil
	}
	// An EMPTY revision short-circuits before the registry: no tag can be empty,
	// so MaxVersion's constraint parse and its string-equality fallback both fail
	// no matter what the index says, and the round trip is pure waste — a full
	// index download for a classic repository, on every render, since an empty
	// revision also bypasses the render cache. "HEAD" deliberately does NOT
	// short-circuit: a registry may publish a literal HEAD tag, which the
	// string-equality fallback would match, so skipping it would deviate from
	// upstream for a shape that is at least possible.
	if revision == "" {
		return "", errors.New("empty revision is neither a version nor a constraint")
	}
	var tags []string
	if enableOCI {
		t, err := client.GetTags(chart, true)
		if err != nil {
			return "", fmt.Errorf("unable to get tags: %w", err)
		}
		tags = t
	} else {
		index, err := client.GetIndex(true, maxHelmIndexSize)
		if err != nil {
			return "", err
		}
		entries, err := index.GetEntries(chart)
		if err != nil {
			return "", err
		}
		tags = entries.Tags()
	}
	return argoversions.MaxVersion(revision, tags)
}

// chartClientForSource builds ArgoCD's chart client for a chart source and
// reports whether its repository is an OCI registry — the verdict the client is
// built with, and the one revision resolution needs too.
func chartClientForSource(ctx context.Context, opts *RenderOptions, project string, source *cluster.ApplicationSource) (chartClient, bool, error) {
	repo, err := resolveRepoOrBare(ctx, opts, project, source.RepoURL)
	if err != nil {
		return nil, false, err
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
				return nil, false, fmt.Errorf("failed to record registry credentials for %s: %w", source.RepoURL, err)
			}
		}
		if auth != nil || opts.HelmRegistryConfig != "" {
			repo = repo.DeepCopy()
			repo.Username = ""
			repo.Password = ""
		}
	}
	return newChartClient(repo, enableOCI), enableOCI, nil
}

// resolveChartVersion returns the version to PULL and the revision the render is
// LABELLED with, alongside the resolution error the caller reads to decide
// whether that version can re-decide the download cache.
//
// Resolve the revision the way the repo-server does, and pull THAT: the version
// is both the pin for `helm pull --version` and the value of
// ARGOCD_APP_REVISION*, so resolving once is what keeps the label and the pulled
// chart from disagreeing.
//
// An UNRESOLVABLE revision is not fatal here, and that is deliberate: the two
// shapes that reach it are argocdf's own "HEAD/empty means latest" (a documented
// deviation — upstream's resolution rejects both, since neither is a version or
// a semver constraint) and a genuine registry failure, which the pull reports
// loudly a moment later against the same registry. Failing the render here would
// turn the first case, which renders today, into an error while adding nothing
// to the second. The fallback keeps the pull exactly as it was and labels the
// build env with what the application DECLARED — never with the git commit,
// which is the bug being fixed.
func resolveChartVersion(client chartClient, opts *RenderOptions, app *cluster.Application, source *cluster.ApplicationSource, enableOCI bool) (version, resolved string, resolveErr error) {
	version = source.TargetRevision
	if version == "HEAD" {
		version = "" // like empty, HEAD means "latest" for chart sources
	}
	resolved, resolveErr = resolveChartRevision(client, source.Chart, source.TargetRevision, enableOCI)
	if resolveErr == nil {
		version = resolved
	} else {
		resolved = source.TargetRevision
		// Say so once, at DEBUG: the fallback is silent by design but it changes
		// a user-visible value (ARGOCD_APP_REVISION reports the declared revision
		// rather than a resolved version), and nothing else in the run explains
		// why.
		if opts.Logger != nil {
			opts.Logger.Debug("Chart revision not resolved; labelling the render with the declared revision",
				"app", app.Name,
				"chart", source.Chart,
				"revision", source.TargetRevision,
				"reason", resolveErr)
		}
	}
	return version, resolved, resolveErr
}

// fetchRemoteChart materializes a source's remote chart as a local unpacked
// directory through ArgoCD's chart client, wrapped in argocdf's persistent
// chart cache (pinned versions only; a hit skips fetching — and auth —
// entirely). cached reports whether the returned directory lives in the
// SHARED persistent cache (callers that mutate the chart must copy it first).
// The returned cleanup must be called when the chart directory is no longer
// needed; for cached charts it is a no-op.
//
// revision is the RESOLVED chart version — what ArgoCD hands GenerateManifests
// for a chart source, and therefore what ARGOCD_APP_REVISION* must report. It is
// not the git commit: a chart's content has nothing to do with the commit that
// referenced it, and using the commit made every app substituting
// $ARGOCD_APP_REVISION into its helm values differ between two sides that pull
// the SAME pinned chart.
func fetchRemoteChart(ctx context.Context, opts *RenderOptions, app *cluster.Application, source *cluster.ApplicationSource) (dir, revision string, cached bool, cleanup func(), err error) {
	cacheDir, chartDir, hit, cacheEnabled := chartCacheDecision(
		opts.ChartCacheDir, source.RepoURL, source.Chart, source.TargetRevision, dirExists,
	)
	if cacheEnabled && hit {
		// A hit means the revision passed IsImmutableChartVersion, i.e. one
		// exact version — which is what ArgoCD's resolution returns unchanged.
		// So the build-env label needs no client here, and the hit keeps
		// skipping the registry (and auth) entirely.
		return chartDir, source.TargetRevision, true, func() {}, nil
	}

	select {
	case <-ctx.Done():
		return "", "", false, nil, ctx.Err()
	default:
	}

	client, enableOCI, err := chartClientForSource(ctx, opts, app.Spec.Project, source)
	if err != nil {
		return "", "", false, nil, err
	}

	version, resolved, resolveErr := resolveChartVersion(client, opts, app, source, enableOCI)

	// A CONSTRAINT is not cacheable as written — it resolves against the mutable
	// registry index, so the same string can mean different content next week —
	// but the version it resolved TO is. Re-deciding the cache with the resolved
	// version splits those two halves: the mutable one is re-resolved from the
	// registry on every run (the index or tag fetch just above), and only the
	// immutable one is served from disk. So `^1.2.0` now reuses a download
	// instead of re-pulling the chart every run, and a constraint whose maximum
	// moves resolves to a different version and therefore a different key.
	//
	// The render cache is untouched by this: a constraint revision still bypasses
	// it (rendercache's IsImmutableChartVersion rule), which is exactly why the
	// download cache is worth having here — such an app re-renders every run.
	//
	// Note this hit costs the credential resolution and the index fetch, unlike
	// the pinned hit above which contacts nothing at all: resolving the
	// constraint is what produced the key.
	if !cacheEnabled && resolveErr == nil {
		cacheDir, chartDir, hit, cacheEnabled = chartCacheDecision(
			opts.ChartCacheDir, source.RepoURL, source.Chart, resolved, dirExists,
		)
		if cacheEnabled && hit {
			return chartDir, resolved, true, func() {}, nil
		}
	}
	// --pass-credentials comes from the application source, exactly as the
	// repo-server passes it to ExtractChart (reposerver/repository.go:423-427).
	passCredentials := source.Helm != nil && source.Helm.PassCredentials

	extracted, closer, err := extractChartInterruptible(ctx, client, source.Chart, version, passCredentials)
	if err != nil {
		if ctx.Err() != nil {
			return "", "", false, nil, ctx.Err()
		}
		return "", "", false, nil, &chartFetchError{
			chart:   source.Chart,
			repoURL: source.RepoURL,
			cause:   err,
		}
	}

	if cacheEnabled && publishChartToCache(extracted, cacheDir, chartDir) {
		_ = closer.Close()
		return chartDir, resolved, true, func() {}, nil
	}
	// Cache disabled (or publishing failed): serve the extracted directory
	// directly so rendering stays functional; cleanup removes it.
	return extracted, resolved, false, func() { _ = closer.Close() }, nil
}

// argoTempPathRe matches the per-run temp directories ArgoCD's own fetchers
// create: os.TempDir() plus a UUID — what the chart client passes to
// `helm pull --destination`, and what the OCI client extracts an artifact into.
// argocdf never sees those paths (files.CreateTempDir is called inside
// ExtractChart / Extract), so they can only be recognized by shape.
var argoTempPathRe = regexp.MustCompile(
	`[^ \x60"]*/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// chartFetchError reports a failed chart fetch with per-run temp paths redacted,
// WITHOUT flattening the cause: Error() renders the redacted text while Unwrap keeps
// the chain intact, so errors.Is/As still reach the exec and net errors underneath.
// Formatting with %s instead would have read identically and silently broken any
// future retry-on-401 or context check - a match that never fires rather than a
// compile error.
type chartFetchError struct {
	chart   string
	repoURL string
	cause   error
}

func (e *chartFetchError) Error() string {
	return fmt.Sprintf("failed to fetch helm chart %s from %s: %s",
		e.chart, e.repoURL, redactArgoTempPaths(e.cause.Error()))
}

func (e *chartFetchError) Unwrap() error { return e.cause }

// redactArgoTempPaths replaces those paths with a stable token.
//
// A failed pull surfaces helm's whole argv, temp destination included, and that
// lands verbatim in a report - which for argocdf usually means a PR comment. The
// path is deleted by the time anyone reads it, so it carries no diagnostic value,
// and leaving it in makes every re-run rewrite the comment with a new path. The
// registry URL, the chart, the version and the underlying error all survive.
func redactArgoTempPaths(msg string) string {
	return argoTempPathRe.ReplaceAllString(msg, "(temp dir)")
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
