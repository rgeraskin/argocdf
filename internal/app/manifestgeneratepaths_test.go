package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/log"

	"github.com/rgeraskin/argocdf/internal/cluster"
	"github.com/rgeraskin/argocdf/internal/config"
	"github.com/rgeraskin/argocdf/internal/diff"
	"github.com/rgeraskin/argocdf/internal/testutil"
)

// TestFilterAffectedApps_ManifestGeneratePaths pins the semantics of ArgoCD's
// argocd.argoproj.io/manifest-generate-paths annotation as argocdf applies it.
// The values here were verified against ArgoCD v3.3.11's own resolver AND against
// a live controller: a webhook push touching only apps/kustomize-base refreshed an
// overlay annotated `../kustomize-base;.`, while a push touching an unrelated path
// did not - and every app WITHOUT the annotation refreshed either way.
//
// The load-bearing case is "declares only the base": the annotation REPLACES the
// default rather than extending it, so the app's own path stops matching. That is
// ArgoCD's behavior, it is the easiest thing to get wrong when writing the
// annotation, and a reader of this table should not have to discover it in prod.
func TestFilterAffectedApps_ManifestGeneratePaths(t *testing.T) {
	const localURL = "https://github.com/org/repo"

	// annotated returns an app whose single source is apps/overlay, declaring the
	// given manifest-generate-paths value.
	annotated := func(value string) cluster.Application {
		app := testutil.TestApp("overlay", "argocd", localURL, "apps/overlay")
		if value != "\x00" { // sentinel: no annotation at all
			app.Annotations = map[string]string{
				"argocd.argoproj.io/manifest-generate-paths": value,
			}
		}
		return app
	}

	tests := []struct {
		name    string
		app     cluster.Application
		changed string
		want    bool
	}{
		// Relative entries resolve against the source path: apps/overlay + ../base.
		{"declares base, base changed", annotated("../base"), "apps/base/cm.yaml", true},
		{"declares base, unrelated changed", annotated("../base"), "apps/other/cm.yaml", false},
		// The trap: declaring only the base stops the app's OWN path from matching.
		{"declares base only, own path changed", annotated("../base"), "apps/overlay/kustomization.yaml", false},
		{"declares base and self, own path changed", annotated("../base;."), "apps/overlay/kustomization.yaml", true},
		{"declares base and self, base changed", annotated("../base;."), "apps/base/cm.yaml", true},
		{"declares base and self, unrelated changed", annotated("../base;."), "apps/other/cm.yaml", false},
		// A leading slash is repo-root-relative, NOT joined to the source path.
		{"absolute entry, matching change", annotated("/apps/base"), "apps/base/cm.yaml", true},
		{"absolute entry, unrelated change", annotated("/apps/base"), "apps/overlay/kustomization.yaml", false},
		// Self-declaration narrows to exactly the source path.
		{"declares self only, own path changed", annotated("."), "apps/overlay/kustomization.yaml", true},
		{"declares self only, base changed", annotated("."), "apps/base/cm.yaml", false},
		// Globs go through Go's filepath.Match, which does NOT cross separators -
		// so a pattern matches files in one directory level, not a subtree.
		{"glob in declared dir, matching file", annotated("../base/*.yaml"), "apps/base/cm.yaml", true},
		{"glob in declared dir, other extension", annotated("../base/*.yaml"), "apps/base/README.md", false},
		{"glob does not cross separators", annotated("../base/*.yaml"), "apps/base/nested/cm.yaml", false},
		{"glob over one path segment", annotated("/apps/*/cm.yaml"), "apps/base/cm.yaml", true},
		// Present-but-empty declares nothing: fall back to argocdf's path matching
		// (NOT to ArgoCD's "always refresh", which would render every app forever).
		{"empty annotation falls back to path matching", annotated(""), "apps/overlay/kustomization.yaml", true},
		{"empty annotation, unrelated change", annotated(""), "apps/other/cm.yaml", false},
		// Separators only: ArgoCD skips empty entries when splitting on ";", so the
		// declaration resolves to nothing at all. Treated exactly like the empty
		// annotation above - a `;` typo must not delete the app from every report.
		{"separator-only annotation falls back to path matching", annotated(";"), "apps/overlay/kustomization.yaml", true},
		{"separator-only annotation, unrelated change", annotated(";"), "apps/other/cm.yaml", false},
		{"several separators fall back too", annotated(";;;"), "apps/overlay/kustomization.yaml", true},
		// A lone "/" is the opposite extreme and NOT a typo argocdf can second-guess:
		// upstream turns it into the refresh path "", which matches every file. The
		// app becomes always-affected, and the resolved paths appear in --verbose.
		{"lone slash declares the whole repository", annotated("/"), "apps/other/cm.yaml", true},
		{"whitespace-only annotation falls back", annotated("  "), "apps/overlay/kustomization.yaml", true},
		// No annotation: unchanged behavior.
		{"no annotation, own path changed", annotated("\x00"), "apps/overlay/kustomization.yaml", true},
		{"no annotation, base changed", annotated("\x00"), "apps/base/cm.yaml", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := log.New(nil)
			logger.SetLevel(log.FatalLevel)
			a := &App{cfg: &config.Config{RepoURL: localURL}, logger: logger}

			got := a.filterAffectedApps(
				[]cluster.Application{tt.app},
				testutil.TestChangedFiles(nil, []string{tt.changed}, nil),
			)
			if affected := len(got) == 1; affected != tt.want {
				t.Errorf("affected = %v, want %v (changed %s)", affected, tt.want, tt.changed)
			}
		})
	}
}

// TestManifestGeneratePathsIgnoresForeignRepoSources pins that declared paths are
// resolved only for sources in the repo being diffed - the paths are repo-relative,
// so a source pointing elsewhere cannot satisfy them (ArgoCD's webhook applies the
// same per-source URL check before consulting the annotation).
func TestManifestGeneratePathsIgnoresForeignRepoSources(t *testing.T) {
	const localURL = "https://github.com/org/repo"

	app := testutil.TestApp("overlay", "argocd", "https://github.com/other/repo", "apps/base")
	app.Annotations = map[string]string{
		"argocd.argoproj.io/manifest-generate-paths": ".",
	}

	logger := log.New(nil)
	logger.SetLevel(log.FatalLevel)
	a := &App{cfg: &config.Config{RepoURL: localURL}, logger: logger}

	got := a.filterAffectedApps(
		[]cluster.Application{app},
		testutil.TestChangedFiles(nil, []string{"apps/base/cm.yaml"}, nil),
	)
	if len(got) != 0 {
		t.Errorf("app with a foreign-repo source was reported affected: %v", got)
	}
}

// TestManifestGeneratePathsReplacesRefValueFileMatching is the helm-specific trap.
// Without an annotation, argocdf finds a $ref value file through
// helmRefFilesAffected even when it lives outside every source path. A
// declaration REPLACES that matcher, so an app that declares its chart directory
// and forgets its values file silently stops reacting to values changes - the same
// caveat ArgoCD documents for apps referencing external helm values files.
func TestManifestGeneratePathsReplacesRefValueFileMatching(t *testing.T) {
	const localURL = "https://github.com/org/repo"

	// chart source in the local repo, taking values from a ref source rooted at
	// config/ - so the values file lives at config/env/prod.yaml.
	sources := []cluster.ApplicationSource{
		{
			RepoURL: localURL, Path: "apps/chart",
			Helm: &cluster.ApplicationSourceHelm{ValueFiles: []string{"$values/env/prod.yaml"}},
		},
		{RepoURL: localURL, Ref: "values", Path: "config"},
	}

	for _, tt := range []struct {
		name       string
		annotation string
		want       bool
	}{
		{"no annotation: the $ref matcher finds the values file", "", true},
		{"declares the chart dir only: values change is MISSED", "apps/chart", false},
		{"declares the values file too: found again", "apps/chart;/config/env/prod.yaml", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			app := testutil.TestAppMultiSource("chart-with-values", "argocd", sources)
			if tt.annotation != "" {
				app.Annotations = map[string]string{
					"argocd.argoproj.io/manifest-generate-paths": tt.annotation,
				}
			}
			logger := log.New(nil)
			logger.SetLevel(log.FatalLevel)
			a := &App{cfg: &config.Config{RepoURL: localURL}, logger: logger}

			got := a.filterAffectedApps([]cluster.Application{app},
				testutil.TestChangedFiles(nil, []string{"config/env/prod.yaml"}, nil))
			if affected := len(got) == 1; affected != tt.want {
				t.Errorf("affected = %v, want %v", affected, tt.want)
			}
		})
	}
}

// TestManifestGeneratePathsEmptySourcePathIsRepoRoot pins a shape that surprises:
// a `ref` source (and a remote chart source) has NO path, so a relative entry
// resolves against the repo ROOT - `.` then matches every file in the repository
// and the app is always affected. This is ArgoCD's own resolution, kept for parity;
// a multi-source helm app that wants narrowing must name paths explicitly.
func TestManifestGeneratePathsEmptySourcePathIsRepoRoot(t *testing.T) {
	const localURL = "https://github.com/org/repo"

	app := testutil.TestAppMultiSource("chart-with-values", "argocd", []cluster.ApplicationSource{
		{RepoURL: "ghcr.io/org/charts", Chart: "app"}, // foreign, no path
		{RepoURL: localURL, Ref: "values"},            // local, no path -> repo root
	})
	app.Annotations = map[string]string{
		"argocd.argoproj.io/manifest-generate-paths": ".",
	}

	logger := log.New(nil)
	logger.SetLevel(log.FatalLevel)
	a := &App{cfg: &config.Config{RepoURL: localURL}, logger: logger}

	got := a.filterAffectedApps([]cluster.Application{app},
		testutil.TestChangedFiles(nil, []string{"totally/unrelated.yaml"}, nil))
	if len(got) != 1 {
		t.Errorf("`.` on a path-less source should match the whole repo, got %d apps", len(got))
	}
}

// TestManifestGeneratePathsUnresolvableIsNeverAffected pins the other end of that
// shape: when NO source lives in the repo being diffed (a remote-chart-only app),
// the declaration cannot resolve at all and the app can never be reported. argocdf
// warns in that case rather than failing quietly, because the app would otherwise
// disappear from every report with no explanation.
func TestManifestGeneratePathsUnresolvableIsNeverAffected(t *testing.T) {
	const localURL = "https://github.com/org/repo"

	app := testutil.TestApp("remote-chart", "argocd", "ghcr.io/org/charts", "")
	app.Spec.Source.Chart = "app"
	app.Annotations = map[string]string{
		"argocd.argoproj.io/manifest-generate-paths": ".",
	}

	var logs bytes.Buffer
	logger := log.New(&logs)
	logger.SetLevel(log.WarnLevel)
	a := &App{cfg: &config.Config{RepoURL: localURL}, logger: logger}

	got := a.filterAffectedApps([]cluster.Application{app},
		testutil.TestChangedFiles(nil, []string{"apps/chart/values.yaml"}, nil))
	if len(got) != 0 {
		t.Errorf("declared paths cannot resolve outside the diffed repo, got %d apps", len(got))
	}
	// The warning is the only signal a user gets, so it is part of the contract.
	if !strings.Contains(logs.String(), "can never be reported as affected") {
		t.Errorf("expected a warning about the unresolvable declaration, got: %q", logs.String())
	}
}

// TestManifestGeneratePathsDoesNotGateDiscovery pins the annotation's boundary in
// the apps-of-apps pipeline: it gates WAVE-0 SELECTION only, never discovery.
//
// A cluster-listed child whose annotation excludes the change is dropped from wave
// 0 - but when its PARENT is selected and the parent's render modifies the child's
// spec, discovery re-enqueues the child from the rendered Application CR, which
// carries no such annotation and is never filtered. So an annotated child loses
// own-content detection (its chart dir changing) but KEEPS spec-change detection
// through its parent. The inverse amplification also follows from selection being
// the only gate: annotate a PARENT with paths that miss the catalog and the whole
// subtree - added, modified and removed children - disappears with it.
func TestManifestGeneratePathsDoesNotGateDiscovery(t *testing.T) {
	logger := log.New(nil)
	logger.SetLevel(log.FatalLevel)

	const localURL = "https://example.com/org/repo.git"
	cfg := &config.Config{RepoURL: localURL, Concurrency: 2, MaxDepth: 5}

	// Real worktree dirs containing the parent's source path: processOneApp
	// stats local source paths before rendering (the external-repo fix), so a
	// path source pointing into a nonexistent worktree would render empty and
	// the parent would emit no child CR to discover.
	base, target := t.TempDir(), t.TempDir()
	for _, wt := range []string{base, target} {
		if err := os.MkdirAll(filepath.Join(wt, "charts", "apps"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fake := &fakeRenderer{baseWorktree: base, targetWorktree: target}

	a := &App{
		factory:        NewFactory(cfg, logger),
		cfg:            cfg,
		logger:         logger,
		renderer:       fake,
		differ:         diff.NewManifestDiffer(),
		discoverer:     diff.NewAppDiscoverer(),
		baseWorktree:   fake.baseWorktree,
		targetWorktree: fake.targetWorktree,
	}

	// The parent owns the catalog; the change is there.
	parent := testutil.TestApp("parent", "argocd", localURL, "charts/apps")
	// The cluster-listed child declares paths the change does NOT touch.
	child := testutil.TestApp("child", "argocd", localURL, "apps/child-chart")
	child.Annotations = map[string]string{
		"argocd.argoproj.io/manifest-generate-paths": ".",
	}

	changed := testutil.TestChangedFiles(nil, []string{"charts/apps/values-apps.yaml"}, nil)

	// Wave-0 selection: the annotation must drop the child, keep the parent.
	affected := a.filterAffectedApps([]cluster.Application{parent, child}, changed)
	if len(affected) != 1 || affected[0].Name != "parent" {
		names := make([]string, len(affected))
		for i, app := range affected {
			names[i] = app.Name
		}
		t.Fatalf("wave-0 selection = %v, want [parent] (child excluded by its annotation)", names)
	}

	// The parent's render (fakeRenderer) emits the child's Application CR with a
	// different spec per side, so discovery must bring the child back.
	diffs, err := a.processApplications(context.Background(), affected)
	if err != nil {
		t.Fatalf("processApplications() error: %v", err)
	}
	var childDiff bool
	for _, d := range diffs {
		if d.Name == "child" {
			childDiff = true
			if d.Error != nil {
				t.Fatalf("discovered child finished with error: %v", d.Error)
			}
		}
	}
	if !childDiff {
		t.Fatalf("annotated child was not re-discovered from its parent's render; got %d results", len(diffs))
	}
}

// TestManifestGeneratePathsPerSource pins ArgoCD's per-source resolution: one
// annotation, joined to EACH source's own path, so `.` covers every source dir.
func TestManifestGeneratePathsPerSource(t *testing.T) {
	const localURL = "https://github.com/org/repo"

	app := testutil.TestAppMultiSource("multi", "argocd", []cluster.ApplicationSource{
		{RepoURL: localURL, Path: "apps/first"},
		{RepoURL: localURL, Path: "apps/second"},
	})
	app.Annotations = map[string]string{
		"argocd.argoproj.io/manifest-generate-paths": ".",
	}

	logger := log.New(nil)
	logger.SetLevel(log.FatalLevel)
	a := &App{cfg: &config.Config{RepoURL: localURL}, logger: logger}

	for _, tc := range []struct {
		changed string
		want    bool
	}{
		{"apps/first/values.yaml", true},
		{"apps/second/values.yaml", true},
		{"apps/third/values.yaml", false},
	} {
		got := a.filterAffectedApps(
			[]cluster.Application{app},
			testutil.TestChangedFiles(nil, []string{tc.changed}, nil),
		)
		if affected := len(got) == 1; affected != tc.want {
			t.Errorf("changed %s: affected = %v, want %v", tc.changed, affected, tc.want)
		}
	}
}

// TestManifestGeneratePathsEmptyDiffAffectsNothing pins argocdf's deliberate
// deviation from ArgoCD on the OTHER empty input.
//
// AppFilesHaveChanged returns true for an empty changed-file list: a webhook whose
// payload omitted the file list means "unknown", and refreshing is the safe answer.
// argocdf's list is never unknown - it computed the diff - so empty means nothing
// changed. Delegating that case made `--base X --target X` report every annotated
// app as changed and render them all, while unannotated apps correctly stayed out.
func TestManifestGeneratePathsEmptyDiffAffectsNothing(t *testing.T) {
	const localURL = "https://github.com/org/repo"

	annotated := testutil.TestApp("annotated", "argocd", localURL, "apps/overlay")
	annotated.Annotations = map[string]string{
		"argocd.argoproj.io/manifest-generate-paths": "../base;.",
	}
	// A second app with a declaration that matches everything: `.` on a source at
	// the repo root is the widest declaration there is, so if any shape survives an
	// empty diff it is this one.
	wildcard := testutil.TestApp("wildcard", "argocd", localURL, "")
	wildcard.Annotations = map[string]string{
		"argocd.argoproj.io/manifest-generate-paths": ".",
	}
	plain := testutil.TestApp("plain", "argocd", localURL, "apps/chart")

	logger := log.New(nil)
	logger.SetLevel(log.FatalLevel)
	a := &App{cfg: &config.Config{RepoURL: localURL}, logger: logger}

	got := a.filterAffectedApps(
		[]cluster.Application{annotated, wildcard, plain},
		testutil.TestChangedFiles(nil, nil, nil),
	)
	if len(got) != 0 {
		names := make([]string, 0, len(got))
		for _, app := range got {
			names = append(names, app.Name)
		}
		t.Errorf("empty diff reported %d apps affected (%v), want none", len(got), names)
	}
}

// TestManifestGeneratePathsSourceHydratorFollowsUpstream covers the one shape where
// ArgoCD IGNORES the annotation it otherwise honors.
//
// GetSourceRefreshPaths short-circuits an app with spec.sourceHydrator: for the
// source that equals the hydrator's SYNC source it returns []string{source.Path}
// and never looks at the annotation (util/app/path/path.go:108-115). Since
// ApplicationSpec.GetSources() returns exactly that sync source for a hydrator app,
// argocdf iterates it and inherits the exception - so a declaration is silently
// reduced to the sync source's own path.
//
// argocdf is FAITHFUL here rather than surprising: upstream narrows a hydrator
// app's refresh to the sync path in the same way, so both tools select on the same
// paths. This test exists because the behavior is invisible in argocdf's own code -
// nothing here mentions hydrators - and a future upstream change would otherwise
// move selection silently.
func TestManifestGeneratePathsSourceHydratorFollowsUpstream(t *testing.T) {
	const localURL = "https://github.com/org/repo"

	app := testutil.TestApp("hydrated", "argocd", localURL, "apps/overlay")
	app.Spec.Source = nil
	app.Spec.SourceHydrator = &cluster.SourceHydrator{
		DrySource:  cluster.DrySource{RepoURL: localURL, TargetRevision: "main", Path: "dry/overlay"},
		SyncSource: cluster.SyncSource{TargetBranch: "env/prod", Path: "apps/overlay"},
	}
	app.Annotations = map[string]string{
		"argocd.argoproj.io/manifest-generate-paths": "../base",
	}

	logger := log.New(nil)
	logger.SetLevel(log.FatalLevel)
	a := &App{cfg: &config.Config{RepoURL: localURL}, logger: logger}

	cases := []struct {
		changed string
		want    bool
		why     string
	}{
		{
			changed: "apps/overlay/kustomization.yaml",
			want:    true,
			why:     "the sync source's own path is what upstream substitutes for the declaration",
		},
		{
			changed: "apps/base/cm.yaml",
			want:    false,
			why:     "the declared ../base is ignored for a hydrator sync source, upstream included",
		},
	}
	for _, tc := range cases {
		got := a.filterAffectedApps(
			[]cluster.Application{app},
			testutil.TestChangedFiles(nil, []string{tc.changed}, nil),
		)
		if affected := len(got) == 1; affected != tc.want {
			t.Errorf("changed %s: affected = %v, want %v (%s)", tc.changed, affected, tc.want, tc.why)
		}
	}
}
