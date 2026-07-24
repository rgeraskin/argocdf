package cluster

import (
	"github.com/argoproj/argo-cd/v3/util/glob"
)

// IsLiteralNamespace reports whether entry is a plain namespace name rather
// than a pattern. Namespace names are DNS-1123 labels (lowercase alphanumerics
// and '-'), so any character outside that set means the entry is a glob or a
// /regex/.
func IsLiteralNamespace(entry string) bool {
	if entry == "" {
		return false
	}
	for _, r := range entry {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

// AllLiteralNamespaces reports whether every entry is a literal namespace
// name. An all-literal list can be served by per-namespace List calls, so
// strictly namespace-scoped RBAC suffices; any pattern entry forces a
// cluster-wide list.
func AllLiteralNamespaces(entries []string) bool {
	for _, e := range entries {
		if !IsLiteralNamespace(e) {
			return false
		}
	}
	return true
}

// MatchesNamespacePatterns reports whether namespace matches any of the
// entries, using ArgoCD's --application-namespaces pattern semantics: glob
// patterns (team-*, *), or regular expressions wrapped in slashes (/…/).
// This is the matcher behind ArgoCD's security.IsNamespaceEnabled, used here
// WITHOUT its control-plane short-circuit — argocdf's namespace list is
// exhaustive, not additive.
func MatchesNamespacePatterns(namespace string, entries []string) bool {
	return glob.MatchStringInList(entries, namespace, glob.REGEXP)
}
