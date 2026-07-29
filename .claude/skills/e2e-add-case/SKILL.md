---
name: e2e-add-case
description: Add a new test case to the argocdf e2e suite (the e2e/ submodule) - fixture, spec, or new-app case flows, expectation regeneration, hollow-pin and determinism checks. Use whenever asked to add/extend an e2e case, scenario, or coverage for a render behavior.
---

# Add a new e2e test case

The e2e suite lives in the `e2e/` submodule: `case/<name>` git branches are the test cases (one focused commit off `master` each), `expected/<name>/` on master holds both halves of that case's verification: `reports/` are argocdf's byte-exact outputs (RECORDED — `--regenerate` wipes and rewrites them) and `checks.grep` pins the case's INTENT for the mechanical review gate (AUTHORED — it survives regeneration, which is why the rm is scoped to `reports/`). The harness lives in this repo at `scripts/e2e/`. Read `e2e/README.md` for the architecture.

## Prerequisites

```bash
mise run build                  # fresh ./argocdf binary
mise run e2e:bootstrap-static   # kind cluster, no controller (idempotent; picks up mise [env] pins)
git -C e2e checkout master && git -C e2e status --short   # clean, on master
```

Use the **static** cluster for authoring: `mise run e2e:bootstrap` is the baseline and installs a real ArgoCD whose controller syncs the REMOTE repo at `targetRevision: HEAD`, so it would not see a fixture until it is pushed — and you cannot push before the review gate passes. The static cluster applies the same Application set directly and produces byte-identical output, so pins made on it are valid for both. Verify on the baseline after publishing.

## 1. Choose the case type

- **App-source case** (rendered content changes): the branch edits files under `e2e/apps/<dir>/`. Most cases.
- **Spec case** (an Application's spec changes): the branch edits `e2e/charts/apps/values-apps.yaml` (the catalog). Covers helm/kustomize spec fields, child add/remove/spec-change.
- **New-app case** (needs a child app that doesn't exist yet): FIRST add the fixture dir + catalog entry to e2e **master** (commit), then `mise run e2e:bootstrap-static` (the cluster must learn the new child), then branch the actual change. A master catalog change shifts context lines in OTHER cases' root-app diffs - rebase every existing branch and regenerate ALL expectations (see step 7).

## 2. Create the branch

```bash
cd e2e
git checkout -b case/<kebab-name> master
# ... ONE focused edit ...
git add <only-the-files-you-changed>   # NEVER `git add -A` on case branches:
                                       # untracked expected/ or working files
                                       # get swept into the branch, and
                                       # checking master back out DELETES them
git commit -m "case/<name>: <what it proves>"
git checkout master
```

Rules: one intention per case; kebab-case name; NO company references (placeholder registries like ghcr.io/acme; public charts only - podinfo is the standard remote chart). Charts that render random values per run (helm test hooks with randAlphaNum - podinfo does) need `helm.skipTests: true` in the catalog entry or the expectations will flake.

## 3. Author the checks file (BEFORE regenerating - TDD for pins)

Write `e2e/expected/<name>/checks.grep` on e2e master now, from the case's INTENT - never from generated output you have already seen (a check derived from output would happily bless a hollow report). The review gate (`scripts/e2e/review-expected.sh`) enforces it against `expected/<name>/reports/unified.diff` forever. Format (`--help` documents it): one directive per line, `#` comments and blanks ignored:

```
# the summary block this case may produce, and nothing more
expect:affected=8 changed=8 resources=+0,-0,~8
# pins the replica bump this case exists to prove
must:^\+    replicas: 2
must-not:# No changes
```

`must:<ERE>` must match `expected/<name>/reports/unified.diff`; `must-not:<ERE>` must NOT. At least one `must:` line for the exact artifact the case proves (the changed field, the lint label, the error text, the discovered app) is required - a case without a checks file, or with zero `must:` lines, fails review. Patterns match the REPORT text (unified.diff hunks: 4-space-indented YAML, `+`/`-` line prefixes), unlike `--lint` patterns which see raw manifests.

Exactly one `expect:` line is also required, and it is the half that catches a report with something EXTRA in it - `must:`/`must-not:` say nothing about the rest of the report, so an unexpected application section or resource would just get pinned. **Predict the numbers before regenerating**: count the apps whose source path the change touches (a shared path fans out - `apps/web-app` is rendered by 8 apps), add the parent when the catalog changed, and count the resources the edit can move. A mismatch means the case reaches further than you thought, which is exactly what you want to learn before it becomes a pin. Omitted fields must be absent from the report too: no `resources=` for a case with no resource changes, `errors=N` only when a render error is expected.

## 4. Regenerate the expectation

If the case needs argocdf flags the suite does not run by default, add its `CASE_ARGS` entry in `scripts/e2e/run.sh` FIRST - flags chosen after regenerating would leave the pins describing a different run. Entries are `"<name>:<flags>"`, appended after `DEFAULT_ARGS`, so a scalar flag overrides the default on its own (`--renderer native`); repeatable flags (`--lint`, `-f`, `--application-namespaces`) accumulate on top of the defaults rather than replacing them.

**A case that asserts on lint output MUST bring its own linter.** It is deliberately not in `DEFAULT_ARGS`: linting every app of every case spawned a policy engine per app per side for cases with nothing to pin, and that contention is what made `--lint-timeout` fire. Add either `"<name>:--lint scripts/lint-kyverno.sh"` (the shell adapters — keep using these for anything that proves the SHELL contract, e.g. `ARGOCDF_CONTEXT` propagation) or `"<name>:--lint-kyverno policies/kyverno"` (argocdf's built-in adapters), matching the existing lint entries. The two produce byte-identical findings, which `lint-builtin` vs `lint-two-adapters` pins by running one fixture both ways — so a new case picks whichever path it is actually testing.

```bash
cd .. && ARGOCDF_BIN=./argocdf scripts/e2e/run.sh --regenerate case/<name>
```

Regeneration ends with the review gate over the whole expected/ tree: if the fresh pins violate your checks file or the global rules, it FAILS and the pins must not be committed.

## 5. Verify the pin is REAL (hollow-pin check - this trap has bitten twice)

The mechanical part is enforced for you: review runs automatically after `--regenerate`, after full `run.sh` runs and before `push.sh` publishes, and standalone via `mise run e2e:review` - your checks file plus global rules (never-pinned lint-crash/`jq: error` lines, `<worktree>`/`<tempfile>`/`panic:`, the exit-code contract, cross-format count agreement, all five reports/ files present). But the gate asserts only what the checks file says: still review `e2e/expected/<name>/` like code and grep for the exact artifact the case exists to pin. If it is absent, the case is hollow - common causes:

- lint/grep patterns written against REPORT text instead of the RAW rendered manifests the tool receives (raw = 2-space indent, alphabetized keys, so `image:` appears as `- image:` list form);
- the edit didn't affect any app the cluster knows (file outside every app's source path - see the `kustomize-relative-base` case, which pins exactly that limitation);
- error-path cases: per-run temp paths and helm's map-ordered `--set` flags in error text are normalized by the harness - but multi-parameter apps in error cases are still best avoided (see how `error-invalid-yaml` targets a single-app chart with no values/parameters).

Exit code lives in `expected/<name>/reports/meta.yaml`: 0 = no changes, 1 = fatal, 2 = changes present. Error-path cases pin 0 or 2 deliberately - app-level render errors are report content, not process failures (review enforces this: exit 2 iff changed > 0, exit 1 never pinned).

## 6. Determinism check

The runner is `--no-cache` (every run renders fresh), so run the case twice:

```bash
ARGOCDF_BIN=./argocdf scripts/e2e/run.sh case/<name>
ARGOCDF_BIN=./argocdf scripts/e2e/run.sh case/<name>
```

Both must PASS. A flake means nondeterministic render content (random chart values, shared-path mutation) - fix the fixture, don't normalize around it unless it is argocdf-inherited (like the error-text rules already in `scripts/e2e/normalize.sh`). Special case: kustomize-app and kustomize-app-overrides share `apps/kustomize-app` ON PURPOSE (regression pin for the same-path `kustomize edit` leak fix) - a flake THERE means that fix regressed, not that the fixture is wrong.

## 7. If master changed (new-app flow): rebase + full regeneration

```bash
cd e2e
for b in $(git branch --list "case/*" | tr -d ' '); do
  git rebase master "$b" || { git rebase --abort; echo "CONFLICT: $b"; }
done
git checkout master
# conflicted branches (usually catalog-append collisions): recreate from
# master with the same one edit rather than resolving
# rm ONLY the recorded half: expected/<case>/checks.grep is authored from intent
# and must survive. `rm -rf e2e/expected` would delete every one of them.
cd .. && rm -rf e2e/expected/*/reports && ARGOCDF_BIN=./argocdf scripts/e2e/run.sh --regenerate
```

(The publish step repeats this rebase anyway — `scripts/e2e/push.sh` refuses to push until every branch is exactly one commit off master — but rebasing BEFORE regenerating is what makes the regenerated expectations correct.)

## 8. Full-suite verification and review

```bash
ARGOCDF_BIN=./argocdf scripts/e2e/run.sh    # ALL cases must PASS (incl. the
                                            # review gate at the end)
git -C e2e diff && git -C e2e status        # review expected/ LIKE CODE -
                                            # every hunk is pinned behavior
```

## 9. Commit and publish (both repos, in this order)

```bash
git -C e2e add expected/ <master-fixture-paths-if-any>
git -C e2e commit -m "Pin expectations for case/<name>"        # e2e master
git add e2e && git commit -m "test: e2e case <name> (<what it proves>)"  # pointer bump
mise run e2e:push   # refuses to publish if review fails, then rebases all
                    # case/* onto master, pushes master, FORCE-pushes the
                    # branches (rebase rewrites their heads on every master
                    # commit - force is inherent, master only ever
                    # fast-forwards)
```

Add the case to `e2e/CASES.md` (one table row: the branch change, what the pinned report must show, and what it proves — the middle column is what makes a hollow claim visible), and update `e2e/README.md`'s network note if the case fetches remote artifacts.

Markdown style for docs in these repos: never hard-wrap prose - one paragraph or list item = one source line (viewers word-wrap).
