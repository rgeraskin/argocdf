package render

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

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

// chartCacheEntryRe matches a chart-cache ENTRY directory name: the 64
// lowercase hex characters chartCacheKey produces. The shape IS the identity —
// nothing else argocdf writes under charts/ is named like a sha256 — which is
// what lets the GC below recognize an entry at any depth, and therefore sweep
// the LEGACY layouts (charts/<key>/ and charts/cluster/<key>/, written before
// the credential-source scoping existed) without knowing they ever existed.
var chartCacheEntryRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// isChartCacheEntryDir reports whether a directory NAME is a chart cache entry.
func isChartCacheEntryDir(name string) bool {
	return chartCacheEntryRe.MatchString(name)
}

// Orphaned staging directories: publishChartToCache stages a chart in
// os.MkdirTemp(parent, "argocdf-chart-*.tmp") — a SIBLING of the entry it is
// about to claim by rename — so a staging directory can outlive the process
// that made it. Two ways: the publish fails (a copy error, or a lost rename
// race against a concurrent publisher), in which case its deferred cleanup is
// SafeRemoveAll, which refuses any path outside os.TempDir() and the chart
// cache is not there; or argocdf is killed mid-copy. Either way nothing else
// ever looks at the directory again, so the GC sweeps it on age like an entry.
// A successful publish renames the staging directory INTO place and leaves
// nothing behind.
const (
	chartStagingPrefix = "argocdf-chart-"
	chartStagingSuffix = ".tmp"
)

// isChartStagingDir reports whether a directory NAME has publishChartToCache's
// staging shape. It cannot collide with an entry (64 hex characters contain no
// dash or dot) nor with the unpacked chart inside one (the walk never descends
// into an entry).
func isChartStagingDir(name string) bool {
	return strings.HasPrefix(name, chartStagingPrefix) && strings.HasSuffix(name, chartStagingSuffix)
}

// GCChartCache bounds the persistent chart download cache, mirroring
// rendercache.Cache.GC pass for pass. root is the chart cache ROOT
// (<cache dir>/charts), the common parent of every credential-source scope.
//
// An ENTRY is any directory whose base name is exactly 64 lowercase hex
// characters — one chartCacheKey, holding one unpacked chart — found at ANY
// depth below root. The walk does not descend into one: its content is the
// chart. Detecting entries by SHAPE rather than by the layout that produced
// them is what makes this sweep the current scoped tree
// (charts/<mode>/[<instance>/]<key>/) and both LEGACY layouts (charts/<key>/
// and charts/cluster/<key>/, written before the scoping existed and never read
// again) under one rule. An entry's age is its directory's mtime, which is the
// time it was published: publishChartToCache fills a staging sibling and
// renames it into place, and a rename does not restamp the directory it moves
// (both halves pinned by TestChartEntryMtimeIsThePublishTime, since a publish
// path that stopped preserving it would leave every entry looking fresh and
// nothing would ever age out). A cache HIT does not touch it, so age here means
// "downloaded long ago", not "unused for a while". An entry's size is the sum
// of the regular files inside it.
//
// Eviction runs in two passes, exactly like the render cache: first every entry
// older than maxAge, then — if what remains still exceeds maxBytes in total —
// the oldest entries by mtime until the total is within budget. A non-positive
// maxAge disables age-based eviction; a non-positive maxBytes disables
// size-based eviction. Orphaned staging directories left beside entries by an
// interrupted or losing publish (see isChartStagingDir) are swept on the same
// age rule, so an in-flight publish from a concurrent argocdf is never touched;
// they are not counted toward the size budget, since they belong to no entry.
//
// removed counts the directories actually deleted, entries and swept staging
// directories alike. GC is best-effort throughout: a missing root reports
// (0, nil), an unreadable subtree is skipped rather than abandoning the sweep,
// a directory that will not delete is simply kept, and anything that is neither
// an entry nor a staging directory is left alone.
func GCChartCache(root string, maxAge time.Duration, maxBytes int64) (removed int, err error) {
	type cacheEntry struct {
		path    string
		size    int64
		modTime time.Time
	}

	var (
		entries []cacheEntry
		staging []string
		cutoff  = time.Now().Add(-maxAge)
	)

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// The root itself is the caller's problem (a missing cache is not an
			// error, see below); anything deeper is skipped so one unreadable
			// scope cannot cost the whole sweep.
			if path == root {
				return err
			}
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		switch {
		case isChartCacheEntryDir(d.Name()):
			info, ierr := d.Info()
			if ierr != nil {
				return fs.SkipDir
			}
			entries = append(entries, cacheEntry{
				path:    path,
				size:    chartEntrySize(path),
				modTime: info.ModTime(),
			})
			return fs.SkipDir
		case isChartStagingDir(d.Name()):
			info, ierr := d.Info()
			if ierr == nil && maxAge > 0 && info.ModTime().Before(cutoff) {
				staging = append(staging, path)
			}
			return fs.SkipDir
		}
		return nil
	})
	if walkErr != nil {
		if os.IsNotExist(walkErr) {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to walk chart cache %s: %w", root, walkErr)
	}

	// Age-based eviction.
	if maxAge > 0 {
		kept := entries[:0:0]
		for _, e := range entries {
			if e.modTime.Before(cutoff) {
				if rmErr := os.RemoveAll(e.path); rmErr == nil {
					removed++
					continue
				}
			}
			kept = append(kept, e)
		}
		entries = kept
	}

	// Size-based eviction: oldest first until within budget.
	if maxBytes > 0 {
		var total int64
		for _, e := range entries {
			total += e.size
		}
		if total > maxBytes {
			sort.Slice(entries, func(i, j int) bool {
				return entries[i].modTime.Before(entries[j].modTime)
			})
			for _, e := range entries {
				if total <= maxBytes {
					break
				}
				if rmErr := os.RemoveAll(e.path); rmErr == nil {
					removed++
					total -= e.size
				}
			}
		}
	}

	for _, p := range staging {
		if rmErr := os.RemoveAll(p); rmErr == nil {
			removed++
		}
	}

	return removed, nil
}

// chartEntrySize sums the regular files inside one cache entry. Symlinks are
// not followed and not counted: copyDir recreates them as links, so their
// target's bytes either already count inside the entry or live outside it. An
// unreadable subtree contributes what could be read — undercounting a size
// budget is the harmless direction.
func chartEntrySize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}
