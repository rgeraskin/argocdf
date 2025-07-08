# Changelog

All notable changes to this project will be documented in this file.

## 0.4.0

- Add `--lint` (repeatable; env `ARGOCDF_LINT` for a single command) and `--lint-timeout` (default `5s`): pipe each affected app's rendered manifests through user-supplied shell commands and report their stdout as warnings. Each side (base and target) is linted separately — the command runs with that side's worktree as its working directory, so repo-relative policy paths resolve to the branch's own files — and every non-empty stdout line becomes a `[base]`/`[target]`-labeled warning in all output formats, alongside the existing parse warnings — a `[base]`-only finding was fixed by the change under review, `[target]`-only was introduced, both sides = pre-existing. Any policy tool plugs in through a jq-style adapter (kyverno, conftest, kubeconform, ...); argocdf never parses tool-specific output. A command failure (spawn error, timeout, or exit ≠ 0) is reported as a non-fatal warning line — stdout lines received before the failure are kept

## 0.3.0

- Add `split[=N]` file-output option for `md-fields`/`md-unified` (e.g. `-f md-unified,split:pr-comment.md`): a report larger than N bytes (default 60000, safely under GitHub's 65,536-char comment cap) is written as multiple self-contained part files (`pr-comment.md`, `pr-comment.2.md`, ...) instead of one oversized file. Each part carries the upsert marker and a `part i/N` heading with balanced `<details>` blocks and code fences, so CI can post every part as its own PR comment. An app's report stays within a single part; only an app that alone exceeds the limit is split at resource boundaries, and only a single resource diff larger than a whole part is truncated (with a note). Stale part files from a previous, larger run are removed automatically.
- Add `--helm-add-repos` (env `ARGOCDF_HELM_ADD_REPOS`): make chart dependency HTTP(S) repositories resolvable before `helm dependency build`, deduplicated per run. A URL already registered under any name is only refreshed (`helm repo update <name>`) — no new repositories.yaml entry; unknown URLs are added under a collision-proof `argocdf-dep-<hash>` name. Note it mutates local helm state either way (index caches, and repositories.yaml for unknown URLs), hence off by default; intended for ephemeral CI runners where the helm repo cache is empty
- When `helm dependency build` fails because a dependency repository is not registered ("no cached repository" / "no repository definition"), the error now includes an actionable hint listing the repos to `helm repo add` and pointing at `--helm-add-repos`
- Update the GitHub Actions examples: both workflows render with `split` and post each part as its own comment, and pass `--helm-add-repos` for fresh-runner chart dependencies; `wf-extra.yaml` drops its oversized-comment truncation step

## 0.2.3

- Fix: skip base-branch render for child apps discovered only on the target branch (apps-of-apps). Previously they were rendered against the base worktree with the target spec, which failed hard when the spec referenced files absent on base (e.g. a newly added values file in a pre-existing chart directory). New child apps also now correctly report all their resources as added instead of "No changes".

## 0.2.2

- Make the `argocdf` name in the report footer a link to the project repo (HTML and Markdown outputs)

## 0.2.1

- Fix: when no applications are affected, still write a complete report to file outputs (markdown/HTML/unified) instead of leaving a 0-byte file — preserves the markdown upsert marker so CI can overwrite a stale PR comment. Terminal output stays quiet (the run already logs the empty result).

## 0.2.0

- Upgrade Argo CD dependency from v2.14.21 to v3.3.11 (module path `argo-cd/v3`, k8s.io libs to 0.34.0)
- Add `--verbose`/`-v` flag; log resolved repo URL, cluster context, and namespace
- Allow every flag to be set via `ARGOCDF_*` environment variables (flag > env > default precedence)
- Fix multi-source apps: route every source through `Factory.GetRenderer` so path sources with a `Chart.yaml` are detected as Helm instead of falling back to plain-YAML concatenation
- Add test pinning the wave-barrier invariant of apps-of-apps processing (parallel renders within a wave, single-threaded discovery/requeue between waves)
- Install Helm & Kustomize via mise in CI (pinned versions) instead of relying on runner-preinstalled tools; run tests verbosely
- Refresh DIFFERENCES.md to match current implementation

## 0.1.0

- Initial release