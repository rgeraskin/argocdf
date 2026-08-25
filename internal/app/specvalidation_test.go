package app

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/charmbracelet/log"

	"github.com/rgeraskin/argocdf/internal/cluster"
	"github.com/rgeraskin/argocdf/internal/config"
	"github.com/rgeraskin/argocdf/internal/diff"
	"github.com/rgeraskin/argocdf/internal/render"
	"github.com/rgeraskin/argocdf/internal/rendercache"
	"github.com/rgeraskin/argocdf/internal/testutil"
	"github.com/rgeraskin/argocdf/internal/types"
)

// countingRenderer records the worktree of every render it is asked for, so a
// test can assert which SIDES were rendered - including the side that was not.
type countingRenderer struct {
	mu    sync.Mutex
	paths []string
}

func (r *countingRenderer) RenderApplication(
	_ context.Context,
	app *cluster.Application,
	repoPath string,
	_ string,
) (*render.RenderResult, error) {
	r.mu.Lock()
	r.paths = append(r.paths, repoPath)
	r.mu.Unlock()

	return &render.RenderResult{
		Manifests:  []byte(revConfigMap("rendered-"+app.Name, "rev")),
		SourceType: types.SourceTypeHelm,
	}, nil
}

func (r *countingRenderer) rendered() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.paths...)
}

// validChartSpec is a spec ArgoCD accepts. A remote CHART source deliberately:
// sourcePathsExist skips the worktree check for one, so the render is really
// attempted and the counting renderer really sees it.
func validChartSpec() *cluster.ApplicationSpec {
	return &cluster.ApplicationSpec{Source: &cluster.ApplicationSource{
		RepoURL:        "https://charts.example.com",
		Chart:          "web",
		TargetRevision: "1.2.3",
	}}
}

// invalidArtifactSpec is the spec that motivated spec validation: an OCI-ARTIFACT
// source with no `path`. A live ArgoCD 3.3.11 controller stamps InvalidSpecError
// on such an application and never renders it.
func invalidArtifactSpec() *cluster.ApplicationSpec {
	return &cluster.ApplicationSpec{Source: &cluster.ApplicationSource{
		RepoURL:        "oci://registry.example.com/artifacts/web",
		TargetRevision: "1.2.3",
	}}
}

func specValidationApp(t *testing.T, renderer applicationRenderer) *App {
	t.Helper()

	logger := log.New(nil)
	logger.SetLevel(log.FatalLevel)
	cfg := &config.Config{Concurrency: 1, MaxDepth: 5, TargetBranch: "feature"}

	return &App{
		factory:        NewFactory(cfg, logger),
		cfg:            cfg,
		logger:         logger,
		renderer:       renderer,
		differ:         diff.NewManifestDiffer(),
		discoverer:     diff.NewAppDiscoverer(),
		baseRef:        "main",
		baseWorktree:   t.TempDir(),
		targetWorktree: t.TempDir(),
	}
}

// TestInvalidSpecFailsThePerSideRender pins WHERE spec validation runs: once per
// SIDE, inside renderBranch, over the spec that side renders with.
//
// The two sides carry two different specs (a child app's spec can change with the
// PR), so a per-APPLICATION check placed anywhere else would pass this test's
// first subtest and fail its second: with a valid base spec and an invalid target
// one it would refuse before the base render, reporting zero renders and naming
// the wrong branch. That asymmetry is the whole assertion.
//
// The application FAILS rather than diffing one-sided. An empty side reads as
// "every resource added" (or removed), and for an invalid TARGET spec that would
// be a fabrication: ArgoCD keeps serving the last synced state and stamps a
// condition, it does not prune the application. An error is also how every other
// unrenderable application already behaves.
func TestInvalidSpecFailsThePerSideRender(t *testing.T) {
	tests := []struct {
		name         string
		oldSpec      *cluster.ApplicationSpec // the base side's spec
		spec         *cluster.ApplicationSpec // the target side's spec
		wantErr      string
		wantRenders  int
		wantSideName string
	}{
		{
			// A PR that FIXES a broken child spec: nothing is rendered at all,
			// because the base side is refused before processOneApp reaches the
			// target render.
			name:         "invalid on the base side",
			oldSpec:      invalidArtifactSpec(),
			spec:         validChartSpec(),
			wantErr:      "failed to render base branch",
			wantRenders:  0,
			wantSideName: "base",
		},
		{
			// A PR that BREAKS one: the base side rendered before the target
			// spec was ever looked at, which is what proves validation is per
			// side and not per application.
			name:         "invalid on the target side",
			oldSpec:      validChartSpec(),
			spec:         invalidArtifactSpec(),
			wantErr:      "failed to render target branch",
			wantRenders:  1,
			wantSideName: "target",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			renderer := &countingRenderer{}
			a := specValidationApp(t, renderer)

			// Through processWave, because the contract includes where the error
			// LANDS: as this application's AppDiff.Error, leaving the rest of the
			// wave to diff normally.
			results := a.processWave(context.Background(), []*diff.QueuedApp{{
				Name:      "artifact-app",
				Namespace: "argocd",
				Spec:      tt.spec,
				OldSpec:   tt.oldSpec,
			}, {
				Name:      "healthy-app",
				Namespace: "argocd",
				Spec:      validChartSpec(),
			}})

			if len(results) != 2 {
				t.Fatalf("processWave() returned %d results, want 2", len(results))
			}

			bad, good := results[0], results[1]
			if bad.Error == nil {
				t.Fatalf("AppDiff.Error = nil, want the %s side's spec refused", tt.wantSideName)
			}
			if !strings.Contains(bad.Error.Error(), tt.wantErr) {
				t.Errorf("AppDiff.Error = %q, want it to name the %s side (%q)",
					bad.Error, tt.wantSideName, tt.wantErr)
			}
			if !strings.Contains(bad.Error.Error(),
				"spec.source.repoURL and either spec.source.path or spec.source.chart are required") {
				t.Errorf("AppDiff.Error = %q, want ArgoCD's own InvalidSpecError message", bad.Error)
			}
			if bad.Diff != nil {
				t.Errorf("AppDiff.Diff = %v, want nil (a refused spec renders nothing)", bad.Diff)
			}

			// One bad application must not cost the wave its other diffs.
			if good.Error != nil {
				t.Errorf("healthy-app AppDiff.Error = %v, want nil", good.Error)
			}
			if good.Diff == nil {
				t.Error("healthy-app AppDiff.Diff = nil, want the application to have diffed normally")
			}

			// Renders of the refused application: the healthy one adds two of its
			// own, so count only the sides that could belong to it.
			renders := renderer.rendered()
			wantTotal := tt.wantRenders + 2
			if len(renders) != wantTotal {
				t.Fatalf("RenderApplication calls = %d (%v), want %d: %d for the refused application",
					len(renders), renders, wantTotal, tt.wantRenders)
			}
			if tt.wantRenders == 1 {
				baseRenders := 0
				for _, p := range renders {
					if p == a.baseWorktree {
						baseRenders++
					}
				}
				if baseRenders != 2 {
					t.Errorf("base-worktree renders = %d, want 2 (both applications' base side)", baseRenders)
				}
			}
		})
	}
}

// TestInvalidSpecIsRefusedBeforeTheRenderCache pins the ORDER inside
// renderBranch: validation runs before the render cache is consulted.
//
// The other order fails silently and durably, which is what this test builds: an
// entry stored while the spec was still valid answers for the invalid one, since
// the key is built from the source's identity and the commit and not from the
// fields validation reads. The report would then show the manifests of an
// application ArgoCD has since refused, for as long as that entry lives on disk.
func TestInvalidSpecIsRefusedBeforeTheRenderCache(t *testing.T) {
	renderer := &countingRenderer{}
	a := specValidationApp(t, renderer)

	logger := log.New(nil)
	logger.SetLevel(log.FatalLevel)
	cache, err := rendercache.New(t.TempDir(), logger)
	if err != nil {
		t.Fatalf("rendercache.New() error: %v", err)
	}
	a.cache = cache

	app := &cluster.Application{Spec: *invalidArtifactSpec()}
	app.Name = "artifact-app"
	app.Namespace = "argocd"

	// A cacheable key is the precondition: without one there is no stale entry to
	// be answered from, and the test would prove nothing. An exact version tag on
	// an artifact source is immutable, so it keys.
	const commit = "1234567890abcdef1234567890abcdef12345678"
	key, haveKey := a.renderCacheKey(app, commit)
	if !haveKey {
		t.Fatalf("renderCacheKey() reported no key for %v; this test needs a cacheable spec", app.Spec.Source)
	}
	stale := []byte(revConfigMap("stale-from-cache", "rev"))
	if perr := cache.Put(key, &rendercache.Entry{Manifests: stale, SourceType: string(types.SourceTypeHelm)}); perr != nil {
		t.Fatalf("cache.Put() error: %v", perr)
	}

	manifests, _, err := a.renderBranch(context.Background(), app, a.baseWorktree, commit, "main", "new app")
	if err == nil {
		t.Fatalf("renderBranch() = %q with nil error, want the spec refused: the cache answered for a spec ArgoCD rejects",
			manifests)
	}
	if a.cacheHits.Load() != 0 {
		t.Errorf("render-cache hits = %d, want 0 (the spec is refused before the lookup)", a.cacheHits.Load())
	}
	if len(renderer.rendered()) != 0 {
		t.Errorf("RenderApplication calls = %v, want none", renderer.rendered())
	}
}

// artifactChildCRD is a child Application CR with an OCI-ARTIFACT source, with or
// without the `path` ArgoCD requires. Rendered by a parent, it is how the
// motivating shape actually reaches argocdf: an artifact-only application can
// never be SELECTED (an oci:// URL cannot equal the diffed git repo's URL), so
// apps-of-apps discovery is its only route into a report.
func artifactChildCRD(name, path string) string {
	pathLine := ""
	if path != "" {
		pathLine = "\n    path: " + path
	}

	return fmt.Sprintf(`apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: %s
  namespace: argocd
spec:
  project: default
  source:
    repoURL: oci://registry.example.com/artifacts/web
    targetRevision: 1.2.3%s
  destination:
    server: https://kubernetes.default.svc
`, name, pathLine)
}

// brokenChildRenderer renders a parent whose catalog KEEPS the same child on both
// sides but drops its `path` on the target side - a PR that breaks a child's spec.
// Every render is recorded as "app@side".
type brokenChildRenderer struct {
	targetWorktree string

	mu    sync.Mutex
	calls []string
}

func (r *brokenChildRenderer) RenderApplication(
	_ context.Context,
	app *cluster.Application,
	repoPath string,
	_ string,
) (*render.RenderResult, error) {
	side := "base"
	if repoPath == r.targetWorktree {
		side = "target"
	}

	r.mu.Lock()
	r.calls = append(r.calls, app.Name+"@"+side)
	r.mu.Unlock()

	if app.Name == "parent" {
		path := "."
		if side == "target" {
			path = "" // the PR removes it
		}
		return &render.RenderResult{
			Manifests:  []byte(artifactChildCRD("artifact-child", path)),
			SourceType: types.SourceTypeHelm,
		}, nil
	}

	return &render.RenderResult{
		Manifests:  []byte(revConfigMap("child-cm", "1.0.0")),
		SourceType: types.SourceTypeHelm,
	}, nil
}

func (r *brokenChildRenderer) rendered() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.calls...)
}

// TestInvalidChildSpecIsReportedThroughDiscovery covers the path the unit tests
// above cannot reach and the one the motivating bug actually travelled: the
// invalid application is not in the cluster listing and cannot be selected - it is
// DISCOVERED from a parent's rendered catalog, which is the only way an
// artifact-only application ever enters a report.
//
// What it pins: the child is still reported (an error, not an absence), the error
// names the side whose spec is broken while the other side rendered, and the
// PARENT still diffs normally - the child CR's own spec change is visible there,
// which is the only remaining evidence a reviewer gets about what broke.
func TestInvalidChildSpecIsReportedThroughDiscovery(t *testing.T) {
	renderer := &brokenChildRenderer{}
	a := specValidationApp(t, renderer)
	renderer.targetWorktree = a.targetWorktree

	parent := cluster.Application{Spec: *validChartSpec()}
	parent.Name = "parent"
	parent.Namespace = "argocd"

	diffs, err := a.processApplications(context.Background(), []cluster.Application{parent})
	if err != nil {
		t.Fatalf("processApplications() error: %v", err)
	}

	byName := map[string]*diff.AppDiff{}
	for _, d := range diffs {
		byName[d.Name] = d
	}

	child := byName["artifact-child"]
	if child == nil {
		t.Fatalf("artifact-child missing from the report; got %v", byName)
	}
	if child.Error == nil {
		t.Fatalf("artifact-child Error = nil, want the target side's spec refused")
	}
	if !strings.Contains(child.Error.Error(), "failed to render target branch") {
		t.Errorf("artifact-child Error = %q, want it to name the target side", child.Error)
	}
	if !strings.Contains(child.Error.Error(),
		"spec.source.repoURL and either spec.source.path or spec.source.chart are required") {
		t.Errorf("artifact-child Error = %q, want ArgoCD's own InvalidSpecError message", child.Error)
	}

	// The parent is the reviewer's only remaining evidence of what broke, so its
	// own diff must survive the child's failure.
	parentDiff := byName["parent"]
	if parentDiff == nil {
		t.Fatal("parent missing from the report")
	}
	if parentDiff.Error != nil {
		t.Errorf("parent Error = %v, want nil", parentDiff.Error)
	}
	if parentDiff.Diff == nil {
		t.Error("parent Diff = nil, want the changed child CR to have diffed")
	}

	// The child's BASE side was valid and did render; only the target was refused.
	calls := renderer.rendered()
	want := map[string]bool{"parent@base": true, "parent@target": true, "artifact-child@base": true}
	for _, c := range calls {
		if !want[c] {
			t.Errorf("unexpected render %q (calls: %v)", c, calls)
		}
		delete(want, c)
	}
	if len(want) != 0 {
		t.Errorf("missing renders %v (calls: %v)", want, calls)
	}
}

// TestInvalidSpecIsStillSelected pins the NEGATIVE half of the rule: selection
// does not validate, so an application with a refused spec is still matched to the
// change and still reaches the report - as an error.
//
// The temptation is to drop it during filtering, where the check looks cheaper and
// saves a render. That is the exact failure this feature exists to end: an
// application ArgoCD refuses would vanish from the report entirely, which reads as
// "not affected by this PR" and is indistinguishable from everything being fine.
// The fixture is invalid AND selectable - a `chart` with no `targetRevision`,
// beside a path the change touches - because the motivating shape (no path at all)
// cannot be selected by containment in the first place.
func TestInvalidSpecIsStillSelected(t *testing.T) {
	logger := log.New(nil)
	logger.SetLevel(log.FatalLevel)
	cfg := &config.Config{RepoURL: "https://github.com/org/repo"}
	a := &App{cfg: cfg, logger: logger}

	app := cluster.Application{Spec: cluster.ApplicationSpec{Source: &cluster.ApplicationSource{
		RepoURL: "https://github.com/org/repo",
		Path:    "charts/web",
		Chart:   "web", // no TargetRevision: ArgoCD refuses this spec
	}}}
	app.Name = "half-broken"
	app.Namespace = "argocd"

	if err := cluster.ValidateSourceSpec(&app); err == nil {
		t.Fatal("fixture is not invalid; this test needs a spec ArgoCD refuses")
	}

	got := a.filterAffectedApps(
		[]cluster.Application{app},
		testutil.TestChangedFiles([]string{"charts/web/values.yaml"}, nil, nil),
	)
	if len(got) != 1 {
		t.Fatalf("filterAffectedApps() selected %d apps, want 1: a refused spec must not be filtered away", len(got))
	}
}

// TestSkippedSideIsNotValidated pins that validation follows the RENDER, not the
// application: a side processOneApp never renders is never validated either.
//
// A newly-added child has no base counterpart, so its base render is skipped -
// and the spec that side WOULD have used must not be able to fail the
// application. The fixture is artificial on purpose (a new app carries no old
// spec in production); what it guards is a refactor that hoists the check out of
// renderBranch and into processOneApp, where both specs get validated whether or
// not either is rendered. That refactor looks like a simplification and would
// break new and removed applications.
func TestSkippedSideIsNotValidated(t *testing.T) {
	renderer := &countingRenderer{}
	a := specValidationApp(t, renderer)

	results := a.processWave(context.Background(), []*diff.QueuedApp{{
		Name:      "new-app",
		Namespace: "argocd",
		Spec:      validChartSpec(),
		OldSpec:   invalidArtifactSpec(), // the base side's spec, never rendered
		IsNew:     true,
	}})

	if results[0].Error != nil {
		t.Fatalf("AppDiff.Error = %v, want nil: the base side is skipped, so its spec is not validated",
			results[0].Error)
	}
	if got := renderer.rendered(); len(got) != 1 || got[0] != a.targetWorktree {
		t.Errorf("renders = %v, want exactly one from the target worktree", got)
	}
}
