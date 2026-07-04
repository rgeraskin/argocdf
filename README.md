# argocdf - ArgoCD Diff Tool

`argocdf` shows manifest diffs for ArgoCD applications affected by your PR changes. It supports the apps-of-apps pattern with recursive discovery.

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

## Requirements

- `helm` binary in PATH (for Helm chart rendering)
- `kustomize` binary in PATH (for Kustomize rendering)
- Access to a Kubernetes cluster with ArgoCD Applications
- Go 1.24+ (only if installing via `go install` or building from source)

## Installation

### Homebrew

```bash
brew install rgeraskin/homebrew/argocdf
```

### Go

```bash
go install github.com/rgeraskin/argocdf/cmd/argocdf@latest
```

### Binary download

Grab a prebuilt archive for your OS/arch from the
[releases page](https://github.com/rgeraskin/argocdf/releases), then extract
`argocdf` into a directory on your `PATH`.

### From source

```bash
git clone https://github.com/rgeraskin/argocdf.git
cd argocdf
mise run build   # produces ./argocdf
```

## Usage

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

# Quiet mode with markdown file output
argocdf -q -f md-fields:pr-comment.md

# Multiple file outputs
argocdf -f md-fields:pr.md -f html-side-by-side:report.html

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

| Flag               | Short | Description                | Default           |
|--------------------|-------|----------------------------|-------------------|
| `--kubeconfig`     | `-k`  | Path to kubeconfig file    | `~/.kube/config`  |
| `--context`        |       | Kubernetes context to use  | (from kubeconfig) |
| `--namespace`      | `-n`  | ArgoCD namespace to search | `argocd`          |
| `--all-namespaces` | `-A`  | Search all namespaces      | `false`           |

### Git Flags

| Flag         | Short | Description                             | Default            |
|--------------|-------|-----------------------------------------|--------------------|
| `--repo-dir` | `-r`  | Path to git repository                  | Current directory  |
| `--repo-url` |       | Repository URL for matching ArgoCD apps | Auto-detected      |
| `--base`     |       | Base branch for comparison              | `main` or `master` |
| `--target`   |       | Target branch for comparison            | Current HEAD       |

### Rendering Flags

| Flag                          | Description                                                              | Default       |
|-------------------------------|--------------------------------------------------------------------------|---------------|
| `--kube-version`              | Kubernetes version for rendering                                         | Auto-detected |
| `--kustomize-enable-helm`     | Enable Helm chart inflation via kustomize                                | `false`       |
| `--kustomize-build-options`   | Additional kustomize build options (space-separated)                     | (none)        |
| `--kustomize-load-restrictor` | Load restrictor mode (e.g., `LoadRestrictionsNone`)                      | (none)        |
| `--helm-skip-refresh`         | Skip refreshing the repo cache during `helm dependency build`            | `true`        |
| `--no-api-versions`           | Do not pass cluster-discovered API versions to helm via `--api-versions` | `false`       |

### Output Flags

| Flag              | Short | Description                                                                        | Default  |
|-------------------|-------|------------------------------------------------------------------------------------|----------|
| `--stdout`        |       | Terminal output format: `fields`, `summary`, `unified`, `none`                     | `fields` |
| `--file`          | `-f`  | File output in `format:path` (can be repeated)                                     | (none)   |
| `--quiet`         | `-q`  | Suppress terminal output (same as `--stdout none`)                                 | `false`  |
| `--verbose`       | `-v`  | Enable debug logging (config resolution, cache hits, per-app processing) to stderr | `false`  |
| `--context-lines` | `-U`  | Number of context lines in unified diff output (-1 for unlimited)                  | `3`      |

**File output formats:**
- `md-fields` - GitHub-flavored markdown with field-level diffs
- `md-unified` - Markdown with unified diff format
- `html-side-by-side` - Interactive HTML with side-by-side diff
- `unified` - Patch-compatible unified diff

### Recursion Flags

| Flag             | Description                    | Default |
|------------------|--------------------------------|---------|
| `--no-recursive` | Disable apps-of-apps recursion | `false` |
| `--max-depth`    | Maximum recursion depth        | `10`    |

### Concurrency Flags

| Flag            | Description                                           | Default        |
|-----------------|-------------------------------------------------------|----------------|
| `--concurrency` | Applications to render in parallel (`1` = sequential) | Number of CPUs |

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

Every flag can also be set through an environment variable. The variable name is
the flag name upper-cased, with dashes replaced by underscores, and prefixed with
`ARGOCDF_`:

| Flag                          | Environment variable                 |
|-------------------------------|--------------------------------------|
| `--repo-dir`                  | `ARGOCDF_REPO_DIR`                    |
| `--repo-url`                  | `ARGOCDF_REPO_URL`                    |
| `--namespace`                 | `ARGOCDF_NAMESPACE`                   |
| `--context`                   | `ARGOCDF_CONTEXT`                     |
| `--kustomize-enable-helm`     | `ARGOCDF_KUSTOMIZE_ENABLE_HELM`      |
| `--kustomize-load-restrictor` | `ARGOCDF_KUSTOMIZE_LOAD_RESTRICTOR`  |
| ...                           | `ARGOCDF_<FLAG>` for any other flag   |

Precedence is **explicit flag > environment variable > default**, so a flag passed
on the command line always wins over the matching environment variable. Empty
variables are ignored.

```bash
# These two invocations are equivalent
argocdf --repo-dir /path/to/repo --kustomize-enable-helm

export ARGOCDF_REPO_DIR=/path/to/repo
export ARGOCDF_KUSTOMIZE_ENABLE_HELM=true
argocdf
```

Two additional variables are read directly (they have no flag equivalent):

| Variable                | Description                                                                       |
|-------------------------|-----------------------------------------------------------------------------------|
| `ARGOCDF_EXTERNAL_DIFF` | External diff command for side-by-side terminal output (e.g. `delta --side-by-side`) |
| `KUBECONFIG`            | Standard kubeconfig path, honored during cluster auto-detection                   |

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

This project uses [mise](https://mise.jdx.dev/) to pin toolchain versions
(`.mise.toml`) and define tasks. Run `mise tasks` to list them.

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
mise run e2e:bootstrap   # create a kind cluster with ArgoCD CRDs
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
