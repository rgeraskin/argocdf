package render

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"regexp"
	"strings"
)

// immutableChartVersionRe matches a single exact semver version (optional v
// prefix, optional prerelease/build metadata). Anything else - operators
// (^ ~ > <), wildcards (x, *), hyphen ranges, ORs, partial versions like
// "1.2", HEAD, empty - is a CONSTRAINT that helm resolves against the mutable
// repository index, so the version it selects can move between runs.
var immutableChartVersionRe = regexp.MustCompile(`^v?\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

// IsImmutableChartVersion reports whether a chart target revision names ONE
// exact, immutable version. It is the single predicate both caches share: the
// chart-download cache below only persists immutable versions, and the render
// cache (rendercache.ComputeKey) bypasses itself for a remote chart whose
// revision is mutable - a "HEAD", "*" or "^2.0.0" chart can resolve to
// different content over time under identical key inputs, so a hit could
// serve manifests from a chart that no longer exists. Two predicates is how
// the caches previously DISAGREED: the chart cache treated only ""/HEAD/* as
// mutable (so a range like ^2.0.0 was wrongly cached forever), while the
// render cache keyed the literal revision string and never bypassed at all.
func IsImmutableChartVersion(version string) bool {
	return immutableChartVersionRe.MatchString(strings.TrimSpace(version))
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
