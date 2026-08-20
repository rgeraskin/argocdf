package render

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"regexp"
	"strings"

	argogit "github.com/argoproj/argo-cd/v3/util/git"
)

// immutableChartVersionRe matches a single exact semver version (optional v
// prefix, optional prerelease/build metadata). Anything else - operators
// (^ ~ > <), wildcards (x, *), hyphen ranges, ORs, partial versions like
// "1.2", HEAD, empty - is a CONSTRAINT that helm resolves against the mutable
// repository index, so the version it selects can move between runs.
var immutableChartVersionRe = regexp.MustCompile(`^v?\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

// IsImmutableChartVersion reports whether a chart target revision names ONE
// exact, immutable version. It is the single predicate both caches share: the
// chart-download cache below only persists immutable versions — a CONSTRAINT is
// first resolved against the registry and then keyed by the version it resolved
// TO, so the mutable half is re-decided every run and only the immutable half is
// cached — and the render cache (rendercache.ComputeKey) bypasses itself for a
// remote chart whose revision is mutable - a "HEAD", "*" or "^2.0.0" chart can resolve to
// different content over time under identical key inputs, so a hit could
// serve manifests from a chart that no longer exists. Two predicates is how
// the caches previously DISAGREED: the chart cache treated only ""/HEAD/* as
// mutable (so a range like ^2.0.0 was wrongly cached forever), while the
// render cache keyed the literal revision string and never bypassed at all.
func IsImmutableChartVersion(version string) bool {
	return immutableChartVersionRe.MatchString(strings.TrimSpace(version))
}

// ociDigestRe matches an OCI content digest ("sha256:<hex>", or any registered
// algorithm in the same shape). A digest names immutable content by definition —
// the registry cannot serve different bytes under it.
var ociDigestRe = regexp.MustCompile(`^[a-z0-9]+(?:[.+_-][a-z0-9]+)*:[a-zA-Z0-9=_-]{32,}$`)

// IsImmutableOCIRevision reports whether an OCI-artifact source's target
// revision names content that cannot change under it: a digest, or one exact
// version tag.
//
// Anything else — "latest", a floating tag, a semver CONSTRAINT (which ArgoCD
// resolves against the registry's tag list, util/oci's versions.MaxVersion), or
// empty — can point at different bytes on a later run, so the render cache must
// bypass rather than serve a stale artifact's manifests.
//
// An exact version TAG is treated as immutable, which is a convention rather
// than a guarantee (a publisher can move a tag). That is deliberately the SAME
// assumption argocdf already makes for pinned helm chart versions in an OCI
// registry, which are stored as tags too: two different answers to one question
// is how the chart cache and the render cache previously disagreed.
func IsImmutableOCIRevision(revision string) bool {
	rev := strings.TrimSpace(revision)
	return ociDigestRe.MatchString(rev) || IsImmutableChartVersion(rev)
}

// IsImmutableGitRevision reports whether a git source's target revision names
// content that cannot change under it: a commit SHA (full or truncated — a
// truncated one still names exactly one commit), or one exact version tag.
//
// It exists for sources in ANOTHER repository, where argocdf has no resolved
// commit to key a cache on. A branch name, "HEAD", or an empty revision moves by
// design: the same string can name different content on a later run, so a cache
// entry under it can be stale while everything local is unchanged.
//
// An exact version TAG is treated as immutable by the same convention pinned
// chart versions and OCI tags already rely on (git tags CAN be moved; moving a
// released version tag is not something the ecosystem does). Keeping one
// convention across all three is the point — three predicates disagreeing about
// what "pinned" means is how the chart and render caches diverged before.
//
// The SHA predicates are ArgoCD's own, so "what looks like a commit" cannot drift
// from upstream's reading of it. That inherits upstream's shape exactly, including
// its edge: any hex-looking name of seven or more characters reads as a truncated
// SHA, so a branch called `deadbeef` or an all-digit date tag like `20260819` is
// treated as immutable. Accepted knowingly — it is the same movable-name risk the
// version-tag convention above already takes, and having the predicate BE
// upstream's is worth more than closing a shape nobody writes.
func IsImmutableGitRevision(revision string) bool {
	rev := strings.TrimSpace(revision)
	return argogit.IsCommitSHA(rev) || argogit.IsTruncatedCommitSHA(rev) || IsImmutableChartVersion(rev)
}

// chartCacheKey is the content-independent identity of a pinned remote chart:
// sha256(repoURL|chart|version). Because pinned versions are immutable this key
// maps 1:1 to chart contents.
func chartCacheKey(repoURL, chart, version string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(repoURL))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(chart))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(version))
	return hex.EncodeToString(h.Sum(nil))
}

// chartCachePaths returns the per-chart cache directory (the rename target of an
// atomic pull) and the unpacked chart directory inside it (what `helm template`
// is pointed at). The unpacked directory is named after the chart's base name,
// matching `helm pull --untar` behavior.
func chartCachePaths(baseDir, repoURL, chart, version string) (cacheDir, chartDir string) {
	cacheDir = filepath.Join(baseDir, chartCacheKey(repoURL, chart, version))
	chartDir = filepath.Join(cacheDir, filepath.Base(chart))
	return cacheDir, chartDir
}

// chartCacheDecision decides how to service a remote chart request given the
// configured base dir. It is pure (dirExists is injected) so the hit/miss
// layout can be tested without helm or a network. When enabled is false the
// caller must fall back to the always-fetch path. When enabled is true it
// returns the local unpacked chart dir and whether it is already present (hit);
// on a miss the caller pulls into cacheDir and then templates chartDir.
func chartCacheDecision(
	baseDir, repoURL, chart, version string,
	dirExists func(string) bool,
) (cacheDir, chartDir string, hit, enabled bool) {
	if baseDir == "" || !IsImmutableChartVersion(version) {
		return "", "", false, false
	}
	cacheDir, chartDir = chartCachePaths(baseDir, repoURL, chart, version)
	return cacheDir, chartDir, dirExists(chartDir), true
}
