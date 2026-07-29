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
  expect:affected=N changed=M [resources=+a,-r,~m] [errors=E]
                     the report's summary block must say EXACTLY this

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
      *)
        fail "$name" "$checks:$lineno: unrecognized directive (want must:<ERE>, must-not:<ERE>, expect:<summary> or # comment): $line"
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

  # Never-pin patterns. The lint rule is scoped to the runner's warning-line
  # shape (lint "<cmd>": exit status N) so legitimately pinned RENDER errors
  # that mention an exit status (error-invalid-yaml) still pass — the scope
  # comes from the lint "<cmd>": prefix. No trailing colon: a lint command
  # that dies without stderr produces the bare 'exit status N' form.
  ban "$name" "$rep" 'jq: error' "pinned 'jq: error' (lint adapter crash)"
  ban "$name" "$rep" 'lint ("|&#34;)[^"]*("|&#34;): exit status [0-9]+' "pinned lint-adapter failure line ('exit status N' on a lint warning)"
  # The BUILT-IN adapters (--lint-kyverno/--lint-conftest) have their own warning
  # vocabulary, prefixed with the flag name and the policy dir instead of a quoted
  # command. Without these the gate would happily pin a missing kyverno binary, a
  # broken policy, a timeout or a changed report format - the same hollow pins the
  # shell rule above exists to stop.
  ban "$name" "$rep" 'lint-(kyverno|conftest) [^:]*: (exit status [0-9]+|timeout after|signal:)' \
    "pinned built-in lint adapter failure (exit status/timeout on a --lint-kyverno/--lint-conftest warning)"
  ban "$name" "$rep" 'with no report output' "pinned built-in lint adapter failure ('with no report output')"
  ban "$name" "$rep" 'unparsable report' "pinned built-in lint adapter failure ('unparsable report' - the tool's output format changed)"
  ban "$name" "$rep" 'no resolved kube context' "pinned built-in kyverno refusal (no resolved kube context)"
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
