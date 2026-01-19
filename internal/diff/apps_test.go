package diff

import (
	"testing"

	"github.com/rgeraskin/argocdf/internal/cluster"
)

func TestDiscoverApplications(t *testing.T) {
	discoverer := NewAppDiscoverer()

	tests := []struct {
		name        string
		content     string
		wantCount   int
		wantNames   []string
		wantErr     bool
	}{
		{
			name:      "empty content",
			content:   "",
			wantCount: 0,
			wantNames: nil,
		},
		{
			name: "single application",
			content: `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: my-app
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://github.com/example/repo.git
    path: apps/my-app
    targetRevision: HEAD
  destination:
    server: https://kubernetes.default.svc
    namespace: default
`,
			wantCount: 1,
			wantNames: []string{"my-app"},
		},
		{
			name: "multiple applications",
			content: `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: app-one
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://github.com/example/repo.git
    path: apps/one
    targetRevision: HEAD
  destination:
    server: https://kubernetes.default.svc
    namespace: default
---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: app-two
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://github.com/example/repo.git
    path: apps/two
    targetRevision: HEAD
  destination:
    server: https://kubernetes.default.svc
    namespace: default
`,
			wantCount: 2,
			wantNames: []string{"app-one", "app-two"},
		},
		{
			name: "mixed resources - only applications returned",
			content: `apiVersion: v1
kind: ConfigMap
metadata:
  name: my-config
  namespace: default
data:
  key: value
---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: my-app
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://github.com/example/repo.git
    path: apps/my-app
    targetRevision: HEAD
  destination:
    server: https://kubernetes.default.svc
    namespace: default
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-deployment
  namespace: default
spec:
  replicas: 1
`,
			wantCount: 1,
			wantNames: []string{"my-app"},
		},
		{
			name: "non-argocd application ignored",
			content: `apiVersion: other.io/v1
kind: Application
metadata:
  name: other-app
  namespace: default
spec:
  foo: bar
`,
			wantCount: 0,
			wantNames: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apps, err := discoverer.DiscoverApplications(tt.content)
			if (err != nil) != tt.wantErr {
				t.Errorf("DiscoverApplications() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if len(apps) != tt.wantCount {
				t.Errorf("DiscoverApplications() got %d apps, want %d", len(apps), tt.wantCount)
				return
			}
			for i, name := range tt.wantNames {
				if apps[i].Name != name {
					t.Errorf("DiscoverApplications() app[%d].Name = %s, want %s", i, apps[i].Name, name)
				}
			}
		})
	}
}

func TestFindNewApplications(t *testing.T) {
	discoverer := NewAppDiscoverer()

	tests := []struct {
		name       string
		oldContent string
		newContent string
		wantCount  int
		wantNames  []string
		wantErr    bool
	}{
		{
			name:       "empty old content - all apps are new",
			oldContent: "",
			newContent: `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: new-app
  namespace: argocd
spec:
  project: default
`,
			wantCount: 1,
			wantNames: []string{"new-app"},
		},
		{
			name: "no new apps",
			oldContent: `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: existing-app
  namespace: argocd
spec:
  project: default
`,
			newContent: `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: existing-app
  namespace: argocd
spec:
  project: default
`,
			wantCount: 0,
			wantNames: nil,
		},
		{
			name: "one new app added",
			oldContent: `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: existing-app
  namespace: argocd
spec:
  project: default
`,
			newContent: `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: existing-app
  namespace: argocd
spec:
  project: default
---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: new-app
  namespace: argocd
spec:
  project: default
`,
			wantCount: 1,
			wantNames: []string{"new-app"},
		},
		{
			name: "app removed - not returned as new",
			oldContent: `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: removed-app
  namespace: argocd
spec:
  project: default
`,
			newContent: "",
			wantCount:  0,
			wantNames:  nil,
		},
		{
			name: "same name different namespace is new",
			oldContent: `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: my-app
  namespace: argocd
spec:
  project: default
`,
			newContent: `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: my-app
  namespace: argocd
spec:
  project: default
---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: my-app
  namespace: other-namespace
spec:
  project: default
`,
			wantCount: 1,
			wantNames: []string{"my-app"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apps, err := discoverer.FindNewApplications(tt.oldContent, tt.newContent)
			if (err != nil) != tt.wantErr {
				t.Errorf("FindNewApplications() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if len(apps) != tt.wantCount {
				t.Errorf("FindNewApplications() got %d apps, want %d", len(apps), tt.wantCount)
				return
			}
			for i, name := range tt.wantNames {
				if apps[i].Name != name {
					t.Errorf("FindNewApplications() app[%d].Name = %s, want %s", i, apps[i].Name, name)
				}
			}
		})
	}
}

func TestFindModifiedApplications(t *testing.T) {
	discoverer := NewAppDiscoverer()

	tests := []struct {
		name       string
		oldContent string
		newContent string
		wantCount  int
		wantNames  []string
		wantErr    bool
	}{
		{
			name:       "empty old content - no modified apps",
			oldContent: "",
			newContent: `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: new-app
  namespace: argocd
spec:
  project: default
`,
			wantCount: 0,
			wantNames: nil,
		},
		{
			name: "identical apps - no modifications",
			oldContent: `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: my-app
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://github.com/example/repo.git
    path: apps/my-app
    targetRevision: HEAD
`,
			newContent: `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: my-app
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://github.com/example/repo.git
    path: apps/my-app
    targetRevision: HEAD
`,
			wantCount: 0,
			wantNames: nil,
		},
		{
			name: "modified app - path changed",
			oldContent: `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: my-app
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://github.com/example/repo.git
    path: apps/old-path
    targetRevision: HEAD
`,
			newContent: `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: my-app
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://github.com/example/repo.git
    path: apps/new-path
    targetRevision: HEAD
`,
			wantCount: 1,
			wantNames: []string{"my-app"},
		},
		{
			name: "modified app - helm parameters changed",
			oldContent: `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: my-app
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://github.com/example/repo.git
    path: apps/my-app
    helm:
      parameters:
        - name: replicas
          value: "1"
`,
			newContent: `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: my-app
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://github.com/example/repo.git
    path: apps/my-app
    helm:
      parameters:
        - name: replicas
          value: "3"
`,
			wantCount: 1,
			wantNames: []string{"my-app"},
		},
		{
			name: "mixed scenario - one new, one modified, one unchanged",
			oldContent: `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: unchanged-app
  namespace: argocd
spec:
  project: default
  source:
    path: apps/unchanged
---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: modified-app
  namespace: argocd
spec:
  project: default
  source:
    path: apps/old-path
`,
			newContent: `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: unchanged-app
  namespace: argocd
spec:
  project: default
  source:
    path: apps/unchanged
---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: modified-app
  namespace: argocd
spec:
  project: default
  source:
    path: apps/new-path
---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: new-app
  namespace: argocd
spec:
  project: default
  source:
    path: apps/new
`,
			wantCount: 1,
			wantNames: []string{"modified-app"},
		},
		{
			name: "app removed - not returned as modified",
			oldContent: `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: removed-app
  namespace: argocd
spec:
  project: default
`,
			newContent: "",
			wantCount:  0,
			wantNames:  nil,
		},
		{
			name: "whitespace-only change detected as modification",
			oldContent: `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: my-app
  namespace: argocd
spec:
  project: default
`,
			newContent: `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: my-app
  namespace: argocd
spec:
  project: default
  source:
    path: apps/added
`,
			wantCount: 1,
			wantNames: []string{"my-app"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apps, err := discoverer.FindModifiedApplications(tt.oldContent, tt.newContent)
			if (err != nil) != tt.wantErr {
				t.Errorf("FindModifiedApplications() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if len(apps) != tt.wantCount {
				t.Errorf("FindModifiedApplications() got %d apps, want %d", len(apps), tt.wantCount)
				return
			}
			for i, name := range tt.wantNames {
				if apps[i].Name != name {
					t.Errorf("FindModifiedApplications() app[%d].Name = %s, want %s", i, apps[i].Name, name)
				}
			}
		})
	}
}

func TestFindModifiedApplications_SpecsParsed(t *testing.T) {
	discoverer := NewAppDiscoverer()

	oldContent := `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: my-app
  namespace: argocd
spec:
  project: dev
  source:
    repoURL: https://github.com/example/repo.git
    path: apps/old-path
    targetRevision: main
  destination:
    server: https://kubernetes.default.svc
    namespace: old-namespace
`
	newContent := `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: my-app
  namespace: argocd
spec:
  project: prod
  source:
    repoURL: https://github.com/example/repo.git
    path: apps/new-path
    targetRevision: release
  destination:
    server: https://kubernetes.default.svc
    namespace: new-namespace
`

	apps, err := discoverer.FindModifiedApplications(oldContent, newContent)
	if err != nil {
		t.Fatalf("FindModifiedApplications() error = %v", err)
	}
	if len(apps) != 1 {
		t.Fatalf("FindModifiedApplications() got %d apps, want 1", len(apps))
	}

	app := apps[0]

	// Verify old spec
	if app.OldSpec.Source == nil {
		t.Fatal("OldSpec.Source is nil")
	}
	if app.OldSpec.Source.Path != "apps/old-path" {
		t.Errorf("OldSpec.Source.Path = %s, want apps/old-path", app.OldSpec.Source.Path)
	}
	if app.OldSpec.Source.TargetRevision != "main" {
		t.Errorf("OldSpec.Source.TargetRevision = %s, want main", app.OldSpec.Source.TargetRevision)
	}
	if app.OldSpec.Destination.Namespace != "old-namespace" {
		t.Errorf("OldSpec.Destination.Namespace = %s, want old-namespace", app.OldSpec.Destination.Namespace)
	}
	if app.OldSpec.Project != "dev" {
		t.Errorf("OldSpec.Project = %s, want dev", app.OldSpec.Project)
	}

	// Verify new spec
	if app.NewSpec.Source == nil {
		t.Fatal("NewSpec.Source is nil")
	}
	if app.NewSpec.Source.Path != "apps/new-path" {
		t.Errorf("NewSpec.Source.Path = %s, want apps/new-path", app.NewSpec.Source.Path)
	}
	if app.NewSpec.Source.TargetRevision != "release" {
		t.Errorf("NewSpec.Source.TargetRevision = %s, want release", app.NewSpec.Source.TargetRevision)
	}
	if app.NewSpec.Destination.Namespace != "new-namespace" {
		t.Errorf("NewSpec.Destination.Namespace = %s, want new-namespace", app.NewSpec.Destination.Namespace)
	}
	if app.NewSpec.Project != "prod" {
		t.Errorf("NewSpec.Project = %s, want prod", app.NewSpec.Project)
	}
}

func TestAppDiffQueue(t *testing.T) {
	t.Run("basic queue operations", func(t *testing.T) {
		queue := NewAppDiffQueue(10)

		if !queue.IsEmpty() {
			t.Error("new queue should be empty")
		}

		added := queue.Add(QueuedApp{Name: "app1", Namespace: "ns1", Depth: 0})
		if !added {
			t.Error("Add() should return true for new app")
		}

		if queue.IsEmpty() {
			t.Error("queue should not be empty after Add")
		}

		app := queue.Next()
		if app == nil {
			t.Fatal("Next() returned nil")
		}
		if app.Name != "app1" {
			t.Errorf("Next() returned app %s, want app1", app.Name)
		}

		if !queue.IsEmpty() {
			t.Error("queue should be empty after Next")
		}

		if queue.ProcessedCount() != 1 {
			t.Errorf("ProcessedCount() = %d, want 1", queue.ProcessedCount())
		}
	})

	t.Run("prevents duplicate processing", func(t *testing.T) {
		queue := NewAppDiffQueue(10)

		queue.Add(QueuedApp{Name: "app1", Namespace: "ns1", Depth: 0})
		queue.Next() // process it

		// Try to add same app again
		added := queue.Add(QueuedApp{Name: "app1", Namespace: "ns1", Depth: 1})
		if added {
			t.Error("Add() should return false for already processed app")
		}
	})

	t.Run("respects max depth", func(t *testing.T) {
		queue := NewAppDiffQueue(2)

		added := queue.Add(QueuedApp{Name: "app1", Namespace: "ns1", Depth: 0})
		if !added {
			t.Error("Add() should succeed at depth 0")
		}

		added = queue.Add(QueuedApp{Name: "app2", Namespace: "ns1", Depth: 1})
		if !added {
			t.Error("Add() should succeed at depth 1")
		}

		added = queue.Add(QueuedApp{Name: "app3", Namespace: "ns1", Depth: 2})
		if added {
			t.Error("Add() should fail at depth >= maxDepth")
		}
	})

	t.Run("OldSpec support", func(t *testing.T) {
		queue := NewAppDiffQueue(10)

		oldSpec := &cluster.ApplicationSpec{Project: "old-project"}
		newSpec := &cluster.ApplicationSpec{Project: "new-project"}

		queue.Add(QueuedApp{
			Name:      "modified-app",
			Namespace: "ns1",
			Depth:     0,
			Spec:      newSpec,
			OldSpec:   oldSpec,
		})

		app := queue.Next()
		if app.Spec.Project != "new-project" {
			t.Errorf("Spec.Project = %s, want new-project", app.Spec.Project)
		}
		if app.OldSpec.Project != "old-project" {
			t.Errorf("OldSpec.Project = %s, want old-project", app.OldSpec.Project)
		}
	})
}
