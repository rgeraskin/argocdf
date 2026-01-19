// Package testutil provides test utilities including fixtures.
package testutil

import (
	"github.com/rgeraskin/argocdf/internal/cluster"
	"github.com/rgeraskin/argocdf/internal/diff"
	"github.com/rgeraskin/argocdf/internal/git"
	"github.com/rgeraskin/argocdf/internal/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestApp creates an ArgoCD Application for testing.
func TestApp(name, namespace, repoURL, path string) cluster.Application {
	return cluster.Application{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: cluster.ApplicationSpec{
			Source: &cluster.ApplicationSource{
				RepoURL: repoURL,
				Path:    path,
			},
		},
	}
}

// TestAppMultiSource creates a multi-source ArgoCD Application for testing.
func TestAppMultiSource(name, namespace string, sources []cluster.ApplicationSource) cluster.Application {
	return cluster.Application{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: cluster.ApplicationSpec{
			Sources: sources,
		},
	}
}

// TestAppWithHelm creates an ArgoCD Application with Helm configuration.
func TestAppWithHelm(name, namespace, repoURL, chart string) cluster.Application {
	return cluster.Application{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: cluster.ApplicationSpec{
			Source: &cluster.ApplicationSource{
				RepoURL: repoURL,
				Chart:   chart,
				Helm:    &cluster.ApplicationSourceHelm{},
			},
		},
	}
}

// TestChangedFiles creates a ChangedFiles struct for testing.
func TestChangedFiles(added, modified, deleted []string) *git.ChangedFiles {
	return &git.ChangedFiles{
		Added:    added,
		Modified: modified,
		Deleted:  deleted,
	}
}

// TestAppDiff creates an AppDiff for testing.
func TestAppDiff(name, namespace string, hasChanges bool) *types.AppDiff {
	return &types.AppDiff{
		Name:      name,
		Namespace: namespace,
		DiffResult: &diff.ManifestSetDiff{
			HasChanges: hasChanges,
		},
	}
}

// TestAppDiffWithChanges creates an AppDiff with detailed change information.
func TestAppDiffWithChanges(name, namespace string, added, removed, modified int) *types.AppDiff {
	result := &diff.ManifestSetDiff{
		HasChanges: added > 0 || removed > 0 || modified > 0,
		Added:      make([]diff.Manifest, added),
		Removed:    make([]diff.Manifest, removed),
		Modified:   make([]diff.ManifestDiff, modified),
	}

	// Create dummy manifests
	for i := 0; i < added; i++ {
		result.Added[i] = diff.Manifest{
			Kind: "ConfigMap",
			Name: "added-" + string(rune('a'+i)),
		}
	}
	for i := 0; i < removed; i++ {
		result.Removed[i] = diff.Manifest{
			Kind: "ConfigMap",
			Name: "removed-" + string(rune('a'+i)),
		}
	}
	for i := 0; i < modified; i++ {
		result.Modified[i] = diff.ManifestDiff{
			Key: "ConfigMap/modified-" + string(rune('a'+i)),
		}
	}

	return &types.AppDiff{
		Name:       name,
		Namespace:  namespace,
		DiffResult: result,
	}
}

// TestAppDiffWithError creates an AppDiff with an error.
func TestAppDiffWithError(name, namespace string, err error) *types.AppDiff {
	return &types.AppDiff{
		Name:      name,
		Namespace: namespace,
		Error:     err,
	}
}

// TestQueuedApp creates a QueuedApp for testing.
func TestQueuedApp(name, namespace string, depth int, parent string, spec *cluster.ApplicationSpec) diff.QueuedApp {
	return diff.QueuedApp{
		Name:      name,
		Namespace: namespace,
		Depth:     depth,
		ParentApp: parent,
		Spec:      spec,
	}
}

// TestApplicationSource creates an ApplicationSource for testing.
func TestApplicationSource(repoURL, path string) cluster.ApplicationSource {
	return cluster.ApplicationSource{
		RepoURL: repoURL,
		Path:    path,
	}
}

// TestHelmSource creates a Helm-based ApplicationSource for testing.
func TestHelmSource(repoURL, chart, version string) cluster.ApplicationSource {
	return cluster.ApplicationSource{
		RepoURL:        repoURL,
		Chart:          chart,
		TargetRevision: version,
		Helm:           &cluster.ApplicationSourceHelm{},
	}
}

// TestRefSource creates a ref-based ApplicationSource for multi-source apps.
func TestRefSource(repoURL, path, ref string) cluster.ApplicationSource {
	return cluster.ApplicationSource{
		RepoURL: repoURL,
		Path:    path,
		Ref:     ref,
	}
}
