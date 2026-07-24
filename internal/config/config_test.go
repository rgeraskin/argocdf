package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseFileOutput(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		want    FileOutput
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid md-fields format",
			spec: "md-fields:output.md",
			want: FileOutput{Format: "md-fields", Path: "output.md"},
		},
		{
			name: "valid html-side-by-side format",
			spec: "html-side-by-side:report.html",
			want: FileOutput{Format: "html-side-by-side", Path: "report.html"},
		},
		{
			name: "valid md-unified format",
			spec: "md-unified:diff.md",
			want: FileOutput{Format: "md-unified", Path: "diff.md"},
		},
		{
			name: "valid unified format",
			spec: "unified:changes.patch",
			want: FileOutput{Format: "unified", Path: "changes.patch"},
		},
		{
			name: "path with directories",
			spec: "md-fields:/tmp/output/diff.md",
			want: FileOutput{Format: "md-fields", Path: "/tmp/output/diff.md"},
		},
		{
			name: "path with colons (Windows-like)",
			spec: "md-fields:C:/Users/test/output.md",
			want: FileOutput{Format: "md-fields", Path: "C:/Users/test/output.md"},
		},
		{
			name: "split option with default value",
			spec: "md-unified,split:pr-comment.md",
			want: FileOutput{Format: "md-unified", Path: "pr-comment.md", SplitMax: DefaultSplitMax},
		},
		{
			name: "split option with explicit value",
			spec: "md-fields,split=30000:pr-comment.md",
			want: FileOutput{Format: "md-fields", Path: "pr-comment.md", SplitMax: 30000},
		},
		{
			name: "split option with path containing commas",
			spec: "md-fields,split:out,put.md",
			want: FileOutput{Format: "md-fields", Path: "out,put.md", SplitMax: DefaultSplitMax},
		},
		{
			name: "path with comma but no options",
			spec: "md-fields:out,put.md",
			want: FileOutput{Format: "md-fields", Path: "out,put.md"},
		},
		{
			name:    "split option on unified format",
			spec:    "unified,split:changes.patch",
			wantErr: true,
			errMsg:  "only valid for md-fields and md-unified",
		},
		{
			name:    "split option on html format",
			spec:    "html-side-by-side,split:report.html",
			wantErr: true,
			errMsg:  "only valid for md-fields and md-unified",
		},
		{
			name:    "split option with non-integer value",
			spec:    "md-fields,split=big:out.md",
			wantErr: true,
			errMsg:  "invalid split value",
		},
		{
			name:    "split option with too-small value",
			spec:    "md-fields,split=100:out.md",
			wantErr: true,
			errMsg:  "too small",
		},
		{
			name:    "unknown option",
			spec:    "md-fields,chunk=100:out.md",
			wantErr: true,
			errMsg:  "unknown file output option",
		},
		{
			name:    "missing colon separator",
			spec:    "md-output.md",
			wantErr: true,
			errMsg:  "invalid file output format",
		},
		{
			name:    "unknown format",
			spec:    "json:output.json",
			wantErr: true,
			errMsg:  "unknown file format",
		},
		{
			name:    "empty path",
			spec:    "md-fields:",
			wantErr: true,
			errMsg:  "file path cannot be empty",
		},
		{
			name:    "empty spec",
			spec:    "",
			wantErr: true,
			errMsg:  "invalid file output format",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseFileOutput(tt.spec)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ParseFileOutput() expected error containing %q, got nil", tt.errMsg)
					return
				}
				if tt.errMsg != "" && !contains(err.Error(), tt.errMsg) {
					t.Errorf("ParseFileOutput() error = %v, want error containing %q", err, tt.errMsg)
				}
				return
			}
			if err != nil {
				t.Errorf("ParseFileOutput() unexpected error = %v", err)
				return
			}
			if got != tt.want {
				t.Errorf("ParseFileOutput() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConfigValidate(t *testing.T) {
	// Create a temporary git repository for testing
	tmpDir := t.TempDir()
	gitDir := filepath.Join(tmpDir, ".git")
	if err := os.Mkdir(gitDir, 0755); err != nil {
		t.Fatalf("Failed to create .git dir: %v", err)
	}

	tests := []struct {
		name    string
		config  *Config
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid config with fields stdout",
			config: &Config{
				RepoPath:     tmpDir,
				StdoutFormat: "fields",
				MaxDepth:     10,
				Concurrency:  1,
			},
		},
		{
			name: "valid config with summary stdout",
			config: &Config{
				RepoPath:     tmpDir,
				StdoutFormat: "summary",
				MaxDepth:     10,
				Concurrency:  1,
			},
		},
		{
			name: "valid config with unified stdout",
			config: &Config{
				RepoPath:     tmpDir,
				StdoutFormat: "unified",
				MaxDepth:     10,
				Concurrency:  4,
			},
		},
		{
			name: "valid config with none stdout and file output",
			config: &Config{
				RepoPath:     tmpDir,
				StdoutFormat: "none",
				FileOutputs:  []FileOutput{{Format: "md-fields", Path: "output.md"}},
				MaxDepth:     10,
				Concurrency:  1,
			},
		},
		{
			name: "missing repo path",
			config: &Config{
				RepoPath:     "",
				StdoutFormat: "fields",
				MaxDepth:     10,
			},
			wantErr: true,
			errMsg:  "repository path is required",
		},
		{
			name: "invalid repo path",
			config: &Config{
				RepoPath:     "/nonexistent/path",
				StdoutFormat: "fields",
				MaxDepth:     10,
			},
			wantErr: true,
			errMsg:  "repository path error",
		},
		{
			name: "repo path is file not directory",
			config: func() *Config {
				f, _ := os.CreateTemp(t.TempDir(), "file")
				_ = f.Close()
				return &Config{
					RepoPath:     f.Name(),
					StdoutFormat: "fields",
					MaxDepth:     10,
				}
			}(),
			wantErr: true,
			errMsg:  "not a directory",
		},
		{
			name: "directory without .git",
			config: &Config{
				RepoPath:     t.TempDir(),
				StdoutFormat: "fields",
				MaxDepth:     10,
			},
			wantErr: true,
			errMsg:  "not a git repository",
		},
		{
			name: "invalid stdout format",
			config: &Config{
				RepoPath:     tmpDir,
				StdoutFormat: "invalid",
				MaxDepth:     10,
			},
			wantErr: true,
			errMsg:  "invalid stdout format",
		},
		{
			name: "valid config with argocd renderer",
			config: &Config{
				RepoPath:     tmpDir,
				StdoutFormat: "fields",
				MaxDepth:     10,
				Concurrency:  1,
				Renderer:     RendererArgoCD,
			},
		},
		{
			name: "invalid renderer",
			config: &Config{
				RepoPath:     tmpDir,
				StdoutFormat: "fields",
				MaxDepth:     10,
				Concurrency:  1,
				Renderer:     "helmfile",
			},
			wantErr: true,
			errMsg:  "invalid renderer",
		},
		{
			name: "valid repo-creds sources",
			config: &Config{
				RepoPath:     tmpDir,
				StdoutFormat: "fields",
				MaxDepth:     10,
				Concurrency:  1,
				RepoCreds:    RepoCredsLocal,
			},
		},
		{
			name: "invalid repo-creds source",
			config: &Config{
				RepoPath:     tmpDir,
				StdoutFormat: "fields",
				MaxDepth:     10,
				Concurrency:  1,
				RepoCreds:    "vault",
			},
			wantErr: true,
			errMsg:  "invalid repo-creds source",
		},
		{
			name: "negative lint timeout",
			config: &Config{
				RepoPath:     tmpDir,
				StdoutFormat: "fields",
				MaxDepth:     10,
				Concurrency:  1,
				Lint:         []string{"true"},
				LintTimeout:  -time.Second,
			},
			wantErr: true,
			errMsg:  "lint timeout must be positive",
		},
		{
			name: "none stdout without file outputs",
			config: &Config{
				RepoPath:     tmpDir,
				StdoutFormat: "none",
				FileOutputs:  nil,
				MaxDepth:     10,
			},
			wantErr: true,
			errMsg:  "no output configured",
		},
		{
			name: "max depth less than 1",
			config: &Config{
				RepoPath:     tmpDir,
				StdoutFormat: "fields",
				MaxDepth:     0,
			},
			wantErr: true,
			errMsg:  "max depth must be at least 1",
		},
		{
			name: "concurrency less than 1",
			config: &Config{
				RepoPath:     tmpDir,
				StdoutFormat: "fields",
				MaxDepth:     10,
				Concurrency:  0,
			},
			wantErr: true,
			errMsg:  "concurrency must be at least 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if tt.wantErr {
				if err == nil {
					t.Errorf("Validate() expected error containing %q, got nil", tt.errMsg)
					return
				}
				if tt.errMsg != "" && !contains(err.Error(), tt.errMsg) {
					t.Errorf("Validate() error = %v, want error containing %q", err, tt.errMsg)
				}
				return
			}
			if err != nil {
				t.Errorf("Validate() unexpected error = %v", err)
			}
		})
	}
}

func TestConfigWithDefaults(t *testing.T) {
	tests := []struct {
		name   string
		config *Config
		check  func(*Config) bool
		desc   string
	}{
		{
			name:   "sets default context",
			config: &Config{},
			check:  func(c *Config) bool { return c.Context == DefaultContext },
			desc:   "Context should be set to default",
		},
		{
			name:   "sets default argocd namespace",
			config: &Config{},
			check:  func(c *Config) bool { return c.ArgoCDNamespace == DefaultNamespace },
			desc:   "ArgoCDNamespace should be set to default",
		},
		{
			name:   "sets default repo-creds source",
			config: &Config{},
			check:  func(c *Config) bool { return c.RepoCreds == RepoCredsCluster },
			desc:   "RepoCreds should default to cluster",
		},
		{
			name:   "all-namespaces normalizes to the match-everything pattern",
			config: &Config{AllNamespaces: true},
			check: func(c *Config) bool {
				return len(c.ApplicationNamespaces) == 1 && c.ApplicationNamespaces[0] == "*"
			},
			desc: "-A should become ApplicationNamespaces=[\"*\"]",
		},
		{
			name:   "all-namespaces overrides an explicit namespace list",
			config: &Config{AllNamespaces: true, ApplicationNamespaces: []string{"team-a"}},
			check: func(c *Config) bool {
				return len(c.ApplicationNamespaces) == 1 && c.ApplicationNamespaces[0] == "*"
			},
			desc: "'*' subsumes any other entry",
		},
		{
			name:   "application namespaces list is preserved without -A",
			config: &Config{ApplicationNamespaces: []string{"team-a", "team-*"}},
			check: func(c *Config) bool {
				return len(c.ApplicationNamespaces) == 2 && c.ApplicationNamespaces[1] == "team-*"
			},
			desc: "explicit list should be preserved",
		},
		{
			name:   "sets default stdout format",
			config: &Config{},
			check:  func(c *Config) bool { return c.StdoutFormat == DefaultStdoutFormat },
			desc:   "StdoutFormat should be set to default",
		},
		{
			name:   "sets default max depth",
			config: &Config{},
			check:  func(c *Config) bool { return c.MaxDepth == DefaultMaxDepth },
			desc:   "MaxDepth should be set to default",
		},
		{
			name:   "preserves custom context",
			config: &Config{Context: "custom-context"},
			check:  func(c *Config) bool { return c.Context == "custom-context" },
			desc:   "Custom context should be preserved",
		},
		{
			name:   "preserves custom argocd namespace",
			config: &Config{ArgoCDNamespace: "custom-ns"},
			check:  func(c *Config) bool { return c.ArgoCDNamespace == "custom-ns" },
			desc:   "Custom ArgoCD namespace should be preserved",
		},
		{
			name:   "preserves custom stdout format",
			config: &Config{StdoutFormat: "summary"},
			check:  func(c *Config) bool { return c.StdoutFormat == "summary" },
			desc:   "Custom stdout format should be preserved",
		},
		{
			name:   "preserves custom max depth",
			config: &Config{MaxDepth: 5},
			check:  func(c *Config) bool { return c.MaxDepth == 5 },
			desc:   "Custom max depth should be preserved",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.config.WithDefaults()
			if !tt.check(tt.config) {
				t.Errorf("WithDefaults() failed: %s", tt.desc)
			}
		})
	}
}

func TestNew(t *testing.T) {
	cfg := New()

	if cfg.Context != DefaultContext {
		t.Errorf("New() Context = %v, want %v", cfg.Context, DefaultContext)
	}
	if cfg.ArgoCDNamespace != DefaultNamespace {
		t.Errorf("New() ArgoCDNamespace = %v, want %v", cfg.ArgoCDNamespace, DefaultNamespace)
	}
	if cfg.RepoCreds != DefaultRepoCreds {
		t.Errorf("New() RepoCreds = %v, want %v", cfg.RepoCreds, DefaultRepoCreds)
	}
	if cfg.StdoutFormat != DefaultStdoutFormat {
		t.Errorf("New() StdoutFormat = %v, want %v", cfg.StdoutFormat, DefaultStdoutFormat)
	}
	if cfg.MaxDepth != DefaultMaxDepth {
		t.Errorf("New() MaxDepth = %v, want %v", cfg.MaxDepth, DefaultMaxDepth)
	}
}

func TestNativeOnlyFlagWarnings(t *testing.T) {
	tests := []struct {
		name           string
		renderer       string
		skipRefreshSet bool
		addReposSet    bool
		wantCount      int
	}{
		{"native renderer never warns", RendererNative, true, true, 0},
		{"argocd renderer with flag defaults never warns", RendererArgoCD, false, false, 0},
		{"argocd renderer warns per explicitly set flag", RendererArgoCD, true, true, 2},
		{"argocd renderer warns for helm-add-repos alone", RendererArgoCD, false, true, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Config{Renderer: tt.renderer}
			got := c.NativeOnlyFlagWarnings(tt.skipRefreshSet, tt.addReposSet)
			if len(got) != tt.wantCount {
				t.Errorf("NativeOnlyFlagWarnings() = %v, want %d warnings", got, tt.wantCount)
			}
		})
	}
}

// contains checks if s contains substr
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsAt(s, substr))
}

func containsAt(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
