# argocdf - ArgoCD Diff Tool

`argocdf` shows manifest diffs for ArgoCD applications affected by your PR changes. It supports the apps-of-apps pattern with recursive discovery.

> **Note:** argocdf aims to reproduce how ArgoCD renders and diffs your applications, and reuses parts of ArgoCD's codebase to do so (which is why the binary isn't tiny). Still, it's not a perfect replica — some behaviors and features differ. See [DIFFERENCES.md](DIFFERENCES.md) for a detailed comparison with ArgoCD's implementation.

## `argocd app diff` vs argocdf

ArgoCD ships its own diff command, so why argocdf?

|                      | `argocd app diff`                                                     | argocdf                                                                                      |
|----------------------|-----------------------------------------------------------------------|----------------------------------------------------------------------------------------------|
| **Needs**            | A running, reachable argocd-server + `argocd login`                   | Only kubectl access to the cluster                                                           |
| **Scope**            | ONE app, named by you                                                 | ALL apps affected by your git change, discovered automatically (incl. apps-of-apps children) |
| **Compares**         | Desired state vs the app's **live** target state                      | Base branch vs target branch (what your PR will change)                                      |
| **`--local` mode**   | Renders a pre-checked-out path with your helm config, no repo secrets | `--repo-creds=local` reproduces exactly that credential behavior                             |
| **Repo credentials** | The server-side repo-server resolves them from ArgoCD's secrets       | `--repo-creds=cluster` (default) reads the same secrets through the same ArgoCD code         |
| **Output**           | Terminal diff                                                         | Terminal, GitHub markdown (PR-comment-ready, split-aware), unified patch, interactive HTML   |

In short: `argocd app diff` answers "how does this one app differ from the cluster right now"; argocdf answers "what will this PR change, across every affected app" — and runs fine in CI with nothing but a kubeconfig.

## Features

- **Auto-detection**: Automatically detects repository path, branches, and cluster version
- **Multi-source support**: Handles applications with `spec.source` and `spec.sources[]` configurations
- **Helm rendering**: Renders Helm charts (local and remote repositories, including OCI)
- **Kustomize rendering**: Renders Kustomize directories
- **Apps-of-apps**: Recursively discovers and diffs child applications from rendered manifests
- **Multiple outputs**: Colored terminal, GitHub-flavored markdown, unified diff, and interactive HTML report
- **Semantic diffing**: Identifies added, removed, and modified resources by kind/name/namespace
- **Parallel rendering**: Renders affected applications concurrently from ephemeral git worktrees
- **Persistent cache**: Content-addressed render/chart cache speeds up repeat runs
- **CI-friendly**: `diff`-style exit codes and stable PR-comment markers for automated pipelines

## How It Works

1. **Connects to cluster**: Uses kubeconfig to connect to the Kubernetes cluster
2. **Fetches applications**: Lists ArgoCD Applications from the specified namespace(s)
3. **Analyzes changes**: Compares git branches (from their merge base) to find changed files
4. **Filters affected apps**: Identifies applications whose source paths have changes (or which declare the changed path — see [Dependencies outside an app's source path](#dependencies-outside-an-apps-source-path))
5. **Renders manifests**: For each affected app, renders both sides from ephemeral worktrees (the merge base and the target branch tip) — the user's working tree is never touched
6. **Computes diffs**: Compares rendered manifests to identify changes
7. **Recursive discovery**: If diffs contain new or modified Application CRDs, adds them to the queue (see below)
8. **Outputs results**: Displays colored terminal output and/or generates HTML report

### Apps-of-Apps Rendering Order

A PR can change parents and children of an apps-of-apps hierarchy at the same time, and a parent's change may itself rewrite a child's spec (e.g. its Helm values). The children a parent manages — and the specs it gives them — are only knowable by *rendering the parent*, so there is no dependency graph to sort up front. Instead, argocdf processes the queue in **waves** and corrects mis-ordered renders by requeueing:

1. **Wave 0** renders every directly affected app concurrently, using its live cluster spec.
2. After the wave completes, each app's rendered output is scanned (on both sides) for Application CRDs. When a parent's diff shows a child was **added**, the child is enqueued for the next wave. When a child was **modified**, the child is enqueued — or, if it already rendered this wave with its cluster spec, **requeued**: its stale result is discarded and it re-renders in the next wave.
3. A discovered child renders its base side with the spec extracted from the parent's *merge-base* render and its target side with the spec from the parent's *target-branch* render — so children always reflect the values the PR actually gives them, not what the cluster currently has.
4. Waves repeat until the queue is empty. Multi-level chains (parent → child → grandchild) propagate naturally, one level per wave.

Two guards bound the recursion: `--max-depth`, and a spec-identity check that refuses to requeue an app with the same specs it was already processed with — this is what terminates self-managing root apps and mutually referencing apps.

**Concurrency model**: `--concurrency` parallelizes rendering only *within* a wave. The wave boundary is a hard barrier — discovery, queueing, and requeueing run single-threaded between waves — so parallel rendering cannot reorder parent/child processing or race the recursion guards. Concurrent renders that share a chart directory serialize `helm dependency build` behind a per-chart lock. This invariant is pinned by `TestProcessApplicationsWaveBarrier` in `internal/app/app_test.go`.

### Dependencies outside an app's source path

argocdf selects applications by matching changed files against each app's `source.path`, which cannot see a source's build graph: a kustomize overlay whose base is `../shared`, a helm chart that includes a file from a sibling directory, a values file elsewhere in the repo. A change to one of those is real but invisible, and the app is reported as unaffected.

ArgoCD has the same blind spot in its webhook refresh filter, and solves it with an annotation - which argocdf honors, resolved by ArgoCD's own code so both tools select the same apps:

```yaml
metadata:
  annotations:
    # this app is generated by the base AND by its own directory
    argocd.argoproj.io/manifest-generate-paths: ../kustomize-base;.
```

- Entries are separated by `;`. A leading `/` is relative to the **repo root**; anything else is joined to that source's `path`. Globs use `filepath.Match`, which does **not** cross `/`.
- The annotation **replaces** the default rather than extending it, in both tools. Declaring only `../kustomize-base` stops the app from reacting to changes in its own directory - include `.` unless that is what you want.
- Multi-source apps resolve the same annotation per source, so `.` means "each source's own path".
- An annotation that declares nothing usable - present but empty, or separators only (`;`, `;;`) - is treated as absent: argocdf falls back to path matching, so a typo degrades to the default instead of removing the app from every report. ArgoCD would refresh always instead.

**Helm apps need care.** Two shapes bite:

- The declaration replaces argocdf's helm file handling too — **every** value file and `fileParameter`, not just `$values/...` ones. Without an annotation, a change to a `$values/...` file, to an entry that escapes the source path (`../shared/vals.yaml`), or to a repo-root-absolute one (`/config/prod.yaml`) is attributed to each app that reads it. Once you declare paths, only the declared paths count — so list those files too (`apps/chart;/config/env/prod.yaml`) or changes to them stop being reported. ArgoCD documents the same caveat for apps referencing external helm values files.
- A `ref:` source (and a remote chart source) has **no path**, so a relative entry resolves against the repo root: `.` on such a source matches *every* file in the repository and the app becomes always-affected. Name paths explicitly instead of using `.` on multi-source helm apps.

If no source of an annotated app lives in the repo being diffed — a remote-chart-only app, say — the declaration cannot resolve and the app can never be reported affected. argocdf logs a warning in that case, because the app would otherwise silently vanish from every report.

**Apps-of-apps: the annotation gates initial selection only.** Discovery is annotation-blind — children are enqueued from their *parent's rendered* Application CRs, not from the changed-file list — which cuts both ways:

- An annotated **child** keeps spec-change detection through its parent (the parent renders, the child re-renders with its new spec) even when its own annotation wouldn't have matched. It only loses detection of changes to its *own* content paths if the declaration forgets them.
- An annotated **parent** gates its whole subtree: if its declaration misses the catalog path, the parent isn't rendered, so added, modified and *removed* children all disappear from the report with it. Annotating an apps-of-apps parent narrows more than that one app.

Note the defaults differ, so the annotation does opposite work in each tool: ArgoCD refreshes every app in the repository on any commit and uses the annotation to *narrow* that (and to reuse cached manifests); argocdf renders only what the paths match and uses it to *widen*. Without a declaration there is no way for argocdf to attribute a `../relative` base change to the app that consumes it - the alternative would be rendering every application on every run.

## Multi-Source Applications

argocdf supports ArgoCD's multi-source feature where applications can have multiple sources, including `ref:` sources for external values:

```yaml
spec:
  sources:
    - chart: my-chart
      repoURL: https://charts.example.com
      helm:
        valueFiles:
          - $values/envs/prod/values.yaml  # References the 'values' source below
    - repoURL: https://github.com/org/config
      ref: values  # This source provides values files
```

## Installation

Requirements:

- `helm` binary in PATH (for Helm chart rendering)
- `kustomize` binary in PATH (for Kustomize rendering)
- Access to a Kubernetes cluster with ArgoCD Applications
- Go 1.24+ (only if installing via `go install` or building from source)

### Homebrew

```bash
brew install rgeraskin/homebrew/argocdf
```

### Go

```bash
go install github.com/rgeraskin/argocdf/cmd/argocdf@latest
```

### Binary download

Grab a prebuilt archive for your OS/arch from the [releases page](https://github.com/rgeraskin/argocdf/releases), then extract `argocdf` into a directory on your `PATH`.

### From source

```bash
git clone https://github.com/rgeraskin/argocdf.git
cd argocdf
mise run build   # produces ./argocdf
```

## Usage

argocdf runs just as well in CI as in your local terminal. For GitHub Actions, see [examples/github-actions](examples/github-actions/README.md) for a ready-to-adapt workflow.

```bash
# Basic usage (auto-detects everything):
# Uses current k8s context, argocd namespace, and current branch
# Also, repoURL is auto-detected from the cloned repo
argocdf

# Specify branches explicitly
argocdf --base main --target feature/new-service

# Use a different Kubernetes context
argocdf --context my-cluster

# Scan all namespaces for ArgoCD applications
argocdf --all-namespaces

# Apps-in-any-namespace: scan exactly these namespaces
# (globs and /regex/ work; add 'argocd' explicitly if wanted)
argocdf --application-namespaces 'argocd,team-*'

# Private repos without cluster secret access: use your helm logins
argocdf --repo-creds local

# Quiet mode with markdown file output
argocdf -q -f md-fields:pr-comment.md

# Multiple file outputs
argocdf -f md-fields:pr.md -f html-side-by-side:report.html

# Split oversized markdown into PR-comment-sized parts
# (pr-comment.md, pr-comment.2.md, ...)
argocdf -q -f md-unified,split:pr-comment.md

# Unified diff for patch workflows
argocdf --stdout unified
argocdf -f unified:changes.patch

# Summary only in terminal
argocdf --stdout summary

# Debug logging (troubleshoot detection, filtering, rendering)
argocdf -v

# Use external diff tool for side-by-side view
ARGOCDF_EXTERNAL_DIFF="delta --side-by-side" argocdf
```

## Flags

### Kubernetes Flags

| Flag                       | Short | Description                                                                                                                                                                                             | Default           |
|----------------------------|-------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------|
| `--kubeconfig`             | `-k`  | Path to kubeconfig file                                                                                                                                                                                 | `~/.kube/config`  |
| `--context`                |       | Kubernetes context to use                                                                                                                                                                               | (from kubeconfig) |
| `--argocd-namespace`       |       | ArgoCD control-plane namespace: repository secrets/settings are read here, and Applications are listed here unless `--application-namespaces` is set                                                    | `argocd`          |
| `--application-namespaces` |       | Comma-separated list of namespaces to scan for Applications — **exhaustive** when set (include the ArgoCD namespace explicitly if wanted); entries may be literal names, globs (`team-*`), or `/regex/` | (unset)           |
| `--all-namespaces`         | `-A`  | Scan all namespaces (same as `--application-namespaces='*'`)                                                                                                                                            | `false`           |

An all-literal `--application-namespaces` list is served with one namespaced read per entry, so strictly namespace-scoped RBAC suffices; any glob or `/regex/` entry requires cluster-wide list permission (as `-A` always did).

### Git Flags

| Flag         | Short | Description                             | Default            |
|--------------|-------|-----------------------------------------|--------------------|
| `--repo-dir` | `-r`  | Path to git repository                  | Current directory  |
| `--repo-url` |       | Repository URL for matching ArgoCD apps | Auto-detected      |
| `--base`     |       | Base branch for comparison              | `main` or `master` |
| `--target`   |       | Target branch for comparison            | Current HEAD       |

### Rendering Flags

| Flag                          | Description                                                                                                                      | Default       |
|-------------------------------|----------------------------------------------------------------------------------------------------------------------------------|---------------|
| `--kube-version`              | Kubernetes version for rendering                                                                                                 | Auto-detected |
| `--kustomize-enable-helm`     | Enable Helm chart inflation via kustomize                                                                                        | `false`       |
| `--kustomize-build-options`   | Additional kustomize build options (space-separated)                                                                             | (none)        |
| `--kustomize-load-restrictor` | Load restrictor mode (e.g., `LoadRestrictionsNone`)                                                                              | (none)        |
| `--repo-creds`                | Repository credential source: `cluster` (ArgoCD repository secrets), `local` (your helm config), `none` (anonymous) — see below | `cluster`     |
| `--no-api-versions`           | Do not pass cluster-discovered API versions to helm via `--api-versions`                                                         | `false`       |

**Rendering runs through ArgoCD's own repo-server code** (`reposerver/repository.GenerateManifests` — the same code path behind `argocd app diff --local`), so option translation matches ArgoCD exactly: helm runs with `--include-crds` (CRDs from `crds/` appear in diffs), all `spec.source.helm`/`kustomize` fields are honored, `$ARGOCD_APP_*` build-env substitution works, `.argocd-source*.yaml` overrides are merged, and helm dependencies are built in an isolated temp helm home — your helm config is never touched, and dependency repos from `Chart.yaml` are registered there automatically, so fresh CI runners need no helm setup. YAML is re-serialized from ArgoCD's parsed objects (key order may differ from raw helm output; diffs are computed semantically so this does not affect results). Set `ARGOCD_LOG_LEVEL=info` to see ArgoCD's per-command exec traces when debugging renders.

### Repository Credentials (`--repo-creds`)

Private chart repositories and registries authenticate through one of three credential sources — all three run the identical render pipeline and differ only in where credentials come from:

- **`cluster`** (default) — read ArgoCD's repository secrets and credential templates from `--argocd-namespace`, using ArgoCD's own `util/db` code, and feed them into rendering exactly as the application controller does. Private OCI and classic chart sources — and private chart **dependencies** (a chart's own `Chart.yaml` requirements) — then work with zero local helm setup. RBAC needed: `get,list,watch` on Secrets and ConfigMaps in the ArgoCD namespace (note: like ArgoCD's own components, argocdf lists ALL secrets in that namespace — the repository secrets are found by label). A read failure aborts the run with a message naming the other modes; a cluster without repository secrets simply renders credential-less.
- **`local`** — use your own helm configuration: classic repository entries from repositories.yaml (credentials are stored inline there by `helm repo add --username … --password …`), and your `helm registry login` state for OCI registries — including credential helpers such as the macOS keychain. Nothing is ever written to your helm config. Setup contract: `helm registry login` for private OCI registries, `helm repo add` with credentials for private classic repos (it serves purely as a credential store — no `helm repo update` needed); public repositories need nothing. This is the mode for users whose RBAC does not include the ArgoCD namespace.
- **`none`** — render anonymously (public repositories only).

In both credentialed modes, remote chart downloads run through ArgoCD's own chart client (the repo-server's fetch path), so username/password, TLS client certificates (from the repository secret in `cluster` mode, from `certFile`/`keyFile` entries in `local` mode), `insecure`, and — in `cluster` mode — proxy settings from the resolved repository all apply. Custom CA bundles are not supported: ArgoCD serves those from its certificate database, which a CLI run does not have. Credentials also authenticate git clones of external repositories referenced by multi-source apps and apps-of-apps children (HTTPS basic auth and bearer tokens; SSH remotes keep using your ambient git configuration).

Two credential-adjacent notes: a **chart-cache hit skips fetching — and therefore authentication — entirely** (pinned chart versions are immutable, so the cached content is identical; use `--no-cache` to force a re-fetch), and switching `--repo-creds` never invalidates the cache.

### Output Flags

| Flag              | Short | Description                                                                        | Default  |
|-------------------|-------|------------------------------------------------------------------------------------|----------|
| `--stdout`        |       | Terminal output format: `fields`, `summary`, `unified`, `none`                     | `fields` |
| `--file`          | `-f`  | File output in `format[,option]:path` (can be repeated)                            | (none)   |
| `--quiet`         | `-q`  | Suppress terminal output (same as `--stdout none`)                                 | `false`  |
| `--verbose`       | `-v`  | Enable debug logging (config resolution, cache hits, per-app processing) to stderr | `false`  |
| `--context-lines` | `-U`  | Number of context lines in unified diff output (-1 for unlimited)                  | `3`      |

**File output formats:**
- `md-fields` - GitHub-flavored markdown with field-level diffs
- `md-unified` - Markdown with unified diff format
- `html-side-by-side` - Interactive HTML with side-by-side diff
- `unified` - Patch-compatible unified diff

**File output options** (appended to the format, comma-separated):
- `split[=N]` - split markdown output into parts of at most `N` bytes (default `60000`, safely under GitHub's 65,536-char comment cap). Only valid for `md-fields` and `md-unified`.

```bash
argocdf -f md-unified,split:pr-comment.md
argocdf -f md-fields,split=30000:pr-comment.md
```

When the report fits in one part, the output is a single file, identical to running without `split`. Otherwise parts are written to `pr-comment.md`, `pr-comment.2.md`, `pr-comment.3.md`, … — each a self-contained document (upsert marker, `part i/N` heading, balanced `<details>` blocks and code fences) that CI can post as its own PR comment.

An application's report is kept in a single part; only an app that alone exceeds the limit is split across parts at resource boundaries, and only a single resource diff larger than a whole part gets truncated (with a note).

The summary and footer land on the last part. Leftover part files from a previous, larger run are removed automatically.

### Recursion Flags

| Flag             | Description                    | Default |
|------------------|--------------------------------|---------|
| `--no-recursive` | Disable apps-of-apps recursion | `false` |
| `--max-depth`    | Maximum recursion depth        | `10`    |

### Concurrency Flags

| Flag            | Description                                           | Default        |
|-----------------|-------------------------------------------------------|----------------|
| `--concurrency` | Applications to render in parallel (`1` = sequential) | Number of CPUs |

### Lint Flags

| Flag              | Description                                                          | Default |
|-------------------|----------------------------------------------------------------------|---------|
| `--lint-kyverno`  | Lint with the kyverno policies in DIR (can be repeated)              | none    |
| `--lint-conftest` | Lint with the rego policies in DIR (can be repeated)                 | none    |
| `--lint`          | Shell command that lints rendered manifests (can be repeated)        | none    |
| `--lint-timeout`  | Timeout for each lint invocation                                     | `10s`   |

For kyverno and conftest there is nothing to write:

```bash
argocdf --lint-kyverno policies/kyverno --lint-conftest policies/conftest
```

argocdf runs the tool itself and parses its report, so there is no shell pipeline, no `jq`, and none of the ways an adapter is usually written wrong (see the exit-code trap below). `kyverno apply` is pointed at the cluster argocdf is diffing automatically — it refuses to run rather than fall back to the ambient one. The directory is repo-relative and resolved per side, so a PR that changes a policy lints each side with its own version; a side where it is absent (the base of a PR adding the first policy) is skipped rather than failed, and a directory missing from your working tree is warned about at startup so a typo does not read as "no findings". Order is fixed and report-visible: `--lint` commands, then kyverno, then conftest.

`--lint` remains the escape hatch for any other tool. Each command receives an application's rendered multi-doc YAML on stdin (via `sh -c`) and emits findings as **one warning per stdout line**. Both sides are linted separately, and each side's command runs **with that side's checkout as its working directory** — a repo-relative policy path (like `policies/` below) resolves to the policy files as of that branch, so changing a policy in a PR is itself reflected in the lint results. Every finding lands in the report's warning block with the same side labels used for parse warnings:

- `[base]`-only — the violation existed on the base branch and this change **fixes** it
- `[target]`-only — this change **introduces** the violation
- both sides — pre-existing, untouched by this change

The exit code is the only health signal: stdout lines are always reported as warnings, and a spawn failure, timeout, or exit ≠ 0 adds one non-fatal `lint "<command>": ...` warning line. That warning echoes a truncated prefix of the command, so don't embed secrets in the command text — pass them via the environment or files instead. Tools like kyverno and conftest exit non-zero when policies fail (normal operation), so end the pipeline in an adapter — typically `jq`, which also normalizes any tool's output to line-per-finding.

Lint commands inherit argocdf's environment plus two variables naming the cluster argocdf is diffing, so a cluster-aware adapter (`kyverno apply --cluster`, `kubectl apply --dry-run=server`) can validate against exactly that cluster instead of whatever the invoking shell happens to point at: **`ARGOCDF_CONTEXT`** is the resolved kube context (`--context` when given, otherwise the kubeconfig's `current-context` — no fallback logic needed in the adapter) and **`ARGOCDF_KUBECONFIG`** is the kubeconfig argocdf itself read (`--kubeconfig`, else `$KUBECONFIG`, else `~/.kube/config`; it may be a `:`-separated list, passed verbatim). Neither is ever exported empty: a value argocdf cannot resolve is simply absent.

A cluster-aware adapter should **require** `ARGOCDF_CONTEXT` and fail when it is missing, not carry on without it. Continuing means the tool falls back to the ambient context and reports findings about a *different* cluster than the one under review — and in the report that is indistinguishable from a correct result, which is worse than no lint at all:

```bash
if [ -z "${ARGOCDF_CONTEXT:-}" ]; then
  echo "ARGOCDF_CONTEXT not set: refusing to lint against an unknown cluster"
  exit 1
fi
kubectl apply --dry-run=server -f - --context "$ARGOCDF_CONTEXT"
```

`ARGOCDF_KUBECONFIG` is a different matter and stays optional: absent means argocdf used the ambient kubeconfig, which the adapter already inherited. Offline linters (conftest, kubeconform against local schemas) need neither variable — the requirement belongs only to the steps that actually consult a cluster.

Note that argocdf does **not** rewrite `KUBECONFIG` for lint commands: your own setting is left exactly as it is, and the resolved path is offered alongside it under `ARGOCDF_KUBECONFIG`. A tool that has no context flag can still be steered by writing a stub kubeconfig that sets only `current-context` and prepending it to `KUBECONFIG` — a kubeconfig merge takes `current-context` from the first file that sets one, while clusters, users and auth plugins keep coming from your real config:

```bash
stub=$(mktemp)
printf 'apiVersion: v1\nkind: Config\ncurrent-context: %s\n' "$ARGOCDF_CONTEXT" >"$stub"
export KUBECONFIG="$stub:${ARGOCDF_KUBECONFIG:-$HOME/.kube/config}"
```

Empty tool output is ambiguous, and getting it wrong is the most common way an adapter goes bad in both directions: **resolve it with the tool's exit code, never by making `jq` fail on empty input.** A clean run and a crashed run can both print nothing — kyverno prints nothing (exit 0) whenever no rendered resource matches a policy's `matchConstraints`, which for a report full of ConfigMaps or Application CRs is the normal case. An adapter that treats empty as failure (the `jq -rn 'input | ...'` idiom, which exits non-zero when no JSON arrives) then attaches a spurious lint-failure warning to every such application, drowning real findings; one that ignores exit codes entirely reports a crashed tool as "clean". Branch explicitly: output present → parse it; empty and exit 0 → say nothing; empty and exit ≠ 0 → print one self-identifying failure line (or just exit non-zero and let argocdf's own warning carry it).

A worked example with a tool that has no built-in flag — "will the cluster even accept these manifests?", via a server-side dry run (schema, admission, missing CRDs):

```bash
argocdf --lint ': "${ARGOCDF_CONTEXT:?}"; err=$(kubectl apply --dry-run=server \
  --context "$ARGOCDF_CONTEXT" -f - 2>&1 >/dev/null) || printf "%s\n" "$err"'
```

Three details in that one-liner are the contract in miniature. Success prints one `… (server dry run)` line **per resource on stdout**, and every stdout line becomes a finding — so stdout is discarded and only stderr, where kubectl puts the errors, is reported. `printf` on the failure branch exits 0, because findings are not a tool failure. And the context check is hoisted into its own `: "${ARGOCDF_CONTEXT:?}"` rather than written inline as `--context "${ARGOCDF_CONTEXT:?}"`: inline, the unset variable kills only the command substitution, leaving `err` empty, so `|| printf` emits a blank line and the adapter exits **0 with no findings** — the linter never ran, yet every application is reported clean. Hoisted, the shell exits non-zero before kubectl starts and argocdf attaches its own failure warning.

**Apply CRDs before relying on policies over custom resources.** argocdf renders what your PR *will* produce, which routinely includes resources the cluster does not have yet — an operator's CRs introduced alongside its CRD, or a chart shipping CRs for an operator you have not installed. kyverno can only evaluate a resource whose GVK it can map to a GVR, so a policy over such a kind is **skipped silently**: no finding, no error, and a report that reads clean. Nothing distinguishes "the policy passed" from "the policy never ran", so apply the CRD first (or accept that those policies are not enforced in the diff). This is also why argocdf passes `--continue-on-fail`: without it a single unmappable document makes kyverno return an empty report, discarding the findings it already made on everything else. `case/lint-unmappable-kind` in the e2e suite pins both halves of that behavior.

**One check per policy.** kyverno stops at the first failing validation within a policy, so a policy carrying four `validations:` reports only the first violation it hits — you fix it, re-run, and discover the next. Split independent checks into separate policies to see them all at once.

Whether you use `--lint-kyverno` or your own adapter, keep non-policy files out of the directory kyverno is pointed at: a `kyverno-test.yaml` beside a policy makes it emit NOTHING — exit 0, empty stdout, empty stderr — which any correct adapter can only read as "no findings", so the policy silently stops being enforced with nothing anywhere to signal it. `apply` recurses to any depth and skips dot-files and dot-dirs, so `policies/.tests/` is a safe home for test manifests (a `kyverno-test.yaml` there can reference `../<policy>.yaml`); conftest is not affected, since its own `*_test.rego` unit tests define `test_*` rules rather than `deny` rules.

That one-liner is only meant to show the contract — the shell quoting gets cryptic fast. For real use, put the tool and its output handling into a small script committed to your repo and pass that to `--lint`. Reference implementations: [`lint-kyverno.sh`](https://github.com/rgeraskin/argocdf-test-repo/blob/master/scripts/lint-kyverno.sh) and [`lint-conftest.sh`](https://github.com/rgeraskin/argocdf-test-repo/blob/master/scripts/lint-conftest.sh). Because each side's command runs in that side's worktree, the script — like the policies it references — is picked up in each branch's own version:

```bash
argocdf --lint ./scripts/lint-manifests.sh
```

```bash
#!/usr/bin/env bash
# scripts/lint-manifests.sh — argocdf lint adapter.
# Reads rendered manifests on stdin, prints one finding per line on stdout.
# No errexit/pipefail: kyverno and conftest exit non-zero on FINDINGS (normal
# operation), and one tool's outcome must never skip the tools after it.
set -u

# Capture stdin once so several tools can each read the manifests.
manifests=$(cat)

# emit <label> <raw-output> <exit-code> <jq-filter>
# Findings when the tool produced output; ONE failure line when it produced
# nothing AND failed; silence when it produced nothing and succeeded.
emit() {
  if [ -n "$2" ]; then
    printf '%s\n' "$2" | jq -r "$4"
  elif [ "$3" -ne 0 ]; then
    echo "$1 failed (exit $3)"
  fi
}

# kyverno consults the cluster (--cluster) to resolve the CRDs of any custom
# resources in the input, so it must be argocdf's cluster: without the exported
# context, findings would describe whatever cluster the shell points at.
if [ -z "${ARGOCDF_CONTEXT:-}" ]; then
  echo "ARGOCDF_CONTEXT not set: refusing to lint against an unknown cluster"
  exit 1
fi
[ -n "${ARGOCDF_KUBECONFIG:-}" ] && export KUBECONFIG="$ARGOCDF_KUBECONFIG"

# --continue-on-fail: rendered manifests routinely contain custom resources
# whose CRDs kyverno cannot resolve (a PR that adds a CRD alongside its CRs, a
# chart shipping CRs for another operator). Without it, ONE unmappable document
# aborts the whole apply — and the violations it did find on ordinary workloads
# in the same input are lost silently.
out=$(kyverno apply policies/ --resource - --policy-report --output-format json \
  --cluster --context "$ARGOCDF_CONTEXT" --continue-on-fail 2>/dev/null <<<"$manifests")
emit "kyverno apply" "$out" "$?" '.results[]?
  | select(.result == "fail" or .result == "warn")
  | "[kyverno/\(.policy)] \(.resources[0].kind)/\(.resources[0].name): \(.message | gsub("\n"; " "))"'

# conftest evaluates the manifests offline, so it needs neither variable.
out=$(conftest test - --policy policy/ --output json 2>/dev/null <<<"$manifests")
emit "conftest" "$out" "$?" '.[] | .failures[]?.msg, .warnings[]?.msg
  | select(. != null) | "[conftest] " + gsub("\n"; " ")'
```

A working adapter under continuous test lives in the e2e suite: [`scripts/lint-kyverno.sh`](https://github.com/rgeraskin/argocdf-test-repo/blob/master/scripts/lint-kyverno.sh) in [argocdf-test-repo](https://github.com/rgeraskin/argocdf-test-repo) (this repository's `e2e/` submodule). Every argocdf release runs it against all 36 e2e cases, on both sides of each one, so its empty-output handling, exit-code contract and use of the exported cluster selectors are exercised continuously. One case exists specifically to prove the cluster reached is argocdf's: its pinned finding comes from a policy over Application CRs, which kyverno can only evaluate after resolving that CRD from the cluster.

### CI Flags

| Flag          | Description                                                                    | Default                 |
|---------------|--------------------------------------------------------------------------------|-------------------------|
| `--exit-code` | Exit `0` if no changes, `1` on error, `2` if changes are present (like `diff`) | `false`                 |
| `--marker`    | Marker id for the markdown PR-comment upsert marker                            | `<!-- argocdf-diff -->` |

### Cache Flags

| Flag          | Description                                | Default                    |
|---------------|--------------------------------------------|----------------------------|
| `--no-cache`  | Disable the persistent render cache        | `false`                    |
| `--cache-dir` | Base directory for render and chart caches | `<user cache dir>/argocdf` |

## Environment Variables

Every flag can also be set through an environment variable. The variable name is the flag name upper-cased, with dashes replaced by underscores, and prefixed with `ARGOCDF_`:

| Flag                          | Environment variable                |
|-------------------------------|-------------------------------------|
| `--repo-dir`                  | `ARGOCDF_REPO_DIR`                  |
| `--repo-url`                  | `ARGOCDF_REPO_URL`                  |
| `--argocd-namespace`          | `ARGOCDF_ARGOCD_NAMESPACE`          |
| `--application-namespaces`    | `ARGOCDF_APPLICATION_NAMESPACES`    |
| `--context`                   | `ARGOCDF_CONTEXT`                   |
| `--kustomize-enable-helm`     | `ARGOCDF_KUSTOMIZE_ENABLE_HELM`     |
| `--kustomize-load-restrictor` | `ARGOCDF_KUSTOMIZE_LOAD_RESTRICTOR` |
| ...                           | `ARGOCDF_<FLAG>` for any other flag |

`ARGOCDF_APPLICATION_NAMESPACES` accepts a comma-separated list (`team-a,team-*`), exactly like ArgoCD's own `ARGOCD_APPLICATION_NAMESPACES`.

Precedence is **explicit flag > environment variable > default**, so a flag passed on the command line always wins over the matching environment variable. Empty variables are ignored.

Repeatable flags (`--file`, `--lint`) carry exactly **one** value when set through their environment variable — the whole value is taken verbatim (lint commands may contain commas and quotes, so no splitting is possible). Repeat the flag on the command line to configure multiple values.

```bash
# These two invocations are equivalent
argocdf --repo-dir /path/to/repo --kustomize-enable-helm

export ARGOCDF_REPO_DIR=/path/to/repo
export ARGOCDF_KUSTOMIZE_ENABLE_HELM=true
argocdf
```

Two additional variables are read directly (they have no flag equivalent):

| Variable                | Description                                                                          |
|-------------------------|--------------------------------------------------------------------------------------|
| `ARGOCDF_EXTERNAL_DIFF` | External diff command for side-by-side terminal output (e.g. `delta --side-by-side`) |
| `KUBECONFIG`            | Standard kubeconfig path, honored during cluster auto-detection                      |

## Commands

| Command               | Description                                      |
|-----------------------|--------------------------------------------------|
| `argocdf version`     | Print version, commit, and build date            |
| `argocdf cache info`  | Show cache location, entry count, and total size |
| `argocdf cache clean` | Remove the entire cache directory                |

## Output Examples

### GitHub PR Comments

Generate markdown output for GitHub PR comments:

```bash
argocdf -q -f md-fields:diff.md # md-unified looks good too
cat diff.md  # Copy and paste into GitHub PR comment
```

The output uses:
- GitHub-flavored markdown with collapsible `<details>` sections
- Emoji badges for change types (🟢 added, 🔴 removed, 🟡 modified)
- `diff` code blocks for syntax-highlighted changes

### Side-by-Side Diff

For terminal side-by-side diff, set the `ARGOCDF_EXTERNAL_DIFF` environment variable to your preferred diff tool:

**Recommended setup with [delta](https://github.com/dandavison/delta):**
```bash
export ARGOCDF_EXTERNAL_DIFF="delta --side-by-side --hunk-header-style=omit --file-style=omit"
argocdf
```

**Alternative with [difftastic](https://github.com/Wilfred/difftastic):**
```bash
export ARGOCDF_EXTERNAL_DIFF="difft --display side-by-side-show-both"
argocdf
```

### HTML Output

Generate an interactive HTML report with side-by-side diffs:

```bash
argocdf -f html-side-by-side:report.html
```

## Development

This project uses [mise](https://mise.jdx.dev/) to pin toolchain versions (`.mise.toml`) and define tasks. Run `mise tasks` to list them.

```bash
# Build (produces ./argocdf)
mise run build

# Run tests
mise run test

# Run in development mode
mise run dev

# Format code
mise run fmt

# Run linter
mise run lint

# Run all checks (vet + lint + test), as CI does
mise run check
```

### End-to-end tests

```bash
mise run e2e:bootstrap   # create a kind cluster with ArgoCD CRDs (WIP)
mise run e2e:clean       # tear it down
```

## Project Structure

```
argocdf/
├── cmd/argocdf/
│   ├── main.go                 # CLI entry point (Cobra), flags, cache/version commands
│   └── version.go              # Version string assembly (ldflags + build info)
├── internal/
│   ├── app/                    # Main orchestrator and dependency-injection factory
│   ├── config/                 # Configuration struct and auto-detection logic
│   ├── cluster/                # K8s client-go wrapper, ArgoCD Application operations
│   ├── git/                    # Repository operations, changed-files detection, worktrees
│   ├── helmconfig/             # Local repo credentials from the user's helm config
│   ├── render/                 # Helm/Kustomize rendering, multi-source, chart cache
│   ├── rendercache/            # Persistent content-addressed render cache
│   ├── diff/                   # Manifest comparison and recursive apps-of-apps discovery
│   ├── output/                 # Terminal, markdown, unified, and HTML writers
│   ├── types/                  # Shared types
│   └── errors/                 # Custom error types
├── e2e/                        # End-to-end test fixtures (git submodule)
├── .goreleaser.yaml            # Release build configuration
├── .github/workflows/          # CI and release pipelines
├── .mise.toml                  # Toolchain versions and task definitions
├── go.mod
└── README.md
```

## License

MIT
