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
func (r *Runner) runConftest(ctx context.Context, worktree, policyDir, content string) []string {
	label := "lint-conftest " + policyDir

	dir, ok := resolvePolicyDir(worktree, policyDir)
	if !ok {
		// See runKyverno: a side without policies is the PR-adds-a-policy shape,
		// and an empty directory makes conftest error ("no policies found").
		return nil
	}

	argv := []string{
		"conftest", "test",
		"--policy", dir,
		"--parser", "yaml",
		"--all-namespaces",
		"--output", "json",
		"-",
	}

	out := r.execTool(ctx, worktree, label, argv, content)
	if out.warning != "" {
		return []string{out.warning}
	}
	if out.stdout == "" {
		return nil
	}

	return parseConftestReport(label, out.stdout)
}

// parseConftestReport turns conftest's JSON output into warning lines. Split from
// the exec path so the shape conftest actually emits can be pinned without the
// binary.
func parseConftestReport(label, stdout string) []string {
	var results conftestResults
	if err := json.Unmarshal([]byte(stdout), &results); err != nil {
		return []string{fmt.Sprintf("%s: unparsable report: %v", label, err)}
	}

	var warnings []string
	for _, result := range results {
		for _, failure := range result.Failures {
			warnings = append(warnings, fmt.Sprintf("[conftest/%s] %s", result.Namespace, singleLine(failure.Msg)))
		}
		for _, warning := range result.Warnings {
			warnings = append(warnings, fmt.Sprintf("[conftest/%s] %s", result.Namespace, singleLine(warning.Msg)))
		}
	}
	return warnings
}
