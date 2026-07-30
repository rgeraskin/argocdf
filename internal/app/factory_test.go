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

// TestCreateRendererErrorReturnsUntypedNil pins the interface-nil
// contract: on a construction failure the returned applicationRenderer must
// be a true nil interface, not a typed-nil *ArgoCDRenderer wrapped in a
// non-nil interface — Run's deferred cleanup type-asserts the stored value
// and would otherwise call Cleanup on a nil receiver and panic during error
// unwinding.
func TestCreateRendererErrorReturnsUntypedNil(t *testing.T) {
	// The render engine's first construction step creates its registry auth
	// dir under os.TempDir(); pointing TMPDIR at a missing path forces the
	// failure without any stubbing.
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))

	f := NewFactory(&config.Config{}, log.New(io.Discard))
	r, err := f.CreateRenderer("v1.30.0", nil, nil, "")
	if err == nil {
		t.Fatal("CreateRenderer() succeeded; the TMPDIR trick no longer forces a construction failure")
	}
	if r != nil {
		t.Fatalf("CreateRenderer() returned a non-nil interface (%T) alongside the error; Run's deferred Cleanup would panic on the typed nil", r)
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
// when initialize fails AFTER the render engine was constructed (here: output
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
		NoCache:        config.NoCacheAll,
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

// TestChartCacheDirScopedByCredentialSource: the chart cache is keyed by chart and
// version, which are credential-independent - so without a per-source directory a
// chart downloaded with local credentials would satisfy a run whose purpose is
// checking that the CLUSTER's credentials can fetch it. The render cache keys on the
// mode for the same reason; a miss there would otherwise be followed by a fetch that
// never happened.
func TestChartCacheDirScopedByCredentialSource(t *testing.T) {
	base := t.TempDir()
	logger := log.New(nil)
	logger.SetLevel(log.FatalLevel)

	dirFor := func(mode, noCache, instance string) string {
		f := NewFactory(&config.Config{
			CacheDir:  base,
			RepoCreds: mode,
			NoCache:   noCache,
		}, logger)
		return f.chartCacheDir(instance)
	}

	cluster := dirFor(config.RepoCredsCluster, config.NoCacheNone, "")
	local := dirFor(config.RepoCredsLocal, config.NoCacheNone, "")
	none := dirFor(config.RepoCredsNone, config.NoCacheNone, "")

	if cluster == local || cluster == none || local == none {
		t.Errorf("credential sources share a chart cache dir: cluster=%q local=%q none=%q", cluster, local, none)
	}
	if filepath.Base(cluster) != config.RepoCredsCluster {
		t.Errorf("chart cache dir %q is not scoped by the credential source", cluster)
	}
	// An unset source must not produce a differently-scoped directory than the
	// default it resolves to, or a run before WithDefaults would use its own cache.
	if dirFor("", config.NoCacheNone, "") != cluster {
		t.Errorf("empty --repo-creds scoped to %q, want the default %q", dirFor("", config.NoCacheNone, ""), cluster)
	}

	// Disabled: "" is what tells the renderer to skip the cache entirely.
	if got := dirFor(config.RepoCredsCluster, config.NoCacheCharts, ""); got != "" {
		t.Errorf("--no-cache=charts still returned a chart dir: %q", got)
	}
	if got := dirFor(config.RepoCredsCluster, config.NoCacheAll, ""); got != "" {
		t.Errorf("--no-cache=all still returned a chart dir: %q", got)
	}
	// ...while disabling only the render cache must leave downloads reusable.
	if got := dirFor(config.RepoCredsCluster, config.NoCacheRender, ""); got == "" {
		t.Error("--no-cache=render also disabled the chart cache")
	}
}

// TestChartCacheDirScopedByCredentialInstance: one level inside `cluster` mode,
// two clusters are two credential sources. A chart downloaded while reading
// cluster A's repository Secrets must not satisfy a run whose purpose is
// checking that cluster B's Secrets can fetch it - the same false-positive
// verification the mode split fixes, recurring one step in.
func TestChartCacheDirScopedByCredentialInstance(t *testing.T) {
	base := t.TempDir()
	logger := log.New(nil)
	logger.SetLevel(log.FatalLevel)
	f := NewFactory(&config.Config{CacheDir: base, RepoCreds: config.RepoCredsCluster}, logger)

	prod := credentialInstance(config.RepoCredsCluster, "https://prod.example:6443", "argocd")
	staging := credentialInstance(config.RepoCredsCluster, "https://staging.example:6443", "argocd")
	otherNS := credentialInstance(config.RepoCredsCluster, "https://prod.example:6443", "argocd-team")

	dirProd := f.chartCacheDir(prod)
	if dirProd == f.chartCacheDir(staging) {
		t.Error("two clusters share a chart cache dir")
	}
	if dirProd == f.chartCacheDir(otherNS) {
		t.Error("two ArgoCD namespaces share a chart cache dir")
	}
	if f.chartCacheDir(prod) != dirProd {
		t.Error("instance segment is not deterministic")
	}
	// The instance nests INSIDE the mode scope, so `cache clean` semantics and
	// the charts/<mode> layout stay intact.
	if filepath.Base(filepath.Dir(dirProd)) != config.RepoCredsCluster {
		t.Errorf("instance dir %q is not nested under the mode scope", dirProd)
	}
	// The segment stays filesystem-safe and identifiable for server URLs (and
	// any hostile input); the trailing hash keeps sanitized collisions apart.
	weird := credentialInstance(config.RepoCredsCluster, "https://10.0.0.5:6443/api", "argocd")
	weirdAlias := credentialInstance(config.RepoCredsCluster, "https://10.0.0.5:6443_api", "argocd")
	if seg := filepath.Base(f.chartCacheDir(weird)); strings.ContainsAny(seg, "/:\x00") {
		t.Errorf("instance segment %q is not filesystem-safe", seg)
	}
	if f.chartCacheDir(weird) == f.chartCacheDir(weirdAlias) {
		t.Error("sanitization collision merged two instances (the hash suffix must keep them apart)")
	}

	// local and none have no instance dimension - and credentialInstance is
	// what guarantees it, so a caller cannot accidentally scope them.
	if credentialInstance(config.RepoCredsLocal, "https://prod.example:6443", "argocd") != "" {
		t.Error("local mode grew an instance identity")
	}
	if credentialInstance(config.RepoCredsNone, "https://prod.example:6443", "argocd") != "" {
		t.Error("none mode grew an instance identity")
	}
}

// TestCredentialInstanceKeysOnServerNotContextName is the collision pin from
// review: a context NAME is an alias local to one kubeconfig file, so two
// files can both define a "prod" pointing at different clusters - and keying
// the credential instance on the name would let cluster A's cache answer for
// cluster B (or a recreated kind cluster reuse its predecessor's). The
// instance must come from what the name DEREFERENCES to: the API server. Two
// real Clients are built from two kubeconfigs (construction never dials), so
// this pins the wiring, not just the pure function.
func TestCredentialInstanceKeysOnServerNotContextName(t *testing.T) {
	writeKubeconfig := func(server string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "kubeconfig")
		if err := os.WriteFile(p, []byte(`apiVersion: v1
kind: Config
clusters:
- cluster: {server: "`+server+`"}
  name: prod
contexts:
- context: {cluster: prod}
  name: prod
current-context: prod
`), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	clientA, err := cluster.NewClient(writeKubeconfig("https://cluster-a.example:6443"), "prod")
	if err != nil {
		t.Fatal(err)
	}
	clientB, err := cluster.NewClient(writeKubeconfig("https://cluster-b.example:6443"), "prod")
	if err != nil {
		t.Fatal(err)
	}

	// Same alias, same resolved context name - the coordinate that used to
	// feed the instance and is proven here to be worthless as an identity.
	if clientA.ResolvedContext() != "prod" || clientB.ResolvedContext() != "prod" {
		t.Fatalf("resolved contexts = %q, %q; the collision setup requires both to be \"prod\"",
			clientA.ResolvedContext(), clientB.ResolvedContext())
	}

	// Through the App's own derivation (the call site initialize uses), so a
	// refactor that feeds ResolvedContext back in fails HERE, not only in a
	// live two-cluster setup.
	app := &App{cfg: &config.Config{RepoCreds: config.RepoCredsCluster, ArgoCDNamespace: "argocd"}}
	instA := app.credentialInstanceFor(clientA)
	instB := app.credentialInstanceFor(clientB)
	if instA == "" || instB == "" {
		t.Fatalf("instance empty: A=%q B=%q (ClusterServer not populated from the kubeconfig?)", instA, instB)
	}
	if instA == instB {
		t.Error("two clusters behind the same context name share a credential instance - the cache would answer across clusters")
	}

	// And the inverse: one cluster reached through two kubeconfig files (or
	// names) is ONE instance, so the cache is not split by the alias either.
	clientA2, err := cluster.NewClient(writeKubeconfig("https://cluster-a.example:6443"), "prod")
	if err != nil {
		t.Fatal(err)
	}
	if app.credentialInstanceFor(clientA2) != instA {
		t.Error("the same cluster from a second kubeconfig produced a different instance")
	}
}

// TestCreateRenderCacheRespectsLayers pins the other half of the split.
func TestCreateRenderCacheRespectsLayers(t *testing.T) {
	logger := log.New(nil)
	logger.SetLevel(log.FatalLevel)

	for _, tc := range []struct {
		noCache string
		wantNil bool
	}{
		{config.NoCacheNone, false},
		{config.NoCacheCharts, false}, // charts off, renders still cached
		{config.NoCacheRender, true},
		{config.NoCacheAll, true},
	} {
		f := NewFactory(&config.Config{CacheDir: t.TempDir(), NoCache: tc.noCache}, logger)
		cache, err := f.CreateRenderCache()
		if err != nil {
			t.Fatalf("--no-cache=%s: %v", tc.noCache, err)
		}
		if (cache == nil) != tc.wantNil {
			t.Errorf("--no-cache=%s: cache nil = %v, want %v", tc.noCache, cache == nil, tc.wantNil)
		}
	}
}
