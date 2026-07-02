// Package cluster provides Kubernetes cluster interaction functionality.
package cluster

import (
	"context"
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Client wraps Kubernetes client-go for cluster operations.
type Client struct {
	kubeconfig string
	context    string

	restConfig    *rest.Config
	clientset     *kubernetes.Clientset
	dynamicClient dynamic.Interface
}

// NewClient creates a new Kubernetes client with the given configuration.
func NewClient(kubeconfigPath, contextName string) (*Client, error) {
	client := &Client{
		kubeconfig: kubeconfigPath,
		context:    contextName,
	}

	if err := client.connect(); err != nil {
		return nil, err
	}

	return client, nil
}

// connect establishes the connection to the Kubernetes cluster.
func (c *Client) connect() error {
	// Build config from kubeconfig file with context
	loadingRules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: c.kubeconfig}
	configOverrides := &clientcmd.ConfigOverrides{}
	if c.context != "" {
		configOverrides.CurrentContext = c.context
	}

	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)

	config, err := kubeConfig.ClientConfig()
	if err != nil {
		return fmt.Errorf("failed to build config: %w", err)
	}

	c.restConfig = config

	// Create clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create clientset: %w", err)
	}
	c.clientset = clientset

	// Create dynamic client for CRD operations
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create dynamic client: %w", err)
	}
	c.dynamicClient = dynamicClient

	return nil
}

// GetKubeVersion returns the Kubernetes server version.
func (c *Client) GetKubeVersion(ctx context.Context) (string, error) {
	serverVersion, err := c.clientset.Discovery().ServerVersion()
	if err != nil {
		return "", fmt.Errorf("failed to get server version: %w", err)
	}

	return serverVersion.GitVersion, nil
}

// GetAPIVersions discovers the API versions available in the cluster and
// returns them in the form Helm's `--api-versions` flag expects. The result
// contains both bare group/version entries (e.g. "networking.k8s.io/v1", "v1")
// and fully-qualified group/version/Kind entries (e.g.
// "networking.k8s.io/v1/Ingress"), matching what ArgoCD passes to Helm. The
// list is deduplicated and sorted.
//
// Discovery on aggregated API servers can partially fail; in that case the
// partial result is returned alongside the error so callers may still use it.
func (c *Client) GetAPIVersions(_ context.Context) ([]string, error) {
	_, resourceLists, err := c.clientset.Discovery().ServerGroupsAndResources()
	versions := apiVersionsFromResourceLists(resourceLists)
	if err != nil {
		return versions, fmt.Errorf("failed to discover API versions: %w", err)
	}
	return versions, nil
}

// apiVersionsFromResourceLists converts discovered API resource lists into the
// deduplicated, sorted list of entries Helm accepts for `--api-versions`.
// It is a pure function to keep it easily testable without a live cluster.
func apiVersionsFromResourceLists(resourceLists []*metav1.APIResourceList) []string {
	set := make(map[string]struct{})
	for _, rl := range resourceLists {
		if rl == nil || rl.GroupVersion == "" {
			continue
		}
		set[rl.GroupVersion] = struct{}{}
		for _, res := range rl.APIResources {
			if res.Kind == "" {
				continue
			}
			set[rl.GroupVersion+"/"+res.Kind] = struct{}{}
		}
	}

	versions := make([]string, 0, len(set))
	for v := range set {
		versions = append(versions, v)
	}
	sort.Strings(versions)
	return versions
}

// DynamicClient returns the dynamic client for CRD operations.
func (c *Client) DynamicClient() dynamic.Interface {
	return c.dynamicClient
}

// Clientset returns the standard kubernetes clientset.
func (c *Client) Clientset() *kubernetes.Clientset {
	return c.clientset
}

// RESTConfig returns the REST configuration.
func (c *Client) RESTConfig() *rest.Config {
	return c.restConfig
}

// Context returns the kubernetes context being used.
func (c *Client) Context() string {
	return c.context
}

// GVR is a helper type for GroupVersionResource.
type GVR = schema.GroupVersionResource

// Scheme returns a runtime scheme (useful for conversions).
func Scheme() *runtime.Scheme {
	return runtime.NewScheme()
}
