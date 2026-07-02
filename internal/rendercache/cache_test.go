package rendercache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// seedEntry writes a cache file of the given size with the given mtime.
func seedEntry(t *testing.T, dir, name string, size int, mtime time.Time) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		t.Fatalf("seed %s: %v", name, err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", name, err)
	}
	return p
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func TestGCEvictsByAge(t *testing.T) {
	dir := t.TempDir()
	c, err := New(dir, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	now := time.Now()
	old := seedEntry(t, dir, "old.json", 10, now.Add(-48*time.Hour))
	fresh := seedEntry(t, dir, "fresh.json", 10, now.Add(-1*time.Hour))

	removed, err := c.GC(24*time.Hour, 0)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if exists(old) {
		t.Error("expected old entry to be evicted")
	}
	if !exists(fresh) {
		t.Error("expected fresh entry to be kept")
	}
}

func TestGCEvictsBySizeOldestFirst(t *testing.T) {
	dir := t.TempDir()
	c, err := New(dir, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	now := time.Now()
	oldest := seedEntry(t, dir, "a.json", 100, now.Add(-3*time.Hour))
	middle := seedEntry(t, dir, "b.json", 100, now.Add(-2*time.Hour))
	newest := seedEntry(t, dir, "c.json", 100, now.Add(-1*time.Hour))

	// Budget of 150 bytes: total is 300, so the two oldest (200 bytes) get
	// evicted until only the newest (100) remains within budget.
	removed, err := c.GC(0, 150)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}
	if exists(oldest) || exists(middle) {
		t.Error("expected the two oldest entries to be evicted")
	}
	if !exists(newest) {
		t.Error("expected the newest entry to be kept")
	}
}

func TestGCIgnoresNonEntryFiles(t *testing.T) {
	dir := t.TempDir()
	c, err := New(dir, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	now := time.Now()
	other := seedEntry(t, dir, "notes.txt", 10, now.Add(-72*time.Hour))
	json := seedEntry(t, dir, "old.json", 10, now.Add(-72*time.Hour))

	removed, err := c.GC(24*time.Hour, 0)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if !exists(other) {
		t.Error("expected non-.json file to be left untouched")
	}
	if exists(json) {
		t.Error("expected old .json entry to be evicted")
	}
}

func TestGCMissingDirIsNoOp(t *testing.T) {
	c := &Cache{dir: filepath.Join(t.TempDir(), "does-not-exist")}
	removed, err := c.GC(time.Hour, 100)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
}

func TestDirStats(t *testing.T) {
	dir := t.TempDir()
	seedEntry(t, dir, "a.json", 100, time.Now())
	sub := filepath.Join(dir, "charts", "x")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	seedEntry(t, sub, "Chart.yaml", 50, time.Now())

	entries, bytes, err := DirStats(dir)
	if err != nil {
		t.Fatalf("DirStats: %v", err)
	}
	if entries != 2 {
		t.Errorf("entries = %d, want 2", entries)
	}
	if bytes != 150 {
		t.Errorf("bytes = %d, want 150", bytes)
	}
}

func TestPutGetRoundtrip(t *testing.T) {
	c, err := New(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	want := &Entry{Manifests: []byte("apiVersion: v1\nkind: ConfigMap\n"), SourceType: "helm"}
	if err := c.Put("abc123", want); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, ok := c.Get("abc123")
	if !ok {
		t.Fatal("Get: expected hit, got miss")
	}
	if string(got.Manifests) != string(want.Manifests) {
		t.Errorf("Manifests = %q, want %q", got.Manifests, want.Manifests)
	}
	if got.SourceType != want.SourceType {
		t.Errorf("SourceType = %q, want %q", got.SourceType, want.SourceType)
	}
}

func TestGetMissingIsMiss(t *testing.T) {
	c, err := New(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := c.Get("does-not-exist"); ok {
		t.Fatal("Get: expected miss for absent key")
	}
}

func TestGetCorruptEntryIsMissAndEvicted(t *testing.T) {
	dir := t.TempDir()
	c, err := New(dir, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	p := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(p, []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}

	if _, ok := c.Get("corrupt"); ok {
		t.Fatal("Get: expected miss for corrupt entry")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("expected corrupt entry to be evicted, stat err = %v", err)
	}
}

func TestPutIsAtomicNoTempLeftBehind(t *testing.T) {
	dir := t.TempDir()
	c, err := New(dir, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Put("k", &Entry{Manifests: []byte("x"), SourceType: "kustomize"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 cache file, got %d", len(entries))
	}
	if entries[0].Name() != "k.json" {
		t.Errorf("cache file = %q, want k.json", entries[0].Name())
	}
}
