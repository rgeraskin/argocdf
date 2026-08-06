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
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/log"
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

	// Logger, when set, records one line per linter invocation - which linter
	// ran, against what, how it ENDED, how long it took and how many lines it
	// produced. It is the only signal that separates the three outcomes a
	// report cannot tell apart, because all three leave it identical: a linter
	// that ran and found nothing, one that was skipped for want of policies,
	// and one that died. Empty tool output at exit 0 is a legitimate
	// no-findings result by contract, so neither the findings nor their count
	// can carry that distinction - only status can. The e2e suite asserts on
	// these lines (must-log: in checks.grep). Nil disables the logging.
	Logger *log.Logger

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
// a 10s timeout still costs 60s. WaitDelay closes the pipe and kills the command
// shortly after cancellation, so the timeout bounds ARGOCDF's wall-clock.
//
// What it does NOT do is kill a process GROUP: Go kills the process it started, so a
// descendant that outlived `sh` keeps running with its pipe closed. A command that
// backgrounds work can therefore still be alive - and still have side effects - after
// argocdf has reported the timeout and moved on. Bounding argocdf is what this
// guarantees; reaping the tree would need the command started in its own process
// group and the group signalled, which is a Unix-only mechanism argocdf does not
// currently set up.
//
// Deliberately small: by the time it applies, the output is already truncated and
// the run has been reported as timed out.
const waitDelay = 2 * time.Second

// Subject identifies what one Lint call is linting: an application, one SIDE of
// it, and the ephemeral worktree that side is checked out in. App and Side are
// carried for the log only — they are what makes an invocation line attributable
// when --concurrency renders several applications at once.
//
// Deliberately not named Target: "target" already means the target SIDE
// throughout argocdf (diff.SideTarget, side=target in these very log lines), and
// one term must not mean two things.
type Subject struct {
	App string
	// Namespace disambiguates App: with apps-in-any-namespace, team-a/web and
	// team-b/web are different applications with the same name, and a line naming
	// only the name is not attributable to either.
	Namespace string
	Side      string
	Worktree  string
}

// The three linter kinds, named by the FLAG that configures each. Every linter
// has two spellings of its identity and the mapping between them is mechanical:
// the REPORT label is the flag name (a reader of a PR comment has no log beside
// it, so the label must match what someone would have to type), while the LOG
// value drops the "lint-" prefix, which the linter= field name already supplies.
// The bare --lint flag has nothing to strip, so its two spellings coincide.
const (
	kindCommand  = "lint"
	kindKyverno  = "lint-kyverno"
	kindConftest = "lint-conftest"
)

// logHandle names the Nth linter of a kind in the LOG (1-based, flag order),
// dropping the "lint-" prefix that the linter= field name already supplies.
//
// The ORDINAL exists because a repeated --lint has no usable identity of its
// own: its only distinguishing datum is the command text, which is truncated to
// maxDisplayCommand and can therefore collide between two commands that differ
// only past that cut. Built-in adapters are identified well enough by their
// policy directory, but they carry the ordinal too — one rule for the whole
// family beats an exception nobody remembers.
func logHandle(kind string, n int) string {
	return ordinal(strings.TrimPrefix(kind, "lint-"), n)
}

// ordinal is the one place an identity's "#<n>" is formatted.
func ordinal(name string, n int) string {
	return fmt.Sprintf("%s#%d", name, n)
}

// linterID is one linter's identity in each of the two surfaces it appears in.
//
// A REPORT uses one spelling for everything about a linter, name — the FLAG that
// configured it. A PR comment has no log beside it, so the identity has to be the
// one a reader would have to type, and that has to hold for a finding as much as
// for a line about the linter: `grep lint-kyverno#1` then finds everything that
// linter produced. An earlier split (tool-spelled findings, flag-spelled health
// lines) made no single grep able to do that, and the distinction it encoded is
// already carried by the shape of the line.
//
// The LOG uses handle, the same identity minus the prefix its field name repeats.
// Two spellings total, one per surface — not one per line kind.
type linterID struct {
	name   string
	handle string
}

// bracket is how the identity appears in a report: bracketed, and FIRST after the
// side label. A warning list mixes three kinds of line, and until they were
// bracketed only the wording told them apart:
//
//	[base] [lint-kyverno#1/require-pinned-images] Deployment/web: images must be pinned
//	[base] [lint-kyverno#1] not linted: no policies in "policies/kyverno"
//	[base] resource ConfigMap/cm: duplicate key "a" (using last value)
//
// A bracket CONTINUING into a policy is a finding (a rule matched a resource, and
// the text after it is the tool's). A bracket that ends at the linter is argocdf
// speaking about the linter itself — timeout, skip, crash — where there is no
// resource to name. No bracket at all is not lint: it comes from the diff layer,
// which is why unbracketed health lines were ambiguous with parse warnings.
//
// The side label ([base]/[target], added later by diff.LabelSide) stays first:
// which side a line describes outranks which subsystem produced it.
func (id linterID) bracket() string { return "[" + id.name + "]" }

func identify(kind string, n int) linterID {
	// Both spellings are "<something>#<n>"; only the something differs, so the
	// ordinal is formatted in ONE place and the pair cannot drift apart.
	return linterID{name: ordinal(kind, n), handle: logHandle(kind, n)}
}

// Lint runs every configured linter over content, with the subject's worktree as
// the working directory (empty = inherit), and returns the collected warning
// lines. Lint is never fatal: stdout lines are kept even when a command fails,
// and a failure (spawn error, timeout, exit != 0) appends one self-identifying
// warning line instead of returning an error.
//
// The worktree is the side's ephemeral checkout, so repo-relative paths in the
// command (e.g. a policy directory) resolve to that side's version of the files.
// Commands see argocdf's own environment with Env layered on top.
//
// Linters run in a FIXED order — shell commands first, then kyverno, then
// conftest, each in the order given — because that order is visible in the
// report and therefore part of what expectations pin. pflag cannot preserve the
// interleaving of different flags on the command line, so an order has to be
// chosen rather than inferred. The ordinals follow that same order.
func (r *Runner) Lint(ctx context.Context, subject Subject, content string) []string {
	var warnings []string
	for i, command := range r.Commands {
		id := identify(kindCommand, i+1)
		warnings = append(warnings, r.logged(subject, id.handle,
			"command", displayCommand(command), func() result {
				return r.runOne(ctx, id, command, subject.Worktree, content)
			})...)
	}
	for i, policyDir := range r.Kyverno {
		id := identify(kindKyverno, i+1)
		warnings = append(warnings, r.logged(subject, id.handle,
			"policies", policyDir, func() result {
				return r.runKyverno(ctx, id, subject.Worktree, policyDir, content)
			})...)
	}
	for i, policyDir := range r.Conftest {
		id := identify(kindConftest, i+1)
		warnings = append(warnings, r.logged(subject, id.handle,
			"policies", policyDir, func() result {
				return r.runConftest(ctx, id, subject.Worktree, policyDir, content)
			})...)
	}
	return warnings
}

// status is how one invocation ENDED. The three values are not derivable from
// the output: a skipped linter and a clean one both contribute zero lines, and a
// failed one contributes exactly one — indistinguishable from a single finding.
type status string

const (
	statusOK      status = "ok"      // ran; whatever it emitted is findings
	statusSkipped status = "skipped" // no policies for that side; never invoked
	statusFailed  status = "failed"  // timeout, spawn failure, refusal, unusable report
)

// result is one invocation's contributed lines plus how it ended.
type result struct {
	lines  []string
	status status
}

// logged runs one linter invocation and records it (see Runner.Logger).
//
// lines counts every line the invocation contributed — findings, skip notes and
// failure warnings alike — because the log's job is proving what the invocation
// emitted, not classifying it; status is what classifies. detailKey/detailValue
// carry the identity of what the linter was pointed at: a command for the shell
// path, a policy directory for the built-ins. They are named per kind rather
// than sharing one key, because a single "target=" would sit next to "side=
// target" in every line and mean something else entirely.
//
// A failed invocation is logged at WARN: it means this side was NOT linted, and
// the only other trace is one warning line among an application's findings,
// which is easy to miss and easy to read as a finding.
func (r *Runner) logged(
	subject Subject,
	linter, detailKey, detailValue string,
	run func() result,
) []string {
	start := time.Now()
	res := run()
	if r.Logger == nil {
		return res.lines
	}
	fields := []any{
		"app", subject.App, "namespace", subject.Namespace, "side", subject.Side,
		"linter", linter, detailKey, detailValue,
		"status", string(res.status),
		"lines", len(res.lines),
		"duration", RoundDuration(time.Since(start)),
	}
	if res.status == statusFailed {
		r.Logger.Warn("Linted", fields...)
	} else {
		r.Logger.Info("Linted", fields...)
	}
	return res.lines
}

// RoundDuration rounds a lint duration for logging: milliseconds below a second,
// tenths of a second above it.
//
// Coarser than it used to be, and safe to coarsen only because status now says
// whether the budget was hit. While the duration was the ONLY hint at that, the
// millisecond tail was load-bearing — 10.009s against a 10s --lint-timeout was
// how a timeout announced itself. Rounding to whole seconds would still be wrong:
// a 9.6s success would print as 10s, erasing the headroom reading that the field
// exists for.
//
// Exported so the per-side aggregate in internal/app rounds identically; one
// rule for lint durations, not one per call site.
func RoundDuration(d time.Duration) time.Duration {
	if d < time.Second {
		return d.Round(time.Millisecond)
	}
	return d.Round(100 * time.Millisecond)
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
func (r *Runner) execTool(ctx context.Context, dir, linter string, argv []string, content string) toolOutput {
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
	// which reads as "cleaner than reality".
	//
	// The two paths then diverge, deliberately. The shell path keeps its stdout and
	// appends the timeout line, because each line is an independent finding and
	// half of them are still true. A built-in's stdout is ONE json document, so a
	// truncated one is not half a report - it is a document whose findings cannot be
	// trusted to be all of them. runKyverno/runConftest therefore return the timeout
	// line ALONE: they discard the partial report rather than publish a count that
	// might read cleaner than reality. TestExecToolHealthContract pins what this
	// helper returns; the adapters' own tests pin the discard.
	if errors.Is(context.Cause(ctx), errLintTimeout) {
		out.warning = fmt.Sprintf("%s timeout after %s", linter, r.Timeout)
		return out
	}

	// A non-zero exit WITH a report is normal, not a failure: both tools exit
	// non-zero precisely because they found something.
	if out.stdout != "" {
		return out
	}

	out.warning = fmt.Sprintf("%s %v with no report output", linter, err)
	if first := firstLine(stderr.String()); first != "" {
		out.warning += ": " + first
	}
	return out
}

// skippedForNoPolicies is what a built-in adapter contributes on a side whose
// policy directory holds nothing: one NOTE, and status skipped.
//
// The note exists because silence made the report state something false. A side
// without policies is not linted, so every finding is labelled with the OTHER side
// only — and the label contract reads a one-sided finding as "introduced by this
// change" ([target]) or "fixed by it" ([base]). On the PR that adds the first
// policy, that turns "pre-existing violations, newly detected" into "this PR
// introduced N violations", which is the report asserting something untrue rather
// than merely omitting something. And it misfires exactly when the tolerance is
// used: with policies on both branches the skip never happens at all.
//
// Reported per SIDE rather than as one summary note about the asymmetry, because
// that is what the fact is: a property of this side, in the same warning list and
// the same [base]/[target] vocabulary as the findings it qualifies. The comparison
// then needs no cross-side inference, and the mirror case — a change that DELETES
// the policies — is covered by the same line without knowing about it.
//
// The cost is a warning badge on every affected application when a PR adds a first
// policy, since ParseWarnings has no severity tier. Accepted: the badge is not a
// false claim about that application, and the alternative was a report that
// misattributes its findings.
func skippedForNoPolicies(linter, policyDir string) result {
	// Shaped like every other line argocdf authors about a linter - outcome first,
	// detail after the colon ("timeout after 10s", "exit status 3: ..."). The
	// directory rides in the detail rather than as a second label, and nothing says
	// "on this side": the [base]/[target] prefix already does.
	return result{
		lines:  []string{fmt.Sprintf("%s not linted: no policies in %q", linter, policyDir)},
		status: statusSkipped,
	}
}

// resolvePolicyDir returns the absolute policy directory and whether it holds any
// policy the tool would load (see HasPolicies for why that is not "holds
// anything"). A missing or policy-less directory reports false rather than an
// error: on the base side of a PR that adds the first policy there is legitimately
// nothing to apply, and both tools treat an empty policy set as a hard error,
// which would otherwise attach a spurious lint failure to every application. Not
// linting is still reported — as a note, see skippedForNoPolicies — because it
// changes how the findings on the other side must be read. A directory that cannot
// be read is a third case and returns an error, which the adapters report as a
// FAILED invocation.
//
// A RELATIVE path resolves against the side's worktree, which is what makes each
// side lint with its own version of the policies. An ABSOLUTE path is used as
// given and therefore points both sides at the same tree — deliberate (a policy
// set shared outside the repo is a real setup) but it forfeits per-side
// resolution, so a PR that changes such a policy shows no difference between
// sides.
func resolvePolicyDir(worktree, policyDir string, exts []string) (string, bool, error) {
	dir := policyDir
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(worktree, policyDir)
	}
	ok, err := HasPolicies(dir, exts)
	if err != nil {
		return "", false, err
	}
	return dir, ok, nil
}

// Policy file extensions per tool: what each one actually LOADS out of a directory
// it is handed. kyverno reads yaml (and accepts json); conftest compiles rego and
// treats everything else as data.
var (
	KyvernoPolicyExts  = []string{".yaml", ".yml", ".json"}
	ConftestPolicyExts = []string{".rego"}
)

// HasPolicies reports whether dir holds at least one file the tool would load,
// searching recursively because both tools do.
//
// "Has entries" is NOT the same question, and using it made the skip note wrong in
// ordinary repository layouts: a .gitkeep, a README, an empty child directory or a
// tool's own test fixtures all make a directory look populated while the tool has
// nothing to apply. kyverno then exits 0 with no results, which argocdf reported as
// status=ok - a linter that silently checked nothing, which is the exact failure
// mode the skip note exists to make visible.
//
// A path that cannot serve as a policy directory is an error rather than "no
// policies": permission denied, or a path that is a regular file, is a setup mistake
// the user must see, not a side that legitimately has no policies. Absence itself is
// not an error - that is the PR-adds-the-first-policy shape.
//
// The root is resolved through SYMLINKS first, because both tools read through one
// when handed the path and `policies/kyverno -> ../shared/policies` is an ordinary
// monorepo layout. filepath.WalkDir never follows a link, root included, so walking
// the literal path reported a symlinked directory as having no policies - the false
// note this whole mechanism exists to prevent. Symlinked CHILDREN inside the tree
// stay unfollowed: that needs cycle detection, and it is a layout neither tool's
// own docs encourage.
func HasPolicies(dir string, exts []string) (bool, error) {
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	} // on error keep the literal path: a missing dir must stay "absent", not "unreadable"

	// WalkDir on a regular file visits it as an ordinary entry and reports no error,
	// so without this a file where a directory was named produced "no policies"
	// (or, with a policy-looking name, silently handed the tool a file path).
	if info, err := os.Stat(dir); err == nil && !info.IsDir() {
		return false, errNotADirectory
	}

	found := false
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory that vanished mid-walk, or one entry we cannot stat, must
			// not mask policies found elsewhere in the tree.
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		if slices.Contains(exts, strings.ToLower(filepath.Ext(path))) {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil // absent: the first-policy shape, not a failure
		}
		// The PATH is stripped from what escapes here. A caller reports the
		// directory the USER spelled (relative, as they typed it), matching every
		// other line argocdf authors about a linter; the resolved absolute worktree
		// path is per-run noise that would land in a PR comment.
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			return false, pathErr.Err
		}
		return false, err
	}
	return found, nil
}

// errNotADirectory is HasPolicies' verdict on a path that exists but cannot hold
// policies. Path-free, for the reason above.
var errNotADirectory = errors.New("not a directory")

// runOne runs one shell command. id identifies it in the warning lines it
// contributes and is exported to the command as ARGOCDF_LINT_ID; the shell path
// can never be skipped, so it ends either ok or failed.
func (r *Runner) runOne(ctx context.Context, id linterID, command, dir, content string) result {
	label := id.bracket()
	cancel := func() {}
	if r.Timeout > 0 {
		ctx, cancel = context.WithTimeoutCause(ctx, r.Timeout, errLintTimeout)
	}
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.WaitDelay = waitDelay
	cmd.Dir = dir
	// ARGOCDF_LINT_ID is per-INVOCATION, so it is layered over Env here rather
	// than living in it: Env is one map shared by every command of the run, and
	// concurrent renders would race on a per-command value written into it.
	//
	// It exists so a shell adapter can prefix its findings exactly as argocdf
	// prefixes its own lines about the same linter ([lint#2/policy-name]) - the
	// identity is argocdf's to assign, and a command cannot infer its own position
	// among the --lint flags. Exported unconditionally, so an adapter may require
	// it and fail loudly rather than guess.
	cmd.Env = childEnv(os.Environ(), r.commandEnv(id))
	cmd.Stdin = strings.NewReader(content)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	warnings := nonEmptyLines(stdout.String())
	if err == nil {
		return result{lines: warnings, status: statusOK}
	}

	// The COMMAND is not echoed back. The identity already says which --lint this
	// was, and a reader who needs the text has it in the flag they wrote (the log
	// line's command= carries only the same truncated prefix). Echoing it cost more
	// than it gave: it
	// was truncated to maxDisplayCommand anyway, and it put whatever the command
	// contained - credentials included - into a PR comment.
	if errors.Is(context.Cause(ctx), errLintTimeout) {
		return result{
			lines:  append(warnings, fmt.Sprintf("%s timeout after %s", label, r.Timeout)),
			status: statusFailed,
		}
	}
	msg := fmt.Sprintf("%s %v", label, err)
	if first := firstLine(stderr.String()); first != "" {
		msg += ": " + first
	}
	return result{lines: append(warnings, msg), status: statusFailed}
}

// commandEnv is Env plus the variables that differ per invocation. Env itself is
// never mutated: it is shared across every command and every concurrent
// application render.
func (r *Runner) commandEnv(id linterID) map[string]string {
	env := make(map[string]string, len(r.Env)+1)
	for k, v := range r.Env {
		env[k] = v
	}
	env["ARGOCDF_LINT_ID"] = id.name
	return env
}

// childEnv builds the environment of a lint child process: parent with every
// entry naming one of extra's keys REMOVED, then extra appended (sorted, so
// the result is deterministic).
//
// The removal keeps the child's environment single-valued; it is NOT what makes
// argocdf's value win. exec.Cmd deduplicates Env and keeps the LAST value for
// each key, so appending alone would already override a stale inherited
// ARGOCDF_CONTEXT (verified on go1.25). The inverted reading — that an appended
// entry LOSES to an inherited one — is the easy mistake here, and it turns the
// built-in adapters' KUBECONFIG append into a bug that is not there. The scrub
// earns its place by making the slice say exactly what the child will see, which
// is what the tests below assert against.
//
// This is NOT the reason the render engine scrubs HELM_*: there the inherited
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
