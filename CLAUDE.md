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
| `internal/cluster` | K8s client, ArgoCD Application types (via type aliases) |
| `internal/git` | Repository operations, change detection, URL normalization |
| `internal/render` | Helm/Kustomize manifest rendering |
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

### Automatic Helm Dependency Management

When rendering local Helm charts, argocdf automatically runs `helm dependency build` if the chart has a `Chart.yaml` with a `dependencies:` section. Helm is smart enough to skip already-fetched dependencies, so this is safe to run unconditionally.

This ensures charts with dependencies (like umbrella charts) render correctly without manual setup.

Caveat: `helm dependency build` never adds or refreshes classic HTTP(S) chart repositories — their index must already be in the local helm cache, which on a fresh CI runner it never is (fails with "no cached repository"/"no repository definition"). The opt-in `--helm-add-repos` flag makes argocdf register those repos first, deduplicated per URL per run: a URL already registered under any name is only refreshed (helm matches dependency repos by URL, so no new entry is written), and only unknown URLs are added, under hash-derived names that can never clobber a user's entry. It still mutates local helm state either way — index caches are refreshed and unknown URLs get new repositories.yaml entries — which is why it is off by default; the missing-repo error message points at it.

## Running the Tool

```bash
# Basic usage (auto-detects everything)
./argocdf

# Specify branches
./argocdf --base main --target feature-branch

# Different cluster/namespace
./argocdf --context prod-cluster -n argocd

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

Markdown formats accept the `split[=N]` option (options ride on the format
segment, so paths with commas/colons stay intact): a report larger than N bytes
(default 60000, under GitHub's 65,536-char comment cap) is written as multiple
self-contained part files — `pr-comment.md`, `pr-comment.2.md`, ... — each with
the upsert marker, a `part i/N` heading, and balanced `<details>`/fences. Apps
stay whole within a part; an app that alone exceeds the limit is split at
resource boundaries; a single oversized resource diff is truncated with a note.
Packing lives in `internal/output/markdown.go` (`assembleParts`/`packBodies`).

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

Configuration flows through Cobra flags into `internal/config.Config`. Every flag
is also settable via an environment variable named `ARGOCDF_<FLAG>` (flag name
upper-cased, dashes → underscores), e.g. `--repo-dir` → `ARGOCDF_REPO_DIR`.

This is wired by `bindEnv` in `cmd/argocdf/main.go`, which runs first in
`runMain`. It uses **viper `AutomaticEnv()`** with prefix `ARGOCDF` and a
`-`→`_` key replacer as a pure env lookup — `viper.BindPFlags` is not called
because it isn't needed (env values are applied directly through `pflag.Set`, so
they are parsed by each flag's own type and invalid values fail fast with a typed
error). In this setup `v.IsSet(name)` is true only when the env var is actually
set (non-empty).

Precedence — **explicit flag > environment variable > default** — is enforced by
the `f.Changed` guard in `bindEnv`: flags the user passed on the command line are
skipped, so their env vars never override them. That guard is the load-bearing
line; keep it if you refactor this.

Two variables are read directly (no flag equivalent): `ARGOCDF_EXTERNAL_DIFF`
(`internal/output/terminal.go`) and `KUBECONFIG` (`internal/config/detect.go`).

## Test Data

`testdata/` contains real ArgoCD Application manifests for testing:
- `apps.yaml` - Application manifests
- `crd.yaml` - CRD definitions
