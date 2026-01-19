// Package cluster provides ArgoCD Application operations.
package cluster

import (
	"context"
	"fmt"

	argoapp "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ArgoCD Application GVR (GroupVersionResource).
var ApplicationGVR = schema.GroupVersionResource{
	Group:    "argoproj.io",
	Version:  "v1alpha1",
	Resource: "applications",
}

// Type aliases for ArgoCD types - provides cleaner imports for consumers.
type (
	Application               = argoapp.Application
	ApplicationSpec           = argoapp.ApplicationSpec
	ApplicationSource         = argoapp.ApplicationSource
	ApplicationSourceHelm     = argoapp.ApplicationSourceHelm
	HelmParameter             = argoapp.HelmParameter
	HelmFileParameter         = argoapp.HelmFileParameter
	ApplicationSourceKustomize = argoapp.ApplicationSourceKustomize
	ApplicationDestination    = argoapp.ApplicationDestination
)

// ApplicationService provides operations on ArgoCD Applications.
type ApplicationService struct {
	client *Client
}

// NewApplicationService creates a new ApplicationService.
func NewApplicationService(client *Client) *ApplicationService {
	return &ApplicationService{client: client}
}

// List retrieves all ArgoCD Applications from the specified namespace.
func (s *ApplicationService) List(ctx context.Context, namespace string) ([]Application, error) {
	list, err := s.client.dynamicClient.Resource(ApplicationGVR).
		Namespace(namespace).
		List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list applications in namespace %s: %w", namespace, err)
	}

	return s.convertList(list)
}

// ListAllNamespaces retrieves ArgoCD Applications from all namespaces.
func (s *ApplicationService) ListAllNamespaces(ctx context.Context) ([]Application, error) {
	list, err := s.client.dynamicClient.Resource(ApplicationGVR).
		List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list applications across all namespaces: %w", err)
	}

	return s.convertList(list)
}

// Get retrieves a specific ArgoCD Application.
func (s *ApplicationService) Get(ctx context.Context, namespace, name string) (*Application, error) {
	obj, err := s.client.dynamicClient.Resource(ApplicationGVR).
		Namespace(namespace).
		Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get application %s/%s: %w", namespace, name, err)
	}

	return s.convertOne(obj)
}

// convertList converts an unstructured list to typed Applications.
func (s *ApplicationService) convertList(list *unstructured.UnstructuredList) ([]Application, error) {
	apps := make([]Application, 0, len(list.Items))

	for _, item := range list.Items {
		app, err := s.convertOne(&item)
		if err != nil {
			return nil, err
		}
		apps = append(apps, *app)
	}

	return apps, nil
}

// convertOne converts an unstructured object to a typed Application.
func (s *ApplicationService) convertOne(obj *unstructured.Unstructured) (*Application, error) {
	var app Application
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &app)
	if err != nil {
		return nil, fmt.Errorf("failed to convert unstructured to Application: %w", err)
	}

	return &app, nil
}

// FilterByRepoURL filters applications that match the given repository URL.
func FilterByRepoURL(apps []Application, repoURL string) []Application {
	filtered := make([]Application, 0)

	for _, app := range apps {
		sources := app.Spec.GetSources()
		for _, source := range sources {
			if normalizeRepoURL(source.RepoURL) == normalizeRepoURL(repoURL) {
				filtered = append(filtered, app)
				break
			}
		}
	}

	return filtered
}

// normalizeRepoURL normalizes a git URL for comparison.
func normalizeRepoURL(url string) string {
	// Remove trailing .git
	if len(url) > 4 && url[len(url)-4:] == ".git" {
		url = url[:len(url)-4]
	}
	// Remove trailing slash
	if len(url) > 0 && url[len(url)-1] == '/' {
		url = url[:len(url)-1]
	}
	return url
}
