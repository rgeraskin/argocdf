---
name: e2e-audit-cases
description: Audit the argocdf e2e suite for hollow pins - assertions that cannot fail - by reading every case's branch diff, CASES.md row and pinned report in BOTH directions, then converting the findings into mechanical gate rules. Use for a periodic suite audit, after a batch of new cases, after an argocdf change that rewrote many pins, before a release, or when asked whether the cases still prove what they claim.
---

# Audit the e2e cases for hollow pins

A **hollow pin** is an assertion that cannot fail: the case passes forever whether or not the behavior it targets still works. The e2e suite has structural reasons to grow them, so they need to be hunted deliberately - this skill is that hunt, plus the step that keeps it from being needed as often. Read `e2e/README.md` and `e2e/CASES.md` first; `e2e-add-case` is the sibling skill for authoring.

Every pass ends by converting what it found into rules (see "Convert findings into rules"). The audit is not a recurring inspection to be repeated identically forever - it is how mechanical checks get discovered. A finding that stays human is a finding that will have to be re-found.

## Why a green suite is not the audit

`scripts/e2e/run.sh` ending in `all N cases PASS` means the output has not drifted. It does not mean any case would notice if the behavior it targets broke. `expected/<case>/checks.grep` enforces what someone thought to write down; `scripts/e2e/review-expected.sh` enforces global hygiene. Neither interrogates the space around the assertions, and that is where hollow pins live. **Review the diffs; do not rely on the checks passing.**

## The two directions

Every case gets checked both ways, because they fail differently:

- **claim -> report**: does the artifact CASES.md promises actually appear in the pinned report? A claim can be aspirational.
- **report -> claim**: is everything in the pinned report explained by the row? Extra app sections, extra resources or extra findings get pinned silently - nothing asserts exhaustiveness, and a brand-new case has no baseline diff in which an extra would stand out.

Then the decisive question for every assertion, and especially every `must-not:` line: **what regression would make this line appear or disappear?** If there is no answer, the assertion is decoration.

## Gather the three artifacts

```bash
# 1. what each branch actually changes (content lines only, comments stripped)
for b in $(git -C e2e branch --list 'case/*' | tr -d ' *' | sort); do
  printf "### %s\n" "${b#case/}"
  git -C e2e diff master.."$b" | grep -E '^[+-]' | grep -vE '^(\+\+\+|---)' | grep -vE '^[+-]\s*#' | sed 's/^/    /'
done

# 2. the intent, one row per case
cat e2e/CASES.md

# 3. what the pins actually contain, as structure (direction B)
python3 .claude/skills/e2e-audit-cases/pin-inventory.py
```

## Per-case pass

1. **Cheap first filter**: does the claim's subject appear in the branch diff at all? If the row talks about a file, resource or flag the branch never touches, the claim can only be riding on an unchanged context line or on nothing. Both need justification in the row.
2. **Direction A**: locate each artifact the row promises in the pin. Confirm `checks.grep` names the load-bearing ones - a row that claims more than the checks assert is drift, even when the pin is correct.
3. **Direction B**: read the inventory entry against the row. Every app section, resource key and warning line must be explained by the branch change. Unexplained content is a finding even when it looks harmless.
4. **Falsifiability**: for each `must-not:` and each clause of `proves`, name the regression that would trip it. Prove the load-bearing ones by mutation (below) rather than by reasoning alone.
5. **Read the pin as an artifact, not only as an assertion**: is it valid patch syntax? valid markdown? Does an error message reference a flag argocdf does not have? Bugs surface here that no assertion was written for.

## Hollow-pin taxonomy

Seven shapes found in practice. Each entry is `tell -> fix`.

| pattern                          | tell                                                                                                                                                                                     | fix                                                                                                                                     |
|----------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------|
| **subject not perturbed**        | the claim is about a file the branch never edits, so it renders identically on both sides (or not at all) and can never reach a diff                                                     | make the branch edit it; the pin usually does not move, and the assertion becomes live                                                  |
| **one token from passing**       | the fixture's property is accidental - a plausible "cleanup" would silently make the case prove nothing (a CR group one character from a CRD that IS installed)                          | choose a value that cannot be mistaken (`notinstalled.example.com`), and say in the fixture why                                         |
| **masked by format**             | the render pipeline erases the distinction the case claims (`\| quote` hides scalar type), or the tool never produces it for that input (helm's strvals never int-parses a leading zero) | change the fixture so the distinction is visible (`toYaml`, `kindOf`), or move the claim to a case where it is                          |
| **proven only negatively**       | the claim's subject appears in no report; only an error would reveal its loss                                                                                                            | add the twin case that moves it, so each case also asserts the other did not move                                                       |
| **claim in the wrong row**       | the evidence exists, but in a different case's report (a same-path leak needs two renders in ONE run)                                                                                    | move the sentence to the row whose pin actually carries the evidence                                                                    |
| **review-only cross-case claim** | "differs only in the title line", "identical fixture" - true today, unenforced, and regenerating both sides makes it quietly false                                                       | add a mechanical comparison, or mark the claim as review-only in the row                                                                |
| **context-line proof**           | the assertion matches an unchanged context line inside someone else's hunk                                                                                                               | acceptable if the byte pin still moves when the value is wrong - but say so in the row, and prefer a case whose own change is the proof |

Append new shapes as they are found. The taxonomy is the accumulated memory of this audit: the next pass starts from known shapes rather than from scratch.

## Techniques

**Mutation (the tripwire test)** - the only way to *prove* an assertion is live. Break the invariant on a temp branch, confirm the assertion fires, delete the branch:

```bash
cd e2e && git checkout -q -b tmp/tripwire master
# ... make the change the case is supposed to catch (enable recursion, drop a flag) ...
git add -A <paths> && git commit -q -m "tmp: tripwire"
cd .. && ./argocdf --repo-dir e2e --repo-url https://github.com/rgeraskin/argocdf-test-repo.git \
  --context kind-argocdf --argocd-namespace argocd --renderer argocd --repo-creds cluster \
  --no-cache --base master --target tmp/tripwire --stdout unified | grep -E '<the string the must-not forbids>'
git -C e2e checkout -q master && git -C e2e branch -D tmp/tripwire
```

**Run mutations and the full suite one at a time, never together.** They compete for the same machine, and the suite's failure mode under load is a LINT TIMEOUT: `kyverno apply --cluster` pays an API discovery per invocation, `--concurrency 4` runs four at once, and a mutation experiment alongside that pushed three cases past `--lint-timeout 30s` - reported as `lint "<cmd>": timeout after 30s` in place of their real findings. That looks exactly like a regression in the cases you are auditing and costs a re-run to disprove. (The never-pin rules stop a loaded `--regenerate` from turning such a timeout into a pin, which is the only reason contention is a wasted run rather than a corrupted suite.)

**Prefer empirical proof to source-reading.** Reading helm's `pkg/strvals` suggested `forceString` was inert; *removing* it and seeing the rendered ConfigMap come out byte-identical proved it. When a claim is about tool behavior, do read the tool's source - then confirm with bytes.

**Cross-check parity claims against real ArgoCD.** For a claim of the form "argocdf matches ArgoCD", ask ArgoCD: `argocd app manifests --revision <case-branch> <app>`. Check `argocd context` first - a production context answers a different question and looks like agreement. The repo-server log is the non-invasive alternative: `kubectl -n argocd logs deploy/argocd-repo-server --since=48h | grep <app>` lists every revision it ever rendered, so "was it even asked?" is answerable without touching any CLI session.

**Let the gate catch you too.** Author `checks.grep` before regenerating a fixture you have just changed. A fixture built to expose a behavior can fail to expose it; the gate refusing the pin is the fastest way to learn that.

## Convert findings into rules (required closing step)

For every finding, ask: **could this have been caught mechanically?** If yes, implement the rule in the same session as the fixture fix - a finding left to human attention is one that will have to be re-found. Rules live in `scripts/e2e/review-expected.sh` (Rule A: per-case checks; Rule B: global rules) and are asserted on every full run and inside `push.sh`, so adoption is permanent.

What makes a good rule: it must survive `--regenerate`, which rewrites `reports/` but never `checks.grep`. Anything authored in `checks.grep` therefore keeps asserting against freshly generated output; anything derived from `reports/` only re-derives itself.

Rules already in force (do not re-derive them; verify they still bite):

- **Exhaustiveness (direction B)** - ADOPTED. `expect:affected=N changed=M [resources=+a,-r,~m] [errors=E]`, exactly one per case, authorizes the whole summary block: omitted fields must be absent from the report too. An extra application section or changed resource now fails the gate instead of being pinned. Mutation-tested six ways (wrong count, missing line, duplicate line, field named where the report lacks it, field dropped where the report has it, unparsable field).

Standing candidates, highest value first:

- **App-section identity.** Counts miss a swap (app A replaced by app B at the same count). An authored `apps:root-app,typed-params` line closes that.
- **Cross-case equality.** For twin cases ("differs only in the title line"), diff the two `reports/` directories ignoring the first line and fail on any other difference.
- **Falsifiability metadata.** A `must-not:` line whose comment does not state what would make it fire is a smell; the gate can require a comment above each `must-not:`, which at least forces the author to answer the question.

Record every adopted rule in the audit report, and delete the corresponding item from the "not guaranteed" list below. The audit's own scope should shrink pass over pass; if it does not, the findings are not being converted.

## Rules of engagement

- Never regenerate to make a discrepancy disappear before understanding it. `--regenerate` rewrites `reports/` only; a wrong pin becomes the new baseline silently.
- Never derive a `checks.grep` pattern from generated output. Patterns come from intent.
- When an assertion is right but unfalsifiable, fix the **fixture or branch**, not the assertion.
- Record the *why* at the edit site (a comment in the fixture or catalog), not only in CASES.md. A future cleanup reads the fixture, not the catalog of intent.
- Three cases pin argocdf's actual contract rather than an ideal one (`kustomize-relative-base`, `error-invalid-yaml`, `no-changes`). Deliberate invisibility is fine - unstated invisibility is the finding.

## Deliverable

1. A per-case table: case | branch edit -> pinned effect | verdict. Every case appears, including the trivial ones.
2. Findings, each with its taxonomy pattern and a proposed fix, ordered by cost.
3. **Rules adopted this pass**, with the diff to `review-expected.sh` and the per-case `checks.grep` lines they require.
4. The remaining "not guaranteed" list, shortened by whatever step 3 covered. As of the last audit: CASES.md is prose with no binding to the pin; `must-not` falsifiability is unchecked; app-section IDENTITY is unchecked (only counts are, via `expect:`); the suite verifies argocdf against itself, not against ArgoCD; each case pins ONE flag combination (`--renderer native`, `--repo-creds local|none` are unpinned); cross-case equality claims are review-only.

## Cadence

Run after a batch of new cases, after an argocdf change that rewrote many pins, before a release, or on request. A full pass over ~45 cases is one focused session; the per-case work is minutes each, and the value concentrates in the cases whose claims mention something the branch does not touch - start there.
