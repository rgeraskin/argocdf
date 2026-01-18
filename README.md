# argocdf - ArgoCD Diff Tool

`argocdf` shows manifest diffs for ArgoCD applications affected by your PR changes. It supports the apps-of-apps pattern with recursive discovery.

## Features

- **Auto-detection**: Automatically detects repository path, branches, and cluster version
- **Multi-source support**: Handles applications with `spec.source` and `spec.sources[]` configurations
- **Helm rendering**: Renders Helm charts (local and remote repositories, including OCI)
- **Kustomize rendering**: Renders Kustomize directories
- **Apps-of-apps**: Recursively discovers and diffs child applications from rendered manifests
- **Multiple outputs**: Terminal (colored) and HTML report formats
- **Semantic diffing**: Identifies added, removed, and modified resources by kind/name/namespace

## Requirements

- Go 1.21+
- `helm` binary in PATH (for Helm chart rendering)
- `kustomize` binary in PATH (for Kustomize rendering)
- Access to a Kubernetes cluster with ArgoCD Applications

## Installation

```bash
# From source
go install github.com/rgeraskin/argocdf/cmd/argocdf@latest

# Or build locally
make build
```

## Usage

```bash
# Basic usage (auto-detects everything)
argocdf

# Specify branches explicitly
argocdf --base main --target feature/new-service

# Use a different Kubernetes context
argocdf --context my-cluster

# Scan all namespaces for ArgoCD applications
argocdf --all-namespaces

# Generate HTML report
argocdf --output both --html-file report.html

# Show only summary (no detailed diff)
argocdf --summary-only

# Side-by-side diff using external tool
export KUBECTL_EXTERNAL_DIFF="delta --side-by-side --hunk-header-style=omit --file-style=omit"
argocdf --side-by-side

# Generate GitHub-compatible output for PR comments
argocdf --output html --github --html-file diff.md
```

## Flags

| Flag | Short | Description | Default |
|------|-------|-------------|---------|
| `--kubeconfig` | `-k` | Path to kubeconfig file | `~/.kube/config` |
| `--context` | | Kubernetes context | `pp-admin-aws` |
| `--namespace` | `-n` | ArgoCD namespace to search | `argocd` |
| `--all-namespaces` | `-A` | Search all namespaces | `false` |
| `--repo` | `-r` | Path to git repository | Current directory |
| `--base` | | Base branch for comparison | `main` or `master` |
| `--target` | | Target branch for comparison | Current HEAD |
| `--kube-version` | | Kubernetes version for rendering | Auto-detected |
| `--output` | `-o` | Output format: terminal, html, both | `terminal` |
| `--html-file` | | HTML output file path | `argocdf-report.html` |
| `--no-recursive` | | Disable apps-of-apps recursion | `false` |
| `--max-depth` | | Maximum recursion depth | `10` |
| `--side-by-side` | | Show side-by-side YAML diff | `false` |
| `--summary-only` | | Show only affected apps without detailed diff | `false` |
| `--github` | | Output GitHub-compatible markdown (pasteable to PR comments) | `false` |

## GitHub PR Comments

Use the `--github` flag to generate output that can be pasted directly into GitHub PR comments:

```bash
argocdf --output html --github --html-file diff.md
cat diff.md  # Copy and paste into GitHub PR comment
```

The output uses:
- GitHub-flavored markdown with collapsible `<details>` sections
- Emoji badges for change types (🟢 added, 🔴 removed, 🟡 modified)
- `diff` code blocks for syntax-highlighted changes
- Markdown tables for the summary

## Side-by-Side Diff

The `--side-by-side` flag enables a side-by-side diff view similar to the ArgoCD web UI.

### Terminal Output

For terminal output, argocdf uses the `KUBECTL_EXTERNAL_DIFF` environment variable (following kubectl's convention). Set this to your preferred diff tool:

**Recommended setup with [delta](https://github.com/dandavison/delta):**
```bash
export KUBECTL_EXTERNAL_DIFF="delta --side-by-side --hunk-header-style=omit --file-style=omit"
```

**Alternative with [difftastic](https://github.com/Wilfred/difftastic):**
```bash
export KUBECTL_EXTERNAL_DIFF="difft --display side-by-side-show-both"
```

Then run:
```bash
argocdf --side-by-side
```

### HTML Output

For HTML output, `--side-by-side` uses [diff2html](https://diff2html.xyz/) to render GitHub-style side-by-side diffs with syntax highlighting:

```bash
argocdf --output html --side-by-side --html-file report.html
```

## How It Works

1. **Connects to cluster**: Uses kubeconfig to connect to the Kubernetes cluster
2. **Fetches applications**: Lists ArgoCD Applications from the specified namespace(s)
3. **Analyzes changes**: Compares git branches to find changed files
4. **Filters affected apps**: Identifies applications whose source paths have changes
5. **Renders manifests**: For each affected app:
   - Checks out base branch → renders manifests
   - Checks out target branch → renders manifests
6. **Computes diffs**: Compares rendered manifests to identify changes
7. **Recursive discovery**: If diffs contain new Application CRDs, adds them to the queue
8. **Outputs results**: Displays colored terminal output and/or generates HTML report

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

## Development

```bash
# Build
make build

# Run tests
make test

# Run with verbose output
make dev

# Format code
make fmt

# Run linter
make lint
```

## Project Structure

```
argocdf/
├── cmd/argocdf/
│   └── main.go                 # CLI entry point (Cobra)
├── internal/
│   ├── app/
│   │   ├── app.go              # Main orchestrator
│   │   └── factory.go          # Dependency injection
│   ├── config/
│   │   ├── config.go           # Configuration struct
│   │   └── detect.go           # Auto-detection logic
│   ├── cluster/
│   │   ├── client.go           # K8s client-go wrapper
│   │   └── applications.go     # ArgoCD Application operations
│   ├── git/
│   │   ├── repository.go       # Repository operations
│   │   └── diff.go             # Changed files detection
│   ├── render/
│   │   ├── renderer.go         # Renderer interface
│   │   ├── helm.go             # Helm rendering
│   │   ├── kustomize.go        # Kustomize rendering
│   │   └── multisource.go      # Multi-source handling
│   ├── diff/
│   │   ├── differ.go           # Diff interface
│   │   ├── manifest.go         # Manifest comparison
│   │   └── apps.go             # Recursive app discovery
│   ├── output/
│   │   ├── output.go           # Output interface
│   │   ├── terminal.go         # Terminal output
│   │   └── html.go             # HTML report
│   ├── types/
│   │   └── types.go            # Shared types
│   └── errors/
│       └── errors.go           # Custom error types
├── go.mod
├── Makefile
└── README.md
```

## License

MIT
