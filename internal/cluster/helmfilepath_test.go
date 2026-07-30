package cluster

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	pathutil "github.com/argoproj/argo-cd/v3/util/io/path"
)

var testSchemes = []string{"https", "http"}

// TestResolveHelmFilePath pins ArgoCD's resolution rule as the matcher sees it:
// relative entries resolve against the SOURCE path, absolute ones against the
// repository root, and escapes are refused.
func TestResolveHelmFilePath(t *testing.T) {
	tests := []struct {
		name       string
		sourcePath string
		entry      string
		want       string
		wantRemote bool
		wantErr    string
	}{
		{
			name:       "relative entry resolves against the source path",
			sourcePath: "apps/chart",
			entry:      "values.yaml",
			want:       "apps/chart/values.yaml",
		},
		{
			// The reason this function exists: ArgoCD renders this, so it must
			// select the app too.
			name:       "entry escaping the source path stays inside the repo",
			sourcePath: "apps/chart",
			entry:      "../shared/vals.yaml",
			want:       "apps/shared/vals.yaml",
		},
		{
			// An absolute entry is repo-root-relative, NOT filesystem-absolute -
			// the same convention as manifest-generate-paths.
			name:       "absolute entry resolves against the repository root",
			sourcePath: "apps/chart",
			entry:      "/config/prod.yaml",
			want:       "config/prod.yaml",
		},
		{
			name:       "empty source path is the repository root",
			sourcePath: "",
			entry:      "shared/vals.yaml",
			want:       "shared/vals.yaml",
		},
		{
			name:       "nested traversal that still lands inside the repo",
			sourcePath: "apps/team/chart",
			entry:      "../../shared/vals.yaml",
			want:       "apps/shared/vals.yaml",
		},
		{
			name:       "entry escaping the repository is refused",
			sourcePath: "apps/chart",
			entry:      "../../../outside.yaml",
			wantErr:    "outside repository root",
		},
		{
			name:       "entry resolving to the repository root is refused",
			sourcePath: "",
			entry:      ".",
			wantErr:    "failed to resolve path",
		},
		{
			name:       "allowed URL scheme is remote, not a repository file",
			sourcePath: "apps/chart",
			entry:      "https://example.test/values.yaml",
			wantRemote: true,
		},
		{
			name:       "forbidden URL scheme is refused",
			sourcePath: "apps/chart",
			entry:      "ftp://example.test/values.yaml",
			wantErr:    "not allowed",
		},
		{
			// Unsubstituted ARGOCD_APP_* variables resolve literally. Selection
			// does not run envsubst, so such an entry simply matches nothing -
			// no false positive.
			name:       "unsubstituted variable resolves literally",
			sourcePath: "apps/chart",
			entry:      "values-$ARGOCD_APP_NAME.yaml",
			want:       "apps/chart/values-$ARGOCD_APP_NAME.yaml",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, remote, err := ResolveHelmFilePath(tt.sourcePath, tt.entry, testSchemes)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("ResolveHelmFilePath() = %q, want error containing %q", got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ResolveHelmFilePath() error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveHelmFilePath() unexpected error: %v", err)
			}
			if remote != tt.wantRemote {
				t.Errorf("ResolveHelmFilePath() remote = %v, want %v", remote, tt.wantRemote)
			}
			if got != tt.want {
				t.Errorf("ResolveHelmFilePath() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestResolveHelmFilePathParity is the tripwire for the synthetic root.
//
// ResolveHelmFilePath resolves against a root that does not exist, which works only
// because ArgoCD's resolver touches the filesystem through os.Readlink alone. That
// is upstream's implementation detail, not its contract: were an argo-cd bump to
// require the entry to exist, every resolution would fail and the matcher would
// silently stop matching apps - a regression with no visible symptom. So this test
// resolves the same entries against a REAL tree, where the files exist, and demands
// the same answers.
func TestResolveHelmFilePathParity(t *testing.T) {
	root := t.TempDir()
	files := []string{
		"apps/chart/values.yaml",
		"apps/shared/vals.yaml",
		"config/prod.yaml",
	}
	for _, f := range files {
		full := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("k: v\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	entries := []string{"values.yaml", "../shared/vals.yaml", "/config/prod.yaml"}
	for _, entry := range entries {
		t.Run(entry, func(t *testing.T) {
			// The real thing: an existing root with the file present, exactly as
			// the repo-server resolves while rendering.
			resolved, remote, err := pathutil.ResolveValueFilePathOrUrl(
				filepath.Join(root, "apps/chart"), root, entry, testSchemes)
			if err != nil {
				t.Fatalf("upstream resolution against a real tree failed: %v", err)
			}
			if remote {
				t.Fatalf("upstream reported %q as remote", entry)
			}
			wantRel, err := filepath.Rel(root, string(resolved))
			if err != nil {
				t.Fatal(err)
			}

			got, _, err := ResolveHelmFilePath("apps/chart", entry, testSchemes)
			if err != nil {
				t.Fatalf("ResolveHelmFilePath() failed where the real tree succeeded - upstream may now "+
					"require the entry to exist, which breaks resolution against the synthetic root: %v", err)
			}
			if got != filepath.ToSlash(wantRel) {
				t.Errorf("ResolveHelmFilePath() = %q, real-tree resolution = %q", got, wantRel)
			}
		})
	}
}

// TestResolveHelmFilePathSymlinkDivergence pins the one documented cost of the
// synthetic root, so it stays a known limitation rather than a surprise: upstream
// follows a symlinked entry (os.Readlink on the resolved path), and with no tree to
// read the link from, the matcher sees the link's own path instead of its target.
//
// Harmless for its purpose - git reports a changed file under the path it has in
// the tree, which is what this returns.
func TestResolveHelmFilePathSymlinkDivergence(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "apps/chart"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "apps/shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "apps/shared/vals.yaml")
	if err := os.WriteFile(target, []byte("k: v\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "apps/chart/link.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	resolved, _, err := pathutil.ResolveValueFilePathOrUrl(
		filepath.Join(root, "apps/chart"), root, "link.yaml", testSchemes)
	if err != nil {
		t.Fatal(err)
	}
	realRel, err := filepath.Rel(root, string(resolved))
	if err != nil {
		t.Fatal(err)
	}
	if realRel != "apps/shared/vals.yaml" {
		t.Fatalf("upstream real-tree resolution = %q, want the symlink target apps/shared/vals.yaml", realRel)
	}

	got, _, err := ResolveHelmFilePath("apps/chart", "link.yaml", testSchemes)
	if err != nil {
		t.Fatal(err)
	}
	if got != "apps/chart/link.yaml" {
		t.Errorf("ResolveHelmFilePath() = %q, want the link's own path apps/chart/link.yaml", got)
	}
}

// TestResolveRefFilePath pins the $<ref>/... rule that used to live in two places
// (selection and the render-cache key) and was wrong in both at different times.
//
// The path half now delegates to ArgoCD's resolver with an EMPTY source path, which
// is equivalent to what getResolvedRefValueFile does (ref checkout as both appPath
// and repoRoot), so traversal and resolve-to-root refusals come from upstream.
func TestResolveRefFilePath(t *testing.T) {
	const localURL = "https://github.com/org/repo"
	refSources := map[string]ApplicationSource{
		// A ref source carrying a Path ON PURPOSE: the Path must never participate,
		// which is the bug this helper exists to make unrepeatable.
		"values": {RepoURL: localURL, Ref: "values", Path: "config"},
		"other":  {RepoURL: "https://github.com/other/repo", Ref: "other"},
	}

	tests := []struct {
		name    string
		entry   string
		want    string
		wantOK  bool
		wantURL string
	}{
		{
			name:    "root-relative within the ref repository, ignoring its Path",
			entry:   "$values/env/prod.yaml",
			want:    "env/prod.yaml",
			wantOK:  true,
			wantURL: localURL,
		},
		{
			// The historical bug: joining the ref source's Path would give
			// config/env/prod.yaml here.
			name:    "ref source Path is not joined",
			entry:   "$values/prod.yaml",
			want:    "prod.yaml",
			wantOK:  true,
			wantURL: localURL,
		},
		{
			// Upstream blanks the first segment, so the remainder is repo-ROOT
			// absolute: a leading slash resolves the same way rather than being
			// treated as a filesystem path. Both previous copies got this wrong -
			// selection compared "/config/x.yaml" against repo-relative changed
			// files (never matched) and the key bypassed on it.
			name:    "absolute remainder is repo-root-relative",
			entry:   "$values//config/x.yaml",
			want:    "config/x.yaml",
			wantOK:  true,
			wantURL: localURL,
		},
		{
			name:    "interior traversal is cleaned",
			entry:   "$values/nested/../env/prod.yaml",
			want:    "env/prod.yaml",
			wantOK:  true,
			wantURL: localURL,
		},
		{
			// The caller checks the repo URL, not this helper - selection compares
			// it with the diffed repo, the cache key asks its SameRepo closure.
			name:    "foreign-repo ref source still resolves; the caller judges it",
			entry:   "$other/env/prod.yaml",
			want:    "env/prod.yaml",
			wantOK:  true,
			wantURL: "https://github.com/other/repo",
		},
		{name: "not a $ref entry", entry: "values.yaml"},
		{name: "escaping relative entry is not a $ref entry", entry: "../shared/vals.yaml"},
		{name: "no path segment after the ref name", entry: "$values"},
		{name: "empty ref name", entry: "$/env/prod.yaml"},
		{name: "unknown ref name", entry: "$nope/env/prod.yaml"},
		{name: "traversal out of the ref repository", entry: "$values/../../etc/passwd"},
		{name: "remote URL after the ref name", entry: "$values/https://example.test/v.yaml"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src, rel, ok := ResolveRefFilePath(tt.entry, refSources, testSchemes)

			if ok != tt.wantOK {
				t.Fatalf("ResolveRefFilePath(%q) ok = %v, want %v (path %q)", tt.entry, ok, tt.wantOK, rel)
			}
			if !tt.wantOK {
				return
			}
			if rel != tt.want {
				t.Errorf("ResolveRefFilePath(%q) = %q, want %q", tt.entry, rel, tt.want)
			}
			if src.RepoURL != tt.wantURL {
				t.Errorf("ResolveRefFilePath(%q) ref source repo = %q, want %q", tt.entry, src.RepoURL, tt.wantURL)
			}
		})
	}
}
