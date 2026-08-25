package render

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// key64 builds a directory name in the ENTRY shape (what chartCacheKey emits)
// without hashing anything, so a test can say which entry it means.
func key64(t *testing.T, seed string) string {
	t.Helper()
	if len(seed) > 64 {
		t.Fatalf("seed %q is longer than a cache key", seed)
	}
	return seed + strings.Repeat("a", 64-len(seed))
}

// writeChartEntry creates one cache entry at rel below root holding a chart of
// size bytes, and stamps the ENTRY directory's mtime to age ago - the directory
// mtime being what GCChartCache reads, and what publishChartToCache's
// stage-then-rename leaves at the publish time.
func writeChartEntry(t *testing.T, root, rel string, size int, age time.Duration) string {
	t.Helper()
	entry := filepath.Join(root, rel)
	chart := filepath.Join(entry, "nginx", "templates")
	if err := os.MkdirAll(chart, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chart, "deployment.yaml"), make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-age)
	if err := os.Chtimes(entry, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	return entry
}

func mustExist(t *testing.T, path, why string) {
	t.Helper()
	if !dirExists(path) {
		t.Errorf("%s was removed: %s", path, why)
	}
}

func mustBeGone(t *testing.T, path, why string) {
	t.Helper()
	if dirExists(path) {
		t.Errorf("%s survived: %s", path, why)
	}
}

// TestGCChartCacheEvictsByAge: the first pass drops every entry older than
// maxAge, wherever it sits, and a non-positive maxAge disables the pass.
func TestGCChartCacheEvictsByAge(t *testing.T) {
	root := t.TempDir()
	old := writeChartEntry(t, root, filepath.Join("cluster", key64(t, "01d")), 10, 40*24*time.Hour)
	fresh := writeChartEntry(t, root, filepath.Join("cluster", key64(t, "02f")), 10, time.Hour)

	removed, err := GCChartCache(root, 30*24*time.Hour, 0)
	if err != nil {
		t.Fatalf("GCChartCache() error = %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	mustBeGone(t, old, "40 days old under a 30-day maxAge")
	mustExist(t, fresh, "an hour old - a run must never evict what it just downloaded")

	// A non-positive maxAge disables the pass entirely.
	older := writeChartEntry(t, root, filepath.Join("cluster", key64(t, "03d")), 10, 400*24*time.Hour)
	if removed, err := GCChartCache(root, 0, 0); err != nil || removed != 0 {
		t.Errorf("GCChartCache(maxAge=0) = (%d, %v), want (0, nil)", removed, err)
	}
	mustExist(t, older, "maxAge=0 disables age eviction")
}

// TestGCChartCacheEvictsBySizeOldestFirst: the second pass removes the OLDEST
// entries and stops as soon as the total fits, which is the property a
// remove-everything-over-budget loop would also satisfy - so the youngest entry
// staying put is the assertion that matters.
func TestGCChartCacheEvictsBySizeOldestFirst(t *testing.T) {
	root := t.TempDir()
	scope := filepath.Join("cluster", "server-1234abcd")
	oldest := writeChartEntry(t, root, filepath.Join(scope, key64(t, "01")), 1000, 3*time.Hour)
	middle := writeChartEntry(t, root, filepath.Join(scope, key64(t, "02")), 1000, 2*time.Hour)
	newest := writeChartEntry(t, root, filepath.Join(scope, key64(t, "03")), 1000, time.Hour)

	// 3000 bytes on disk, budget 2500: evicting the oldest alone brings the
	// total to 2000, so the loop must stop there.
	removed, err := GCChartCache(root, 0, 2500)
	if err != nil {
		t.Fatalf("GCChartCache() error = %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1 (one eviction takes the total within budget)", removed)
	}
	mustBeGone(t, oldest, "oldest entry, evicted first")
	mustExist(t, middle, "the budget was met before it - a second eviction is waste")
	mustExist(t, newest, "newest entry")

	// A non-positive maxBytes disables the pass.
	if removed, err := GCChartCache(root, 0, 0); err != nil || removed != 0 {
		t.Errorf("GCChartCache(maxBytes=0) = (%d, %v), want (0, nil)", removed, err)
	}
	mustExist(t, middle, "maxBytes=0 disables size eviction")
}

// TestGCChartCacheSweepsEveryLayout is the reason entries are detected by SHAPE:
// the two layouts that predate the credential-source scoping are never read
// again, so nothing but the GC can ever reclaim them - and the GC only can
// because it does not know which layout it is looking at.
func TestGCChartCacheSweepsEveryLayout(t *testing.T) {
	root := t.TempDir()
	legacyFlat := writeChartEntry(t, root, key64(t, "01"), 10, 40*24*time.Hour)
	legacyMode := writeChartEntry(t, root, filepath.Join("cluster", key64(t, "02")), 10, 40*24*time.Hour)
	scoped := writeChartEntry(t, root,
		filepath.Join("cluster", "https_10.0.0.5_6443_argocd-deadbeef", key64(t, "03")), 10, 40*24*time.Hour)
	local := writeChartEntry(t, root, filepath.Join("local", key64(t, "04")), 10, 40*24*time.Hour)

	removed, err := GCChartCache(root, 30*24*time.Hour, 0)
	if err != nil {
		t.Fatalf("GCChartCache() error = %v", err)
	}
	if removed != 4 {
		t.Errorf("removed = %d, want 4 (every layout)", removed)
	}
	for _, p := range []string{legacyFlat, legacyMode, scoped, local} {
		mustBeGone(t, p, "an aged-out entry, whatever layout wrote it")
	}
}

// TestGCChartCacheLeavesNonEntriesAlone: everything the GC does is keyed on the
// entry shape, so a directory that merely SITS at entry depth - a scope, or
// anything a future layout adds - must survive, along with its files.
func TestGCChartCacheLeavesNonEntriesAlone(t *testing.T) {
	root := t.TempDir()
	entry := writeChartEntry(t, root, filepath.Join("cluster", key64(t, "01")), 10, 40*24*time.Hour)

	// A non-entry directory beside the entry, equally old.
	sibling := filepath.Join(root, "cluster", "not-a-key")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	loose := filepath.Join(root, "cluster", "README")
	if err := os.WriteFile(loose, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-400 * 24 * time.Hour)
	for _, p := range []string{sibling, loose} {
		if err := os.Chtimes(p, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := GCChartCache(root, 30*24*time.Hour, 1)
	if err != nil {
		t.Fatalf("GCChartCache() error = %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1 (the entry only)", removed)
	}
	mustBeGone(t, entry, "aged-out entry")
	mustExist(t, sibling, "not an entry - the GC owns entries, not the tree around them")
	if _, err := os.Stat(loose); err != nil {
		t.Errorf("loose file removed: %v", err)
	}
}

// TestGCChartCacheEntryShape pins the boundary of the shape rule in both
// directions: a near-miss must never be deleted (it is not argocdf's to delete),
// and the exact shape must be, or the legacy sweep silently does nothing.
func TestGCChartCacheEntryShape(t *testing.T) {
	tooShort := strings.Repeat("a", 63)
	tooLong := strings.Repeat("a", 65)
	upper := strings.Repeat("A", 64)
	mixed := strings.Repeat("a", 63) + "F"
	nonHex := strings.Repeat("a", 63) + "g"
	exact := strings.Repeat("0123456789abcdef", 4)

	for _, name := range []string{tooShort, tooLong, upper, mixed, nonHex} {
		if isChartCacheEntryDir(name) {
			t.Errorf("isChartCacheEntryDir(%q...) = true, want false (not a chartCacheKey)", name[:8])
		}
	}
	if !isChartCacheEntryDir(exact) {
		t.Error("isChartCacheEntryDir() rejected an exact 64-char lowercase hex name")
	}

	// And end to end, since the predicate could be right while the walk is not.
	root := t.TempDir()
	var survivors []string
	for _, name := range []string{tooShort, tooLong, upper, mixed, nonHex} {
		survivors = append(survivors, writeChartEntry(t, root, name, 10, 400*24*time.Hour))
	}
	victim := writeChartEntry(t, root, exact, 10, 400*24*time.Hour)

	removed, err := GCChartCache(root, 30*24*time.Hour, 0)
	if err != nil {
		t.Fatalf("GCChartCache() error = %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1 (only the exact-shape directory)", removed)
	}
	mustBeGone(t, victim, "exactly 64 lowercase hex characters")
	for _, p := range survivors {
		mustExist(t, p, "a near-miss on the key shape is not a cache entry")
	}
}

// TestGCChartCacheSweepsOrphanedStagingDirs: publishChartToCache stages beside
// the entry it claims, and its cleanup is SafeRemoveAll, which refuses any path
// outside os.TempDir() - so a failed publish under the real cache dir leaves the
// staging directory behind forever. The GC is the only thing that reclaims it,
// and it must do so on AGE, or it would delete a concurrent argocdf's in-flight
// copy.
func TestGCChartCacheSweepsOrphanedStagingDirs(t *testing.T) {
	root := t.TempDir()
	scope := filepath.Join(root, "cluster")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}

	// Real orphans: created exactly as publishChartToCache creates them.
	stale, err := os.MkdirTemp(scope, chartStagingPrefix+"*"+chartStagingSuffix)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "Chart.yaml"), []byte("name: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(stale, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	inFlight, err := os.MkdirTemp(scope, chartStagingPrefix+"*"+chartStagingSuffix)
	if err != nil {
		t.Fatal(err)
	}

	removed, err := GCChartCache(root, 30*24*time.Hour, 0)
	if err != nil {
		t.Fatalf("GCChartCache() error = %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1 (the stale staging dir)", removed)
	}
	mustBeGone(t, stale, "an orphaned staging dir older than maxAge")
	mustExist(t, inFlight, "a fresh staging dir may belong to a concurrent publish")

	// maxAge=0 disables the sweep along with age eviction.
	if err := os.Chtimes(inFlight, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if removed, err := GCChartCache(root, 0, 0); err != nil || removed != 0 {
		t.Errorf("GCChartCache(maxAge=0) = (%d, %v), want (0, nil)", removed, err)
	}
	mustExist(t, inFlight, "maxAge=0 disables the staging sweep too")
}

// TestGCChartCacheMissingRoot: a machine that has never downloaded a chart has
// no charts/ at all, and that is not a failure to report on every run.
func TestGCChartCacheMissingRoot(t *testing.T) {
	removed, err := GCChartCache(filepath.Join(t.TempDir(), "never-created"), 30*24*time.Hour, 1<<30)
	if err != nil {
		t.Errorf("GCChartCache(missing root) error = %v, want nil", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
}

// TestChartEntryMtimeIsThePublishTime pins the clock the GC reads. Age eviction
// is only meaningful if the entry directory's mtime says when the chart was
// downloaded, and that rests on two things worth stating: a rename does not
// restamp the directory it moves (so the mtime survives the atomic claim), and
// publishChartToCache therefore leaves the entry stamped at the moment its copy
// finished. If a future publish path stopped preserving it - copying into place
// instead of renaming, say - every entry would look freshly written and nothing
// would ever age out.
func TestChartEntryMtimeIsThePublishTime(t *testing.T) {
	// A rename carries the directory's own mtime across.
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(src, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(tmp, "dst")
	if err := os.Rename(src, dst); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(stamp) {
		t.Errorf("rename restamped the directory: mtime = %s, want %s", info.ModTime(), stamp)
	}

	// And a real publish lands an entry stamped now, not at some earlier time.
	extracted := filepath.Join(t.TempDir(), "nginx")
	if err := os.MkdirAll(extracted, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extracted, "Chart.yaml"), []byte("name: nginx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cacheDir, chartDir := chartCachePaths(filepath.Join(root, "cluster"), "https://charts.example.com", "nginx", "1.2.3")
	before := time.Now().Add(-2 * time.Second)
	if !publishChartToCache(extracted, cacheDir, chartDir) {
		t.Fatal("publishChartToCache() = false")
	}
	entry, err := os.Stat(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if entry.ModTime().Before(before) {
		t.Errorf("published entry mtime = %s, older than the publish itself (%s)", entry.ModTime(), before)
	}
	// The GC must therefore never touch it under the shipped 30-day bound.
	if removed, err := GCChartCache(root, 30*24*time.Hour, 0); err != nil || removed != 0 {
		t.Errorf("GCChartCache() = (%d, %v) right after a publish, want (0, nil)", removed, err)
	}
	mustExist(t, cacheDir, "just published")
}

// TestGCChartCacheEntrySizeIsItsFiles: the size budget is charged the chart's
// bytes, summed recursively, and only for regular files.
func TestGCChartCacheEntrySizeIsItsFiles(t *testing.T) {
	root := t.TempDir()
	entry := writeChartEntry(t, root, key64(t, "01"), 700, time.Hour)
	// A second file deeper in the same entry, plus a symlink that must not be
	// counted (copyDir recreates links; following one could count bytes twice).
	if err := os.WriteFile(filepath.Join(entry, "nginx", "values.yaml"), make([]byte, 300), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(entry, "nginx", "values.yaml"), filepath.Join(entry, "link.yaml")); err != nil {
		t.Fatal(err)
	}

	if got := chartEntrySize(entry); got != 1000 {
		t.Errorf("chartEntrySize() = %d, want 1000 (regular files only, recursive)", got)
	}
	// 1000 bytes against a 1001-byte budget must survive; against 999 it goes.
	if removed, err := GCChartCache(root, 0, 1001); err != nil || removed != 0 {
		t.Errorf("GCChartCache(maxBytes=1001) = (%d, %v), want (0, nil)", removed, err)
	}
	mustExist(t, entry, "within budget")
	if removed, err := GCChartCache(root, 0, 999); err != nil || removed != 1 {
		t.Errorf("GCChartCache(maxBytes=999) = (%d, %v), want (1, nil)", removed, err)
	}
	mustBeGone(t, entry, "over budget")
}
