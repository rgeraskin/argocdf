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
