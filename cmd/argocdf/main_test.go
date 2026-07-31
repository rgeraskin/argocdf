package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/log"
	"github.com/spf13/cobra"

	"github.com/rgeraskin/argocdf/internal/config"
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

// TestWarnRemovedEnvVars covers the blind spot left by removing flags: cobra
// rejects an unknown --flag, but nothing rejects ARGOCDF_RENDERER left behind in a
// CI job, because bindEnv only visits flags that still exist.
func TestWarnRemovedEnvVars(t *testing.T) {
	t.Run("set variables are named", func(t *testing.T) {
		t.Setenv("ARGOCDF_RENDERER", "native")
		t.Setenv("ARGOCDF_HELM_ADD_REPOS", "stable=https://charts.example.test")

		var buf bytes.Buffer
		warnRemovedEnvVars(log.NewWithOptions(&buf, log.Options{Level: log.WarnLevel}))

		out := buf.String()
		for _, want := range []string{"ARGOCDF_RENDERER", "ARGOCDF_HELM_ADD_REPOS", "removed in 0.5.0"} {
			if !strings.Contains(out, want) {
				t.Errorf("warning does not mention %q; got: %s", want, out)
			}
		}
		if strings.Contains(out, "ARGOCDF_HELM_SKIP_REFRESH") {
			t.Errorf("warned about a variable that is not set; got: %s", out)
		}
	})

	t.Run("empty value counts as unset", func(t *testing.T) {
		// bindEnv ignores empty values (viper's IsSet is false), so warning about
		// them would be noise for an unset-but-declared variable.
		t.Setenv("ARGOCDF_RENDERER", "")

		var buf bytes.Buffer
		warnRemovedEnvVars(log.NewWithOptions(&buf, log.Options{Level: log.WarnLevel}))

		if buf.Len() != 0 {
			t.Errorf("warned for an empty value: %s", buf.String())
		}
	})

	t.Run("nothing set, nothing said", func(t *testing.T) {
		for _, v := range removedEnvVars {
			// Setenv registers the restore; Unsetenv then clears it for this test.
			t.Setenv(v.name, "")
			if err := os.Unsetenv(v.name); err != nil {
				t.Fatal(err)
			}
		}

		var buf bytes.Buffer
		warnRemovedEnvVars(log.NewWithOptions(&buf, log.Options{Level: log.WarnLevel}))

		if buf.Len() != 0 {
			t.Errorf("unexpected output: %s", buf.String())
		}
	})
}

// TestBindEnvNoCacheLayer: --no-cache carries an optional VALUE now, so its
// environment variable must be validated rather than coerced. A typo in
// ARGOCDF_NO_CACHE that silently left the caches enabled would be the worst
// outcome - the run would look cache-free and quietly reuse entries.
func TestBindEnvNoCacheLayer(t *testing.T) {
	newCmd := func(target *string) *cobra.Command {
		cmd := &cobra.Command{Use: "argocdf"}
		cmd.Flags().Var(config.NewNoCacheFlag(target), "no-cache", "")
		cmd.Flags().Lookup("no-cache").NoOptDefVal = config.NoCacheAll
		return cmd
	}

	t.Run("layer from the environment", func(t *testing.T) {
		t.Setenv("ARGOCDF_NO_CACHE", "render")
		target := config.NoCacheNone
		if err := bindEnv(newCmd(&target)); err != nil {
			t.Fatal(err)
		}
		if target != config.NoCacheRender {
			t.Errorf("target = %q, want %q", target, config.NoCacheRender)
		}
	})

	t.Run("legacy boolean still means all", func(t *testing.T) {
		// ARGOCDF_NO_CACHE=true predates the layers and appears in CI configs.
		t.Setenv("ARGOCDF_NO_CACHE", "true")
		target := config.NoCacheNone
		if err := bindEnv(newCmd(&target)); err != nil {
			t.Fatal(err)
		}
		if target != config.NoCacheAll {
			t.Errorf("target = %q, want %q", target, config.NoCacheAll)
		}
	})

	t.Run("invalid layer is a startup error", func(t *testing.T) {
		t.Setenv("ARGOCDF_NO_CACHE", "manifests")
		target := config.NoCacheNone
		err := bindEnv(newCmd(&target))
		if err == nil {
			t.Fatal("bindEnv accepted an invalid layer")
		}
		if !strings.Contains(err.Error(), "ARGOCDF_NO_CACHE") {
			t.Errorf("error does not name the variable: %v", err)
		}
	})
}
