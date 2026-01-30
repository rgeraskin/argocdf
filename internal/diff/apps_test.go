package diff

import (
	"testing"

	"github.com/rgeraskin/argocdf/internal/cluster"
)

func TestDiscoverApplications(t *testing.T) {
	discoverer := NewAppDiscoverer()

	tests := []struct {
		name      string
		content   string
		wantCount int
		wantNames []string
		wantErr   bool
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

// TestParseApplicationSpec_HelmConfig verifies that all Helm config fields are correctly parsed.
// This is a regression test for the bug where parameters and fileParameters were not being parsed.
func TestParseApplicationSpec_HelmConfig(t *testing.T) {
	discoverer := NewAppDiscoverer()

	content := `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: helm-app
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://github.com/example/repo.git
    path: charts/my-app
    targetRevision: HEAD
    helm:
      releaseName: my-release
      version: "1.2.3"
      valueFiles:
        - values.yaml
        - values-prod.yaml
      values: |
        replicaCount: 3
      parameters:
        - name: image.tag
          value: v1.0.0
        - name: service.port
          value: "8080"
          forceString: true
      fileParameters:
        - name: config.data
          path: config.json
        - name: secrets.data
          path: secrets.yaml
  destination:
    server: https://kubernetes.default.svc
    namespace: default
`

	apps, err := discoverer.DiscoverApplications(content)
	if err != nil {
		t.Fatalf("DiscoverApplications() error = %v", err)
	}
	if len(apps) != 1 {
		t.Fatalf("DiscoverApplications() got %d apps, want 1", len(apps))
	}

	app := apps[0]
	if app.Spec.Source == nil {
		t.Fatal("Spec.Source is nil")
	}
	if app.Spec.Source.Helm == nil {
		t.Fatal("Spec.Source.Helm is nil")
	}

	helm := app.Spec.Source.Helm

	// Check releaseName
	if helm.ReleaseName != "my-release" {
		t.Errorf("Helm.ReleaseName = %s, want my-release", helm.ReleaseName)
	}

	// Check version
	if helm.Version != "1.2.3" {
		t.Errorf("Helm.Version = %s, want 1.2.3", helm.Version)
	}

	// Check valueFiles
	if len(helm.ValueFiles) != 2 {
		t.Errorf("Helm.ValueFiles length = %d, want 2", len(helm.ValueFiles))
	} else {
		if helm.ValueFiles[0] != "values.yaml" {
			t.Errorf("Helm.ValueFiles[0] = %s, want values.yaml", helm.ValueFiles[0])
		}
		if helm.ValueFiles[1] != "values-prod.yaml" {
			t.Errorf("Helm.ValueFiles[1] = %s, want values-prod.yaml", helm.ValueFiles[1])
		}
	}

	// Check inline values
	if helm.Values == "" {
		t.Error("Helm.Values is empty")
	}

	// Check parameters (this was the bug!)
	if len(helm.Parameters) != 2 {
		t.Errorf("Helm.Parameters length = %d, want 2", len(helm.Parameters))
	} else {
		if helm.Parameters[0].Name != "image.tag" || helm.Parameters[0].Value != "v1.0.0" {
			t.Errorf("Helm.Parameters[0] = {%s, %s}, want {image.tag, v1.0.0}",
				helm.Parameters[0].Name, helm.Parameters[0].Value)
		}
		if helm.Parameters[1].Name != "service.port" || helm.Parameters[1].Value != "8080" {
			t.Errorf("Helm.Parameters[1] = {%s, %s}, want {service.port, 8080}",
				helm.Parameters[1].Name, helm.Parameters[1].Value)
		}
		if !helm.Parameters[1].ForceString {
			t.Error("Helm.Parameters[1].ForceString should be true")
		}
	}

	// Check fileParameters (this was also the bug!)
	if len(helm.FileParameters) != 2 {
		t.Errorf("Helm.FileParameters length = %d, want 2", len(helm.FileParameters))
	} else {
		if helm.FileParameters[0].Name != "config.data" || helm.FileParameters[0].Path != "config.json" {
			t.Errorf("Helm.FileParameters[0] = {%s, %s}, want {config.data, config.json}",
				helm.FileParameters[0].Name, helm.FileParameters[0].Path)
		}
		if helm.FileParameters[1].Name != "secrets.data" || helm.FileParameters[1].Path != "secrets.yaml" {
			t.Errorf("Helm.FileParameters[1] = {%s, %s}, want {secrets.data, secrets.yaml}",
				helm.FileParameters[1].Name, helm.FileParameters[1].Path)
		}
	}
}

// TestParseApplicationSpec_KustomizeConfig verifies that Kustomize config is correctly parsed.
func TestParseApplicationSpec_KustomizeConfig(t *testing.T) {
	discoverer := NewAppDiscoverer()

	content := `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: kustomize-app
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://github.com/example/repo.git
    path: overlays/prod
    targetRevision: HEAD
    kustomize:
      namePrefix: prod-
      nameSuffix: -v1
      images:
        - nginx:1.21
        - redis:6.2
      commonLabels:
        env: production
        team: platform
      commonAnnotations:
        owner: devops
  destination:
    server: https://kubernetes.default.svc
    namespace: default
`

	apps, err := discoverer.DiscoverApplications(content)
	if err != nil {
		t.Fatalf("DiscoverApplications() error = %v", err)
	}
	if len(apps) != 1 {
		t.Fatalf("DiscoverApplications() got %d apps, want 1", len(apps))
	}

	app := apps[0]
	if app.Spec.Source == nil {
		t.Fatal("Spec.Source is nil")
	}
	if app.Spec.Source.Kustomize == nil {
		t.Fatal("Spec.Source.Kustomize is nil")
	}

	kust := app.Spec.Source.Kustomize

	if kust.NamePrefix != "prod-" {
		t.Errorf("Kustomize.NamePrefix = %s, want prod-", kust.NamePrefix)
	}
	if kust.NameSuffix != "-v1" {
		t.Errorf("Kustomize.NameSuffix = %s, want -v1", kust.NameSuffix)
	}
	if len(kust.Images) != 2 {
		t.Errorf("Kustomize.Images length = %d, want 2", len(kust.Images))
	}
	if len(kust.CommonLabels) != 2 {
		t.Errorf("Kustomize.CommonLabels length = %d, want 2", len(kust.CommonLabels))
	}
	if kust.CommonLabels["env"] != "production" {
		t.Errorf("Kustomize.CommonLabels[env] = %s, want production", kust.CommonLabels["env"])
	}
}

// TestParseApplicationSpec_MultiSource verifies that multi-source applications are correctly parsed.
func TestParseApplicationSpec_MultiSource(t *testing.T) {
	discoverer := NewAppDiscoverer()

	content := `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: multi-source-app
  namespace: argocd
spec:
  project: default
  sources:
    - repoURL: https://github.com/example/repo.git
      path: base
      targetRevision: HEAD
      ref: values
    - repoURL: https://charts.example.com
      chart: my-chart
      targetRevision: 1.0.0
      helm:
        releaseName: my-release
        valueFiles:
          - $values/values.yaml
        parameters:
          - name: env
            value: prod
  destination:
    server: https://kubernetes.default.svc
    namespace: default
`

	apps, err := discoverer.DiscoverApplications(content)
	if err != nil {
		t.Fatalf("DiscoverApplications() error = %v", err)
	}
	if len(apps) != 1 {
		t.Fatalf("DiscoverApplications() got %d apps, want 1", len(apps))
	}

	app := apps[0]
	if len(app.Spec.Sources) != 2 {
		t.Fatalf("Spec.Sources length = %d, want 2", len(app.Spec.Sources))
	}

	// Check first source (ref source)
	src1 := app.Spec.Sources[0]
	if src1.Ref != "values" {
		t.Errorf("Sources[0].Ref = %s, want values", src1.Ref)
	}

	// Check second source (helm chart)
	src2 := app.Spec.Sources[1]
	if src2.Chart != "my-chart" {
		t.Errorf("Sources[1].Chart = %s, want my-chart", src2.Chart)
	}
	if src2.Helm == nil {
		t.Fatal("Sources[1].Helm is nil")
	}
	if len(src2.Helm.Parameters) != 1 {
		t.Errorf("Sources[1].Helm.Parameters length = %d, want 1", len(src2.Helm.Parameters))
	}
	if src2.Helm.Parameters[0].Name != "env" || src2.Helm.Parameters[0].Value != "prod" {
		t.Errorf("Sources[1].Helm.Parameters[0] = {%s, %s}, want {env, prod}",
			src2.Helm.Parameters[0].Name, src2.Helm.Parameters[0].Value)
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

	t.Run("UpdatePending updates existing pending app", func(t *testing.T) {
		queue := NewAppDiffQueue(10)

		// Add app with "old" spec (simulates cluster spec)
		oldSpec := &cluster.ApplicationSpec{Project: "cluster-spec"}
		queue.Add(QueuedApp{Name: "app1", Namespace: "ns1", Depth: 0, Spec: oldSpec})

		// Update with "new" spec (simulates git spec discovered by parent)
		newSpec := &cluster.ApplicationSpec{Project: "git-spec"}
		gitOldSpec := &cluster.ApplicationSpec{Project: "git-old-spec"}
		updated := queue.UpdatePending(QueuedApp{
			Name:      "app1",
			Namespace: "ns1",
			Spec:      newSpec,
			OldSpec:   gitOldSpec,
			ParentApp: "parent-app",
		})

		if !updated {
			t.Error("UpdatePending should return true for pending app")
		}

		// Verify spec was updated
		app := queue.Next()
		if app.Spec.Project != "git-spec" {
			t.Errorf("Spec.Project = %s, want git-spec", app.Spec.Project)
		}
		if app.OldSpec.Project != "git-old-spec" {
			t.Errorf("OldSpec.Project = %s, want git-old-spec", app.OldSpec.Project)
		}
		if app.ParentApp != "parent-app" {
			t.Errorf("ParentApp = %s, want parent-app", app.ParentApp)
		}
	})

	t.Run("UpdatePending returns false for processed app", func(t *testing.T) {
		queue := NewAppDiffQueue(10)
		queue.Add(QueuedApp{Name: "app1", Namespace: "ns1", Depth: 0})
		queue.Next() // process it

		updated := queue.UpdatePending(QueuedApp{Name: "app1", Namespace: "ns1"})
		if updated {
			t.Error("UpdatePending should return false for processed app")
		}
	})

	t.Run("UpdatePending returns false for non-existent app", func(t *testing.T) {
		queue := NewAppDiffQueue(10)

		updated := queue.UpdatePending(QueuedApp{Name: "app1", Namespace: "ns1"})
		if updated {
			t.Error("UpdatePending should return false for non-existent app")
		}
	})

	t.Run("RequeueProcessed requeues already-processed app", func(t *testing.T) {
		queue := NewAppDiffQueue(10)

		// Add and process app with cluster spec
		clusterSpec := &cluster.ApplicationSpec{Project: "cluster"}
		queue.Add(QueuedApp{Name: "app1", Namespace: "ns1", Depth: 0, Spec: clusterSpec})
		queue.Next() // process it

		// Requeue with git spec (simulates parent discovering spec change)
		gitSpec := &cluster.ApplicationSpec{Project: "git"}
		gitOldSpec := &cluster.ApplicationSpec{Project: "git-old"}
		requeued := queue.RequeueProcessed(QueuedApp{
			Name:      "app1",
			Namespace: "ns1",
			Depth:     1,
			Spec:      gitSpec,
			OldSpec:   gitOldSpec,
			ParentApp: "parent",
		})

		if !requeued {
			t.Error("RequeueProcessed should return true for processed app")
		}

		// Verify app is back in queue with new spec
		if queue.IsEmpty() {
			t.Fatal("queue should not be empty after requeue")
		}

		app := queue.Next()
		if app.Spec.Project != "git" {
			t.Errorf("Spec.Project = %s, want git", app.Spec.Project)
		}
		if app.OldSpec.Project != "git-old" {
			t.Errorf("OldSpec.Project = %s, want git-old", app.OldSpec.Project)
		}
	})

	t.Run("RequeueProcessed returns false for pending app", func(t *testing.T) {
		queue := NewAppDiffQueue(10)
		queue.Add(QueuedApp{Name: "app1", Namespace: "ns1", Depth: 0})

		requeued := queue.RequeueProcessed(QueuedApp{Name: "app1", Namespace: "ns1"})
		if requeued {
			t.Error("RequeueProcessed should return false for pending (not processed) app")
		}
	})

	t.Run("RequeueProcessed returns false for non-existent app", func(t *testing.T) {
		queue := NewAppDiffQueue(10)

		requeued := queue.RequeueProcessed(QueuedApp{Name: "app1", Namespace: "ns1"})
		if requeued {
			t.Error("RequeueProcessed should return false for non-existent app")
		}
	})
}
