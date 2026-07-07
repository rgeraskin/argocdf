# Changelog

All notable changes to this project will be documented in this file.

## 0.3.0

- Add `split[=N]` file-output option for `md-fields`/`md-unified` (e.g. `-f md-unified,split:pr-comment.md`): a report larger than N bytes (default 60000, safely under GitHub's 65,536-char comment cap) is written as multiple self-contained part files (`pr-comment.md`, `pr-comment.2.md`, ...) instead of one oversized file. Each part carries the upsert marker and a `part i/N` heading with balanced `<details>` blocks and code fences, so CI can post every part as its own PR comment. An app's report stays within a single part; only an app that alone exceeds the limit is split at resource boundaries, and only a single resource diff larger than a whole part is truncated (with a note). Stale part files from a previous, larger run are removed automatically.
- Update the GitHub Actions examples: both workflows render with `split` and post each part as its own comment; `wf-extra.yaml` drops its oversized-comment truncation step

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