// Package app provides the main application orchestrator.
package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

// App is the main application orchestrator.
type App struct {
	factory    *Factory
	cfg        *config.Config
	logger     *log.Logger
	kubeClient *cluster.Client
	appService *cluster.ApplicationService
	repo       *git.Repository
	renderer   *render.Factory
	differ     *diff.ManifestDiffer
	discoverer *diff.AppDiscoverer
	writer     output.Writer

	// Render cache (nil when disabled or bypassed for this run)
	cache       *rendercache.Cache
	kubeVersion string
	cacheHits   int
	cacheMisses int
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
	a.logger.Info("Fetching ArgoCD applications...")
	apps, err := a.fetchApplications(ctx)
	if err != nil {
		return fmt.Errorf("failed to fetch applications: %w", err)
	}
	a.logger.Info("Found applications", "count", len(apps))

	// Get changed files
	a.logger.Info("Analyzing git changes...")
	changedFiles, err := a.repo.GetDiff(a.cfg.BaseBranch, a.cfg.TargetBranch)
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
	// Bypass the cache entirely when the working tree is dirty: rendering runs
	// from a working-tree checkout that may include uncommitted changes the git
	// tree hash does not capture.
	if a.cache != nil {
		status, serr := git.RunGitCommand(a.cfg.RepoPath, "status", "--porcelain")
		switch {
		case serr != nil:
			a.logger.Debug("Could not determine working tree status; disabling render cache", "error", serr)
			a.cache = nil
		case strings.TrimSpace(status) != "":
			a.logger.Debug("Working tree is dirty; disabling render cache for this run")
			a.cache = nil
		default:
			a.logger.Debug("Render cache enabled", "dir", a.cache.Dir())
		}
	}

	// Create renderer
	a.renderer = a.factory.CreateRenderFactory(kubeVersion)

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

	for _, app := range apps {
		sources := app.Spec.GetSources()
		for _, source := range sources {
			normalizedSourceURL := git.NormalizeRepoURL(source.RepoURL)
			// Check if this source uses our repo
			if normalizedSourceURL != repoURL {
				a.logger.Debug("Skipping app - repo URL mismatch",
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
				a.logger.Debug("Skipping app - no changes in path",
					"app", app.Name,
					"path", source.Path)
			}
		}
	}

	return affected
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

	// Process queue
	for !queue.IsEmpty() {
		queuedApp := queue.Next()
		if queuedApp == nil {
			break
		}

		a.logger.Info("Processing application", "name", queuedApp.Name, "depth", queuedApp.Depth)

		appDiff, err := a.processOneApp(ctx, queuedApp)
		if err != nil {
			a.logger.Warn("Error processing application", "name", queuedApp.Name, "error", err)
			appDiff = &types.AppDiff{
				Name:          queuedApp.Name,
				Namespace:     queuedApp.Namespace,
				ParentAppName: queuedApp.ParentApp,
				Error:         err,
			}
		}
		key := fmt.Sprintf("%s/%s", appDiff.Namespace, appDiff.Name)
		results[key] = appDiff

		// Look for new and modified Application CRDs in the diff (apps-of-apps pattern)
		diffResult, _ := appDiff.DiffResult.(*diff.ManifestSetDiff)
		if !a.cfg.NoRecursive && appDiff.Error == nil && diffResult != nil && diffResult.HasChanges {
			// Find newly added child applications
			newApps, err := a.discoverer.FindNewApplications(appDiff.RenderedOld, appDiff.RenderedNew)
			if err != nil {
				a.logger.Warn("Error discovering new child apps", "parent", queuedApp.Name, "error", err)
			} else {
				for _, newApp := range newApps {
					added := queue.Add(diff.QueuedApp{
						Name:      newApp.Name,
						Namespace: newApp.Namespace,
						Depth:     queuedApp.Depth + 1,
						ParentApp: queuedApp.Name,
						Spec:      &newApp.Spec,
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
						Name:      modApp.Name,
						Namespace: modApp.Namespace,
						Depth:     queuedApp.Depth + 1,
						ParentApp: queuedApp.Name,
						Spec:      &modApp.NewSpec,
						OldSpec:   &modApp.OldSpec,
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
		a.logger.Info("Render cache", "hits", a.cacheHits, "misses", a.cacheMisses)
	}

	return resultSlice, nil
}

// processOneApp processes a single application and returns its diff.
func (a *App) processOneApp(ctx context.Context, queuedApp *diff.QueuedApp) (*types.AppDiff, error) {
	appDiff := &types.AppDiff{
		Name:          queuedApp.Name,
		Namespace:     queuedApp.Namespace,
		ParentAppName: queuedApp.ParentApp,
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

	// Render from base branch using old spec
	renderedOld, sourceTypeOld, err := a.renderBranch(ctx, appOld, a.cfg.BaseBranch, "new app")
	if err != nil {
		return nil, fmt.Errorf("failed to render base branch: %w", err)
	}
	appDiff.SourceType = sourceTypeOld

	// Render from target branch using new spec
	renderedNew, sourceTypeNew, err := a.renderBranch(ctx, appNew, a.cfg.TargetBranch, "deleted app")
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

// renderBranch renders an application from the given branch, consulting the
// persistent render cache first. On a cache hit it returns the cached manifests
// and SKIPS the branch checkout entirely (the main speedup). On a miss it
// checks out the branch, renders, and stores the result.
//
// missingKind describes how a missing source path is interpreted for logging
// (e.g. "new app" on the base branch, "deleted app" on the target branch).
func (a *App) renderBranch(
	ctx context.Context,
	app *cluster.Application,
	branch, missingKind string,
) ([]byte, types.SourceType, error) {
	// Compute the cache key without checking out (git rev-parse reads the object
	// database directly). An empty key means "bypass the cache for this render".
	key, haveKey := a.renderCacheKey(app, branch)

	if a.cache != nil && haveKey {
		if entry, ok := a.cache.Get(key); ok {
			a.cacheHits++
			a.logger.Debug("Render cache hit", "app", app.Name, "branch", branch)
			return entry.Manifests, types.SourceType(entry.SourceType), nil
		}
		a.cacheMisses++
		a.logger.Debug("Render cache miss", "app", app.Name, "branch", branch)
	}

	var (
		manifests  []byte
		sourceType types.SourceType
	)
	err := a.repo.WithBranch(branch, func() error {
		if !a.sourcePathsExist(app, a.repo.Path()) {
			a.logger.Debug("Source path does not exist, treating as "+missingKind,
				"app", app.Name, "branch", branch)
			return nil
		}
		result, rerr := a.renderer.RenderApplication(ctx, app, a.repo.Path())
		if rerr != nil {
			return rerr
		}
		manifests = result.Manifests
		sourceType = result.SourceType
		return nil
	})
	if err != nil {
		return nil, "", err
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
				"app", app.Name, "branch", branch, "error", perr)
		}
	}

	return manifests, sourceType, nil
}

// renderCacheKey computes the cache key for rendering app at branch. It returns
// ok=false whenever caching should be bypassed for this render (cache disabled,
// commit unresolvable, or a local source tree hash unavailable).
func (a *App) renderCacheKey(app *cluster.Application, branch string) (string, bool) {
	if a.cache == nil {
		return "", false
	}

	commit, err := a.repo.CommitHash(branch)
	if err != nil {
		a.logger.Debug("Cannot resolve commit for cache key", "branch", branch, "error", err)
		return "", false
	}

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
