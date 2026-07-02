package rendercache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strconv"

	"github.com/rgeraskin/argocdf/internal/cluster"
)

// SchemaVersion is embedded in every cache key. Bump it to invalidate all
// previously cached entries (e.g. when the render pipeline changes in a way
// that is not otherwise captured by the key inputs).
const SchemaVersion = "rendercache-v1"

// KeyOptions holds the render-relevant options that affect rendered output and
// therefore must participate in the cache key.
type KeyOptions struct {
	KustomizeEnableHelm     bool
	KustomizeBuildOptions   string
	KustomizeLoadRestrictor string
	HelmSkipRefresh         bool
}

// KeyInput bundles everything required to compute a cache key for a single
// render (one application spec at one commit).
type KeyInput struct {
	AppName     string
	Namespace   string
	Spec        *cluster.ApplicationSpec
	KubeVersion string
	Options     KeyOptions
	// Commit is the resolved commit hash being rendered.
	Commit string
	// ResolveTree returns the content-addressed git object hash for a local
	// source path at the given commit. It must return ok=false when the hash
	// cannot be resolved (e.g. the path does not exist at that commit), which
	// causes the whole key computation to bail out (cache bypass).
	ResolveTree func(commit, path string) (string, bool)
}

// ComputeKey computes the sha256 hex cache key for a render. It returns
// ok=false (and no key) when any required input is unavailable — for example a
// nil spec, an unmarshalable spec, or a local source path whose tree hash
// cannot be resolved. Callers treat a false result as "bypass the cache for
// this render", never as an error.
func ComputeKey(in KeyInput) (string, bool) {
	if in.Spec == nil {
		return "", false
	}

	specJSON, err := json.Marshal(in.Spec)
	if err != nil {
		return "", false
	}

	h := sha256.New()
	// writeField writes a length-independent, delimiter-separated field to keep
	// the concatenation unambiguous.
	writeField := func(parts ...string) {
		for _, p := range parts {
			_, _ = io.WriteString(h, p)
			_, _ = h.Write([]byte{0})
		}
	}

	writeField(SchemaVersion)
	writeField(in.AppName, in.Namespace)
	_, _ = h.Write(specJSON)
	_, _ = h.Write([]byte{0})
	writeField(in.KubeVersion)
	writeField(
		strconv.FormatBool(in.Options.KustomizeEnableHelm),
		in.Options.KustomizeBuildOptions,
		in.Options.KustomizeLoadRestrictor,
		strconv.FormatBool(in.Options.HelmSkipRefresh),
	)

	// Per-source content identity.
	for _, src := range in.Spec.GetSources() {
		if src.Chart != "" {
			// Remote chart: identity is repo + chart + target revision.
			writeField("chart", src.RepoURL, src.Chart, src.TargetRevision)
			continue
		}
		// Local-path source: identity is the content-addressed git tree hash.
		if in.ResolveTree == nil {
			return "", false
		}
		treeHash, ok := in.ResolveTree(in.Commit, src.Path)
		if !ok {
			return "", false
		}
		writeField("path", src.Path, treeHash)
	}

	return hex.EncodeToString(h.Sum(nil)), true
}
