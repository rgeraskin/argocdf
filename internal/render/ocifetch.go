package render

import (
	"context"
	"fmt"

	argoappv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	utilio "github.com/argoproj/argo-cd/v3/util/io"
	argooci "github.com/argoproj/argo-cd/v3/util/oci"

	"github.com/rgeraskin/argocdf/internal/cluster"
)

// maxExtractedOCISize bounds OCI artifact extraction, mirroring the repo-server
// default (ARGOCD_REPO_SERVER_OCI_MANIFEST_MAX_EXTRACTED_SIZE=1G).
const maxExtractedOCISize = 1 << 30

// DefaultOCILayerMediaTypes mirrors the repo-server default for
// --oci-layer-media-types (ARGOCD_REPO_SERVER_OCI_LAYER_MEDIA_TYPES). Only
// layers with one of these media types count as the artifact's CONTENT layer,
// and ArgoCD requires exactly one such layer per artifact — so the list decides
// what argocdf can render at all, not merely what it prefers. It is a plain
// mirror of upstream's default rather than an argocdf flag: a user who has
// re-configured their repo-server would need the same value here, and inventing
// a flag before anyone asks for one would fork the default.
var DefaultOCILayerMediaTypes = []string{
	"application/vnd.oci.image.layer.v1.tar",
	"application/vnd.oci.image.layer.v1.tar+gzip",
	"application/vnd.cncf.helm.chart.content.v1.tar+gzip",
}

// ociClient is the slice of ArgoCD's util/oci.Client that artifact fetching
// uses — an interface seam so tests can stub the registry away.
type ociClient interface {
	ResolveRevision(ctx context.Context, revision string, noCache bool) (string, error)
	Extract(ctx context.Context, revision string) (string, utilio.Closer, error)
}

// newOCIClient builds ArgoCD's OCI client — the repo-server's own artifact path
// (newOCIClientResolveRevision plus the Extract call in runRepoOperation):
// digest resolution, semver-constraint resolution over the registry's tag list,
// ORAS pull with the repo's username/password + TLS + proxy, content-layer
// validation and size-bounded extraction.
//
// The repo URL is passed WITH its oci:// scheme: NewClientWithLock trims the
// prefix for the repository reference itself but url.Parse()es the original to
// find the registry host, so a scheme-less URL yields an empty host and the
// client fails to construct. Overridable in tests.
//
// imagePaths is where the pulled artifact tarball is memoized, keyed by
// registry URL + digest. Handing every client of a run the SAME registry makes
// the two sides of a diff (and every app sharing an artifact) pull once instead
// of once each, exactly as the repo-server's shared s.ociPaths does.
var newOCIClient = func(repo *argoappv1.Repository, imagePaths utilio.TempPaths) (ociClient, error) {
	return argooci.NewClient(
		repo.Repo, repo.GetOCICreds(), repo.Proxy, repo.NoProxy, DefaultOCILayerMediaTypes,
		argooci.WithImagePaths(imagePaths),
		argooci.WithManifestMaxExtractedSize(maxExtractedOCISize),
		argooci.WithEventHandlers(noopOCIEventHandlers()),
	)
}

// noopOCIEventHandlers supplies the observability hooks ArgoCD's OCI client
// calls UNCONDITIONALLY and without a nil guard — `inc := c.OnResolveRevision(...)`
// is the first statement of ResolveRevision (util/oci/client.go:373), and Extract,
// GetTags, DigestMetadata and TestRepo all open the same way. The repo-server
// always installs metrics handlers, so upstream never meets the nil case; a
// client built without them panics on its first call, which is what the first
// live fetch here did. argocdf has no metrics server, so the hooks do nothing —
// but they must EXIST.
func noopOCIEventHandlers() argooci.EventHandlers {
	span := func(string) func() { return func() {} }
	spanFail := func(string) func(string) { return func(string) {} }
	fail := func(string) func() { return func() {} }
	return argooci.EventHandlers{
		OnExtract:             span,
		OnResolveRevision:     span,
		OnDigestMetadata:      span,
		OnTestRepo:            span,
		OnGetTags:             span,
		OnExtractFail:         spanFail,
		OnResolveRevisionFail: spanFail,
		OnDigestMetadataFail:  spanFail,
		OnTestRepoFail:        fail,
		OnGetTagsFail:         fail,
	}
}

// fetchOCIArtifact materializes an OCI-artifact source as a local unpacked
// directory through ArgoCD's OCI client. The returned cleanup removes the
// extraction (the pulled tarball stays in imagePaths for the rest of the run).
//
// digest is the resolved artifact digest — what ArgoCD hands GenerateManifests
// as the revision for an OCI source, and therefore what ARGOCD_APP_REVISION*
// must report. It is the artifact's content identity, which is what a template
// substituting the revision into an image tag actually wants; the git commit that
// happened to reference it is not.
//
// Unlike remote charts this is NOT wrapped in a persistent cross-run cache. The
// tarball write is not atomic (ArgoCD's saveCompressedImageToPath tars straight
// onto the cache path), so a run killed mid-pull would leave a truncated file
// that every later run would then serve as a valid artifact — the chart cache
// avoids that with a staging directory and an atomic rename, which is only
// possible because argocdf drives the extraction itself there. Repeat runs are
// served by the RENDER cache instead, which never re-enters this path.
func fetchOCIArtifact(
	ctx context.Context,
	opts *RenderOptions,
	app *cluster.Application,
	source *cluster.ApplicationSource,
	imagePaths utilio.TempPaths,
) (dir, digest string, cleanup func(), err error) {
	select {
	case <-ctx.Done():
		return "", "", nil, ctx.Err()
	default:
	}

	repo, err := resolveRepoOrBare(ctx, opts, app.Spec.Project, source.RepoURL)
	if err != nil {
		return "", "", nil, err
	}

	client, err := newOCIClient(repo, imagePaths)
	if err != nil {
		return "", "", nil, &ociFetchError{repoURL: source.RepoURL, revision: source.TargetRevision, cause: err}
	}

	// noCache=true: argocdf wires no tags cache, so the flag cannot change the
	// outcome — it is passed positively to say so, rather than implying a cache
	// that does not exist. Registry auth is the RESOLVED repository's
	// username/password, unstripped: OCI artifacts authenticate through ORAS
	// directly, never through the helm registry config the chart path strips
	// credentials for (see registryAuthFile).
	digest, err = client.ResolveRevision(ctx, source.TargetRevision, true)
	if err != nil {
		return "", "", nil, &ociFetchError{repoURL: source.RepoURL, revision: source.TargetRevision, cause: err}
	}

	extracted, closer, err := client.Extract(ctx, digest)
	if err != nil {
		if ctx.Err() != nil {
			return "", "", nil, ctx.Err()
		}
		return "", "", nil, &ociFetchError{repoURL: source.RepoURL, revision: source.TargetRevision, cause: err}
	}

	return extracted, digest, func() { _ = closer.Close() }, nil
}

// ociFetchError reports a failed OCI artifact fetch with per-run temp paths
// redacted, keeping the cause chain intact for errors.Is/As — the same contract
// as chartFetchError, for the same reason: this text reaches PR comments.
type ociFetchError struct {
	repoURL  string
	revision string
	cause    error
}

func (e *ociFetchError) Error() string {
	return fmt.Sprintf("failed to fetch oci artifact %s at revision %s: %s",
		e.repoURL, e.revision, redactArgoTempPaths(e.cause.Error()))
}

func (e *ociFetchError) Unwrap() error { return e.cause }
