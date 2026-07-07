package rendercache

import (
	"testing"

	"github.com/rgeraskin/argocdf/internal/cluster"
)

// fixedResolver returns a constant tree hash for any path.
func fixedResolver(hash string) func(commit, path string) (string, bool) {
	return func(_, _ string) (string, bool) { return hash, true }
}

// mapResolver resolves only the paths present in m; anything else is a miss.
func mapResolver(m map[string]string) func(commit, path string) (string, bool) {
	return func(_, p string) (string, bool) {
		h, ok := m[p]
		return h, ok
	}
}

// allRepo treats every repo URL as the local repo.
func allRepo(string) bool { return true }

// fixedReader returns the same file content for any path.
func fixedReader(content string) func(commit, path string) (string, bool) {
	return func(_, _ string) (string, bool) { return content, true }
}

func baseInput() KeyInput {
	return KeyInput{
		AppName:   "my-app",
		Namespace: "argocd",
		Spec: &cluster.ApplicationSpec{
			Source: &cluster.ApplicationSource{
				RepoURL:        "https://github.com/owner/repo",
				Path:           "apps/foo",
				TargetRevision: "HEAD",
			},
		},
		KubeVersion: "1.29.0",
		Options: KeyOptions{
			KustomizeEnableHelm: false,
			HelmSkipRefresh:     true,
		},
		Commit:      "deadbeef",
		ResolveTree: fixedResolver("treehash-1"),
	}
}

func mustKey(t *testing.T, in KeyInput) string {
	t.Helper()
	k, ok := ComputeKey(in)
	if !ok {
		t.Fatal("ComputeKey: expected ok=true")
	}
	if k == "" {
		t.Fatal("ComputeKey: empty key")
	}
	return k
}

func TestComputeKeyStable(t *testing.T) {
	k1 := mustKey(t, baseInput())
	k2 := mustKey(t, baseInput())
	if k1 != k2 {
		t.Errorf("expected identical keys for identical inputs, got %s != %s", k1, k2)
	}
}

func TestComputeKeyChangesWithSpec(t *testing.T) {
	base := mustKey(t, baseInput())

	in := baseInput()
	in.Spec.Source.Path = "apps/bar"
	if got := mustKey(t, in); got == base {
		t.Error("expected different key when spec path changes")
	}

	in2 := baseInput()
	in2.Spec.Source.Helm = &cluster.ApplicationSourceHelm{
		Parameters: []cluster.HelmParameter{{Name: "image.tag", Value: "v2"}},
	}
	if got := mustKey(t, in2); got == base {
		t.Error("expected different key when helm params change")
	}
}

func TestComputeKeyChangesWithKubeVersion(t *testing.T) {
	base := mustKey(t, baseInput())
	in := baseInput()
	in.KubeVersion = "1.30.0"
	if got := mustKey(t, in); got == base {
		t.Error("expected different key when kube version changes")
	}
}

func TestComputeKeyChangesWithTreeHash(t *testing.T) {
	base := mustKey(t, baseInput())
	in := baseInput()
	in.ResolveTree = fixedResolver("treehash-2")
	if got := mustKey(t, in); got == base {
		t.Error("expected different key when source tree hash changes")
	}
}

func TestComputeKeyChangesWithOptions(t *testing.T) {
	base := mustKey(t, baseInput())
	in := baseInput()
	in.Options.KustomizeEnableHelm = true
	if got := mustKey(t, in); got == base {
		t.Error("expected different key when render options change")
	}
}

func TestComputeKeyChangesWithHelmAddRepos(t *testing.T) {
	base := mustKey(t, baseInput())
	in := baseInput()
	in.Options.HelmAddRepos = true
	if got := mustKey(t, in); got == base {
		t.Error("expected different key when HelmAddRepos changes")
	}
}

func TestComputeKeyBypassOnUnresolvableTree(t *testing.T) {
	in := baseInput()
	in.ResolveTree = func(_, _ string) (string, bool) { return "", false }
	if _, ok := ComputeKey(in); ok {
		t.Error("expected ok=false when a local source tree hash cannot be resolved")
	}
}

func TestComputeKeyNilSpec(t *testing.T) {
	in := baseInput()
	in.Spec = nil
	if _, ok := ComputeKey(in); ok {
		t.Error("expected ok=false for nil spec")
	}
}

// TestComputeKeyChangesWithValueFileContent verifies that a change to a helm
// value file living OUTSIDE the chart path changes the key.
func TestComputeKeyChangesWithValueFileContent(t *testing.T) {
	build := func(vfHash string) KeyInput {
		in := baseInput()
		in.Spec.Source.Helm = &cluster.ApplicationSourceHelm{
			ValueFiles: []string{"../values/prod.yaml"},
		}
		in.ResolveTree = mapResolver(map[string]string{
			"apps/foo":              "tree-foo",
			"apps/values/prod.yaml": vfHash, // resolved relative to chart dir
			"apps/foo/Chart.yaml":   "chart-yaml",
		})
		// Chart.yaml exists without a Chart.lock, so the hermeticity probe
		// reads it; a chart without dependencies is cacheable.
		in.ReadFile = fixedReader("apiVersion: v2\nname: foo\nversion: 1.0.0\n")
		in.SameRepo = allRepo
		return in
	}
	k1 := mustKey(t, build("vf-v1"))
	k2 := mustKey(t, build("vf-v2"))
	if k1 == k2 {
		t.Error("expected different key when value file content changes")
	}
}

// TestComputeKeyChangesWithRefValueFile verifies that a change to a $ref value
// file (resolved against a same-repo ref source) changes the key.
func TestComputeKeyChangesWithRefValueFile(t *testing.T) {
	build := func(vfHash string) KeyInput {
		in := baseInput()
		in.Spec.Source = nil
		in.Spec.Sources = []cluster.ApplicationSource{
			{
				RepoURL: "https://github.com/owner/repo",
				Path:    "chart",
				Helm: &cluster.ApplicationSourceHelm{
					ValueFiles: []string{"$values/env/prod.yaml"},
				},
			},
			{
				RepoURL: "https://github.com/owner/repo",
				Ref:     "values",
				Path:    "config",
			},
		}
		in.ResolveTree = mapResolver(map[string]string{
			"chart":                "tree-chart",
			"config/env/prod.yaml": vfHash,
			"":                     "root-tree", // ref source uses root tree
		})
		in.SameRepo = allRepo
		return in
	}
	k1 := mustKey(t, build("ref-v1"))
	k2 := mustKey(t, build("ref-v2"))
	if k1 == k2 {
		t.Error("expected different key when $ref value file content changes")
	}
}

// TestComputeKeyBypassOnExternalRef verifies that a $ref value file pointing at
// an external repository (content not present locally) bypasses the cache.
func TestComputeKeyBypassOnExternalRef(t *testing.T) {
	in := baseInput()
	in.Spec.Source = nil
	in.Spec.Sources = []cluster.ApplicationSource{
		{
			RepoURL: "https://github.com/owner/repo",
			Path:    "chart",
			Helm: &cluster.ApplicationSourceHelm{
				ValueFiles: []string{"$values/env/prod.yaml"},
			},
		},
		{
			RepoURL: "https://github.com/other/external",
			Ref:     "values",
			Path:    "config",
		},
	}
	in.ResolveTree = mapResolver(map[string]string{"chart": "tree-chart"})
	// Only the local repo counts as same-repo; the external ref must bypass.
	in.SameRepo = func(u string) bool { return u == "https://github.com/owner/repo" }
	if _, ok := ComputeKey(in); ok {
		t.Error("expected ok=false when a $ref value file points at an external repo")
	}
}

// TestComputeKeyKustomizeUsesRootTree verifies that a kustomize/directory source
// (no helm, no Chart.yaml) keys off the commit root tree, so it changes when ANY
// repo file changes.
func TestComputeKeyKustomizeUsesRootTree(t *testing.T) {
	build := func(rootHash string) KeyInput {
		in := baseInput()
		// No helm and no Chart.yaml at the path => kustomize-like.
		in.ResolveTree = mapResolver(map[string]string{"": rootHash})
		in.SameRepo = allRepo
		return in
	}
	k1 := mustKey(t, build("root-v1"))
	k2 := mustKey(t, build("root-v2"))
	if k1 == k2 {
		t.Error("expected different key when the commit root tree changes for a kustomize source")
	}
}

// TestComputeKeyAbsentValueFileSentinelStable verifies that a value file absent
// at the commit produces a stable key (via the "absent" sentinel) rather than a
// bypass.
func TestComputeKeyAbsentValueFileSentinelStable(t *testing.T) {
	build := func() KeyInput {
		in := baseInput()
		in.Spec.Source.Helm = &cluster.ApplicationSourceHelm{
			ValueFiles: []string{"values/missing.yaml"},
		}
		// Chart path resolves, but the value file does not exist.
		in.ResolveTree = mapResolver(map[string]string{"apps/foo": "tree-foo"})
		in.SameRepo = allRepo
		return in
	}
	k1, ok := ComputeKey(build())
	if !ok {
		t.Fatal("expected ok=true for an absent value file (sentinel), got bypass")
	}
	k2 := mustKey(t, build())
	if k1 != k2 {
		t.Error("expected stable key for an absent value file across computations")
	}
}

func TestComputeKeyRemoteChartNeedsNoResolver(t *testing.T) {
	in := KeyInput{
		AppName:   "chart-app",
		Namespace: "argocd",
		Spec: &cluster.ApplicationSpec{
			Source: &cluster.ApplicationSource{
				RepoURL:        "https://charts.example.com",
				Chart:          "nginx",
				TargetRevision: "1.2.3",
			},
		},
		KubeVersion: "1.29.0",
		Commit:      "deadbeef",
		// ResolveTree intentionally nil: remote charts must not need it.
	}
	base := mustKey(t, in)

	// Changing the chart target revision must change the key.
	in2 := in
	specCopy := *in.Spec
	srcCopy := *in.Spec.Source
	srcCopy.TargetRevision = "1.2.4"
	specCopy.Source = &srcCopy
	in2.Spec = &specCopy
	if got := mustKey(t, in2); got == base {
		t.Error("expected different key when remote chart revision changes")
	}
}

// TestComputeKeyDependencyHermeticity pins the range-without-lock cache-bypass
// rule (GPT review P1): a local chart whose dependency version is a range and
// has no committed Chart.lock resolves against the mutable repo index, so it
// must NOT be cached. Exact pins, committed locks, and dependency-free charts
// stay cacheable.
func TestComputeKeyDependencyHermeticity(t *testing.T) {
	// helmInput builds a helm-like source at apps/foo with a Chart.yaml.
	helmInput := func(resolved map[string]string, chartYaml string) KeyInput {
		in := baseInput()
		in.Spec.Source.Helm = &cluster.ApplicationSourceHelm{}
		in.ResolveTree = mapResolver(resolved)
		if chartYaml != "" {
			in.ReadFile = fixedReader(chartYaml)
		}
		in.SameRepo = allRepo
		return in
	}
	base := map[string]string{
		"apps/foo":            "tree-foo",
		"apps/foo/Chart.yaml": "chart-yaml",
	}
	withLock := map[string]string{
		"apps/foo":            "tree-foo",
		"apps/foo/Chart.yaml": "chart-yaml",
		"apps/foo/Chart.lock": "lock-hash",
	}
	const exactDeps = `apiVersion: v2
name: foo
version: 1.0.0
dependencies:
  - name: cluster
    version: 0.3.0
    repository: https://cloudnative-pg.github.io/charts
  - name: other
    version: 0.3.1-1.5.2
    repository: oci://ghcr.io/example
`
	const rangeDeps = `apiVersion: v2
name: foo
version: 1.0.0
dependencies:
  - name: cluster
    version: ">=0.3.0"
    repository: https://cloudnative-pg.github.io/charts
`

	tests := []struct {
		name   string
		in     KeyInput
		wantOK bool
	}{
		{name: "exact pins, no lock: cacheable", in: helmInput(base, exactDeps), wantOK: true},
		{name: "range, no lock: bypass", in: helmInput(base, rangeDeps), wantOK: false},
		{name: "range, committed lock: cacheable", in: helmInput(withLock, rangeDeps), wantOK: true},
		{name: "no dependencies: cacheable", in: helmInput(base, "apiVersion: v2\nname: foo\nversion: 1.0.0\n"), wantOK: true},
		{name: "Chart.yaml unreadable (nil ReadFile): bypass", in: helmInput(base, ""), wantOK: false},
		{name: "malformed Chart.yaml: bypass", in: helmInput(base, "dependencies:\n\t- broken"), wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok := ComputeKey(tt.in)
			if ok != tt.wantOK {
				t.Errorf("ComputeKey ok = %v, want %v", ok, tt.wantOK)
			}
		})
	}
}

// TestExactSemver pins the exact-vs-range version classification.
func TestExactSemver(t *testing.T) {
	exact := []string{"0.3.0", "v1.2.3", "0.3.1-1.5.2", "1.2.3+build.7", " 1.0.0 "}
	ranges := []string{"", "^1.2.3", "~1.2", ">=0.3.0", "1.x", "1.2.*", "1.2", "1.2.3 - 1.4.0", "1.2.3 || 2.0.0", "*"}
	for _, v := range exact {
		if !exactSemver(v) {
			t.Errorf("exactSemver(%q) = false, want true", v)
		}
	}
	for _, v := range ranges {
		if exactSemver(v) {
			t.Errorf("exactSemver(%q) = true, want false", v)
		}
	}
}

// TestComputeKeyMixedLockAndUnlockedSources pins per-source hermeticity in one
// app: a multi-source app combining a locked chart with an unlocked exact-pin
// chart stays cacheable, while swapping the second chart's dependency to a
// version range makes the WHOLE app bypass (a combined render cannot be
// soundly cached if any ingredient is non-hermetic). Apps in the same repo are
// keyed independently, so a locked chart's app keeps caching regardless of
// what other charts in the repo look like.
func TestComputeKeyMixedLockAndUnlockedSources(t *testing.T) {
	build := func(chartBYaml string) KeyInput {
		in := baseInput()
		in.Spec.Source = nil
		in.Spec.Sources = []cluster.ApplicationSource{
			{RepoURL: "https://github.com/owner/repo", Path: "chart-a", Helm: &cluster.ApplicationSourceHelm{}},
			{RepoURL: "https://github.com/owner/repo", Path: "chart-b", Helm: &cluster.ApplicationSourceHelm{}},
		}
		in.ResolveTree = mapResolver(map[string]string{
			"chart-a":            "tree-a",
			"chart-a/Chart.yaml": "chart-a-yaml",
			"chart-a/Chart.lock": "chart-a-lock", // committed lock: hermetic, never read
			"chart-b":            "tree-b",
			"chart-b/Chart.yaml": "chart-b-yaml", // no lock: content decides
		})
		in.ReadFile = fixedReader(chartBYaml) // only chart-b is ever read
		in.SameRepo = allRepo
		return in
	}

	exact := "apiVersion: v2\nname: b\nversion: 1.0.0\ndependencies:\n  - name: dep\n    version: 0.3.0\n"
	ranged := "apiVersion: v2\nname: b\nversion: 1.0.0\ndependencies:\n  - name: dep\n    version: \">=0.3.0\"\n"

	if _, ok := ComputeKey(build(exact)); !ok {
		t.Error("locked chart + unlocked exact-pin chart: expected cacheable")
	}
	if _, ok := ComputeKey(build(ranged)); ok {
		t.Error("locked chart + unlocked range chart: expected bypass for the whole app")
	}
}
