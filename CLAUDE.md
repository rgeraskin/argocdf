# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**argocdf** is an ArgoCD Diff Tool - a Go CLI that analyzes Git changes and displays manifest diffs for all ArgoCD applications affected by those changes. It renders manifests from both branches using Helm/Kustomize and computes semantic diffs.

## Common Commands

Tasks are defined in `.mise.toml` (there is no Makefile).

```bash
# Build
mise run build          # Build binary to ./argocdf
mise run install        # Install to GOPATH/bin

# Test
mise run test           # Run tests with verbose output
mise run test-coverage  # Generate coverage report

# Development
mise run dev            # Run in development mode
mise run lint           # Run golangci-lint
mise run fmt            # Format code (go fmt + goimports)
mise run vet            # Run go vet
mise run check          # vet + lint + test

# Dependencies
mise run deps           # Download dependencies
mise run tidy           # Tidy go.mod
```

## Architecture

The tool follows a pipeline architecture orchestrated by `internal/app/app.go`:

1. **Connect** → K8s cluster via client-go
2. **Detect** → Auto-detect repo, branches, cluster version
3. **Fetch** → List ArgoCD Applications from cluster
4. **Filter** → Match apps to changed files in git diff (`App.filterAffectedApps`: source-path containment, `$ref` value files, and ArgoCD's `manifest-generate-paths` annotation — see below)
5. **Render** → Generate manifests for both branches (Helm/Kustomize)
6. **Diff** → Compute semantic diff (added/removed/modified resources)
7. **Output** → Terminal (colored) and/or HTML report

### Key Packages

| Package | Purpose |
|---------|---------|
| `cmd/argocdf` | CLI entry point (Cobra) |
| `internal/app` | Main orchestrator and factory |
| `internal/config` | Configuration and auto-detection |
| `internal/cluster` | K8s client, ArgoCD Application types (via type aliases), cluster repo credentials (`util/db`), namespace matching |
| `internal/helmconfig` | Local repo credentials from the user's helm config (`--repo-creds=local`) |
| `internal/git` | Repository operations, change detection, URL normalization |
| `internal/render` | Manifest rendering through ArgoCD's repo-server code, chart fetching via ArgoCD's chart client |
| `internal/diff` | Manifest comparison, apps-of-apps discovery |
| `internal/output` | Output writers (terminal, markdown, HTML, unified) |

### ArgoCD Types Dependency

The `internal/cluster` package uses ArgoCD's official types from `github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1` via type aliases. This ensures automatic compatibility with all ArgoCD Application fields and eliminates field drift bugs.

```go
// Type aliases in internal/cluster/applications.go
type Application = argoapp.Application
type ApplicationSource = argoapp.ApplicationSource
type ApplicationSourceHelm = argoapp.ApplicationSourceHelm
// ... etc
```

Trade-off: This adds ~35MB to the binary size but eliminates maintenance burden of keeping custom structs in sync with ArgoCD's schema.

### Design Patterns

- **Interface-based design**: `applicationRenderer`, `Writer`, `Differ` allow pluggable implementations
- **Factory pattern**: `internal/app/factory.go` handles dependency injection
- **Multi-writer**: Simultaneous output to multiple destinations (terminal + files)
- **Queue-based recursion**: Depth-limited discovery for apps-of-apps pattern

### Output Writers

| Writer | File | Description |
|--------|------|-------------|
| `TerminalWriter` | `terminal.go` | Colored terminal output (fields/summary/unified modes) |
| `MarkdownWriter` | `markdown.go` | GitHub/Atlantis markdown with collapsible sections |
| `HTMLWriter` | `html.go` | Interactive HTML with diff2html side-by-side view |
| `UnifiedWriter` | `unified_writer.go` | Patch-compatible unified diff format |
| `MultiWriter` | `output.go` | Fans out to multiple writers simultaneously |

## ArgoCD Application Support

The tool supports both single and multi-source applications:
- `spec.source` - Single source configuration
- `spec.sources[]` - Multiple sources (Helm + values from git, etc.)

Apps-of-apps pattern is handled via recursive discovery with configurable max depth.

### App selection and `manifest-generate-paths`

`App.filterAffectedApps` decides which apps a changed-file list touches. Four matchers, in this order: an app that DECLARES `argocd.argoproj.io/manifest-generate-paths` is judged by that declaration ALONE (`manifestGeneratePathsAffected`), otherwise `$ref` value files AND `fileParameters` resolve through `helmRefFilesAffected` (one list: ArgoCD resolves both through the same two branches, and `rendercache/key.go` hashes both, so matching only one leaves a `--set-file`-from-a-ref-repo app silently unreported) — ROOT-relative within the ref repository, never joined to the ref source's `Path`, because that is what ArgoCD's `getResolvedRefValueFile` does (it splits the entry on `/`, BLANKS the first segment so the remainder is repo-root-absolute, and passes the ref repo checkout as both appPath and repoRoot; `RefTarget` has no path). Joining instead makes the matcher look for a file that does not exist, so a values change goes unreported — the same mistake that lived in the render-cache key until `rendercache-v3`. That rule now has exactly ONE implementation, `cluster.ResolveRefFilePath`, used by both the matcher and `rendercache/key.go`: it splits the `$name/rest` and delegates the path to `ResolveHelmFilePath` with an EMPTY source path (equivalent to upstream, since appPath and repoRoot are the same directory there), so the traversal and resolve-to-root refusals are ArgoCD's too. Reintroducing the join in that one place fails tests in all three packages — which is the point, since the two former copies each had it wrong at different times, and it also fixed a shape both got wrong: an ABSOLUTE remainder (`$values//config/x.yaml`) resolves repo-root-relative rather than being compared verbatim (selection: never matched) or bypassed (cache key). Note a ref source that ALSO carries a `Path` is a renderable source here (`isPureRef` requires neither Path nor Chart), so changes under it match by ordinary containment as well. Then `source.Path` containment. Finally `helmLocalFilesAffected` resolves the SAME two lists for non-`$ref` entries of sources in the diffed repo — a value file or `fileParameter` may sit OUTSIDE the source path (`../shared/vals.yaml`) or be repo-root-absolute (`/config/prod.yaml`), both of which ArgoCD renders happily (it refuses only escapes from the REPOSITORY) and neither of which containment can see. That resolution is NOT reimplemented: `cluster.ResolveHelmFilePath` calls ArgoCD's own `pathutil.ResolveValueFilePathOrUrl` — the function the repo-server resolves with — against a repo root that DELIBERATELY does not exist, because selection runs before any worktree does (`Run` filters at app.go:178, `setupWorktrees` at 201) and upstream's only filesystem call is `os.Readlink`, which reports a missing path as "not a symlink". That is upstream's implementation detail rather than its contract, so `TestResolveHelmFilePathParity` re-runs the same entries against a REAL tree: were an argo-cd bump to require existence, resolution would fail for every entry and the matcher would silently stop matching. Its documented cost is that a symlinked value file resolves to the link's own path (`TestResolveHelmFilePathSymlinkDivergence`), which is the right level for matching a changed-file list anyway. Chart sources are skipped (their relative value files live in the EXTRACTED chart, and the empty `Path` would otherwise resolve them against the repo root), and the ordering matters: the annotation still REPLACES all three of the others. KNOWN GAP: a change touching ONLY a `--lint-kyverno`/`--lint-conftest` policy directory selects no app at all (selection matches `source.path`, and policies are not a source), so nothing renders and no policy runs — `case/lint-policy-added` therefore has to change an app's files too, and pins the skip-note MECHANISM rather than the pre-existing-violation story. Selection also does NOT run `Env.Envsubst` over an entry, because ArgoCD's env builder (`newEnv`) is unexported and the revision differs per side, so `values-$ARGOCD_APP_NAME.yaml` resolves literally and matches nothing - harmless for an in-path entry (containment still selects it) but a real miss for an escaping or absolute one; pinned as a table row in `TestResolveHelmFilePath` and in DIFFERENCES.md §16 rather than silently accepted. The empty-change-list case is the mirror deviation: `ChangedUnderDeclaredPaths` returns FALSE where upstream returns true, because argocdf computed the diff itself, so empty means "nothing changed" and not "payload omitted the list" - without that guard `--base X --target X` reported every annotated app as changed. The annotation is resolved by ArgoCD's own `util/app/path.GetSourceRefreshPaths`/`AppFilesHaveChanged`, wrapped in `internal/cluster` so the argo-cd import stays with the type aliases (`HasManifestGeneratePaths`/`ManifestGeneratePaths`/`ChangedUnderDeclaredPaths`).

The direction is inverted between the two tools and that is the point: ArgoCD refreshes EVERY app in a repo on any commit and the annotation NARROWS that (webhook filtering + manifest-cache reuse), while argocdf renders only path matches so the annotation WIDENS — it is the only way to reach a dependency argocdf cannot infer (a kustomize base at `../shared`, a values file elsewhere). Because it is a declaration of what generates the manifests, it REPLACES argocdf's own matching for that app rather than adding to it, exactly as in ArgoCD: `../base` alone stops the app's own path from matching, so `../base;.` is the usual form. Three deliberate deviations from a naive port: an annotation declaring nothing USABLE is treated as absent - present-but-EMPTY, and separators only (`;`, `;;`), which ArgoCD's split discards entry by entry (ArgoCD would refresh always; argocdf falls back to path matching, so either typo degrades to the default instead of rendering everything forever or deleting the app from every report); an EMPTY changed-file list affects nothing (`ChangedUnderDeclaredPaths` returns false where upstream returns true - see below); and declared paths are only resolved for sources in the repo being diffed. The separator-only fallback is told apart from a genuinely unresolvable declaration by whether any source belongs to the diffed repo: a remote-chart-only app has nothing to fall back TO, so it keeps the WARN-and-never-affected behavior. Two helm shapes bite and are pinned in the tests: the declaration also replaces `helmRefFilesAffected` AND `helmLocalFilesAffected`, so an annotated app that forgets its `$values`, escaping (`../shared/vals.yaml`), absolute (`/config/prod.yaml`) or `fileParameters` paths silently stops reacting to changes in them (ArgoCD documents the same caveat), and a `ref:`/remote-chart source has an EMPTY `Path`, so a relative entry resolves against the repo ROOT — `.` there matches every file and the app becomes always-affected. When no source resolves the declaration at all (remote-chart-only app) the app can never be reported, so `manifestGeneratePathsAffected` WARNS; that warning is asserted, since it is the only signal a user would get before the app quietly disappeared from every report. APPS-OF-APPS BOUNDARY: the annotation gates wave-0 selection ONLY — discovery is annotation-blind (children are enqueued from the parent's RENDERED Application CRs, `processApplications` never consults changed files) — so an annotated CHILD keeps spec-change detection through its parent while losing own-content detection its declaration forgets (pinned by `TestManifestGeneratePathsDoesNotGateDiscovery`), and an annotated PARENT whose declaration misses the catalog hides its ENTIRE subtree: added, modified and removed children all vanish with the unrendered parent. Semantics verified against v3.3.11's resolver and a live controller; pinned by `internal/app/manifestgeneratepaths_test.go` and `e2e/case/kustomize-relative-base`.

## Render Engine

One engine implements the app-side `applicationRenderer` seam (`internal/app/app.go`), built by `Factory.CreateRenderer`: `internal/render/argocd.go` wraps ArgoCD's `reposerver/repository.GenerateManifests` (the `argocd app diff --local` code path) for exact ArgoCD parity. ArgoCD does source-type dispatch, the complete helm/kustomize option translation (incl. `--include-crds` by default), `ARGOCD_APP_*` build-env substitution, `.argocd-source*.yaml` overrides, and dependency builds into an isolated temp helm home. argocdf still owns worktrees, remote-chart fetching (through ArgoCD's `util/helm.Client.ExtractChart` wrapped in the persistent chart cache — see `internal/render/chartfetch.go`), `$ref` checkout (registered in a `TempPaths` keyed by normalized repo URL — GenerateManifests never clones), and the render cache. Integration constraints that matter when touching this file: `q.Repo` and `q.ApplicationSource` must be non-nil, the source is deep-copied because GenerateManifests mutates it, calls are serialized per appPath (dependency builds write `charts/`/`Chart.lock` into the worktree), `isLocal=false` is what ISOLATES the helm home (in ALL `--repo-creds` modes) — but that isolation only holds because `NewArgoCDRenderer` SCRUBS inherited `HELM_*` env vars — NOT for duplicate-key reasons (`exec.Cmd` dedups `Env` keeping the LAST value, so ArgoCD's appended vars do win): ArgoCD appends only `XDG_{CACHE,CONFIG,DATA}_HOME` + `HELM_CONFIG_HOME`, and helm resolves its own `HELM_CACHE_HOME`/`HELM_DATA_HOME` in PREFERENCE to those `XDG_*` paths, while `HELM_REGISTRY_CONFIG`/`HELM_REPOSITORY_*`/`HELM_PLUGINS` are never set by ArgoCD at all (see `render.inheritedHelmEnvVars` for the verified breakdown) — and the revision param feeds `ARGOCD_APP_REVISION*`. Dependency logging is routed centrally by `configureDependencyLogging` (`cmd/argocdf/main.go`): ArgoCD's global logrus re-emits through argocdf's logger under an `argocd` prefix (errors-only unless `--verbose`) — or `argocd/exec` when the record carries `util/exec`'s `execID` field, because that global logger is ONE stream carrying several sources and the first thing a reader needs is whether to look at a subprocess's stderr or at ArgoCD's Go code (a rename upstream costs the sub-prefix, not correctness) — with one message transform on the way through: the EXPECTED first-attempt `helm template` failure of an umbrella chart is demoted to DEBUG (`render.IsRetriedMissingDependencyLog`, which calls ArgoCD's OWN `helm.IsMissingDependencyErr` — the predicate whose true-value triggers GenerateManifests' `helm dependency build`-and-retry at repository.go:1305-1311 — so it cannot drift from what upstream actually recovers from; demoted never dropped, guarded on `helm template` so a `helm dependency build` that really failed stays loud, and nothing is lost if the retry fails too because that error is RETURNED and surfaces as `processWave`'s app-named WARN plus `AppDiff.Error`). client-go's klog rides the slog bridge under `client-go` in `--verbose` (klog encodes V(n) as slog level -n — `klogHandler` drops V>4, clamps the rest to DEBUG — unmapped negative levels would otherwise render as INFO — and demotes context-canceled watch errors to DEBUG: the reflector reports argocdf's own deliberate informer shutdown at error severity) and is discarded otherwise, and the per-exec tracer (a FRESH logrus logger per command, unhookable) gets `ARGOCD_LOG_FORMAT=text` plus, without `--verbose`, `ARGOCD_LOG_LEVEL=error` — explicit values always respected.

History: through 0.4.x argocdf rendered with its own "native" helm/kustomize pipeline; 0.5.0 replaced it with ArgoCD's repo-server code (a `--renderer` selector existed only during 0.5.0 development, never in a release) and removed `--helm-skip-refresh`/`--helm-add-repos` with it. The replacement changed `$ref` value-file resolution to ArgoCD's repo-root semantics in the render-cache key too (`rendercache-v3`). `rendercache-v4` then added the credential SOURCE and the helm repository ALIASES to the key, and `rendercache-v5` added the credential-source INSTANCE (cluster API SERVER + ArgoCD namespace in `cluster` mode, empty otherwise — the server, not the context name: a name is a kubeconfig-local ALIAS, so two files can both call different clusters `prod` and a recreated kind cluster repoints a name in place; the server is what the name dereferences to and what ArgoCD keys cluster secrets by, pinned by `TestCredentialInstanceKeysOnServerNotContextName` through the App's own derivation seam `credentialInstanceFor`), the sorted/deduplicated cluster API-VERSION set (charts branch on `.Capabilities.APIVersions.Has`, and it also keys the `--no-api-versions` toggle), and a BYPASS for remote charts whose target revision is not one exact immutable version — `HEAD`, `*` and constraint ranges resolve against the mutable registry index, so identical key inputs can render differently over time (`render.IsImmutableChartVersion` is the ONE predicate both caches share; they used to disagree, with the chart cache wrongly persisting ranges). The mode is hashed because a mode switch is usually a QUESTION - render with `local`, re-run with `cluster` to check ArgoCD's own credentials work - and answering it from the other mode's entries makes a repository ArgoCD cannot reach fail only after merge; the alias fingerprint cannot stand in for it, since a missing OCI registry, a nameless credential template and a wrong password all leave the alias list identical. The instance is hashed because the same question recurs one level in: two clusters both read with `--repo-creds cluster` are two credential sources. It costs almost nothing: the cache is per-machine, so CI and a laptop never share entries, and a single machine switching modes is doing exactly that verification. The chart cache is scoped by credential source for the same reason (`Factory.chartCacheDir` appends `charts/<mode>` plus, in cluster mode, an instance segment - sanitized server+namespace with a short hash), so a mode or cluster switch re-fetches instead of serving a chart the other source downloaded - one re-download per source, deliberate. REMAINING LIMIT: within ONE credential source a downloaded chart is served without contacting the registry, so a credential that expired since is invisible; `--no-cache=charts` forces the download for apps that re-render anyway (a render-cache HIT skips fetching altogether, which is why charts-only bypass reaches only re-rendering apps), and helm DEPENDENCY builds always re-fetch since the helm home is per-run. The aliases are hashed because: ArgoCD runs `helm repo add <name> <url>` for every non-OCI repo of the active credential list before a dependency build (`util/helm/helm.go:105`), so a `repository: "@name"` dependency resolves through the list argocdf supplied - the mapping decides WHAT renders, not just whether the fetch is allowed, and a name pointed elsewhere (by switching `--repo-creds`, or by editing one source) previously served the other render from cache. `App.helmRepoAliases` carries only named non-OCI entries (nameless ones are URL-only, OCI ones take the registry-login path) and never credentials: hashing a password would invalidate the cache for something that cannot change a manifest.

### Repository Credentials (`--repo-creds cluster|local|none`)

All three modes run the identical pipeline; they differ only in what fills the credential fields of `render.RenderOptions` (four `Repository`/`RepoCreds` lists + a `ResolveRepo` closure; `none` leaves them empty — render code is mode-blind):

- **cluster** (default): `internal/cluster/repocreds.go` reads ArgoCD's repository secrets via ArgoCD's own `util/db`/`util/settings` from `--argocd-namespace`. Gotchas encoded there: a preflight `Secrets List (Limit:1)` probe MUST run before the settings manager's first use (its informer `WaitForCacheSync` has no internal timeout — forbidden RBAC would hang forever), and the manager context is bounded (~30s; the synced informer indexer keeps serving reads after it ends — client-go's reflector logs that expected watch cancellation through klog, handled by `configureDependencyLogging` in main). Load failures are FATAL in app.initialize with a message naming the other modes.
- **local**: `internal/helmconfig` parses repositories.yaml (classic creds are inline there) into the same lists and sets `HELM_REGISTRY_CONFIG` to the user's registry config (resolved via `helm env`) — an explicit helm env var beats the isolated `HELM_CONFIG_HOME`-derived default, so OCI logins (including credential helpers) pierce the isolated homes read-only. The path also travels as `RepoCredentials.HelmRegistryConfig` → `RenderOptions.HelmRegistryConfig`, because the engine's env scrub would otherwise remove it (the engine re-installs it and then owns NO auth file — local lists have no OCI creds, so nothing ever logs in and the strip is a no-op). Chart fetches that resolve inline OCI creds anyway (hand-crafted repositories.yaml entries) are stripped too — no seeding, auth rides the user's registry config — so `helm registry login` never runs in local mode either.
- The engine composes `q.Repos`/`q.HelmRepoCreds` per source with ArgoCD's `source.IsOCI()` gate (mirroring controller/state.go:300-315, including its degradations; note `type: helm` + `enableOCI` repos ride the HELM list UNCONDITIONALLY — that's how git-path charts get their private OCI dependency creds) and resolves `q.Repo` through `ResolveRepo`.
- OCI registry auth NEVER goes through `helm registry login` (`internal/render/registryauth.go`): helm's ORAS credential store auto-detects platform native stores (any `docker-credential-*` helper in PATH — on macOS every login would land in the SHARED system keychain, where concurrent renders' login→build→logout cycles race: `-25299` duplicate items, and 401s when one render's logout deletes the credential another's dependency build is using; a fresh isolated config file does NOT prevent this). Instead the engine owns a per-run registry config (seeded with a placeholder entry — a "configured" file is what disables the native-store detection — plus every OCI credential from the lists, written argocdf-side under a mutex with atomic renames, 0600, removed via `Cleanup()`), points `HELM_REGISTRY_CONFIG` at it, and STRIPS username/password from OCI-flavored entries in `q.Repos`/`q.HelmRepoCreds` and from the repo handed to the chart client, so ArgoCD's login gates (username AND password non-empty) never fire and pulls read the file. Classic (non-OCI) repos keep their creds — they flow via `helm repo add`/`pull` argv, not the registry config. Auth keys are registry HOSTS (same-host repos with different creds collapse; upstream has the same granularity).
- Chart fetches (`chartfetch.go`): credential-resolution failures are LOUD (no anonymous fallback — a swallowed error would resurface as a misleading 401 from helm); `ExtractChart` runs interruptibly (goroutine + background drain — ArgoCD's chart client takes no context); a cache hit skips fetching and auth entirely (pinned versions are immutable — intentional); cache-backed chart dirs are COPIED to a private temp dir before `GenerateManifests` (it mutates appPath, and `chartDepMutex` is process-local so it cannot protect the shared persistent cache from concurrent argocdf PROCESSES).
- External git repositories: renderable sources and `$ref` sources living in OTHER repos are cloned at their `TargetRevision` via `git.CloneWithCreds` — HTTP(S) basic-auth/bearer credentials ride an `http.extraheader` passed through `GIT_CONFIG_*` env vars (never argv); one clone per (URL, revision) per app render (`externalrepo.go`). SSH remotes use ambient git config.
- The engine uses `app.InstanceName(ArgoCDNamespace)` — `"<ns>_<name>"` outside the control-plane namespace — for `AppName`/the default helm release name, and sets `ProjectName` (feeds `ARGOCD_APP_PROJECT_NAME`); remote-chart relative helm value files resolve against (and are contained to) the EXTRACTED chart dir, not the worktree.
- IMPORT BOUNDARY: only `internal/cluster` and `internal/app` may import `util/db`/`util/settings`; `internal/render` sees only the lists + closure.

Namespace flags: `--argocd-namespace` (control-plane; secrets + default app listing) and `--application-namespaces` (EXHAUSTIVE list, glob + `/regex/` entries matched via ArgoCD's `glob.MatchStringInList` — deliberately WITHOUT `IsNamespaceEnabled`'s control-plane short-circuit; all-literal lists are served per-namespace so namespace-scoped RBAC suffices). `-A` normalizes to `["*"]` in config.WithDefaults.

The trade-off mirrors the types-import decision: ~85MB extra binary for structurally-eliminated behavior drift. On argo-cd version bumps, expect `GenerateManifests` signature churn (loud, compile-time) and re-check that the in-tree gitops-engine fork hasn't diverged from the published module argocdf links.

### Automatic Helm Dependency Management

Charts with a `dependencies:` section (umbrella charts) render without manual setup: ArgoCD's `GenerateManifests` runs the dependency build itself, inside an isolated temp helm home where it registers the chart's dependency repositories automatically. Nothing reads or mutates the user's helm config, and fresh CI runners need no `helm repo add` priming.

## Linting Rendered Manifests (`--lint`)

`--lint "shell command"` (repeatable; `--lint-timeout`, default 10s) pipes each affected app's rendered multi-doc YAML into the command's stdin via `sh -c`, per side. Each side's command runs with that side's ephemeral worktree as its working directory, so repo-relative policy paths resolve to the branch's own version of the files (a PR changing a policy lints each side with its own policy). Every non-empty stdout line becomes a warning appended to `ManifestSetDiff.ParseWarnings` with the existing `[base]`/`[target]` labels (`diff.LabelSide`), so all writers, badges, and split-packing handle lint findings with zero writer-specific code. The label semantics double as the diff: `[base]`-only = fixed by the PR, `[target]`-only = introduced, both = pre-existing.

The runner lives in `internal/lint` and is spliced into `processOneApp` (`internal/app/app.go`) after `DiffManifests`; sides with an empty render (new/deleted apps) are skipped. Error contract: the process outcome is the only health signal — stdout lines are always kept, and spawn failure, timeout, or exit ≠ 0 appends one non-fatal self-identifying warning line. Stdout content never influences error detection; tools that exit non-zero on findings (kyverno, conftest) are expected to sit behind a jq adapter that exits 0. Empty tool output is ambiguous and must be resolved by the tool's EXIT CODE, never by a `jq -rn 'input'` that dies on empty stdin: kyverno legitimately prints nothing (exit 0) when no rendered resource matches a policy, so the jq-dies idiom attaches a spurious lint-failure warning to every ConfigMap- or CR-only app (it had polluted 23 of 35 e2e cases). The README's example adapter and the e2e adapter both branch on the exit code instead. `ARGOCDF_LINT` can carry only a single command (StringArray-via-env limitation); repeat `--lint` for several.

Every invocation logs ONE line — `Linted app=… side=… linter=kyverno#1 policies=… status=ok lines=0 duration=1.9s` — because the REPORT cannot tell three outcomes apart (ran-and-clean, skipped for want of policies, died: empty output at exit 0 is a legitimate no-findings result by contract, and a failed invocation contributes exactly one line, indistinguishable from one finding). `status` is the only field that separates them, `lines` counts everything contributed (findings, skip notes AND failure warnings — the log's job is what was emitted, `status` classifies it), and `status=failed` logs at WARN because it means the side was NOT linted. The per-side aggregate (`Lint totals`, `App.lintSide`) is DEBUG: `findings` there is just the sum of those lines, so it is both derived and the ambiguous one. Naming: each linter's identity has two spellings, mapped mechanically — the REPORT label is the flag name (`lint-kyverno#1`, since a PR comment has no log beside it and the label is what a reader would have to type), the LOG value drops the `lint-` the `linter=` field already implies (`kyverno#1`), and the bare `--lint` flag has nothing to strip so both coincide (`lint#1`). What a linter was pointed at rides `command=`/`policies=` rather than one shared `target=`, which would sit next to `side=target` meaning something else. In the REPORT every lint line is BRACKETED and whether the bracket CONTINUES past the linter says which family the line belongs to: `[lint-kyverno#1/require-pinned-images] Deployment/web: …` is a finding (the text after it is the tool's), `[lint-kyverno#1] not linted: no policies in "policies/kyverno"` is argocdf speaking about the linter itself (timeout, skip, crash — no resource to name, outcome first and detail after the colon), and NO bracket is not lint at all but a diff-layer parse warning, which is exactly what unbracketed health lines were ambiguous with. ONE identity per SURFACE, not one per line kind: a report always names the FLAG so `grep lint-kyverno#1` finds everything that linter produced, findings included; the log drops the `lint-` its field name repeats. `linterID` carries both — `name` (flag-spelled, bracketed via `bracket()`) for the report, `handle` (`logHandle`) for `linter=`. An intermediate design spelled findings after the TOOL and only health lines after the flag; it was abandoned because no single grep could then follow one linter, and REINSTATING it is the drift to watch for (`TestLinterIdentityPerSurface` and `TestFindingBracketCarriesTheLinterIdentity` fail if it comes back). Both spellings carry the ordinal, which on findings is what tells two directories of the same tool apart when both hold a policy of the same name — pinned end-to-end by `case/lint-two-policy-dirs`, whose must-not pair proves the ordinal follows FLAG order rather than merely differing. `diff.LabelSide` still prefixes the side, which outranks the subsystem: `[base] [lint-kyverno#1] …`. argocdf EXPORTS the identity to every `--lint` command as `ARGOCDF_LINT_ID` (per invocation, layered over `Env` in `commandEnv` — `Env` is shared by every command of the run and would be raced by concurrent renders), so an adapter can prefix its findings exactly as argocdf prefixes its own lines; the e2e adapters REQUIRE it (`: "${ARGOCDF_LINT_ID:?}"`) so a regression in the export fails the suite instead of passing quietly. Their identity is therefore the truthful `lint#1`/`lint#2` — they are `--lint` commands — so `case/lint-builtin` and `case/lint-two-adapters` are byte-identical EXCEPT for that token, and `review-expected.sh`'s `same-report-as` gate normalizes it alongside the case name for exactly that reason. Note argocdf does NOT prefix a command's own stdout: an adapter that ignores `ARGOCDF_LINT_ID` produces unlabelled findings, which is the documented limit of the contract. Every linter carries a 1-based ORDINAL in flag order: a repeated `--lint` has no other identity, since its command text is truncated at `maxDisplayCommand` (48) and two commands differing only past that cut are otherwise identical in both log and report; the built-ins take it too because one rule for the family beats an exception. `lint.Subject` (app/side/worktree), never `Target` — `target` is the target SIDE everywhere else. Durations round through `lint.RoundDuration` (ms below a second, tenths above), which is only safe because `status` now carries the did-it-time-out signal the millisecond tail used to; whole seconds would print a 9.6s success as `10s` and erase the headroom the field exists to show.

`--lint-kyverno DIR` / `--lint-conftest DIR` are BUILT-IN adapters for the two tools the shell examples are written around (`internal/lint/kyverno.go`, `conftest.go`): argocdf execs the tool and parses its JSON in Go, so the jq stage — the usual source of adapter bugs — disappears. They exec the CLIs rather than embedding kyverno's CEL engine or OPA (a far larger dependency and a behavior change); what is replaced is the pipeline, not the tool. Shared machinery in `lint.go`: `execTool` applies the same health contract as the shell path (empty stdout + exit 0 = no findings; empty + non-zero = real failure; non-empty + non-zero = normal, both tools exit non-zero on findings), and `resolvePolicyDir` skips a directory holding no file the tool would LOAD (`lint.HasPolicies`, recursive, per-tool extensions, root resolved through symlinks because both tools read through one and `WalkDir` follows none — "has entries" made a `.gitkeep`-only directory look populated while kyverno exited 0 with no results, i.e. status=ok for a linter that checked nothing; an UNREADABLE directory is a third case and fails loudly) because the base side of a PR adding the first policy legitimately has none and both tools treat an empty policy set as fatal — the typo that tolerance permits is caught instead by `App.warnMissingPolicyDirs`, which warns at startup for a directory absent from the working tree. That skip is NOT silent in the report: it contributes one note per side (`skippedForNoPolicies` — `[base] [lint-kyverno#1] not linted: no policies in "policies/kyverno"`), because an unlinted side leaves every finding one-sided and the label contract reads a one-sided finding as introduced-by-this-change (`[target]`) or fixed-by-it (`[base]`) — so silence turned "pre-existing violations, newly detected" into "this PR introduced N violations", exactly on the PR the tolerance exists for. The note is per SIDE rather than one summary note about the asymmetry because that is what the fact is: reporting it in the same warning list and vocabulary as the findings needs no cross-side inference, and the mirror case (a PR DELETING the policies) is covered by the same line. Its cost is a warning badge on every affected app when a PR adds a first policy, since `ParseWarnings` has no severity tier — accepted, because the badge is not a false claim about that app while the silent report was. Parsing is split into pure `parseKyvernoReport`/`parseConftestReport` so the shapes the tools actually emit are pinned in unit tests from captured output. The kyverno parser emits one line PER resource (the `resources` list is schema-shaped — kyverno 1.18 fills one entry per result for both policy types, verified — so naming only the first would silently drop subjects if any producer grouped them) and surfaces `error` results marked `ERROR`: an error is kyverno failing to EVALUATE (broken CEL), which is invisible otherwise because kyverno stops at the first failing validation within a policy, pushing authors to split checks across many small policies. A resource whose CRD the cluster lacks is a DIFFERENT case — unmappable GVK, skipped with no result, no error and nothing on stderr — which is why `--continue-on-fail` is passed (without it one such document makes kyverno return zero bytes and discard every other finding) and why `case/lint-unmappable-kind` pins both halves. Two behaviors are load-bearing: the kyverno adapter takes the resolved context as a `Runner.KubeContext` FIELD (the `ARGOCDF_*` env vars exist because shell commands have no other channel) and REFUSES to run without one rather than defaulting to the ambient cluster, and it keeps `--cluster`/`--continue-on-fail` for the reasons the reference adapters document. Run order is fixed — shell commands, then kyverno, then conftest — because pflag cannot preserve cross-flag ordering and the order is report-visible. `case/lint-builtin` carries the identical fixture to `case/lint-two-adapters`, so their expectations differ only in the title line and in each linter's IDENTITY (`lint-kyverno#1`/`lint-conftest#1` against `lint#1`/`lint#2` — different flags, which is why `same-report-as` normalizes that token): that pins the built-ins as EQUIVALENT to the shell adapters, and `case/lint-builtin-cr` is what proves the context wiring (a CUSTOM RESOURCE policy needs the cluster to map the GVK; a workload policy passes even when that is broken).

Every command also gets the EFFECTIVE cluster selectors so cluster-aware adapters (`kyverno apply --cluster`, `kubectl --dry-run=server`) hit the cluster argocdf is diffing: `Runner.Env` (built in `Factory.CreateLintRunner(kubeContext)`, fed from `App.createLintRunner` — extracted so the client→runner wiring is unit-tested, since a call site passing `""` would otherwise satisfy every test while adapters silently fell back to the shell's cluster) carries `ARGOCDF_CONTEXT` — the RESOLVED context from `cluster.Client.ResolvedContext()` (`--context`, else the kubeconfig's `current-context`, computed once during `connect` by the pure `resolveContextName`) — and `ARGOCDF_KUBECONFIG` (`cfg.KubeconfigPath`, verbatim: it may be a path LIST). Two invariants: a value argocdf cannot resolve is OMITTED, never exported empty (so a cluster-aware adapter REQUIRES `ARGOCDF_CONTEXT` and fails when it is absent — carrying on would silently lint another cluster; both the README example and the e2e adapter do this, which is what makes the e2e suite fail if propagation regresses), and `childEnv` REMOVES inherited entries for these keys before appending argocdf's — which keeps the slice honest rather than fixing a precedence bug: `exec.Cmd` dedups `Env` keeping the LAST value, so appending alone already beats a stale env-configured `ARGOCDF_CONTEXT` (this is NOT the `HELM_*` hazard, which is about helm's own `HELM_*`-over-`XDG_*` precedence). Nothing to export ⇒ `cmd.Env` stays nil (inherit everything, byte-identical to pre-0.5.0). `KUBECONFIG` is deliberately NOT rewritten.

## Running the Tool

```bash
# Basic usage (auto-detects everything)
./argocdf

# Specify branches
./argocdf --base main --target feature-branch

# Different cluster/namespace
./argocdf --context prod-cluster --argocd-namespace argocd

# Apps-in-any-namespace (exhaustive list; globs and /regex/ allowed)
./argocdf --application-namespaces 'argocd,team-*'

# Scan all namespaces
./argocdf -A
```

### Output Formats

**Terminal output** (`--stdout`):
- `fields` (default) - Field-level changes with colors
- `summary` - Counts only, no diff details
- `unified` - Traditional unified diff format
- `none` - Suppress terminal output

**File output** (`-f/--file format[,option]:path`):
- `md-fields` - GitHub-flavored markdown with field-level diffs
- `md-unified` - Markdown with unified diff format
- `html-side-by-side` - Interactive HTML with side-by-side diff
- `unified` - Patch-compatible unified diff

Markdown formats accept the `split[=N]` option (options ride on the format segment, so paths with commas/colons stay intact): a report larger than N bytes (default 60000, under GitHub's 65,536-char comment cap) is written as multiple self-contained part files — `pr-comment.md`, `pr-comment.2.md`, ... — each with the upsert marker, a `part i/N` heading, and balanced `<details>`/fences. Apps stay whole within a part; an app that alone exceeds the limit is split at resource boundaries; a single oversized resource diff is truncated with a note. Packing lives in `internal/output/markdown.go` (`assembleParts`/`packBodies`).

```bash
# Quiet mode with markdown file output
./argocdf -q -f md-fields:pr-comment.md

# Split oversized markdown into PR-comment-sized parts
./argocdf -q -f md-unified,split:pr-comment.md

# Multiple file outputs
./argocdf -f md-fields:pr.md -f html-side-by-side:report.html

# Unified diff for patch workflows
./argocdf --stdout unified
./argocdf -f unified:changes.patch

# Summary only in terminal
./argocdf --stdout summary

# Use external diff tool (e.g., delta for side-by-side)
ARGOCDF_EXTERNAL_DIFF="delta --side-by-side" ./argocdf
```

## Configuration & Environment Variables

Configuration flows through Cobra flags into `internal/config.Config`. Every flag is also settable via an environment variable named `ARGOCDF_<FLAG>` (flag name upper-cased, dashes → underscores), e.g. `--repo-dir` → `ARGOCDF_REPO_DIR`.

This is wired by `bindEnv` in `cmd/argocdf/main.go`, which runs first in `runMain`. It uses **viper `AutomaticEnv()`** with prefix `ARGOCDF` and a `-`→`_` key replacer as a pure env lookup — `viper.BindPFlags` is not called because it isn't needed (env values are applied directly through `pflag.Set`, so they are parsed by each flag's own type and invalid values fail fast with a typed error). In this setup `v.IsSet(name)` is true only when the env var is actually set (non-empty).

Precedence — **explicit flag > environment variable > default** — is enforced by the `f.Changed` guard in `bindEnv`: flags the user passed on the command line are skipped, so their env vars never override them. That guard is the load-bearing line; keep it if you refactor this.

Two variables are read directly (no flag equivalent): `ARGOCDF_EXTERNAL_DIFF` (`internal/output/terminal.go`) and `KUBECONFIG` (`internal/config/detect.go`).

## Test Data

`testdata/` contains real ArgoCD Application manifests for testing:
- `apps.yaml` - Application manifests
- `crd.yaml` - CRD definitions
