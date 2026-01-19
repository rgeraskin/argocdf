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

# Quiet mode with markdown file output
argocdf -q -f md-fields:pr-comment.md

# Multiple file outputs
argocdf -f md-fields:pr.md -f html-side-by-side:report.html

# Unified diff for patch workflows
argocdf --stdout unified
argocdf -f unified:changes.patch

# Summary only in terminal
argocdf --stdout summary

# Use external diff tool for side-by-side view
ARGOCDF_EXTERNAL_DIFF="delta --side-by-side" argocdf
```

## Flags

### Kubernetes Flags

| Flag | Short | Description | Default |
|------|-------|-------------|---------|
| `--kubeconfig` | `-k` | Path to kubeconfig file | `~/.kube/config` |
| `--context` | | Kubernetes context to use | (from kubeconfig) |
| `--namespace` | `-n` | ArgoCD namespace to search | `argocd` |
| `--all-namespaces` | `-A` | Search all namespaces | `false` |

### Git Flags

| Flag | Short | Description | Default |
|------|-------|-------------|---------|
| `--repo-dir` | `-r` | Path to git repository | Current directory |
| `--repo-url` | | Repository URL for matching ArgoCD apps | Auto-detected |
| `--base` | | Base branch for comparison | `main` or `master` |
| `--target` | | Target branch for comparison | Current HEAD |

### Rendering Flags

| Flag | Description | Default |
|------|-------------|---------|
| `--kube-version` | Kubernetes version for rendering | Auto-detected |
| `--kustomize-enable-helm` | Enable Helm chart inflation via kustomize | `false` |
| `--kustomize-build-options` | Additional kustomize build options (space-separated) | (none) |
| `--kustomize-load-restrictor` | Load restrictor mode (e.g., `LoadRestrictionsNone`) | (none) |

### Output Flags

| Flag | Short | Description | Default |
|------|-------|-------------|---------|
| `--stdout` | | Terminal output format: `fields`, `summary`, `unified`, `none` | `fields` |
| `--file` | `-f` | File output in `format:path` (can be repeated) | (none) |
| `--quiet` | `-q` | Suppress terminal output (same as `--stdout none`) | `false` |
| `--context-lines` | `-U` | Number of context lines in unified diff output (-1 for unlimited) | `3` |

**File output formats:**
- `md-fields` - GitHub-flavored markdown with field-level diffs
- `md-unified` - Markdown with unified diff format
- `html-side-by-side` - Interactive HTML with side-by-side diff
- `unified` - Patch-compatible unified diff

### Recursion Flags

| Flag | Description | Default |
|------|-------------|---------|
| `--no-recursive` | Disable apps-of-apps recursion | `false` |
| `--max-depth` | Maximum recursion depth | `10` |

## Output Examples

### GitHub PR Comments

Generate markdown output for GitHub PR comments:

```bash
argocdf -q -f md-fields:diff.md
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
