# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

> MAJOR is still 0, so the public API is not stable ([spec item 4](https://semver.org/spec/v2.0.0.html#spec-item-4)): MINOR carries everything that is not purely a fix — BREAKING changes INCLUDED, which is why there is no 1.x-style major bump for them — and PATCH is fixes and docs only. Every breaking change is titled `BREAKING:` — whether it breaks an invocation or the format of a report something else parses — so `grep BREAKING` over this file lists them all, and pin a MINOR version if you need stability.
>
> Release dates are the GitHub release's publication date (UTC).

## [Unreleased]

### Added

- `oci://` artifact sources render

  ArgoCD 3.1 made an OCI registry a source type in its own right: `repoURL: oci://ghcr.io/org/my-chart` with a tag or digest as `targetRevision`, no `chart:` field, and the pulled artifact itself is the application — `path` then selects a directory INSIDE it. argocdf now renders those, through ArgoCD's own OCI client (revision-to-digest resolution including semver constraints, ORAS pull, content-layer and media-type validation, size-bounded extraction), so an app on the artifact spelling appears in reports instead of failing. Registry credentials come from repository secrets of `type: oci` in `--repo-creds cluster`. Artifacts are pulled once per run and shared by both diff sides; a revision the registry can re-point (a floating tag, a constraint) bypasses the render cache, the same soundness rule pinned chart versions already get. `e2e/case/oci-artifact-bump` pins the spelling end to end, `e2e/case/private-oci-artifact` pins an AUTHENTICATED pull (credentials resolved from a `type: oci` repository Secret through ArgoCD's own code, fetched by ORAS rather than `helm pull`), and `e2e/case/plain-oci-artifact` pins a NON-HELM artifact, whose contents extract without the top-level directory a pushed chart has stripped and render as a directory source.

### Changed

- BREAKING: an application whose spec ArgoCD refuses is reported as an error, not as a diff

  ArgoCD validates an Application's sources before it ever asks the repo-server for manifests: every source needs a `repoURL` and either a `path` or a `chart`, a `chart` needs a `targetRevision`, and a source of a multi-source application may substitute a `ref`. A spec that fails those rules gets an `InvalidSpecError` condition and is never rendered. argocdf renders by calling ArgoCD's manifest generation directly, which sits BELOW that check, so it rendered such an application happily and usually reported "No changes" — the worst available answer, because the application does not exist in the cluster at all. It now refuses the same specs ArgoCD refuses, and the application gets an error line carrying ArgoCD's own message instead of a diff (so `errors` in the summary counts it, and every other application still diffs). Anything parsing reports for these applications sees an error where a clean diff used to be. The check runs per SIDE and before the render cache: the report names which side's spec is refused — a PR that BREAKS a spec fails on the target side after the base side rendered, one that FIXES a broken spec fails on the base side — and a manifest set cached while the spec was still valid can no longer answer for the invalid one. It is deliberately narrow — only the source rules, mirrored from ArgoCD's own function and pinned against it in BOTH directions, so the mirror cannot outlive a rule ArgoCD relaxes or one it adds. Project permissions and destination clusters are still not validated, and `--exit-code` is unchanged: it still maps changes only, so a CI job that needs to fail on an error must read the report.

- A chart pinned by a semver constraint reuses its download

  `targetRevision: ^1.2.0` re-downloaded the chart on every run, because a constraint resolves against the mutable registry index and so cannot be a cache key. Now that argocdf resolves it (see the `ARGOCD_APP_REVISION` fix below), the download is keyed by the version the constraint resolved TO: the mutable half of the question — which version does this constraint mean now — is asked of the registry every run, and only the immutable half comes from disk. A constraint whose maximum has moved therefore lands on a different key and pulls. This is worth having exactly where the render cache cannot help: a constraint revision bypasses that cache, so such an application re-renders every run regardless.

- The render cache no longer describes an external git source with local content

  A renderable source living in another git repository was keyed by the DIFFED commit's tree — a hash of the wrong repository, the same defect the OCI-artifact keying fixed, and unsound in one direction: the external revision can advance while nothing local changes, so an entry under a branch name could go stale. Such a source is now keyed by its repository URL, revision and path, and bypasses the cache unless the revision names fixed content (a commit SHA, or an exact version tag by the same convention pinned chart versions and OCI tags already use). Dropping the local tree hash also removes a spurious miss: an unrelated local commit no longer invalidates a render that reads nothing local. All previously cached entries are invalidated (`rendercache-v7`).

### Fixed

- A failed `kustomize build` says what actually broke, and stops naming a temp directory

  ArgoCD hands the cluster's API-version set to `kustomize build --enable-helm` exactly as it hands it to `helm template` — one `--helm-api-versions` pair per advertised group/version AND kind — and quotes the whole command line back in any failure. Unlike the helm path, it does not strip that list from the error it RETURNS, so an application whose kustomization fails to build reported a single 17,732-character line with the diagnosis in its last 290 characters: unreadable in a terminal, truncated in a PR comment, and the only explanation an `errors=N` summary offers. Runs of those pairs now collapse to `--helm-api-versions <309 elided>` in the returned error as well as in the log record, keeping the count because it is the one informative part of the list. The helm spelling is unchanged — ArgoCD strips it from the returned error itself, so argocdf still elides it only in log records. The same error also carried argocdf's ephemeral worktree directory three times, because `kustomize build` is invoked with an ABSOLUTE app path and kustomize names absolute paths in its own diagnoses, where helm runs with a working directory and is quoted as `helm template .`; the render root is now redacted to `.` exactly as ArgoCD redacts it in the kustomize command list it returns. `e2e/case/kustomize-build-fail` pins both halves — it is the suite's first kustomize error case, and its review gate refusing to pin a leaked temp path is what surfaced the second one.

- `ARGOCD_APP_REVISION` reports what the source actually rendered from

  ArgoCD resolves the revision it renders with per source — the commit for a git source, the resolved chart version for a remote chart, the artifact digest for an OCI source — and that value is what `$ARGOCD_APP_REVISION`, `_SHORT` and `_SHORT_8` expand to in helm values, parameters and file paths. argocdf passed the git commit for every source. For a remote chart or artifact that is both wrong and unstable: the commit is the one input guaranteed to differ between the two sides of a diff, so an application substituting the revision into an image tag or a label showed a spurious change on every PR while both sides pulled the same pinned chart. The revision is now resolved per source, with chart resolution delegated to ArgoCD's own `util/versions` (an exact version as declared, a semver constraint resolved against the tag list or repository index) and the resolved version pinning the pull as well, so the label and the pulled chart cannot disagree. `HEAD` and empty stay argocdf's "latest" — upstream's resolution rejects both — and the build env then reports the revision the application declared rather than failing the render.

- An external git source reports its own commit, not the diffed repository's

  The `ARGOCD_APP_REVISION` fix below reached remote charts and OCI artifacts; a renderable source living in ANOTHER git repository is the third kind and had the same defect. It renders from a clone of that repository at its own `targetRevision`, so the diffed repository's commit described unrelated content — and being the one input guaranteed to differ between the two sides of a diff, it made a cross-repo application report a change on every PR while both sides rendered identical external content. The clone's resolved commit is now what the render is labelled with. A commit that cannot be read back is not fatal: the content is there and only its name is unknown, so the label falls back to the diffed commit as before.

- A `chart:` source that is also a `ref:` is no longer cloned as a git repository

  Such a source had its chart-repository URL handed to `git clone`, which fails on a chart repository, taking the whole application's render down before its own source branch was reached. ArgoCD never meets this: it materializes a ref repository only when a `$ref/...` value file actually names one. argocdf now registers the ref target and skips the materialization for any source that is not a git repository, which reproduces both of ArgoCD's outcomes — an unreferenced ref renders, and a referenced one fails in manifest generation with `failed to find repo`, which is where ArgoCD refuses a chart ref explicitly too.

- Adding `oci://` to a Helm chart source is no longer reported as no change

  ArgoCD tests the `oci://` prefix BEFORE it looks at `chart:`, so writing `repoURL: oci://ghcr.io/org` next to `chart: my-chart` does not add a redundant scheme — it retypes the source, `chart:` stops being read, and ArgoCD resolves the tag against the repoURL itself, which usually fails. argocdf normalized the prefix away (its chart client trims `oci://` because `helm pull` re-adds it), so both spellings pulled the same chart and the report said "No changes" about a change that breaks the application in ArgoCD. The dispatch order is now upstream's in the renderer and in the render-cache key alike, so such a source renders — or fails — as ArgoCD does. Two smaller parity fixes came with it: a source that is both a `$ref` and an `oci://` artifact is no longer skipped as ref-only, and an `oci://` URL is never handed to `git clone`.

## [0.6.0] - 2026-08-12

### Changed

- Each lint invocation logs one line, saying how it ENDED

  A report cannot tell three outcomes apart: a linter that ran and found nothing, one skipped because that side has no policies, and one that died. Empty output at exit 0 is a legitimate no-findings result by contract, and a failed invocation contributes exactly one line — indistinguishable from a single finding. So a kyverno that timed out logged `lines=1` on its invocation and `findings=1` in the side's totals, and both read like a single policy violation while the side had gone entirely unchecked. Every invocation now logs `status=ok|skipped|failed` next to which linter ran, what it was pointed at, its line count and its duration, and `status=failed` logs at WARN because it means that side was NOT linted. The per-side aggregate drops to DEBUG — its count is just the sum of those lines, and the ambiguous half of the pair. Each linter also carries a 1-based ordinal in flag order — `lint#2` in the report, `kyverno#1` in the log's `linter=` field — which is the only thing that tells two repeated `--lint` commands apart once their command text is truncated. Both lines also name the application's `namespace`: with apps-in-any-namespace, `team-a/web` and `team-b/web` are different applications with the same name, so `app=web` alone is attributable to neither.

- BREAKING: every lint warning names the linter that produced it

  Every lint warning is relabelled, so anything grepping or post-processing PR comments for the old `[kyverno/policy]` labels needs updating. A report's warning list mixed three kinds of line, and only the wording told them apart. Every lint line now opens with the linter's identity — the flag that configured it plus its position among the flags of that kind — and whether that bracket CONTINUES past it says what you are reading: `[lint-kyverno#1/disallow-latest-tag] Deployment/web: …` is a finding and the text after it is the tool's, `[lint-kyverno#1] timeout after 10s` is argocdf reporting on the linter itself, and no bracket at all is not lint but a YAML parse warning — which is exactly what the unbracketed form was ambiguous with. One identity per surface: a report always names the flag (so `grep lint-kyverno#1` finds everything that linter produced), the log drops the prefix its field name repeats. The ordinal is what keeps two directories of the same tool apart when both hold a policy of the same name, and argocdf exports it as **`ARGOCDF_LINT_ID`** so your own `--lint` adapter can prefix its findings the same way — a command's stdout is otherwise passed through verbatim, so an adapter that ignores the variable produces unlabelled findings. A failure line no longer echoes the command: the identity says which `--lint` it was, so nothing you pass in a command — credentials included — reaches a PR comment.

### Fixed

- A side with no lint policies says so instead of reading as clean

  The base side of a change that adds its first policy has nothing to apply, which is tolerated rather than failed — correctly, since both tools treat an empty policy set as fatal. But it was silent, and that made the report assert something false: an unlinted side leaves every finding one-sided, and one-sided reads as "introduced by this change" when a first policy really means "pre-existing, newly detected". Each skipped side now contributes one note (`[base] [lint-kyverno#1] not linted: no policies in "policies/kyverno"`). The mirror case — a change that DELETES the policies — needs no separate handling: it is the same per-side note, on the target side, and the e2e suite pins that direction too (where the violation pre-exists on both sides, a `[base]`-only finding means it stopped being CHECKED, not that the change fixed it). "No policies" now also means "no file the tool would load", so a directory holding only a `.gitkeep` or a README is reported as not linted instead of yielding a linter that silently checked nothing, and one that cannot be read fails loudly.

- The dependency probe ArgoCD retries is no longer reported as an error

  A chart with a `dependencies:` section and no committed `charts/` directory logged one ERRO per render on a completely healthy run — the loudest line in a successful run described a step that worked. ArgoCD uses a failed `helm template` as its probe for "dependencies are not vendored yet", then runs `helm dependency build` and templates again, but the non-zero exit is logged before the caller decides it was harmless. The probe is demoted to DEBUG (`--verbose` still shows it), recognised by ArgoCD's own predicate so it cannot drift from what upstream actually retries. A `helm dependency build` that really failed stays at ERROR, and a retry that fails again still surfaces as the application's report error. Library log lines also carry an `argocd/exec` prefix when they describe a subprocess, so it is clear whether to read a tool's stderr or ArgoCD's own code.

- ArgoCD's `--api-versions` flood no longer buries the log line it belongs to

  ArgoCD passes one `--api-versions` pair per group/version AND per kind the cluster advertises, then quotes the whole helm command line into the record it logs on failure: 16,000 characters against a cluster advertising 309 of them, of which the last ~180 are the actual failure — unreadable in a terminal, useless to grep. Runs of those pairs now collapse to `--api-versions <309 elided>`. (An error travelling into a PR comment was never affected: ArgoCD strips the list there itself. The log record escapes that because it is written inside the helm wrapper, before the message is rewritten.)

- `--verbose` now actually enables ArgoCD's debug stream

  0.5.0 advertised `--verbose` as re-emitting everything ArgoCD logs through argocdf's own logger, but only the QUIET path ever set a level: a verbose run stayed at logrus's default, where ArgoCD's debug records were filtered before argocdf could forward them. The level is now set explicitly in both directions, and argocdf's own `ARGOCD_LOG_LEVEL=error` default is undone when a later configuration turns verbosity on — a value you set yourself is never touched.

- An application rendered twice is linted once

  An application can be rendered more than once: a child queued from the cluster listing is re-rendered with the git spec its parent's catalog supplies, and that later result replaces the earlier one — deliberately, since the earlier one rendered a base side the parent did not manage. Linting sat inside the render, so the discarded render was linted too, and its findings were thrown away with it. With a cluster-aware adapter that is the most expensive thing in the run: a second `kyverno apply --cluster` per side of every re-rendered application, tens of seconds each, multiplied by `--concurrency`. It also produced a log line the report could not account for — a superseded invocation that timed out logged `status=failed` at WARN while no warning line existed anywhere in the report, because the discarded result took its warnings with it. Linting now runs once per application, after discovery settles, on the manifests the report actually shows.

## [0.5.0] - 2026-08-05

### Added

- Add `--repo-creds cluster|local|none`: where repository credentials come from

  `cluster` (default) reads ArgoCD's repository secrets from the control-plane namespace through ArgoCD's own code, so private OCI and helm chart sources — and private chart dependencies — authenticate with no local helm setup; `local` uses your helm config; `none` disables. Remote chart downloads go through ArgoCD's chart client behind argocdf's persistent cache, so OCI dispatch, TLS client certs and proxies behave as they do in ArgoCD. Switching sources re-renders instead of answering from another source's cache, and re-fetches instead of serving its charts: the mode is part of the render-cache key and the chart cache is scoped by it, so "does ArgoCD's own credential set work?" is a question a cached run cannot answer for you.

- New `--lint-kyverno DIR` and `--lint-conftest DIR` built-in adapters

  argocdf execs the tool and parses its report itself, so the shell pipeline and its `jq` stage disappear — along with the two ways an adapter is usually written wrong: treating empty output as failure, and treating a findings exit as a crash. Findings are reported one line per offending resource, and kyverno's `error` results (a broken CEL expression, which otherwise just stops producing findings) surface marked `ERROR`.

- Honor ArgoCD's `manifest-generate-paths` annotation when selecting apps

  A dependency outside an app's `source.path` - a kustomize overlay whose base is `../shared`, a chart pulling `file://../lib` - is invisible to path matching, so the app was reported unaffected. Declaring `argocd.argoproj.io/manifest-generate-paths` now includes it, resolved by ArgoCD's own code so both tools select the same applications. The declaration REPLACES the default rather than extending it, exactly as in ArgoCD, and that has two edges worth knowing: `../base` alone stops the app reacting to its own directory (write `../base;.`), and it also replaces argocdf's helm file matching - `$values/...`, escaping (`../shared/vals.yaml`) and repo-root-absolute entries, and `fileParameters` alike - so a helm app must name those paths too. A declaration no source can resolve is warned about rather than silently making the app unreportable.

- `--lint` commands learn which cluster argocdf is diffing

  Every command gets `ARGOCDF_CONTEXT` (the resolved context) and `ARGOCDF_KUBECONFIG`, so cluster-aware adapters (`kyverno apply --cluster`, `kubectl apply --dry-run=server`) validate against the cluster under review instead of whatever the invoking shell pointed at.

### Changed

- BREAKING: `--namespace`/`-n` is replaced by `--argocd-namespace`

  The ArgoCD control-plane namespace (env `ARGOCDF_ARGOCD_NAMESPACE`, default `argocd`): repository secrets are read there and Applications are listed there. The `-n` shorthand is dropped so old invocations fail loudly instead of silently changing meaning. New `--application-namespaces` covers apps-in-any-namespace (literals, globs, `/regex/`), and `-A` scans all.

- BREAKING: rendering now runs through ArgoCD's own repo-server code, replacing argocdf's helm/kustomize pipeline

  Every render calls `reposerver/repository.GenerateManifests` — the `argocd app diff --local` code path — for exact ArgoCD parity: `--include-crds` (CRDs from `crds/` now appear in diffs), ArgoCD's own translation of every `spec.source.helm`/`kustomize` field, `$ARGOCD_APP_*` substitution, `.argocd-source*.yaml` overrides, recursive directory sources that include hidden (dot-)directories exactly as ArgoCD does, `$ref` value files resolved against the ref repository root - which also decides app SELECTION, so an app whose `$values` file changed is now reported instead of silently missing whenever its ref source carries a path, and `$ref` `fileParameters` (`--set-file`) now select their app too - and dependency builds in an isolated helm home that never touches your helm config. `--helm-skip-refresh` and `--helm-add-repos` are removed with the old pipeline — the engine registers chart dependency repositories itself, so fresh CI runners need no `helm repo add` priming and nothing mutates local helm state. Costs ~85MB of binary — the same trade as the ArgoCD types import: size for structurally-eliminated behavior drift.

- `--no-cache` takes an optional layer: `all` (the bare flag), `render`, or `charts`

  The two caches have different jobs, so they are now disabled separately. `--no-cache=render` re-renders everything while keeping multi-megabyte chart downloads, which is the common case when iterating on manifests; `--no-cache=charts` re-downloads for the apps that re-render anyway. The bare flag still disables both, `--no-cache=false` re-enables them when an environment variable set them off, and the old boolean spellings keep their meaning.

- `--lint-timeout` defaults to 10s, and each invocation's duration is logged

  Cluster-aware adapters pay an API discovery per invocation, so with `--concurrency` the tail is contention rather than tool speed.

- Reports are deterministic and self-describing

  Application sections are sorted by (namespace, name) in every format, so two runs can be compared byte-for-byte, and file footers stamp the argocdf version with the resolved base → target commits.

- Library logs are quiet by default and prefixed under `--verbose`

  ArgoCD's per-command exec tracer and client-go's expected watch-cancellation error no longer surface in normal runs; `--verbose` re-emits everything through argocdf's own logger with `argocd:`/`client-go:` prefixes, in argocdf's format and timeline.

- Rebuild the e2e suite as a real end-to-end artifact

  The e2e submodule is now 50 `case/*` branches with byte-pinned reports in every format, two kind bootstrap modes producing identical output (a real ArgoCD controller, or the same Application set applied once), and a review gate that fails a pin which has stopped proving its point. A behavior suite (`internal/render/behavior_test.go`) additionally pins what every rendering scenario produces end-to-end.

### Fixed

- The render cache refuses inputs it cannot prove stable

  Two soundness gaps. A remote chart pinned to a MUTABLE revision - `HEAD`, `*`, or a constraint range like `^2.0.0` - was cached by its literal revision string, so publishing a newer chart never invalidated anything: the same Git comparison kept returning the old manifests forever. Such revisions now bypass the render cache, with the same exact-version predicate the chart-download cache uses (which previously disagreed: it cached ranges too). And the discovered cluster API-version set - a render input charts branch on via `.Capabilities.APIVersions.Has` - was absent from the key, so installing a CRD, toggling `--no-api-versions`, or switching clusters at an identical Kubernetes version could reuse manifests rendered for different capabilities. The set is now hashed, sorted and deduplicated so discovery order cannot thrash the cache. All prior entries are invalidated once by the `rendercache-v5` bump.

- Value files outside an app's `source.path` now select that app

  A helm `valueFiles` or `fileParameters` entry may point outside the source path (`../shared/vals.yaml`) or at the repository root (`/config/prod.yaml`) - ArgoCD renders both, but argocdf matched neither, so a PR touching a values file shared by several apps could report nothing at all. The rule is not reimplemented: selection now calls the same ArgoCD resolver the render uses, so selection, rendering and the render cache agree on which files an app reads, and an entry escaping the repository is still refused exactly as ArgoCD refuses it. `$ref` `fileParameters` are matched too, closing the same gap one field over.

- Apps-of-apps children removed between branches are reported as deleted

  The child renders from its base-branch spec and all of its resources show as removed, symmetric with added children; previously only the parent's Application resource was visible and the child's workloads vanished from the report. Cascades to grandchildren within the discovery depth limit.

- Sources in other git repositories render from their own checkout

  Multi-source apps and apps-of-apps children pointing at another repo were resolved against the local worktree, so they rendered empty on both sides and reported "No changes" — hiding the diff entirely.

- Scheme-less OCI chart repositories are pulled as OCI

  `ghcr.io/org` — how ArgoCD stores OCI repos — was treated as a classic HTTP repo and failed with "could not find protocol handler". A scheme-less URL can only be OCI.

## [0.4.1] - 2026-07-23

### Fixed

- `spec.source.helm.skipSchemaValidation` is respected

  Passed through as `--skip-schema-validation` (needs helm >= 3.16). Apps that opt out of `values.schema.json` rendered fine in ArgoCD but failed here with "values don't meet the specifications of the schema(s)".

## [0.4.0] - 2026-07-08

### Added

- Add `--lint`: pipe rendered manifests through commands of your own

  Each side is linted separately, with that side's worktree as the working directory, so repo-relative policy paths resolve to the branch's own files. Every non-empty stdout line becomes a `[base]`/`[target]`-labeled warning in all output formats, and the labels double as the diff: `[base]`-only was fixed by the change under review, `[target]`-only was introduced, both sides is pre-existing. Any policy tool plugs in through a small adapter — argocdf never parses tool-specific output. A spawn error, timeout or non-zero exit becomes a non-fatal warning, and stdout received before it is kept. `--lint-timeout` defaults to 5s.

## [0.3.0] - 2026-07-07

### Added

- Add `split[=N]` for markdown file outputs

  A report larger than N bytes (default 60000, under GitHub's 65,536-char comment cap) is written as self-contained parts — `pr-comment.md`, `pr-comment.2.md`, ... — each carrying the upsert marker, a `part i/N` heading and balanced `<details>` blocks and fences, so CI can post each part as its own comment. An app stays within one part unless it alone exceeds the limit, and stale parts from a previous larger run are removed.

- Add `--helm-add-repos`: register dependency repositories before `helm dependency build`

  Deduplicated per run: a URL already registered under any name is only refreshed, unknown URLs are added as `argocdf-dep-<hash>`. It mutates local helm state either way, so it is off by default — meant for ephemeral CI runners whose helm repo cache is empty.

### Changed

- A missing dependency repository now says what to do about it

  "no cached repository" / "no repository definition" errors list the repos to `helm repo add` and point at `--helm-add-repos`.

- The GitHub Actions examples post split reports

  Both workflows render with `split` and post each part as its own comment, and pass `--helm-add-repos` for fresh-runner dependencies; `wf-extra.yaml` drops its oversized-comment truncation step.

## [0.2.3] - 2026-07-07

### Fixed

- Child apps found only on the target branch skip the base render

  They were rendered against the base worktree with the target spec, which failed hard whenever that spec referenced files absent on base — a newly added values file, say. Such children now report all their resources as added instead of "No changes".

## [0.2.2] - 2026-07-05

### Changed

- The `argocdf` name in report footers links to the project repo (HTML and Markdown)

## [0.2.1] - 2026-07-04

### Fixed

- A run with no affected applications still writes a complete report

  File outputs (markdown/HTML/unified) were left 0 bytes, losing the markdown upsert marker CI needs to overwrite a stale PR comment. Terminal output stays quiet, since the run already logs the empty result.

## [0.2.0] - 2026-07-04

### Added

- Add `--verbose`/`-v`

  Logs the resolved repo URL, cluster context and namespace.

- Every flag is settable through an `ARGOCDF_*` environment variable

  Precedence is flag > env > default.

### Changed

- Upgrade the Argo CD dependency to v3.3.11 (from v2.14.21)

  Module path `argo-cd/v3`, k8s.io libraries to 0.34.0.

### Fixed

- Multi-source apps detect Helm path sources

  Every source now goes through `Factory.GetRenderer`, so a path source with a `Chart.yaml` renders as Helm instead of falling back to plain-YAML concatenation.

## [0.1.0] - 2026-07-03

### Added

- Initial release

[0.6.0]: https://github.com/rgeraskin/argocdf/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/rgeraskin/argocdf/compare/v0.4.1...v0.5.0
[0.4.1]: https://github.com/rgeraskin/argocdf/compare/v0.4.0...v0.4.1
[0.4.0]: https://github.com/rgeraskin/argocdf/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/rgeraskin/argocdf/compare/v0.2.3...v0.3.0
[0.2.3]: https://github.com/rgeraskin/argocdf/compare/v0.2.2...v0.2.3
[0.2.2]: https://github.com/rgeraskin/argocdf/compare/v0.2.1...v0.2.2
[0.2.1]: https://github.com/rgeraskin/argocdf/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/rgeraskin/argocdf/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/rgeraskin/argocdf/releases/tag/v0.1.0
