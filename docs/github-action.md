# Running argocdf in GitHub Actions

This recipe runs `argocdf` on every pull request, renders the ArgoCD diff to a
markdown file, and upserts a single PR comment that is updated in place on each
push (instead of piling up new comments).

It relies on three CI-native features:

- `--exit-code` — exits `0` when there are no changes, `1` on error, and `2`
  when changes are present (same convention as `diff` and
  `terraform plan -detailed-exitcode`).
- `--marker` / the default comment marker — every markdown report starts with an
  invisible HTML comment (`<!-- argocdf-diff -->`) that the comment-upsert action
  matches on to find and update its own comment.
- `--base origin/main` — resolves the diff base against the remote-tracking
  branch. argocdf already prefers `origin/<base>` automatically when the local
  base is stale, but passing it explicitly is the most robust setting in CI.

## Why `origin/main` matters

If the checked-out local `main` is even one commit behind `origin/main`, an
upstream commit that landed after your PR branch was cut (for example an image
bump) will incorrectly show up as part of your PR's diff. Checking out with
`fetch-depth: 0` and diffing against `origin/main` avoids this. `argocdf` will
also warn and fall back to `origin/<base>` on its own when it detects a stale
local base.

## Example workflow

```yaml
name: argocd-diff

on:
  pull_request:

permissions:
  contents: read
  pull-requests: write   # required to create/update the PR comment

jobs:
  diff:
    runs-on: ubuntu-latest
    steps:
      - name: Checkout (full history so origin/main is available)
        uses: actions/checkout@v4
        with:
          fetch-depth: 0

      - name: Install Helm
        uses: azure/setup-helm@v4

      - name: Install Kustomize
        uses: imranismail/setup-kustomize@v2

      - name: Install argocdf
        run: |
          go install github.com/rgeraskin/argocdf/cmd/argocdf@latest
          echo "$(go env GOPATH)/bin" >> "$GITHUB_PATH"

      - name: Configure kubeconfig
        run: |
          mkdir -p "$HOME/.kube"
          echo "${{ secrets.KUBECONFIG }}" | base64 -d > "$HOME/.kube/config"
          chmod 600 "$HOME/.kube/config"

      - name: Run argocdf
        id: argocdf
        run: |
          set +e
          argocdf -q -f md-fields:diff.md --exit-code --base origin/main
          code=$?
          echo "exit_code=$code" >> "$GITHUB_OUTPUT"
          # 0 = no changes, 2 = changes present; both are success for the job.
          # Anything else is a real failure.
          if [ "$code" -ne 0 ] && [ "$code" -ne 2 ]; then
            exit "$code"
          fi

      - name: Find existing comment
        if: steps.argocdf.outputs.exit_code == '2'
        uses: peter-evans/find-comment@v3
        id: find
        with:
          issue-number: ${{ github.event.pull_request.number }}
          comment-author: github-actions[bot]
          body-includes: "<!-- argocdf-diff -->"

      - name: Create or update PR comment
        if: steps.argocdf.outputs.exit_code == '2'
        uses: peter-evans/create-or-update-comment@v4
        with:
          comment-id: ${{ steps.find.outputs.comment-id }}
          issue-number: ${{ github.event.pull_request.number }}
          body-path: diff.md
          edit-mode: replace
```

## Notes

- The marker (`<!-- argocdf-diff -->`) is emitted automatically as the first
  line of `diff.md`. `find-comment`'s `body-includes` matches on it, and
  `create-or-update-comment` replaces the matched comment so a single, always
  up-to-date comment is maintained.
- To run several independent diffs on one PR (for example per-cluster), give
  each its own marker with `--marker <id>` (e.g. `--marker prod`, which emits
  `<!-- argocdf-diff:prod -->`) and match each `find-comment` step on the
  corresponding marker string.
- The `--exit-code` value of `2` drives the comment steps here. If you would
  rather always post (even a "no changes" comment), drop the
  `exit_code == '2'` conditions and unconditionally upsert `diff.md`.
- Store the cluster kubeconfig in an encrypted repository/organization secret
  (`secrets.KUBECONFIG` above, base64-encoded). A read-only ServiceAccount token
  scoped to listing ArgoCD `Application` resources is sufficient.
