// Package types holds the SourceType enum shared by the render, diff and app
// packages.
package types

// SourceType represents the type of ArgoCD application source.
type SourceType string

const (
	SourceTypeHelm      SourceType = "helm"
	SourceTypeKustomize SourceType = "kustomize"
	SourceTypePlain     SourceType = "plain"
	SourceTypeUnknown   SourceType = "unknown"
)
