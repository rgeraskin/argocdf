package cluster

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestResolveContextName(t *testing.T) {
	tests := []struct {
		name     string
		override string
		raw      clientcmdapi.Config
		rawErr   error
		want     string
	}{
		{
			name:     "explicit override wins over current-context",
			override: "prod",
			raw:      clientcmdapi.Config{CurrentContext: "dev"},
			want:     "prod",
		},
		{
			name: "no override falls back to current-context",
			raw:  clientcmdapi.Config{CurrentContext: "dev"},
			want: "dev",
		},
		{
			// The override needs no kubeconfig to be meaningful.
			name:     "override still wins when the raw config is unreadable",
			override: "prod",
			raw:      clientcmdapi.Config{},
			rawErr:   errors.New("no such file"),
			want:     "prod",
		},
		{
			name:   "unreadable raw config without override resolves to unknown",
			raw:    clientcmdapi.Config{CurrentContext: "ignored"},
			rawErr: errors.New("no such file"),
			want:   "",
		},
		{
			name: "config without current-context resolves to unknown",
			raw:  clientcmdapi.Config{},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveContextName(tt.override, tt.raw, tt.rawErr); got != tt.want {
				t.Errorf("resolveContextName() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestNewClientResolvesContext pins that the resolved name is captured during
// the connect that already happens (no live cluster needed: building the client
// never dials).
func TestNewClientResolvesContext(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
clusters:
- cluster: {server: "https://127.0.0.1:1"}
  name: offline
contexts:
- context: {cluster: offline}
  name: from-file
- context: {cluster: offline}
  name: from-flag
current-context: from-file
`), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		contextName string
		want        string
	}{
		{name: "no --context uses the kubeconfig current-context", want: "from-file"},
		{name: "--context overrides current-context", contextName: "from-flag", want: "from-flag"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := NewClient(kubeconfig, tt.contextName)
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}
			if got := client.ResolvedContext(); got != tt.want {
				t.Errorf("ResolvedContext() = %q, want %q", got, tt.want)
			}
		})
	}
}

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
