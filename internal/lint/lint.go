// Package lint pipes rendered manifests through user-supplied shell commands
// and turns their stdout lines into report warnings.
//
// The contract is tool-agnostic: each command receives an application's
// rendered multi-doc YAML on stdin and emits one finding per stdout line.
// The process outcome is the only health signal — stdout content plays no
// role in error detection. Tools like kyverno or conftest exit non-zero on
// findings during normal operation, so commands are expected to end in an
// adapter (typically jq) that exits 0 when the pipeline worked.
package lint

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Runner executes lint commands against rendered manifest content.
type Runner struct {
	// Commands are shell commands run in order via `sh -c`.
	Commands []string

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

// Lint runs every command with content on stdin and dir as the working
// directory (empty = inherit), and returns the collected warning lines. Lint
// is never fatal: stdout lines are kept even when the command fails, and a
// failure (spawn error, timeout, exit != 0) appends one self-identifying
// warning line instead of returning an error.
//
// dir is the side's ephemeral worktree, so repo-relative paths in the command
// (e.g. a policy directory) resolve to that side's version of the files.
// Commands see argocdf's own environment with Env layered on top.
func (r *Runner) Lint(ctx context.Context, dir, content string) []string {
	var warnings []string
	for _, command := range r.Commands {
		warnings = append(warnings, r.runOne(ctx, command, dir, content)...)
	}
	return warnings
}

func (r *Runner) runOne(ctx context.Context, command, dir, content string) []string {
	cancel := func() {}
	if r.Timeout > 0 {
		ctx, cancel = context.WithTimeoutCause(ctx, r.Timeout, errLintTimeout)
	}
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
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
// The removal is load-bearing, not tidiness: a duplicated key resolves to its
// FIRST occurrence in Go child processes, so merely appending would let a
// stale inherited value win — e.g. a user who configured ARGOCDF_CONTEXT in
// the environment and then overrode it with --context on the command line
// would see lint commands target the environment's cluster. This is the same
// hazard the argocd renderer scrubs the HELM_* variables for.
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
