# argocdf in GitHub Actions

Two ready-to-adapt workflows live in this directory. Both install tooling with [mise](https://mise.jdx.dev), render the diff of every affected ArgoCD Application, and post it as a PR comment. `wf-extra.yaml` additionally collapses the previous argocdf comment as *outdated* so only the latest is expanded.

| Workflow                           | Trigger                            | Reach for it when                                                                                                     |
|------------------------------------|------------------------------------|-----------------------------------------------------------------------------------------------------------------------|
| [`wf-simple.yaml`](wf-simple.yaml) | `pull_request` (every push)        | You want the diff posted automatically on each push, with the smallest workflow that still works.                     |
| [`wf-extra.yaml`](wf-extra.yaml)   | comment `argocdf` / `argocdf plan` | You want on-demand runs, an audit label, a full-report artifact, oversized plans posted across several comments, and fork-PR hardening. |

## Simple: run on every PR

[`wf-simple.yaml`](wf-simple.yaml) triggers on `pull_request`, so every push to a PR renders the diff and posts it back as a comment. It is `wf-extra.yaml` with the extra machinery stripped out — no fork-PR guard, no comment ack/label, no report artifact — leaving just the render and post.

Both workflows render with the `split` file-output option and post each resulting part file as its own PR comment, so every comment stays under GitHub's 65,536-char cap and the whole plan is always on the PR. A report that fits produces a single `pr-comment.md` and a single comment.

Since the simple workflow runs on every push and never collapses older comments, busy PRs with multi-part plans accumulate noise. If that becomes a problem, copy the "Collapse previous argocdf comments" step from `wf-extra.yaml` (or switch to the comment-triggered workflow entirely).

## Comment-triggered `argocdf plan`

[`wf-extra.yaml`](wf-extra.yaml) is an Atlantis-style GitHub Actions workflow: comment **`argocdf`** or **`argocdf plan`** on a pull request and CI renders the diff and posts it back. On top of the simple flow it adds:

- **On-demand runs** — a `plan` costs a cluster round-trip and Helm/Kustomize renders, so triggering it by comment (not on every push) keeps noise and cost down. Also runnable via `workflow_dispatch`.
- **An audit trail** — every PR it runs on gets an `argocdf` label, giving you a searchable index of where the tool has been exercised, plus a 👀 reaction as instant "run started" feedback.
- **Robust comments** — the full report is uploaded as an artifact, and an oversized plan is posted in full across several comments: argocdf's `split` option emits self-contained part files (whole app sections, balanced `<details>`/fences, `part i/N` headings), and the post step publishes each as its own comment. The collapse step minimizes every part of an outdated plan, since each carries the argocdf marker.
- **Fork-PR hardening** — see below.

### The fork-PR guard is load-bearing — keep it

Because this workflow is comment-triggered (`issue_comment`) yet checks out and executes PR-authored code (`helm dependency build`, `argocdf` render) while holding privileged secrets, it is exactly the shape of a "pwn request": a fork PR could otherwise run its own code with your cluster creds and PAT. The **Refuse fork PRs and pin head SHA** step is the mitigation and runs *before* checkout:

- It refuses any PR whose head repo differs from the base repo (fork), so untrusted code never executes with these secrets. The `author-association` gate on the job only checks who *commented*, not whose *code* runs.
- It pins checkout to the immutable head commit SHA (not the mutable `refs/pull/<n>/head`), closing the check-then-checkout TOCTOU window.

Paired with `persist-credentials: false` on checkout (so no token is written into `.git/config` for PR code to read), this keeps the privileged job safe. Don't drop these when adapting `wf-extra.yaml`. `wf-simple.yaml` omits them deliberately: a plain `pull_request` trigger already runs PR code in an unprivileged context, so the pwn-request concern doesn't apply the same way — but do not switch it to `pull_request_target` without adding the guard back.

## Adapting either workflow

Both are stripped of org-specific values. Search for placeholders and replace them.

The **Login to GHCR for helm** step is only needed if your Helm charts pull dependencies from a *private* OCI registry; delete it for public chart deps.

### Why `--base origin/main` + `fetch-depth: 0`

If the checked-out local `main` is even one commit behind `origin/main`, an upstream commit that landed after your PR branch was cut (for example an image bump) will incorrectly show up as part of your PR's diff. `fetch-depth: 0` fetches full history so both `origin/main` and the merge-base commit are available locally; diffing against `origin/main` then avoids the false diff. `argocdf` also warns and falls back to `origin/<base>` on its own when it detects a stale local base.

### Config via `ARGOCDF_*` env, not flags

Every argocdf flag is also settable as `ARGOCDF_<FLAG>` (uppercased, dashes → underscores). Putting the stable ones in the environment — rather than the workflow's `run:` line — lets a local `argocdf` invocation behave identically to CI. With mise that looks like:

```toml
# .mise.toml
[tools]
"github:rgeraskin/argocdf" = "0.3.0"   # pin the version CI installs

[env]
ARGOCDF_REPO_URL = "https://github.com/<your-org>/<your-repo>/"
ARGOCDF_KUSTOMIZE_ENABLE_HELM = "true"
ARGOCDF_KUSTOMIZE_LOAD_RESTRICTOR = "LoadRestrictionsNone"
```

The workflow's `run:` line then only carries the per-run bits (`--base`, `--target`, `--file`, `--concurrency`).
