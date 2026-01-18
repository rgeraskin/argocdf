# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**argocdf** is an ArgoCD Diff Tool - a Go CLI that analyzes Git changes and displays manifest diffs for all ArgoCD applications affected by those changes. It renders manifests from both branches using Helm/Kustomize and computes semantic diffs.

## Common Commands

```bash
# Build
make build          # Build binary to ./bin/argocdf
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
| `internal/output` | Terminal and HTML output writers |

### Design Patterns

- **Interface-based design**: `Renderer`, `Writer`, `Differ` allow pluggable implementations
- **Factory pattern**: `internal/app/factory.go` handles dependency injection
- **Multi-writer**: Simultaneous output to terminal and HTML
- **Queue-based recursion**: Depth-limited discovery for apps-of-apps pattern

## ArgoCD Application Support

The tool supports both single and multi-source applications:
- `spec.source` - Single source configuration
- `spec.sources[]` - Multiple sources (Helm + values from git, etc.)

Apps-of-apps pattern is handled via recursive discovery with configurable max depth.

## Running the Tool

```bash
# Basic usage (auto-detects everything)
./bin/argocdf

# Specify branches
./bin/argocdf --base main --target feature-branch

# Different output formats
./bin/argocdf -o html --html-file report.html
./bin/argocdf -o both  # terminal + html

# Different cluster/namespace
./bin/argocdf --context prod-cluster -n argocd
```

## Test Data

`testdata/` contains real ArgoCD Application manifests for testing:
- `apps.yaml` - Application manifests
- `crd.yaml` - CRD definitions
