package cluster

import (
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAPIVersionsFromResourceLists(t *testing.T) {
	tests := []struct {
		name  string
		lists []*metav1.APIResourceList
		want  []string
	}{
		{
			name:  "nil input",
			lists: nil,
			want:  []string{},
		},
		{
			name: "core and grouped versions with kinds",
			lists: []*metav1.APIResourceList{
				{
					GroupVersion: "v1",
					APIResources: []metav1.APIResource{
						{Kind: "Pod"},
						{Kind: "Service"},
					},
				},
				{
					GroupVersion: "networking.k8s.io/v1",
					APIResources: []metav1.APIResource{
						{Kind: "Ingress"},
					},
				},
			},
			want: []string{
				"networking.k8s.io/v1",
				"networking.k8s.io/v1/Ingress",
				"v1",
				"v1/Pod",
				"v1/Service",
			},
		},
		{
			name: "dedupes and skips empties",
			lists: []*metav1.APIResourceList{
				nil,
				{GroupVersion: ""},
				{
					GroupVersion: "apps/v1",
					APIResources: []metav1.APIResource{
						{Kind: "Deployment"},
						{Kind: ""},           // skipped
						{Kind: "Deployment"}, // duplicate
					},
				},
				{
					GroupVersion: "apps/v1", // duplicate group/version
					APIResources: []metav1.APIResource{
						{Kind: "StatefulSet"},
					},
				},
			},
			want: []string{
				"apps/v1",
				"apps/v1/Deployment",
				"apps/v1/StatefulSet",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := apiVersionsFromResourceLists(tt.lists)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("apiVersionsFromResourceLists() = %v, want %v", got, tt.want)
			}
		})
	}
}
