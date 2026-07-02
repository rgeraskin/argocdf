// Package diff provides recursive application discovery.
package diff

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rgeraskin/argocdf/internal/cluster"
	"github.com/rgeraskin/argocdf/internal/types"
)

// appKey returns a unique key for an application in the format "namespace/name".
func appKey(namespace, name string) string {
	return fmt.Sprintf("%s/%s", namespace, name)
}

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
	result := d.parser.ParseManifests(content)

	var apps []DiscoveredApplication

	for _, m := range result.Manifests {
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
// Returns an empty spec on error (best effort parsing).
func parseApplicationSpec(specMap map[string]interface{}) cluster.ApplicationSpec {
	spec := cluster.ApplicationSpec{}

	// Marshal the map to JSON, then unmarshal into the typed struct
	// This handles all fields automatically, including nested structures
	data, err := json.Marshal(specMap)
	if err != nil {
		// This shouldn't happen for map[string]interface{} from YAML parsing,
		// but log it if it does for debugging purposes
		return spec
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		// Log warning: spec parsing failed but continue with partial/empty spec
		// This can happen if the Application has non-standard fields
		// Returning empty spec allows the tool to continue with limited functionality
		return spec
	}

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
		key := appKey(app.Namespace, app.Name)
		oldNames[key] = true
	}

	// Find apps that are in new but not in old
	var newlyAdded []DiscoveredApplication
	for _, app := range newApps {
		key := appKey(app.Namespace, app.Name)
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
		key := appKey(app.Namespace, app.Name)
		oldAppMap[key] = app
	}

	// Find apps that exist in both but have different raw YAML (indicating changes)
	var modified []ModifiedApplication
	for _, newApp := range newApps {
		key := appKey(newApp.Namespace, newApp.Name)
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
	// processedSpecs records the (Spec, OldSpec) identity each app was last
	// processed with. It is used by RequeueProcessed to break requeue loops
	// (e.g. self-managing root apps that discover their own Application CRD).
	processedSpecs map[string]string
	maxDepth       int
}

// QueuedApp represents an application in the processing queue.
type QueuedApp struct {
	Name            string
	Namespace       string
	Depth           int
	ParentApp       string
	ParentNamespace string
	Spec            *cluster.ApplicationSpec // Spec to use for target branch (also for base if OldSpec is nil)
	OldSpec         *cluster.ApplicationSpec // Optional: spec to use for base branch (for modified child apps)
}

// specSignature returns a stable identity for an app's (Spec, OldSpec) pair.
// Apps requeued with an identical signature to what was already processed are
// refused, which terminates apps-of-apps requeue loops.
func specSignature(app QueuedApp) string {
	spec, _ := json.Marshal(app.Spec)
	oldSpec, _ := json.Marshal(app.OldSpec)
	return string(spec) + "|" + string(oldSpec)
}

// NewAppDiffQueue creates a new AppDiffQueue.
func NewAppDiffQueue(maxDepth int) *AppDiffQueue {
	return &AppDiffQueue{
		pending:        make([]QueuedApp, 0),
		processed:      make(map[string]bool),
		processedSpecs: make(map[string]string),
		maxDepth:       maxDepth,
	}
}

// isPending reports whether an app with the given key is already in pending.
func (q *AppDiffQueue) isPending(key string) bool {
	for i := range q.pending {
		if appKey(q.pending[i].Namespace, q.pending[i].Name) == key {
			return true
		}
	}
	return false
}

// Add adds an application to the queue if not already processed or pending.
func (q *AppDiffQueue) Add(app QueuedApp) bool {
	key := appKey(app.Namespace, app.Name)
	if q.processed[key] {
		return false
	}
	if q.isPending(key) {
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
	key := appKey(app.Namespace, app.Name)
	q.processed[key] = true
	q.processedSpecs[key] = specSignature(app)
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

// UpdatePending updates the spec of a pending app if it exists.
// Returns true if the app was found in pending and updated.
func (q *AppDiffQueue) UpdatePending(app QueuedApp) bool {
	key := appKey(app.Namespace, app.Name)

	// Can't update if already processed
	if q.processed[key] {
		return false
	}

	// Find and update in pending slice
	for i := range q.pending {
		if appKey(q.pending[i].Namespace, q.pending[i].Name) == key {
			q.pending[i].Spec = app.Spec
			q.pending[i].OldSpec = app.OldSpec
			q.pending[i].ParentApp = app.ParentApp
			q.pending[i].ParentNamespace = app.ParentNamespace
			return true
		}
	}
	return false
}

// RequeueProcessed moves an already-processed app back to pending with new specs.
// This handles the case where a child app was processed before its parent
// discovered that the child's spec changed in git.
// Re-processing is needed even if the first attempt succeeded - the "successful"
// result is semantically wrong (both branches rendered with cluster spec instead
// of old-git-spec → new-git-spec). Returns true if requeued.
func (q *AppDiffQueue) RequeueProcessed(app QueuedApp) bool {
	key := appKey(app.Namespace, app.Name)

	// Only requeue if it was actually processed
	if !q.processed[key] {
		return false
	}

	// Enforce maxDepth like Add does, so requeuing can't recurse without bound.
	if app.Depth >= q.maxDepth {
		return false
	}

	// Spec-identity guard: refuse to requeue when the incoming (Spec, OldSpec)
	// is identical to what the app was already processed with. This is the loop
	// terminator for self-managing / mutually-referencing apps: the first
	// requeue (cluster spec -> git specs) differs and is allowed; a second
	// requeue with the same git specs is refused.
	if sig, ok := q.processedSpecs[key]; ok && sig == specSignature(app) {
		return false
	}

	// Remove from processed and add to pending
	delete(q.processed, key)
	q.pending = append(q.pending, app)
	return true
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
		key := appKey(d.Namespace, d.Name)
		tree.allNodes[key] = &AppTreeNode{
			AppDiff:  d,
			Children: make([]*AppTreeNode, 0),
		}
	}

	// Build parent-child relationships
	for _, d := range diffs {
		key := appKey(d.Namespace, d.Name)
		node := tree.allNodes[key]

		if d.ParentAppName == "" {
			// Root node
			tree.Root = append(tree.Root, node)
		} else if parentNode, ok := tree.allNodes[appKey(d.ParentAppNamespace, d.ParentAppName)]; ok {
			// Attach to the exact parent (matched by namespace/name)
			parentNode.Children = append(parentNode.Children, node)
		} else {
			// Parent not present in the result set: treat as a root so the
			// node still appears in the tree instead of being dropped.
			tree.Root = append(tree.Root, node)
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
