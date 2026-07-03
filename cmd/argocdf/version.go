package main

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// versionString returns a human-readable version line, preferring values
// injected at release time via ldflags and falling back to the Go build info
// embedded by `go build`/`go install`.
func versionString() string {
	v, c, d := Version, Commit, Date

	if info, ok := debug.ReadBuildInfo(); ok {
		if v == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			v = info.Main.Version
		}
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				if c == "" {
					c = s.Value
				}
			case "vcs.time":
				if d == "" {
					d = s.Value
				}
			}
		}
	}

	out := v
	if c != "" {
		if len(c) > 12 {
			c = c[:12]
		}
		out += fmt.Sprintf(" (commit %s)", c)
	}
	if d != "" {
		out += fmt.Sprintf(" built %s", d)
	}
	out += fmt.Sprintf(" %s/%s", runtime.GOOS, runtime.GOARCH)
	return out
}
