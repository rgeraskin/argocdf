package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// The breaking 0.5.0 namespace change relies on pflag's StringSlice splitting
// a comma-separated env value when bindEnv routes it through Flags().Set —
// this pins that behavior (and mirrors ArgoCD's own comma-split env).
func TestBindEnv_ApplicationNamespacesCommaSplit(t *testing.T) {
	var namespaces []string
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().StringSliceVar(&namespaces, "application-namespaces", nil, "")
	t.Setenv("ARGOCDF_APPLICATION_NAMESPACES", "team-a,team-*")

	if err := bindEnv(cmd); err != nil {
		t.Fatalf("bindEnv() error: %v", err)
	}
	if len(namespaces) != 2 || namespaces[0] != "team-a" || namespaces[1] != "team-*" {
		t.Errorf("application-namespaces = %v, want [team-a team-*]", namespaces)
	}
}

func TestBindEnv_ExplicitFlagBeatsEnv(t *testing.T) {
	var argocdNamespace string
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().StringVar(&argocdNamespace, "argocd-namespace", "argocd", "")
	if err := cmd.Flags().Set("argocd-namespace", "explicit"); err != nil { // marks the flag Changed
		t.Fatal(err)
	}
	t.Setenv("ARGOCDF_ARGOCD_NAMESPACE", "from-env")

	if err := bindEnv(cmd); err != nil {
		t.Fatalf("bindEnv() error: %v", err)
	}
	if argocdNamespace != "explicit" {
		t.Errorf("argocd-namespace = %q, want the explicit flag to beat the env var", argocdNamespace)
	}
}

func TestBindEnv_InvalidValueNamesTheEnvVar(t *testing.T) {
	var all bool
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().BoolVar(&all, "all-namespaces", false, "")
	t.Setenv("ARGOCDF_ALL_NAMESPACES", "banana")

	err := bindEnv(cmd)
	if err == nil {
		t.Fatal("bindEnv() = nil error, want a typed parse failure")
	}
	if !strings.Contains(err.Error(), "ARGOCDF_ALL_NAMESPACES") {
		t.Errorf("error %q does not name the offending environment variable", err)
	}
}
