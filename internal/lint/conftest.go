package lint

import (
	"context"
	"encoding/json"
	"fmt"
)

// conftestResults is the subset of `conftest test --output json` argocdf reads.
// The CLI emits an ARRAY (one entry per rego package it evaluated), each with
// the package name in "namespace" and its findings split into failures and
// warnings. A message is whatever the policy's rule produced, so the resource it
// refers to is named by the policy rather than by the tool.
type conftestResults []struct {
	Namespace string `json:"namespace"`
	Failures  []struct {
		Msg string `json:"msg"`
	} `json:"failures"`
	Warnings []struct {
		Msg string `json:"msg"`
	} `json:"warnings"`
}

// runConftest lints content with the rego policies in policyDir and returns one
// warning per failure and warning.
//
// Unlike kyverno this is entirely OFFLINE: rego evaluates each document as plain
// data, so there is no GVK->GVR mapping to resolve and nothing to gain from the
// cluster. --all-namespaces evaluates every rego package found instead of only
// `main`, which is what lets a policy directory hold more than one rule set.
//
// policyDir is handed over whole rather than as a file list: conftest recurses,
// loads only rego, and a policy added under any file name still runs. Keep
// non-policy files out of it — conftest's own `*_test.rego` unit tests are
// harmless (they define test_* rules, not deny rules), but tool-specific test
// manifests are not universally safe (a kyverno-test.yaml beside a kyverno
// policy makes `kyverno apply` emit nothing at all, silently).
func (r *Runner) runConftest(ctx context.Context, id linterID, worktree, policyDir, content string) result {

	dir, ok, err := resolvePolicyDir(worktree, policyDir, ConftestPolicyExts)
	if err != nil {
		// Unusable (unreadable, or not a directory at all) is a setup mistake, not
		// a side without policies.
		return result{
			lines:  []string{fmt.Sprintf("%s unusable policy directory %q: %v", id.bracket(), policyDir, err)},
			status: statusFailed,
		}
	}
	if !ok {
		// See runKyverno: a side without policies is the PR-adds-a-policy shape,
		// and an empty directory makes conftest error ("no policies found").
		return skippedForNoPolicies(id.bracket(), policyDir)
	}

	argv := []string{
		"conftest", "test",
		"--policy", dir,
		"--parser", "yaml",
		"--all-namespaces",
		"--output", "json",
		"-",
	}

	out := r.execTool(ctx, worktree, id.bracket(), argv, content)
	if out.warning != "" {
		return result{lines: []string{out.warning}, status: statusFailed}
	}
	if out.stdout == "" {
		return result{status: statusOK}
	}

	return parseConftestReport(id, out.stdout)
}

// parseConftestReport turns conftest's JSON output into warning lines. Split from
// the exec path so the shape conftest actually emits can be pinned without the
// binary. An unparsable report is FAILED, for the reason parseKyvernoReport
// documents.
func parseConftestReport(id linterID, stdout string) result {
	var results conftestResults
	if err := json.Unmarshal([]byte(stdout), &results); err != nil {
		return result{
			lines:  []string{fmt.Sprintf("%s unparsable report: %v", id.bracket(), err)},
			status: statusFailed,
		}
	}

	// entry, not result: `result` is the invocation-outcome type in this package.
	var warnings []string
	for _, entry := range results {
		for _, failure := range entry.Failures {
			warnings = append(warnings, fmt.Sprintf("[%s/%s] %s", id.name, entry.Namespace, singleLine(failure.Msg)))
		}
		for _, warning := range entry.Warnings {
			warnings = append(warnings, fmt.Sprintf("[%s/%s] %s", id.name, entry.Namespace, singleLine(warning.Msg)))
		}
	}
	return result{lines: warnings, status: statusOK}
}
