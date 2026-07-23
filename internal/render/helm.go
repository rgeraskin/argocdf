// Package render provides Helm chart rendering.
package render

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/rgeraskin/argocdf/internal/cluster"
	"github.com/rgeraskin/argocdf/internal/types"
	"sigs.k8s.io/yaml"
)

// chartDepLocks serializes `helm dependency build` per chart directory. When
// apps render in parallel from a shared worktree, two apps pointing at the same
// local chart would otherwise run `helm dependency build` into the same
// charts/ directory concurrently and race. It maps an absolute chart path to
// its *sync.Mutex.
var chartDepLocks sync.Map

// chartDepMutex returns the mutex guarding `helm dependency build` for the
// given chart path, creating it on first use.
func chartDepMutex(chartPath string) *sync.Mutex {
	m, _ := chartDepLocks.LoadOrStore(chartPath, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// HelmRenderer renders Helm charts using the helm binary.
type HelmRenderer struct {
	opts RenderOptions
}

// NewHelmRenderer creates a new HelmRenderer.
func NewHelmRenderer(opts RenderOptions) *HelmRenderer {
	return &HelmRenderer{opts: opts}
}

// SourceType returns the source type for Helm.
func (r *HelmRenderer) SourceType() types.SourceType {
	return types.SourceTypeHelm
}

// Render renders a Helm chart source.
// The context can be used to cancel long-running helm template operations.
func (r *HelmRenderer) Render(ctx context.Context, app *cluster.Application, source *cluster.ApplicationSource, repoPath string) ([]byte, error) {
	// Check context before starting
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	// Determine chart location and build args
	args, env, tempDir, tempFiles, err := r.buildArgs(ctx, app, source, repoPath)
	if err != nil {
		return nil, err
	}

	// Cleanup temp resources after helm command completes
	if tempDir != "" {
		defer func() {
			_ = SafeRemoveAll(tempDir)
		}()
	}
	for _, f := range tempFiles {
		defer func(path string) {
			_ = os.Remove(path)
		}(f)
	}

	// For local charts, ensure dependencies are built
	if source.Path != "" {
		chartPath := filepath.Join(repoPath, source.Path)
		if err := r.ensureDependencies(ctx, chartPath); err != nil {
			return nil, fmt.Errorf("failed to build dependencies: %w", err)
		}
	}

	// Run helm template with context
	cmd := exec.CommandContext(ctx, "helm", args...)
	if env != nil {
		// Use the isolated environment so helm sees the temp repo config
		// created by handleRemoteChart
		cmd.Env = env
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("helm template failed: %v\nstderr: %s", err, stderr.String())
	}

	return stdout.Bytes(), nil
}

// buildArgs builds the helm template command arguments.
// Returns args, env (isolated environment for remote charts, nil otherwise),
// tempDir (for remote chart cleanup), tempFiles (for inline values cleanup), and error.
func (r *HelmRenderer) buildArgs(ctx context.Context, app *cluster.Application, source *cluster.ApplicationSource, repoPath string) ([]string, []string, string, []string, error) {
	var env []string
	var tempDir string
	var tempFiles []string

	// Determine release name
	releaseName := app.Name
	if source.Helm != nil && source.Helm.ReleaseName != "" {
		releaseName = source.Helm.ReleaseName
	}

	args := []string{"template", releaseName}

	// Determine chart location
	if source.Chart != "" {
		// Remote chart from repository
		chartRef, chartEnv, tempDirPath, isLocalChart, err := r.handleRemoteChart(ctx, source)
		if err != nil {
			return nil, nil, "", nil, err
		}
		env = chartEnv
		tempDir = tempDirPath
		args = append(args, chartRef)
		// For remote charts, targetRevision is the chart version. When the
		// chart was served from the download cache it is already unpacked at a
		// pinned version and chartRef is a local directory; helm rejects
		// --version on a local path, so it is omitted in that case.
		if !isLocalChart && source.TargetRevision != "" && source.TargetRevision != "HEAD" {
			args = append(args, "--version", source.TargetRevision)
		}
	} else if source.Path != "" {
		// Local chart from repository
		chartPath := filepath.Join(repoPath, source.Path)
		args = append(args, chartPath)
	} else {
		return nil, nil, "", nil, fmt.Errorf("no chart or path specified in source")
	}

	// Add namespace
	namespace := app.Spec.Destination.Namespace
	if namespace != "" {
		args = append(args, "--namespace", namespace)
	}

	// Add Kubernetes version if specified. The version is sanitized to a bare
	// major.minor.patch so vendor suffixes (e.g. "-gke.1091002") don't parse as
	// a semver prerelease and break chart constraints like ">=1.29.5".
	if r.opts.KubeVersion != "" {
		args = append(args, "--kube-version", SanitizeKubeVersion(r.opts.KubeVersion))
	}

	// Add discovered cluster API versions so charts can branch on
	// .Capabilities.APIVersions. Helm accepts repeated --api-versions flags.
	for _, v := range r.opts.APIVersions {
		args = append(args, "--api-versions", v)
	}

	// Add Helm-specific options
	if source.Helm != nil {
		chartDir := repoPath
		if source.Path != "" {
			chartDir = filepath.Join(repoPath, source.Path)
		}
		var err error
		args, tempFiles, err = r.addHelmOptions(args, source.Helm, repoPath, chartDir)
		if err != nil {
			// Cleanup any temp files created before the error
			for _, f := range tempFiles {
				_ = os.Remove(f)
			}
			if tempDir != "" {
				_ = SafeRemoveAll(tempDir)
			}
			return nil, nil, "", nil, err
		}
	}

	return args, env, tempDir, tempFiles, nil
}

// isolatedHelmEnv returns an environment with helm cache/config/data homes
// isolated to tempDir, so repo operations don't touch the user's helm config.
func isolatedHelmEnv(tempDir string) []string {
	return append(os.Environ(),
		"HELM_CACHE_HOME="+filepath.Join(tempDir, "cache"),
		"HELM_CONFIG_HOME="+filepath.Join(tempDir, "config"),
		"HELM_DATA_HOME="+filepath.Join(tempDir, "data"),
	)
}

// handleRemoteChart handles fetching a chart from a remote repository.
// Returns the chart reference, the isolated environment to use for helm
// commands (nil when the default environment suffices), a temp directory to
// cleanup (empty when none was created), and whether the returned reference is
// a local (already-unpacked) chart directory rather than a remote reference.
//
// For pinned (immutable) chart versions and an enabled chart cache, the chart
// is pulled+unpacked once into the persistent cache and subsequent renders
// template the local directory directly, skipping helm repo add/update.
func (r *HelmRenderer) handleRemoteChart(ctx context.Context, source *cluster.ApplicationSource) (string, []string, string, bool, error) {
	repoURL := source.RepoURL

	// Fast path: persistent download cache for pinned chart versions.
	cacheDir, chartDir, hit, enabled := chartCacheDecision(
		r.opts.ChartCacheDir, repoURL, source.Chart, source.TargetRevision, dirExists,
	)
	if enabled {
		if hit {
			return chartDir, nil, "", true, nil
		}
		if err := r.pullChartToCache(ctx, source, cacheDir, chartDir); err == nil {
			return chartDir, nil, "", true, nil
		}
		// On any pull/cache error, fall through to the always-fetch path so
		// rendering stays functional even if the cache cannot be populated.
	}

	if strings.HasPrefix(repoURL, "oci://") {
		// OCI registry - helm can pull directly; the chart version is
		// passed separately via --version
		return repoURL + "/" + source.Chart, nil, "", false, nil
	}

	// HTTP/HTTPS repo - need to add repo first
	// Create a temp directory for repo operations
	tempDir, err := os.MkdirTemp("", "argocdf-helm-")
	if err != nil {
		return "", nil, "", false, fmt.Errorf("failed to create temp dir: %w", err)
	}
	env := isolatedHelmEnv(tempDir)

	// Generate a unique repo name
	repoName := "argocdf-temp-" + source.Chart

	// Add the repo with context
	addArgs := []string{"repo", "add", repoName, repoURL, "--force-update"}
	addCmd := exec.CommandContext(ctx, "helm", addArgs...)
	addCmd.Env = env
	if output, err := addCmd.CombinedOutput(); err != nil {
		_ = SafeRemoveAll(tempDir)
		if ctx.Err() != nil {
			return "", nil, "", false, ctx.Err()
		}
		return "", nil, "", false, fmt.Errorf("failed to add helm repo: %v\noutput: %s", err, output)
	}

	// Update the repo with context
	updateArgs := []string{"repo", "update", repoName}
	updateCmd := exec.CommandContext(ctx, "helm", updateArgs...)
	updateCmd.Env = env
	if output, err := updateCmd.CombinedOutput(); err != nil {
		_ = SafeRemoveAll(tempDir)
		if ctx.Err() != nil {
			return "", nil, "", false, ctx.Err()
		}
		return "", nil, "", false, fmt.Errorf("failed to update helm repo: %v\noutput: %s", err, output)
	}

	chartRef := repoName + "/" + source.Chart
	return chartRef, env, tempDir, false, nil
}

// dirExists reports whether p exists and is a directory.
func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// pullChartToCache pulls a pinned chart version into the persistent cache using
// an atomic claim: the chart is unpacked into a sibling temp directory and then
// renamed into place, so concurrent renders never observe a partial chart. If a
// concurrent render already published the chart, the existing directory is
// treated as a hit.
func (r *HelmRenderer) pullChartToCache(ctx context.Context, source *cluster.ApplicationSource, cacheDir, chartDir string) error {
	parent := filepath.Dir(cacheDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("failed to create chart cache dir: %w", err)
	}

	// Isolated helm homes so the pull never touches the user's helm config.
	homeTmp, err := os.MkdirTemp("", "argocdf-helmhome-")
	if err != nil {
		return fmt.Errorf("failed to create temp helm home: %w", err)
	}
	defer func() { _ = SafeRemoveAll(homeTmp) }()

	// Unpack into a sibling temp dir that we atomically rename into place.
	untarTmp, err := os.MkdirTemp(parent, "argocdf-chart-*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp chart dir: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = SafeRemoveAll(untarTmp)
		}
	}()

	args := []string{"pull"}
	if strings.HasPrefix(source.RepoURL, "oci://") {
		args = append(args, source.RepoURL+"/"+source.Chart)
	} else {
		args = append(args, source.Chart, "--repo", source.RepoURL)
	}
	args = append(args, "--version", source.TargetRevision, "--untar", "--untardir", untarTmp)

	cmd := exec.CommandContext(ctx, "helm", args...)
	cmd.Env = isolatedHelmEnv(homeTmp)
	if output, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("failed to pull helm chart: %v\noutput: %s", err, output)
	}

	// Atomically publish. If another render won the race, treat as a hit.
	if err := os.Rename(untarTmp, cacheDir); err != nil {
		if dirExists(chartDir) {
			return nil
		}
		return fmt.Errorf("failed to publish cached chart: %w", err)
	}
	published = true
	return nil
}

// addHelmOptions adds Helm-specific options to the command arguments.
// repoPath is the repository root, chartDir is the chart directory (for relative path resolution).
// Returns the updated args, a list of temp files to cleanup, and any error.
func (r *HelmRenderer) addHelmOptions(args []string, helm *cluster.ApplicationSourceHelm, repoPath, chartDir string) ([]string, []string, error) {
	var tempFiles []string

	// Add value files
	for _, valueFile := range helm.ValueFiles {
		// Handle $ref references in value files
		resolvedPath, err := r.resolveValueFilePath(valueFile, repoPath, chartDir)
		if err != nil {
			return nil, tempFiles, fmt.Errorf("failed to resolve value file %q: %w", valueFile, err)
		}
		args = append(args, "--values", resolvedPath)
	}

	// Add inline values (string)
	if helm.Values != "" {
		// Write inline values to a temp file
		tmpFile, err := os.CreateTemp("", "values-*.yaml")
		if err != nil {
			return nil, tempFiles, fmt.Errorf("failed to create temp file for inline values: %w", err)
		}
		if _, err := tmpFile.WriteString(helm.Values); err != nil {
			_ = tmpFile.Close()
			_ = os.Remove(tmpFile.Name())
			return nil, tempFiles, fmt.Errorf("failed to write inline values: %w", err)
		}
		_ = tmpFile.Close()
		tempFiles = append(tempFiles, tmpFile.Name())
		args = append(args, "--values", tmpFile.Name())
	}

	// Add inline values object (structured)
	// ValuesObject is a runtime.RawExtension containing JSON bytes
	if helm.ValuesObject != nil && len(helm.ValuesObject.Raw) > 0 {
		// Convert JSON to YAML and write to a temp file
		valuesYAML, err := yaml.JSONToYAML(helm.ValuesObject.Raw)
		if err != nil {
			return nil, tempFiles, fmt.Errorf("failed to convert values object to YAML: %w", err)
		}
		tmpFile, err := os.CreateTemp("", "values-object-*.yaml")
		if err != nil {
			return nil, tempFiles, fmt.Errorf("failed to create temp file for values object: %w", err)
		}
		if _, err := tmpFile.Write(valuesYAML); err != nil {
			_ = tmpFile.Close()
			_ = os.Remove(tmpFile.Name())
			return nil, tempFiles, fmt.Errorf("failed to write values object: %w", err)
		}
		_ = tmpFile.Close()
		tempFiles = append(tempFiles, tmpFile.Name())
		args = append(args, "--values", tmpFile.Name())
	}

	// Add parameter overrides
	for _, param := range helm.Parameters {
		if param.ForceString {
			args = append(args, "--set-string", fmt.Sprintf("%s=%s", param.Name, param.Value))
		} else {
			args = append(args, "--set", fmt.Sprintf("%s=%s", param.Name, param.Value))
		}
	}

	// Add file parameters
	for _, fileParam := range helm.FileParameters {
		resolvedPath, err := r.resolveValueFilePath(fileParam.Path, repoPath, chartDir)
		if err != nil {
			return nil, tempFiles, fmt.Errorf("failed to resolve file parameter %q: %w", fileParam.Path, err)
		}
		args = append(args, "--set-file", fmt.Sprintf("%s=%s", fileParam.Name, resolvedPath))
	}

	// Skip values.schema.json validation when the app opts out (Helm's
	// --skip-schema-validation, helm >= 3.16). ArgoCD renders such apps
	// fine, so without this flag argocdf fails where ArgoCD succeeds.
	if helm.SkipSchemaValidation {
		args = append(args, "--skip-schema-validation")
	}

	// Note: helm.Version is the Helm binary version to use for templating
	// (e.g. "3"), not a chart version, so it is intentionally not passed
	// as --version. The tool always uses the helm binary on PATH.

	return args, tempFiles, nil
}

// resolveValueFilePath resolves a value file path, handling $ref references.
// repoPath is the repository root, chartDir is the chart directory.
// Relative paths are resolved relative to chartDir (matching ArgoCD behavior).
// Returns an error if the resolved path escapes the allowed directory boundaries.
func (r *HelmRenderer) resolveValueFilePath(path, repoPath, chartDir string) (string, error) {
	// Check if path uses a $ref reference
	if strings.HasPrefix(path, "$") {
		// Format: $refname/path/to/file.yaml
		parts := strings.SplitN(path, "/", 2)
		if len(parts) == 2 {
			refName := strings.TrimPrefix(parts[0], "$")
			if refPath, ok := r.opts.RefSources[refName]; ok {
				resolved := filepath.Join(refPath, parts[1])
				// Validate that the resolved path stays within the ref source directory
				if err := ValidatePathContainment(refPath, resolved); err != nil {
					return "", fmt.Errorf("invalid ref path %q: %w", path, err)
				}
				return resolved, nil
			}
		}
		// If ref not found, return path as-is (let helm handle the error)
		return path, nil
	}

	// Regular path - make absolute if needed
	// Relative paths are resolved relative to the chart directory
	var resolved string
	if !filepath.IsAbs(path) {
		resolved = filepath.Join(chartDir, path)
	} else {
		resolved = path
	}

	// Validate that the resolved path stays within the repository
	if err := ValidatePathContainment(repoPath, resolved); err != nil {
		return "", fmt.Errorf("invalid value file path %q: %w", path, err)
	}

	return resolved, nil
}

// CanRender checks if the helm binary is available.
func (r *HelmRenderer) CanRender() error {
	cmd := exec.Command("helm", "version", "--short")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("helm binary not found or not working: %w", err)
	}
	return nil
}

// SanitizeKubeVersion extracts a bare major.minor.patch version from a raw
// Kubernetes server version. It strips a leading "v" and any vendor suffix
// introduced by "-" (prerelease, e.g. "-gke.1091002", "-eks-abc123") or "+"
// (build metadata). Versions without a patch component (e.g. "1.29") are
// returned unchanged after trimming. If the input has no usable numeric
// prefix, the trimmed input is returned as-is so helm can surface the error.
func SanitizeKubeVersion(version string) string {
	v := strings.TrimSpace(version)
	v = strings.TrimPrefix(v, "v")

	// Strip anything from the first "-" (prerelease) or "+" (build metadata).
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}

	return v
}

// ParseKubeVersion parses a Kubernetes version string.
func ParseKubeVersion(version string) (major, minor string, err error) {
	// Remove v prefix if present
	version = strings.TrimPrefix(version, "v")

	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("invalid kubernetes version: %s", version)
	}

	return parts[0], parts[1], nil
}

// ensureDependencies checks if the chart has dependencies and builds them if needed.
// It runs `helm dependency build` if Chart.yaml exists with a dependencies section.
// Helm is smart enough to skip already-fetched dependencies.
func (r *HelmRenderer) ensureDependencies(ctx context.Context, chartPath string) error {
	// Serialize per chart directory: parallel renders may share a worktree, and
	// concurrent `helm dependency build` runs into the same charts/ dir race.
	// It still mutates the (ephemeral) worktree, which is acceptable since the
	// worktree is thrown away, but it must not run concurrently for a path.
	mu := chartDepMutex(chartPath)
	mu.Lock()
	defer mu.Unlock()

	// Check if Chart.yaml exists
	chartYamlPath := filepath.Join(chartPath, "Chart.yaml")
	if _, err := os.Stat(chartYamlPath); os.IsNotExist(err) {
		// No Chart.yaml, nothing to do
		return nil
	}

	// Read Chart.yaml to check for dependencies
	chartYaml, err := os.ReadFile(chartYamlPath)
	if err != nil {
		return fmt.Errorf("failed to read Chart.yaml: %w", err)
	}

	// Simple check for dependencies section
	// We look for "dependencies:" at the start of a line
	if !strings.Contains(string(chartYaml), "\ndependencies:") &&
		!strings.HasPrefix(string(chartYaml), "dependencies:") {
		// No dependencies defined
		return nil
	}

	// Parse the dependencies to learn which third-party HTTP(S) repositories
	// they pull from — used by --helm-add-repos below and by the actionable
	// hint on a missing-repo failure. A parse failure just degrades to no repo
	// info; `helm dependency build` below surfaces the real problem.
	var chart chartDefinition
	_ = yaml.Unmarshal(chartYaml, &chart)
	repoURLs := dependencyRepoURLs(chart.Dependencies)

	// `helm dependency build` never adds or refreshes classic HTTP(S) repos —
	// it requires their index to already sit in the user-level helm cache.
	// When opted in (CI-friendly), register them up front. Off by default
	// because it mutates the user's helm repository config.
	if r.opts.HelmAddRepos {
		for _, repoURL := range repoURLs {
			if err := r.registerDependencyRepo(ctx, repoURL); err != nil {
				return err
			}
		}
	}

	// Check if charts/ directory exists and has content
	// Note: We always run helm dependency build when dependencies are defined
	// because just checking if charts/ has *any* content is not sufficient -
	// some dependencies might be missing while others are present.
	// Helm is smart enough to skip already-fetched dependencies.

	// Run helm dependency build with context. The read lock lets builds run
	// concurrently with each other while excluding in-flight repo
	// registrations that rewrite repositories.yaml (see helmRepoConfigMu).
	args := []string{"dependency", "build", chartPath}
	if r.opts.HelmSkipRefresh {
		args = append(args, "--skip-refresh")
	}
	cmd := exec.CommandContext(ctx, "helm", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	helmRepoConfigMu.RLock()
	err = cmd.Run()
	helmRepoConfigMu.RUnlock()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return dependencyBuildError(err, stderr.String(), repoURLs)
	}

	return nil
}

// chartDependency is the subset of a Chart.yaml dependency entry needed to
// resolve its repository.
type chartDependency struct {
	Name       string `json:"name"`
	Repository string `json:"repository"`
}

// chartDefinition is the subset of Chart.yaml parsed for dependency handling.
type chartDefinition struct {
	Dependencies []chartDependency `json:"dependencies"`
}

// dependencyRepoURLs returns the deduplicated, sorted classic HTTP(S) chart
// repository URLs referenced by the chart's dependencies. Everything else is
// excluded: oci:// registries (helm pulls them directly, no index needed),
// file:// paths (local), and @alias/alias: references (they resolve through an
// existing repositories.yaml entry, so there is no URL to add).
func dependencyRepoURLs(deps []chartDependency) []string {
	seen := make(map[string]bool)
	var urls []string
	for _, d := range deps {
		u := strings.TrimSpace(d.Repository)
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			continue
		}
		// Normalize away trailing slashes so slash variants of one repo share a
		// dedupe key, once-state, and hash name (helm's own URL matching is
		// trailing-slash tolerant, see findRepoByURL).
		u = strings.TrimRight(u, "/")
		if !seen[u] {
			seen[u] = true
			urls = append(urls, u)
		}
	}
	sort.Strings(urls)
	return urls
}

// depRepoStates deduplicates `helm repo add` + `helm repo update` per
// repository URL across the whole run: charts typically share third-party
// repos, and registering one is process-global state, so once is enough. It
// maps a repo URL to its *depRepoState.
var depRepoStates sync.Map

// helmRepoConfigMu guards helm's process-external repository state
// (repositories.yaml and the repo index cache) within this process. Helm
// itself flocks repositories.yaml for writers, but readers (`helm repo list`,
// `helm dependency build`) take no lock and the file is rewritten with a
// truncate-then-write, so a reader racing a writer can observe a partial file.
// Registrations take the write lock for their whole list+add+update sequence;
// dependency builds take the read lock so they still run concurrently with
// each other.
var helmRepoConfigMu sync.RWMutex

// depRepoState runs a repo registration exactly once and remembers its result.
// Failures are cached for the rest of the run on purpose: many apps typically
// share one dependency repo, and retrying a dead repo once per app would turn
// one failure into a slow, hammering run. The trade-off is that a transient
// error (DNS blip) is sticky until the next run — acceptable because a plan
// run is short-lived and cheap to re-trigger, and the error names the repo.
type depRepoState struct {
	once sync.Once
	err  error
}

// depRepoName derives a deterministic local name for a dependency repository
// URL. Hash-based names cannot clobber a repo the user registered under a
// human name, and two argocdf runs always agree on the name so --force-update
// re-adds are no-ops.
func depRepoName(repoURL string) string {
	sum := sha256.Sum256([]byte(repoURL))
	return "argocdf-dep-" + hex.EncodeToString(sum[:6])
}

// registerDependencyRepo makes a dependency repository resolvable by
// `helm dependency build`, once per process. When the URL is already
// registered under any name (helm matches dependencies by URL, not name), only
// that entry's index is refreshed — nothing new is written to
// repositories.yaml, so a developer machine with e.g. `cnpg` already added
// stays unpolluted. Only unknown URLs get a new (hash-named) entry.
//
// Unlike handleRemoteChart this intentionally uses the default helm
// environment: `helm dependency build` resolves repos from the user-level
// config/cache, so the index must land there — which is exactly why this is
// opt-in via --helm-add-repos.
func (r *HelmRenderer) registerDependencyRepo(ctx context.Context, repoURL string) error {
	s, _ := depRepoStates.LoadOrStore(repoURL, &depRepoState{})
	st := s.(*depRepoState)
	st.once.Do(func() {
		// Exclusive: the list+add+update sequence must not interleave with
		// other registrations (check-then-add consistency) or with dependency
		// builds reading repositories.yaml (see helmRepoConfigMu).
		helmRepoConfigMu.Lock()
		defer helmRepoConfigMu.Unlock()

		name := existingRepoName(ctx, repoURL)
		cmds := [][]string{{"repo", "update", name}}
		if name == "" {
			name = depRepoName(repoURL)
			cmds = [][]string{
				{"repo", "add", name, repoURL, "--force-update"},
				{"repo", "update", name},
			}
		}
		for _, args := range cmds {
			cmd := exec.CommandContext(ctx, "helm", args...)
			if output, err := cmd.CombinedOutput(); err != nil {
				if ctx.Err() != nil {
					st.err = ctx.Err()
					return
				}
				st.err = fmt.Errorf("helm %s %s failed for dependency repo %s: %v\noutput: %s",
					args[0], args[1], repoURL, err, output)
				return
			}
		}
	})
	return st.err
}

// repoListEntry is one entry of `helm repo list -o json`.
type repoListEntry struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// existingRepoName returns the name of an already-registered helm repository
// whose URL matches repoURL, or "" when none matches or the list cannot be
// read (helm exits non-zero when no repositories are configured at all —
// the fresh-runner case — which just means fall through to adding).
func existingRepoName(ctx context.Context, repoURL string) string {
	out, err := exec.CommandContext(ctx, "helm", "repo", "list", "-o", "json").Output()
	if err != nil {
		return ""
	}
	var repos []repoListEntry
	if err := json.Unmarshal(out, &repos); err != nil {
		return ""
	}
	return findRepoByURL(repos, repoURL)
}

// findRepoByURL returns the name of the first entry matching url, ignoring
// trailing slashes — mirroring helm's urlutil.Equal semantics used by
// `helm dependency build` to match a dependency's repository to an entry.
func findRepoByURL(repos []repoListEntry, url string) string {
	want := strings.TrimRight(url, "/")
	for _, r := range repos {
		if strings.TrimRight(r.URL, "/") == want {
			return r.Name
		}
	}
	return ""
}

// missingRepoErr reports whether helm stderr indicates a dependency repository
// missing from the local helm state. Helm emits "no repository definition for
// <url>" when repositories.yaml has no entry for the URL, and "no cached
// repository for <name> found" when an entry exists but its index was never
// downloaded (e.g. a fresh CI runner).
func missingRepoErr(stderr string) bool {
	return strings.Contains(stderr, "no repository definition for") ||
		strings.Contains(stderr, "no cached repository for")
}

// dependencyBuildError wraps a `helm dependency build` failure, appending an
// actionable hint when stderr shows the well-known unregistered-repository
// failure: which repos to `helm repo add`, and that --helm-add-repos automates
// it for CI.
func dependencyBuildError(err error, stderr string, repoURLs []string) error {
	buildErr := fmt.Errorf("helm dependency build failed: %v\nstderr: %s", err, stderr)
	if !missingRepoErr(stderr) {
		return buildErr
	}
	hint := "the chart's dependency repositories are not registered in the local helm cache"
	if len(repoURLs) > 0 {
		hint += fmt.Sprintf("; run `helm repo add <name> <url> && helm repo update` for: %s",
			strings.Join(repoURLs, " "))
	}
	hint += " — or pass --helm-add-repos (ARGOCDF_HELM_ADD_REPOS=true) to let argocdf register them (intended for CI)"
	return fmt.Errorf("%w\nhint: %s", buildErr, hint)
}
