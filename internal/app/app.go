// Package app provides the main application orchestrator.
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/log"

	"github.com/rgeraskin/argocdf/internal/cluster"
	"github.com/rgeraskin/argocdf/internal/config"
	"github.com/rgeraskin/argocdf/internal/diff"
	"github.com/rgeraskin/argocdf/internal/git"
	"github.com/rgeraskin/argocdf/internal/helmconfig"
	"github.com/rgeraskin/argocdf/internal/lint"
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

// applicationRenderer is the part of the render engine that App uses to render
// an application's manifests. It is a seam that lets tests substitute a fake
// renderer for the queue/wave orchestration in processApplications, and lets
// the factory swap engines (--renderer=native|argocd). revision is the commit
// being rendered; the argocd engine feeds it into the ARGOCD_APP_REVISION*
// build-env variables, the native engine ignores it.
type applicationRenderer interface {
	RenderApplication(ctx context.Context, app *cluster.Application, repoPath, revision string) (*render.RenderResult, error)
}

// applicationLister is the slice of cluster.ApplicationService that App uses
// to fetch Applications from the cluster — a seam so tests can pin the
// namespace-routing behavior of fetchApplications without a live cluster.
type applicationLister interface {
	List(ctx context.Context, namespace string) ([]cluster.Application, error)
	ListNamespaces(ctx context.Context, namespaces []string) ([]cluster.Application, error)
	ListAllNamespaces(ctx context.Context) ([]cluster.Application, error)
}

// App is the main application orchestrator.
type App struct {
	factory    *Factory
	cfg        *config.Config
	logger     *log.Logger
	kubeClient *cluster.Client
	appService applicationLister
	repo       *git.Repository
	renderer   applicationRenderer
	differ     *diff.ManifestDiffer
	discoverer *diff.AppDiscoverer
	// linter runs --lint commands against each side's rendered manifests
	// (nil when linting is disabled).
	linter *lint.Runner
	writer output.Writer
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
	// The argocd engine keeps a per-run registry auth file with short-lived
	// tokens and mutates the process helm env; undo both on the way out. The
	// defer is registered BEFORE initialize and resolves a.renderer at unwind
	// time, so an initialize failure AFTER renderer construction (e.g. output
	// writer creation) still cleans up.
	defer func() {
		if c, ok := a.renderer.(interface{ Cleanup() }); ok {
			c.Cleanup()
		}
	}()

	// Initialize components
	if err := a.initialize(ctx); err != nil {
		return fmt.Errorf("initialization failed: %w", err)
	}

	// Fetch ArgoCD applications
	contextName := a.cfg.Context
	if contextName == "" {
		contextName = "(current)"
	}
	namespaces := a.cfg.ArgoCDNamespace
	if len(a.cfg.ApplicationNamespaces) > 0 {
		namespaces = strings.Join(a.cfg.ApplicationNamespaces, ",")
	}
	a.logger.Info("Fetching ArgoCD applications...", "context", contextName, "namespaces", namespaces)
	apps, err := a.fetchApplications(ctx)
	if err != nil {
		return fmt.Errorf("failed to fetch applications: %w", err)
	}
	a.logger.Info("Found applications", "count", len(apps))

	// Get changed files from the merge base so commits made on the base branch
	// after the target branch diverged don't show up as phantom changes
	a.logger.Info("Analyzing git changes...")
	a.baseRef = a.resolveBaseRef()

	// Resolve the compared commits once, straight from the object database (no
	// worktree needed). They feed the render cache keys and the provenance
	// footer — which must carry the SHAs even on the no-apps-affected path
	// below, where a durable (PR-comment) report is still written.
	a.baseCommit, err = a.repo.CommitHash(a.baseRef)
	if err != nil {
		return fmt.Errorf("failed to resolve base commit: %w", err)
	}
	a.targetCommit, err = a.repo.CommitHash(a.cfg.TargetBranch)
	if err != nil {
		return fmt.Errorf("failed to resolve target commit: %w", err)
	}

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
		// "No applications affected" is a result, not an absence of one. Emit it
		// (empty tree, zero summary) to the file writers only, so each produces a
		// self-describing report instead of a 0-byte file indistinguishable from a
		// crash, and the markdown writer keeps its upsert marker so CI can overwrite
		// a stale PR comment. The terminal stays quiet: the INFO log above already
		// told the user. There is nothing to render, so worktree setup and
		// processing are skipped entirely.
		if err := a.writeOutput(output.FileOnly(a.writer), diff.NewAppTree(nil), output.ComputeSummary(nil)); err != nil {
			return fmt.Errorf("failed to write output: %w", err)
		}
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
	if err := a.writeOutput(a.writer, tree, summary); err != nil {
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
	kubeVersion, kvErr := resolveKubeVersion(a.cfg.KubeVersion, func() (string, error) {
		return a.kubeClient.GetKubeVersion(ctx)
	})
	if kvErr != nil {
		a.logger.Warn("Failed to get cluster version, using default", "error", kvErr)
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

	// Load repository credentials per --repo-creds. Load failures are fatal
	// in cluster and local modes — no silent degradation; the message names
	// the escape hatches. `none` renders anonymously. A cluster (or helm
	// config) WITHOUT credentials is not a failure: empty lists render
	// credential-less.
	var creds *cluster.RepoCredentials
	switch a.cfg.RepoCreds {
	case config.RepoCredsCluster:
		creds, err = a.kubeClient.LoadRepoCredentials(ctx, a.cfg.ArgoCDNamespace)
		if err != nil {
			return fmt.Errorf(
				"cannot read ArgoCD repository secrets in namespace %q: %w\n"+
					"fix RBAC, or use --repo-creds=local (your helm config) / --repo-creds=none (anonymous)",
				a.cfg.ArgoCDNamespace, err)
		}
		a.logger.Debug("Loaded repository credentials from cluster secrets",
			"helmRepos", len(creds.HelmRepos), "ociRepos", len(creds.OCIRepos),
			"helmRepoCreds", len(creds.HelmRepoCreds), "ociRepoCreds", len(creds.OCIRepoCreds))
	case config.RepoCredsLocal:
		creds, err = helmconfig.LoadLocalRepoCredentials()
		if err != nil {
			return fmt.Errorf("cannot load local helm credentials: %w", err)
		}
		a.logger.Debug("Loaded repository credentials from local helm config",
			"helmRepos", len(creds.HelmRepos))
	}

	// Create renderer
	a.renderer, err = a.factory.CreateRenderFactory(kubeVersion, apiVersions, creds)
	if err != nil {
		return fmt.Errorf("failed to create renderer: %w", err)
	}

	// Create differ and discoverer
	a.differ = a.factory.CreateManifestDiffer()
	a.discoverer = a.factory.CreateAppDiscoverer()
	a.linter = a.createLintRunner()
	a.warnMissingPolicyDirs()

	// Create output writer
	a.writer, err = a.factory.CreateOutputWriter()
	if err != nil {
		return fmt.Errorf("failed to create output writer: %w", err)
	}

	return nil
}

// createLintRunner builds the lint runner for this run, feeding it the context
// the cluster client actually connected with so lint commands can target the
// cluster being diffed. Extracted from initialize so the wiring from client to
// runner is testable without a live cluster: passing the wrong thing here (or
// nothing) is exactly how cluster-aware lint adapters would silently fall back
// to the invoking shell's cluster. Returns nil when --lint is not configured.
func (a *App) createLintRunner() *lint.Runner {
	var kubeContext string
	if a.kubeClient != nil {
		kubeContext = a.kubeClient.ResolvedContext()
	}
	return a.factory.CreateLintRunner(kubeContext)
}

// warnMissingPolicyDirs warns once per configured policy directory that does not
// exist in the working tree.
//
// Absence is deliberately NOT fatal at render time: on the base side of a PR that
// adds the first policy there is legitimately nothing to apply, and both tools
// treat an empty policy set as a hard error, so the adapters skip a missing
// directory instead of attaching a spurious lint failure to every application.
// The cost of that tolerance is that a TYPO reads exactly like "no findings".
// Checking the working tree closes it: the path a user typed should exist in the
// checkout they typed it from, whichever branch pairs get rendered later.
func (a *App) warnMissingPolicyDirs() {
	check := func(flag string, dirs []string) {
		for _, dir := range dirs {
			path := dir
			if !filepath.IsAbs(path) {
				path = filepath.Join(a.cfg.RepoPath, dir)
			}
			if info, err := os.Stat(path); err != nil || !info.IsDir() {
				a.logger.Warn("Lint policy directory not found in the working tree; "+
					"that side will be linted with no policies at all",
					"flag", flag, "dir", dir, "resolved", path)
			}
		}
	}
	check("--lint-kyverno", a.cfg.LintKyverno)
	check("--lint-conftest", a.cfg.LintConftest)
}

// lintSide runs the lint commands for one side of one application and logs how
// long they took at INFO. The duration is the diagnostic that a bare
// "--lint-timeout" warning lacks: it distinguishes a genuinely slow or hung
// adapter from contention between concurrent invocations, and shows the headroom
// left when nothing timed out.
func (a *App) lintSide(ctx context.Context, appName, side, worktree, rendered string) []string {
	start := time.Now()
	warnings := a.linter.Lint(ctx, worktree, rendered)
	a.logger.Info("Linted rendered manifests",
		"app", appName, "side", side,
		"duration", time.Since(start).Round(time.Millisecond),
		"findings", len(warnings))
	return warnings
}

// resolveKubeVersion returns the Kubernetes version to render with. An
// explicitly configured version wins and skips cluster detection entirely
// (the e2e kube-version-override behavior); otherwise the version is detected
// from the cluster, falling back to DefaultKubeVersionFallback when detection
// fails (the returned error is the detection error, for logging — the
// returned version is always usable).
func resolveKubeVersion(explicit string, detect func() (string, error)) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	v, err := detect()
	if err != nil {
		return config.DefaultKubeVersionFallback, err
	}
	return v, nil
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
// target branch tip (their commits were already resolved in Run). It always
// returns a non-nil cleanup function so callers can defer it unconditionally,
// even on error (partial worktrees are cleaned up).
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

// fetchApplications retrieves ArgoCD applications from the configured
// namespaces. With --application-namespaces unset, only the ArgoCD
// control-plane namespace is scanned. An all-literal list is served by
// per-namespace List calls (strictly namespace-scoped RBAC suffices); any
// glob or /regex/ entry requires a cluster-wide list, filtered afterwards
// with ArgoCD's own pattern matcher.
func (a *App) fetchApplications(ctx context.Context) ([]cluster.Application, error) {
	entries := a.cfg.ApplicationNamespaces
	if len(entries) == 0 {
		return a.appService.List(ctx, a.cfg.ArgoCDNamespace)
	}
	if cluster.AllLiteralNamespaces(entries) {
		return a.appService.ListNamespaces(ctx, entries)
	}
	apps, err := a.appService.ListAllNamespaces(ctx)
	if err != nil {
		return nil, err
	}
	var matched []cluster.Application
	for _, application := range apps {
		if cluster.MatchesNamespacePatterns(application.Namespace, entries) {
			matched = append(matched, application)
		}
	}
	return matched, nil
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
						IsNew:           true,
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

			// Find removed child applications (present on base, absent on target).
			// A removed child's target render must be skipped via the explicit
			// IsRemoved flag: its source directory typically still exists on the
			// target branch (only the parent's catalog entry was deleted), so a
			// target render would succeed and wrongly diff the child as alive.
			// Recursion is free: the removed child's own empty target render makes
			// its FindRemovedApplications return every base-side grandchild.
			removedApps, err := a.discoverer.FindRemovedApplications(appDiff.RenderedOld, appDiff.RenderedNew)
			if err != nil {
				a.logger.Warn("Error discovering removed child apps", "parent", queuedApp.Name, "error", err)
			} else {
				for _, removedApp := range removedApps {
					childApp := diff.QueuedApp{
						Name:            removedApp.Name,
						Namespace:       removedApp.Namespace,
						Depth:           queuedApp.Depth + 1,
						ParentApp:       queuedApp.Name,
						ParentNamespace: queuedApp.Namespace,
						// The base-side spec is the only one that exists; with
						// OldSpec nil, processOneApp renders the base side from it.
						Spec:      &removedApp.Spec,
						IsRemoved: true,
					}

					// All three modified-children cases apply to removed children:
					// the child can still be pending (queued from the cluster
					// before its parent revealed the removal — the pending render
					// would diff it as alive) or already processed (that result
					// rendered both sides — semantically wrong for the same
					// reason). Requeues survive the specSignature guard because
					// IsRemoved is part of the signature; the spec itself usually
					// matches the earlier pass (cluster spec == base spec).

					// Case 1: App is still pending - mark it removed
					if queue.UpdatePending(childApp) {
						a.logger.Debug("Updated pending child application as removed",
							"parent", queuedApp.Name, "child", removedApp.Name)
						appDiff.ChildAppNames = append(appDiff.ChildAppNames, removedApp.Name)
						continue
					}

					// Case 2: App not in queue at all - add it (pure child discovery)
					if queue.Add(childApp) {
						a.logger.Debug("Discovered removed child application",
							"parent", queuedApp.Name, "child", removedApp.Name)
						appDiff.ChildAppNames = append(appDiff.ChildAppNames, removedApp.Name)
						continue
					}

					// Case 3: App was already processed - requeue for re-processing
					if queue.RequeueProcessed(childApp) {
						a.logger.Info("Re-queuing already-processed child as removed",
							"parent", queuedApp.Name, "child", removedApp.Name)
						appDiff.ChildAppNames = append(appDiff.ChildAppNames, removedApp.Name)
					}
				}
			}
		}
	}

	// Convert map to slice, sorted by (namespace, name) so reports are
	// deterministic run-to-run (map iteration order is randomized). Stable
	// ordering lets report outputs be compared byte-for-byte across runs.
	var resultSlice []*types.AppDiff
	for _, r := range results {
		resultSlice = append(resultSlice, r)
	}
	sort.Slice(resultSlice, func(i, j int) bool {
		if resultSlice[i].Namespace != resultSlice[j].Namespace {
			return resultSlice[i].Namespace < resultSlice[j].Namespace
		}
		return resultSlice[i].Name < resultSlice[j].Name
	})
	// Child name lists follow discovery order (map iteration inside the render
	// results); sort them too so any output surfacing them stays deterministic.
	for _, r := range resultSlice {
		sort.Strings(r.ChildAppNames)
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
	//
	// Newly-added child apps (discovered only on the target branch) have no
	// base-branch counterpart, so the base render is skipped and the empty
	// base side makes the whole app diff as added. Rendering them against the
	// base worktree with the target spec can fail hard when the spec
	// references files that don't exist there yet (e.g. a new values file in
	// a pre-existing chart directory), which sourcePathsExist cannot catch.
	var renderedOld []byte
	if queuedApp.IsNew {
		a.logger.Debug("Skipping base render for newly-added application",
			"app", queuedApp.Name, "branch", a.baseRef)
	} else {
		rendered, sourceTypeOld, err := a.renderBranch(ctx, appOld, a.baseWorktree, a.baseCommit, a.baseRef, "new app")
		if err != nil {
			return nil, fmt.Errorf("failed to render base branch: %w", err)
		}
		renderedOld = rendered
		appDiff.SourceType = sourceTypeOld
	}

	// Render from the target worktree (committed target tip) using new spec.
	//
	// Removed child apps (discovered only on the base branch) have no
	// target-branch counterpart, so the target render is skipped and the empty
	// target side makes the whole app diff as removed. The skip must key off
	// the explicit flag, not a missing source path: removing a child usually
	// deletes only the parent's catalog entry while the child's source
	// directory still exists on the target branch, so sourcePathsExist cannot
	// catch it and a target render would succeed with live manifests.
	var renderedNew []byte
	if queuedApp.IsRemoved {
		a.logger.Debug("Skipping target render for removed application",
			"app", queuedApp.Name, "branch", a.cfg.TargetBranch)
	} else {
		rendered, sourceTypeNew, err := a.renderBranch(ctx, appNew, a.targetWorktree, a.targetCommit, a.cfg.TargetBranch, "deleted app")
		if err != nil {
			return nil, fmt.Errorf("failed to render target branch: %w", err)
		}
		renderedNew = rendered
		if appDiff.SourceType == "" {
			appDiff.SourceType = sourceTypeNew
		}
	}

	appDiff.RenderedOld = string(renderedOld)
	appDiff.RenderedNew = string(renderedNew)

	// Compute diff
	diffResult, err := a.differ.DiffManifests(appDiff.RenderedOld, appDiff.RenderedNew)
	if err != nil {
		return nil, fmt.Errorf("failed to compute diff: %w", err)
	}

	// Lint each side's rendered manifests. Lint findings join ParseWarnings
	// under the same [base]/[target] labels: a [base]-only finding was fixed by
	// the change under review, [target]-only was introduced, both = pre-existing.
	// Each side's command runs in that side's worktree, so repo-relative paths
	// (e.g. a policy directory) resolve to the files as of that branch.
	// Timing is logged per app and side: cluster-aware adapters (kyverno apply
	// --cluster, kubectl --dry-run=server) pay for an API round trip per
	// invocation, and --concurrency runs several at once, so a --lint-timeout
	// warning is usually contention rather than a broken tool. Without this
	// number the only signal is the timeout itself, which names no cause.
	if a.linter != nil {
		if appDiff.RenderedOld != "" {
			diffResult.ParseWarnings = append(diffResult.ParseWarnings,
				diff.LabelSide(diff.SideBase, a.lintSide(ctx, appDiff.Name, diff.SideBase, a.baseWorktree, appDiff.RenderedOld))...)
		}
		if appDiff.RenderedNew != "" {
			diffResult.ParseWarnings = append(diffResult.ParseWarnings,
				diff.LabelSide(diff.SideTarget, a.lintSide(ctx, appDiff.Name, diff.SideTarget, a.targetWorktree, appDiff.RenderedNew))...)
		}
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
		result, rerr := a.renderer.RenderApplication(ctx, app, worktreePath, commit)
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
			HelmAddRepos:            a.cfg.HelmAddRepos,
			Renderer:                a.cfg.Renderer,
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
		ReadFile: func(commit, path string) (string, bool) {
			content, rerr := a.repo.FileContent(commit, path)
			if rerr != nil {
				return "", false
			}
			return content, true
		},
	})
}

// sourcePathsExist checks if all local source paths for an application exist on disk.
// Remote chart sources (with Chart field set) are skipped since they don't need a local path.
// Sources living in another repository are skipped too: their paths exist only in that
// repo's checkout, cloned at render time (render/externalrepo.go). The external test must
// mirror render.isExternalSource, keyed off the same cfg.RepoURL the factory feeds into
// RenderOptions.RepoURL, so both layers agree on what "external" means; an unknown local
// repo URL treats everything as local.
func (a *App) sourcePathsExist(app *cluster.Application, repoPath string) bool {
	localRepoURL := a.cfg.RepoURL
	for _, source := range app.Spec.GetSources() {
		// Remote charts don't need a local path
		if source.Chart != "" {
			continue
		}
		if localRepoURL != "" && source.RepoURL != "" &&
			git.NormalizeRepoURL(source.RepoURL) != git.NormalizeRepoURL(localRepoURL) {
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

// writeOutput writes the results to the given writer. The normal path passes
// a.writer (all configured outputs); the no-applications-affected path passes a
// file-only subset so the terminal stays quiet (see Run).
func (a *App) writeOutput(w output.Writer, tree *diff.AppTree, summary output.Summary) error {
	title := fmt.Sprintf("ArgoCD Diff: %s → %s", a.cfg.BaseBranch, a.cfg.TargetBranch)

	if err := w.WriteHeader(title); err != nil {
		return err
	}

	if err := w.WriteTree(tree); err != nil {
		return err
	}

	if err := w.WriteSummary(summary); err != nil {
		return err
	}

	if err := w.WriteFooter(output.Provenance{
		Version:   a.cfg.Version,
		BaseSHA:   a.baseCommit,
		TargetSHA: a.targetCommit,
	}); err != nil {
		return err
	}

	return w.Flush()
}
