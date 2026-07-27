package lint

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func newRunner(commands ...string) *Runner {
	return &Runner{Commands: commands, Timeout: 5 * time.Second}
}

func TestLintCollectsStdoutLines(t *testing.T) {
	r := newRunner(`echo one; echo two`)
	got := r.Lint(context.Background(), "", "")
	if len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Errorf("expected [one two], got %v", got)
	}
}

func TestLintPipesContentToStdin(t *testing.T) {
	r := newRunner(`cat`)
	got := r.Lint(context.Background(), "", "kind: ConfigMap\nkind: Secret\n")
	if len(got) != 2 || got[0] != "kind: ConfigMap" || got[1] != "kind: Secret" {
		t.Errorf("stdin content not piped through, got %v", got)
	}
}

func TestLintNoOutputMeansNoWarnings(t *testing.T) {
	r := newRunner(`true`)
	if got := r.Lint(context.Background(), "", "input"); got != nil {
		t.Errorf("expected no warnings, got %v", got)
	}
}

func TestLintSkipsBlankLines(t *testing.T) {
	r := newRunner("printf 'one\\n\\n  \\ntwo\\n'")
	got := r.Lint(context.Background(), "", "")
	if len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Errorf("blank lines should be dropped, got %v", got)
	}
}

func TestLintKeepsOutputOnFailure(t *testing.T) {
	r := newRunner(`echo finding; exit 3`)
	got := r.Lint(context.Background(), "", "")
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
	got := r.Lint(context.Background(), "", "")
	if len(got) != 1 {
		t.Fatalf("expected one error line, got %v", got)
	}
	if !strings.HasSuffix(got[0], ": err1") {
		t.Errorf("error line should end with the first stderr line, got %q", got[0])
	}
}

func TestLintCommandNotFound(t *testing.T) {
	r := newRunner(`definitely-not-a-real-binary-xyz`)
	got := r.Lint(context.Background(), "", "")
	if len(got) != 1 || !strings.HasPrefix(got[0], "lint ") {
		t.Errorf("expected a single lint error line, got %v", got)
	}
}

func TestLintTimeout(t *testing.T) {
	r := &Runner{Commands: []string{`sleep 5`}, Timeout: 100 * time.Millisecond}
	start := time.Now()
	got := r.Lint(context.Background(), "", "")
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("lint did not honor timeout, took %s", elapsed)
	}
	if len(got) != 1 || !strings.Contains(got[0], "timeout after 100ms") {
		t.Errorf("expected timeout warning, got %v", got)
	}
}

func TestLintTimeoutKeepsPartialOutput(t *testing.T) {
	r := &Runner{Commands: []string{`echo early-finding; sleep 5`}, Timeout: 200 * time.Millisecond}
	got := r.Lint(context.Background(), "", "")
	if len(got) != 2 || got[0] != "early-finding" || !strings.Contains(got[1], "timeout after") {
		t.Errorf("expected partial output + timeout line, got %v", got)
	}
}

func TestLintZeroTimeoutMeansNoLimit(t *testing.T) {
	r := &Runner{Commands: []string{`sleep 0.2; echo done`}, Timeout: 0}
	got := r.Lint(context.Background(), "", "")
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
	r := newRunner(`sleep 5`)
	start := time.Now()
	got := r.Lint(ctx, "", "")
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("lint did not honor parent cancellation, took %s", elapsed)
	}
	// Cancellation is not a timeout: it surfaces through the generic error
	// branch as a lint error line rather than being silently dropped.
	if len(got) != 1 || strings.Contains(got[0], "timeout") || !strings.HasPrefix(got[0], "lint ") {
		t.Errorf("expected a generic lint error line on cancellation, got %v", got)
	}
}

func TestLintParentDeadlineNotMisreportedAsTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	// The runner's own timeout is far away; only the parent deadline fires.
	r := &Runner{Commands: []string{`sleep 5`}, Timeout: 10 * time.Second}
	got := r.Lint(ctx, "", "")
	if len(got) != 1 || !strings.HasPrefix(got[0], "lint ") {
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
	got := r.Lint(context.Background(), dir, "")
	if len(got) != 1 || got[0] != "from-dir" {
		t.Errorf("command should run with dir as working directory, got %v", got)
	}
}

func TestLintMultipleCommandsRunInOrder(t *testing.T) {
	r := newRunner(`echo first`, `echo second`)
	got := r.Lint(context.Background(), "", "")
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Errorf("expected commands to run in order, got %v", got)
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

	got := r.Lint(context.Background(), "", "")
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
	got := r.Lint(context.Background(), "", "")
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
