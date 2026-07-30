package rendercache

import (
	"path"
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

func TestComputeKeyChangesWithKustomizeBuildOptions(t *testing.T) {
	base := mustKey(t, baseInput())
	in := baseInput()
	in.Options.KustomizeBuildOptions = "--enable-alpha-plugins"
	if got := mustKey(t, in); got == base {
		t.Error("expected different key when KustomizeBuildOptions changes")
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
// file (resolved against a same-repo ref source) changes the key. The path
// resolves against the ref repository ROOT — never the ref source's Path —
// matching the engine (ArgoCD's RefTarget has no Path field).
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
				// A ref source's Path must NOT shift value-file resolution:
				// only "env/prod.yaml" (repo-root-relative) may be hashed.
				Path: "config",
			},
		}
		in.ResolveTree = mapResolver(map[string]string{
			"chart":         "tree-chart",
			"env/prod.yaml": vfHash,
			"":              "root-tree", // ref source uses root tree
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

// The exact-vs-range version classification now lives in
// render.IsImmutableChartVersion (one predicate for both caches) and is pinned
// by render's TestIsImmutableChartVersion; the ComputeKey tests here exercise
// it through dependency hermeticity and remote-chart bypasses.

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

// pathRecordingResolver returns a fixed hash for every path and records which
// paths were asked for, so a test can assert what the key actually hashed.
func pathRecordingResolver(asked *[]string, hashes map[string]string) func(commit, path string) (string, bool) {
	return func(_, p string) (string, bool) {
		*asked = append(*asked, p)
		if h, ok := hashes[p]; ok {
			return h, true
		}
		return "treehash-1", true
	}
}

func askedFor(asked []string, want string) bool {
	for _, p := range asked {
		if p == want {
			return true
		}
	}
	return false
}

// TestComputeKeyAbsoluteValueFileIsRepoRootRelative: an absolute helm value-file
// entry is repo-ROOT-relative in ArgoCD, not filesystem-absolute. The key used to
// bypass on the leading "/", which made every app with a /config/prod.yaml
// permanently uncacheable while SELECTION matched that same file happily - the two
// layers disagreeing about which files an app reads.
func TestComputeKeyAbsoluteValueFileIsRepoRootRelative(t *testing.T) {
	var asked []string
	in := baseInput()
	in.SameRepo = allRepo
	in.ReadFile = fixedReader("")
	in.Spec.Source.Helm = &cluster.ApplicationSourceHelm{ValueFiles: []string{"/config/prod.yaml"}}
	in.ResolveTree = pathRecordingResolver(&asked, nil)

	if _, ok := ComputeKey(in); !ok {
		t.Fatal("ComputeKey bypassed an absolute value file; ArgoCD resolves it from the repository root")
	}
	if !askedFor(asked, "config/prod.yaml") {
		t.Errorf("key did not hash the repo-root-relative path; asked for %v", asked)
	}
}

// ...and its content must actually move the key, or "cacheable" would just mean
// "silently ignores that file".
func TestComputeKeyAbsoluteValueFileContentParticipates(t *testing.T) {
	keyWith := func(hash string) string {
		var asked []string
		in := baseInput()
		in.SameRepo = allRepo
		in.ReadFile = fixedReader("")
		in.Spec.Source.Helm = &cluster.ApplicationSourceHelm{ValueFiles: []string{"/config/prod.yaml"}}
		in.ResolveTree = pathRecordingResolver(&asked, map[string]string{"config/prod.yaml": hash})
		return mustKey(t, in)
	}

	if keyWith("blob-a") == keyWith("blob-b") {
		t.Error("editing an absolute value file did not change the cache key")
	}
}

// TestComputeKeyRemoteValueFileBypasses: a remote (http/https) value file has no
// repository content to hash, and it can change under a FIXED commit - so it must
// bypass. The previous code joined the URL onto the chart path, failed to resolve
// that nonsense path, and recorded it as "absent", producing a cacheable key for a
// render whose inputs live outside git.
func TestComputeKeyRemoteValueFileBypasses(t *testing.T) {
	for _, url := range []string{"https://example.test/v.yaml", "http://example.test/v.yaml"} {
		in := baseInput()
		in.SameRepo = allRepo
		in.ReadFile = fixedReader("")
		in.Spec.Source.Helm = &cluster.ApplicationSourceHelm{ValueFiles: []string{url}}
		in.ResolveTree = fixedResolver("treehash-1")

		if _, ok := ComputeKey(in); ok {
			t.Errorf("ComputeKey cached a render reading %s; its content is not in the repository", url)
		}
	}
}

// TestComputeKeyRelativeValueFilesUnchanged pins that delegating to ArgoCD's
// resolver did NOT move the keys of the shapes that already worked - which is why
// SchemaVersion does not need another bump.
func TestComputeKeyRelativeValueFilesUnchanged(t *testing.T) {
	for _, entry := range []string{"values.yaml", "../shared/vals.yaml", "nested/dir/vals.yaml"} {
		var asked []string
		in := baseInput()
		in.SameRepo = allRepo
		in.ReadFile = fixedReader("")
		in.Spec.Source.Helm = &cluster.ApplicationSourceHelm{ValueFiles: []string{entry}}
		in.ResolveTree = pathRecordingResolver(&asked, nil)

		if _, ok := ComputeKey(in); !ok {
			t.Fatalf("entry %q became uncacheable", entry)
		}
		want := path.Clean(path.Join("apps/foo", entry))
		if !askedFor(asked, want) {
			t.Errorf("entry %q hashed something other than %q; asked for %v", entry, want, asked)
		}
	}
}

// TestComputeKeyChangesWithHelmRepoAlias: a helm repository alias is a render
// INPUT, not just an access mechanism. ArgoCD runs `helm repo add <name> <url>` for
// every non-OCI repo in the active credential list before a dependency build, so a
// chart depending on `repository: "@myrepo"` resolves that name from the list. Point
// the name somewhere else - by switching --repo-creds, or by editing one source -
// and the same commit renders different dependency content.
func TestComputeKeyChangesWithHelmRepoAlias(t *testing.T) {
	base := func(aliases ...HelmRepoAlias) KeyInput {
		in := baseInput()
		in.HelmRepoAliases = aliases
		return in
	}

	none := mustKey(t, base())
	one := mustKey(t, base(HelmRepoAlias{Name: "myrepo", URL: "https://charts.example.test"}))
	moved := mustKey(t, base(HelmRepoAlias{Name: "myrepo", URL: "https://mirror.example.test"}))
	renamed := mustKey(t, base(HelmRepoAlias{Name: "other", URL: "https://charts.example.test"}))

	if none == one {
		t.Error("adding an alias did not change the key")
	}
	if one == moved {
		t.Error("pointing the same alias at another URL did not change the key - the stale-render case this exists for")
	}
	if one == renamed {
		t.Error("renaming an alias did not change the key")
	}
}

// The key must not depend on the order a credential source happened to list its
// repositories, or two runs of the same configuration would miss each other.
func TestComputeKeyHelmRepoAliasOrderIndependent(t *testing.T) {
	a := HelmRepoAlias{Name: "alpha", URL: "https://a.example.test"}
	b := HelmRepoAlias{Name: "beta", URL: "https://b.example.test"}

	in1 := baseInput()
	in1.HelmRepoAliases = []HelmRepoAlias{a, b}
	in2 := baseInput()
	in2.HelmRepoAliases = []HelmRepoAlias{b, a}

	if mustKey(t, in1) != mustKey(t, in2) {
		t.Error("key depends on alias order; the same configuration would not hit its own entries")
	}
}

// TestComputeKeyChangesWithRepoCredsMode pins the verification workflow: render
// with --repo-creds local, then re-run with cluster to check that ArgoCD's own
// credentials can produce the same manifests. If the second run is served from the
// first one's cache it answers "yes" without ever consulting the cluster, and the
// merge fails on a repository ArgoCD cannot reach.
//
// The alias fingerprint cannot cover this: a missing OCI registry, a credential
// template with no repository name, and a wrong password all leave the alias list
// identical - which is why the mode is hashed as well as the mappings.
func TestComputeKeyChangesWithRepoCredsMode(t *testing.T) {
	keyFor := func(mode string) string {
		in := baseInput()
		in.RepoCredsMode = mode
		// Identical mappings on both sides: the point is that the MODE alone must
		// separate them, since that is all that differs in the failing scenario.
		in.HelmRepoAliases = []HelmRepoAlias{{Name: "stable", URL: "https://charts.example.test"}}
		return mustKey(t, in)
	}

	cluster, local, none := keyFor("cluster"), keyFor("local"), keyFor("none")
	if cluster == local {
		t.Error("local and cluster renders share a key: a cluster-creds verification run would be answered by the local run")
	}
	if cluster == none || local == none {
		t.Error("--repo-creds none shares a key with a credentialed mode")
	}
	if keyFor("cluster") != cluster {
		t.Error("the same mode produced two different keys")
	}
}

// chartInput builds a remote-chart KeyInput at the given target revision.
func chartInput(revision string) KeyInput {
	return KeyInput{
		AppName:   "chart-app",
		Namespace: "argocd",
		Spec: &cluster.ApplicationSpec{
			Source: &cluster.ApplicationSource{
				RepoURL:        "https://charts.example.com",
				Chart:          "nginx",
				TargetRevision: revision,
			},
		},
		KubeVersion: "1.29.0",
		Commit:      "deadbeef",
	}
}

// TestComputeKeyMutableChartRevisionBypasses pins the remote-chart half of the
// immutability rule (GPT review #1): a chart whose target revision helm
// resolves against the mutable registry index - HEAD, "*", empty, or a
// constraint range - can produce different content over time under IDENTICAL
// key inputs, so it must never be cached. Before this rule the key hashed the
// literal revision string: render a chart at HEAD, publish a newer chart, and
// the same Git comparison kept returning the previous manifests forever.
func TestComputeKeyMutableChartRevisionBypasses(t *testing.T) {
	for _, rev := range []string{"HEAD", "", "*", "^2.0.0", "~1.2", "1.x", "1.2", ">=1.0.0"} {
		if _, ok := ComputeKey(chartInput(rev)); ok {
			t.Errorf("revision %q: ComputeKey ok = true, want bypass (mutable revisions must not be cached)", rev)
		}
	}
	// Exact versions stay cacheable - including prerelease/build metadata.
	for _, rev := range []string{"1.2.3", "v1.2.3", "0.3.1-rc.1", "1.2.3+build.7"} {
		mustKey(t, chartInput(rev))
	}
}

// TestComputeKeyAPIVersions pins the API-version-set rules (GPT review #2):
// the set is a render input (.Capabilities.APIVersions.Has), so membership
// changes must miss, while discovery ORDER and duplicates are not render
// inputs and must not thrash the cache. Nil and empty both mean "helm gets no
// --api-versions" (the --no-api-versions toggle) and hash identically.
func TestComputeKeyAPIVersions(t *testing.T) {
	withAPIVersions := func(vs []string) KeyInput {
		in := baseInput()
		in.APIVersions = vs
		return in
	}

	base := mustKey(t, withAPIVersions([]string{"batch/v1", "cert-manager.io/v1"}))

	if mustKey(t, withAPIVersions([]string{"cert-manager.io/v1", "batch/v1"})) != base {
		t.Error("discovery order changed the key")
	}
	if mustKey(t, withAPIVersions([]string{"batch/v1", "batch/v1", "cert-manager.io/v1"})) != base {
		t.Error("a duplicate entry changed the key")
	}
	if mustKey(t, withAPIVersions([]string{"batch/v1"})) == base {
		t.Error("removing an API version (a CRD uninstalled) did not change the key")
	}
	if mustKey(t, withAPIVersions(nil)) == base {
		t.Error("disabling API versions (--no-api-versions) did not change the key")
	}
	if mustKey(t, withAPIVersions(nil)) != mustKey(t, withAPIVersions([]string{})) {
		t.Error("nil and empty API-version sets hash differently")
	}
}

// TestComputeKeyRepoCredsInstance pins the instance half of credential-source
// keying (GPT review #4): two clusters both read with --repo-creds=cluster are
// two credential sources, and verifying cluster B's credentials must not be
// answered from entries cluster A rendered. The instance sits BESIDE the mode
// in the key, so local/none (empty instance) behave exactly as before.
func TestComputeKeyRepoCredsInstance(t *testing.T) {
	withInstance := func(inst string) KeyInput {
		in := baseInput()
		in.RepoCredsMode = "cluster"
		in.RepoCredsInstance = inst
		return in
	}

	prod := mustKey(t, withInstance("prod-cluster\x00argocd"))
	if mustKey(t, withInstance("staging-cluster\x00argocd")) == prod {
		t.Error("two contexts share a render-cache key")
	}
	if mustKey(t, withInstance("prod-cluster\x00argocd-team")) == prod {
		t.Error("two ArgoCD namespaces share a render-cache key")
	}
	if mustKey(t, withInstance("prod-cluster\x00argocd")) != prod {
		t.Error("instance keying is not deterministic")
	}
}
