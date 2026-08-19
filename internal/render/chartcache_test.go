package render

import (
	"path/filepath"
	"testing"
)

// TestIsImmutableChartVersion pins the single exact-vs-mutable classification
// BOTH caches share. The range rows are the ones that used to disagree: the
// chart cache treated everything but ""/HEAD/* as pinned, so a constraint like
// ^2.0.0 was cached forever while the version it resolves to moved.
func TestIsImmutableChartVersion(t *testing.T) {
	exact := []string{"1.2.3", "v2.0.0", "0.3.1-rc.1", "1.2.3+build.7", " 1.0.0 "}
	mutable := []string{
		"", "HEAD", "*", // "latest" spellings
		"^2.0.0", "~1.2", ">=0.3.0", "1.x", "1.2.*", "1.2", // constraints helm resolves against the index
		"1.2.3 - 1.4.0", "1.2.3 || 2.0.0",
	}
	for _, v := range exact {
		if !IsImmutableChartVersion(v) {
			t.Errorf("IsImmutableChartVersion(%q) = false, want true", v)
		}
	}
	for _, v := range mutable {
		if IsImmutableChartVersion(v) {
			t.Errorf("IsImmutableChartVersion(%q) = true, want false", v)
		}
	}
}

func TestChartCacheKeyStableAndDistinct(t *testing.T) {
	base := chartCacheKey("https://charts.example.com", "nginx", "1.2.3")
	if base != chartCacheKey("https://charts.example.com", "nginx", "1.2.3") {
		t.Error("expected identical key for identical inputs")
	}
	// Each dimension must change the key.
	if base == chartCacheKey("https://other.example.com", "nginx", "1.2.3") {
		t.Error("expected different key when repo URL changes")
	}
	if base == chartCacheKey("https://charts.example.com", "redis", "1.2.3") {
		t.Error("expected different key when chart changes")
	}
	if base == chartCacheKey("https://charts.example.com", "nginx", "1.2.4") {
		t.Error("expected different key when version changes")
	}
}

func TestChartCachePaths(t *testing.T) {
	cacheDir, chartDir := chartCachePaths("/base", "https://charts.example.com", "nginx", "1.2.3")
	wantKey := chartCacheKey("https://charts.example.com", "nginx", "1.2.3")
	if cacheDir != filepath.Join("/base", wantKey) {
		t.Errorf("cacheDir = %q, want %q", cacheDir, filepath.Join("/base", wantKey))
	}
	if chartDir != filepath.Join("/base", wantKey, "nginx") {
		t.Errorf("chartDir = %q, want .../nginx", chartDir)
	}
}

func TestChartCacheDecision(t *testing.T) {
	present := map[string]bool{}
	dirExists := func(p string) bool { return present[p] }

	// Disabled: no base dir.
	if _, _, _, enabled := chartCacheDecision("", "repo", "nginx", "1.2.3", dirExists); enabled {
		t.Error("expected disabled when base dir is empty")
	}

	// Disabled: unpinned version.
	if _, _, _, enabled := chartCacheDecision("/base", "repo", "nginx", "HEAD", dirExists); enabled {
		t.Error("expected disabled for unpinned version")
	}

	// Enabled + miss (chart dir not present).
	cacheDir, chartDir, hit, enabled := chartCacheDecision("/base", "repo", "nginx", "1.2.3", dirExists)
	if !enabled {
		t.Fatal("expected enabled for pinned version and base dir")
	}
	if hit {
		t.Error("expected miss when chart dir is absent")
	}
	if cacheDir == "" || chartDir == "" {
		t.Error("expected non-empty cache/chart dirs")
	}

	// Enabled + hit (chart dir present).
	present[chartDir] = true
	_, _, hit2, _ := chartCacheDecision("/base", "repo", "nginx", "1.2.3", dirExists)
	if !hit2 {
		t.Error("expected hit when chart dir is present")
	}
}

// TestIsImmutableOCIRevision pins the artifact-revision predicate the render
// cache bypasses on. A digest is immutable by construction; an exact version tag
// is treated as immutable by the same convention pinned chart versions already
// rely on (they are registry tags too); everything a registry can re-point — a
// floating tag, a semver constraint, empty — is not.
func TestIsImmutableOCIRevision(t *testing.T) {
	immutable := []string{
		"sha256:c1e2d0d3f4a5b6978899aabbccddeeff00112233445566778899aabbccddeeff",
		"sha512:" + "ab12" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"6.7.0", "v6.7.0", "0.3.1-rc.1", "6.7.0+build.7", " 6.7.0 ",
	}
	for _, rev := range immutable {
		if !IsImmutableOCIRevision(rev) {
			t.Errorf("IsImmutableOCIRevision(%q) = false, want true", rev)
		}
	}

	mutable := []string{
		"", "latest", "main", "stable", "HEAD", "*", "^6.0.0", "~6.7", "6.x", "6.7", ">=6.0.0",
		// Digest-shaped but too short to be one, so not a digest — and not a
		// version either.
		"sha256:deadbeef",
	}
	for _, rev := range mutable {
		if IsImmutableOCIRevision(rev) {
			t.Errorf("IsImmutableOCIRevision(%q) = true, want false", rev)
		}
	}
}

// TestIsImmutableGitRevision pins the predicate the render cache uses for a
// source in another repository. A commit SHA (full or truncated) names one
// commit; an exact version tag is treated as fixed by the same convention pinned
// chart versions and OCI tags rely on; a branch name, HEAD or empty moves by
// design.
func TestIsImmutableGitRevision(t *testing.T) {
	immutable := []string{
		"0123456789abcdef0123456789abcdef01234567", // full SHA
		"0123456789ABCDEF0123456789abcdef01234567", // mixed case
		"0123456", // truncated SHA (7 is git's default)
		"6.7.0", "v6.7.0", "0.3.1-rc.1", " 6.7.0 ",
	}
	for _, rev := range immutable {
		if !IsImmutableGitRevision(rev) {
			t.Errorf("IsImmutableGitRevision(%q) = false, want true", rev)
		}
	}

	mutable := []string{"", "HEAD", "main", "master", "release-1", "v1-branch", "*", "6.7", "^6.0.0"}
	for _, rev := range mutable {
		if IsImmutableGitRevision(rev) {
			t.Errorf("IsImmutableGitRevision(%q) = true, want false", rev)
		}
	}
}
