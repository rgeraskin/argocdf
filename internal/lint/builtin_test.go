package lint

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
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
	got := parseKyvernoReport(identify(kindKyverno, 1), kyvernoReportJSON).lines
	want := []string{
		"[lint-kyverno#1/disallow-latest-tag] Deployment/cluster-info-web: container images must be pinned to a tag (':latest' or tag-less images are not allowed)",
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
			want:   []string{"[lint-kyverno#1/p] Pod/x: m"},
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
			want:   []string{"[lint-kyverno#1/p] ERROR expression 'x' resulted in error: no such key: y"},
		},
		{
			name:   "an error WITH a resource still names it",
			report: `{"results":[{"policy":"p","result":"error","resources":[{"kind":"Pod","name":"x"}],"message":"boom"}]}`,
			want:   []string{"[lint-kyverno#1/p] ERROR Pod/x: boom"},
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
			want:   []string{"[lint-kyverno#1/p] Deployment/a: m", "[lint-kyverno#1/p] StatefulSet/b: m"},
		},
		{
			name:   "multi-line message folds onto one line, since one line = one warning",
			report: `{"results":[{"policy":"p","result":"fail","resources":[{"kind":"Pod","name":"x"}],"message":"first\nsecond"}]}`,
			want:   []string{"[lint-kyverno#1/p] Pod/x: first second"},
		},
		{
			name:   "no resources: message stands alone rather than naming null/null",
			report: `{"results":[{"policy":"p","result":"fail","message":"m"}]}`,
			want:   []string{"[lint-kyverno#1/p] m"},
		},
		{
			name:   "no results at all",
			report: `{"results":[]}`,
			want:   nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseKyvernoReport(identify(kindKyverno, 1), tt.report).lines
			if strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Errorf("got %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParseConftestReport(t *testing.T) {
	got := parseConftestReport(identify(kindConftest, 1), conftestReportJSON).lines
	want := []string{
		`[lint-conftest#1/no_plaintext_credentials] ConfigMap/cluster-info-cm: data key "note" must not carry a plaintext credential`,
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("parseConftestReport() = %#v, want %#v", got, want)
	}
}

func TestParseConftestReportFailuresBeforeWarnings(t *testing.T) {
	report := `[{"namespace":"ns","failures":[{"msg":"f1"},{"msg":"f2"}],"warnings":[{"msg":"w1"}]}]`
	got := parseConftestReport(identify(kindConftest, 1), report).lines
	want := []string{"[lint-conftest#1/ns] f1", "[lint-conftest#1/ns] f2", "[lint-conftest#1/ns] w1"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// Unparsable output must surface rather than be swallowed: it means the tool
// changed its contract, which would otherwise look exactly like "no findings".
// It counts as FAILED, not as one finding: the tool exited 0 and said something
// argocdf cannot read, so the manifests went unchecked.
func TestParseReportsSurfaceUnparsableOutput(t *testing.T) {
	// The identity must MATCH the parser: running conftest's parser under a kyverno
	// identity would mislabel every line the moment this asserted on the bracket.
	parsers := []struct {
		kind  string
		parse func(linterID, string) result
	}{
		{kind: kindKyverno, parse: parseKyvernoReport},
		{kind: kindConftest, parse: parseConftestReport},
	}
	for _, p := range parsers {
		id := identify(p.kind, 1)
		got := p.parse(id, "not json at all")
		if !strings.HasPrefix(got.lines[0], id.bracket()) {
			t.Errorf("line %q does not open with %s", got.lines[0], id.bracket())
		}
		if len(got.lines) != 1 || !strings.Contains(got.lines[0], "unparsable report") {
			t.Errorf("got %#v, want one 'unparsable report' warning", got.lines)
		}
		if got.status != statusFailed {
			t.Errorf("status = %q, want %q", got.status, statusFailed)
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
			got, ok, err := resolvePolicyDir(worktree, tt.dir, KyvernoPolicyExts)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != populated {
				t.Errorf("dir = %q, want %q", got, populated)
			}
		})
	}
}

// A side with no policies is not a failure - it is the normal shape when a change
// adds the first policy - but it is not silent either. Silence made the report
// assert something false: an unlinted side produces findings on the OTHER side
// only, and a one-sided finding reads as "introduced by this change" when it is
// really "pre-existing, newly detected". One note per side says so, in the same
// warning list and the same [base]/[target] vocabulary as the findings.
func TestBuiltinsNoteSideWithoutPolicies(t *testing.T) {
	r := &Runner{Kyverno: []string{"policies/kyverno"}, Conftest: []string{"policies/conftest"}, KubeContext: "ctx"}
	got := r.Lint(context.Background(), Subject{Worktree: t.TempDir()}, "kind: ConfigMap\n")
	want := []string{
		`[lint-kyverno#1] not linted: no policies in "policies/kyverno"`,
		`[lint-conftest#1] not linted: no policies in "policies/conftest"`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Lint() =\n%#v\nwant\n%#v", got, want)
	}
}

// The note is what an invocation CONTRIBUTED, so it counts in lines - while status
// is what says the linter never ran. Pinned because the pair reads like a
// contradiction (skipped, yet one line) and is easy to "fix" back to lines=0,
// which would make a skip indistinguishable from a clean run again.
func TestSkipNoteIsLoggedAsSkippedWithOneLine(t *testing.T) {
	r := &Runner{Kyverno: []string{"policies/kyverno"}}
	lines := lintLogLines(t, r)
	line := lineWith(t, lines, "linter=kyverno#1")
	for _, want := range []string{"status=skipped", "lines=1", "INFO"} {
		if !strings.Contains(line, want) {
			t.Errorf("log line %q, want %s", line, want)
		}
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
	got := r.Lint(context.Background(), Subject{Worktree: worktree}, "kind: ConfigMap\n")
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
	warnings := r.Lint(context.Background(), Subject{}, "")
	elapsed := time.Since(start)

	if len(warnings) == 0 || !strings.Contains(warnings[len(warnings)-1], "timeout after 100ms") {
		t.Errorf("warnings = %#v, want a trailing timeout warning", warnings)
	}
	if elapsed > 10*time.Second {
		t.Errorf("took %s, want the timeout to bound wall-clock", elapsed)
	}
}

// A finding's bracket carries the same identity as every other line about that
// linter - the FLAG - so one grep finds all of it. The ordinal is what keeps two
// directories of the same tool apart when both hold a policy of the same name: the
// collision the ordinals exist for, one level in from the flags.
func TestFindingBracketCarriesTheLinterIdentity(t *testing.T) {
	report := `{"results":[{"policy":"same-name","result":"fail","resources":[{"kind":"Pod","name":"x"}],"message":"m"}]}`

	first := parseKyvernoReport(identify(kindKyverno, 1), report).lines
	second := parseKyvernoReport(identify(kindKyverno, 2), report).lines

	if len(first) != 1 || first[0] != "[lint-kyverno#1/same-name] Pod/x: m" {
		t.Errorf("first dir = %#v, want [lint-kyverno#1/same-name] Pod/x: m", first)
	}
	if len(second) != 1 || second[0] != "[lint-kyverno#2/same-name] Pod/x: m" {
		t.Errorf("second dir = %#v, want [lint-kyverno#2/same-name] Pod/x: m", second)
	}
}

// healthLine is one shape argocdf authors about a linter itself.
//
// failure marks the shapes the e2e review gate must REFUSE to pin: a crashed or
// unusable linter must never become a stored expectation. The gate enforces that by
// pattern, in scripts/e2e/review-expected.sh, and TestReviewGateBansEveryFailureShape
// checks the two against each other - this table is the single list both read, so
// adding a shape here without a ban there fails a test instead of quietly hollowing
// the gate (which is exactly what happened when the unusable-directory line landed).
type healthLine struct {
	name    string
	run     func() []string
	want    string
	failure bool
}

// healthLineShapes builds the table against a worktree it populates.
func healthLineShapes(t *testing.T, worktree string) []healthLine {
	t.Helper()
	populated := filepath.Join(worktree, "policies", "kyverno")
	if err := os.MkdirAll(populated, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(populated, "p.yaml"), []byte("kind: X\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "afile"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	return []healthLine{
		{
			name: "shell command exits non-zero",
			run: func() []string {
				return (&Runner{Commands: []string{"exit 3"}, Timeout: 5 * time.Second}).
					Lint(context.Background(), Subject{}, "")
			},
			want:    "[lint#1] exit status 3",
			failure: true,
		},
		{
			name: "shell command times out",
			run: func() []string {
				return (&Runner{Commands: []string{"sleep 30"}, Timeout: 50 * time.Millisecond}).
					Lint(context.Background(), Subject{}, "")
			},
			want:    "[lint#1] timeout after 50ms",
			failure: true,
		},
		{
			name: "built-in refuses without a resolved context",
			run: func() []string {
				return (&Runner{Kyverno: []string{"policies/kyverno"}}).
					Lint(context.Background(), Subject{Worktree: worktree}, "")
			},
			want:    "[lint-kyverno#1] no resolved kube context, refusing to lint against an unknown cluster",
			failure: true,
		},
		{
			// A path that cannot hold policies at all. It names the directory the
			// USER spelled, not the resolved absolute one: this line reaches PR
			// comments, where a per-run worktree path is noise.
			name: "built-in cannot use the policy path",
			run: func() []string {
				return (&Runner{Kyverno: []string{"afile"}, KubeContext: "ctx"}).
					Lint(context.Background(), Subject{Worktree: worktree}, "")
			},
			want:    `[lint-kyverno#1] unusable policy directory "afile": not a directory`,
			failure: true,
		},
		{
			// The ONE health line that keeps its argument: it is about that
			// directory being absent, so naming it is the content of the note.
			name: "built-in skips a side with no policies",
			run: func() []string {
				return (&Runner{Conftest: []string{"policies/absent"}}).
					Lint(context.Background(), Subject{Worktree: worktree}, "")
			},
			// The one health line that is NOT a failure, and therefore the one the
			// gate must keep pinnable: case/lint-policy-added pins exactly this.
			want: `[lint-conftest#1] not linted: no policies in "policies/absent"`,
		},
		{
			// A tool that answered with something unreadable. No subprocess needed:
			// the parser is the thing that decides.
			name: "built-in cannot parse the report",
			run: func() []string {
				return parseKyvernoReport(identify(kindKyverno, 1), "not json").lines
			},
			want:    "[lint-kyverno#1] unparsable report: invalid character 'o' in literal null (expecting 'u')",
			failure: true,
		},
	}
}

// The SHAPES of the lines argocdf authors about a linter, pinned byte-exactly.
// Two things depend on them beyond readability: the e2e review gate bans the failure
// ones from being pinned as expectations, and it keys on these shapes to do it. A
// reformat here silently hollows that ban, which is how a crashing adapter would
// start passing review.
func TestHealthLineShapes(t *testing.T) {
	worktree := t.TempDir()
	for _, tt := range healthLineShapes(t, worktree) {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.run()
			if len(got) != 1 || got[0] != tt.want {
				t.Errorf("got %#v, want [%q]", got, tt.want)
			}
		})
	}
}

// TestReviewGateBansEveryFailureShape mechanizes the "must be kept in step with
// TestHealthLineShapes" comment in scripts/e2e/review-expected.sh.
//
// The gate refuses to pin a lint FAILURE line, so that a crashed or unusable linter
// cannot become a stored expectation. It does that by pattern, in another language,
// in another directory - coupled to these shapes by nothing but that comment, which
// is precisely what a hurried change loses to: the unusable-policy-directory line
// shipped with no ban, and was caught by review rather than by the suite.
//
// So both lists are read here: every failure shape must match at least one ban, and
// the skip note must match NONE (case/lint-policy-added pins it, and a ban would
// make that case unpinnable).
func TestReviewGateBansEveryFailureShape(t *testing.T) {
	const script = "../../scripts/e2e/review-expected.sh"
	source, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("cannot read the review gate (%s): %v", script, err)
	}

	// ban "$name" "$rep" '<ERE>' "<label>"
	banLine := regexp.MustCompile(`(?m)^\s*ban "\$name" "\$rep" '([^']*)'`)
	matches := banLine.FindAllStringSubmatch(string(source), -1)
	if len(matches) == 0 {
		t.Fatalf("no ban patterns found in %s - has the helper been renamed?", script)
	}
	var bans []*regexp.Regexp
	for _, m := range matches {
		re, err := regexp.Compile(m[1])
		if err != nil {
			// The gate uses grep -E; a pattern Go cannot compile is one this test
			// cannot check, which must not pass silently.
			t.Fatalf("ban pattern %q does not compile as a Go regexp: %v", m[1], err)
		}
		bans = append(bans, re)
	}

	for _, tt := range healthLineShapes(t, t.TempDir()) {
		t.Run(tt.name, func(t *testing.T) {
			var matched []string
			for _, re := range bans {
				if re.MatchString(tt.want) {
					matched = append(matched, re.String())
				}
			}
			switch {
			case tt.failure && len(matched) == 0:
				t.Errorf("no ban in %s matches the failure line %q - a case pinning it would pass review", script, tt.want)
			case !tt.failure && len(matched) > 0:
				t.Errorf("line %q is banned by %v, but it is not a failure and is legitimately pinned", tt.want, matched)
			}
		})
	}
}

// "Has entries" and "has policies" are different questions, and using the first
// made the skip note wrong in ordinary layouts: a directory holding only a
// .gitkeep, a README or a nested empty directory looks populated while the tool has
// nothing to apply — kyverno then exits 0 with no results, which argocdf reported as
// status=ok, a linter that silently checked nothing.
func TestHasPolicies(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("yaml/policy.yaml", "kind: X\n")
	write("nested/sub/deep/policy.yml", "kind: X\n")
	write("keep-only/.gitkeep", "")
	write("keep-only/README.md", "notes\n")
	write("rego/deny.rego", "package x\n")
	write("mixed/README.md", "notes\n")
	write("mixed/sub/policy.json", "{}\n")
	if err := os.MkdirAll(filepath.Join(root, "empty", "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	write("afile", "x")
	// `policies/kyverno -> ../shared/policies` is an ordinary monorepo layout, and
	// both tools read through it when handed the path.
	if err := os.Symlink(filepath.Join(root, "yaml"), filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		dir     string
		exts    []string
		want    bool
		wantErr bool
	}{
		{name: "a yaml policy", dir: "yaml", exts: KyvernoPolicyExts, want: true},
		// Both tools recurse, so the check must too, or a perfectly good layout is
		// reported as having no policies.
		{name: "a policy nested three deep", dir: "nested", exts: KyvernoPolicyExts, want: true},
		{name: "json counts for kyverno", dir: "mixed", exts: KyvernoPolicyExts, want: true},
		{name: "only a .gitkeep and a README", dir: "keep-only", exts: KyvernoPolicyExts},
		{name: "only empty child directories", dir: "empty", exts: KyvernoPolicyExts},
		// Extensions are per TOOL: the same directory answers differently.
		{name: "rego is a conftest policy", dir: "rego", exts: ConftestPolicyExts, want: true},
		{name: "rego is NOT a kyverno policy", dir: "rego", exts: KyvernoPolicyExts},
		{name: "yaml is NOT a conftest policy", dir: "yaml", exts: ConftestPolicyExts},
		// Absence is the PR-adds-the-first-policy shape, not a failure.
		{name: "a missing directory is not an error", dir: "typo", exts: KyvernoPolicyExts},
		// filepath.WalkDir follows no symlink, root included, so walking the literal
		// path reported a symlinked policy directory as having none — the false note
		// this mechanism exists to prevent.
		{name: "a symlinked directory is followed", dir: "linked", exts: KyvernoPolicyExts, want: true},
		// A file where a directory was named is a setup mistake, and must not read as
		// a side that legitimately has no policies.
		{name: "a regular file is an error, not 'no policies'", dir: "afile", exts: KyvernoPolicyExts, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := HasPolicies(filepath.Join(root, tt.dir), tt.exts)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("HasPolicies() = %v, want %v", got, tt.want)
			}
		})
	}
}

// A side whose policy directory cannot be READ is a setup mistake, not a side that
// legitimately has no policies: it must fail loudly instead of contributing the skip
// note, which would read as "this branch has no policies yet".
func TestBuiltinsFailOnUnreadablePolicyDir(t *testing.T) {
	worktree := t.TempDir()
	// A regular file where a directory was named: readable, but not a policy tree.
	if err := os.WriteFile(filepath.Join(worktree, "policies"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// ...and one where the policy PATH itself is a file, which WalkDir happily
	// visits as an ordinary entry - so this shape used to produce the skip note.
	if err := os.WriteFile(filepath.Join(worktree, "afile"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, dir := range []string{"policies/kyverno", "afile"} {
		r := &Runner{Kyverno: []string{dir}, KubeContext: "ctx"}
		got := r.Lint(context.Background(), Subject{Worktree: worktree}, "")
		if len(got) != 1 || !strings.Contains(got[0], "unusable policy directory") {
			t.Fatalf("%s: got %#v, want one unusable-directory failure", dir, got)
		}
		// The user-spelled path, and no resolved absolute one leaking into a report.
		if !strings.Contains(got[0], `"`+dir+`"`) || strings.Contains(got[0], worktree) {
			t.Errorf("%s: line %q should name the directory as spelled, not the resolved path", dir, got[0])
		}
		if strings.Contains(got[0], "not linted: no policies") {
			t.Errorf("%s: an unusable directory must not report as a side without policies", dir)
		}
	}
}
