package render

import (
	"path/filepath"
	"testing"
)

func TestIsPinnedChartVersion(t *testing.T) {
	tests := []struct {
		version string
		want    bool
	}{
		{"1.2.3", true},
		{"v2.0.0", true},
		{"", false},
		{"HEAD", false},
		{"*", false},
	}
	for _, tt := range tests {
		if got := isPinnedChartVersion(tt.version); got != tt.want {
			t.Errorf("isPinnedChartVersion(%q) = %v, want %v", tt.version, got, tt.want)
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
