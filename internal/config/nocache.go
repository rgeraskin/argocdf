package config

import (
	"fmt"
	"strings"
)

// Cache layers --no-cache can switch off. argocdf keeps two persistent caches
// with different jobs: the RENDER cache stores rendered manifests keyed by
// everything that determines them, and the CHART cache stores extracted remote
// charts keyed by chart and version. They fail differently, so they are disabled
// separately.
const (
	// NoCacheAll disables both caches. This is what a bare --no-cache means, so
	// the flag keeps its original meaning for anyone who already passes it.
	NoCacheAll = "all"
	// NoCacheRender disables the render cache while keeping downloaded charts.
	// The useful half of the split: re-render everything without paying for
	// multi-megabyte chart downloads again.
	NoCacheRender = "render"
	// NoCacheCharts disables the chart cache while keeping rendered manifests.
	// Narrow by nature - a render-cache HIT skips fetching altogether, so this
	// only reaches apps whose render misses for some other reason.
	NoCacheCharts = "charts"
	// NoCacheNone keeps both caches. It is the default, and the state a user
	// reaches by writing --no-cache=false - which is the spelling the help text
	// advertises, because "--no-cache=none" reads as a double negative. "none" stays
	// accepted as an alias, since it is the canonical internal value.
	NoCacheNone = "none"
)

// NoCacheFlag is the pflag.Value behind --no-cache. It exists to validate at
// parse time - so a typo in ARGOCDF_NO_CACHE fails immediately with a message
// naming the accepted values, rather than silently leaving caches enabled - and to
// keep accepting the BOOLEAN spellings the flag used before it grew a value:
// `--no-cache=false` and `ARGOCDF_NO_CACHE=true` predate the layers and still
// appear in scripts.
type NoCacheFlag struct {
	target *string
}

// NewNoCacheFlag binds the flag to target, which must already hold a valid value
// (NoCacheNone for the default).
func NewNoCacheFlag(target *string) *NoCacheFlag {
	return &NoCacheFlag{target: target}
}

func (f *NoCacheFlag) String() string {
	if f.target == nil || *f.target == "" {
		return NoCacheNone
	}

	return *f.target
}

// Type is what pflag prints in usage as the value placeholder.
func (f *NoCacheFlag) Type() string { return "layer" }

// Set parses and validates one --no-cache value.
func (f *NoCacheFlag) Set(value string) error {
	parsed, err := ParseNoCache(value)
	if err != nil {
		return err
	}
	*f.target = parsed

	return nil
}

// ParseNoCache normalizes a --no-cache value: a layer name, or one of the boolean
// spellings the flag accepted when it was a bool (true = every cache off, false =
// caching on).
func ParseNoCache(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	// Bare --no-cache arrives as the empty string only when a caller sets it
	// directly; pflag substitutes NoOptDefVal for the command line.
	case "", NoCacheAll, "true", "t", "yes", "y", "1":
		return NoCacheAll, nil
	case NoCacheNone, "false", "f", "no", "n", "0":
		return NoCacheNone, nil
	case NoCacheRender:
		return NoCacheRender, nil
	case NoCacheCharts:
		return NoCacheCharts, nil
	default:
		return "", fmt.Errorf(
			"invalid cache layer %q: want %s, %s, %s or %s",
			value, NoCacheAll, NoCacheRender, NoCacheCharts, NoCacheNone)
	}
}

// RenderCacheEnabled reports whether rendered manifests may be reused.
func (c *Config) RenderCacheEnabled() bool {
	return c.NoCache != NoCacheAll && c.NoCache != NoCacheRender
}

// ChartCacheEnabled reports whether downloaded charts may be reused.
func (c *Config) ChartCacheEnabled() bool {
	return c.NoCache != NoCacheAll && c.NoCache != NoCacheCharts
}
