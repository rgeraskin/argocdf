// Package diff provides recursive application discovery.
package diff

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rgeraskin/argocdf/internal/cluster"
	"github.com/rgeraskin/argocdf/internal/types"
)

// AppDiscoverer discovers new applications from rendered manifests.
type AppDiscoverer struct {
	parser *ManifestParser
}

// NewAppDiscoverer creates a new AppDiscoverer.
func NewAppDiscoverer() *AppDiscoverer {
	return &AppDiscoverer{
		parser: NewManifestParser(),
	}
}

// DiscoveredApplication represents an Application CRD found in rendered manifests.
type DiscoveredApplication struct {
	Name      string
	Namespace string
	Spec      cluster.ApplicationSpec
	RawYAML   string
}

// DiscoverApplications finds Application CRDs in the given manifest content.
func (d *AppDiscoverer) DiscoverApplications(content string) ([]DiscoveredApplication, error) {
	manifests, err := d.parser.ParseManifests(content)
	if err != nil {
		return nil, err
	}

	var apps []DiscoveredApplication

	for _, m := range manifests {
		if m.Kind != "Application" {
			continue
		}
		if !strings.Contains(m.APIVersion, "argoproj.io") {
			continue
		}

		// Extract spec from the object
		spec := cluster.ApplicationSpec{}
		if specMap, ok := m.Object["spec"].(map[string]interface{}); ok {
			spec = parseApplicationSpec(specMap)
		}

		apps = append(apps, DiscoveredApplication{
			Name:      m.Name,
			Namespace: m.Namespace,
			Spec:      spec,
			RawYAML:   m.Raw,
		})
	}

	return apps, nil
}

// parseApplicationSpec parses a spec map into ApplicationSpec using JSON round-trip.
// This approach automatically handles all fields (including nested Helm/Kustomize configs)
// without needing to manually extract each field.
func parseApplicationSpec(specMap map[string]interface{}) cluster.ApplicationSpec {
	spec := cluster.ApplicationSpec{}

	// Marshal the map to JSON, then unmarshal into the typed struct
	// This handles all fields automatically, including nested structures
	data, err := json.Marshal(specMap)
	if err != nil {
		return spec
	}
	_ = json.Unmarshal(data, &spec)

	return spec
}

// FindNewApplications compares old and new manifests to find newly added Applications.
func (d *AppDiscoverer) FindNewApplications(oldContent, newContent string) ([]DiscoveredApplication, error) {
	oldApps, err := d.DiscoverApplications(oldContent)
	if err != nil {
		return nil, fmt.Errorf("failed to discover old applications: %w", err)
	}

	newApps, err := d.DiscoverApplications(newContent)
	if err != nil {
		return nil, fmt.Errorf("failed to discover new applications: %w", err)
	}

	// Build set of old app names
	oldNames := make(map[string]bool)
	for _, app := range oldApps {
		key := fmt.Sprintf("%s/%s", app.Namespace, app.Name)
		oldNames[key] = true
	}

	// Find apps that are in new but not in old
	var newlyAdded []DiscoveredApplication
	for _, app := range newApps {
		key := fmt.Sprintf("%s/%s", app.Namespace, app.Name)
		if !oldNames[key] {
			newlyAdded = append(newlyAdded, app)
		}
	}

	return newlyAdded, nil
}

// ModifiedApplication represents a child app that exists in both old and new but has changes.
type ModifiedApplication struct {
	Name      string
	Namespace string
	OldSpec   cluster.ApplicationSpec
	NewSpec   cluster.ApplicationSpec
}

// FindModifiedApplications compares old and new manifests to find Applications that exist
// in both but have different specs (indicating the child app configuration changed).
func (d *AppDiscoverer) FindModifiedApplications(oldContent, newContent string) ([]ModifiedApplication, error) {
	oldApps, err := d.DiscoverApplications(oldContent)
	if err != nil {
		return nil, fmt.Errorf("failed to discover old applications: %w", err)
	}

	newApps, err := d.DiscoverApplications(newContent)
	if err != nil {
		return nil, fmt.Errorf("failed to discover new applications: %w", err)
	}

	// Build map of old apps by key
	oldAppMap := make(map[string]DiscoveredApplication)
	for _, app := range oldApps {
		key := fmt.Sprintf("%s/%s", app.Namespace, app.Name)
		oldAppMap[key] = app
	}

	// Find apps that exist in both but have different raw YAML (indicating changes)
	var modified []ModifiedApplication
	for _, newApp := range newApps {
		key := fmt.Sprintf("%s/%s", newApp.Namespace, newApp.Name)
		if oldApp, exists := oldAppMap[key]; exists {
			// Compare raw YAML to detect any changes
			if oldApp.RawYAML != newApp.RawYAML {
				modified = append(modified, ModifiedApplication{
					Name:      newApp.Name,
					Namespace: newApp.Namespace,
					OldSpec:   oldApp.Spec,
					NewSpec:   newApp.Spec,
				})
			}
		}
	}

	return modified, nil
}

// AppDiffQueue manages the queue of applications to process for apps-of-apps.
type AppDiffQueue struct {
	pending   []QueuedApp
	processed map[string]bool
	maxDepth  int
}

// QueuedApp represents an application in the processing queue.
type QueuedApp struct {
	Name      string
	Namespace string
	Depth     int
	ParentApp string
	Spec      *cluster.ApplicationSpec    // Spec to use for target branch (also for base if OldSpec is nil)
	OldSpec   *cluster.ApplicationSpec    // Optional: spec to use for base branch (for modified child apps)
}

// NewAppDiffQueue creates a new AppDiffQueue.
func NewAppDiffQueue(maxDepth int) *AppDiffQueue {
	return &AppDiffQueue{
		pending:   make([]QueuedApp, 0),
		processed: make(map[string]bool),
		maxDepth:  maxDepth,
	}
}

// Add adds an application to the queue if not already processed.
func (q *AppDiffQueue) Add(app QueuedApp) bool {
	key := fmt.Sprintf("%s/%s", app.Namespace, app.Name)
	if q.processed[key] {
		return false
	}
	if app.Depth >= q.maxDepth {
		return false
	}
	q.pending = append(q.pending, app)
	return true
}

// Next returns the next application to process, or nil if queue is empty.
func (q *AppDiffQueue) Next() *QueuedApp {
	if len(q.pending) == 0 {
		return nil
	}
	app := q.pending[0]
	q.pending = q.pending[1:]
	key := fmt.Sprintf("%s/%s", app.Namespace, app.Name)
	q.processed[key] = true
	return &app
}

// IsEmpty returns true if the queue is empty.
func (q *AppDiffQueue) IsEmpty() bool {
	return len(q.pending) == 0
}

// ProcessedCount returns the number of processed applications.
func (q *AppDiffQueue) ProcessedCount() int {
	return len(q.processed)
}

// AppTree structures the diff results into a tree based on parent-child relationships.
type AppTree struct {
	Root     []*AppTreeNode
	allNodes map[string]*AppTreeNode
}

// AppTreeNode represents a node in the application tree.
type AppTreeNode struct {
	AppDiff  interface{} // *types.AppDiff - using interface{} to avoid import cycle in output pkg
	Children []*AppTreeNode
}

// NewAppTree creates a new AppTree from a slice of AppDiffs.
func NewAppTree(diffs []*types.AppDiff) *AppTree {
	tree := &AppTree{
		Root:     make([]*AppTreeNode, 0),
		allNodes: make(map[string]*AppTreeNode),
	}

	// Create nodes for all apps
	for _, d := range diffs {
		key := fmt.Sprintf("%s/%s", d.Namespace, d.Name)
		tree.allNodes[key] = &AppTreeNode{
			AppDiff:  d,
			Children: make([]*AppTreeNode, 0),
		}
	}

	// Build parent-child relationships
	for _, d := range diffs {
		key := fmt.Sprintf("%s/%s", d.Namespace, d.Name)
		node := tree.allNodes[key]

		if d.ParentAppName == "" {
			// Root node
			tree.Root = append(tree.Root, node)
		} else {
			// Find parent and add as child
			for _, parentNode := range tree.allNodes {
				if parentAppDiff, ok := parentNode.AppDiff.(*types.AppDiff); ok {
					if parentAppDiff.Name == d.ParentAppName {
						parentNode.Children = append(parentNode.Children, node)
						break
					}
				}
			}
		}
	}

	return tree
}

// Walk traverses the tree depth-first, calling fn for each node.
func (t *AppTree) Walk(fn func(node *AppTreeNode, depth int)) {
	for _, root := range t.Root {
		walkNode(root, 0, fn)
	}
}

func walkNode(node *AppTreeNode, depth int, fn func(node *AppTreeNode, depth int)) {
	fn(node, depth)
	for _, child := range node.Children {
		walkNode(child, depth+1, fn)
	}
}
