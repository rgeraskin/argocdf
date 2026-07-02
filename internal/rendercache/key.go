package rendercache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"path"
	"strconv"
	"strings"

	"github.com/rgeraskin/argocdf/internal/cluster"
)

// SchemaVersion is embedded in every cache key. Bump it to invalidate all
// previously cached entries (e.g. when the render pipeline changes in a way
// that is not otherwise captured by the key inputs).
//
// v2: keys now hash render inputs that live OUTSIDE a source path — helm
// valueFiles/fileParameters (including $ref references) and, for
// kustomize/directory sources, the whole commit tree (see ComputeKey).
const SchemaVersion = "rendercache-v2"

// KeyOptions holds the render-relevant options that affect rendered output and
// therefore must participate in the cache key.
type KeyOptions struct {
	KustomizeEnableHelm     bool
	KustomizeBuildOptions   string
	KustomizeLoadRestrictor string
	HelmSkipRefresh         bool
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
//     paths resolve against the ref source's path). A value file that is absent
//     at the commit contributes an "absent" sentinel rather than a bypass,
//     because absence is itself part of the render identity.
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
		strconv.FormatBool(in.Options.HelmSkipRefresh),
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
// to a repo-relative path, mirroring internal/render/helm.go
// resolveValueFilePath. It returns bypass=true when the reference cannot be
// soundly resolved to local repo content: a $ref pointing at an external repo,
// an unknown/malformed $ref, or a path that escapes the repository root.
func resolveKeyValueFilePath(
	ref, chartPath string,
	refSources map[string]cluster.ApplicationSource,
	sameRepo func(repoURL string) bool,
) (relPath string, bypass bool) {
	if strings.HasPrefix(ref, "$") {
		refName, rest, ok := strings.Cut(strings.TrimPrefix(ref, "$"), "/")
		if !ok || refName == "" {
			return "", true
		}
		refSource, found := refSources[refName]
		if !found {
			return "", true
		}
		// Only same-repo ref sources have content available locally.
		if sameRepo == nil || !sameRepo(refSource.RepoURL) {
			return "", true
		}
		p := path.Clean(path.Join(refSource.Path, rest))
		if pathEscapesRepo(p) {
			return "", true
		}
		return p, false
	}

	// Relative path: resolved against the chart directory (ArgoCD behavior).
	if path.IsAbs(ref) {
		return "", true
	}
	p := path.Clean(path.Join(chartPath, ref))
	if pathEscapesRepo(p) {
		return "", true
	}
	return p, false
}

// pathEscapesRepo reports whether a cleaned, repo-relative path leaves the
// repository root (i.e. starts with ".." or is absolute).
func pathEscapesRepo(p string) bool {
	if path.IsAbs(p) {
		return true
	}
	return p == ".." || strings.HasPrefix(p, "../")
}
