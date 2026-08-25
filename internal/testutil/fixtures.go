// Package testutil provides test utilities including fixtures.
package testutil

import (
	"github.com/rgeraskin/argocdf/internal/cluster"
	"github.com/rgeraskin/argocdf/internal/git"
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

// TestChangedFiles creates a ChangedFiles struct for testing.
func TestChangedFiles(added, modified, deleted []string) *git.ChangedFiles {
	return &git.ChangedFiles{
		Added:    added,
		Modified: modified,
		Deleted:  deleted,
	}
}
