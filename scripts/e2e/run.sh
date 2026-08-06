#!/usr/bin/env bash
# Run the argocdf e2e suite: for every case branch, diff master -> the branch
# with argocdf and compare all report formats byte-for-byte
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
# Every case then runs a SECOND time with the caches ENABLED, against a suite-wide
# cache directory, and that run is compared against the same pins. The cache layer
# would otherwise be untestable end to end: a key too coarse to tell two sets of
# inputs apart can serve the wrong manifests, and no --no-cache run would ever
# notice. Both passes must match the same expectations, so every pin doubles as a
# cache-soundness oracle without authoring a single extra case.
#
# The cached pass runs TWICE: the first invocation populates (each case's target
# side is unique to it, so nothing earlier can have cached it), the second is
# measured and compared. It must report HITS - a bypass and a correct hit produce
# identical reports, so without that check a silently uncached suite would pass
# while testing nothing - and its MISSES may not exceed its render ERRORS: a failed
# render is never cached and legitimately misses again, but any miss beyond that is
# a warm key that failed to reproduce, which byte comparison cannot see (a
# from-scratch render produces the right bytes). The cache is shared across cases
# (master-side renders are identical between them, so they legitimately reuse each
# other).
#
# --regenerate records from the fresh pass only: a pin taken from a cached render
# would let a stale render become the expectation.
#
# Because the cache is shared, a case can depend on ANOTHER case having populated it:
# expected/<case>/requires-case declares that dependency (resolved into the run order,
# and pulled into a single-case run), while expected/<case>/cache-precondition asserts
# the state actually arrived. Only private-chart-unauth needs either today.
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
              --repo-creds cluster
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
# occurrence of a scalar flag - so "--repo-creds none" alone overrides the
# default creds mode; never restate the whole set. Flags are shell-quoted, so
# values containing spaces work: "my-case:--lint 'kyverno apply ... | jq ...'".
# Two caveats from the flag TYPES (not from this harness):
#   - REPEATABLE flags ACCUMULATE instead of replacing: --lint, -f/--file and
#     --application-namespaces add to the defaults. A default --lint command
#     cannot be removed per case, only added to.
#   - booleans are switched off with the =false form: --kustomize-enable-helm=false
#     (--no-cache is not a boolean any more: it takes a layer, and the cached
#     pass below re-enables both with --no-cache=false)
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
  # The policy directory exists ONLY on the case branch, which is the shape every
  # other lint case cannot have: policies/kyverno and policies/conftest live on
  # master, so both sides always have them and the skip path never runs. Here the
  # base side has no policies, is not linted, and says so - the note that keeps its
  # [target]-only finding from reading as "introduced by this change".
  "lint-policy-added:--lint-kyverno policies/kyverno-added"
  # The same tool twice, which is the only shape where a finding's ORDINAL is the
  # thing distinguishing it: both invocations are kyverno, so #1/#2 (flag order) is
  # all that says which directory a finding came from.
  "lint-two-policy-dirs:--lint-kyverno policies/kyverno --lint-kyverno policies/kyverno-broken"
  # The ONLY case that renders without credentials, carrying the identical fixture to
  # private-chart-bump so the two differ in exactly one flag. It is the end-to-end
  # tripwire for the per-mode chart-cache scope: an anonymous run that SUCCEEDS means
  # a chart fetched under CLUSTER credentials satisfied it.
  #
  # It therefore depends on private-chart-bump's cached pass having run FIRST - it is
  # the only case that fetches this chart, so nothing else fills charts/cluster. Cases
  # run in sort order and "bump" < "unauth", which is why the name ends in -unauth
  # rather than -anon (which sorted first and made the gate hollow: it passed with the
  # scope removed). The ordering is not trusted, it is checked -
  # expected/private-chart-unauth/cache-precondition fails the case if the entry is
  # missing.
  #
  # What it proves is that the pull was ATTEMPTED, not that auth was enforced -
  # anonymous pulls never reach auth here, because the cluster's repository Secret
  # carries insecure: "true" for the registry's self-signed cert and without
  # credentials the pull fails TLS verification first. That message is
  # platform-specific, so normalize.sh collapses its tail (see the rule there).
  "private-chart-unauth:--repo-creds none"
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

# ---- requires-case: dependencies, resolved into the run ORDER -----------------
# A case whose cached pass needs another case to have run first declares it in
# expected/<case>/requires-case (one case name per line). This is the SATISFIER;
# expected/<case>/cache-precondition is the ASSERTION, and the split is deliberate:
# the declaration says how the state gets there, the glob verifies it actually did,
# so a wrong or missing declaration still fails loudly instead of silently.
#
# Applied to every run, not just single-case ones. A full run happens to be correct
# today because sort order puts private-chart-bump before private-chart-unauth - that
# is incidental, and one rename away from being wrong. Resolving here makes the order
# a declared property. For a single-case run it also pulls the dependency in, so
# `run.sh private-chart-unauth` works instead of failing by construction.
#
# ONE level, no transitive chains: a requirement that itself declares one is not
# followed. Deliberate - the precondition glob is the backstop, so an unmet
# transitive need fails as an unmet precondition rather than hiding.
in_list() { local needle="$1"; shift; local e; for e in "$@"; do [ "$e" = "$needle" ] && return 0; done; return 1; }
resolved=()
for name in "${cases[@]}"; do
  req_file="expected/$name/requires-case"
  if [ -f "$req_file" ]; then
    while IFS= read -r req || [ -n "$req" ]; do
      case "$req" in ''|\#*) continue ;; esac
      req=${req#case/}
      in_list "$req" ${resolved[@]+"${resolved[@]}"} && continue
      git rev-parse --verify -q "case/$req" >/dev/null || git rev-parse --verify -q "origin/case/$req" >/dev/null \
        || { echo "requires-case: $req_file names '$req', which has no case/$req branch"; exit 1; }
      [ "$req" = "$name" ] && { echo "requires-case: $req_file names its own case"; exit 1; }
      in_list "$req" "${cases[@]}" || echo "note: running $req first - required by $name ($req_file)"
      resolved+=("$req")
    done < "$req_file"
  fi
  in_list "$name" ${resolved[@]+"${resolved[@]}"} || resolved+=("$name")
done
cases=(${resolved[@]+"${resolved[@]}"})

# ONE run at a time: concurrent invocations share out/ and the suite cache, and the
# race is silent - one run's per-case rm -rf lands under the other's compare, and the
# survivor reports a failure that belongs to neither run. The lock is an atomic
# mkdir; a crashed run can leave it behind, so the message names it rather than
# guessing at liveness.
LOCK_DIR="$PWD/out/.run.lock"
mkdir -p out
if ! mkdir "$LOCK_DIR" 2>/dev/null; then
  echo "another run.sh appears to be running (lock: $LOCK_DIR)"
  echo "concurrent runs share out/ and the suite cache and corrupt each other's results;"
  echo "if no other run is alive, remove the stale lock: rmdir $LOCK_DIR"
  exit 1
fi
trap 'rmdir "$LOCK_DIR" 2>/dev/null' EXIT
trap 'exit 130' INT TERM

# An isolated cache directory, wiped per suite run: the user's real cache would make
# hit counts depend on whatever they rendered earlier, and the suite must start cold.
CACHE_DIR="$PWD/out/.rendercache"
rm -rf "$CACHE_DIR"

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

  # must-log: runtime assertions from checks.grep against the FRESH run's log, for
  # facts a report cannot carry - "this linter was actually invoked" being the
  # motivating one (an adapter that never ran and one that ran finding nothing leave
  # IDENTICAL reports by contract). review-expected.sh ignores these lines: they
  # assert against a live run, not against the pinned tree.
  if [ -f "expected/$name/checks.grep" ]; then
    while IFS= read -r line || [ -n "$line" ]; do
      case "$line" in
        must-log:*)
          pat=${line#must-log:}
          grep -Eq -- "$pat" "$out/run.log" \
            || result="FAIL (must-log pattern not found in run.log: $pat)"
          ;;
      esac
    done < "expected/$name/checks.grep"
  fi
  # ---- cached pass -----------------------------------------------------------
  # Same case, caches ENABLED, compared against the same expectations. Runs only
  # when the fresh pass matched: after a real failure its output is the diagnosis,
  # and a second failure line would just be noise.
  if [ "$result" = PASS ]; then
    cached_args=("${DEFAULT_ARGS[@]}" --base master --target "$ref")
    for f in "${FORMATS[@]}"; do
      cached_args+=(-f "$f:$out/cached/report.$(ext "$f")_$f")
    done
    # The cache flags go LAST, so they beat anything in CASE_ARGS: a case that set
    # its own --no-cache would otherwise silently defeat this pass. If a case ever
    # NEEDS its own cache flags, it has to be exempted from the cached pass instead.
    cached_args+=(${case_extra[@]+"${case_extra[@]}"}
                  --no-cache=false --cache-dir "$CACHE_DIR")
    mkdir -p "$out/cached"
    printf '%s\n' "${cached_args[@]}" > "$out/cached/args"

    # A case may DECLARE what the suite cache must already hold for its cached pass to
    # mean anything (see expected/*/cache-precondition). Checked rather than assumed,
    # so an ordering dependency cannot be broken silently. A line is a glob, optionally
    # prefixed "min=N " to require at least N matches - the difference between "some
    # chart is cached" and "BOTH versions of THIS chart are cached", which is what
    # keeps the precondition from being satisfied by an unrelated earlier case.
    pre="expected/$name/cache-precondition"
    if [ -f "$pre" ]; then
      while IFS= read -r line || [ -n "$line" ]; do
        case "$line" in ''|\#*) continue ;; esac
        want=1 glob="$line"
        case "$line" in
          min=*) want=${line%% *}; want=${want#min=}; glob=${line#* } ;;
        esac
        got=$(compgen -G "$CACHE_DIR/$glob" 2>/dev/null | wc -l | tr -d ' ')
        if [ "$got" -lt "$want" ]; then
          result="FAIL (cache precondition unmet: $got match(es) for $glob, want >= $want - case order changed, the cache layout did, or this run did not include the case that fills it; see expected/$name/cache-precondition)"
        fi
      done < "$pre"
    fi
  fi

  if [ "$result" = PASS ]; then
    # Two invocations, by design rather than by retry. The FIRST populates: whatever
    # this case renders that no earlier case shared - always its target side, since
    # every case branch is unique - gets written to the cache. The SECOND is what is
    # MEASURED and compared. Without the split, a case whose base side hit (warm from
    # earlier cases) was accepted on invocation one, so its target-side entries were
    # written but never re-read - the suite proved base-side cache soundness fifty
    # times and target-side soundness almost never. A case that renders NOTHING
    # would fail the hits check below - none exists today (even no-changes renders
    # root-app), but a future selection-only case pinning affected=0 would need an
    # exemption here.
    for attempt in 1 2; do
      "$ARGOCDF_BIN" "${cached_args[@]}" > "$out/cached/run.log.$attempt" 2>&1
      cached_rc=$?
    done
    command cp -f "$out/cached/run.log.2" "$out/cached/run.log"

    for f in "${FORMATS[@]}"; do
      src="$out/cached/report.$(ext "$f")_$f"
      dst="$out/cached/$f.$(ext "$f")"
      [ -f "$src" ] && command mv -f "$src" "$dst" && "$SCRIPT_DIR/normalize.sh" "$dst"
    done

    # The measured invocation's cache stats, and the errors that legitimately
    # explain a miss: a FAILED render is never cached (a failure must be
    # re-attempted, which is what makes the private-chart-unauth tripwire work),
    # so each errored app accounts for at most one miss. Every miss beyond that
    # is a render that SHOULD have been served - a key that failed to reproduce
    # for identical inputs - which byte comparison alone cannot see, because a
    # from-scratch render produces the right bytes.
    hits=$(sed -n -E 's/.*Render cache hits=([0-9]+).*/\1/p' "$out/cached/run.log" | tail -1)
    misses=$(sed -n -E 's/.*Render cache hits=[0-9]+ misses=([0-9]+).*/\1/p' "$out/cached/run.log" | tail -1)
    errors=$(grep -c 'Error processing application' "$out/cached/run.log")

    if [ "$cached_rc" != "$rc" ]; then
      # Checked BEFORE the hits requirement: a cached run that died prints no stats
      # line at all, and reporting that as "the cache was not exercised" would send
      # the reader after the wrong thing.
      result="FAIL (cached exit $cached_rc != fresh exit $rc)"
    elif [ -z "$hits" ] || [ "$hits" -eq 0 ]; then
      result="FAIL (cached pass reported no cache hits - the cache was not exercised)"
    elif [ "$misses" -gt "$errors" ]; then
      result="FAIL (cached pass missed $misses render(s) with only $errors render error(s) - a warm cache key failed to reproduce)"
    else
      for f in "${FORMATS[@]}"; do
        exp="expected/$name/reports/$f.$(ext "$f")"
        got="$out/cached/$f.$(ext "$f")"
        if [ -f "$exp" ] && ! diff -q "$exp" "$got" >/dev/null 2>&1; then
          result="FAIL (cached $f differs - a cache hit served different bytes)"
          diff -u "$exp" "$got" | head -40 > "$out/cached/$f.expected-diff" 2>/dev/null
        fi
      done
    fi
  fi

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
