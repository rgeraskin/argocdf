#!/usr/bin/env bash
# Mechanical review gate for e2e/expected/: byte-exact regeneration happily
# pins BROKEN behavior ("hollow pins" - a "# No changes" report for a case
# that exists to prove a diff, a lint-adapter crash line pinned as expected
# output). This script validates every case against explicit, machine-checked
# assertions so wrong pins cannot be committed unnoticed.
#
#   review-expected.sh           validate every case, list ALL violations
#   review-expected.sh --help    usage + the checks-file format
#
# Rule A: per-case assertions from e2e/expected/<case>/checks.grep (authored from the
# case's INTENT before regenerating - see --help). Rule B: global rules over
# every case's report files (never-pin patterns, exit-code contract,
# cross-format count agreement, required files).
#
# Invoked automatically by run.sh (full runs and --regenerate) and push.sh;
# standalone via `mise run e2e:review`. One line per violation
# ("FAIL <case>: <what>"), summary count at the end, exit 1 on any.
# BSD+GNU portable (macOS dev machines and Linux CI): grep/sed/awk only,
# no in-place sed, no GNU-only flags - same rules as normalize.sh.
set -uo pipefail

usage() {
  cat <<'EOF'
usage: review-expected.sh [--help]

Validates e2e/expected/ (the pinned argocdf reports) and exits 1 listing
every violation as "FAIL <case>: <what>".

Rule A - per-case checks files: e2e/expected/<case>/checks.grep, one
directive per line, applied to expected/<case>/reports/unified.diff:

  # comment          ignored, as are blank lines
  must:<ERE>         pattern must match the case's reports/unified.diff
  must-not:<ERE>     pattern must NOT match it
  must-log:<ERE>     pattern must match the case's fresh-run LOG - checked by
                     run.sh at run time (this gate validates the pinned tree
                     offline, and a log is not a pin), for facts a report
                     cannot carry: "this linter was actually invoked"
  must-not-log:<ERE> pattern must NOT appear in that log, for the absence half:
                     a line DEMOTED out of the default stream is otherwise only
                     observed, never falsifiable
  expect:affected=N changed=M [resources=+a,-r,~m] [errors=E]
                     the report's summary block must say EXACTLY this

A case may also have expected/<case>/same-report-as naming another case: every
report file of the two must then be byte-identical after each case's own name
and each linter's identity are normalized out. That is what makes "these two
cases differ only in the flag that selects the implementation" a checked claim
instead of a review-time one - the identity is normalized because it NAMES that
flag (lint#1 for a --lint command, lint-kyverno#1 for the built-in), so it is
the one token expected to differ; the findings either side of it are not.

must:/must-not: assert what is present or absent; expect: is what makes a
report EXHAUSTIVE. Without it an extra application section or an extra
changed resource is simply pinned: the byte comparison cannot object
(--regenerate rewrote the pin) and a brand-new case has no baseline diff in
which the extra would stand out. Exactly one expect: line per case, and the
optional fields are symmetric - naming resources= when the report has no
Resources line fails, and omitting it when the report has one fails too.

Write the must: line for the exact artifact the case exists to prove -
from the case's intent, BEFORE regenerating, never from generated output
(a check derived from output would happily pin a hollow report). Every
case needs a checks.grep with at least one must: line.

A case directory holds both halves of its verification, and the split is
load-bearing: checks.grep is AUTHORED from the case's intent and must
survive regeneration, while reports/ is written wholly by argocdf and is
wiped and rewritten by --regenerate. That is why regeneration scopes its
rm to reports/ and never to the case directory.

Rule B - global rules, every case, all report formats:
  - never pinned: lint-adapter failure lines, in BOTH shapes - the shell one
    (lint "<cmd>": exit status N) and the built-in one (lint-kyverno <dir>:
    exit status N / timeout after / with no report output / unparsable report /
    no resolved kube context); 'jq: error', normalizer placeholders
    <worktree>/<tempfile>, 'panic:'
  - meta.yaml exit code: 2 iff unified.diff says applications changed > 0,
    0 iff it says 0 changed; exit 1 (fatal) is never a valid pin
  - affected/changed counts must agree across unified.diff, md-fields.md
    and md-unified.md
  - all five reports/ files present: unified.diff, md-fields.md,
    md-unified.md, html-side-by-side.html, meta.yaml
EOF
}

case "${1:-}" in
  -h|--help) usage; exit 0 ;;
  '') ;;
  *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
esac

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR/../../e2e" || exit 1

REPORTS=(unified.diff md-fields.md md-unified.md html-side-by-side.html)

# The marker argocdf leaves in a report for a spec ArgoCD REFUSES, and the literal
# the invalid-spec rule below greps for. It must stay in step with the error
# cluster.invalidSpec builds - a reword there would hollow that rule - which
# TestReviewGateMarksTheInvalidSpecError enforces by reading THIS assignment and
# asserting the error still contains it. One assignment, so that test has one
# thing to find.
INVALID_SPEC_MARKER='invalid Application spec'
violations=0
fail() { printf 'FAIL %s: %s\n' "$1" "$2"; violations=$((violations+1)); }

[ -d expected ] || { echo "e2e/expected/ not found (init the e2e submodule first)"; exit 1; }

branch_exists() {
  git rev-parse --verify -q "case/$1" >/dev/null 2>&1 \
    || git rev-parse --verify -q "origin/case/$1" >/dev/null 2>&1
}

# Never-pin pattern over every report format of one case; one violation per
# (case, rule) listing the offending files. Patterns accept both the plain
# and the HTML-escaped spelling (md/html escape quotes and angle brackets).
ban() { # ban <case> <dir> <ERE> <label>
  hits=""
  for f in "${REPORTS[@]}"; do
    [ -f "$2$f" ] && grep -Eq -- "$3" "$2$f" && hits="$hits $f"
  done
  [ -z "$hits" ] || fail "$1" "$4 in:$hits"
}

# ---- Rule A: per-case checks files --------------------------------------
for dir in expected/*/; do
  [ -d "$dir" ] || continue
  name=$(basename "$dir")
  checks="${dir}checks.grep"
  ud="${dir}reports/unified.diff"
  if [ ! -f "$checks" ]; then
    fail "$name" "no checks file - write e2e/expected/$name/checks.grep with at least one must: line for the artifact the case proves"
    continue
  fi
  musts=0
  expects=0
  lineno=0
  while IFS= read -r line || [ -n "$line" ]; do
    lineno=$((lineno+1))
    case "$line" in
      ''|'#'*) ;;
      expect:*)
        expects=$((expects+1))
        # Rebuild the summary block this directive describes and compare it to
        # the pinned one as a whole, so a field the directive omits has to be
        # absent from the report as well - that symmetry is the exhaustiveness.
        want=""
        bad_field=""
        for field in ${line#expect:}; do
          case "$field" in
            affected=*) want="$want# Applications affected: ${field#affected=}
" ;;
            changed=*) want="$want# Applications changed: ${field#changed=}
" ;;
            resources=*)
              # Shape-checked here because it is the one composite field; a
              # malformed count in any field still fails, just as a summary
              # mismatch rather than by name.
              spec=${field#resources=}
              case "$spec" in
                +[0-9]*,-[0-9]*,~[0-9]*) : ;;
                *) bad_field="$field" ;;
              esac
              added=${spec%%,*}; rest=${spec#*,}
              removed=${rest%%,*}; modified=${rest#*,}
              want="$want# Resources: ${added} added, ${removed} removed, ${modified} modified
" ;;
            errors=*) want="$want# Errors: ${field#errors=}
" ;;
            *) bad_field="$field" ;;
          esac
        done
        if [ -n "$bad_field" ]; then
          fail "$name" "$checks:$lineno: unparsable expect: field '$bad_field' (want affected=N changed=M [resources=+a,-r,~m] [errors=E])"
        elif [ -f "$ud" ]; then
          # The summary block is the tail of the report, from "# Summary" on.
          got=$(sed -n '/^# Summary$/,$p' "$ud" | grep -E '^# (Applications|Resources|Errors)')
          if [ "$got" != "$(printf '%s' "$want")" ]; then
            fail "$name" "expect: summary mismatch
        authored: $(printf '%s' "$want" | tr '\n' '|')
        pinned:   $(printf '%s' "$got" | tr '\n' '|')"
          fi
        fi
        ;;
      must:*)
        musts=$((musts+1))
        pat=${line#must:}
        # A missing unified.diff is reported once by rule B, not per pattern.
        if [ -f "$ud" ] && ! grep -Eq -- "$pat" "$ud"; then
          fail "$name" "must: pattern not found in unified.diff: $pat"
        fi
        ;;
      must-not:*)
        pat=${line#must-not:}
        if [ -f "$ud" ] && grep -Eq -- "$pat" "$ud"; then
          fail "$name" "must-not: pattern present in unified.diff: $pat"
        fi
        ;;
      must-log:*|must-not-log:*)
        # Runtime assertions against the fresh run's log; enforced by run.sh,
        # not here - the gate validates the pinned tree offline and has no log.
        ;;
      *)
        fail "$name" "$checks:$lineno: unrecognized directive (want must:<ERE>, must-not:<ERE>, must-log:<ERE>, must-not-log:<ERE>, expect:<summary> or # comment): $line"
        ;;
    esac
  done < "$checks"
  [ "$musts" -gt 0 ] || fail "$name" "$checks has no must: line - pin at least the artifact the case exists to prove"
  case "$expects" in
    1) ;;
    0) fail "$name" "$checks has no expect: line - authorize the summary block (expect:affected=N changed=M [resources=+a,-r,~m] [errors=E]) so an EXTRA application or resource fails instead of being pinned" ;;
    *) fail "$name" "$checks has $expects expect: lines - exactly one summary can be authorized" ;;
  esac
done

# There is no stale-checks guard any more, and none is needed: a checks file used
# to live in its own tree where it could outlive its case, but it now sits INSIDE
# expected/<case>/, so "the case is gone" and "the checks file is gone" are the
# same event. run.sh still fails a case directory with no branch behind it.

# ---- Cross-case equivalence: expected/<case>/same-report-as ---------------
# The built-in lint adapters are pinned as EQUIVALENT to the shell adapters by
# carrying identical fixtures, so their reports must differ only where the case
# NAME appears (titles, headers, footers). Checked mechanically: normalize each
# case's own name out of both trees and require byte equality, so a divergence
# - one adapter losing a finding, a format drifting - fails here instead of
# surviving as two independently-plausible pins.
for dir in expected/*/; do
  [ -d "$dir" ] || continue
  name=$(basename "$dir")
  peer_file="${dir}same-report-as"
  [ -f "$peer_file" ] || continue
  while IFS= read -r peer || [ -n "$peer" ]; do
    case "$peer" in ''|\#*) continue ;; esac
    if [ ! -d "expected/$peer/reports" ]; then
      fail "$name" "same-report-as names '$peer', which has no expected/$peer/reports"
      continue
    fi
    for f in "${REPORTS[@]}" meta.yaml; do
      a="${dir}reports/$f"
      b="expected/$peer/reports/$f"
      [ -f "$a" ] || [ -f "$b" ] || continue
      if [ ! -f "$a" ] || [ ! -f "$b" ]; then
        fail "$name" "same-report-as $peer: $f exists on one side only"
        continue
      fi
      # The linter IDENTITY is normalized alongside the case name, because the two
      # cases legitimately differ there: a shell adapter is lint#N and a built-in
      # is lint-<tool>#N, and those really are different flags. The claim being
      # checked is that the FINDINGS and the report structure are the same, not
      # that the flags were. Everything else stays a byte comparison, so an adapter
      # losing a finding or a format drifting still fails here.
      if ! diff -q <(sed -E "s/$name/CASE/g; s/\[lint(-[a-z]+)?#[0-9]+/[LINTER/g" "$a") \
                   <(sed -E "s/$peer/CASE/g; s/\[lint(-[a-z]+)?#[0-9]+/[LINTER/g" "$b") >/dev/null 2>&1; then
        fail "$name" "same-report-as $peer: $f differs beyond the case name and linter identity (the equivalence claim broke)"
      fi
    done
  done < "$peer_file"
done

# ---- Rule B: global rules over every case --------------------------------
reviewed=0
for dir in expected/*/; do
  [ -d "$dir" ] || continue
  name=$(basename "$dir")
  reviewed=$((reviewed+1))

  rep="${dir}reports/"

  # All five recorded files must exist.
  for f in "${REPORTS[@]}" meta.yaml; do
    [ -f "$rep$f" ] || fail "$name" "missing expected/$name/reports/$f"
  done

  # Never-pin patterns. A lint adapter that CRASHED must never become a stored
  # expectation - a missing binary, a broken policy, a timeout or a changed report
  # format would otherwise be pinned as though it were the case's point.
  #
  # The scope is the shape argocdf gives lines it authors ITSELF about a linter:
  # the bracketed identity, then the failure. That prefix is what keeps a
  # legitimately pinned RENDER error mentioning an exit status (error-invalid-yaml)
  # passing, and it is why these patterns must be kept in step with
  # TestHealthLineShapes in internal/lint - a reformat there hollows the ban here.
  #
  # That "must" is ENFORCED, not just asked for: TestReviewGateBansEveryFailureShape
  # reads this list out of this file and requires every failure shape in that table
  # to match one of these patterns - and the one non-failure shape (the skip note,
  # which case/lint-policy-added pins) to match none. Adding a health line without a
  # ban here fails that test. It exists because the unusable-policy-directory line
  # shipped with no ban and review caught it, not the suite.
  # No trailing colon on 'exit status N': a command that dies without stderr
  # produces the bare form.
  ban "$name" "$rep" 'jq: error' "pinned 'jq: error' (lint adapter crash)"
  ban "$name" "$rep" '\[lint(-[a-z]+)?#[0-9]+\] (exit status [0-9]+|timeout after|signal:)' \
    "pinned lint-adapter failure line (exit status/timeout/signal on a lint warning)"
  ban "$name" "$rep" 'with no report output' "pinned built-in lint adapter failure ('with no report output')"

  ban "$name" "$rep" 'unparsable report' "pinned built-in lint adapter failure ('unparsable report' - the tool's output format changed)"

  # A spec ArgoCD REFUSES must never be pinned by accident. argocdf reports such
  # an application as an error instead of a diff, so an unrealistic fixture - one
  # no ArgoCD cluster would ever render - leaves $INVALID_SPEC_MARKER in the report
  # where it used to leave a plausible clean diff. That silence is how
  # case/oci-artifact-spelling stayed green for days against a spec the controller
  # was refusing the whole time.
  #
  # Not a plain ban, because a case ABOUT spec validation legitimately pins it: the
  # marker is allowed exactly where checks.grep DECLARES it with a must: line. So a
  # deliberate case says so out loud and every accidental one fails, which is why
  # this needs no per-case allowlist.
  #
  # That must: line has to carry the marker LITERALLY, which is the point rather
  # than a limitation: a broad pattern an unrelated case already has (must:# Error:)
  # would otherwise authorize an invalid-spec pin it says nothing about. Declaring
  # means naming.
  #
  # unified.diff is enough: --regenerate writes every format from ONE run, so the
  # marker cannot appear in one format only, and the cross-format count rule below
  # ties them together anyway.
  if [ -f "$rep"unified.diff ] && grep -q "$INVALID_SPEC_MARKER" "$rep"unified.diff; then
    if ! grep -Eq "^must:.*$INVALID_SPEC_MARKER" "${dir}checks.grep" 2>/dev/null; then
      fail "$name" "pins '$INVALID_SPEC_MARKER' (a spec ArgoCD REFUSES) with no must: line naming that literal - fix the fixture (every source needs a repoURL and either a path or a chart), or, if the case is ABOUT an invalid spec, declare it with must:<pattern containing '$INVALID_SPEC_MARKER'>"
    fi
  fi
  ban "$name" "$rep" 'no resolved kube context' "pinned built-in kyverno refusal (no resolved kube context)"
  ban "$name" "$rep" 'unusable policy directory' \
    "pinned built-in lint adapter failure ('unusable policy directory' - the path is not a readable directory)"
  ban "$name" "$rep" '(<|&lt;)worktree(>|&gt;)' "normalizer placeholder <worktree> pinned (a report leaked a temp path)"
  ban "$name" "$rep" '(<|&lt;)tempfile(>|&gt;)' "normalizer placeholder <tempfile> pinned (a report leaked a temp path)"
  ban "$name" "$rep" 'panic:' "'panic:' pinned (crash output is never an expectation)"

  # Counts from the unified.diff summary block.
  ud="${rep}unified.diff"
  affected="" changed=""
  if [ -f "$ud" ]; then
    affected=$(sed -n -E 's/^# Applications affected: ([0-9]+)$/\1/p' "$ud" | head -1)
    changed=$(sed -n -E 's/^# Applications changed: ([0-9]+)$/\1/p' "$ud" | head -1)
    if [ -z "$affected" ] || [ -z "$changed" ]; then
      fail "$name" "unified.diff has no parsable '# Applications affected/changed' summary lines"
      affected="" changed=""
    fi
  fi

  # Exit-code contract: 2 iff changed > 0, 0 iff changed == 0. Exit 1 (fatal)
  # is never a valid pin - render errors are report content, not process
  # failures.
  meta="${rep}meta.yaml"
  if [ -f "$meta" ]; then
    rc=$(awk '/^exit:/{print $2}' "$meta")
    if [ -z "$rc" ]; then
      fail "$name" "meta.yaml has no 'exit:' line"
    elif [ "$rc" = "1" ]; then
      fail "$name" "meta.yaml pins exit 1 (fatal) - a pin must be 0 or 2"
    elif [ -n "$changed" ]; then
      if [ "$changed" -gt 0 ] && [ "$rc" != "2" ]; then
        fail "$name" "exit-code contract: $changed application(s) changed but meta.yaml pins exit $rc (want 2)"
      elif [ "$changed" -eq 0 ] && [ "$rc" != "0" ]; then
        fail "$name" "exit-code contract: 0 applications changed but meta.yaml pins exit $rc (want 0)"
      fi
    fi
  fi

  # Cross-format agreement: the markdown summary lines must carry the same
  # affected/changed counts as unified.diff.
  if [ -n "$affected" ]; then
    for f in md-fields.md md-unified.md; do
      [ -f "$rep$f" ] || continue
      counts=$(sed -n -E 's/^\*\*Summary:\*\* ([0-9]+) applications affected \| ([0-9]+) changed.*/\1 \2/p' "$rep$f" | head -1)
      if [ -z "$counts" ]; then
        fail "$name" "$f has no parsable '**Summary:** N applications affected | M changed' line"
      elif [ "$counts" != "$affected $changed" ]; then
        fail "$name" "cross-format counts disagree: unified.diff says $affected affected/$changed changed, $f says ${counts% *} affected/${counts#* } changed"
      fi
    done
  fi
done

echo
if [ "$violations" -gt 0 ]; then
  echo "$violations review violation(s) in e2e/expected/ (see FAIL lines above)"
  exit 1
fi
echo "review OK: $reviewed case(s), all checks pass"
