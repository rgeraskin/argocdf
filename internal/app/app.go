// Package app provides the main application orchestrator.
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/charmbracelet/log"

	"github.com/rgeraskin/argocdf/internal/cluster"
	"github.com/rgeraskin/argocdf/internal/config"
	"github.com/rgeraskin/argocdf/internal/diff"
	"github.com/rgeraskin/argocdf/internal/git"
	"github.com/rgeraskin/argocdf/internal/output"
	"github.com/rgeraskin/argocdf/internal/render"
	"github.com/rgeraskin/argocdf/internal/rendercache"
	"github.com/rgeraskin/argocdf/internal/types"
)

// ErrChangesPresent is returned by Run (only when Config.ExitCode is set) after
// output has been written, to signal that at least one application changed. main
// maps it to the detailed exit code 2, following the convention of `diff` and
// `terraform plan -detailed-exitcode`.
var ErrChangesPresent = errors.New("changes present")

// ExitCodeFor maps a Run result to a process exit code:
//
//	0 = success, no changes
//	1 = error
//	2 = changes present (Config.ExitCode enabled)
func ExitCodeFor(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrChangesPresent):
		return 2
	default:
		return 1
	}
}

// applicationRenderer is the part of render.Factory that App uses to render an
// application's manifests. It is a seam that lets tests substitute a fake
// renderer for the queue/wave orchestration in processApplications.
type applicationRenderer interface {
	RenderApplication(ctx context.Context, app *cluster.Application, repoPath string) (*render.RenderResult, error)
}

// App is the main application orchestrator.
type App struct {
	factory    *Factory
	cfg        *config.Config
	logger     *log.Logger
	kubeClient *cluster.Client
	appService *cluster.ApplicationService
	repo       *git.Repository
	renderer   applicationRenderer
	differ     *diff.ManifestDiffer
	discoverer *diff.AppDiscoverer
	writer     output.Writer
	// baseRef is the ref used for the base side of comparisons: the merge base
	// of the base and target branches, or the base branch tip as a fallback.
	baseRef string

	// Ephemeral worktree paths and their resolved commits. All renders run
	// against these fixed, committed trees instead of checking out branches in
	// the user's working tree. Populated by setupWorktrees.
	baseWorktree   string
	targetWorktree string
	baseCommit     string
	targetCommit   string

	// Render cache (nil when disabled for this run)
	cache       *rendercache.Cache
	kubeVersion string
	// cacheHits/cacheMisses are incremented from parallel render goroutines and
	// must be accessed atomically.
	cacheHits   atomic.Int64
	cacheMisses atomic.Int64
}

// New creates a new App with the given configuration.
func New(cfg *config.Config, logger *log.Logger) (*App, error) {
	factory := NewFactory(cfg, logger)

	return &App{
		factory: factory,
		cfg:     cfg,
		logger:  logger,
	}, nil
}

// Run executes the main application logic.
func (a *App) Run(ctx context.Context) error {
	// Initialize components
	if err := a.initialize(ctx); err != nil {
		return fmt.Errorf("initialization failed: %w", err)
	}

	// Fetch ArgoCD applications
	contextName := a.cfg.Context
	if contextName == "" {
		contextName = "(current)"
	}
	namespace := a.cfg.Namespace
	if a.cfg.AllNamespaces {
		namespace = "(all)"
	}
	a.logger.Info("Fetching ArgoCD applications...", "context", contextName, "namespace", namespace)
	apps, err := a.fetchApplications(ctx)
	if err != nil {
		return fmt.Errorf("failed to fetch applications: %w", err)
	}
	a.logger.Info("Found applications", "count", len(apps))

	// Get changed files from the merge base so commits made on the base branch
	// after the target branch diverged don't show up as phantom changes
	a.logger.Info("Analyzing git changes...")
	a.baseRef = a.resolveBaseRef()
	changedFiles, err := a.repo.GetDiff(a.baseRef, a.cfg.TargetBranch)
	if err != nil {
		return fmt.Errorf("failed to get changed files: %w", err)
	}

	a.logger.Info("Changed files", "count", len(changedFiles.AllPaths()))

	// Filter affected applications
	affectedApps := a.filterAffectedApps(apps, changedFiles)
	a.logger.Info("Affected applications", "count", len(affectedApps))

	if len(affectedApps) == 0 {
		a.logger.Info("No applications affected by changes")
		return nil
	}

	// Set up ephemeral worktrees for the base and target trees. All renders run
	// against these fixed paths, so we never checkout branches in (or otherwise
	// mutate) the user's working tree, and both sides can render in parallel.
	// The deferred cleanup removes the worktrees on normal exit and on
	// signal/context cancellation (which unwinds Run via render errors).
	cleanupWorktrees, err := a.setupWorktrees()
	defer cleanupWorktrees()
	if err != nil {
		return fmt.Errorf("failed to set up worktrees: %w", err)
	}

	// Process applications (with recursion for apps-of-apps)
	a.logger.Info("Processing applications...")
	appDiffs, err := a.processApplications(ctx, affectedApps)
	if err != nil {
		return fmt.Errorf("failed to process applications: %w", err)
	}

	// Build tree and output results
	tree := diff.NewAppTree(appDiffs)
	summary := output.ComputeSummary(appDiffs)

	// Write output
	if err := a.writeOutput(tree, summary); err != nil {
		return fmt.Errorf("failed to write output: %w", err)
	}

	// After output is fully flushed, signal "changes present" so the caller can
	// map it to a detailed exit code (used by CI). Errors don't reach here.
	if a.cfg.ExitCode && summary.AppsWithChanges > 0 {
		return ErrChangesPresent
	}

	return nil
}

// initialize sets up all required components.
func (a *App) initialize(ctx context.Context) error {
	var err error

	// Create cluster client
	a.logger.Debug("Connecting to Kubernetes cluster...")
	a.kubeClient, err = a.factory.CreateClusterClient()
	if err != nil {
		return fmt.Errorf("failed to create cluster client: %w", err)
	}

	// Get Kubernetes version for rendering
	kubeVersion := a.cfg.KubeVersion
	if kubeVersion == "" {
		kubeVersion, err = a.kubeClient.GetKubeVersion(ctx)
		if err != nil {
			a.logger.Warn("Failed to get cluster version, using default", "error", err)
			kubeVersion = config.DefaultKubeVersionFallback
		}
	}
	a.logger.Debug("Using Kubernetes version", "version", kubeVersion)
	a.kubeVersion = kubeVersion

	// Discover cluster API versions for helm's --api-versions (unless disabled).
	// Failure is non-fatal: warn and continue with whatever was discovered.
	var apiVersions []string
	if !a.cfg.NoAPIVersions {
		apiVersions, err = a.kubeClient.GetAPIVersions(ctx)
		if err != nil {
			a.logger.Warn("Failed to discover cluster API versions, continuing", "error", err)
		}
		a.logger.Debug("Discovered cluster API versions", "count", len(apiVersions))
	}

	// Create application service
	a.appService = a.factory.CreateAppService(a.kubeClient)

	// Open git repository
	a.logger.Debug("Opening git repository...")
	a.repo, err = a.factory.CreateRepository()
	if err != nil {
		return fmt.Errorf("failed to open repository: %w", err)
	}

	// Create render cache (may be nil when disabled via --no-cache).
	// Cache failures degrade to normal rendering.
	a.cache, err = a.factory.CreateRenderCache()
	if err != nil {
		a.logger.Warn("Failed to initialize render cache, continuing without it", "error", err)
		a.cache = nil
	}
	// Note: the cache no longer needs the dirty-working-tree bypass that used to
	// live here. Renders now always run from committed ephemeral worktrees (see
	// setupWorktrees), so the git tree hash always captures the rendered content
	// and cache entries are always valid.
	if a.cache != nil {
		a.logger.Debug("Render cache enabled", "dir", a.cache.Dir())
	}

	// Create renderer
	a.renderer = a.factory.CreateRenderFactory(kubeVersion, apiVersions)

	// Create differ and discoverer
	a.differ = a.factory.CreateManifestDiffer()
	a.discoverer = a.factory.CreateAppDiscoverer()

	// Create output writer
	a.writer, err = a.factory.CreateOutputWriter()
	if err != nil {
		return fmt.Errorf("failed to create output writer: %w", err)
	}

	return nil
}

// resolveBaseRef resolves the merge base of the base and target branches so
// change detection and base-side rendering both use PR-style diff semantics.
// Falls back to the base branch tip if the merge base cannot be resolved
// (e.g., unrelated histories).
//
// It prefers the remote-tracking ref origin/<base> over the local base branch
// when the local branch is stale (strictly behind origin/<base>), or when no
// local base branch exists. A stale local base makes upstream commits that
// landed on origin/<base> after the PR branch was cut appear as part of the PR
// diff. No network fetch is performed; only refs already present locally are
// consulted.
func (a *App) resolveBaseRef() string {
	effectiveBase := a.effectiveBaseBranch()

	mergeBase, err := a.repo.MergeBase(effectiveBase, a.cfg.TargetBranch)
	if err != nil {
		a.logger.Warn("Failed to resolve merge base, using base branch tip",
			"base", effectiveBase,
			"target", a.cfg.TargetBranch,
			"error", err)
		return effectiveBase
	}
	return mergeBase
}

// effectiveBaseBranch chooses between the local base branch and its
// remote-tracking ref origin/<base>. See resolveBaseRef for the rationale.
func (a *App) effectiveBaseBranch() string {
	base := a.cfg.BaseBranch

	// An explicitly remote base (e.g. "origin/main") is used verbatim; there is
	// no "origin/origin/main" to consult.
	if strings.HasPrefix(base, "origin/") {
		return base
	}

	remoteRef := "origin/" + base
	if !a.repo.RemoteRefExists(remoteRef) {
		return base
	}

	localHash, localErr := a.repo.CommitHash(base)
	if localErr != nil {
		// No local base branch (common in CI checkouts) but origin/<base>
		// exists: use the remote ref.
		a.logger.Debug("local base branch not found; using remote-tracking ref",
			"base", base, "remote", remoteRef)
		return remoteRef
	}

	remoteHash, remoteErr := a.repo.CommitHash(remoteRef)
	if remoteErr != nil || localHash == remoteHash {
		return base
	}

	// Local and remote differ. Prefer origin/<base> only when the local base is
	// strictly behind it (it's an ancestor); otherwise the local base is ahead or
	// diverged and we keep it.
	if a.repo.IsAncestor(base, remoteRef) {
		n, _ := a.repo.CountCommitsBetween(base, remoteRef)
		a.logger.Warn(fmt.Sprintf("local base branch is %d commit(s) behind %s; using %s",
			n, remoteRef, remoteRef),
			"base", base, "remote", remoteRef)
		return remoteRef
	}

	a.logger.Debug("local base branch is ahead of or diverged from remote; using local base",
		"base", base, "remote", remoteRef)
	return base
}

// setupWorktrees creates ephemeral detached worktrees for the base ref and the
// target branch tip and resolves their commit hashes (reused for cache keys).
// It always returns a non-nil cleanup function so callers can defer it
// unconditionally, even on error (partial worktrees are cleaned up).
//
// Behavior note: the target side renders the COMMITTED target branch tip, not
// the user's (possibly dirty) working tree. warnIfWorkingTreeDirty surfaces
// this to the user.
func (a *App) setupWorktrees() (func(), error) {
	var cleanups []func()
	cleanupAll := func() {
		// Remove in reverse creation order.
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}

	a.warnIfWorkingTreeDirty()

	basePath, baseCleanup, err := a.repo.AddWorktree(a.baseRef)
	if err != nil {
		return cleanupAll, fmt.Errorf("failed to create base worktree at %s: %w", a.baseRef, err)
	}
	cleanups = append(cleanups, baseCleanup)
	a.baseWorktree = basePath

	targetPath, targetCleanup, err := a.repo.AddWorktree(a.cfg.TargetBranch)
	if err != nil {
		return cleanupAll, fmt.Errorf("failed to create target worktree at %s: %w", a.cfg.TargetBranch, err)
	}
	cleanups = append(cleanups, targetCleanup)
	a.targetWorktree = targetPath

	// Resolve commits once from the main repo's object database (no checkout);
	// renderCacheKey reuses these for every app.
	a.baseCommit, err = a.repo.CommitHash(a.baseRef)
	if err != nil {
		return cleanupAll, fmt.Errorf("failed to resolve base commit: %w", err)
	}
	a.targetCommit, err = a.repo.CommitHash(a.cfg.TargetBranch)
	if err != nil {
		return cleanupAll, fmt.Errorf("failed to resolve target commit: %w", err)
	}

	a.logger.Debug("Created ephemeral worktrees",
		"base", a.baseWorktree, "baseCommit", a.baseCommit,
		"target", a.targetWorktree, "targetCommit", a.targetCommit)

	return cleanupAll, nil
}

// warnIfWorkingTreeDirty logs a one-time warning when the user's working tree
// has uncommitted changes. Since rendering now runs from the committed target
// tip, those changes are not reflected in the diff.
func (a *App) warnIfWorkingTreeDirty() {
	status, err := git.RunGitCommand(a.cfg.RepoPath, "status", "--porcelain")
	if err != nil {
		a.logger.Debug("Could not determine working tree status", "error", err)
		return
	}
	if strings.TrimSpace(status) != "" {
		a.logger.Warn("Uncommitted changes are not included in the diff; the target side renders the committed tip",
			"target", a.cfg.TargetBranch)
	}
}

// fetchApplications retrieves ArgoCD applications from the cluster.
func (a *App) fetchApplications(ctx context.Context) ([]cluster.Application, error) {
	if a.cfg.AllNamespaces {
		return a.appService.ListAllNamespaces(ctx)
	}
	return a.appService.List(ctx, a.cfg.Namespace)
}

// filterAffectedApps filters applications that are affected by the changed files.
func (a *App) filterAffectedApps(apps []cluster.Application, changed *git.ChangedFiles) []cluster.Application {
	repoURL := git.NormalizeRepoURL(a.cfg.RepoURL)
	a.logger.Debug("Filtering apps", "localRepoURL", repoURL, "changedFiles", changed.AllPaths())
	var affected []cluster.Application

	changedPaths := changed.AllPaths()

	for _, app := range apps {
		sources := app.Spec.GetSources()

		// Build a lookup of ref name -> ref source so we can resolve
		// $<ref>/... value file references (which may be declared on a
		// different source than the ref source itself).
		refSources := make(map[string]cluster.ApplicationSource)
		for _, source := range sources {
			if source.Ref != "" {
				refSources[source.Ref] = source
			}
		}

		for _, source := range sources {
			// A helm source may reference value files in another (ref) source
			// via $<ref>/path. This is independent of this source's own repo
			// URL (the helm chart often lives in a different repo).
			if a.helmValueFilesAffected(source, refSources, repoURL, changedPaths) {
				a.logger.Debug("App affected via ref value file", "app", app.Name)
				affected = append(affected, app)
				break
			}

			normalizedSourceURL := git.NormalizeRepoURL(source.RepoURL)
			// Check if this source uses our repo
			if normalizedSourceURL != repoURL {
				a.logger.Debug("Skipping source - repo URL mismatch",
					"app", app.Name,
					"appRepoURL", normalizedSourceURL,
					"localRepoURL", repoURL)
				continue
			}

			// Check if the source path has changes
			if source.Path != "" && changed.HasChangesInPath(source.Path) {
				a.logger.Debug("App affected",
					"app", app.Name,
					"path", source.Path)
				affected = append(affected, app)
				break
			} else {
				a.logger.Debug("Skipping source - no changes in path",
					"app", app.Name,
					"path", source.Path)
			}
		}
	}

	return affected
}

// helmValueFilesAffected reports whether any of a helm source's value files
// reference a $<ref>/... path in the local repo that was changed. It resolves
// the ref name against the app's ref sources, and only matches when the ref
// source points at the local repo being diffed.
func (a *App) helmValueFilesAffected(
	source cluster.ApplicationSource,
	refSources map[string]cluster.ApplicationSource,
	repoURL string,
	changedPaths []string,
) bool {
	if source.Helm == nil {
		return false
	}

	for _, vf := range source.Helm.ValueFiles {
		if !strings.HasPrefix(vf, "$") {
			continue
		}

		// Split "$values/env/prod.yaml" into ref name ("values") and the
		// remaining path within the ref source ("env/prod.yaml").
		refName, remainder, ok := strings.Cut(strings.TrimPrefix(vf, "$"), "/")
		if !ok {
			continue
		}

		refSource, ok := refSources[refName]
		if !ok {
			continue
		}

		// Only local-repo ref sources map to changed files in this repo.
		if git.NormalizeRepoURL(refSource.RepoURL) != repoURL {
			continue
		}

		// Repo-relative path of the referenced value file.
		relPath := path.Clean(path.Join(refSource.Path, remainder))
		for _, cp := range changedPaths {
			if path.Clean(cp) == relPath {
				return true
			}
		}
	}

	return false
}

// processApplications processes all affected applications with recursion.
func (a *App) processApplications(ctx context.Context, apps []cluster.Application) ([]*types.AppDiff, error) {
	results := make(map[string]*types.AppDiff)
	queue := a.factory.CreateAppQueue()

	// Add initial apps to queue
	for _, app := range apps {
		queue.Add(diff.QueuedApp{
			Name:      app.Name,
			Namespace: app.Namespace,
			Depth:     0,
			Spec:      &app.Spec,
		})
	}

	// Process the queue in waves. Each wave drains the current pending batch and
	// renders those apps concurrently with a bounded worker pool. The queue
	// itself stays single-threaded: after a wave completes we run the
	// (sequential) child-discovery logic per app, which may enqueue the next
	// wave. This preserves all existing requeue/dedup semantics without locking
	// the queue.
	for !queue.IsEmpty() {
		// Drain the current pending batch into a wave.
		var wave []*diff.QueuedApp
		for {
			qa := queue.Next()
			if qa == nil {
				break
			}
			wave = append(wave, qa)
		}

		// Render the wave concurrently; results are index-aligned with wave.
		waveDiffs := a.processWave(ctx, wave)

		// Sequentially collect results and run child discovery, which may
		// enqueue the next wave.
		for i, queuedApp := range wave {
			appDiff := waveDiffs[i]
			key := fmt.Sprintf("%s/%s", appDiff.Namespace, appDiff.Name)
			results[key] = appDiff

			// Look for new and modified Application CRDs in the diff (apps-of-apps pattern)
			diffResult, _ := appDiff.DiffResult.(*diff.ManifestSetDiff)
			if a.cfg.NoRecursive || appDiff.Error != nil || diffResult == nil || !diffResult.HasChanges {
				continue
			}

			// Find newly added child applications
			newApps, err := a.discoverer.FindNewApplications(appDiff.RenderedOld, appDiff.RenderedNew)
			if err != nil {
				a.logger.Warn("Error discovering new child apps", "parent", queuedApp.Name, "error", err)
			} else {
				for _, newApp := range newApps {
					added := queue.Add(diff.QueuedApp{
						Name:            newApp.Name,
						Namespace:       newApp.Namespace,
						Depth:           queuedApp.Depth + 1,
						ParentApp:       queuedApp.Name,
						ParentNamespace: queuedApp.Namespace,
						Spec:            &newApp.Spec,
					})
					if added {
						a.logger.Debug("Discovered new child application", "parent", queuedApp.Name, "child", newApp.Name)
						appDiff.ChildAppNames = append(appDiff.ChildAppNames, newApp.Name)
					}
				}
			}

			// Find modified child applications (specs changed between branches)
			modifiedApps, err := a.discoverer.FindModifiedApplications(appDiff.RenderedOld, appDiff.RenderedNew)
			if err != nil {
				a.logger.Warn("Error discovering modified child apps", "parent", queuedApp.Name, "error", err)
			} else {
				for _, modApp := range modifiedApps {
					childApp := diff.QueuedApp{
						Name:            modApp.Name,
						Namespace:       modApp.Namespace,
						Depth:           queuedApp.Depth + 1,
						ParentApp:       queuedApp.Name,
						ParentNamespace: queuedApp.Namespace,
						Spec:            &modApp.NewSpec,
						OldSpec:         &modApp.OldSpec,
					}

					// Case 1: App is still pending - update its spec
					if queue.UpdatePending(childApp) {
						a.logger.Debug("Updated pending child application with git spec",
							"parent", queuedApp.Name, "child", modApp.Name)
						appDiff.ChildAppNames = append(appDiff.ChildAppNames, modApp.Name)
						continue
					}

					// Case 2: App not in queue at all - add it (pure child discovery)
					if queue.Add(childApp) {
						a.logger.Debug("Discovered modified child application",
							"parent", queuedApp.Name, "child", modApp.Name)
						appDiff.ChildAppNames = append(appDiff.ChildAppNames, modApp.Name)
						continue
					}

					// Case 3: App was already processed - requeue for re-processing
					if queue.RequeueProcessed(childApp) {
						a.logger.Info("Re-queuing already-processed child with git spec",
							"parent", queuedApp.Name, "child", modApp.Name)
						appDiff.ChildAppNames = append(appDiff.ChildAppNames, modApp.Name)
					}
				}
			}
		}
	}

	// Convert map to slice
	var resultSlice []*types.AppDiff
	for _, r := range results {
		resultSlice = append(resultSlice, r)
	}

	if a.cache != nil {
		a.logger.Info("Render cache", "hits", a.cacheHits.Load(), "misses", a.cacheMisses.Load())
	}

	return resultSlice, nil
}

// processWave renders a batch of queued apps concurrently using a bounded
// worker pool (a.cfg.Concurrency). It returns a slice of AppDiffs index-aligned
// with wave; render errors are captured as AppDiff.Error rather than aborting
// the wave. Each goroutine writes to a distinct output index, and the shared
// cache counters are atomic, so no additional locking is required.
func (a *App) processWave(ctx context.Context, wave []*diff.QueuedApp) []*types.AppDiff {
	out := make([]*types.AppDiff, len(wave))

	concurrency := a.cfg.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > len(wave) {
		concurrency = len(wave)
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	for i := range wave {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()

			queuedApp := wave[i]
			a.logger.Info("Processing application", "name", queuedApp.Name, "depth", queuedApp.Depth)

			appDiff, err := a.processOneApp(ctx, queuedApp)
			if err != nil {
				a.logger.Warn("Error processing application", "name", queuedApp.Name, "error", err)
				appDiff = &types.AppDiff{
					Name:               queuedApp.Name,
					Namespace:          queuedApp.Namespace,
					ParentAppName:      queuedApp.ParentApp,
					ParentAppNamespace: queuedApp.ParentNamespace,
					Error:              err,
				}
			}
			out[i] = appDiff
		}(i)
	}
	wg.Wait()

	return out
}

// processOneApp processes a single application and returns its diff.
func (a *App) processOneApp(ctx context.Context, queuedApp *diff.QueuedApp) (*types.AppDiff, error) {
	appDiff := &types.AppDiff{
		Name:               queuedApp.Name,
		Namespace:          queuedApp.Namespace,
		ParentAppName:      queuedApp.ParentApp,
		ParentAppNamespace: queuedApp.ParentNamespace,
	}

	// Build Application objects for rendering
	// For modified child apps, OldSpec differs from Spec
	oldSpec := queuedApp.Spec
	if queuedApp.OldSpec != nil {
		oldSpec = queuedApp.OldSpec
	}

	appOld := &cluster.Application{
		Spec: *oldSpec,
	}
	appOld.Name = queuedApp.Name
	appOld.Namespace = queuedApp.Namespace

	appNew := &cluster.Application{
		Spec: *queuedApp.Spec,
	}
	appNew.Name = queuedApp.Name
	appNew.Namespace = queuedApp.Namespace

	// Render from the base worktree (merge base) using old spec, so the base
	// side matches the merge-base semantics used for change detection.
	renderedOld, sourceTypeOld, err := a.renderBranch(ctx, appOld, a.baseWorktree, a.baseCommit, a.baseRef, "new app")
	if err != nil {
		return nil, fmt.Errorf("failed to render base branch: %w", err)
	}
	appDiff.SourceType = sourceTypeOld

	// Render from the target worktree (committed target tip) using new spec.
	renderedNew, sourceTypeNew, err := a.renderBranch(ctx, appNew, a.targetWorktree, a.targetCommit, a.cfg.TargetBranch, "deleted app")
	if err != nil {
		return nil, fmt.Errorf("failed to render target branch: %w", err)
	}
	if appDiff.SourceType == "" {
		appDiff.SourceType = sourceTypeNew
	}

	appDiff.RenderedOld = string(renderedOld)
	appDiff.RenderedNew = string(renderedNew)

	// Compute diff
	diffResult, err := a.differ.DiffManifests(appDiff.RenderedOld, appDiff.RenderedNew)
	if err != nil {
		return nil, fmt.Errorf("failed to compute diff: %w", err)
	}
	appDiff.DiffResult = diffResult

	return appDiff, nil
}

// renderBranch renders an application from the given ephemeral worktree,
// consulting the persistent render cache first. On a cache hit it returns the
// cached manifests and SKIPS rendering entirely (the main speedup). On a miss
// it renders from the worktree path and stores the result.
//
// worktreePath is the fixed checkout to render from; commit is its resolved
// hash (used for the cache key). label is a human-readable branch/ref name for
// logging. missingKind describes how a missing source path is interpreted
// (e.g. "new app" on the base side, "deleted app" on the target side).
func (a *App) renderBranch(
	ctx context.Context,
	app *cluster.Application,
	worktreePath, commit, label, missingKind string,
) ([]byte, types.SourceType, error) {
	// Compute the cache key from the precomputed commit (git rev-parse reads the
	// object database directly). An empty key means "bypass the cache".
	key, haveKey := a.renderCacheKey(app, commit)

	if a.cache != nil && haveKey {
		if entry, ok := a.cache.Get(key); ok {
			a.cacheHits.Add(1)
			a.logger.Debug("Render cache hit", "app", app.Name, "branch", label)
			return entry.Manifests, types.SourceType(entry.SourceType), nil
		}
		a.cacheMisses.Add(1)
		a.logger.Debug("Render cache miss", "app", app.Name, "branch", label)
	}

	var (
		manifests  []byte
		sourceType types.SourceType
	)
	if !a.sourcePathsExist(app, worktreePath) {
		a.logger.Debug("Source path does not exist, treating as "+missingKind,
			"app", app.Name, "branch", label)
	} else {
		result, rerr := a.renderer.RenderApplication(ctx, app, worktreePath)
		if rerr != nil {
			return nil, "", rerr
		}
		manifests = result.Manifests
		sourceType = result.SourceType
	}

	// Store on a real render only. When haveKey is false the source path was
	// unresolvable (missing on this commit), which is exactly the empty
	// new/deleted-app render we must not cache.
	if a.cache != nil && haveKey {
		if perr := a.cache.Put(key, &rendercache.Entry{
			Manifests:  manifests,
			SourceType: string(sourceType),
		}); perr != nil {
			a.logger.Warn("Failed to write render cache entry",
				"app", app.Name, "branch", label, "error", perr)
		}
	}

	return manifests, sourceType, nil
}

// renderCacheKey computes the cache key for rendering app at the given commit.
// It returns ok=false whenever caching should be bypassed for this render
// (cache disabled or a local source tree hash unavailable).
func (a *App) renderCacheKey(app *cluster.Application, commit string) (string, bool) {
	if a.cache == nil {
		return "", false
	}

	localRepoURL := git.NormalizeRepoURL(a.cfg.RepoURL)

	return rendercache.ComputeKey(rendercache.KeyInput{
		AppName:     app.Name,
		Namespace:   app.Namespace,
		Spec:        &app.Spec,
		KubeVersion: a.kubeVersion,
		Options: rendercache.KeyOptions{
			KustomizeEnableHelm:     a.cfg.KustomizeEnableHelm,
			KustomizeBuildOptions:   a.cfg.KustomizeBuildOptions,
			KustomizeLoadRestrictor: a.cfg.KustomizeLoadRestrictor,
			HelmSkipRefresh:         a.cfg.HelmSkipRefresh,
		},
		Commit: commit,
		ResolveTree: func(commit, path string) (string, bool) {
			h, terr := a.repo.TreeHash(commit, path)
			if terr != nil {
				return "", false
			}
			return h, true
		},
		// A ref source is resolvable to local content only when it points at
		// the repository being diffed; external-repo refs force a cache bypass.
		SameRepo: func(repoURL string) bool {
			return git.NormalizeRepoURL(repoURL) == localRepoURL
		},
	})
}

// sourcePathsExist checks if all local source paths for an application exist on disk.
// Remote chart sources (with Chart field set) are skipped since they don't need a local path.
func (a *App) sourcePathsExist(app *cluster.Application, repoPath string) bool {
	for _, source := range app.Spec.GetSources() {
		// Remote charts don't need a local path
		if source.Chart != "" {
			continue
		}
		if source.Path == "" {
			continue
		}
		fullPath := filepath.Join(repoPath, source.Path)
		if _, err := os.Stat(fullPath); os.IsNotExist(err) {
			return false
		}
	}
	return true
}

// writeOutput writes the results to the configured output.
func (a *App) writeOutput(tree *diff.AppTree, summary output.Summary) error {
	title := fmt.Sprintf("ArgoCD Diff: %s → %s", a.cfg.BaseBranch, a.cfg.TargetBranch)

	if err := a.writer.WriteHeader(title); err != nil {
		return err
	}

	if err := a.writer.WriteTree(tree); err != nil {
		return err
	}

	if err := a.writer.WriteSummary(summary); err != nil {
		return err
	}

	if err := a.writer.WriteFooter(); err != nil {
		return err
	}

	return a.writer.Flush()
}
