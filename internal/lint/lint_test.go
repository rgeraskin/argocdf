package lint

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/log"
)

func newRunner(commands ...string) *Runner {
	return &Runner{Commands: commands, Timeout: 5 * time.Second}
}

func TestLintCollectsStdoutLines(t *testing.T) {
	r := newRunner(`echo one; echo two`)
	got := r.Lint(context.Background(), Subject{}, "")
	if len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Errorf("expected [one two], got %v", got)
	}
}

func TestLintPipesContentToStdin(t *testing.T) {
	r := newRunner(`cat`)
	got := r.Lint(context.Background(), Subject{}, "kind: ConfigMap\nkind: Secret\n")
	if len(got) != 2 || got[0] != "kind: ConfigMap" || got[1] != "kind: Secret" {
		t.Errorf("stdin content not piped through, got %v", got)
	}
}

func TestLintNoOutputMeansNoWarnings(t *testing.T) {
	r := newRunner(`true`)
	if got := r.Lint(context.Background(), Subject{}, "input"); got != nil {
		t.Errorf("expected no warnings, got %v", got)
	}
}

func TestLintSkipsBlankLines(t *testing.T) {
	r := newRunner("printf 'one\\n\\n  \\ntwo\\n'")
	got := r.Lint(context.Background(), Subject{}, "")
	if len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Errorf("blank lines should be dropped, got %v", got)
	}
}

func TestLintKeepsOutputOnFailure(t *testing.T) {
	r := newRunner(`echo finding; exit 3`)
	got := r.Lint(context.Background(), Subject{}, "")
	if len(got) != 2 {
		t.Fatalf("expected finding + error line, got %v", got)
	}
	if got[0] != "finding" {
		t.Errorf("stdout line should be kept on failure, got %q", got[0])
	}
	if !strings.Contains(got[1], "exit status 3") {
		t.Errorf("error line should mention exit status, got %q", got[1])
	}
}

func TestLintFailureIncludesFirstStderrLine(t *testing.T) {
	r := newRunner(`echo err1 >&2; echo err2 >&2; exit 1`)
	got := r.Lint(context.Background(), Subject{}, "")
	if len(got) != 1 {
		t.Fatalf("expected one error line, got %v", got)
	}
	if !strings.HasSuffix(got[0], ": err1") {
		t.Errorf("error line should end with the first stderr line, got %q", got[0])
	}
}

func TestLintCommandNotFound(t *testing.T) {
	r := newRunner(`definitely-not-a-real-binary-xyz`)
	got := r.Lint(context.Background(), Subject{}, "")
	if len(got) != 1 || !strings.HasPrefix(got[0], "[lint#1] ") {
		t.Errorf("expected a single lint error line, got %v", got)
	}
}

func TestLintTimeout(t *testing.T) {
	r := &Runner{Commands: []string{`sleep 30`}, Timeout: 100 * time.Millisecond}
	start := time.Now()
	got := r.Lint(context.Background(), Subject{}, "")
	// The guaranteed bound is Timeout + waitDelay, not Timeout: cancellation
	// SIGKILLs only sh, and whether that also kills the `sleep` holding the
	// stdout pipe depends on the shell — bash execs a single simple command
	// (macOS /bin/sh), dash forks it (Ubuntu /bin/sh) and the full waitDelay is
	// spent. A bound below Timeout+waitDelay passes only on the exec-optimizing
	// shell; the sleep is long enough that 10s still proves the timeout bounds
	// wall-clock. Same reasoning as the *BoundsWallClock tests in builtin_test.go.
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("lint did not honor timeout, took %s (WaitDelay = %s)", elapsed, waitDelay)
	}
	if len(got) != 1 || !strings.Contains(got[0], "timeout after 100ms") {
		t.Errorf("expected timeout warning, got %v", got)
	}
}

func TestLintTimeoutKeepsPartialOutput(t *testing.T) {
	r := &Runner{Commands: []string{`echo early-finding; sleep 5`}, Timeout: 200 * time.Millisecond}
	got := r.Lint(context.Background(), Subject{}, "")
	if len(got) != 2 || got[0] != "early-finding" || !strings.Contains(got[1], "timeout after") {
		t.Errorf("expected partial output + timeout line, got %v", got)
	}
}

func TestLintZeroTimeoutMeansNoLimit(t *testing.T) {
	r := &Runner{Commands: []string{`sleep 0.2; echo done`}, Timeout: 0}
	got := r.Lint(context.Background(), Subject{}, "")
	if len(got) != 1 || got[0] != "done" {
		t.Errorf("zero timeout should not kill the command, got %v", got)
	}
}

func TestLintParentContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	r := newRunner(`sleep 30`)
	start := time.Now()
	got := r.Lint(ctx, Subject{}, "")
	// Bounded by cancellation + waitDelay, for the shell-dependent reason spelled
	// out in TestLintTimeout.
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("lint did not honor parent cancellation, took %s (WaitDelay = %s)", elapsed, waitDelay)
	}
	// Cancellation is not a timeout: it surfaces through the generic error
	// branch as a lint error line rather than being silently dropped.
	if len(got) != 1 || strings.Contains(got[0], "timeout") || !strings.HasPrefix(got[0], "[lint#1] ") {
		t.Errorf("expected a generic lint error line on cancellation, got %v", got)
	}
}

func TestLintParentDeadlineNotMisreportedAsTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	// The runner's own timeout is far away; only the parent deadline fires.
	r := &Runner{Commands: []string{`sleep 5`}, Timeout: 10 * time.Second}
	got := r.Lint(ctx, Subject{}, "")
	if len(got) != 1 || !strings.HasPrefix(got[0], "[lint#1] ") {
		t.Fatalf("expected a single lint error line, got %v", got)
	}
	if strings.Contains(got[0], "timeout after") {
		t.Errorf("parent deadline must not be attributed to --lint-timeout, got %q", got[0])
	}
}

func TestLintRunsInGivenDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "policy-note.txt"), []byte("from-dir\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := newRunner(`cat policy-note.txt`)
	got := r.Lint(context.Background(), Subject{Worktree: dir}, "")
	if len(got) != 1 || got[0] != "from-dir" {
		t.Errorf("command should run with dir as working directory, got %v", got)
	}
}

func TestLintMultipleCommandsRunInOrder(t *testing.T) {
	r := newRunner(`echo first`, `echo second`)
	got := r.Lint(context.Background(), Subject{}, "")
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Errorf("expected commands to run in order, got %v", got)
	}
}

// lintLogLines runs one Lint call against a captured logger and returns the
// non-empty log lines it produced.
func lintLogLines(t *testing.T, r *Runner) []string {
	t.Helper()
	var buf bytes.Buffer
	r.Logger = log.New(&buf)
	r.Lint(context.Background(), Subject{App: "web", Side: "base"}, "")
	return nonEmptyLines(buf.String())
}

// lineWith returns the single log line containing want, failing if there is not
// exactly one.
func lineWith(t *testing.T, lines []string, want string) string {
	t.Helper()
	var found []string
	for _, line := range lines {
		if strings.Contains(line, want) {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("found %d lines containing %q, want exactly 1: %#v", len(found), want, lines)
	}
	return found[0]
}

// One line per INVOCATION, each attributable and each saying how it ended. This
// is the log's whole job: a report cannot distinguish a linter that ran and found
// nothing from one that was skipped or died, because all three leave it identical.
func TestLintLogsOneLinePerInvocation(t *testing.T) {
	r := &Runner{
		Commands: []string{`echo finding`, `exit 3`},
		Kyverno:  []string{"policies/absent"}, // no such dir in the (empty) worktree
		Timeout:  5 * time.Second,
	}
	lines := lintLogLines(t, r)
	if len(lines) != 3 {
		t.Fatalf("got %d log lines, want one per invocation: %#v", len(lines), lines)
	}

	tests := []struct {
		handle     string
		wantStatus string
		wantLevel  string
	}{
		// Ordinals are per KIND and follow flag order, so the two --lint commands
		// are #1 and #2 while the first --lint-kyverno is #1 again.
		{handle: "linter=lint#1", wantStatus: "status=ok", wantLevel: "INFO"},
		{handle: "linter=lint#2", wantStatus: "status=failed", wantLevel: "WARN"},
		{handle: "linter=kyverno#1", wantStatus: "status=skipped", wantLevel: "INFO"},
	}
	for _, tt := range tests {
		t.Run(tt.handle, func(t *testing.T) {
			line := lineWith(t, lines, tt.handle)
			if !strings.Contains(line, "Linted") {
				t.Errorf("line %q does not carry the Linted message", line)
			}
			if !strings.Contains(line, "app=web") || !strings.Contains(line, "side=base") {
				t.Errorf("line %q is not attributable to an app and side", line)
			}
			if !strings.Contains(line, tt.wantStatus) {
				t.Errorf("line %q, want %s", line, tt.wantStatus)
			}
			// A failed invocation means this side was NOT linted; the only other
			// trace is one warning line among the findings, which reads like a
			// finding. It must not arrive at the same level as a clean run.
			if !strings.Contains(line, tt.wantLevel) {
				t.Errorf("line %q, want level %s", line, tt.wantLevel)
			}
		})
	}
}

// A REPORT names a linter one way for everything it produces - the FLAG - and the
// LOG drops the prefix its field name repeats. Two spellings, one per surface, and
// pinned together so the pair cannot drift into one per line kind.
func TestLinterIdentityPerSurface(t *testing.T) {
	tests := []struct {
		kind       string
		wantLog    string
		wantReport string
	}{
		{kind: kindCommand, wantLog: "lint#1", wantReport: "[lint#1]"},
		{kind: kindKyverno, wantLog: "kyverno#1", wantReport: "[lint-kyverno#1]"},
		{kind: kindConftest, wantLog: "conftest#1", wantReport: "[lint-conftest#1]"},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			id := identify(tt.kind, 1)
			if id.handle != tt.wantLog {
				t.Errorf("log handle = %q, want %q", id.handle, tt.wantLog)
			}
			if got := id.bracket(); got != tt.wantReport {
				t.Errorf("report bracket = %q, want %q", got, tt.wantReport)
			}
		})
	}
}

// A repeated --lint has no identity but its ordinal: the command text is
// truncated to maxDisplayCommand, so two commands differing only past that cut
// would otherwise be indistinguishable in both the log and the report.
func TestRepeatedCommandsGetDistinctOrdinals(t *testing.T) {
	shared := strings.Repeat("x", maxDisplayCommand)
	r := &Runner{
		Commands: []string{
			"echo " + shared + " first >&2; exit 1",
			"echo " + shared + " second >&2; exit 1",
		},
		Timeout: 5 * time.Second,
	}
	got := r.Lint(context.Background(), Subject{}, "")
	if len(got) != 2 {
		t.Fatalf("got %#v, want one failure line per command", got)
	}
	if !strings.HasPrefix(got[0], "[lint#1] ") || !strings.HasPrefix(got[1], "[lint#2] ") {
		t.Errorf("report labels = %#v, want lint#1 then lint#2", got)
	}
}

// Durations are rounded for reading, but not so far that the headroom below
// --lint-timeout stops being visible: whole seconds would print a 9.6s success as
// 10s, indistinguishable from a 10s timeout.
func TestRoundDuration(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want time.Duration
	}{
		{in: 0, want: 0},
		{in: 47*time.Millisecond + 400*time.Microsecond, want: 47 * time.Millisecond},
		{in: 1898 * time.Millisecond, want: 1900 * time.Millisecond},
		{in: 10009 * time.Millisecond, want: 10 * time.Second},
		{in: 9600 * time.Millisecond, want: 9600 * time.Millisecond},
	}
	for _, tt := range tests {
		if got := RoundDuration(tt.in); got != tt.want {
			t.Errorf("RoundDuration(%s) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

func TestChildEnv(t *testing.T) {
	tests := []struct {
		name   string
		parent []string
		extra  map[string]string
		want   []string
	}{
		{
			name:   "nil extra leaves the environment untouched",
			parent: []string{"PATH=/bin", "HOME=/home/u"},
			extra:  nil,
			want:   nil,
		},
		{
			name:   "empty extra leaves the environment untouched",
			parent: []string{"PATH=/bin"},
			extra:  map[string]string{},
			want:   nil,
		},
		{
			name:   "appends when the parent has no such key",
			parent: []string{"PATH=/bin", "HOME=/home/u"},
			extra:  map[string]string{"ARGOCDF_CONTEXT": "prod"},
			want:   []string{"PATH=/bin", "HOME=/home/u", "ARGOCDF_CONTEXT=prod"},
		},
		{
			name:   "replaces an inherited value and keeps unrelated vars",
			parent: []string{"PATH=/bin", "ARGOCDF_CONTEXT=stale", "HOME=/home/u"},
			extra:  map[string]string{"ARGOCDF_CONTEXT": "prod"},
			want:   []string{"PATH=/bin", "HOME=/home/u", "ARGOCDF_CONTEXT=prod"},
		},
		{
			name:   "removes every inherited occurrence of the key",
			parent: []string{"ARGOCDF_CONTEXT=stale", "PATH=/bin", "ARGOCDF_CONTEXT=older"},
			extra:  map[string]string{"ARGOCDF_CONTEXT": "prod"},
			want:   []string{"PATH=/bin", "ARGOCDF_CONTEXT=prod"},
		},
		{
			name:   "multiple keys are appended in sorted order",
			parent: []string{"ARGOCDF_KUBECONFIG=/stale/config", "PATH=/bin"},
			extra: map[string]string{
				"ARGOCDF_KUBECONFIG": "/a/config:/b/config",
				"ARGOCDF_CONTEXT":    "prod",
			},
			want: []string{"PATH=/bin", "ARGOCDF_CONTEXT=prod", "ARGOCDF_KUBECONFIG=/a/config:/b/config"},
		},
		{
			name:   "an empty inherited value is replaced too",
			parent: []string{"ARGOCDF_CONTEXT="},
			extra:  map[string]string{"ARGOCDF_CONTEXT": "prod"},
			want:   []string{"ARGOCDF_CONTEXT=prod"},
		},
		{
			name:   "malformed parent entries are preserved",
			parent: []string{"NO_EQUALS_SIGN", "PATH=/bin"},
			extra:  map[string]string{"ARGOCDF_CONTEXT": "prod"},
			want:   []string{"NO_EQUALS_SIGN", "PATH=/bin", "ARGOCDF_CONTEXT=prod"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := childEnv(tt.parent, tt.extra)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("childEnv() = %v, want %v", got, tt.want)
			}
			// Exactly one occurrence, so the slice states unambiguously what the
			// child will see. Note this is not what makes argocdf's value win —
			// exec.Cmd dedups Env keeping the LAST value — it is what makes the
			// environment auditable and these assertions meaningful.
			for name, value := range tt.extra {
				var count int
				for _, entry := range got {
					if entry == name+"="+value {
						count++
					} else if strings.HasPrefix(entry, name+"=") {
						t.Errorf("childEnv() kept a foreign entry for %s: %q", name, entry)
					}
				}
				if count != 1 {
					t.Errorf("childEnv() has %d entries for %s=%s, want exactly 1", count, name, value)
				}
			}
		})
	}
}

func TestLintExportsEnvToCommand(t *testing.T) {
	// The parent already carries a conflicting value and an unrelated one: the
	// first must lose to argocdf's, the second must survive.
	t.Setenv("ARGOCDF_CONTEXT", "shell-context")
	t.Setenv("LINT_UNRELATED", "kept")

	r := &Runner{
		Commands: []string{
			`echo "context=$ARGOCDF_CONTEXT"; echo "kubeconfig=$ARGOCDF_KUBECONFIG";` +
				` echo "unrelated=$LINT_UNRELATED"; echo "count=$(env | grep -c '^ARGOCDF_CONTEXT=')"`,
		},
		Timeout: 5 * time.Second,
		Env: map[string]string{
			"ARGOCDF_CONTEXT":    "argocdf-context",
			"ARGOCDF_KUBECONFIG": "/a/config:/b/config",
		},
	}

	got := r.Lint(context.Background(), Subject{}, "")
	want := []string{
		"context=argocdf-context",
		"kubeconfig=/a/config:/b/config",
		"unrelated=kept",
		"count=1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("lint command environment = %v, want %v", got, want)
	}
}

func TestLintWithoutEnvInheritsParent(t *testing.T) {
	// No Env configured: cmd.Env stays nil and the child inherits everything,
	// exactly as before the variables were introduced.
	t.Setenv("ARGOCDF_CONTEXT", "shell-context")

	r := newRunner(`echo "context=$ARGOCDF_CONTEXT"`)
	got := r.Lint(context.Background(), Subject{}, "")
	if len(got) != 1 || got[0] != "context=shell-context" {
		t.Errorf("expected the inherited value to reach the command, got %v", got)
	}
}

func TestDisplayCommandTruncatesAndFlattens(t *testing.T) {
	long := "kyverno apply policy.yaml --resource -\n  | jq -rn 'input | .results[]?'" + strings.Repeat(" x", 40)
	got := displayCommand(long)
	if strings.Contains(got, "\n") {
		t.Errorf("displayCommand should flatten newlines, got %q", got)
	}
	if len(got) > maxDisplayCommand+len("...") {
		t.Errorf("displayCommand should truncate, got %d chars: %q", len(got), got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncated command should end with ellipsis, got %q", got)
	}
}

// ARGOCDF_LINT_ID lets a shell adapter prefix its findings exactly as argocdf
// prefixes its own lines about the same linter. It is per-INVOCATION, which is the
// part worth pinning: a value written into the shared Env map would be raced by
// concurrent application renders and would report one linter's identity under
// another's.
func TestLintExportsPerInvocationIdentity(t *testing.T) {
	r := &Runner{
		Commands: []string{`echo "id=$ARGOCDF_LINT_ID"`, `echo "id=$ARGOCDF_LINT_ID"`},
		Timeout:  5 * time.Second,
		Env:      map[string]string{"ARGOCDF_CONTEXT": "ctx"},
	}
	got := r.Lint(context.Background(), Subject{}, "")
	want := []string{"id=lint#1", "id=lint#2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("exported identities = %#v, want %#v", got, want)
	}
	// The shared map must be untouched, or the next invocation inherits this one's.
	if _, leaked := r.Env["ARGOCDF_LINT_ID"]; leaked {
		t.Error("per-invocation value leaked into the shared Env map")
	}
}

// The built-in adapters take their identity as data, so a stale ARGOCDF_LINT_ID in
// the environment cannot influence them — and the value a command sees is always
// argocdf's, never an inherited one.
func TestLintIdentityOverridesInheritedValue(t *testing.T) {
	t.Setenv("ARGOCDF_LINT_ID", "from-the-shell")
	r := newRunner(`echo "id=$ARGOCDF_LINT_ID"; echo "count=$(env | grep -c '^ARGOCDF_LINT_ID=')"`)
	got := r.Lint(context.Background(), Subject{}, "")
	want := []string{"id=lint#1", "count=1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// Concurrent Lint calls must not see each other's identity.
//
// This is the one claim in the per-invocation identity design that no other test
// exercises: ARGOCDF_LINT_ID is layered over Env per invocation precisely BECAUSE Env
// is one map shared by every command of the run, and processOneApp lints from
// --concurrency goroutines at once. A value written into the shared map would race,
// and the symptom would be a report attributing one linter's findings to another -
// intermittently, which is exactly what reading the code does not catch. Run under
// `mise run test-race` for the detector; the assertion below also fails a
// cross-contamination that happens to win the race deterministically.
func TestLintConcurrentInvocationsKeepTheirOwnIdentity(t *testing.T) {
	const apps = 8
	r := &Runner{
		// Two commands, so each goroutine exercises both ordinals, and the app name
		// travels a different channel (Subject) from the identity (env).
		Commands: []string{`echo "$ARGOCDF_LINT_ID"`, `echo "$ARGOCDF_LINT_ID"`},
		Timeout:  10 * time.Second,
		Env:      map[string]string{"ARGOCDF_CONTEXT": "ctx"},
	}

	type result struct {
		app   string
		lines []string
	}
	results := make(chan result, apps)
	var wg sync.WaitGroup
	for i := 0; i < apps; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			app := fmt.Sprintf("app-%d", i)
			results <- result{app: app, lines: r.Lint(context.Background(),
				Subject{App: app, Namespace: "ns", Side: "base"}, "")}
		}(i)
	}
	wg.Wait()
	close(results)

	seen := 0
	for got := range results {
		seen++
		want := []string{"lint#1", "lint#2"}
		if !reflect.DeepEqual(got.lines, want) {
			t.Errorf("%s saw %#v, want %#v", got.app, got.lines, want)
		}
	}
	if seen != apps {
		t.Errorf("collected %d results, want %d", seen, apps)
	}
	// The shared map is still exactly what the caller configured.
	if !reflect.DeepEqual(r.Env, map[string]string{"ARGOCDF_CONTEXT": "ctx"}) {
		t.Errorf("shared Env mutated to %#v", r.Env)
	}
}
