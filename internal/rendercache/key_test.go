package rendercache

import (
	"testing"

	"github.com/rgeraskin/argocdf/internal/cluster"
)

// fixedResolver returns a constant tree hash for any path.
func fixedResolver(hash string) func(commit, path string) (string, bool) {
	return func(_, _ string) (string, bool) { return hash, true }
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
