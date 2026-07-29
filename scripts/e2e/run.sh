#!/usr/bin/env bash
# Run the argocdf e2e suite: for every case branch, diff master -> the branch
# with argocdf (argocd renderer) and compare all report formats byte-for-byte
# against expected/<case>/. Flags come from DEFAULT_ARGS below, plus any per-case
# overrides in CASE_ARGS.
#
#   run.sh                     run all cases (from expected/, or branches with --regenerate)
#   run.sh case/helm-values    run one or more specific cases
#   run.sh --regenerate [...]  rewrite expected/<case>/ from the current run
#
# Full runs and --regenerate end with the hollow-pin review gate
# (review-expected.sh): byte-exact pins additionally have to satisfy the
# per-case checks.grep files and the global never-pin rules.
#
# Environment:
#   ARGOCDF_BIN   argocdf binary (default: ../argocdf, i.e. a fresh local build)
#   KUBE_CONTEXT  kube context with the bootstrap state (default: kind-argocdf)
#
# The runner never checks branches out - argocdf renders refs via its own
# ephemeral worktrees - and never touches the cluster (see bootstrap.sh).
# kube-version and cluster API versions are AUTO-DETECTED from the kind
# cluster, whose node image is pinned by digest in bootstrap.sh - so renders
# stay deterministic while e2e exercises argocdf's detection paths.
# Runs use --no-cache: every run is a FRESH render, so nondeterminism (random
# chart values, ordering bugs) surfaces immediately instead of hiding behind
# render-cache hits.
#
# Expectations are MODE-AGNOSTIC: verified identical against both the
# controller-backed cluster (e2e:bootstrap) and the controller-less one
# (e2e:bootstrap-static). The Application set matches by construction and argocdf
# reads only .spec/metadata, never controller-written .status. Practical split:
# regenerate on --static (no push, no sync wait), verify on the baseline - which
# needs e2e master PUSHED, since its controller syncs the remote at HEAD.
set -uo pipefail
# This harness lives in the argocdf repo and operates ON the e2e submodule
# (fixtures, branches, expected outputs). Fixture-side content - the lint
# adapter and policies - stays IN the e2e repo, resolved per side/branch.
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# Resolve a relative ARGOCDF_BIN against the CALLER's cwd before we cd away;
# default = the argocdf repo root's build output.
ARGOCDF_BIN="${ARGOCDF_BIN:-$SCRIPT_DIR/../../argocdf}"
case "$ARGOCDF_BIN" in
  /*) : ;;
  *) ARGOCDF_BIN="$PWD/$ARGOCDF_BIN" ;;
esac
cd "$SCRIPT_DIR/../../e2e"
KUBE_CONTEXT="${KUBE_CONTEXT:-kind-argocdf}"
REPO_URL="https://github.com/rgeraskin/argocdf-test-repo.git"
FORMATS=(unified md-fields md-unified html-side-by-side)
ext() { case "$1" in unified) echo diff ;; html-side-by-side) echo html ;; *) echo md ;; esac; }

# ---- argocdf flags ---------------------------------------------------------
# DEFAULT_ARGS: the flag set EVERY case runs with. --base/--target are computed
# per case and appended in the loop.
DEFAULT_ARGS=(--quiet --repo-dir . --repo-url "$REPO_URL"
              --context "$KUBE_CONTEXT" --argocd-namespace argocd
              --renderer argocd --repo-creds cluster
              --kustomize-enable-helm --concurrency 4
              --no-cache --exit-code
              # Above argocdf's 10s default: kyverno apply --cluster costs an API
              # discovery per invocation (~0.3s warm, ~1.4s cold when serial) and
              # --concurrency 4 runs four at once against an API server also
              # serving the controller's reconcile loops in the controller-backed
              # cluster. Three cases tipped over the old 5s default there while
              # every other byte matched. The budget only bounds a hung adapter,
              # so headroom is free, and a timeout is never a valid pin
              # (review-expected.sh bans the warning line). argocdf logs each
              # invocation's duration at INFO if this ever needs retuning.
              --lint-timeout 30s)
# NOTE: --lint is deliberately NOT here. Linting every app of every case cost a
# kyverno process per app per side for 39 cases while only 7 have any finding to
# pin, and those extra invocations were the contention that made the timeout
# fire. The lint commands live in CASE_ARGS below, on the cases that assert on
# them; adding --lint to a case means regenerating it.

# CASE_ARGS: per-case flag overrides, one "<case>:<flags>" entry per case that
# needs them ("<case>" = branch name without the case/ prefix). They are
# APPENDED after DEFAULT_ARGS, and argocdf's flag parser takes the LAST
# occurrence of a scalar flag - so "--renderer native" alone overrides the
# default renderer; never restate the whole set. Flags are shell-quoted, so
# values containing spaces work: "my-case:--lint 'kyverno apply ... | jq ...'".
# Two caveats from the flag TYPES (not from this harness):
#   - REPEATABLE flags ACCUMULATE instead of replacing: --lint, -f/--file and
#     --application-namespaces add to the defaults. A default --lint command
#     cannot be removed per case, only added to.
#   - booleans are switched off with the =false form: --no-cache=false
# Each case still has exactly ONE expectation dir, so changing a case's flags
# means regenerating it. Keys are validated against the case/* branches below:
# renaming a branch must not silently drop its flags.
CASE_ARGS=(
  # --- the cases that assert on lint output; every other case runs without a
  # linter at all. kustomize-basic and kustomize-overrides share one app source
  # that carries a deliberate :latest violation, so both pin a pre-existing
  # (both-sides) finding.
  "kustomize-basic:--lint scripts/lint-kyverno.sh"
  "kustomize-overrides:--lint scripts/lint-kyverno.sh"
  "lint-introduced:--lint scripts/lint-kyverno.sh"
  "lint-fixed-by-pr:--lint scripts/lint-kyverno.sh"
  "lint-cr-policy:--lint scripts/lint-kyverno.sh"
  # Two linters at once: --lint ACCUMULATES, and the rego policy is orthogonal to
  # the kyverno ones, so lint-conftest's findings can only come from conftest
  # while lint-two-adapters must show one from each.
  "lint-conftest:--lint scripts/lint-kyverno.sh --lint scripts/lint-conftest.sh"
  "lint-two-adapters:--lint scripts/lint-kyverno.sh --lint scripts/lint-conftest.sh"
  # --- the same work through argocdf's BUILT-IN adapters, which take a policy
  # directory instead of a shell pipeline. lint-builtin carries the identical
  # fixture to lint-two-adapters, so its expectations differ only in the title
  # line: that is what pins the built-ins as equivalent to the shell adapters,
  # not merely working. lint-builtin-cr is the one that proves the built-in
  # passes the resolved context (a CUSTOM RESOURCE policy needs the cluster to
  # map the GVK; a workload policy would pass even with that wiring broken).
  "lint-builtin:--lint-kyverno policies/kyverno --lint-conftest policies/conftest"
  "lint-builtin-cr:--lint-kyverno policies/kyverno"
  # A CR whose CRD the cluster lacks, linted alongside a real violation: pins that
  # kyverno skips the unmappable kind SILENTLY and that --continue-on-fail keeps
  # the other findings, which it destroys without the flag.
  "lint-unmappable-kind:--lint-kyverno policies/kyverno"
  # A policy whose CEL is broken, in its OWN directory: kyverno reports
  # result=error, which argocdf surfaces marked. The dir is separate because a
  # broken policy in policies/kyverno would stop every other lint case from
  # producing the findings it pins.
  "lint-broken-policy:--lint-kyverno policies/kyverno-broken"
  # "helm-values:--renderer native"
)

# Echo the override flags for one case, empty when it has none.
case_args_for() {
  local entry
  for entry in ${CASE_ARGS[@]+"${CASE_ARGS[@]}"}; do
    case "$entry" in
      "$1:"*) printf '%s' "${entry#"$1":}"; return 0 ;;
    esac
  done
}

regenerate=false
cases=()
explicit_cases=0
for arg in "$@"; do
  case "$arg" in
    --regenerate) regenerate=true ;;
    *) cases+=("${arg#case/}"); explicit_cases=1 ;;
  esac
done

# Case list: explicit args, else the case/* BRANCHES - they are the single
# source of truth (argocdf diffs branches; expected/ is only the assertion
# store). A branch without expectations FAILS below instead of being skipped.
if [ ${#cases[@]} -eq 0 ]; then
  cases=($(git for-each-ref --format='%(refname:short)' refs/heads/case/ refs/remotes/origin/case/ \
    | sed 's#^origin/##' | sed 's#^case/##' | sort -u))
fi
[ ${#cases[@]} -gt 0 ] || { echo "no cases found (no case/* branches)"; exit 1; }

command -v "$ARGOCDF_BIN" >/dev/null 2>&1 || [ -x "$ARGOCDF_BIN" ] \
  || { echo "argocdf binary not found: $ARGOCDF_BIN (build it: mise run build)"; exit 1; }
kubectl --context "$KUBE_CONTEXT" get ns argocd >/dev/null 2>&1 \
  || { echo "cluster not ready; run: mise run e2e:bootstrap"; exit 1; }

# CASE_ARGS hygiene - a harness misconfiguration, so fatal before anything runs
# (and regardless of which cases were selected). An entry naming a case that no
# longer exists would silently stop applying: the case would run with DEFAULT
# flags and still be compared against expectations generated WITH its overrides.
# A duplicate key would silently shadow all but the first.
seen_keys=""
for entry in ${CASE_ARGS[@]+"${CASE_ARGS[@]}"}; do
  case "$entry" in
    *:*) ;;
    *) echo "CASE_ARGS: malformed entry (want '<case>:<flags>'): $entry"; exit 1 ;;
  esac
  key=${entry%%:*}
  git rev-parse --verify -q "case/$key" >/dev/null || git rev-parse --verify -q "origin/case/$key" >/dev/null \
    || { echo "CASE_ARGS: entry for '$key' has no case/$key branch (fix the key in run.sh or restore the branch)"; exit 1; }
  case " $seen_keys " in
    *" $key "*) echo "CASE_ARGS: duplicate entry for '$key' (only the first would apply)"; exit 1 ;;
  esac
  seen_keys="$seen_keys $key"
done

fails=0
printf '%-28s %-8s %s\n' CASE EXIT RESULT
for name in "${cases[@]}"; do
  # Prefer the local branch (development flow); fall back to origin.
  ref="case/$name"
  git rev-parse --verify -q "$ref" >/dev/null || ref="origin/case/$name"
  git rev-parse --verify -q "$ref" >/dev/null || { printf '%-28s %-8s %s\n' "$name" "-" "FAIL (branch missing)"; fails=$((fails+1)); continue; }

  out="out/$name"
  rm -rf "$out"; mkdir -p "$out"
  args=("${DEFAULT_ARGS[@]}" --base master --target "$ref")
  for f in "${FORMATS[@]}"; do
    args+=(-f "$f:$out/report.$(ext "$f")_$f")
  done
  # Overrides go LAST so the scalar flags among them beat the defaults above.
  # eval is what preserves the spec's own quoting (flag values with spaces); it
  # only ever sees CASE_ARGS, authored in this file - never case content.
  if ! eval "case_extra=($(case_args_for "$name"))" 2>/dev/null; then
    printf '%-28s %-8s %s\n' "$name" "-" "FAIL (unparsable CASE_ARGS entry - check its quoting)"
    fails=$((fails+1)); continue
  fi
  args+=(${case_extra[@]+"${case_extra[@]}"})
  # The effective argv, so a byte-diff after a flag change is diagnosable.
  printf '%s\n' "${args[@]}" > "$out/args"
  "$ARGOCDF_BIN" "${args[@]}" > "$out/run.log" 2>&1
  rc=$?

  # Normalize outputs; give files their final names.
  for f in "${FORMATS[@]}"; do
    src="$out/report.$(ext "$f")_$f"
    dst="$out/$f.$(ext "$f")"
    [ -f "$src" ] && command mv -f "$src" "$dst" && "$SCRIPT_DIR/normalize.sh" "$dst"
  done

  if $regenerate; then
    # Rebuild the RECORDED half from scratch so a format that stopped being
    # produced leaves no stale file behind (the strict compare below would
    # otherwise fail on it forever). Scoped to reports/ on purpose: that
    # subdirectory holds only machine-written content, so regeneration cannot
    # reach expected/$name/checks.grep, which is authored from the case's intent
    # and must never be derived from output.
    rm -rf "expected/$name/reports"; mkdir -p "expected/$name/reports"
    for f in "${FORMATS[@]}"; do
      [ -f "$out/$f.$(ext "$f")" ] && command cp -f "$out/$f.$(ext "$f")" "expected/$name/reports/"
    done
    echo "exit: $rc" > "expected/$name/reports/meta.yaml"
    printf '%-28s %-8s %s\n' "$name" "$rc" "REGENERATED"
    continue
  fi

  # Compare. Each format must exist on BOTH sides or NEITHER: an expectation
  # without a produced report means the run broke; a produced report without
  # an expectation means the expectation dir is stale/partial. Silently
  # skipping either would let an incomplete expectation dir PASS.
  if [ ! -d "expected/$name/reports" ]; then
    printf '%-28s %-8s %s\n' "$name" "$rc" "FAIL (no expectations - run: mise run e2e:regenerate case/$name)"
    fails=$((fails+1))
    continue
  fi
  want_rc=$(awk '/^exit:/{print $2}' "expected/$name/reports/meta.yaml" 2>/dev/null)
  result=PASS
  [ "$rc" = "$want_rc" ] || result="FAIL (exit $rc != $want_rc)"
  for f in "${FORMATS[@]}"; do
    exp="expected/$name/reports/$f.$(ext "$f")"
    got="$out/$f.$(ext "$f")"
    if [ -f "$exp" ] && [ ! -f "$got" ]; then
      result="FAIL ($f not produced by run)"
    elif [ ! -f "$exp" ] && [ -f "$got" ]; then
      result="FAIL ($f lacks expectation - run: mise run e2e:regenerate case/$name)"
    elif [ -f "$exp" ] && ! diff -q "$exp" "$got" >/dev/null 2>&1; then
      result="FAIL ($f differs)"
      diff -u "$exp" "$got" | head -40 > "$out/$f.expected-diff" 2>/dev/null
    fi
  done
  [ "$result" = PASS ] || fails=$((fails+1))
  printf '%-28s %-8s %s\n' "$name" "$rc" "$result"
done

# Stale-assertion check (full runs only): expected/ must describe EXACTLY the
# case/* branches. A leftover dir (case renamed/removed without cleaning its
# expectations) is dead weight nothing verifies - fail, don't warn.
if [ $explicit_cases -eq 0 ] && ! $regenerate; then
  for d in $(ls expected 2>/dev/null); do
    git rev-parse --verify -q "case/$d" >/dev/null || git rev-parse --verify -q "origin/case/$d" >/dev/null \
      || { echo "FAIL: stale expectations expected/$d - no case/$d branch (delete the dir or restore the branch)"; fails=$((fails+1)); }
  done
fi

# Hollow-pin review gate: mechanically validate the whole expected/ tree
# (each case's authored checks.grep + global rules - see review-expected.sh --help)
# after any run that blesses it: a full pass or a regenerate. Explicit-case
# verification runs skip it, same as the stale-assertion check above.
review_failed=false
if $regenerate || [ $explicit_cases -eq 0 ]; then
  echo
  "$SCRIPT_DIR/review-expected.sh" || review_failed=true
  if $review_failed && $regenerate; then
    echo "review FAILED on the regenerated expectations: they pin broken behavior - fix the case/checks and regenerate again; do NOT commit expected/ as-is"
  fi
fi

echo
if [ $fails -gt 0 ]; then echo "$fails case(s) FAILED (details in out/<case>/)"; exit 1; fi
if $review_failed; then echo "expected/ FAILED review (violations above)"; exit 1; fi
echo "all ${#cases[@]} cases PASS"
