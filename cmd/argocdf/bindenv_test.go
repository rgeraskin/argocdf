package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// newProbeCmd builds a throwaway command exposing a string flag (--namespace)
// and a bool flag (--kustomize-enable-helm), then parses argv so that any flags
// present are marked Changed — exactly as Cobra does before RunE. Passing argv
// simulates the user typing those flags on the command line.
func newProbeCmd(t *testing.T, argv ...string) *cobra.Command {
	t.Helper()
	var ns string
	c := &cobra.Command{Use: "probe", RunE: func(*cobra.Command, []string) error { return nil }}
	c.Flags().StringVarP(&ns, "namespace", "n", "argocd", "")
	c.Flags().Bool("kustomize-enable-helm", false, "")
	c.SetArgs(argv)
	if err := c.Execute(); err != nil {
		t.Fatalf("parse argv %v: %v", argv, err)
	}
	return c
}

// TestBindEnvPrecedence pins the effective source ordering to
// flag > environment > default. The flag-vs-env case is guarded by f.Changed in
// bindEnv; removing that guard makes this test fail.
func TestBindEnvPrecedence(t *testing.T) {
	t.Run("explicit flag beats env", func(t *testing.T) {
		t.Setenv("ARGOCDF_NAMESPACE", "from-env")
		c := newProbeCmd(t, "-n", "cli-wins")
		if err := bindEnv(c); err != nil {
			t.Fatal(err)
		}
		if got, _ := c.Flags().GetString("namespace"); got != "cli-wins" {
			t.Fatalf("namespace: want %q, got %q", "cli-wins", got)
		}
	})

	t.Run("env applies when flag absent", func(t *testing.T) {
		t.Setenv("ARGOCDF_NAMESPACE", "from-env")
		c := newProbeCmd(t)
		if err := bindEnv(c); err != nil {
			t.Fatal(err)
		}
		if got, _ := c.Flags().GetString("namespace"); got != "from-env" {
			t.Fatalf("namespace: want %q, got %q", "from-env", got)
		}
	})

	t.Run("default when neither flag nor env", func(t *testing.T) {
		c := newProbeCmd(t)
		if err := bindEnv(c); err != nil {
			t.Fatal(err)
		}
		if got, _ := c.Flags().GetString("namespace"); got != "argocd" {
			t.Fatalf("namespace: want default %q, got %q", "argocd", got)
		}
	})

	t.Run("empty env is ignored", func(t *testing.T) {
		t.Setenv("ARGOCDF_NAMESPACE", "")
		c := newProbeCmd(t)
		if err := bindEnv(c); err != nil {
			t.Fatal(err)
		}
		if got, _ := c.Flags().GetString("namespace"); got != "argocd" {
			t.Fatalf("namespace: want default %q, got %q", "argocd", got)
		}
	})
}

// TestBindEnvTypedParsing confirms env values are parsed by each flag's own type,
// so a bad bool fails fast with an error naming the env var.
func TestBindEnvTypedParsing(t *testing.T) {
	t.Run("valid bool from env", func(t *testing.T) {
		t.Setenv("ARGOCDF_KUSTOMIZE_ENABLE_HELM", "true")
		c := newProbeCmd(t)
		if err := bindEnv(c); err != nil {
			t.Fatal(err)
		}
		if got, _ := c.Flags().GetBool("kustomize-enable-helm"); !got {
			t.Fatal("kustomize-enable-helm: want true, got false")
		}
	})

	t.Run("invalid bool errors", func(t *testing.T) {
		t.Setenv("ARGOCDF_KUSTOMIZE_ENABLE_HELM", "notabool")
		c := newProbeCmd(t)
		err := bindEnv(c)
		if err == nil {
			t.Fatal("want error for invalid bool, got nil")
		}
		if want := "ARGOCDF_KUSTOMIZE_ENABLE_HELM"; !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q should name %q", err, want)
		}
	})
}
