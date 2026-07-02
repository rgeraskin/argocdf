// Package types defines shared types used across the argocdf application.
package types

// SourceType represents the type of ArgoCD application source.
type SourceType string

const (
	SourceTypeHelm      SourceType = "helm"
	SourceTypeKustomize SourceType = "kustomize"
	SourceTypePlain     SourceType = "plain"
	SourceTypeUnknown   SourceType = "unknown"
)

// AppDiff contains the full diff information for an ArgoCD Application.
// Note: DiffResult is interface{} to avoid import cycles - cast to *diff.ManifestSetDiff when using.
type AppDiff struct {
	// Name is the ArgoCD Application name
	Name string

	// Namespace is the namespace where the Application CR exists
	Namespace string

	// ParentAppName is the name of the parent app (for apps-of-apps pattern)
	ParentAppName string

	// ParentAppNamespace is the namespace of the parent app (for apps-of-apps pattern).
	// Together with ParentAppName it uniquely identifies the parent, so same-named
	// apps in different namespaces attach to the correct parent.
	ParentAppNamespace string

	// ChildAppNames contains names of child applications discovered
	ChildAppNames []string

	// SourceType indicates whether this is a Helm, Kustomize, or plain app
	SourceType SourceType

	// DiffResult contains the manifest diff result (use *diff.ManifestSetDiff)
	DiffResult interface{}

	// RenderedOld is the full rendered output from base branch
	RenderedOld string

	// RenderedNew is the full rendered output from target branch
	RenderedNew string

	// Error holds any error that occurred while processing this app
	Error error
}

// DiscoveredApp represents a newly discovered Application CRD from rendered manifests.
type DiscoveredApp struct {
	Name      string
	Namespace string
	Spec      map[string]interface{}
}

// RefSource represents a source with ref: field that provides values files to other sources.
type RefSource struct {
	RefName  string
	RepoURL  string
	Revision string
	Path     string
}
