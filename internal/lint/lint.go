// Package lint pipes rendered manifests through policy tools and turns their
// findings into report warnings.
//
// Two shapes share one contract. A user-supplied SHELL command (--lint) receives
// an application's rendered multi-doc YAML on stdin and emits one finding per
// stdout line; the process outcome is the only health signal, so a command is
// expected to end in an adapter (typically jq) that exits 0 when the pipeline
// worked, because kyverno and conftest both exit non-zero on findings during
// normal operation. The BUILT-IN adapters (--lint-kyverno, --lint-conftest) run
// the same tools against a policy directory and parse their JSON here instead,
// which removes the jq dependency, the shell quoting, and the two ways an adapter
// is usually written wrong: mistaking empty output for failure, and mistaking a
// findings exit for a crash.
package lint

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Runner executes lint commands against rendered manifest content.
type Runner struct {
	// Commands are shell commands run in order via `sh -c`.
	Commands []string

	// Kyverno and Conftest are policy directories for the BUILT-IN adapters,
	// which exec the tool themselves and parse its JSON instead of relying on a
	// user-written shell pipeline. Paths are relative to the side's worktree, so
	// a PR that changes a policy lints each side with its own version — the same
	// property the shell commands get from their working directory.
	Kyverno  []string
	Conftest []string

	// KubeContext and Kubeconfig target the cluster argocdf is diffing, for the
	// built-in adapters that consult it. Shell commands get the same values
	// through Env instead: the environment variables are the SHELL contract, so
	// the built-ins take them as data rather than round-tripping through it.
	KubeContext string
	Kubeconfig  string

	// Timeout bounds each command invocation.
	Timeout time.Duration

	// Env holds variables exported to every command on top of argocdf's own
	// environment, replacing any inherited entry with the same name (see
	// childEnv). Values are passed verbatim; a key that must not be visible to
	// commands is simply left out — argocdf never exports an empty value,
	// so an adapter can test for "unset". Empty/nil means the commands inherit
	// argocdf's environment untouched.
	Env map[string]string
}

// errLintTimeout marks a context cancelled by the Runner's own timeout, so a
// parent deadline expiring first is not misreported as --lint-timeout.
var errLintTimeout = errors.New("lint timeout")

// waitDelay bounds how long cmd.Wait keeps waiting after the timeout has already
// killed the command. Without it --lint-timeout does not bound wall-clock at all:
// a command that FORKS (`tool | filter &`, or any `a; b` compound where the shell
// does not exec) leaves a descendant holding the inherited stdout pipe, and Wait
// blocks on that pipe until the descendant exits on its own — a `sleep 60` behind
// a 10s timeout still costs 60s. WaitDelay closes the pipe and SIGKILLs the
// process group shortly after cancellation, so the timeout means what it says.
// Deliberately small: by the time it applies, the output is already truncated and
// the run has been reported as timed out.
const waitDelay = 2 * time.Second

// Lint runs every command with content on stdin and dir as the working
// directory (empty = inherit), and returns the collected warning lines. Lint
// is never fatal: stdout lines are kept even when the command fails, and a
// failure (spawn error, timeout, exit != 0) appends one self-identifying
// warning line instead of returning an error.
//
// dir is the side's ephemeral worktree, so repo-relative paths in the command
// (e.g. a policy directory) resolve to that side's version of the files.
// Commands see argocdf's own environment with Env layered on top.
//
// Linters run in a FIXED order — shell commands first, then kyverno, then
// conftest, each in the order given — because that order is visible in the
// report and therefore part of what expectations pin. pflag cannot preserve the
// interleaving of different flags on the command line, so an order has to be
// chosen rather than inferred.
func (r *Runner) Lint(ctx context.Context, dir, content string) []string {
	var warnings []string
	for _, command := range r.Commands {
		warnings = append(warnings, r.runOne(ctx, command, dir, content)...)
	}
	for _, policyDir := range r.Kyverno {
		warnings = append(warnings, r.runKyverno(ctx, dir, policyDir, content)...)
	}
	for _, policyDir := range r.Conftest {
		warnings = append(warnings, r.runConftest(ctx, dir, policyDir, content)...)
	}
	return warnings
}

// Configured reports whether the runner has any linter to run.
func (r *Runner) Configured() bool {
	return len(r.Commands)+len(r.Kyverno)+len(r.Conftest) > 0
}

// toolOutput carries a built-in adapter's raw stdout, or the single warning line
// that replaces it when the invocation itself failed.
type toolOutput struct {
	stdout  string
	warning string
}

// execTool runs a built-in adapter's argv with content on stdin, applying the
// same health contract as the shell commands: the process OUTCOME is the only
// signal, and empty output is resolved by the EXIT CODE rather than by failing
// to parse it. Exit 0 with nothing on stdout means "no findings" — both tools do
// that routinely when no rendered resource matches a policy — while a non-zero
// exit with no report is a real failure and must surface. A non-zero exit WITH a
// report is normal: both tools exit non-zero precisely because they found
// something.
func (r *Runner) execTool(ctx context.Context, dir, label string, argv []string, content string) toolOutput {
	cancel := func() {}
	if r.Timeout > 0 {
		ctx, cancel = context.WithTimeoutCause(ctx, r.Timeout, errLintTimeout)
	}
	defer cancel()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = waitDelay
	cmd.Dir = dir
	env := childEnv(os.Environ(), r.Env)
	if r.Kubeconfig != "" {
		// Through KUBECONFIG rather than a flag: the value may be an
		// os.PathListSeparator-joined LIST, which single-file flags reject.
		//
		// Appending is enough to OVERRIDE an inherited KUBECONFIG, and does not
		// need the scrub childEnv does: exec.Cmd deduplicates Env and keeps the
		// LAST value for each key ("If Env contains duplicate environment keys,
		// only the last value in the slice for each duplicate key is used"), so
		// the child receives exactly one KUBECONFIG — this one. Verified on
		// go1.25. Two independent reviews read this as a wrong-cluster bug, so
		// the reasoning is written down rather than left to be re-derived.
		if env == nil {
			env = os.Environ()
		}
		env = append(env, "KUBECONFIG="+r.Kubeconfig)
	}
	cmd.Env = env
	cmd.Stdin = strings.NewReader(content)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	out := toolOutput{stdout: strings.TrimSpace(stdout.String())}
	if err == nil {
		return out
	}

	// A TIMEOUT surfaces even when stdout has content, which is the one case
	// where output is not enough to call the run healthy: a truncated report
	// either fails to parse or — worse — parses into silently FEWER findings,
	// which reads as "cleaner than reality". The shell path appends the same line
	// while keeping its stdout; this keeps the two contracts aligned.
	if errors.Is(context.Cause(ctx), errLintTimeout) {
		out.warning = fmt.Sprintf("%s: timeout after %s", label, r.Timeout)
		return out
	}

	// A non-zero exit WITH a report is normal, not a failure: both tools exit
	// non-zero precisely because they found something.
	if out.stdout != "" {
		return out
	}

	out.warning = fmt.Sprintf("%s: %v with no report output", label, err)
	if first := firstLine(stderr.String()); first != "" {
		out.warning += ": " + first
	}
	return out
}

// resolvePolicyDir returns the absolute policy directory and whether it holds
// anything. A missing or EMPTY directory reports false rather than an error: on
// the base side of a PR that adds the first policy there is legitimately nothing
// to apply, and both tools treat an empty policy set as a hard error, which would
// otherwise attach a spurious lint failure to every application.
//
// A RELATIVE path resolves against the side's worktree, which is what makes each
// side lint with its own version of the policies. An ABSOLUTE path is used as
// given and therefore points both sides at the same tree — deliberate (a policy
// set shared outside the repo is a real setup) but it forfeits per-side
// resolution, so a PR that changes such a policy shows no difference between
// sides.
func resolvePolicyDir(worktree, policyDir string) (string, bool) {
	dir := policyDir
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(worktree, policyDir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		return "", false
	}
	return dir, true
}

func (r *Runner) runOne(ctx context.Context, command, dir, content string) []string {
	cancel := func() {}
	if r.Timeout > 0 {
		ctx, cancel = context.WithTimeoutCause(ctx, r.Timeout, errLintTimeout)
	}
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.WaitDelay = waitDelay
	cmd.Dir = dir
	// nil (no Env configured) keeps the inherit-everything default.
	cmd.Env = childEnv(os.Environ(), r.Env)
	cmd.Stdin = strings.NewReader(content)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	warnings := nonEmptyLines(stdout.String())
	if err == nil {
		return warnings
	}

	if errors.Is(context.Cause(ctx), errLintTimeout) {
		return append(warnings,
			fmt.Sprintf("lint %q: timeout after %s", displayCommand(command), r.Timeout))
	}
	msg := fmt.Sprintf("lint %q: %v", displayCommand(command), err)
	if first := firstLine(stderr.String()); first != "" {
		msg += ": " + first
	}
	return append(warnings, msg)
}

// childEnv builds the environment of a lint child process: parent with every
// entry naming one of extra's keys REMOVED, then extra appended (sorted, so
// the result is deterministic).
//
// The removal keeps the child's environment single-valued; it is NOT what makes
// argocdf's value win. exec.Cmd deduplicates Env and keeps the LAST value for
// each key, so appending alone would already override a stale inherited
// ARGOCDF_CONTEXT (verified on go1.25 — an earlier version of this comment
// claimed the opposite, and two code reviews filed a bug against the built-in
// adapters' KUBECONFIG append on the strength of it). The scrub earns its place
// by making the slice say exactly what the child will see, which is what the
// tests below assert against.
//
// This is NOT the reason the argocd renderer scrubs HELM_*: there the inherited
// values win because helm prefers its own HELM_* variables over the XDG_* ones
// ArgoCD sets, and because ArgoCD never sets most of them (see
// render.inheritedHelmEnvVars).
//
// An empty extra returns nil so callers can leave cmd.Env unset and keep the
// inherit-everything behavior bit-for-bit.
func childEnv(parent []string, extra map[string]string) []string {
	if len(extra) == 0 {
		return nil
	}

	env := make([]string, 0, len(parent)+len(extra))
	for _, entry := range parent {
		if name, _, ok := strings.Cut(entry, "="); ok {
			if _, replaced := extra[name]; replaced {
				continue
			}
		}
		env = append(env, entry)
	}

	names := make([]string, 0, len(extra))
	for name := range extra {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		env = append(env, name+"="+extra[name])
	}
	return env
}

// nonEmptyLines splits s into trimmed, non-empty lines.
func nonEmptyLines(s string) []string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// firstLine returns the first non-empty line of s, trimmed.
func firstLine(s string) string {
	lines := nonEmptyLines(s)
	if len(lines) == 0 {
		return ""
	}
	return lines[0]
}

// maxDisplayCommand bounds how much of a lint command is echoed back in
// warning lines; full pipelines with embedded jq programs are too long.
const maxDisplayCommand = 48

// displayCommand shortens a command for use in warning messages.
func displayCommand(command string) string {
	command = strings.Join(strings.Fields(command), " ")
	if len(command) <= maxDisplayCommand {
		return command
	}
	return command[:maxDisplayCommand] + "..."
}
