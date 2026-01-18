# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**argocdf** is an ArgoCD Diff Tool - a Go CLI that analyzes Git changes and displays manifest diffs for all ArgoCD applications affected by those changes. It renders manifests from both branches using Helm/Kustomize and computes semantic diffs.

## Common Commands

```bash
# Build
make build          # Build binary to ./argocdf
make install        # Install to GOPATH/bin

# Test
make test           # Run tests with verbose output
make test-coverage  # Generate coverage report

# Development
make dev            # Run with verbose logging
make lint           # Run golangci-lint
make fmt            # Format code (go fmt + goimports)
make vet            # Run go vet

# Dependencies
make deps           # Download dependencies
make tidy           # Tidy go.mod
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
| `internal/cluster` | K8s client, ArgoCD Application operations |
| `internal/git` | Repository operations, change detection |
| `internal/render` | Helm/Kustomize manifest rendering |
| `internal/diff` | Manifest comparison, apps-of-apps discovery |
| `internal/output` | Output writers (terminal, markdown, HTML, unified) |

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

When rendering local Helm charts, argocdf automatically runs `helm dependency build` if:
1. The chart has a `Chart.yaml` with a `dependencies:` section
2. The `charts/` subdirectory is missing or empty

This ensures charts with dependencies (like umbrella charts) render correctly without manual setup.

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

**File output** (`-f/--file format:path`):
- `md` - GitHub-flavored markdown with collapsible sections
- `md-atlantis` - Atlantis-style markdown
- `html-side-by-side` - Interactive HTML with side-by-side diff
- `unified` - Patch-compatible unified diff

```bash
# Quiet mode with markdown file output
./argocdf -q -f md:pr-comment.md

# Multiple file outputs
./argocdf -f md:pr.md -f html-side-by-side:report.html

# Unified diff for patch workflows
./argocdf --stdout unified
./argocdf -f unified:changes.patch

# Summary only in terminal
./argocdf --stdout summary

# Use external diff tool (e.g., delta for side-by-side)
ARGOCDF_EXTERNAL_DIFF="delta --side-by-side" ./argocdf
```

## Test Data

`testdata/` contains real ArgoCD Application manifests for testing:
- `apps.yaml` - Application manifests
- `crd.yaml` - CRD definitions
