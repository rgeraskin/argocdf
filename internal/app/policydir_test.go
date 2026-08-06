package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/log"

	"github.com/rgeraskin/argocdf/internal/config"
)

// warnMissingPolicyDirs is the only thing standing between a mistyped policy
// path and a report that says every application is clean: the adapters
// deliberately skip a directory they cannot use, because the base side of a PR
// adding the first policy legitimately has none. So the startup check must cover
// exactly what the adapters skip — missing AND empty — or the tolerance leaves a
// silent hole.
func TestWarnMissingPolicyDirs(t *testing.T) {
	repo := t.TempDir()

	withPolicies := filepath.Join(repo, "policies", "kyverno")
	if err := os.MkdirAll(withPolicies, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(withPolicies, "policy.yaml"), []byte("kind: X\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "policies", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "policies", "afile"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A layout that used to look populated: a directory holding only files no tool
	// loads. kyverno then exits 0 with no results, which reported status=ok - a
	// linter that silently checked nothing.
	keepOnly := filepath.Join(repo, "policies", "keep-only")
	if err := os.MkdirAll(filepath.Join(keepOnly, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".gitkeep", "README.md"} {
		if err := os.WriteFile(filepath.Join(keepOnly, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// ...and one where the policy is nested, which both tools would find.
	nested := filepath.Join(repo, "policies", "nested")
	if err := os.MkdirAll(filepath.Join(nested, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "sub", "policy.yaml"), []byte("kind: X\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A symlinked policy directory is a normal monorepo layout and both tools read
	// through it, so the startup warning must stay SILENT for one - it fired before
	// HasPolicies resolved the root.
	if err := os.Symlink(withPolicies, filepath.Join(repo, "policies", "linked")); err != nil {
		t.Fatal(err)
	}

	regoOnly := filepath.Join(repo, "policies", "rego-only")
	if err := os.MkdirAll(regoOnly, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(regoOnly, "deny.rego"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		kyverno  []string
		conftest []string
		wantWarn string // substring, empty = no warning at all
	}{
		{
			name:    "directory with policies is silent",
			kyverno: []string{"policies/kyverno"},
		},
		{
			name:     "missing directory warns",
			kyverno:  []string{"policies/typo"},
			wantWarn: "does not exist in the working tree",
		},
		{
			// The gap both reviewers found: `mkdir -p policies/kyverno` with
			// nothing in it produced no warning and no findings, which is
			// indistinguishable from "all policies passed".
			name:     "EMPTY directory warns, because the adapters skip it too",
			kyverno:  []string{"policies/empty"},
			wantWarn: "holds no policy file in the working tree",
		},
		{
			name:     "a file where a directory was meant warns, and says so",
			conftest: []string{"policies/afile"},
			wantWarn: "is not a directory in the working tree",
		},
		{
			name:    "a symlinked directory with policies is silent",
			kyverno: []string{"policies/linked"},
		},
		{
			name:     "a directory holding only a .gitkeep and a README warns",
			kyverno:  []string{"policies/keep-only"},
			wantWarn: "holds no policy file in the working tree",
		},
		{
			// The tools recurse, so the check must too - warning here would send a
			// user chasing a directory that is perfectly fine.
			name:    "a policy in a subdirectory is found, not warned about",
			kyverno: []string{"policies/nested"},
		},
		{
			// Extensions are per TOOL: rego is a conftest policy and yaml is not,
			// so the same directory answers differently for the two flags.
			name:     "a rego-only directory has no kyverno policies",
			kyverno:  []string{"policies/rego-only"},
			wantWarn: "holds no policy file in the working tree",
		},
		{
			name:     "the flag name is named, so the user knows which one to fix",
			conftest: []string{"policies/typo"},
			wantWarn: "--lint-conftest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := log.New(&buf)
			logger.SetLevel(log.DebugLevel)
			a := &App{
				cfg: &config.Config{
					RepoPath:     repo,
					LintKyverno:  tt.kyverno,
					LintConftest: tt.conftest,
				},
				logger: logger,
			}

			a.warnMissingPolicyDirs()

			got := buf.String()
			switch {
			case tt.wantWarn == "" && got != "":
				t.Errorf("logged %q, want nothing", got)
			case tt.wantWarn != "" && !strings.Contains(got, tt.wantWarn):
				t.Errorf("logged %q, want it to contain %q", got, tt.wantWarn)
			}
		})
	}
}

// An absolute policy directory is resolved as given rather than against the
// repository, so it must not be reported missing just because the repo has no
// such relative path.
func TestWarnMissingPolicyDirsAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "policy.yaml"), []byte("kind: X\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	a := &App{
		cfg:    &config.Config{RepoPath: t.TempDir(), LintKyverno: []string{dir}},
		logger: log.New(&buf),
	}
	a.warnMissingPolicyDirs()

	if buf.String() != "" {
		t.Errorf("logged %q, want nothing for an absolute directory that exists", buf.String())
	}
}
