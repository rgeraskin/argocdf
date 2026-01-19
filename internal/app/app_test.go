// Package app provides tests for the main application orchestrator.
package app

import (
	"testing"

	"github.com/charmbracelet/log"

	"github.com/rgeraskin/argocdf/internal/cluster"
	"github.com/rgeraskin/argocdf/internal/config"
	"github.com/rgeraskin/argocdf/internal/git"
	"github.com/rgeraskin/argocdf/internal/testutil"
)

func TestFilterAffectedApps(t *testing.T) {
	tests := []struct {
		name         string
		apps         []cluster.Application
		repoURL      string
		changedFiles *git.ChangedFiles
		wantCount    int
		wantNames    []string
	}{
		{
			name:         "no apps",
			apps:         []cluster.Application{},
			repoURL:      "https://github.com/org/repo",
			changedFiles: testutil.TestChangedFiles([]string{"app/deployment.yaml"}, nil, nil),
			wantCount:    0,
			wantNames:    nil,
		},
		{
			name: "single app with matching path",
			apps: []cluster.Application{
				testutil.TestApp("my-app", "argocd", "https://github.com/org/repo", "app"),
			},
			repoURL:      "https://github.com/org/repo",
			changedFiles: testutil.TestChangedFiles([]string{"app/deployment.yaml"}, nil, nil),
			wantCount:    1,
			wantNames:    []string{"my-app"},
		},
		{
			name: "single app with non-matching path",
			apps: []cluster.Application{
				testutil.TestApp("my-app", "argocd", "https://github.com/org/repo", "app"),
			},
			repoURL:      "https://github.com/org/repo",
			changedFiles: testutil.TestChangedFiles([]string{"other/deployment.yaml"}, nil, nil),
			wantCount:    0,
			wantNames:    nil,
		},
		{
			name: "app with different repo URL",
			apps: []cluster.Application{
				testutil.TestApp("my-app", "argocd", "https://github.com/other/repo", "app"),
			},
			repoURL:      "https://github.com/org/repo",
			changedFiles: testutil.TestChangedFiles([]string{"app/deployment.yaml"}, nil, nil),
			wantCount:    0,
			wantNames:    nil,
		},
		{
			name: "multiple apps - some affected",
			apps: []cluster.Application{
				testutil.TestApp("app1", "argocd", "https://github.com/org/repo", "app1"),
				testutil.TestApp("app2", "argocd", "https://github.com/org/repo", "app2"),
				testutil.TestApp("app3", "argocd", "https://github.com/org/repo", "app3"),
			},
			repoURL:      "https://github.com/org/repo",
			changedFiles: testutil.TestChangedFiles([]string{"app1/values.yaml", "app3/templates/deployment.yaml"}, nil, nil),
			wantCount:    2,
			wantNames:    []string{"app1", "app3"},
		},
		{
			name: "URL normalization - git@ to https",
			apps: []cluster.Application{
				testutil.TestApp("my-app", "argocd", "git@github.com:org/repo.git", "app"),
			},
			repoURL:      "https://github.com/org/repo",
			changedFiles: testutil.TestChangedFiles([]string{"app/deployment.yaml"}, nil, nil),
			wantCount:    1,
			wantNames:    []string{"my-app"},
		},
		{
			name: "URL normalization - ssh:// format",
			apps: []cluster.Application{
				testutil.TestApp("my-app", "argocd", "ssh://git@github.com/org/repo.git", "app"),
			},
			repoURL:      "https://github.com/org/repo",
			changedFiles: testutil.TestChangedFiles([]string{"app/deployment.yaml"}, nil, nil),
			wantCount:    1,
			wantNames:    []string{"my-app"},
		},
		{
			name: "nested path changes",
			apps: []cluster.Application{
				testutil.TestApp("my-app", "argocd", "https://github.com/org/repo", "apps/production/app"),
			},
			repoURL:      "https://github.com/org/repo",
			changedFiles: testutil.TestChangedFiles([]string{"apps/production/app/values.yaml"}, nil, nil),
			wantCount:    1,
			wantNames:    []string{"my-app"},
		},
		{
			name: "modified files affect app",
			apps: []cluster.Application{
				testutil.TestApp("my-app", "argocd", "https://github.com/org/repo", "app"),
			},
			repoURL:      "https://github.com/org/repo",
			changedFiles: testutil.TestChangedFiles(nil, []string{"app/values.yaml"}, nil),
			wantCount:    1,
			wantNames:    []string{"my-app"},
		},
		{
			name: "deleted files affect app",
			apps: []cluster.Application{
				testutil.TestApp("my-app", "argocd", "https://github.com/org/repo", "app"),
			},
			repoURL:      "https://github.com/org/repo",
			changedFiles: testutil.TestChangedFiles(nil, nil, []string{"app/old-config.yaml"}),
			wantCount:    1,
			wantNames:    []string{"my-app"},
		},
		{
			name: "app with empty path - no match",
			apps: []cluster.Application{
				testutil.TestApp("my-app", "argocd", "https://github.com/org/repo", ""),
			},
			repoURL:      "https://github.com/org/repo",
			changedFiles: testutil.TestChangedFiles([]string{"app/deployment.yaml"}, nil, nil),
			wantCount:    0,
			wantNames:    nil,
		},
		{
			name: "path with trailing slash handling",
			apps: []cluster.Application{
				testutil.TestApp("my-app", "argocd", "https://github.com/org/repo", "app/"),
			},
			repoURL:      "https://github.com/org/repo",
			changedFiles: testutil.TestChangedFiles([]string{"app/deployment.yaml"}, nil, nil),
			wantCount:    1,
			wantNames:    []string{"my-app"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a minimal App for testing filterAffectedApps
			cfg := &config.Config{
				RepoURL: tt.repoURL,
			}
			logger := log.New(nil)
			logger.SetLevel(log.FatalLevel) // Suppress log output in tests
			app := &App{
				cfg:    cfg,
				logger: logger,
			}

			got := app.filterAffectedApps(tt.apps, tt.changedFiles)

			if len(got) != tt.wantCount {
				t.Errorf("filterAffectedApps() got %d apps, want %d", len(got), tt.wantCount)
			}

			// Verify specific app names if expected
			if tt.wantNames != nil {
				gotNames := make([]string, len(got))
				for i, a := range got {
					gotNames[i] = a.ObjectMeta.Name
				}
				for _, wantName := range tt.wantNames {
					found := false
					for _, gotName := range gotNames {
						if gotName == wantName {
							found = true
							break
						}
					}
					if !found {
						t.Errorf("filterAffectedApps() missing expected app %q, got %v", wantName, gotNames)
					}
				}
			}
		})
	}
}

func TestChangedFilesHasChangesInPath(t *testing.T) {
	tests := []struct {
		name    string
		files   *git.ChangedFiles
		dirPath string
		want    bool
	}{
		{
			name:    "file directly in path",
			files:   testutil.TestChangedFiles([]string{"app/deployment.yaml"}, nil, nil),
			dirPath: "app",
			want:    true,
		},
		{
			name:    "file in nested path",
			files:   testutil.TestChangedFiles([]string{"app/templates/deployment.yaml"}, nil, nil),
			dirPath: "app",
			want:    true,
		},
		{
			name:    "file not in path",
			files:   testutil.TestChangedFiles([]string{"other/deployment.yaml"}, nil, nil),
			dirPath: "app",
			want:    false,
		},
		{
			name:    "partial path match - should not match",
			files:   testutil.TestChangedFiles([]string{"application/deployment.yaml"}, nil, nil),
			dirPath: "app",
			want:    false,
		},
		{
			name:    "empty path - no match due to trailing slash logic",
			files:   testutil.TestChangedFiles([]string{"app/deployment.yaml"}, nil, nil),
			dirPath: "",
			want:    false, // empty path becomes "/" which doesn't match
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.files.HasChangesInPath(tt.dirPath); got != tt.want {
				t.Errorf("HasChangesInPath(%q) = %v, want %v", tt.dirPath, got, tt.want)
			}
		})
	}
}
