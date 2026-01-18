# Differences: argocdf vs ArgoCD

This document outlines the key implementation differences between `argocdf` (this tool) and ArgoCD's actual implementation.

## 1. Manifest Rendering Architecture

| Aspect | ArgoCD | argocdf |
|--------|--------|---------|
| **Where rendering happens** | Dedicated `argocd-repo-server` pod with timeout (90s default) | In-process, same machine as CLI |
| **Caching** | Aggressive caching with commit SHA as key; Redis-backed | No caching |
| **Parallel execution** | Parallel Helm manifest generation (v3.0+) | Sequential |
| **Timeout handling** | Configurable via `ARGOCD_EXEC_TIMEOUT` | No timeout |
| **Repository clone** | Maintains local clone, reused across requests | Fresh operations each time |

ArgoCD's repo-server is designed for scale - it caches manifests, handles concurrent requests, and isolates manifest generation from the controller. argocdf runs everything in a single process which is fine for a preview tool but wouldn't scale for production.

## 2. Diff Strategy

| Aspect | ArgoCD | argocdf |
|--------|--------|---------|
| **Two-way vs Three-way diff** | Supports three-way diff using `last-applied-configuration` annotation | Two-way diff only (old manifests vs new manifests) |
| **Server-side diff** | Optional - uses K8s `structured-merge-diff` library | Not supported |
| **Live state comparison** | Compares desired state vs **live cluster state** | Compares old branch vs new branch (no cluster state) |
| **Normalization** | Extensive normalization (secret encoding, role aggregation, field ordering) | Basic field ordering via YAML re-marshal |

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

| Feature | ArgoCD | argocdf |
|---------|--------|---------|
| **Ignore differences with JQ** | Yes, via `jqPathExpressions` | No |
| **Ignore by managedFields manager** | Yes (e.g., ignore `kube-controller-manager` changes) | No |
| **Custom normalizers** | Yes, pluggable via interface | Fixed list only |
| **Per-resource ignore rules** | Yes, via `resource.customizations` in ConfigMap | No |

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
    "metadata.resourceVersion": true,
    "metadata.uid":             true,
    "metadata.generation":      true,
    "metadata.creationTimestamp": true,
    "metadata.managedFields":   true,
    "status":                   true,
}
```

## 4. Secret Handling

| Aspect | ArgoCD | argocdf |
|--------|--------|---------|
| **Secret data masking** | `HideSecretData()` replaces values with `*****` while preserving diff structure | No masking - shows raw values |
| **stringData → data conversion** | Normalizes `stringData` to base64 `data` before diff | No normalization |

## 5. Helm Rendering

| Aspect | ArgoCD | argocdf |
|--------|--------|---------|
| **Version bundled** | Specific Helm version bundled in container image | Uses system `helm` binary |
| **API versions** | Uses `--api-versions` from cluster capabilities | Uses `--kube-version` only |
| **Namespace handling** | Full namespace resolution with cluster defaults | Basic `--namespace` flag |
| **Hooks** | Filters Helm hooks during rendering | No hook filtering |
| **Pass credentials** | Supports `--pass-credentials` for private repos | Not implemented |

### Missing Helm features:

- `--api-versions` for full API capability detection
- Helm hook filtering (`helm.sh/hook` annotations)
- `--pass-credentials` for authenticated chart pulls
- Skip CRDs option (`--skip-crds`)

## 6. Kustomize Rendering

| Aspect | ArgoCD | argocdf |
|--------|--------|---------|
| **`--enable-helm`** | Configurable globally or per-app | Not supported |
| **Build options** | Configurable via `kustomize.buildOptions` | Only `--name-prefix` and `--name-suffix` |
| **Images override** | `--images` flag support | Not supported |
| **Labels/annotations** | `--common-labels`, `--common-annotations` | Not supported |
| **Load restrictor** | Configurable | Not supported |

### Missing Kustomize features:

```yaml
# ArgoCD supports these via Application spec:
spec:
  source:
    kustomize:
      images:
        - nginx:1.21
      commonLabels:
        app: myapp
      commonAnnotations:
        team: platform
```

## 7. Multi-Source Applications

| Aspect | ArgoCD | argocdf |
|--------|--------|---------|
| **Ref source authentication** | Uses stored credentials/SSH keys | Relies on git CLI credentials |
| **Repository caching** | Reuses cached clones | Fresh clone each time |
| **Cross-repo values** | Full `$values` reference support | Basic `$ref` support |
| **Source ordering** | Deterministic merge order | Sequential rendering |

## 8. Config Management Plugins (CMP)

| Aspect | ArgoCD | argocdf |
|--------|--------|---------|
| **Plugin support** | Full CMP sidecar architecture | None |
| **Custom tools** | Jsonnet, Tanka, or any custom tool | Helm/Kustomize only |
| **Plugin discovery** | Automatic via `plugin.yaml` | N/A |

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

| Aspect | ArgoCD | argocdf |
|--------|--------|---------|
| **ApplicationSet** | Full support with generators | Not supported |
| **Template rendering** | Parameters substituted into templates | N/A |
| **Generators** | List, Cluster, Git, Matrix, Merge, etc. | N/A |

## 10. Error Handling & Resilience

| Aspect | ArgoCD | argocdf |
|--------|--------|---------|
| **Retry logic** | Built-in retry for transient failures | No retry |
| **Rate limiting** | Respects API rate limits | No rate limiting |
| **Repository lock** | Exclusive lock for manifest generation | No locking |
| **Graceful degradation** | Continues with partial failures | Fails fast |

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

| Feature | Priority | Complexity |
|---------|----------|------------|
| Live cluster state comparison | High | High |
| Three-way diff | High | High |
| Secret masking | High | Low |
| JQ-based diff ignore | Medium | Medium |
| Server-side diff | Medium | High |
| CMP support | Medium | High |
| ApplicationSet | Medium | High |
| Helm hook filtering | Low | Low |
| Kustomize `--enable-helm` | Low | Low |
| Retry logic | Low | Low |

## References

- [ArgoCD Diff Customization](https://argo-cd.readthedocs.io/en/stable/user-guide/diffing/)
- [ArgoCD Diff Strategies](https://argo-cd.readthedocs.io/en/stable/user-guide/diff-strategies/)
- [gitops-engine diff package](https://pkg.go.dev/github.com/argoproj/gitops-engine/pkg/diff)
- [ArgoCD High Availability](https://argo-cd.readthedocs.io/en/stable/operator-manual/high_availability/)
- [ArgoCD Config Management Plugins](https://argo-cd.readthedocs.io/en/stable/operator-manual/config-management-plugins/)
- [ArgoCD Kustomize](https://argo-cd.readthedocs.io/en/stable/user-guide/kustomize/)
