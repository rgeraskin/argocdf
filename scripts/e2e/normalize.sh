#!/usr/bin/env bash
# Normalize an argocdf report for byte comparison: strip the timestamped
# provenance footers (they embed time.Now() and, after rebases, changing
# commit SHAs). Everything else in a report is deterministic: app sections
# are sorted by argocdf itself and the title is stable (master -> case/NAME).
# Error messages embed per-run temp paths (ephemeral worktrees, inline-values
# temp files); those are replaced with stable placeholders so error-path
# cases stay byte-comparable.
# No in-place sed: BSD and GNU sed disagree on -i syntax, and Linux CI must
# run this too.
# Usage: normalize.sh <file>   (in place)
set -euo pipefail

f="$1"
tmp="$(mktemp)"
grep -v \
  -e '^# Generated at .* by argocdf' \
  -e '^_Generated at .* by \[argocdf\]' \
  -e '<p class="timestamp">Generated at .*</p>' \
  "$f" \
  | sed -E \
      -e 's#[^ `"]*/argocdf-worktree-[0-9]+#<worktree>#g' \
      -e 's#[^ `"]*/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}#<tempfile>#g' \
      > "$tmp" || true
command mv -f "$tmp" "$f"
