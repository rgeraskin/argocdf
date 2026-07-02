package cluster

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestConvertOne(t *testing.T) {
	service := &ApplicationService{}

	tests := []struct {
		name    string
		obj     *unstructured.Unstructured
		wantErr bool
		check   func(*Application) error
	}{
		{
			name: "basic application with single source",
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "argoproj.io/v1alpha1",
					"kind":       "Application",
					"metadata": map[string]interface{}{
						"name":      "test-app",
						"namespace": "argocd",
					},
					"spec": map[string]interface{}{
						"project": "default",
						"source": map[string]interface{}{
							"repoURL":        "https://github.com/example/repo",
							"path":           "charts/myapp",
							"targetRevision": "main",
						},
						"destination": map[string]interface{}{
							"server":    "https://kubernetes.default.svc",
							"namespace": "default",
						},
					},
				},
			},
			wantErr: false,
			check: func(app *Application) error {
				if app.Name != "test-app" {
					t.Errorf("Name = %q, want %q", app.Name, "test-app")
				}
				if app.Namespace != "argocd" {
					t.Errorf("Namespace = %q, want %q", app.Namespace, "argocd")
				}
				if app.Spec.Project != "default" {
					t.Errorf("Spec.Project = %q, want %q", app.Spec.Project, "default")
				}
				if app.Spec.Source == nil {
					t.Error("Spec.Source is nil")
				} else {
					if app.Spec.Source.RepoURL != "https://github.com/example/repo" {
						t.Errorf("Source.RepoURL = %q, want %q", app.Spec.Source.RepoURL, "https://github.com/example/repo")
					}
					if app.Spec.Source.Path != "charts/myapp" {
						t.Errorf("Source.Path = %q, want %q", app.Spec.Source.Path, "charts/myapp")
					}
				}
				return nil
			},
		},
		{
			name: "application with helm config",
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "argoproj.io/v1alpha1",
					"kind":       "Application",
					"metadata": map[string]interface{}{
						"name":      "helm-app",
						"namespace": "argocd",
					},
					"spec": map[string]interface{}{
						"project": "default",
						"source": map[string]interface{}{
							"repoURL":        "https://charts.bitnami.com/bitnami",
							"chart":          "nginx",
							"targetRevision": "15.0.0",
							"helm": map[string]interface{}{
								"releaseName": "my-nginx",
								"values":      "replicaCount: 2",
							},
						},
						"destination": map[string]interface{}{
							"server":    "https://kubernetes.default.svc",
							"namespace": "web",
						},
					},
				},
			},
			wantErr: false,
			check: func(app *Application) error {
				if app.Name != "helm-app" {
					t.Errorf("Name = %q, want %q", app.Name, "helm-app")
				}
				if app.Spec.Source == nil {
					t.Error("Spec.Source is nil")
					return nil
				}
				if app.Spec.Source.Chart != "nginx" {
					t.Errorf("Source.Chart = %q, want %q", app.Spec.Source.Chart, "nginx")
				}
				if app.Spec.Source.Helm == nil {
					t.Error("Source.Helm is nil")
				} else {
					if app.Spec.Source.Helm.ReleaseName != "my-nginx" {
						t.Errorf("Helm.ReleaseName = %q, want %q", app.Spec.Source.Helm.ReleaseName, "my-nginx")
					}
				}
				return nil
			},
		},
		{
			name: "application with multiple sources",
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "argoproj.io/v1alpha1",
					"kind":       "Application",
					"metadata": map[string]interface{}{
						"name":      "multi-source-app",
						"namespace": "argocd",
					},
					"spec": map[string]interface{}{
						"project": "default",
						"sources": []interface{}{
							map[string]interface{}{
								"repoURL":        "https://charts.bitnami.com/bitnami",
								"chart":          "nginx",
								"targetRevision": "15.0.0",
							},
							map[string]interface{}{
								"repoURL":        "https://github.com/example/values",
								"path":           "envs/prod",
								"targetRevision": "main",
							},
						},
						"destination": map[string]interface{}{
							"server":    "https://kubernetes.default.svc",
							"namespace": "default",
						},
					},
				},
			},
			wantErr: false,
			check: func(app *Application) error {
				if app.Name != "multi-source-app" {
					t.Errorf("Name = %q, want %q", app.Name, "multi-source-app")
				}
				sources := app.Spec.Sources
				if len(sources) != 2 {
					t.Errorf("len(Sources) = %d, want 2", len(sources))
					return nil
				}
				if sources[0].Chart != "nginx" {
					t.Errorf("Sources[0].Chart = %q, want %q", sources[0].Chart, "nginx")
				}
				if sources[1].Path != "envs/prod" {
					t.Errorf("Sources[1].Path = %q, want %q", sources[1].Path, "envs/prod")
				}
				return nil
			},
		},
		{
			name: "application with kustomize config",
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "argoproj.io/v1alpha1",
					"kind":       "Application",
					"metadata": map[string]interface{}{
						"name":      "kustomize-app",
						"namespace": "argocd",
					},
					"spec": map[string]interface{}{
						"project": "default",
						"source": map[string]interface{}{
							"repoURL":        "https://github.com/example/repo",
							"path":           "overlays/prod",
							"targetRevision": "main",
							"kustomize": map[string]interface{}{
								"namePrefix": "prod-",
								"images": []interface{}{
									"nginx:1.21",
								},
							},
						},
						"destination": map[string]interface{}{
							"server":    "https://kubernetes.default.svc",
							"namespace": "default",
						},
					},
				},
			},
			wantErr: false,
			check: func(app *Application) error {
				if app.Spec.Source == nil {
					t.Error("Spec.Source is nil")
					return nil
				}
				if app.Spec.Source.Kustomize == nil {
					t.Error("Source.Kustomize is nil")
				} else {
					if app.Spec.Source.Kustomize.NamePrefix != "prod-" {
						t.Errorf("Kustomize.NamePrefix = %q, want %q", app.Spec.Source.Kustomize.NamePrefix, "prod-")
					}
				}
				return nil
			},
		},
		{
			name: "empty object",
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{},
			},
			wantErr: false,
			check: func(app *Application) error {
				// Empty object should unmarshal to zero-value Application
				if app.Name != "" {
					t.Errorf("Name = %q, want empty", app.Name)
				}
				return nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, err := service.convertOne(tt.obj)
			if tt.wantErr {
				if err == nil {
					t.Error("convertOne() expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("convertOne() unexpected error: %v", err)
				return
			}
			if tt.check != nil {
				_ = tt.check(app)
			}
		})
	}
}

func TestConvertList(t *testing.T) {
	service := &ApplicationService{}

	list := &unstructured.UnstructuredList{
		Items: []unstructured.Unstructured{
			{
				Object: map[string]interface{}{
					"apiVersion": "argoproj.io/v1alpha1",
					"kind":       "Application",
					"metadata": map[string]interface{}{
						"name":      "app-1",
						"namespace": "argocd",
					},
					"spec": map[string]interface{}{
						"project": "default",
						"source": map[string]interface{}{
							"repoURL": "https://github.com/example/repo1",
							"path":    "app1",
						},
						"destination": map[string]interface{}{
							"server":    "https://kubernetes.default.svc",
							"namespace": "default",
						},
					},
				},
			},
			{
				Object: map[string]interface{}{
					"apiVersion": "argoproj.io/v1alpha1",
					"kind":       "Application",
					"metadata": map[string]interface{}{
						"name":      "app-2",
						"namespace": "argocd",
					},
					"spec": map[string]interface{}{
						"project": "default",
						"source": map[string]interface{}{
							"repoURL": "https://github.com/example/repo2",
							"path":    "app2",
						},
						"destination": map[string]interface{}{
							"server":    "https://kubernetes.default.svc",
							"namespace": "default",
						},
					},
				},
			},
		},
	}

	apps, err := service.convertList(list)
	if err != nil {
		t.Fatalf("convertList() error: %v", err)
	}

	if len(apps) != 2 {
		t.Fatalf("convertList() returned %d apps, want 2", len(apps))
	}

	if apps[0].Name != "app-1" {
		t.Errorf("apps[0].Name = %q, want %q", apps[0].Name, "app-1")
	}
	if apps[1].Name != "app-2" {
		t.Errorf("apps[1].Name = %q, want %q", apps[1].Name, "app-2")
	}
}

func TestConvertList_Empty(t *testing.T) {
	service := &ApplicationService{}

	list := &unstructured.UnstructuredList{
		Items: []unstructured.Unstructured{},
	}

	apps, err := service.convertList(list)
	if err != nil {
		t.Fatalf("convertList() error: %v", err)
	}

	if len(apps) != 0 {
		t.Errorf("convertList() returned %d apps, want 0", len(apps))
	}
}

func TestFilterByRepoURL(t *testing.T) {
	apps := []Application{
		{},
		{},
		{},
	}
	// Set up test apps manually using the struct
	apps[0].Name = "app-matching"
	apps[0].Spec.Source = &ApplicationSource{
		RepoURL: "https://github.com/example/repo.git",
	}

	apps[1].Name = "app-different"
	apps[1].Spec.Source = &ApplicationSource{
		RepoURL: "https://github.com/other/repo.git",
	}

	apps[2].Name = "app-matching-no-git"
	apps[2].Spec.Source = &ApplicationSource{
		RepoURL: "https://github.com/example/repo", // Same repo, different format
	}

	tests := []struct {
		name      string
		repoURL   string
		wantNames []string
	}{
		{
			name:      "exact match with .git",
			repoURL:   "https://github.com/example/repo.git",
			wantNames: []string{"app-matching", "app-matching-no-git"},
		},
		{
			name:      "match without .git suffix",
			repoURL:   "https://github.com/example/repo",
			wantNames: []string{"app-matching", "app-matching-no-git"},
		},
		{
			name:      "no matches",
			repoURL:   "https://github.com/nonexistent/repo",
			wantNames: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filtered := FilterByRepoURL(apps, tt.repoURL)

			if len(filtered) != len(tt.wantNames) {
				t.Errorf("FilterByRepoURL() returned %d apps, want %d", len(filtered), len(tt.wantNames))
				return
			}

			for i, name := range tt.wantNames {
				if filtered[i].Name != name {
					t.Errorf("filtered[%d].Name = %q, want %q", i, filtered[i].Name, name)
				}
			}
		})
	}
}
