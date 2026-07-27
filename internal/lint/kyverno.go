package lint

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// kyvernoReport is the subset of `kyverno apply --policy-report
// --output-format json` argocdf reads. The CLI emits ONE report object (not an
// array) whose results carry the policy name, the outcome, the offending
// resources and the message.
type kyvernoReport struct {
	Results []struct {
		Policy    string `json:"policy"`
		Result    string `json:"result"`
		Message   string `json:"message"`
		Resources []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"resources"`
	} `json:"results"`
}

// runKyverno lints content with the kyverno policies in policyDir and returns
// one warning per failing or warning result.
//
// The flags are not incidental:
//
//   - --cluster, plus the resolved --context, because a policy may target a
//     CUSTOM RESOURCE (an argoproj.io Application rendered by an apps-of-apps
//     parent) and kyverno can only evaluate a resource whose GVK it can map to a
//     GVR. Live discovery supplies that mapping. The cluster consulted must be
//     the one argocdf is diffing, never the invoking shell's: reporting findings
//     about a DIFFERENT cluster is indistinguishable in the report from a
//     correct result. Without a resolved context this refuses to run.
//   - --continue-on-fail, because a diff tool renders manifests that are not in
//     the cluster YET (a PR adding a CRD alongside its CRs, a chart shipping CRs
//     for an operator this cluster lacks). Those kinds fail discovery in cluster
//     mode, and without the flag ONE unmappable document hard-fails the whole
//     apply, discarding the findings kyverno already made on ordinary workloads
//     in the same input.
func (r *Runner) runKyverno(ctx context.Context, worktree, policyDir, content string) []string {
	label := "lint-kyverno " + policyDir

	dir, ok := resolvePolicyDir(worktree, policyDir)
	if !ok {
		// No policies on THIS side: the normal shape when a PR adds the first
		// policy, so the base side has nothing to apply. Handing kyverno an
		// empty or missing directory makes it error, which would surface as a
		// spurious lint failure on every app.
		return nil
	}

	// Refused rather than defaulted: kyverno would silently fall back to the
	// ambient context and lint the wrong cluster.
	if r.KubeContext == "" {
		return []string{fmt.Sprintf("%s: no resolved kube context, refusing to lint against an unknown cluster", label)}
	}

	argv := []string{
		"kyverno", "apply", dir,
		"--resource", "-",
		"--policy-report",
		"--continue-on-fail",
		"--cluster",
		"--context", r.KubeContext,
		"--output-format", "json",
	}

	out := r.execTool(ctx, worktree, label, argv, content)
	if out.warning != "" {
		return []string{out.warning}
	}
	if out.stdout == "" {
		// Exit 0 with no report: nothing the policies match. Legitimately
		// common — a report full of ConfigMaps matches no workload policy.
		return nil
	}

	return parseKyvernoReport(label, out.stdout)
}

// parseKyvernoReport turns a kyverno policy report into warning lines. Split from
// the exec path so the shape kyverno actually emits can be pinned without a
// cluster or the binary.
func parseKyvernoReport(label, stdout string) []string {
	var report kyvernoReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		return []string{fmt.Sprintf("%s: unparsable report: %v", label, err)}
	}

	var warnings []string
	for _, result := range report.Results {
		// pass/skip/error are not findings: a report is mostly passes, and
		// `error` is kyverno's own trouble with a resource, which --continue-on-fail
		// exists to tolerate.
		if result.Result != "fail" && result.Result != "warn" {
			continue
		}
		subject := ""
		if len(result.Resources) > 0 {
			subject = result.Resources[0].Kind + "/" + result.Resources[0].Name + ": "
		}
		warnings = append(warnings, fmt.Sprintf("[kyverno/%s] %s%s",
			result.Policy, subject, singleLine(result.Message)))
	}
	return warnings
}

// singleLine folds a policy message onto one line: every stdout line becomes a
// separate warning, so an embedded newline would split one finding into two.
func singleLine(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
}
