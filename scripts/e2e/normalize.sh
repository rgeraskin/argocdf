#!/usr/bin/env bash
# Normalize an argocdf report for byte comparison: strip the timestamped
# provenance footers (they embed time.Now() and, after rebases, changing
# commit SHAs). Everything else in a report is deterministic: app sections
# are sorted by argocdf itself and the title is stable (master -> case/NAME).
# Error messages embed per-run temp paths (ephemeral worktrees, inline-values
# temp files); those are replaced with stable placeholders so error-path
# cases stay byte-comparable. Note the review gate REFUSES to pin those two
# placeholders: a temp path reaching a report is argocdf leaking internal state, to
# be fixed there rather than blessed here.
#
# TLS verification wording is different: it comes from Go's x509 via helm, and it
# differs by PLATFORM ("certificate is not standards compliant" on macOS, "signed by
# unknown authority" on Linux) for one and the same failure. Expectations are shared
# between machines, so the tail is collapsed to a stable marker. The prefix is kept,
# so a DIFFERENT failure still changes the report and breaks its pin.
#
# The match stops at `<` or `&` rather than running to end of line: in the HTML report
# the message shares its line with the markup that follows, and `.*` swallowed the
# error card's closing tags and the start of the summary section - excluding all of it
# from byte comparison for that format forever. Neither observed wording contains
# either character.
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
      -e 's#tls: failed to verify certificate: [^<&]*#tls: failed to verify certificate: <platform-specific>#g' \
      > "$tmp" || true
command mv -f "$tmp" "$f"
