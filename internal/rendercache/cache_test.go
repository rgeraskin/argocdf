package rendercache

import (
	"os"
	"path/filepath"
	"testing"
)

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
