package lint

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The report bodies below are real output, captured from kyverno 1.18.2 and
// conftest 0.68.2 while the built-in adapters were written. Hand-written JSON
// would only pin what we believe the tools emit.

const kyvernoReportJSON = `{"kind":"ClusterReport","apiVersion":"openreports.io/v1alpha1",
"metadata":{"name":"merged"},"source":"","summary":{"pass":1,"fail":1,"warn":0,"error":0,"skip":0},
"results":[
 {"source":"ValidatingAdmissionPolicy","policy":"disallow-latest-tag","rule":"disallow-latest-tag",
  "result":"pass","resources":[{"kind":"Deployment","namespace":"default","name":"fine-web","apiVersion":"apps/v1"}],
  "message":"ok"},
 {"source":"ValidatingAdmissionPolicy","policy":"disallow-latest-tag","rule":"disallow-latest-tag",
  "result":"fail","resources":[{"kind":"Deployment","namespace":"default","name":"cluster-info-web","apiVersion":"apps/v1"}],
  "message":"container images must be pinned to a tag (':latest' or tag-less images are not allowed)"}]}`

const conftestReportJSON = `[
 {"filename":"","namespace":"no_plaintext_credentials","successes":0,
  "failures":[{"msg":"ConfigMap/cluster-info-cm: data key \"note\" must not carry a plaintext credential",
               "metadata":{"query":"data.no_plaintext_credentials.deny"}}]},
 {"filename":"","namespace":"other_rules","successes":1}]`

func TestParseKyvernoReport(t *testing.T) {
	got := parseKyvernoReport("lint-kyverno policies/kyverno", kyvernoReportJSON)
	want := []string{
		"[kyverno/disallow-latest-tag] Deployment/cluster-info-web: container images must be pinned to a tag (':latest' or tag-less images are not allowed)",
	}
	if len(got) != len(want) || (len(got) > 0 && got[0] != want[0]) {
		t.Errorf("parseKyvernoReport() = %#v, want %#v", got, want)
	}
}

// The exact warning-line shape is a compatibility surface: e2e pins these bytes,
// and the shell adapters this replaces produced the identical format, so a
// reformat silently invalidates every stored expectation.
func TestParseKyvernoReportLineShape(t *testing.T) {
	tests := []struct {
		name   string
		report string
		want   []string
	}{
		{
			name:   "warn counts as a finding",
			report: `{"results":[{"policy":"p","result":"warn","resources":[{"kind":"Pod","name":"x"}],"message":"m"}]}`,
			want:   []string{"[kyverno/p] Pod/x: m"},
		},
		{
			name:   "pass and skip are not findings",
			report: `{"results":[{"policy":"p","result":"pass","message":"m"},{"policy":"p","result":"skip","message":"m"}]}`,
			want:   nil,
		},
		{
			// An error is kyverno failing to EVALUATE, not a resource failing a
			// check. Dropping these hid broken policies: with checks split across
			// many small policies (kyverno stops at the first failing validation
			// within one, so authors are pushed to split them), a broken CEL
			// expression simply stops producing findings.
			name:   "an error IS reported, marked to distinguish setup from manifests",
			report: `{"results":[{"policy":"p","result":"error","message":"expression 'x' resulted in error: no such key: y"}]}`,
			want:   []string{"[kyverno/p] ERROR expression 'x' resulted in error: no such key: y"},
		},
		{
			name:   "an error WITH a resource still names it",
			report: `{"results":[{"policy":"p","result":"error","resources":[{"kind":"Pod","name":"x"}],"message":"boom"}]}`,
			want:   []string{"[kyverno/p] ERROR Pod/x: boom"},
		},
		{
			// SYNTHETIC: kyverno 1.18 emits one resource per result for both
			// ValidatingAdmissionPolicy and ClusterPolicy (verified against the
			// real CLI), so this shape cannot be captured from it today. It is
			// pinned anyway because `resources` is a LIST in the openreports.io
			// schema — naming only the first would silently drop subjects the
			// moment any producer groups them.
			name:   "every resource gets its own line, not just the first",
			report: `{"results":[{"policy":"p","result":"fail","resources":[{"kind":"Deployment","name":"a"},{"kind":"StatefulSet","name":"b"}],"message":"m"}]}`,
			want:   []string{"[kyverno/p] Deployment/a: m", "[kyverno/p] StatefulSet/b: m"},
		},
		{
			name:   "multi-line message folds onto one line, since one line = one warning",
			report: `{"results":[{"policy":"p","result":"fail","resources":[{"kind":"Pod","name":"x"}],"message":"first\nsecond"}]}`,
			want:   []string{"[kyverno/p] Pod/x: first second"},
		},
		{
			name:   "no resources: message stands alone rather than naming null/null",
			report: `{"results":[{"policy":"p","result":"fail","message":"m"}]}`,
			want:   []string{"[kyverno/p] m"},
		},
		{
			name:   "no results at all",
			report: `{"results":[]}`,
			want:   nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseKyvernoReport("label", tt.report)
			if strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Errorf("got %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParseConftestReport(t *testing.T) {
	got := parseConftestReport("lint-conftest policies/conftest", conftestReportJSON)
	want := []string{
		`[conftest/no_plaintext_credentials] ConfigMap/cluster-info-cm: data key "note" must not carry a plaintext credential`,
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("parseConftestReport() = %#v, want %#v", got, want)
	}
}

func TestParseConftestReportFailuresBeforeWarnings(t *testing.T) {
	report := `[{"namespace":"ns","failures":[{"msg":"f1"},{"msg":"f2"}],"warnings":[{"msg":"w1"}]}]`
	got := parseConftestReport("label", report)
	want := []string{"[conftest/ns] f1", "[conftest/ns] f2", "[conftest/ns] w1"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// Unparsable output must surface rather than be swallowed: it means the tool
// changed its contract, which would otherwise look exactly like "no findings".
func TestParseReportsSurfaceUnparsableOutput(t *testing.T) {
	for _, parse := range []func(string, string) []string{parseKyvernoReport, parseConftestReport} {
		got := parse("label", "not json at all")
		if len(got) != 1 || !strings.Contains(got[0], "unparsable report") {
			t.Errorf("got %#v, want one 'unparsable report' warning", got)
		}
	}
}

func TestResolvePolicyDir(t *testing.T) {
	worktree := t.TempDir()

	populated := filepath.Join(worktree, "policies", "kyverno")
	if err := os.MkdirAll(populated, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(populated, "policy.yaml"), []byte("kind: X\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(worktree, "policies", "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		dir    string
		wantOK bool
	}{
		// The relative case is the one that matters: it resolves against the
		// SIDE's worktree, which is what lints each branch with its own policies.
		{name: "relative dir with policies", dir: "policies/kyverno", wantOK: true},
		{name: "absolute dir with policies", dir: populated, wantOK: true},
		// Missing and empty must both report "nothing here" rather than an error:
		// the base side of a PR adding the first policy has no directory yet, and
		// both tools treat an empty policy set as a hard failure.
		{name: "missing dir", dir: "policies/typo", wantOK: false},
		{name: "empty dir", dir: "policies/empty", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := resolvePolicyDir(worktree, tt.dir)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != populated {
				t.Errorf("dir = %q, want %q", got, populated)
			}
		})
	}
}

// A side with no policies produces no findings AND no warning: it is the normal
// shape, not a failure, and warning about it would fire on every application.
func TestBuiltinsSkipSideWithoutPolicies(t *testing.T) {
	r := &Runner{Kyverno: []string{"policies/kyverno"}, Conftest: []string{"policies/conftest"}, KubeContext: "ctx"}
	if got := r.Lint(context.Background(), t.TempDir(), "kind: ConfigMap\n"); got != nil {
		t.Errorf("Lint() = %#v, want nil", got)
	}
}

// Without a resolved context kyverno would fall back to the ambient one and
// report findings about a DIFFERENT cluster, which is indistinguishable in the
// report from a correct result. It must refuse instead.
func TestKyvernoRefusesWithoutKubeContext(t *testing.T) {
	worktree := t.TempDir()
	dir := filepath.Join(worktree, "policies", "kyverno")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "policy.yaml"), []byte("kind: X\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := &Runner{Kyverno: []string{"policies/kyverno"}} // no KubeContext
	got := r.Lint(context.Background(), worktree, "kind: ConfigMap\n")
	if len(got) != 1 || !strings.Contains(got[0], "no resolved kube context") {
		t.Fatalf("got %#v, want one refusal warning", got)
	}
}

// execTool carries the health contract that the shell adapters get wrong most
// often, so it is pinned directly. `sh` stands in for a policy tool: any binary
// exercises the same branching.
func TestExecToolHealthContract(t *testing.T) {
	tests := []struct {
		name        string
		script      string
		wantStdout  string
		wantWarning string
	}{
		{
			name:   "exit 0, no output: nothing to say, NOT a failure",
			script: "exit 0",
		},
		{
			name:       "non-zero WITH a report: normal, both tools exit non-zero on findings",
			script:     `echo '{"results":[]}'; exit 1`,
			wantStdout: `{"results":[]}`,
		},
		{
			name:        "non-zero with NO report: a real failure that must surface",
			script:      "echo boom >&2; exit 3",
			wantWarning: "exit status 3 with no report output: boom",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Runner{Timeout: 10 * time.Second}
			out := r.execTool(context.Background(), "", "label", []string{"sh", "-c", tt.script}, "")
			if out.stdout != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", out.stdout, tt.wantStdout)
			}
			switch {
			case tt.wantWarning == "" && out.warning != "":
				t.Errorf("warning = %q, want none", out.warning)
			case tt.wantWarning != "" && !strings.Contains(out.warning, tt.wantWarning):
				t.Errorf("warning = %q, want it to contain %q", out.warning, tt.wantWarning)
			}
		})
	}
}

func TestExecToolTimeout(t *testing.T) {
	r := &Runner{Timeout: 50 * time.Millisecond}
	out := r.execTool(context.Background(), "", "label", []string{"sh", "-c", "sleep 5"}, "")
	if !strings.Contains(out.warning, "timeout after 50ms") {
		t.Errorf("warning = %q, want a timeout warning", out.warning)
	}
}

// A timeout must surface even when the tool already wrote something, and the
// partial output must be kept. Suppressing the warning because stdout is
// non-empty is how a truncated report becomes "silently fewer findings" — the
// report looks cleaner than reality with nothing to indicate the run was cut off.
func TestExecToolTimeoutWithPartialOutput(t *testing.T) {
	r := &Runner{Timeout: 100 * time.Millisecond}
	// `exec sleep` so the timeout's kill actually ends the process tree: a FORKED
	// descendant inherits the stdout pipe and cmd.Wait blocks on it until that
	// child exits, so `printf; sleep 5` would take the full 5s despite the 100ms
	// timeout. Worth knowing beyond this test — a lint command that backgrounds
	// something can outlive --lint-timeout the same way.
	out := r.execTool(context.Background(), "", "label",
		[]string{"sh", "-c", `printf '{"results":[]}'; exec sleep 5`}, "")

	if !strings.Contains(out.warning, "timeout after 100ms") {
		t.Errorf("warning = %q, want a timeout warning despite the partial output", out.warning)
	}
	if out.stdout != `{"results":[]}` {
		t.Errorf("stdout = %q, want the partial output preserved", out.stdout)
	}
}

// The built-in adapters point the tool at argocdf's kubeconfig by APPENDING
// KUBECONFIG, without the scrub childEnv does for ARGOCDF_*. That reads like the
// first-wins hazard scrubbed elsewhere, so the actual behavior is pinned:
// exec.Cmd deduplicates Env keeping the LAST value, so the child sees argocdf's
// path and never the inherited one.
func TestExecToolKubeconfigOverridesInherited(t *testing.T) {
	t.Setenv("KUBECONFIG", "/inherited/should/lose")

	r := &Runner{
		Kubeconfig: "/argocdf/should/win",
		Timeout:    10 * time.Second,
		// Non-empty so childEnv returns a built slice rather than nil, covering
		// the branch where the append lands on top of scrubbed entries.
		Env: map[string]string{"ARGOCDF_CONTEXT": "ctx"},
	}
	out := r.execTool(context.Background(), "", "label",
		[]string{"sh", "-c", `printf '%s' "$KUBECONFIG"`}, "")

	if out.stdout != "/argocdf/should/win" {
		t.Errorf("child KUBECONFIG = %q, want argocdf's value to win over the inherited one", out.stdout)
	}
}

func TestConfigured(t *testing.T) {
	tests := []struct {
		name string
		r    Runner
		want bool
	}{
		{name: "nothing", r: Runner{}, want: false},
		{name: "shell command only", r: Runner{Commands: []string{"true"}}, want: true},
		{name: "kyverno only", r: Runner{Kyverno: []string{"d"}}, want: true},
		{name: "conftest only", r: Runner{Conftest: []string{"d"}}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.r.Configured(); got != tt.want {
				t.Errorf("Configured() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --lint-timeout must bound WALL-CLOCK, not just signal the child. A command that
// forks leaves a descendant holding the inherited stdout pipe, and cmd.Wait blocks
// on that pipe until the descendant exits on its own — so without WaitDelay a
// `sleep 30` behind a 100ms timeout costs 30 seconds, and a suite of them costs
// minutes. This is the shape the built-ins and the shell path share.
func TestExecToolTimeoutBoundsWallClock(t *testing.T) {
	r := &Runner{Timeout: 100 * time.Millisecond}

	start := time.Now()
	// No `exec`: sh forks, so killing sh leaves `sleep` holding the pipe.
	out := r.execTool(context.Background(), "", "label",
		[]string{"sh", "-c", `printf 'partial'; sleep 30`}, "")
	elapsed := time.Since(start)

	if !strings.Contains(out.warning, "timeout after 100ms") {
		t.Errorf("warning = %q, want a timeout warning", out.warning)
	}
	// Generous bound: the point is "seconds, not the full sleep", so the assertion
	// stays well clear of waitDelay without becoming flaky on a loaded machine.
	if elapsed > 10*time.Second {
		t.Errorf("took %s, want the timeout to bound wall-clock (WaitDelay = %s)", elapsed, waitDelay)
	}
}

func TestRunOneTimeoutBoundsWallClock(t *testing.T) {
	r := &Runner{Commands: []string{`printf 'partial\n'; sleep 30`}, Timeout: 100 * time.Millisecond}

	start := time.Now()
	warnings := r.Lint(context.Background(), "", "")
	elapsed := time.Since(start)

	if len(warnings) == 0 || !strings.Contains(warnings[len(warnings)-1], "timeout after 100ms") {
		t.Errorf("warnings = %#v, want a trailing timeout warning", warnings)
	}
	if elapsed > 10*time.Second {
		t.Errorf("took %s, want the timeout to bound wall-clock", elapsed)
	}
}
