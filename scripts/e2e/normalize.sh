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
# The API-version COUNT inside an elided render error (`--helm-api-versions <309
# elided>`) is the SIZE of the cluster's advertised API set, which describes the
# CLUSTER rather than argocdf: the same failure quotes 309 against the cluster that
# motivated the eliding and 107 against this one. Pinning the number would make the
# expectation churn on every E2E_KIND_NODE_IMAGE bump for a reason the case says
# nothing about, so the digits collapse to <N> and the MARKER - what such a case
# actually pins - is kept: a regression that stops eliding brings the whole list
# back and still fails. Both bracket spellings are handled because markdown and
# HTML escape them.
#
# It is ALSO what the two bootstrap modes need, though bootstrap.sh now equalizes
# everything it cheaply can (ArgoCD's applicationset/appproject CRDs and the widget
# CRD the controller-backed mode gets by syncing a fixture): metrics-server's
# AGGREGATED metrics.k8s.io/v1beta1 stays baseline-only, 3 entries out of 107. See
# bootstrap.sh's header for why that one cannot be applied as a manifest.
#
# The (helm-)? alternation deliberately covers a spelling that should never REACH a
# report - argocdf's helm-side eliding is log-records-only, because ArgoCD strips
# that list from the error it RETURNS - so do not read it as dead and delete it. It
# is what makes this rule degrade gracefully if that upstream strip ever stops: the
# flood would arrive already elided and get its count collapsed here, instead of
# pinning a mode-dependent number. And it cannot hollow the tripwire that would
# catch such a change, because case/helm-schema-fail pins upstream's OWN
# `<api versions removed>` marker, which carries no count for this rule to touch and
# passes through verbatim - so that case still fails on the text that changed.
#
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
      -e 's#(--(helm-)?api-versions) (<|&lt;)[0-9]+ elided(>|&gt;)#\1 \3N elided\4#g' \
      > "$tmp" || true
command mv -f "$tmp" "$f"
