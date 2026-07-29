#!/usr/bin/env bash
# Publish the e2e artifact: rebase every case/* branch onto master, verify the
# one-commit-off-master invariant, then push master + force-push all branches, and
# prune remote case/* branches that no longer exist locally (phantom cases).
#
# Force-push is inherent to the model: any master commit (fixture change,
# expectation regen) rewrites every case branch via rebase, so remote case
# branches are always replaced wholesale. Master itself is never force-pushed.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR/../../e2e"

[ -z "$(git status --porcelain)" ] || { echo "ERROR: e2e working tree not clean"; exit 1; }
[ "$(git branch --show-current)" = "master" ] || { echo "ERROR: not on master"; exit 1; }

# Never publish expectations that fail the hollow-pin review gate (per-case
# checks files + global rules - see review-expected.sh --help).
"$SCRIPT_DIR/review-expected.sh" || { echo "ERROR: expected/ fails review - fix the violations above before publishing"; exit 1; }

branches=$(git branch --list "case/*" | tr -d ' ')
[ -n "$branches" ] || { echo "ERROR: no case/* branches"; exit 1; }

# Remote case/* branches with no local counterpart are PHANTOM CASES: run.sh
# discovers cases from refs/remotes/origin/case/ as well as local heads, so a
# case that was renamed or deleted keeps reappearing - with no expectations and
# no checks file - and fails forever for everyone who fetches. Publishing is the
# only moment that can fix it, so prune them here.
orphans=$(git ls-remote --heads origin 'refs/heads/case/*' \
  | sed 's#.*refs/heads/##' \
  | while IFS= read -r remote_branch; do
      [ -n "$remote_branch" ] || continue
      git show-ref -q --verify "refs/heads/$remote_branch" || printf '%s\n' "$remote_branch"
    done)

# The rebases below rewrite every LOCAL case branch and the push replaces
# every REMOTE one wholesale - require explicit intent before touching
# anything. E2E_PUSH_YES=1 skips the prompt (and is required when stdin is
# not a terminal).
if [ "${E2E_PUSH_YES:-}" != "1" ]; then
  [ -t 0 ] || { echo "ERROR: refusing to rewrite + force-push all case branches non-interactively (set E2E_PUSH_YES=1)"; exit 1; }
  printf 'Rebase and force-push %s case branch(es) + master to origin' \
    "$(echo "$branches" | wc -w | tr -d ' ')"
  # Deletions are named in full: this is the only destructive-to-history part.
  [ -z "$orphans" ] || printf ',\nand DELETE %s orphaned remote branch(es): %s' \
    "$(echo "$orphans" | wc -w | tr -d ' ')" "$(echo "$orphans" | tr '\n' ' ')"
  printf '? [y/N] '
  read -r reply
  case "$reply" in
    y|Y|yes|YES) ;;
    *) echo "aborted"; exit 1 ;;
  esac
fi

for b in $branches; do
  git rebase -q master "$b" >/dev/null 2>&1 \
    || { git rebase --abort 2>/dev/null; echo "ERROR: rebase conflict on $b - resolve or recreate the branch first"; git checkout -q master; exit 1; }
done
git checkout -q master

tip=$(git rev-parse master)
for b in $branches; do
  [ "$(git rev-parse "$b~1")" = "$tip" ] && [ "$(git rev-list --count master.."$b")" = "1" ] \
    || { echo "ERROR: $b is not exactly one commit off master"; exit 1; }
done

git push origin master
# shellcheck disable=SC2086
git push --force origin $branches
echo "pushed master + $(echo "$branches" | wc -w | tr -d ' ') case branches"

# Prune last: a failed push must not leave the remote missing cases it still
# serves. Deleting the remote branch drops its remote-tracking ref too, which is
# what actually stops run.sh from rediscovering the phantom.
if [ -n "$orphans" ]; then
  # shellcheck disable=SC2086
  git push origin --delete $orphans
  echo "pruned $(echo "$orphans" | wc -w | tr -d ' ') orphaned remote case branch(es)"
fi
