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
4. **Filter** → Match apps to changed files in git diff
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
| `internal/render` | Helm/Kustomize manifest rendering, chart fetching via ArgoCD's chart client |
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

- **Interface-based design**: `Renderer`, `Writer`, `Differ` allow pluggable implementations
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

## Render Engines (`--renderer`)

Two engines implement the app-side `applicationRenderer` seam (`internal/app/app.go`), selected by `--renderer` / `ARGOCDF_RENDERER` in `Factory.CreateRenderFactory`:

- **`native`** (default) — argocdf's own pipeline (`internal/render` `Factory`/`HelmRenderer`/`KustomizeRenderer`): hand-translates `ApplicationSource` fields to helm/kustomize CLI flags.
- **`argocd`** — `internal/render/argocd.go` wraps ArgoCD's `reposerver/repository.GenerateManifests` (the `argocd app diff --local` code path) for exact ArgoCD parity: ArgoCD does source-type dispatch, the complete helm/kustomize option translation (incl. `--include-crds` by default, which native lacks), `ARGOCD_APP_*` build-env substitution, `.argocd-source*.yaml` overrides, and dependency builds into an isolated temp helm home (so `--helm-add-repos`/`--helm-skip-refresh` don't apply — their usage strings say "(native renderer only)" and config warns when they're combined with this engine). argocdf still owns worktrees, remote-chart fetching (through ArgoCD's `util/helm.Client.ExtractChart` wrapped in the persistent chart cache — see `internal/render/chartfetch.go`), `$ref` checkout (registered in a `TempPaths` keyed by normalized repo URL — GenerateManifests never clones), and the render cache (`KeyOptions.Renderer` keeps engine entries separate). Integration constraints that matter when touching this file: `q.Repo` and `q.ApplicationSource` must be non-nil, the source is deep-copied because GenerateManifests mutates it, calls are serialized per appPath (dependency builds write `charts/`/`Chart.lock` into the worktree), `isLocal=false` is what ISOLATES the helm home (in ALL `--repo-creds` modes) — but that isolation only holds because `NewArgoCDRenderer` SCRUBS inherited `HELM_*` env vars (ArgoCD APPENDS its temp-home vars to `os.Environ()`, and Go children like helm resolve the FIRST occurrence of a duplicated key, so an exported `HELM_CONFIG_HOME` would silently win) — and the revision param feeds `ARGOCD_APP_REVISION*`. Dependency logging is routed centrally by `configureDependencyLogging` (`cmd/argocdf/main.go`): ArgoCD's global logrus re-emits through argocdf's logger under an `argocd` prefix (errors-only unless `--verbose`), client-go's klog rides the slog bridge under `client-go` in `--verbose` (klog encodes V(n) as slog level -n — `klogHandler` drops V>4, clamps the rest to DEBUG — unmapped negative levels would otherwise render as INFO — and demotes context-canceled watch errors to DEBUG: the reflector reports argocdf's own deliberate informer shutdown at error severity) and is discarded otherwise, and the per-exec tracer (a FRESH logrus logger per command, unhookable) gets `ARGOCD_LOG_FORMAT=text` plus, without `--verbose`, `ARGOCD_LOG_LEVEL=error` — explicit values always respected.

### Repository Credentials (`--repo-creds cluster|local|none`)

All three modes run the identical pipeline; they differ only in what fills the credential fields of `render.RenderOptions` (four `Repository`/`RepoCreds` lists + a `ResolveRepo` closure; `none` leaves them empty — render code is mode-blind):

- **cluster** (default): `internal/cluster/repocreds.go` reads ArgoCD's repository secrets via ArgoCD's own `util/db`/`util/settings` from `--argocd-namespace`. Gotchas encoded there: a preflight `Secrets List (Limit:1)` probe MUST run before the settings manager's first use (its informer `WaitForCacheSync` has no internal timeout — forbidden RBAC would hang forever), and the manager context is bounded (~30s; the synced informer indexer keeps serving reads after it ends — client-go's reflector logs that expected watch cancellation through klog, handled by `configureDependencyLogging` in main). Load failures are FATAL in app.initialize with a message naming the other modes.
- **local**: `internal/helmconfig` parses repositories.yaml (classic creds are inline there) into the same lists and sets `HELM_REGISTRY_CONFIG` to the user's registry config (resolved via `helm env`) — an explicit helm env var beats the isolated `HELM_CONFIG_HOME`-derived default, so OCI logins (including credential helpers) pierce the isolated homes read-only. The path also travels as `RepoCredentials.HelmRegistryConfig` → `RenderOptions.HelmRegistryConfig`, because the argocd engine's env scrub would otherwise remove it (the engine re-installs it and then owns NO auth file — local lists have no OCI creds, so nothing ever logs in and the strip is a no-op). Chart fetches that resolve inline OCI creds anyway (hand-crafted repositories.yaml entries) are stripped too — no seeding, auth rides the user's registry config — so `helm registry login` never runs in local mode either.
- The argocd engine composes `q.Repos`/`q.HelmRepoCreds` per source with ArgoCD's `source.IsOCI()` gate (mirroring controller/state.go:300-315, including its degradations; note `type: helm` + `enableOCI` repos ride the HELM list UNCONDITIONALLY — that's how git-path charts get their private OCI dependency creds) and resolves `q.Repo` through `ResolveRepo`.
- OCI registry auth NEVER goes through `helm registry login` (`internal/render/registryauth.go`): helm's ORAS credential store auto-detects platform native stores (any `docker-credential-*` helper in PATH — on macOS every login would land in the SHARED system keychain, where concurrent renders' login→build→logout cycles race: `-25299` duplicate items, and 401s when one render's logout deletes the credential another's dependency build is using; a fresh isolated config file does NOT prevent this). Instead the engine owns a per-run registry config (seeded with a placeholder entry — a "configured" file is what disables the native-store detection — plus every OCI credential from the lists, written argocdf-side under a mutex with atomic renames, 0600, removed via `Cleanup()`), points `HELM_REGISTRY_CONFIG` at it, and STRIPS username/password from OCI-flavored entries in `q.Repos`/`q.HelmRepoCreds` and from the repo handed to the chart client, so ArgoCD's login gates (username AND password non-empty) never fire and pulls read the file. Classic (non-OCI) repos keep their creds — they flow via `helm repo add`/`pull` argv, not the registry config. Auth keys are registry HOSTS (same-host repos with different creds collapse; upstream has the same granularity).
- Chart fetches (`chartfetch.go`): credential-resolution failures are LOUD (no anonymous fallback — a swallowed error would resurface as a misleading 401 from helm); `ExtractChart` runs interruptibly (goroutine + background drain — ArgoCD's chart client takes no context); a cache hit skips fetching and auth entirely (pinned versions are immutable — intentional); cache-backed chart dirs are COPIED to a private temp dir before `GenerateManifests` (it mutates appPath, and `chartDepMutex` is process-local so it cannot protect the shared persistent cache from concurrent argocdf PROCESSES).
- External git repositories: renderable sources and `$ref` sources living in OTHER repos are cloned at their `TargetRevision` via `git.CloneWithCreds` — HTTP(S) basic-auth/bearer credentials ride an `http.extraheader` passed through `GIT_CONFIG_*` env vars (never argv); one clone per (URL, revision) per app render (`externalrepo.go`). SSH remotes use ambient git config.
- Native renderer: chart-SOURCE fetches share all of the above, but its dependency builds run in the LOCAL helm env — private dependency auth there comes from the user's own helm setup, not cluster secrets (the argocd engine is the one that covers dependency auth).
- Both engines use `app.InstanceName(ArgoCDNamespace)` — `"<ns>_<name>"` outside the control-plane namespace — for `AppName`/the default helm release name, and the argocd engine sets `ProjectName` (feeds `ARGOCD_APP_PROJECT_NAME`); remote-chart relative helm value files resolve against (and are contained to) the EXTRACTED chart dir, not the worktree.
- IMPORT BOUNDARY: only `internal/cluster` and `internal/app` may import `util/db`/`util/settings`; `internal/render` sees only the lists + closure.

Namespace flags: `--argocd-namespace` (control-plane; secrets + default app listing) and `--application-namespaces` (EXHAUSTIVE list, glob + `/regex/` entries matched via ArgoCD's `glob.MatchStringInList` — deliberately WITHOUT `IsNamespaceEnabled`'s control-plane short-circuit; all-literal lists are served per-namespace so namespace-scoped RBAC suffices). `-A` normalizes to `["*"]` in config.WithDefaults.

The trade-off mirrors the types-import decision: ~85MB extra binary for structurally-eliminated behavior drift. On argo-cd version bumps, expect `GenerateManifests` signature churn (loud, compile-time) and re-check that the in-tree gitops-engine fork hasn't diverged from the published module argocdf links.

### Automatic Helm Dependency Management

When rendering local Helm charts, argocdf automatically runs `helm dependency build` if the chart has a `Chart.yaml` with a `dependencies:` section. Helm is smart enough to skip already-fetched dependencies, so this is safe to run unconditionally.

This ensures charts with dependencies (like umbrella charts) render correctly without manual setup.

Caveat: `helm dependency build` never adds or refreshes classic HTTP(S) chart repositories — their index must already be in the local helm cache, which on a fresh CI runner it never is (fails with "no cached repository"/"no repository definition"). The opt-in `--helm-add-repos` flag makes argocdf register those repos first, deduplicated per URL per run: a URL already registered under any name is only refreshed (helm matches dependency repos by URL, so no new entry is written), and only unknown URLs are added, under hash-derived names that can never clobber a user's entry. It still mutates local helm state either way — index caches are refreshed and unknown URLs get new repositories.yaml entries — which is why it is off by default; the missing-repo error message points at it.

## Linting Rendered Manifests (`--lint`)

`--lint "shell command"` (repeatable; `--lint-timeout`, default 5s) pipes each affected app's rendered multi-doc YAML into the command's stdin via `sh -c`, per side. Each side's command runs with that side's ephemeral worktree as its working directory, so repo-relative policy paths resolve to the branch's own version of the files (a PR changing a policy lints each side with its own policy). Every non-empty stdout line becomes a warning appended to `ManifestSetDiff.ParseWarnings` with the existing `[base]`/`[target]` labels (`diff.LabelSide`), so all writers, badges, and split-packing handle lint findings with zero writer-specific code. The label semantics double as the diff: `[base]`-only = fixed by the PR, `[target]`-only = introduced, both = pre-existing.

The runner lives in `internal/lint` and is spliced into `processOneApp` (`internal/app/app.go`) after `DiffManifests`; sides with an empty render (new/deleted apps) are skipped. Error contract: the process outcome is the only health signal — stdout lines are always kept, and spawn failure, timeout, or exit ≠ 0 appends one non-fatal self-identifying warning line. Stdout content never influences error detection; tools that exit non-zero on findings (kyverno, conftest) are expected to sit behind a jq adapter that exits 0. Empty tool output is ambiguous and must be resolved by the tool's EXIT CODE, never by a `jq -rn 'input'` that dies on empty stdin: kyverno legitimately prints nothing (exit 0) when no rendered resource matches a policy, so the jq-dies idiom attaches a spurious lint-failure warning to every ConfigMap- or CR-only app (it had polluted 23 of 35 e2e cases). The README's example adapter and the e2e adapter both branch on the exit code instead. `ARGOCDF_LINT` can carry only a single command (StringArray-via-env limitation); repeat `--lint` for several.

Every command also gets the EFFECTIVE cluster selectors so cluster-aware adapters (`kyverno apply --cluster`, `kubectl --dry-run=server`) hit the cluster argocdf is diffing: `Runner.Env` (built in `Factory.CreateLintRunner(kubeContext)`, fed from `App.createLintRunner` — extracted so the client→runner wiring is unit-tested, since a call site passing `""` would otherwise satisfy every test while adapters silently fell back to the shell's cluster) carries `ARGOCDF_CONTEXT` — the RESOLVED context from `cluster.Client.ResolvedContext()` (`--context`, else the kubeconfig's `current-context`, computed once during `connect` by the pure `resolveContextName`) — and `ARGOCDF_KUBECONFIG` (`cfg.KubeconfigPath`, verbatim: it may be a path LIST). Two invariants: a value argocdf cannot resolve is OMITTED, never exported empty (so a cluster-aware adapter REQUIRES `ARGOCDF_CONTEXT` and fails when it is absent — carrying on would silently lint another cluster; both the README example and the e2e adapter do this, which is what makes the e2e suite fail if propagation regresses), and `childEnv` REMOVES inherited entries for these keys before appending argocdf's — a duplicated key resolves to its FIRST occurrence in Go children, so appending would let a stale env-configured `ARGOCDF_CONTEXT` beat `--context` (same hazard as the `HELM_*` scrub in the argocd renderer). Nothing to export ⇒ `cmd.Env` stays nil (inherit everything, byte-identical to pre-0.5.0). `KUBECONFIG` is deliberately NOT rewritten.

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
