package app

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/log"

	"github.com/rgeraskin/argocdf/internal/cluster"
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

// TestCreateLintRunner covers the cluster-selector propagation contract: lint
// commands learn the effective context and kubeconfig, and never see an empty
// value for either.
func TestCreateLintRunner(t *testing.T) {
	tests := []struct {
		name        string
		cfg         *config.Config
		kubeContext string
		wantNil     bool
		wantEnv     map[string]string
	}{
		{
			name:        "no linter of any kind means no runner",
			cfg:         &config.Config{KubeconfigPath: "/home/u/.kube/config"},
			kubeContext: "prod",
			wantNil:     true,
		},
		{
			// The built-in adapters must construct a runner on their own: gating
			// only on Lint would make --lint-kyverno silently do nothing.
			name:        "a built-in adapter alone still builds a runner",
			cfg:         &config.Config{LintKyverno: []string{"policies/kyverno"}, KubeconfigPath: "/home/u/.kube/config"},
			kubeContext: "prod",
			wantEnv: map[string]string{
				"ARGOCDF_CONTEXT":    "prod",
				"ARGOCDF_KUBECONFIG": "/home/u/.kube/config",
			},
		},
		{
			name:        "conftest alone also builds a runner",
			cfg:         &config.Config{LintConftest: []string{"policies/conftest"}},
			kubeContext: "prod",
			wantEnv:     map[string]string{"ARGOCDF_CONTEXT": "prod"},
		},
		{
			name:        "both selectors are exported",
			cfg:         &config.Config{Lint: []string{"true"}, LintTimeout: 7 * time.Second, KubeconfigPath: "/home/u/.kube/config"},
			kubeContext: "prod",
			wantEnv: map[string]string{
				"ARGOCDF_CONTEXT":    "prod",
				"ARGOCDF_KUBECONFIG": "/home/u/.kube/config",
			},
		},
		{
			name:        "a kubeconfig list is passed verbatim",
			cfg:         &config.Config{Lint: []string{"true"}, KubeconfigPath: "/a/config:/b/config"},
			kubeContext: "prod",
			wantEnv: map[string]string{
				"ARGOCDF_CONTEXT":    "prod",
				"ARGOCDF_KUBECONFIG": "/a/config:/b/config",
			},
		},
		{
			name:        "an unresolvable context exports nothing for that key",
			cfg:         &config.Config{Lint: []string{"true"}, KubeconfigPath: "/home/u/.kube/config"},
			kubeContext: "",
			wantEnv:     map[string]string{"ARGOCDF_KUBECONFIG": "/home/u/.kube/config"},
		},
		{
			name:        "an auto-detected kubeconfig exports nothing for that key",
			cfg:         &config.Config{Lint: []string{"true"}},
			kubeContext: "prod",
			wantEnv:     map[string]string{"ARGOCDF_CONTEXT": "prod"},
		},
		{
			name:        "nothing to export leaves the environment inherited",
			cfg:         &config.Config{Lint: []string{"true"}},
			kubeContext: "",
			wantEnv:     map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewFactory(tt.cfg, log.New(io.Discard)).CreateLintRunner(tt.kubeContext)
			if tt.wantNil {
				if r != nil {
					t.Fatalf("CreateLintRunner() = %+v, want nil without --lint commands", r)
				}
				return
			}
			if r == nil {
				t.Fatal("CreateLintRunner() = nil, want a runner")
			}
			if !reflect.DeepEqual(r.Commands, tt.cfg.Lint) {
				t.Errorf("Commands = %v, want %v", r.Commands, tt.cfg.Lint)
			}
			if r.Timeout != tt.cfg.LintTimeout {
				t.Errorf("Timeout = %s, want %s", r.Timeout, tt.cfg.LintTimeout)
			}
			if !reflect.DeepEqual(r.Env, tt.wantEnv) {
				t.Errorf("Env = %v, want %v", r.Env, tt.wantEnv)
			}
			if !reflect.DeepEqual(r.Kyverno, tt.cfg.LintKyverno) {
				t.Errorf("Kyverno = %v, want %v", r.Kyverno, tt.cfg.LintKyverno)
			}
			if !reflect.DeepEqual(r.Conftest, tt.cfg.LintConftest) {
				t.Errorf("Conftest = %v, want %v", r.Conftest, tt.cfg.LintConftest)
			}
			// The built-in adapters read these fields, not Env: a call site that
			// wired only the environment would leave them linting the ambient
			// cluster while every Env assertion above still passed.
			if r.KubeContext != tt.kubeContext {
				t.Errorf("KubeContext = %q, want %q", r.KubeContext, tt.kubeContext)
			}
			if r.Kubeconfig != tt.cfg.KubeconfigPath {
				t.Errorf("Kubeconfig = %q, want %q", r.Kubeconfig, tt.cfg.KubeconfigPath)
			}
		})
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

// TestCreateLintRunnerWiring pins the CLIENT-to-RUNNER wiring, which the
// factory test alone cannot cover: it takes the context as an argument, so a
// call site handing it "" (or the wrong value) would still satisfy every other
// test while cluster-aware lint adapters silently fell back to the invoking
// shell's cluster. Building a cluster client never dials, so a temp kubeconfig
// is enough to exercise the real types.
func TestCreateLintRunnerWiring(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
clusters:
- cluster: {server: "https://127.0.0.1:1"}
  name: offline
contexts:
- context: {cluster: offline}
  name: ambient-context
- context: {cluster: offline}
  name: diffed-context
current-context: ambient-context
`), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		contextFlag string
		lint        []string
		wantContext string
		wantNil     bool
	}{
		{
			name:        "explicit context reaches the runner",
			contextFlag: "diffed-context",
			lint:        []string{"true"},
			wantContext: "diffed-context",
		},
		{
			name:        "without --context the kubeconfig's current-context reaches the runner",
			lint:        []string{"true"},
			wantContext: "ambient-context",
		},
		{
			name:        "no --lint commands: no runner at all",
			contextFlag: "diffed-context",
			wantNil:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				KubeconfigPath: kubeconfig,
				Context:        tt.contextFlag,
				Lint:           tt.lint,
				LintTimeout:    5 * time.Second,
			}
			logger := log.New(io.Discard)
			a, err := New(cfg, logger)
			if err != nil {
				t.Fatal(err)
			}
			a.kubeClient, err = cluster.NewClient(cfg.KubeconfigPath, cfg.Context)
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}

			runner := a.createLintRunner()
			if tt.wantNil {
				if runner != nil {
					t.Fatalf("createLintRunner() = %+v, want nil without --lint", runner)
				}
				return
			}
			if runner == nil {
				t.Fatal("createLintRunner() = nil, want a runner")
			}
			if got := runner.Env["ARGOCDF_CONTEXT"]; got != tt.wantContext {
				t.Errorf("ARGOCDF_CONTEXT = %q, want %q (the context the client connected with)", got, tt.wantContext)
			}
			if got := runner.Env["ARGOCDF_KUBECONFIG"]; got != kubeconfig {
				t.Errorf("ARGOCDF_KUBECONFIG = %q, want %q", got, kubeconfig)
			}
		})
	}
}

// TestCreateLintRunnerWithoutClient pins that the wiring tolerates a missing
// client (no cluster connection yet) instead of panicking: the context is then
// simply unknown and nothing is exported for it.
func TestCreateLintRunnerWithoutClient(t *testing.T) {
	cfg := &config.Config{Lint: []string{"true"}, LintTimeout: time.Second}
	a, err := New(cfg, log.New(io.Discard))
	if err != nil {
		t.Fatal(err)
	}

	runner := a.createLintRunner()
	if runner == nil {
		t.Fatal("createLintRunner() = nil, want a runner")
	}
	if got, ok := runner.Env["ARGOCDF_CONTEXT"]; ok {
		t.Errorf("ARGOCDF_CONTEXT = %q, want it absent when no client resolved a context", got)
	}
}
