# Changelog

All notable changes to this project will be documented in this file.

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