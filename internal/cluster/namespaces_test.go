package cluster

import "testing"

func TestIsLiteralNamespace(t *testing.T) {
	tests := []struct {
		entry string
		want  bool
	}{
		{"argocd", true},
		{"team-a", true},
		{"team-123", true},
		{"", false},
		{"*", false},
		{"team-*", false},
		{"/^team-[0-9]+$/", false},
		{"Team", false},         // uppercase is outside DNS-1123 labels
		{"ns.with.dots", false}, // dots mean a pattern, not a namespace name
		{"team_a", false},       // underscore is not valid in namespace names
	}
	for _, tt := range tests {
		if got := IsLiteralNamespace(tt.entry); got != tt.want {
			t.Errorf("IsLiteralNamespace(%q) = %v, want %v", tt.entry, got, tt.want)
		}
	}
}

func TestAllLiteralNamespaces(t *testing.T) {
	if !AllLiteralNamespaces([]string{"argocd", "team-a"}) {
		t.Error("AllLiteralNamespaces([argocd team-a]) = false, want true")
	}
	if AllLiteralNamespaces([]string{"argocd", "team-*"}) {
		t.Error("AllLiteralNamespaces([argocd team-*]) = true, want false")
	}
	if !AllLiteralNamespaces(nil) {
		t.Error("AllLiteralNamespaces(nil) = false, want true")
	}
}

func TestMatchesNamespacePatterns(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		entries   []string
		want      bool
	}{
		{"literal match", "team-a", []string{"team-a"}, true},
		{"literal mismatch", "team-b", []string{"team-a"}, false},
		{"glob match", "team-42", []string{"team-*"}, true},
		{"glob mismatch", "ops-1", []string{"team-*"}, false},
		{"match everything", "anything", []string{"*"}, true},
		{"regex match", "team-42", []string{"/^team-[0-9]+$/"}, true},
		{"regex mismatch", "team-x", []string{"/^team-[0-9]+$/"}, false},
		{"second entry matches", "ops-1", []string{"team-*", "ops-*"}, true},
		// The list is exhaustive: unlike ArgoCD's IsNamespaceEnabled there is
		// no implicit control-plane inclusion.
		{"argocd namespace not implicit", "argocd", []string{"team-*"}, false},
		{"empty list matches nothing", "argocd", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MatchesNamespacePatterns(tt.namespace, tt.entries); got != tt.want {
				t.Errorf("MatchesNamespacePatterns(%q, %v) = %v, want %v", tt.namespace, tt.entries, got, tt.want)
			}
		})
	}
}
