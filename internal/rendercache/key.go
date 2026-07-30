package rendercache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/rgeraskin/argocdf/internal/cluster"
	"github.com/rgeraskin/argocdf/internal/render"
	"sigs.k8s.io/yaml"
)

// SchemaVersion is embedded in every cache key. Bump it to invalidate all
// previously cached entries (e.g. when the render pipeline changes in a way
// that is not otherwise captured by the key inputs).
//
// v2: keys now hash render inputs that live OUTSIDE a source path — helm
// valueFiles/fileParameters (including $ref references) and, for
// kustomize/directory sources, the whole commit tree (see ComputeKey).
//
// v3: the native render engine (and its --helm-skip-refresh/--helm-add-repos
// flags) is gone — everything renders through ArgoCD's repo-server code, and
// $ref value files resolve against the ref repository ROOT (ArgoCD semantics)
// instead of joining the ref source's Path.
const SchemaVersion = "rendercache-v3"

// KeyOptions holds the render-relevant options that affect rendered output and
// therefore must participate in the cache key.
type KeyOptions struct {
	KustomizeEnableHelm     bool
	KustomizeBuildOptions   string
	KustomizeLoadRestrictor string
}

// KeyInput bundles everything required to compute a cache key for a single
// render (one application spec at one commit).
type KeyInput struct {
	AppName     string
	Namespace   string
	Spec        *cluster.ApplicationSpec
	KubeVersion string
	Options     KeyOptions
	// Commit is the resolved commit hash being rendered.
	Commit string
	// ResolveTree returns the content-addressed git object hash for a path at
	// the given commit. It works for both trees (directories) and blobs (files)
	// via `git rev-parse <commit>:<path>`; an empty path resolves the commit's
	// root tree. It must return ok=false when the hash cannot be resolved (e.g.
	// the path does not exist at that commit). For a genuinely absent input the
	// caller decides whether that is a bypass or an "absent" sentinel.
	ResolveTree func(commit, path string) (string, bool)
	// SameRepo reports whether the given (raw) repo URL refers to the same
	// repository currently being diffed. It is used to classify $ref value-file
	// sources: same-repo refs resolve to a path in this repo, external-repo refs
	// cannot be resolved from local content and force a cache bypass. Callers
	// implement this with git.NormalizeRepoURL. When nil, every ref is treated
	// as external (conservative bypass).
	SameRepo func(repoURL string) bool
	// ReadFile returns the content of a repo-relative file at the given commit.
	// It is used to inspect a local chart's Chart.yaml dependencies for cache
	// soundness (see the hermeticity note in ComputeKey). It must return
	// ok=false when the file cannot be read. When nil, charts that would need
	// inspection are conservatively bypassed.
	ReadFile func(commit, path string) (content string, ok bool)
}

// ComputeKey computes the sha256 hex cache key for a render. It returns
// ok=false (and no key) when any required input is unavailable or when caching
// cannot be done soundly — for example a nil spec, an unmarshalable spec, a
// local source path whose tree hash cannot be resolved, a value file that
// escapes the repository, or a $ref value file pointing at an external repo
// (whose content is not present locally). Callers treat a false result as
// "bypass the cache for this render", never as an error.
//
// Soundness of out-of-source-path inputs:
//   - Helm local-chart sources additionally hash every resolved valueFiles and
//     fileParameters path (relative paths resolve against the chart dir; $ref
//     paths resolve against the ref repository root, ArgoCD's semantics). A
//     value file that is absent at the commit contributes an "absent" sentinel
//     rather than a bypass, because absence is itself part of the render
//     identity.
//   - Helm dependency resolution must be hermetic at the commit to be
//     cacheable: a chart whose dependency uses a version RANGE with no
//     committed Chart.lock resolves against the mutable repo index, so it
//     bypasses the cache (see chartDepsHermetic).
//   - Kustomize / directory / unknown sources can reference arbitrary repo
//     paths (bases, components, patches) that cannot be cheaply enumerated. To
//     stay sound we hash the commit's ROOT tree instead of the source-path
//     tree. Trade-off: cache hits then only occur when re-rendering the exact
//     same commit (still the dominant repeat-run case), and are never stale.
func ComputeKey(in KeyInput) (string, bool) {
	if in.Spec == nil {
		return "", false
	}

	specJSON, err := json.Marshal(in.Spec)
	if err != nil {
		return "", false
	}

	h := sha256.New()
	// writeField writes a length-independent, delimiter-separated field to keep
	// the concatenation unambiguous.
	writeField := func(parts ...string) {
		for _, p := range parts {
			_, _ = io.WriteString(h, p)
			_, _ = h.Write([]byte{0})
		}
	}

	writeField(SchemaVersion)
	writeField(in.AppName, in.Namespace)
	_, _ = h.Write(specJSON)
	_, _ = h.Write([]byte{0})
	writeField(in.KubeVersion)
	writeField(
		strconv.FormatBool(in.Options.KustomizeEnableHelm),
		in.Options.KustomizeBuildOptions,
		in.Options.KustomizeLoadRestrictor,
	)

	sources := in.Spec.GetSources()

	// Build a lookup of ref name -> ref source so $<ref>/... value files can be
	// resolved to a repo-relative path.
	refSources := make(map[string]cluster.ApplicationSource, len(sources))
	for _, src := range sources {
		if src.Ref != "" {
			refSources[src.Ref] = src
		}
	}

	// Per-source content identity.
	for i := range sources {
		src := sources[i]

		if src.Chart != "" {
			// Remote chart: identity is repo + chart + target revision. The
			// chart version is immutable, so this is sufficient.
			writeField("chart", src.RepoURL, src.Chart, src.TargetRevision)
			continue
		}

		if in.ResolveTree == nil {
			return "", false
		}

		if isHelmLikeSource(src, in.Commit, in.ResolveTree) {
			// Local helm chart: hash the chart path tree plus every value file
			// and file parameter it pulls in (which may live outside the path).
			treeHash, ok := in.ResolveTree(in.Commit, src.Path)
			if !ok {
				return "", false
			}
			writeField("helm", src.Path, treeHash)

			// Dependency hermeticity: a committed Chart.lock pins dependency
			// resolution and is already part of the tree hash above. Without a
			// lock, a dependency whose version is a RANGE resolves against the
			// mutable repo index (the engine's dependency build refreshes it in
			// an isolated helm home on every render), so the same commit can
			// legitimately render differently over time — such renders must
			// bypass the cache. Exactly-pinned versions resolve
			// deterministically and stay cacheable.
			if !chartDepsHermetic(src.Path, in.Commit, in.ResolveTree, in.ReadFile) {
				return "", false
			}

			if src.Helm != nil {
				extra := make([]string, 0, len(src.Helm.ValueFiles)+len(src.Helm.FileParameters))
				extra = append(extra, src.Helm.ValueFiles...)
				for _, fp := range src.Helm.FileParameters {
					extra = append(extra, fp.Path)
				}
				for _, ref := range extra {
					relPath, bypass := resolveKeyValueFilePath(ref, src.Path, refSources, in.SameRepo)
					if bypass {
						return "", false
					}
					if hash, ok := in.ResolveTree(in.Commit, relPath); ok {
						writeField("vf", ref, relPath, hash)
					} else {
						// Absent at this commit: absence is part of the render
						// identity, so record a stable sentinel instead of
						// bypassing.
						writeField("vf", ref, relPath, "absent")
					}
				}
			}
			continue
		}

		// Kustomize / directory / unknown source: use the commit root tree for
		// soundness (see the doc comment above).
		rootHash, ok := in.ResolveTree(in.Commit, "")
		if !ok {
			return "", false
		}
		writeField("dir", src.Path, rootHash)
	}

	return hex.EncodeToString(h.Sum(nil)), true
}

// isHelmLikeSource reports whether a (non-remote-chart) source should be
// rendered as a local Helm chart: either it carries a Helm config block, or a
// Chart.yaml exists at its path in the commit. The Chart.yaml probe reuses
// ResolveTree (a resolvable blob hash means the file exists).
func isHelmLikeSource(src cluster.ApplicationSource, commit string, resolve func(commit, path string) (string, bool)) bool {
	if src.Helm != nil {
		return true
	}
	if src.Path == "" || resolve == nil {
		return false
	}
	if _, ok := resolve(commit, path.Join(src.Path, "Chart.yaml")); ok {
		return true
	}
	return false
}

// resolveKeyValueFilePath resolves a helm value-file / file-parameter reference
// to a repo-relative path, mirroring the render engine (ArgoCD's semantics):
// "$ref/some/path" resolves against the ref repository ROOT (RefTarget has no
// Path field), relative paths against the chart directory. It returns
// bypass=true when the reference cannot be soundly resolved to local repo
// content: a $ref pointing at an external repo, an unknown/malformed $ref, or
// a path that escapes the repository root.
func resolveKeyValueFilePath(
	ref, chartPath string,
	refSources map[string]cluster.ApplicationSource,
	sameRepo func(repoURL string) bool,
) (relPath string, bypass bool) {
	// A $<ref>/... entry resolves against the REF repository's root, through the
	// same helper selection uses - so the key and the matcher cannot disagree about
	// which file an entry names. A $-prefixed entry the helper refuses (unknown ref
	// name, no path segment, traversal, remote URL) has no local content to hash, so
	// it bypasses rather than falling through to local resolution, which would treat
	// "$values/x" as a relative path.
	if strings.HasPrefix(ref, "$") {
		refSource, rel, ok := cluster.ResolveRefFilePath(
			ref, refSources, render.DefaultValuesFileSchemes)
		if !ok {
			return "", true
		}
		// Only same-repo ref sources have content available locally.
		if sameRepo == nil || !sameRepo(refSource.RepoURL) {
			return "", true
		}
		return rel, false
	}

	// Everything else goes through ArgoCD's own resolver - the same call SELECTION
	// makes (cluster.ResolveHelmFilePath) - so the key cannot hold a different idea
	// of which files an app reads than the matcher does. Three copies of this rule
	// is how the $ref join stayed wrong in two of them.
	//
	// Two shapes this gets right that the previous hand-rolled version did not.
	// An ABSOLUTE entry is repo-ROOT-relative in ArgoCD, not filesystem-absolute,
	// so bypassing on a leading "/" made every app with a /config/prod.yaml
	// permanently uncacheable while selection matched it happily. A REMOTE entry
	// (http/https, per the allowed schemes) must bypass instead: its content lives
	// outside the repository, no tree hash describes it, and the old code silently
	// joined it onto chartPath, failed to resolve that nonsense path, and recorded
	// it as "absent" - a cacheable key for a render that can change under a fixed
	// commit whenever the remote file does.
	rel, remote, err := cluster.ResolveHelmFilePath(chartPath, ref, render.DefaultValuesFileSchemes)
	if err != nil || remote {
		return "", true
	}
	if pathEscapesRepo(rel) {
		return "", true
	}
	return rel, false
}

// pathEscapesRepo reports whether a cleaned, repo-relative path leaves the
// repository root (i.e. starts with ".." or is absolute).
func pathEscapesRepo(p string) bool {
	if path.IsAbs(p) {
		return true
	}
	return p == ".." || strings.HasPrefix(p, "../")
}

// chartDepsHermetic reports whether a local chart's dependency resolution is
// deterministic at the commit, i.e. safe to cache. It is hermetic when a
// Chart.lock is committed (resolution is pinned, and the lock participates in
// the tree hash), when there is no Chart.yaml or no dependencies, or when
// every dependency version is an exact semver. It is NOT hermetic — bypass —
// when any dependency uses a version range without a lock, when Chart.yaml
// exists but cannot be read or parsed, or when readFile is nil and inspection
// is needed.
func chartDepsHermetic(
	srcPath, commit string,
	resolve func(commit, path string) (string, bool),
	readFile func(commit, path string) (string, bool),
) bool {
	if _, ok := resolve(commit, path.Join(srcPath, "Chart.lock")); ok {
		return true // lock pins resolution and is hashed with the chart tree
	}
	chartYamlPath := path.Join(srcPath, "Chart.yaml")
	if _, ok := resolve(commit, chartYamlPath); !ok {
		return true // no Chart.yaml -> no helm dependencies to resolve
	}
	if readFile == nil {
		return false
	}
	content, ok := readFile(commit, chartYamlPath)
	if !ok {
		return false
	}
	var chart struct {
		Dependencies []struct {
			Version string `json:"version"`
		} `json:"dependencies"`
	}
	if err := yaml.Unmarshal([]byte(content), &chart); err != nil {
		return false
	}
	for _, d := range chart.Dependencies {
		if !exactSemver(d.Version) {
			return false
		}
	}
	return true
}

// exactSemverRe matches a single exact semver version (optional v prefix,
// optional prerelease/build metadata). Anything else — operators (^ ~ > <),
// wildcards (x, *), hyphen ranges, ORs, empty — is a range.
var exactSemverRe = regexp.MustCompile(`^v?\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

// exactSemver reports whether the version string pins one exact version.
func exactSemver(version string) bool {
	return exactSemverRe.MatchString(strings.TrimSpace(version))
}
