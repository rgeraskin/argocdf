# Differences: argocdf vs ArgoCD

This document outlines the key implementation differences between `argocdf` (this tool) and ArgoCD's actual implementation.

## 1. Manifest Rendering Architecture

| Aspect                      | ArgoCD                                                        | argocdf                                                                                                                                                                               |
|-----------------------------|---------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **Where rendering happens** | Dedicated `argocd-repo-server` pod with timeout (90s default) | In-process, same machine as CLI                                                                                                                                                       |
| **Caching**                 | Aggressive caching with commit SHA as key; Redis-backed       | Persistent file-based caches: content-addressed render cache (keyed by spec, options, and git tree/blob hashes) plus a download cache for pinned remote charts; `--no-cache` disables |
| **Parallel execution**      | Parallel Helm manifest generation (v3.0+)                     | Parallel rendering within each wave via `--concurrency` (default `min(4, NumCPU)`); discovery between waves is single-threaded                                                        |
| **Timeout handling**        | Configurable via `ARGOCD_EXEC_TIMEOUT`                        | No configurable timeout; SIGINT/SIGTERM cancel in-flight renders via context                                                                                                          |
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

| Aspect                 | ArgoCD                                           | argocdf                                                                                                                                                                                                              |
|------------------------|--------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **Version bundled**    | Specific Helm version bundled in container image | Uses system `helm` binary                                                                                                                                                                                            |
| **API versions**       | Uses `--api-versions` from cluster capabilities  | ✅ Same — discovered from the cluster and passed via `--api-versions` (opt out with `--no-api-versions`), plus sanitized `--kube-version`. Per-source `helm.kubeVersion`/`helm.apiVersions` overrides are not honored |
| **Namespace handling** | Full namespace resolution with cluster defaults  | Basic namespace flags                                                                                                                                                                                                |
| **Hooks**              | Filters Helm hooks during rendering              | No hook filtering                                                                                                                                                                                                    |
| **Pass credentials**   | Supports `--pass-credentials` for private repos  | ✅ Same — `spec.source.helm.passCredentials` is passed to chart fetching, exactly as the repo-server does                                                                                                             |

### Missing Helm features:

- Helm hook filtering (`helm.sh/hook` annotations)
- Skip CRDs option (`--skip-crds`)

## 6. Kustomize Rendering

| Aspect                                                | ArgoCD                                    | argocdf                                                     |
|-------------------------------------------------------|-------------------------------------------|-------------------------------------------------------------|
| **NamePrefix/NameSuffix**                             | Full support                              | ✅ Supported (via `kustomize edit`)                          |
| **Images override**                                   | Full support                              | ✅ Supported (via `kustomize edit set image`)                |
| **Replicas**                                          | Full support                              | ✅ Supported (via `kustomize edit set replicas`)             |
| **CommonLabels**                                      | Full support with force/without-selector  | ✅ Supported (via `kustomize edit add label`)                |
| **CommonAnnotations**                                 | Full support with force                   | ✅ Supported (via `kustomize edit add annotation`)           |
| **Namespace**                                         | Full support                              | ✅ Supported (via `kustomize edit set namespace`)            |
| **Components**                                        | Full support                              | ✅ Supported (via `kustomize edit add component`)            |
| **Patches**                                           | Full support                              | ✅ Supported (direct kustomization.yaml modification)        |
| **`--enable-helm`**                                   | Configurable globally or per-app          | ✅ Supported via `--kustomize-enable-helm` CLI flag          |
| **Build options**                                     | Configurable via `kustomize.buildOptions` | ✅ Supported via `--kustomize-build-options` CLI flag        |
| **Load restrictor**                                   | Configurable                              | ✅ Supported via `--kustomize-load-restrictor` CLI flag      |
| **`kustomize.version`** (per-app binary version)      | Supported via configured tool versions    | ❌ Not supported — always uses the system `kustomize` binary |
| **`kustomize.kubeVersion` / `kustomize.apiVersions`** | Passed through to Helm inflation          | ❌ Not supported                                             |
| **`ignoreMissingComponents`**                         | Supported                                 | ❌ Not supported                                             |

### Implementation approach:

argocdf uses `kustomize edit` commands to apply Application-level overrides before running `kustomize build`, matching ArgoCD's approach. When overrides are present, the repository tree is first copied to a temp directory and edits are applied there — the user's checkout and the render worktrees are never modified.

Source-type detection also matches ArgoCD's repo-server: explicit tool config (`helm:`, `kustomize:`, `directory:`) takes precedence, otherwise the source path is inspected (`Chart.yaml` → Helm, kustomization file → Kustomize, else plain directory). This applies identically to single- and multi-source apps.

```yaml
# Fully supported via Application spec:
spec:
  source:
    kustomize:
      namePrefix: "prod-"
      nameSuffix: "-v2"
      images:
        - nginx:1.21
      replicas:
        - name: deployment
          count: 3
      commonLabels:
        app: myapp
      commonAnnotations:
        team: platform
      namespace: production
      components:
        - ../components/monitoring
      patches:
        - patch: |-
            - op: replace
              path: /spec/replicas
              value: 5
          target:
            kind: Deployment
```

## 7. Multi-Source Applications

| Aspect                        | ArgoCD                                                | argocdf                                                                                                                                                                |
|-------------------------------|-------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **Ref source authentication** | Uses stored credentials/SSH keys                      | Relies on git CLI credentials                                                                                                                                          |
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
| Helm hook filtering           | Low      | Low        |
| Retry logic                   | Low      | Low        |

## 13. Implementation Approach: Reuse vs Reimplementation

argocdf deliberately reimplements some functionality rather than importing ArgoCD's libraries directly. This table documents these decisions:

| Area                       | argocdf Approach                                              | ArgoCD Alternative     | Rationale                                                                      |
|----------------------------|---------------------------------------------------------------|------------------------|--------------------------------------------------------------------------------|
| **Application Types**      | Uses ArgoCD types via aliases                                 | Same                   | Ensures field compatibility, no drift                                          |
| **Repository credentials** | ArgoCD's `util/db` + `util/settings` (`--repo-creds=cluster`) | Same                   | Zero drift in secret parsing, URL matching, and credential-template resolution |
| **Chart downloads**        | ArgoCD's `util/helm.Client.ExtractChart` (both engines)       | Same                   | Repo-server parity: OCI dispatch, registry login, TLS/proxy handling for free  |
| **Helm Rendering**         | `exec.Command("helm", ...)`                                   | gitops-engine          | Simpler, uses user's installed helm version                                    |
| **Kustomize Rendering**    | `exec.Command("kustomize", ...)`                              | gitops-engine          | Simpler, uses user's installed kustomize                                       |
| **Git Operations**         | `exec.Command("git", ...)`                                    | gitops-engine / go-git | Simpler, no version mismatch concerns                                          |
| **Manifest Diffing**       | Custom recursive comparison                                   | gitops-engine diff     | Lighter weight, tailored for preview use case                                  |
| **URL Normalization**      | `git.NormalizeRepoURL()`                                      | ArgoCD has similar     | Small utility, consolidated in git package                                     |

### Why Not Use gitops-engine?

ArgoCD's `gitops-engine` provides rendering and diffing, but:

1. **Designed for controller context** - Expects ArgoCD repo-server architecture
2. **Heavy abstraction layers** - Adds complexity for features argocdf doesn't need
3. **Binary bloat** - Would significantly increase binary size
4. **Version coupling** - Ties argocdf to specific ArgoCD internals

The `exec.Command` approach for Helm/Kustomize is:
- Simpler to understand and debug
- Uses the exact binaries users have installed
- No version mismatch between embedded vs system tools
- More portable across environments

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

| Aspect                                | ArgoCD                                                                                                   | argocdf                                                                                        |
|---------------------------------------|----------------------------------------------------------------------------------------------------------|------------------------------------------------------------------------------------------------|
| **Default when a commit lands**       | refresh/re-render EVERY application in the repository — a new commit invalidates the whole manifest cache | only applications whose `source.path` contains a changed file (plus `$ref` value files)         |
| **`manifest-generate-paths`**         | narrows the default: declared paths gate webhook refresh and manifest-cache reuse                        | ✅ widens the default: declared paths are the only way to reach a dependency it cannot infer     |
| **Resolution of the annotation**      | `util/app/path.GetSourceRefreshPaths` + `AppFilesHaveChanged`                                             | ✅ the same functions, called through `internal/cluster`                                         |
| **Present-but-empty annotation**      | declares nothing, so the default (refresh everything) applies                                            | treated as absent: falls back to path matching, NOT to "always affected"                       |
| **Git-provider dependence**           | webhook support for the annotation is limited to GitHub, GitLab and Gogs                                 | none — argocdf computes the changed-file list itself with git                                   |
| **`$values` files once declared**     | not covered by the declaration either — the docs note external values files miss out                     | ✅ same: the declaration replaces argocdf's `$ref` value-file matching, so declare those paths   |
| **Declaration nothing can resolve**   | n/a (ArgoCD renders on request regardless)                                                               | app can never be reported affected — argocdf WARNS rather than vanishing silently               |

The defaults are opposites, and that is the whole asymmetry: ArgoCD's default is safe-but-expensive (render everything, then let the annotation trim it), while argocdf's is cheap-but-partial (render what the paths match). So a dependency outside an application's source path — a kustomize overlay whose base is `../shared`, a helm chart including a sibling file, a values file elsewhere in the repo — is invisible to argocdf until it is declared:

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
