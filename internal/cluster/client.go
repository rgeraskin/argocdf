// Package cluster provides Kubernetes cluster interaction functionality.
package cluster

import (
	"context"
	"fmt"

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
