package config

import (
	"strings"
	"testing"
)

func TestParseNoCache(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    string
		wantErr bool
	}{
		{name: "layer all", value: "all", want: NoCacheAll},
		{name: "layer render", value: "render", want: NoCacheRender},
		{name: "layer charts", value: "charts", want: NoCacheCharts},
		{name: "layer none", value: "none", want: NoCacheNone},
		{name: "case insensitive", value: "ChArTs", want: NoCacheCharts},
		{name: "surrounding space", value: "  render  ", want: NoCacheRender},
		// The flag was a bool before it grew layers, so these spellings live in
		// scripts and environment variables and must keep working.
		{name: "legacy true", value: "true", want: NoCacheAll},
		{name: "legacy 1", value: "1", want: NoCacheAll},
		{name: "legacy yes", value: "yes", want: NoCacheAll},
		{name: "legacy false", value: "false", want: NoCacheNone},
		{name: "legacy 0", value: "0", want: NoCacheNone},
		// Bare --no-cache reaches pflag as NoOptDefVal, but a caller setting the
		// value directly may pass "" - it must mean the same as the bare flag.
		{name: "empty is the bare flag", value: "", want: NoCacheAll},
		{name: "unknown layer", value: "manifests", wantErr: true},
		{name: "typo", value: "renderr", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseNoCache(tt.value)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseNoCache(%q) = %q, want an error", tt.value, got)
				}
				// The message has to name the alternatives: this is what a user sees
				// when ARGOCDF_NO_CACHE has a typo in it.
				for _, want := range []string{NoCacheAll, NoCacheRender, NoCacheCharts, NoCacheNone} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error does not mention %q: %v", want, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseNoCache(%q) unexpected error: %v", tt.value, err)
			}
			if got != tt.want {
				t.Errorf("ParseNoCache(%q) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}

// TestNoCacheFlagValue covers the pflag.Value side: validation happens at Set, so
// an invalid environment value fails at startup instead of silently leaving the
// caches enabled.
func TestNoCacheFlagValue(t *testing.T) {
	target := NoCacheNone
	flag := NewNoCacheFlag(&target)

	if flag.Type() != "layer" {
		t.Errorf("Type() = %q, want \"layer\" (shown as the value placeholder in usage)", flag.Type())
	}
	if flag.String() != NoCacheNone {
		t.Errorf("String() = %q, want %q", flag.String(), NoCacheNone)
	}
	if err := flag.Set("charts"); err != nil {
		t.Fatalf("Set(charts): %v", err)
	}
	if target != NoCacheCharts {
		t.Errorf("target = %q, want %q", target, NoCacheCharts)
	}
	if err := flag.Set("bogus"); err == nil {
		t.Error("Set(bogus) accepted an invalid layer")
	}
	if target != NoCacheCharts {
		t.Errorf("a rejected value changed the target to %q", target)
	}
}

// TestCacheLayerHelpers is the matrix the rest of the code reads through, so that
// no call site has to know which layers a value covers.
func TestCacheLayerHelpers(t *testing.T) {
	tests := []struct {
		value       string
		wantRender  bool
		wantCharts  bool
		description string
	}{
		{NoCacheNone, true, true, "default: both caches serve"},
		{NoCacheAll, false, false, "bare --no-cache: nothing is reused"},
		{NoCacheRender, false, true, "re-render everything, keep the downloads"},
		{NoCacheCharts, true, false, "re-download, keep rendered manifests"},
		{"", true, true, "an unset field must not disable anything"},
	}

	for _, tt := range tests {
		cfg := &Config{NoCache: tt.value}
		if got := cfg.RenderCacheEnabled(); got != tt.wantRender {
			t.Errorf("%s: RenderCacheEnabled() = %v, want %v (%s)", tt.value, got, tt.wantRender, tt.description)
		}
		if got := cfg.ChartCacheEnabled(); got != tt.wantCharts {
			t.Errorf("%s: ChartCacheEnabled() = %v, want %v (%s)", tt.value, got, tt.wantCharts, tt.description)
		}
	}
}
