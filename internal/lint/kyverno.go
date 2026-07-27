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
//     for an operator this cluster lacks). Measured against kyverno 1.18 with one
//     unmappable document beside one violating Deployment: WITHOUT the flag,
//     exit 1, ZERO bytes of report and `failed to map gvk to gvr` on stderr — the
//     Deployment's finding is destroyed by the unrelated document. WITH it, the
//     finding survives and the unmappable kind is skipped SILENTLY: no result at
//     all, not even an `error` one. That silence is a hazard of its own — a policy
//     over such a kind simply does not run and the report still reads clean — so
//     the README tells users to apply CRDs first, and case/lint-unmappable-kind
//     pins both halves.
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
		var marker string
		switch result.Result {
		case "fail", "warn":
		case "error":
			// An `error` is kyverno failing to EVALUATE, not a resource failing a
			// check — a broken CEL expression reports
			// "expression '...' resulted in error: no such key: x" with no
			// resources attached. Dropping those was hiding the one failure a
			// policy author cannot otherwise notice: with checks split across many
			// small policies (which kyverno's fail-fast within a policy forces),
			// one broken expression just stops producing findings. The marker keeps
			// it distinguishable — a violation is about the manifests, an error is
			// about the setup.
			marker = "ERROR "
		default:
			// pass and skip are not findings; a report is mostly passes.
			continue
		}

		msg := singleLine(result.Message)
		if len(result.Resources) == 0 {
			warnings = append(warnings, fmt.Sprintf("[kyverno/%s] %s%s", result.Policy, marker, msg))
			continue
		}
		// One line PER resource. kyverno 1.18 happens to emit a single resource per
		// result for both ValidatingAdmissionPolicy and ClusterPolicy (verified),
		// but `resources` is a LIST in the openreports.io PolicyReport schema, so
		// naming only the first would silently drop subjects the moment any
		// producer — a future kyverno, or another tool writing the same schema —
		// groups them. Per-resource lines also match how every other finding in the
		// report reads: one resource, one line, greppable by Kind/name.
		for _, res := range result.Resources {
			warnings = append(warnings, fmt.Sprintf("[kyverno/%s] %s%s/%s: %s",
				result.Policy, marker, res.Kind, res.Name, msg))
		}
	}
	return warnings
}

// singleLine folds a policy message onto one line: every stdout line becomes a
// separate warning, so an embedded newline would split one finding into two.
func singleLine(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
}
