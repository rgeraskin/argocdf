package app

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/log"

	"github.com/rgeraskin/argocdf/internal/config"
)

// TestCreateRenderFactoryErrorReturnsUntypedNil pins the interface-nil
// contract: on a construction failure the returned applicationRenderer must
// be a true nil interface, not a typed-nil *ArgoCDRenderer wrapped in a
// non-nil interface — Run's deferred cleanup type-asserts the stored value
// and would otherwise call Cleanup on a nil receiver and panic during error
// unwinding.
func TestCreateRenderFactoryErrorReturnsUntypedNil(t *testing.T) {
	// The argocd engine's first construction step creates its registry auth
	// dir under os.TempDir(); pointing TMPDIR at a missing path forces the
	// failure without any stubbing.
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))

	f := NewFactory(&config.Config{Renderer: config.RendererArgoCD}, log.New(io.Discard))
	r, err := f.CreateRenderFactory("v1.30.0", nil, nil)
	if err == nil {
		t.Fatal("CreateRenderFactory() succeeded; the TMPDIR trick no longer forces a construction failure")
	}
	if r != nil {
		t.Fatalf("CreateRenderFactory() returned a non-nil interface (%T) alongside the error; Run's deferred Cleanup would panic on the typed nil", r)
	}
}

// TestRunCleansUpRendererOnInitializeFailure pins the Run lifecycle contract:
// when initialize fails AFTER the argocd engine was constructed (here: output
// writer creation), the deferred cleanup still runs — the per-run registry
// auth dir is removed and the scrubbed helm env is restored. The test runs
// fully offline: client construction never dials, the explicit KubeVersion
// skips cluster detection, and NoAPIVersions skips API discovery.
func TestRunCleansUpRendererOnInitializeFailure(t *testing.T) {
	// Sandbox the engine's temp artifacts so leftovers are attributable to
	// this run (os.TempDir re-reads TMPDIR on every call).
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("HELM_REGISTRY_CONFIG", "/sentinel/registry.json")

	kubeconfig := filepath.Join(tmp, "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
clusters:
- cluster: {server: "https://127.0.0.1:1"}
  name: offline
contexts:
- context: {cluster: offline}
  name: offline
current-context: offline
`), 0o600); err != nil {
		t.Fatal(err)
	}

	repoDir := t.TempDir()
	if out, err := exec.Command("git", "init", repoDir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	cfg := &config.Config{
		KubeconfigPath: kubeconfig,
		RepoPath:       repoDir,
		KubeVersion:    "v1.30.0",
		NoAPIVersions:  true,
		NoCache:        true,
		Renderer:       config.RendererArgoCD,
		RepoCreds:      config.RepoCredsNone,
		StdoutFormat:   "none",
		// Writer creation fails AFTER the renderer exists: the parent
		// directory of the output file is missing.
		FileOutputs: []config.FileOutput{{Format: "md-fields", Path: filepath.Join(tmp, "missing-dir", "out.md")}},
	}
	logger := log.New(io.Discard)

	a, err := New(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	err = a.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "initialization failed") {
		t.Fatalf("Run() error = %v, want an initialization failure from the output writer", err)
	}

	if got := os.Getenv("HELM_REGISTRY_CONFIG"); got != "/sentinel/registry.json" {
		t.Errorf("HELM_REGISTRY_CONFIG = %q after failed Run, want the inherited value restored (renderer Cleanup did not run)", got)
	}
	leftovers, _ := filepath.Glob(filepath.Join(tmp, "argocdf-registry-*"))
	if len(leftovers) != 0 {
		t.Errorf("registry auth dir leaked after failed Run: %v", leftovers)
	}
}
