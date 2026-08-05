# Differences: argocdf vs ArgoCD

This document outlines the key implementation differences between `argocdf` and ArgoCD's actual implementation.

## 1. Manifest Rendering Architecture

| Aspect                      | ArgoCD                                                        | argocdf                                                                                                                                                                               |
|-----------------------------|---------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **Where rendering happens** | Dedicated `argocd-repo-server` pod with timeout (90s default) | In-process, same machine as CLI                                                                                                                                                       |
| **Caching**                 | Aggressive caching with commit SHA as key; Redis-backed       | Persistent file-based caches: content-addressed render cache (keyed by spec, options, and git tree/blob hashes) plus a download cache for pinned remote charts; `--no-cache` disables |
| **Parallel execution**      | Parallel Helm manifest generation (v3.0+)                     | Parallel rendering within each wave via `--concurrency` (default `min(4, NumCPU)`); discovery between waves is single-threaded                                                        |
| **Timeout handling**        | Configurable via `ARGOCD_EXEC_TIMEOUT`                        | Same `ARGOCD_EXEC_TIMEOUT` (it is ArgoCD's own exec code doing the tool invocations); SIGINT/SIGTERM additionally cancel in-flight renders via context                                |
| **Repository clone**        | Maintains local clone, reused across requests                 | Renders from ephemeral worktrees of the local clone; external `$ref` repos are cloned fresh per render                                                                                |

ArgoCD's repo-server is designed for scale - it caches manifests, handles concurrent requests, and isolates manifest generation from the controller. argocdf runs everything in a single process which is fine for a preview tool but wouldn't scale for production.

## 2. Diff Strategy

| Aspect                        | ArgoCD                                                                      | argocdf                                              |
|-------------------------------|-----------------------------------------------------------------------------|------------------------------------------------------|
| **Two-way vs Three-way diff** | Supports three-way diff using `last-applied-configuration` annotation       | Two-way diff only (old manifests vs new manifests)   |
| **Server-side diff**          | Optional - uses K8s `structured-merge-diff` library                         | Not supported                                        |
| **Live state comparison**     | Compares desired state vs **live cluster state**                            | Compares old branch vs new branch (no cluster state) |
| **Normalization**             | Extensive normalization (secret encoding, role aggregation, field ordering) | Basic field ordering via YAML re-marshal             |

### ArgoCD's gitops-engine approach:

```go
type DiffResult struct {
    Modified       bool
    NormalizedLive []byte  // Live cluster state, normalized
    PredictedLive  []byte  // What live would look like after apply
}
```

### argocdf's approach:

```go
type ManifestSetDiff struct {
    Added    []Manifest   // Only in new branch
    Removed  []Manifest   // Only in old branch
    Modified []ManifestDiff
    // No live state comparison!
}
```

**Critical difference**: ArgoCD compares **desired state vs live cluster state**. argocdf compares **base branch vs target branch**. This means argocdf won't detect:
- Drift caused by manual `kubectl` changes
- Mutations applied by admission webhooks
- Default values added by Kubernetes controllers

## 3. Diff Customization

| Feature                             | ArgoCD                                               | argocdf         |
|-------------------------------------|------------------------------------------------------|-----------------|
| **Ignore differences with JQ**      | Yes, via `jqPathExpressions`                         | No              |
| **Ignore by managedFields manager** | Yes (e.g., ignore `kube-controller-manager` changes) | No              |
| **Custom normalizers**              | Yes, pluggable via interface                         | Fixed list only |
| **Per-resource ignore rules**       | Yes, via `resource.customizations` in ConfigMap      | No              |

### ArgoCD's configurable ignore rules:

```yaml
# argocd-cm ConfigMap
resource.customizations: |
  admissionregistration.k8s.io/MutatingWebhookConfiguration:
    ignoreDifferences: |
      jqPathExpressions:
      - '.webhooks[]?.clientConfig.caBundle'
```

### argocdf's fixed ignore list:

```go
IgnoredFields: map[string]bool{
    "metadata.resourceVersion":   true,
    "metadata.uid":               true,
    "metadata.generation":        true,
    "metadata.creationTimestamp": true,
    "metadata.managedFields":     true,
    "metadata.annotations.kubectl.kubernetes.io/last-applied-configuration": true,
    "status": true,
}
```

## 4. Secret Handling

| Aspect                           | ArgoCD                                                                          | argocdf                       |
|----------------------------------|---------------------------------------------------------------------------------|-------------------------------|
| **Secret data masking**          | `HideSecretData()` replaces values with `*****` while preserving diff structure | No masking - shows raw values |
| **stringData → data conversion** | Normalizes `stringData` to base64 `data` before diff                            | No normalization              |

## 5. Helm Rendering

Since 0.5.0 argocdf renders through ArgoCD's own repo-server code (`reposerver/repository.GenerateManifests`, the `argocd app diff --local` path), so the Helm OPTION TRANSLATION is not a reimplementation with gaps — it is the same code: `--include-crds` by default, `skipCrds`, `skipTests`, `skipSchemaValidation`, parameters/`--set-string` coercion via `forceString`, `fileParameters`, inline `values`, `valuesObject`, `$ref` value files, release-name and namespace handling, `ARGOCD_APP_*` build-env substitution, `.argocd-source*.yaml` overrides, and per-source `helm.kubeVersion`/`helm.apiVersions` overrides all behave as the linked ArgoCD (v3.3.x) behaves, structurally.

What actually differs:

| Aspect               | ArgoCD                                                 | argocdf                                                                                                                                      |
|----------------------|--------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------|
| **Helm binary**      | Specific version bundled in the repo-server image      | The system `helm` on PATH — output can differ between helm versions exactly as it can between differently-versioned repo-servers             |
| **Helm environment** | The repo-server pod's isolated environment             | An isolated per-render temp helm home built by ArgoCD's code, with inherited `HELM_*` env vars scrubbed so the user's machine cannot leak in |
| **Cluster versions** | `--kube-version`/`--api-versions` from the destination | Discovered from the connected cluster (`--kube-version` override, `--no-api-versions` opt-out) — the destination cluster is not consulted    |
| **Hooks in output**  | Rendered manifests include hooks; SYNC filters them    | Same rendered output; argocdf diffs it and never syncs, so hook filtering is out of scope by design                                          |

## 6. Kustomize Rendering

Kustomize sources render through the same ArgoCD repo-server code path as Helm ones: every `spec.source.kustomize` field the linked ArgoCD honors — namePrefix/nameSuffix, images, replicas, commonLabels/commonAnnotations (with their force/without-selector variants), namespace, components, patches, `ignoreMissingComponents`, `kubeVersion`/`apiVersions` for Helm inflation — is applied by ArgoCD's own translation, not by an argocdf reimplementation. Source-type detection is likewise ArgoCD's (explicit tool config first, then filesystem discovery), identically for single- and multi-source apps.

What actually differs:

| Aspect                                           | ArgoCD                                                      | argocdf                                                                                                         |
|--------------------------------------------------|-------------------------------------------------------------|-----------------------------------------------------------------------------------------------------------------|
| **Kustomize binary**                             | Bundled; `kustomize.version` selects a configured alternate | The system `kustomize` on PATH — per-app `kustomize.version` has no configured tool versions to select from     |
| **Cluster-level kustomize settings** (argocd-cm) | `kustomize.buildOptions` etc. read from the control plane   | Not read — `--kustomize-enable-helm`, `--kustomize-build-options`, `--kustomize-load-restrictor` flags stand in |

## 7. Multi-Source Applications

| Aspect                        | ArgoCD                                                | argocdf                                                                                                                                                                |
|-------------------------------|-------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **Ref source authentication** | Uses stored credentials/SSH keys                      | HTTP(S) credentials from the `--repo-creds` source ride `GIT_CONFIG_*` env (never argv); SSH remotes use ambient git config                                            |
| **Repository caching**        | Reuses cached clones                                  | Ref sources pointing at the diffed repo use the local branch checkout (so PR edits to `$values` files diff correctly); external ref repos are cloned fresh each render |
| **Cross-repo values**         | Full `$values` reference support                      | ✅ `$ref` resolution in `valueFiles` and `fileParameters`, with path-containment validation                                                                             |
| **Source type detection**     | Per-source, explicit config then filesystem discovery | ✅ Same logic as single-source apps (explicit config, then `Chart.yaml` auto-detection)                                                                                 |
| **Source ordering**           | Deterministic merge order                             | Sequential rendering                                                                                                                                                   |

## 8. Config Management Plugins (CMP)

| Aspect               | ArgoCD                             | argocdf             |
|----------------------|------------------------------------|---------------------|
| **Plugin support**   | Full CMP sidecar architecture      | None                |
| **Custom tools**     | Jsonnet, Tanka, or any custom tool | Helm/Kustomize only |
| **Plugin discovery** | Automatic via `plugin.yaml`        | N/A                 |

ArgoCD supports arbitrary config management tools via the CMP system:

```yaml
# ConfigMap for custom plugin
apiVersion: v1
kind: ConfigMap
metadata:
  name: cmp-plugin
data:
  plugin.yaml: |
    apiVersion: argoproj.io/v1alpha1
    kind: ConfigManagementPlugin
    metadata:
      name: kustomize-build-with-helm
    spec:
      generate:
        command: ["kustomize", "build", "--enable-helm", "."]
```

## 9. ApplicationSet Support

| Aspect                 | ArgoCD                                  | argocdf       |
|------------------------|-----------------------------------------|---------------|
| **ApplicationSet**     | Full support with generators            | Not supported |
| **Template rendering** | Parameters substituted into templates   | N/A           |
| **Generators**         | List, Cluster, Git, Matrix, Merge, etc. | N/A           |

## 10. Error Handling & Resilience

| Aspect                   | ArgoCD                                 | argocdf                                                                                                                                    |
|--------------------------|----------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------|
| **Retry logic**          | Built-in retry for transient failures  | No retry                                                                                                                                   |
| **Rate limiting**        | Respects API rate limits               | No rate limiting                                                                                                                           |
| **Repository lock**      | Exclusive lock for manifest generation | Per-chart lock serializing `helm dependency build`; no global repo lock                                                                    |
| **Graceful degradation** | Continues with partial failures        | Continues with partial failures — per-app render errors are reported in the output and reflected in the exit code, other apps still render |

## 11. Normalization Differences

### ArgoCD normalizes:

- Secret `stringData` → base64 `data`
- Aggregated ClusterRoles (ignores computed rules)
- Webhook `caBundle` fields
- Controller-managed fields (HPA `replicas`, etc.)
- Type coercion (float64 vs int)
- Empty vs nil maps/slices
- Field ordering consistency

### argocdf normalizes:

- Only ignores fixed metadata paths
- Basic YAML re-marshaling for field ordering
- No type coercion
- No semantic understanding of resources

## 12. Three-Way Diff

ArgoCD uses three-way diff when `last-applied-configuration` annotation exists:

```
          ┌─────────────────┐
          │   Original      │ (from last-applied-configuration)
          │   (what was     │
          │   last applied) │
          └────────┬────────┘
                   │
    ┌──────────────┼──────────────┐
    │              │              │
    ▼              ▼              ▼
┌───────┐    ┌──────────┐    ┌───────┐
│Config │    │ Changes  │    │ Live  │
│(Git)  │    │ detected │    │Cluster│
└───────┘    └──────────┘    └───────┘
```

This allows distinguishing:
- **User changes**: Differences between original and config (intended changes)
- **Controller changes**: Differences between original and live (made by K8s)
- **Conflicts**: When both user and controller modified the same field

argocdf only does two-way diff (base branch vs target branch), missing this nuance.

## Summary of Missing Features

| Feature                       | Priority | Complexity |
|-------------------------------|----------|------------|
| Live cluster state comparison | High     | High       |
| Three-way diff                | High     | High       |
| Secret masking                | High     | Low        |
| JQ-based diff ignore          | Medium   | Medium     |
| Server-side diff              | Medium   | High       |
| CMP support                   | Medium   | High       |
| ApplicationSet                | Medium   | High       |
| Retry logic                   | Low      | Low        |

## 13. Implementation Approach: Reuse vs Reimplementation

Through 0.4.x argocdf rendered with its own `exec.Command("helm"|"kustomize")` pipeline; 0.5.0 deleted it. The boundary is now: manifest GENERATION is ArgoCD's code running in-process, while everything around it — worktrees, discovery, diffing, output — is argocdf's own:

| Area                       | argocdf Approach                                                      | Alternative considered  | Rationale                                                                                                     |
|----------------------------|-----------------------------------------------------------------------|-------------------------|---------------------------------------------------------------------------------------------------------------|
| **Application Types**      | ArgoCD's types via aliases                                            | Custom structs          | Ensures field compatibility, no drift                                                                         |
| **Manifest generation**    | ArgoCD's `reposerver/repository.GenerateManifests`, in-process        | Own helm/kustomize exec | Exact render parity, structurally: option translation, build-env, source dispatch cannot drift (~85MB binary) |
| **Repository credentials** | ArgoCD's `util/db` + `util/settings` (`--repo-creds=cluster`)         | Own secret parsing      | Zero drift in secret parsing, URL matching, and credential-template resolution                                |
| **Chart downloads**        | ArgoCD's `util/helm.Client.ExtractChart` behind argocdf's chart cache | Own helm pull           | Repo-server parity: OCI dispatch, TLS/proxy handling for free (registry auth rides an isolated config file)   |
| **Git Operations**         | `exec.Command("git", ...)`, ephemeral worktrees                       | gitops-engine / go-git  | argocdf owns the two-sided checkout model; no version mismatch concerns                                       |
| **Manifest Diffing**       | Custom recursive comparison                                           | gitops-engine diff      | gitops-engine diffs desired-vs-LIVE state; argocdf diffs branch-vs-branch, a different problem                |
| **URL Normalization**      | `git.NormalizeRepoURL()`                                              | ArgoCD has similar      | Small utility, consolidated in git package                                                                    |

Note that ArgoCD's tools still run as the SYSTEM `helm`/`kustomize` binaries — GenerateManifests execs them — so tool versions remain the user's, exactly as a differently-imaged repo-server would differ.

### Why import the repo-server instead of gitops-engine?

The rendering half of the old "don't import ArgoCD" rationale inverted once render parity became the product: reimplementing option translation is where parity bugs came from, and the ~85MB binary cost bought their structural elimination (the same trade-off as the type aliases). gitops-engine remains unimported for DIFFING, deliberately: its diff answers "how does desired differ from live cluster state" for a sync engine, while argocdf answers "how does branch A differ from branch B" for a preview — the live-state machinery (normalization against cluster defaults, three-way merge metadata) has no counterpart in a two-branch comparison.

## 14. Repository Credentials

| Aspect                    | ArgoCD                                                                       | argocdf                                                                                                                                                                                   |
|---------------------------|------------------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **Credential source**     | repo-server resolves repository secrets / repo-creds templates via `util/db` | ✅ Same code, via `--repo-creds=cluster` (default): `ListHelmRepositories`, `ListOCIRepositories`, `GetAll*RepositoryCredentials`, `GetRepository`                                         |
| **Per-source OCI gating** | controller offers OCI repos/creds only when `source.IsOCI()`                 | ✅ Same, verbatim — including the degradations (an https chart with `oci://` dependencies gets no OCI creds)                                                                               |
| **Local credentials**     | `argocd app diff --local` renders with the user's helm config                | `--repo-creds=local`: same idea, but through the standard pipeline — repositories.yaml entries feed the same credential lists, OCI rides `HELM_REGISTRY_CONFIG` (credential helpers work) |
| **No credentials**        | n/a                                                                          | `--repo-creds=none` renders anonymously                                                                                                                                                   |
| **AppProject scoping**    | repos/creds are filtered by project permissions                              | ❌ Not enforced — argocdf has no AppProject context; all configured repos/creds are offered for matching (`ProjectSourceRepos: ["*"]`)                                                     |
| **Failure mode**          | server components assume RBAC; `argocd admin backup` hard-exits on Forbidden | Fatal with an actionable message naming the escape hatches (`local`/`none`); a cluster without repo secrets is not a failure                                                              |

## 15. Application Namespaces

| Aspect                                   | argocd-server                                                                             | argocdf                                                                                                               |
|------------------------------------------|-------------------------------------------------------------------------------------------|-----------------------------------------------------------------------------------------------------------------------|
| **`--application-namespaces` semantics** | ADDITIVE: the control-plane namespace is always managed, the list adds more               | **EXHAUSTIVE**: only listed namespaces are scanned; include the ArgoCD namespace explicitly                           |
| **RBAC model**                           | requires cluster-scoped watches once enabled                                              | all-literal lists use per-namespace reads (namespace-scoped RBAC suffices); pattern entries require cluster-wide list |
| **Pattern matching**                     | `glob.MatchStringInList` (globs + `/regex/`) with a hardcoded control-plane short-circuit | ✅ Same matcher, minus the short-circuit — which is precisely the additive semantics being dropped                     |

The deviation is deliberate: argocd-server always has RBAC over its own namespace, while argocdf runs under the *user's* RBAC, which may not include the ArgoCD namespace at all.

## 16. Which Applications a Change Affects

This is the one question argocdf must answer that ArgoCD never asks. ArgoCD reconciles a *named* application on request or on a timer; argocdf starts from a git diff and has to decide which applications that diff could possibly change.

| Aspect                              | ArgoCD                                                                                                    | argocdf                                                                                      |
|-------------------------------------|-----------------------------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------|
| **Default when a commit lands**     | refresh/re-render EVERY application in the repository — a new commit invalidates the whole manifest cache | only applications a matcher reaches: `source.path`, helm `valueFiles`, `fileParameters`      |
| **Value file outside source path**  | renders it: relative entries from the source path, absolute ones from the repo ROOT                       | ✅ same rule, same resolver — applied to SELECTION too, so no annotation is needed            |
| **`$ARGOCD_APP_*` in a file path**  | substituted before the path is resolved, so `values-$ARGOCD_APP_NAME.yaml` is a real file                 | ⚠️ rendered the same, but NOT substituted for selection: resolves literally, matches nothing |
| **`manifest-generate-paths`**       | narrows the default: declared paths gate webhook refresh and manifest-cache reuse                         | ✅ widens the default: declared paths are the only way to reach a dependency it cannot infer  |
| **Resolution of the annotation**    | `util/app/path.GetSourceRefreshPaths` + `AppFilesHaveChanged`                                             | ✅ the same functions, called through `internal/cluster`                                      |
| **Present-but-empty annotation**    | declares nothing, so the default (refresh everything) applies                                             | treated as absent: falls back to path matching, NOT to "always affected"                     |
| **Git-provider dependence**         | webhook support for the annotation is limited to GitHub, GitLab and Gogs                                  | none — argocdf computes the changed-file list itself with git                                |
| **Helm files once declared**        | not covered by the declaration either — the docs note external values files miss out                      | ✅ same: it replaces argocdf's `$ref`, escaping, absolute and `fileParameters` matching alike |
| **Declaration nothing can resolve** | n/a (ArgoCD renders on request regardless)                                                                | app can never be reported affected — argocdf WARNS rather than vanishing silently            |

The defaults are opposites, and that is the whole asymmetry: ArgoCD's default is safe-but-expensive (render everything, then let the annotation trim it), while argocdf's is cheap-but-partial (render what the paths match). Helm value files and `fileParameters` are the exception: argocdf resolves those through ArgoCD's own resolver, so a change to `../shared/vals.yaml` or `/config/prod.yaml` is attributed without any annotation. What stays invisible is what a *renderer* discovers rather than a field declares — a kustomize overlay whose base is `../shared`, a chart pulling `file://../lib` — and for those, declaring the paths is the only way in:

```yaml
metadata:
  annotations:
    # the base AND this app's own path; the annotation REPLACES the default
    argocd.argoproj.io/manifest-generate-paths: ../kustomize-base;.
```

Semantics (ArgoCD's, verified against v3.3.11's resolver and a live controller): entries are `;`-separated, a leading `/` is repo-root-relative, everything else is joined to that *source's* path, and globs go through `filepath.Match` — which does not cross `/`. Omitting `.` silently stops the application from reacting to its own directory. `e2e/case/kustomize-relative-base` pins the behavior end to end.

## References

- [ArgoCD Diff Customization](https://argo-cd.readthedocs.io/en/stable/user-guide/diffing/)
- [ArgoCD manifest-paths annotation](https://argo-cd.readthedocs.io/en/stable/operator-manual/high_availability/#manifest-paths-annotation)
- [ArgoCD Diff Strategies](https://argo-cd.readthedocs.io/en/stable/user-guide/diff-strategies/)
- [gitops-engine diff package](https://pkg.go.dev/github.com/argoproj/gitops-engine/pkg/diff)
- [ArgoCD High Availability](https://argo-cd.readthedocs.io/en/stable/operator-manual/high_availability/)
- [ArgoCD Config Management Plugins](https://argo-cd.readthedocs.io/en/stable/operator-manual/config-management-plugins/)
- [ArgoCD Kustomize](https://argo-cd.readthedocs.io/en/stable/user-guide/kustomize/)
