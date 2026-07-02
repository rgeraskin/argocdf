package render

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
)

// isPinnedChartVersion reports whether a target revision names an immutable
// chart version safe to cache persistently. Empty, "HEAD" and "*" mean "latest"
// (mutable) and must always be re-fetched.
func isPinnedChartVersion(version string) bool {
	switch version {
	case "", "HEAD", "*":
		return false
	default:
		return true
	}
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
	if baseDir == "" || !isPinnedChartVersion(version) {
		return "", "", false, false
	}
	cacheDir, chartDir = chartCachePaths(baseDir, repoURL, chart, version)
	return cacheDir, chartDir, dirExists(chartDir), true
}
