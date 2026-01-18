// Package errors defines custom error types for argocdf.
package errors

import (
	"errors"
	"fmt"
)

// Sentinel errors for common failure cases.
var (
	ErrNoClusterConnection = errors.New("failed to connect to Kubernetes cluster")
	ErrNoApplicationsFound = errors.New("no ArgoCD applications found")
	ErrNoChangesDetected   = errors.New("no changes detected between branches")
	ErrUnsupportedSource   = errors.New("unsupported application source type")
	ErrRenderingFailed     = errors.New("failed to render application manifests")
	ErrGitOperationFailed  = errors.New("git operation failed")
	ErrBinaryNotFound      = errors.New("required binary not found in PATH")
	ErrMaxRecursionReached = errors.New("maximum recursion depth reached")
)

// AppError represents an error that occurred while processing a specific application.
type AppError struct {
	AppName   string
	Namespace string
	Op        string
	Err       error
}

func (e *AppError) Error() string {
	if e.Namespace != "" {
		return fmt.Sprintf("%s/%s: %s: %v", e.Namespace, e.AppName, e.Op, e.Err)
	}
	return fmt.Sprintf("%s: %s: %v", e.AppName, e.Op, e.Err)
}

func (e *AppError) Unwrap() error {
	return e.Err
}

// NewAppError creates a new AppError.
func NewAppError(appName, namespace, op string, err error) *AppError {
	return &AppError{
		AppName:   appName,
		Namespace: namespace,
		Op:        op,
		Err:       err,
	}
}

// GitError represents an error that occurred during git operations.
type GitError struct {
	Op      string
	RepoURL string
	Branch  string
	Err     error
}

func (e *GitError) Error() string {
	if e.Branch != "" {
		return fmt.Sprintf("git %s on %s (branch: %s): %v", e.Op, e.RepoURL, e.Branch, e.Err)
	}
	if e.RepoURL != "" {
		return fmt.Sprintf("git %s on %s: %v", e.Op, e.RepoURL, e.Err)
	}
	return fmt.Sprintf("git %s: %v", e.Op, e.Err)
}

func (e *GitError) Unwrap() error {
	return e.Err
}

// NewGitError creates a new GitError.
func NewGitError(op, repoURL, branch string, err error) *GitError {
	return &GitError{
		Op:      op,
		RepoURL: repoURL,
		Branch:  branch,
		Err:     err,
	}
}

// RenderError represents an error that occurred during manifest rendering.
type RenderError struct {
	AppName    string
	SourceType string
	ChartPath  string
	Err        error
}

func (e *RenderError) Error() string {
	return fmt.Sprintf("render %s (type: %s, path: %s): %v", e.AppName, e.SourceType, e.ChartPath, e.Err)
}

func (e *RenderError) Unwrap() error {
	return e.Err
}

// NewRenderError creates a new RenderError.
func NewRenderError(appName, sourceType, chartPath string, err error) *RenderError {
	return &RenderError{
		AppName:    appName,
		SourceType: sourceType,
		ChartPath:  chartPath,
		Err:        err,
	}
}

// ConfigError represents a configuration error.
type ConfigError struct {
	Field string
	Err   error
}

func (e *ConfigError) Error() string {
	return fmt.Sprintf("config error for %s: %v", e.Field, e.Err)
}

func (e *ConfigError) Unwrap() error {
	return e.Err
}

// NewConfigError creates a new ConfigError.
func NewConfigError(field string, err error) *ConfigError {
	return &ConfigError{
		Field: field,
		Err:   err,
	}
}

// MultiError collects multiple errors that occurred during processing.
type MultiError struct {
	Errors []error
}

func (e *MultiError) Error() string {
	if len(e.Errors) == 0 {
		return "no errors"
	}
	if len(e.Errors) == 1 {
		return e.Errors[0].Error()
	}
	return fmt.Sprintf("%d errors occurred; first: %v", len(e.Errors), e.Errors[0])
}

// Add appends an error to the MultiError.
func (e *MultiError) Add(err error) {
	if err != nil {
		e.Errors = append(e.Errors, err)
	}
}

// HasErrors returns true if any errors were collected.
func (e *MultiError) HasErrors() bool {
	return len(e.Errors) > 0
}

// ToError returns nil if no errors, otherwise returns the MultiError.
func (e *MultiError) ToError() error {
	if !e.HasErrors() {
		return nil
	}
	return e
}
