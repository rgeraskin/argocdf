package diff

import "github.com/rgeraskin/argocdf/internal/types"

// AppDiff contains the full diff information for an ArgoCD Application.
//
// It lived in internal/types with an `interface{}` manifest-diff field for as
// long as it was believed that holding a *ManifestSetDiff would cycle back into
// this package. It does not: diff imports cluster and types, and nothing they
// import reaches diff, so the record belongs next to the result it carries and
// the field can say what it holds.
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
	SourceType types.SourceType

	// Diff contains the manifest diff result, or nil when the application could
	// not be rendered (Error then says why). It is not called DiffResult because
	// this package already spells that name for the per-object field-level result.
	Diff *ManifestSetDiff

	// RenderedOld is the full rendered output from base branch
	RenderedOld string

	// RenderedNew is the full rendered output from target branch
	RenderedNew string

	// Error holds any error that occurred while processing this app
	Error error
}
