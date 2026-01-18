// Package cluster provides ArgoCD Application operations.
package cluster

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

// ArgoCD Application GVR (GroupVersionResource).
var ApplicationGVR = schema.GroupVersionResource{
	Group:    "argoproj.io",
	Version:  "v1alpha1",
	Resource: "applications",
}

// Application represents an ArgoCD Application with the fields we need.
// We use a custom struct to avoid importing the full ArgoCD types which can
// cause dependency conflicts, but we serialize/deserialize from the actual CRD.
type Application struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ApplicationSpec   `json:"spec,omitempty"`
	Status            ApplicationStatus `json:"status,omitempty"`
}

// ApplicationSpec contains the specification of an ArgoCD Application.
type ApplicationSpec struct {
	Source      *ApplicationSource   `json:"source,omitempty"`
	Sources     []ApplicationSource  `json:"sources,omitempty"`
	Destination ApplicationDest      `json:"destination"`
	Project     string               `json:"project,omitempty"`
	SyncPolicy  *ApplicationSyncPolicy `json:"syncPolicy,omitempty"`
}

// ApplicationSource defines the source of an application.
type ApplicationSource struct {
	RepoURL        string           `json:"repoURL"`
	Path           string           `json:"path,omitempty"`
	TargetRevision string           `json:"targetRevision,omitempty"`
	Chart          string           `json:"chart,omitempty"`
	Ref            string           `json:"ref,omitempty"`
	Helm           *ApplicationSourceHelm      `json:"helm,omitempty"`
	Kustomize      *ApplicationSourceKustomize `json:"kustomize,omitempty"`
}

// ApplicationSourceHelm contains Helm-specific source configuration.
type ApplicationSourceHelm struct {
	ReleaseName     string            `json:"releaseName,omitempty"`
	ValueFiles      []string          `json:"valueFiles,omitempty"`
	Values          string            `json:"values,omitempty"`
	Parameters      []HelmParameter   `json:"parameters,omitempty"`
	FileParameters  []HelmFileParameter `json:"fileParameters,omitempty"`
	Version         string            `json:"version,omitempty"`
	PassCredentials bool              `json:"passCredentials,omitempty"`
}

// HelmParameter represents a Helm parameter override.
type HelmParameter struct {
	Name        string `json:"name,omitempty"`
	Value       string `json:"value,omitempty"`
	ForceString bool   `json:"forceString,omitempty"`
}

// HelmFileParameter represents a Helm file parameter.
type HelmFileParameter struct {
	Name string `json:"name,omitempty"`
	Path string `json:"path,omitempty"`
}

// ApplicationSourceKustomize contains Kustomize-specific source configuration.
type ApplicationSourceKustomize struct {
	NamePrefix string            `json:"namePrefix,omitempty"`
	NameSuffix string            `json:"nameSuffix,omitempty"`
	Images     []string          `json:"images,omitempty"`
	CommonLabels map[string]string `json:"commonLabels,omitempty"`
	CommonAnnotations map[string]string `json:"commonAnnotations,omitempty"`
	Version    string            `json:"version,omitempty"`
}

// ApplicationDest defines the destination cluster and namespace.
type ApplicationDest struct {
	Server    string `json:"server,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
}

// ApplicationSyncPolicy defines the sync policy for an application.
type ApplicationSyncPolicy struct {
	Automated *SyncPolicyAutomated `json:"automated,omitempty"`
}

// SyncPolicyAutomated defines automated sync policy.
type SyncPolicyAutomated struct {
	Prune    bool `json:"prune,omitempty"`
	SelfHeal bool `json:"selfHeal,omitempty"`
}

// ApplicationStatus contains the status of an ArgoCD Application.
type ApplicationStatus struct {
	Health  HealthStatus `json:"health,omitempty"`
	Sync    SyncStatus   `json:"sync,omitempty"`
}

// HealthStatus represents the health status.
type HealthStatus struct {
	Status  string `json:"status,omitempty"`
	Message string `json:"message,omitempty"`
}

// SyncStatus represents the sync status.
type SyncStatus struct {
	Status   string `json:"status,omitempty"`
	Revision string `json:"revision,omitempty"`
}

// GetSources returns all sources for the application.
// This handles both single source (spec.source) and multi-source (spec.sources) formats.
func (a *Application) GetSources() []ApplicationSource {
	if len(a.Spec.Sources) > 0 {
		return a.Spec.Sources
	}
	if a.Spec.Source != nil {
		return []ApplicationSource{*a.Spec.Source}
	}
	return nil
}

// HasMultipleSources returns true if the application uses multi-source format.
func (a *Application) HasMultipleSources() bool {
	return len(a.Spec.Sources) > 0
}

// IsHelm returns true if any source is a Helm chart.
func (s *ApplicationSource) IsHelm() bool {
	return s.Chart != "" || s.Helm != nil
}

// IsKustomize returns true if the source uses Kustomize.
func (s *ApplicationSource) IsKustomize() bool {
	return s.Kustomize != nil
}

// IsRef returns true if this source is a reference source (provides values files).
func (s *ApplicationSource) IsRef() bool {
	return s.Ref != ""
}

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
	// Convert to JSON then unmarshal to our type
	data, err := obj.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("failed to marshal unstructured: %w", err)
	}

	var app Application
	if err := yaml.Unmarshal(data, &app); err != nil {
		return nil, fmt.Errorf("failed to unmarshal application: %w", err)
	}

	return &app, nil
}

// FilterByRepoURL filters applications that match the given repository URL.
func FilterByRepoURL(apps []Application, repoURL string) []Application {
	filtered := make([]Application, 0)

	for _, app := range apps {
		sources := app.GetSources()
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
