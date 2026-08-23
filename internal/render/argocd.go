// Package render provides manifest rendering; this file implements argocdf's
// render engine, which renders through ArgoCD's own repo-server code
// (reposerver/repository.GenerateManifests) for exact ArgoCD parity.
//
// What ArgoCD's code takes over here: source-type dispatch (helm / kustomize /
// directory, including .argocd-source*.yaml overrides), the complete
// ApplicationSourceHelm/Kustomize option translation, ARGOCD_APP_* build-env
// substitution, helm's --include-crds default, and dependency building into an
// isolated temp helm home (no user helm config is touched, and dependency
// repos from Chart.yaml are registered there automatically).
//
// What stays argocdf's: worktree management, remote-chart fetching (ArgoCD's
// repo-server fetches charts before calling GenerateManifests, so this file
// goes through the persistent chart download cache), $ref-source checkout,
// and the render cache.
package render

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	argoappv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v3/reposerver/apiclient"
	"github.com/argoproj/argo-cd/v3/reposerver/repository"
	argogit "github.com/argoproj/argo-cd/v3/util/git"
	argohelm "github.com/argoproj/argo-cd/v3/util/helm"
	utilio "github.com/argoproj/argo-cd/v3/util/io"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	"github.com/rgeraskin/argocdf/internal/cluster"
	"github.com/rgeraskin/argocdf/internal/git"
	"github.com/rgeraskin/argocdf/internal/types"
)

// maxCombinedDirectoryManifestsSize bounds the combined size of manifest files
// a directory-type source may produce, mirroring the repo-server default
// (ARGOCD_REPO_SERVER_MAX_COMBINED_DIRECTORY_MANIFESTS_SIZE=10M). It must be
// non-zero: a zero quantity makes GenerateManifests reject every file.
var maxCombinedDirectoryManifestsSize = resource.MustParse("10M")

// DefaultValuesFileSchemes mirrors ArgoCD's default helm.valuesFileSchemes
// setting (util/settings): value files may be fetched over these URL schemes.
// Exported because app SELECTION must classify a value file as remote exactly as
// the render does - a second copy of the list would be a drift of its own.
var DefaultValuesFileSchemes = []string{"https", "http"}

// KustomizationNames contains the known kustomization file names.
var KustomizationNames = []string{"kustomization.yaml", "kustomization.yml", "Kustomization"}

// chartDepLocks serializes chart-directory writes (GenerateManifests' helm
// dependency builds, kustomize edits) per directory. When apps render in
// parallel from a shared worktree, two apps pointing at the same path would
// otherwise interleave those writes. It maps an absolute chart path to its
// *sync.Mutex.
var chartDepLocks sync.Map

// chartDepMutex returns the mutex guarding chart-directory writes for the
// given path, creating it on first use.
func chartDepMutex(chartPath string) *sync.Mutex {
	m, _ := chartDepLocks.LoadOrStore(chartPath, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// isPureRef reports whether a source is used ONLY as a ref and produces no
// manifests. ArgoCD's IsRef() is simply Ref != "", but a source may legitimately
// both render manifests (via Path/Chart/an oci:// artifact) AND be referenced by
// other sources. Only a source with a Ref and none of those is skipped from
// rendering.
//
// The IsOCI exclusion is upstream's own: an OCI-artifact source renders from the
// artifact ROOT, so an empty Path is normal there and would otherwise read as
// "ref only" (repository.go:591 spells the same carve-out).
func isPureRef(source cluster.ApplicationSource) bool {
	return source.Ref != "" && source.Path == "" && source.Chart == "" && !source.IsOCI()
}

// ArgoCDRenderer renders applications through ArgoCD's repo-server code.
type ArgoCDRenderer struct {
	opts RenderOptions
	// ownedRegistryAuth is the per-run registry auth file when this engine
	// created one (all --repo-creds modes except local, which pierces with the
	// user's own registry config instead). Removed by Cleanup.
	ownedRegistryAuth *registryAuthFile
	// restoreHelmEnv undoes the process-global helm env mutation made at
	// construction (isolateHelmEnv). Run by Cleanup.
	restoreHelmEnv func()
	// ociPaths memoizes pulled OCI artifact tarballs for the whole run, keyed
	// by registry URL + digest — the repo-server's shared s.ociPaths, scoped to
	// one argocdf run instead of a server lifetime, so both sides of a diff
	// pull an artifact once. ociPathsRoot is its directory, removed by Cleanup.
	ociPaths     utilio.TempPaths
	ociPathsRoot string
}

// NewArgoCDRenderer creates a renderer backed by reposerver's GenerateManifests.
//
// Construction has process-global side effects that make ArgoCD's helm
// isolation actually hold outside a repo-server container: the inherited helm
// environment is scrubbed (see inheritedHelmEnvVars) and HELM_REGISTRY_CONFIG
// is pointed at either the user's registry config (--repo-creds=local, via
// opts.HelmRegistryConfig) or an argocdf-owned per-run auth file seeded from
// the credential lists — whose OCI entries are then stripped of
// username/password so ArgoCD's DependencyBuild never execs `helm registry
// login`/`logout` (on macOS those land in the shared system keychain via
// ORAS native-store detection and race across concurrent renders).
func NewArgoCDRenderer(opts RenderOptions) (*ArgoCDRenderer, error) {
	r := &ArgoCDRenderer{}
	registryConfig := opts.HelmRegistryConfig
	if registryConfig == "" {
		auth, err := newRegistryAuthFile()
		if err != nil {
			return nil, err
		}
		opts.HelmRepos, err = seedAndStripRepos(auth, opts.HelmRepos)
		if err == nil {
			opts.OCIRepos, err = seedAndStripRepos(auth, opts.OCIRepos)
		}
		if err == nil {
			opts.HelmRepoCreds, err = seedAndStripRepoCreds(auth, opts.HelmRepoCreds)
		}
		if err == nil {
			opts.OCIRepoCreds, err = seedAndStripRepoCreds(auth, opts.OCIRepoCreds)
		}
		if err != nil {
			auth.Remove()
			return nil, err
		}
		opts.registryAuth = auth
		r.ownedRegistryAuth = auth
		registryConfig = auth.path
	}
	r.restoreHelmEnv = isolateHelmEnv(registryConfig)

	// The artifact tarball registry is created eagerly (one empty directory per
	// run) so no render path has to synchronize its construction.
	ociRoot, err := os.MkdirTemp("", "argocdf-oci-")
	if err != nil {
		r.Cleanup()
		return nil, fmt.Errorf("failed to create temp dir for oci artifacts: %w", err)
	}
	r.ociPathsRoot = ociRoot
	r.ociPaths = utilio.NewRandomizedTempPaths(ociRoot)

	r.opts = opts
	return r, nil
}

// ociImagePaths returns the run's artifact tarball registry. A renderer built
// as a zero value (unit tests that never call NewArgoCDRenderer) gets a
// throwaway registry under the system temp dir instead of a nil one.
func (r *ArgoCDRenderer) ociImagePaths() utilio.TempPaths {
	if r.ociPaths != nil {
		return r.ociPaths
	}
	return utilio.NewRandomizedTempPaths(os.TempDir())
}

// Cleanup restores the pre-construction helm environment and removes the
// per-run registry auth file (it holds short-lived tokens). Safe to call
// multiple times and on a renderer that owns no auth file. The renderer must
// not render after Cleanup. The env restore is a snapshot of THIS instance's
// construction-time env: the helm env is process-global, so overlapping
// instances must be cleaned up in LIFO order — an out-of-order Cleanup
// rewinds a still-live instance's HELM_REGISTRY_CONFIG.
func (r *ArgoCDRenderer) Cleanup() {
	if r.restoreHelmEnv != nil {
		r.restoreHelmEnv()
		r.restoreHelmEnv = nil
	}
	if r.ownedRegistryAuth != nil {
		r.ownedRegistryAuth.Remove()
	}
	if r.ociPathsRoot != "" {
		_ = SafeRemoveAll(r.ociPathsRoot)
		r.ociPathsRoot = ""
		r.ociPaths = nil
	}
}

// RenderApplication renders all sources of an application via ArgoCD's
// GenerateManifests and concatenates the results as multi-doc YAML. revision is
// the commit being rendered; it feeds the ARGOCD_APP_REVISION* build-env
// variables for sources that come FROM that commit — see sourceRevision in
// renderSource for the remote-source cases, where the repo-server labels the
// render with the resolved chart version or artifact digest instead.
func (r *ArgoCDRenderer) RenderApplication(ctx context.Context, app *cluster.Application, repoPath, revision string) (*RenderResult, error) {
	sources := app.Spec.GetSources()
	if len(sources) == 0 {
		return &RenderResult{SourceType: types.SourceTypeUnknown}, nil
	}

	refSources, tempPaths, cleanup, err := r.prepareRefSources(ctx, app.Spec.Project, sources, repoPath)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare ref sources: %w", err)
	}
	defer cleanup()

	// Renderable sources from OTHER git repositories (apps-of-apps children
	// and multi-source apps may reference them) render from their own
	// checkout at TargetRevision, never from the local worktree.
	externalRepos := newExternalRepoSet(&r.opts, app.Spec.Project)
	defer externalRepos.cleanup()

	var all bytes.Buffer
	sourceType := types.SourceTypeUnknown
	for i := range sources {
		// Pure ref sources produce no manifests.
		if isPureRef(sources[i]) {
			continue
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		srcRepoPath, srcRepoRevision, err := externalRepos.repoPathFor(ctx, &sources[i], repoPath)
		if err != nil {
			rerr := fmt.Errorf("failed to render source %d: %w", i, err)
			return &RenderResult{Error: rerr}, rerr
		}
		// A source rendered from ANOTHER repository is rendered at that
		// repository's resolved commit, so that is the revision it reports (see
		// sourceRevision in renderSource). Empty means the source renders from the
		// local worktree, where the commit being diffed is the right answer.
		srcRevision := revision
		if srcRepoRevision != "" {
			srcRevision = srcRepoRevision
		}

		manifests, srcType, err := r.renderSource(ctx, app, &sources[i], srcRepoPath, srcRevision, refSources, tempPaths)
		if err != nil {
			rerr := fmt.Errorf("failed to render source %d: %w", i, err)
			return &RenderResult{Error: rerr}, rerr
		}
		if sourceType == types.SourceTypeUnknown {
			sourceType = srcType
		}
		if all.Len() > 0 && len(manifests) > 0 {
			all.WriteString("---\n")
		}
		all.Write(manifests)
	}

	return &RenderResult{Manifests: all.Bytes(), SourceType: sourceType}, nil
}

// renderSource renders a single source through GenerateManifests.
func (r *ArgoCDRenderer) renderSource(
	ctx context.Context,
	app *cluster.Application,
	source *cluster.ApplicationSource,
	repoPath, revision string,
	refSources map[string]*argoappv1.RefTarget,
	tempPaths utilio.TempPaths,
) ([]byte, types.SourceType, error) {
	// sourceRevision is what GenerateManifests is handed as the revision, and
	// therefore what ARGOCD_APP_REVISION/_SHORT/_SHORT_8 report to a helm
	// values/parameter substitution. ArgoCD resolves it PER SOURCE
	// (runRepoOperation passes what the source's own client resolved): the commit
	// for a git source — the EXTERNAL repository's commit when the source lives in
	// one, resolved by the caller — the resolved chart version for a chart, the
	// digest for an OCI artifact. Handing a remote source the git commit — as argocdf did until
	// this became per-source — mislabelled the render and made two sides pulling
	// the SAME pinned chart differ, because the commit is the one input that
	// always changes between them.
	var appPath, repoRoot, sourceRevision string
	switch {
	case source.IsOCI():
		// OCI-artifact source (ArgoCD 3.1+): the artifact IS the app. The
		// repo-server resolves TargetRevision to a digest, pulls the artifact,
		// unpacks its single content layer and renders source.Path INSIDE the
		// extracted tree (repository.go:377-414) — ordinary source-type sniffing
		// then decides helm/kustomize/directory from what is in there.
		//
		// This case is FIRST because upstream's dispatch is (repository.go:342):
		// IsOCI() is tested before IsHelm(), so an oci:// URL carrying a chart:
		// field renders as an artifact and the chart: field is never read. That
		// looks like a redundant-scheme cleanup in a PR and is a source-type
		// change; keeping the order identical is what makes argocdf fail where
		// ArgoCD fails instead of quietly normalizing the prefix away (the
		// chart client trims oci:// because helm re-adds it, which used to make
		// both spellings render the same chart and report "no changes").
		artifactDir, digest, cleanupArtifact, err := fetchOCIArtifact(ctx, &r.opts, app, source, r.ociImagePaths())
		if err != nil {
			return nil, "", err
		}
		defer cleanupArtifact()
		appPath = filepath.Join(artifactDir, source.Path)
		if err := ValidatePathContainment(artifactDir, appPath); err != nil {
			return nil, "", fmt.Errorf("invalid source path %q: %w", source.Path, err)
		}
		repoRoot = artifactDir
		sourceRevision = digest
	case source.Chart != "":
		// Remote chart: ArgoCD's repo-server fetches charts BEFORE calling
		// GenerateManifests, so argocdf does the same — through ArgoCD's own
		// chart client, wrapped in the persistent chart cache.
		chartDir, chartVersion, cached, cleanupChart, err := fetchRemoteChart(ctx, &r.opts, app, source)
		if err != nil {
			return nil, "", err
		}
		defer cleanupChart()
		sourceRevision = chartVersion
		if cached {
			// GenerateManifests may build dependencies INTO appPath (charts/,
			// Chart.lock, its skip marker). The persistent cache must stay a
			// pristine shared artifact — and chartDepMutex is process-local,
			// so concurrent argocdf processes could otherwise corrupt each
			// other's cache entries — so cache-backed directories are copied
			// to a private temp dir first. Freshly extracted directories are
			// already private.
			privateDir, cleanupCopy, err := copyChartToTempDir(chartDir)
			if err != nil {
				return nil, "", err
			}
			defer cleanupCopy()
			chartDir = privateDir
		}
		appPath, repoRoot = chartDir, chartDir
	default:
		appPath = filepath.Join(repoPath, source.Path)
		if err := ValidatePathContainment(repoPath, appPath); err != nil {
			return nil, "", fmt.Errorf("invalid source path %q: %w", source.Path, err)
		}
		repoRoot = repoPath
		sourceRevision = revision
	}

	q, err := r.buildManifestRequest(ctx, app, source, refSources)
	if err != nil {
		return nil, "", err
	}

	// Serialize per appPath: GenerateManifests may write into appPath (helm
	// dependency build: charts/, Chart.lock, its skip marker) and restores
	// Chart.lock state after templating. Two apps sharing one chart directory
	// in the same worktree must not interleave those writes. The repo-server
	// never faces this (it locks per repo+revision a level above); argocdf's
	// parallel waves can.
	//
	// ArgoCD applies kustomize overrides (namePrefix, images, patches, ...) by
	// rewriting kustomization.yaml IN PLACE (`kustomize edit` plus a direct
	// write for patches — util/kustomize touches no other file). The
	// repo-server renders from managed checkouts so that mutation is invisible
	// there; argocdf renders every app from a per-side SHARED worktree, where
	// it would leak into later renders of the same path (helm's charts/ writes
	// are content-identical for apps pinning the same dependency versions —
	// same-path apps with DIVERGENT dependency pins would still interfere, an
	// accepted residual risk; kustomize edits are app-specific and are not
	// safe to share). Snapshot the kustomization file under the mutex and
	// restore it afterwards — even when the render fails or panics, since the
	// edit may have already happened. A restore failure is an error: a
	// poisoned worktree would silently corrupt every later render of the path.
	// Unlock and restore are deferred so a panic inside GenerateManifests
	// cannot leave the mutex held or the worktree poisoned.
	resp, err := func() (resp *apiclient.ManifestResponse, err error) {
		mu := chartDepMutex(appPath)
		mu.Lock()
		defer mu.Unlock()
		restoreKustomization, err := snapshotKustomization(appPath)
		if err != nil {
			return nil, err
		}
		defer func() { err = errors.Join(err, restoreKustomization()) }()
		return repository.GenerateManifests(
			ctx, appPath, repoRoot, sourceRevision, q,
			false, // isLocal=false gives the ISOLATED temp helm home (XDG_*, HELM_CONFIG_HOME)
			argogit.NoopCredsStore{},
			maxCombinedDirectoryManifestsSize,
			tempPaths,
		)
	}()
	if err != nil {
		// The one error here that reaches a REPORT carrying argv noise, and so
		// the one place both message transforms belong: util/kustomize returns
		// `kustomize build`'s failure verbatim, so a broken kustomization
		// reaches AppDiff.Error — and a PR comment — as ~17,000 characters of
		// --helm-api-versions around an absolute worktree path, with the
		// diagnosis at the very end. helm shows neither: ArgoCD strips its
		// API-version list from the returned error itself and runs it with a
		// WorkDir, so its argv says `helm template .`.
		return nil, "", reportableRenderError(err, repoRoot)
	}

	manifests, err := manifestsToYAML(resp.Manifests)
	if err != nil {
		return nil, "", err
	}
	return manifests, mapSourceType(resp.SourceType), nil
}

// apiVersionsFlags are the two spellings ArgoCD passes the cluster's API-version
// set under, and they are NOT interchangeable: `helm template` takes
// `--api-versions` (util/helm/cmd.go:437) while `kustomize build --enable-helm`
// takes `--helm-api-versions` (util/kustomize/kustomize.go:424, kustomize >=5.3).
//
// The two are textually DISJOINT — `--helm-api-versions` contains no
// `--api-versions` substring, since only ONE dash precedes `api-versions` there —
// which is both why upstream's helm-only remover never fired on a kustomize argv
// and why these patterns can run in any order without matching each other's
// output.
var apiVersionsFlags = []string{"--helm-api-versions", "--api-versions"}

// apiVersionsRuns matches a consecutive run of `<flag> <value>` pairs, one entry
// per spelling.
//
// Values are group/version[/Kind], so they contain neither whitespace nor a
// backtick: the run ends at the next unrelated argument (typically --include-crds)
// rather than eating it, and — because util/exec formats a failure as
// `<argv>` failed <cause> with the closing backtick flush against the last
// argument — it stops before that backtick when the run ENDS the argv, which it
// does whenever an app sets helm.skipCrds (nothing is appended after the pairs)
// and on every `kustomize build`, where the pairs are the LAST arguments
// unconditionally. With a bare \S+ the backtick was swallowed and the line then
// read as though the failure text were part of the quoted command.
var apiVersionsRuns = func() []*regexp.Regexp {
	runs := make([]*regexp.Regexp, len(apiVersionsFlags))
	for i, flag := range apiVersionsFlags {
		runs[i] = regexp.MustCompile("(?:" + regexp.QuoteMeta(flag) + " [^\\s`]+\\s*)+")
	}
	return runs
}()

// minElidedAPIVersions is where eliding starts paying: a one- or two-entry list
// is already readable, and replacing it would hide real information to save
// nothing. A real cluster contributes hundreds.
const minElidedAPIVersions = 3

// ElideAPIVersions replaces runs of `--api-versions <value>` (helm) and
// `--helm-api-versions <value>` (kustomize) in an ArgoCD message with their
// COUNT.
//
// ArgoCD passes one pair per group/version AND per kind the cluster advertises,
// so the argv it logs (and quotes back in every error) is ~16,000 characters of
// which the last ~180 are the actual failure. GitHub truncates a PR comment line,
// terminals wrap it into a screenful, and grep -o becomes useless — while the
// list itself is never the diagnosis, and is reproducible from the cluster (or
// removable with --no-api-versions).
//
// The count is kept rather than dropped because it is the one informative part:
// it says whether argocdf passed the cluster's API set at all. Each spelling
// keeps its OWN name in the replacement, so the elided line still says which tool
// ran.
//
// WHERE IT IS APPLIED DIFFERS PER TOOL, because upstream's own treatment does:
//
//   - helm — LOG RECORDS ONLY. ArgoCD strips the list from the error it RETURNS
//     itself (util/helm/cmd.go:449-455, leaving `<api versions removed>`), so an
//     error reaching AppDiff.Error and a PR comment is already short. The log
//     record is the one that escapes that, because util/exec logs the failure
//     INSIDE the helm wrapper, before it rewrites the message. Wrapping the
//     returned error there as well is the symmetry to refuse: it would be doing
//     upstream's work a second time.
//   - kustomize — BOTH. util/kustomize has no equivalent of apiVersionsRemover:
//     Build returns executil.Run's error verbatim (only the repo root is
//     redacted, and only in the returned command list), so the flood travels
//     intact into AppDiff.Error and straight into a PR comment. renderSource
//     therefore elides the RETURNED error too, via elideAPIVersionsErr.
//
// Applying it to the returned error is a no-op for helm exactly while upstream
// keeps stripping, so the one function can be used at both call sites without
// re-doing upstream's work.
func ElideAPIVersions(msg string) string {
	for i, run := range apiVersionsRuns {
		flag := apiVersionsFlags[i]
		msg = run.ReplaceAllStringFunc(msg, func(match string) string {
			n := strings.Count(match, flag+" ")
			if n < minElidedAPIVersions {
				return match
			}
			elided := fmt.Sprintf("%s <%d elided>", flag, n)
			// Keep the run's own trailing separator, so the next argument stays
			// separated (and a run at the very end gains no stray space).
			if trimmed := strings.TrimRight(match, " \t\n"); len(trimmed) < len(match) {
				return elided + match[len(trimmed):]
			}
			return elided
		})
	}
	return msg
}

// elidedAPIVersionsError carries a rewritten MESSAGE while Unwrap still reaches
// the original error, so errors.Is/As keep working on a message the report has
// rewritten.
//
// No caller tests a render error that way TODAY (argocdf's only errors.Is on a
// wrapped error is the klog handler's context.Canceled check in main.go), so this
// is a property kept rather than a bug fixed: a transform that rewrites a message
// must not quietly cost the error its identity, and this return path can carry a
// context error — GenerateManifests propagates a cancelled context out of the
// exec it wraps — which is the one a future caller would most likely ask about.
type elidedAPIVersionsError struct {
	err error
	msg string
}

func (e *elidedAPIVersionsError) Error() string { return e.msg }
func (e *elidedAPIVersionsError) Unwrap() error { return e.err }

// redactRenderRoot replaces the render's own root directory with "." in a
// message, which is what ArgoCD does to the kustomize command list it
// returns (util/kustomize/kustomize.go:409-411) — and ONLY there, never to the
// error.
//
// Package-private on purpose, unlike ElideAPIVersions: this is only correct
// where the root is KNOWN, which is the render. The log records main.go routes
// carry the same absolute path, but that hook is a process-global logrus sink
// with no per-render context to redact against — and a developer reading a log
// locally has reason to want the real worktree, which a report's reader never
// does.
//
// Every `kustomize build` failure quotes an argv whose first argument is the
// ABSOLUTE app path, and kustomize's own diagnoses name absolute paths too (a
// components cycle prints its root twice), so one broken kustomization put an
// ephemeral worktree path into a PR comment three times. That is argocdf
// internal state in a user-facing report: the directory is gone by the time
// anyone reads it, it differs between the two sides of the same diff, and it
// says nothing the application name does not.
//
// BOTH spellings of the root are redacted because the render and the tool can
// name the same directory differently: macOS resolves the temp root through a
// symlink (/var → /private/var), and kustomize reports the RESOLVED path while
// the argv carries the one argocdf passed. Redacting only the given form leaves
// the other in the report — which on a Linux CI runner looks fine and on a
// developer's machine does not.
//
// "." rather than a placeholder: it is upstream's own choice for the same
// substitution, and it leaves the message readable as a command someone could
// run from the source root.
func redactRenderRoot(msg, root string) string {
	if root == "" || root == "/" {
		return msg
	}
	roots := []string{root}
	if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != root && resolved != "" && resolved != "/" {
		roots = append(roots, resolved)
	}
	// LONGEST FIRST, because one spelling can CONTAIN the other: macOS resolves
	// /var/... to /private/var/..., so redacting the short form first rewrites
	// the long one's tail into "/private./app" and leaves a path the second pass
	// can no longer find. Sorting by length needs no assumption about which
	// direction a symlink points.
	slices.SortFunc(roots, func(a, b string) int { return len(b) - len(a) })
	for _, r := range roots {
		msg = strings.ReplaceAll(msg, r, ".")
	}
	return msg
}

// reportableRenderError rewrites a GenerateManifests failure for the REPORT it
// is about to become: the API-version flood collapses to its count and the
// render root is redacted. Both are message-only, and the error is returned
// UNWRAPPED when neither changed anything, so an untouched error keeps its own
// identity and formatting.
func reportableRenderError(err error, root string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	clean := redactRenderRoot(ElideAPIVersions(msg), root)
	if clean == msg {
		return err
	}
	return &elidedAPIVersionsError{err: err, msg: clean}
}

// IsRetriedMissingDependencyLog reports whether an ArgoCD log message is the
// EXPECTED first-attempt failure of an umbrella chart's `helm template`, the one
// GenerateManifests itself recovers from.
//
// ArgoCD uses a failed template run as its PROBE for "dependencies are not
// vendored yet": repository.GenerateManifests calls helm.Template, and on
// helm.IsMissingDependencyErr it runs `helm dependency build` and templates again
// (reposerver/repository/repository.go:1305-1311). The probe is control flow, but
// util/exec has already logged the non-zero exit at ERROR by the time the caller
// decides it was harmless (exec.go:270-272) — so every chart with a
// dependencies: section and no committed charts/ dir produces one ERRO per render
// on a completely healthy run.
//
// The predicate is deliberately ArgoCD's OWN: IsMissingDependencyErr is the
// function whose true-value triggers the retry, so this cannot drift from what
// upstream recovers from. Callers demote rather than drop, and nothing is lost if
// the retry then fails for the same reason: GenerateManifests returns that error,
// renderBranch wraps it, and processWave logs it at WARN naming the application
// plus records it as the app's report error — both of which name the app, which
// this anonymous library line never could.
//
// The `helm template` guard keeps a `helm dependency build` failure loud: that is
// the real failure this expected one is otherwise easy to confuse with.
func IsRetriedMissingDependencyLog(msg string) bool {
	if !strings.Contains(msg, "helm template") {
		return false
	}
	return argohelm.IsMissingDependencyErr(errors.New(msg))
}

// snapshotKustomization captures the kustomization file in dir, if one
// exists, and returns a restore func that writes the original bytes back
// (preserving mode). Kustomize accepts exactly one kustomization file per
// directory; the first match wins, mirroring ArgoCD's findKustomizeFile.
// Directories without one (helm charts, plain manifests) get a no-op restore.
func snapshotKustomization(dir string) (func() error, error) {
	for _, name := range KustomizationNames {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("failed to stat %s: %w", path, err)
		}
		if info.IsDir() {
			continue
		}
		original, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("failed to snapshot %s: %w", path, err)
		}
		restore := func() error {
			if err := os.WriteFile(path, original, info.Mode()); err != nil {
				return fmt.Errorf("failed to restore %s after render: %w", path, err)
			}
			return nil
		}
		return restore, nil
	}
	return func() error { return nil }, nil
}

// buildManifestRequest assembles the per-source ManifestRequest.
// GenerateManifests mutates q.ApplicationSource in memory (merging
// .argocd-source*.yaml overrides), so the source is deep-copied — requests
// must never share an ApplicationSource across goroutines.
func (r *ArgoCDRenderer) buildManifestRequest(
	ctx context.Context,
	app *cluster.Application,
	source *cluster.ApplicationSource,
	refSources map[string]*argoappv1.RefTarget,
) (*apiclient.ManifestRequest, error) {
	// Compose the repository lists per source, mirroring ArgoCD's controller
	// (controller/state.go:300-315): OCI repos and credential templates are
	// offered only for oci:// sources. Intentional parity, including the
	// degradations — an https chart with oci:// dependencies gets no OCI
	// creds, and a scheme-less helm-OCI source URL does not count as IsOCI —
	// exactly like stock ArgoCD.
	repos, repoCreds := r.opts.HelmRepos, r.opts.HelmRepoCreds
	if source.IsOCI() {
		repos = append(slices.Clone(repos), r.opts.OCIRepos...)
		repoCreds = append(slices.Clone(repoCreds), r.opts.OCIRepoCreds...)
	}

	// Repo must be non-nil (proxy/creds/env lookups dereference it). The
	// resolved repo carries creds/proxy/TLS from --repo-creds. Dependency
	// auth flows through Repos/HelmRepoCreds into ArgoCD's DependencyBuild:
	// classic repositories keep their credentials (authenticated `helm repo
	// add`), while OCI entries were stripped at construction — their auth
	// rides the HELM_REGISTRY_CONFIG file instead, so no `helm registry
	// login`/`logout` ever runs (see registryAuthFile).
	repo, err := r.resolveSourceRepo(ctx, app.Spec.Project, source.RepoURL)
	if err != nil {
		return nil, err
	}

	return &apiclient.ManifestRequest{
		Repo:          repo,
		Repos:         repos,
		HelmRepoCreds: repoCreds,
		// AppName is the application INSTANCE name — apps outside the
		// control-plane namespace qualify as "<namespace>_<name>" — exactly
		// what ArgoCD sends: it feeds ARGOCD_APP_NAME and the default helm
		// release name.
		AppName:            app.InstanceName(r.opts.ArgoCDNamespace),
		Namespace:          app.Spec.Destination.Namespace,
		ApplicationSource:  source.DeepCopy(),
		KubeVersion:        r.opts.KubeVersion, // parseKubeVersion handles vendor suffixes (-gke.*)
		ApiVersions:        r.opts.APIVersions,
		KustomizeOptions:   r.kustomizeOptions(),
		HelmOptions:        &argoappv1.HelmOptions{ValuesFileSchemes: DefaultValuesFileSchemes},
		RefSources:         refSources,
		HasMultipleSources: len(app.Spec.GetSources()) > 1,
		// ProjectName feeds the ARGOCD_APP_PROJECT_NAME build-env variable.
		ProjectName: app.Spec.Project,
		// AppLabelKey/TrackingMethod are intentionally left empty so no
		// tracking labels are injected — diffs stay comparable to plain
		// chart output.
		//
		// ProjectSourceRepos feeds only a permission-check that rewrites
		// dependency-fetch failures into "repo not permitted in project"
		// errors; argocdf has no AppProject context, so allow everything to
		// keep the real error visible.
		ProjectSourceRepos: []string{"*"},
	}, nil
}

// resolveSourceRepo returns the Repository configured for repoURL — with
// credentials, proxy, and TLS settings from the --repo-creds source — or a
// bare Repository when no credential source is configured. Resolution
// failures are errors (no silent anonymous fallback).
func (r *ArgoCDRenderer) resolveSourceRepo(ctx context.Context, project, repoURL string) (*argoappv1.Repository, error) {
	return resolveRepoOrBare(ctx, &r.opts, project, repoURL)
}

// kustomizeOptions maps argocdf's kustomize flags onto ArgoCD's KustomizeOptions
// build-options string.
func (r *ArgoCDRenderer) kustomizeOptions() *argoappv1.KustomizeOptions {
	var parts []string
	if r.opts.KustomizeBuildOptions != "" {
		parts = append(parts, r.opts.KustomizeBuildOptions)
	}
	if r.opts.KustomizeEnableHelm {
		parts = append(parts, "--enable-helm")
	}
	if r.opts.KustomizeLoadRestrictor != "" {
		parts = append(parts, "--load-restrictor="+r.opts.KustomizeLoadRestrictor)
	}
	if len(parts) == 0 {
		return nil
	}
	return &argoappv1.KustomizeOptions{BuildOptions: strings.Join(parts, " ")}
}

// prepareRefSources builds the RefSources map ("$name" keys, ArgoCD's format)
// and registers each ref repository's checkout in a TempPaths registry keyed by
// the normalized repo URL — exactly where getResolvedRefValueFile looks paths
// up. GenerateManifests never clones; every ref repo must be materialized here:
// the local repo maps to the current worktree, external repos are cloned to
// temp dirs (removed by cleanup).
//
// Note: ArgoCD resolves "$ref/some/path" against the ref repository ROOT
// (RefTarget has no Path field) — the ref source's own Path never participates.
// The render-cache key resolves $ref value files the same way (rendercache).
func (r *ArgoCDRenderer) prepareRefSources(
	ctx context.Context,
	project string,
	sources []cluster.ApplicationSource,
	repoPath string,
) (map[string]*argoappv1.RefTarget, utilio.TempPaths, func(), error) {
	refSources := make(map[string]*argoappv1.RefTarget)
	tempPaths := utilio.NewRandomizedTempPaths(os.TempDir())

	var tempDirs []string
	cleanup := func() {
		for _, dir := range tempDirs {
			_ = SafeRemoveAll(dir)
		}
	}

	for _, source := range sources {
		if source.Ref == "" {
			continue
		}
		// The resolved repo keeps parity with the request's Repo and carries
		// the credentials the external clone below authenticates with.
		refRepo, err := r.resolveSourceRepo(ctx, project, source.RepoURL)
		if err != nil {
			cleanup()
			return nil, nil, nil, err
		}
		refSources["$"+source.Ref] = &argoappv1.RefTarget{
			Repo:           *refRepo,
			TargetRevision: source.TargetRevision,
			Chart:          source.Chart,
		}

		// A ref source that is not a GIT repository has nothing to clone: an
		// oci:// URL names a registry reference and a chart: source names a chart
		// repository, and `git clone` fails on either — taking the whole
		// application's render down before its own branch above is ever reached.
		// Upstream never meets this because it materializes a ref repo LAZILY:
		// resolveReferencedSources loops over the VALUE FILES and only resolves
		// the refs a $ref/... entry actually names. Registering the RefTarget
		// without materializing it reproduces both of upstream's outcomes — an
		// unreferenced ref renders (the shape isPureRef's carve-out exists for),
		// and a referenced one fails inside GenerateManifests with `failed to
		// find repo` from getResolvedRefValueFile, where upstream refuses a chart
		// ref explicitly anyway ("Helm charts are not yet not supported for 'ref'
		// sources").
		if source.IsOCI() || source.Chart != "" {
			continue
		}

		key := argogit.NormalizeGitURL(source.RepoURL)
		if tempPaths.GetPathIfExists(key) != "" {
			continue // repo already materialized under this key
		}

		// A ref pointing at the repo being diffed resolves to the current
		// worktree, so PR edits to $values files actually produce a diff.
		if r.opts.RepoURL != "" && git.NormalizeRepoURL(source.RepoURL) == git.NormalizeRepoURL(r.opts.RepoURL) {
			tempPaths.Add(key, repoPath)
			continue
		}

		tempDir, err := os.MkdirTemp("", "argocdf-ref-")
		if err != nil {
			cleanup()
			return nil, nil, nil, fmt.Errorf("failed to create temp dir for ref %s: %w", source.Ref, err)
		}
		tempDirs = append(tempDirs, tempDir)
		if err := git.CloneWithCreds(source.RepoURL, source.TargetRevision, tempDir, cloneCredsFromRepo(refRepo)); err != nil {
			cleanup()
			return nil, nil, nil, fmt.Errorf("failed to clone ref source %s: %w", source.Ref, err)
		}
		tempPaths.Add(key, tempDir)
	}

	return refSources, tempPaths, cleanup, nil
}

// manifestsToYAML converts GenerateManifests' output (one JSON document string
// per manifest) into the multi-doc YAML stream the differ consumes.
func manifestsToYAML(manifests []string) ([]byte, error) {
	var buf bytes.Buffer
	for _, m := range manifests {
		y, err := yaml.JSONToYAML([]byte(m))
		if err != nil {
			return nil, fmt.Errorf("failed to convert manifest to YAML: %w", err)
		}
		if buf.Len() > 0 {
			buf.WriteString("---\n")
		}
		buf.Write(y)
	}
	return buf.Bytes(), nil
}

// mapSourceType maps ArgoCD's ApplicationSourceType strings onto argocdf's.
func mapSourceType(s string) types.SourceType {
	switch s {
	case string(argoappv1.ApplicationSourceTypeHelm):
		return types.SourceTypeHelm
	case string(argoappv1.ApplicationSourceTypeKustomize):
		return types.SourceTypeKustomize
	case string(argoappv1.ApplicationSourceTypeDirectory):
		return types.SourceTypePlain
	default:
		return types.SourceTypeUnknown
	}
}
