package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/log"
	"github.com/sirupsen/logrus"
)

// captureHandler records the level of every record it receives.
type captureHandler struct {
	levels *[]slog.Level
}

func (h captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h captureHandler) Handle(_ context.Context, r slog.Record) error {
	*h.levels = append(*h.levels, r.Level)
	return nil
}

func (h captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h captureHandler) WithGroup(string) slog.Handler      { return h }

func klogRecord(level slog.Level, attrs ...slog.Attr) slog.Record {
	r := slog.NewRecord(time.Time{}, level, "Failed to watch", 0)
	r.AddAttrs(attrs...)
	return r
}

func TestKlogHandler_LevelMapping(t *testing.T) {
	watchErr := fmt.Errorf("Get \"https://example/api/v1/secrets?watch=true\": %w", context.Canceled)

	tests := []struct {
		name   string
		record slog.Record
		want   slog.Level
	}{
		{
			// The reflector reporting argocdf's own bounded informer
			// shutdown (repocreds.go) — expected, not a real failure.
			name:   "context-canceled error demoted",
			record: klogRecord(slog.LevelError, slog.Any("err", watchErr)),
			want:   slog.LevelDebug,
		},
		{
			// klog often stringifies errors before they reach the sink.
			name:   "context-canceled string demoted",
			record: klogRecord(slog.LevelError, slog.String("err", watchErr.Error())),
			want:   slog.LevelDebug,
		},
		{
			name:   "real watch failure stays error",
			record: klogRecord(slog.LevelError, slog.String("err", "secrets is forbidden: RBAC")),
			want:   slog.LevelError,
		},
		{
			// The string fallback matches only the TERMINAL cause (suffix);
			// an error that merely mentions the text mid-sentence is real.
			name:   "mid-sentence context-canceled mention stays error",
			record: klogRecord(slog.LevelError, slog.String("err", "context canceled by peer: connection reset")),
			want:   slog.LevelError,
		},
		{
			name:   "error without err attr stays error",
			record: klogRecord(slog.LevelError),
			want:   slog.LevelError,
		},
		{
			// klog V(n) arrives as slog level -n; charm's level map would
			// render unmapped negatives as INFO.
			name:   "V-level clamped to debug",
			record: klogRecord(slog.Level(-3)),
			want:   slog.LevelDebug,
		},
		{
			name:   "info stays info",
			record: klogRecord(slog.LevelInfo),
			want:   slog.LevelInfo,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var levels []slog.Level
			h := klogHandler{inner: captureHandler{levels: &levels}}
			if err := h.Handle(context.Background(), tt.record); err != nil {
				t.Fatalf("Handle() error: %v", err)
			}
			if len(levels) != 1 || levels[0] != tt.want {
				t.Fatalf("Handle() forwarded levels %v, want [%v]", levels, tt.want)
			}
		})
	}
}

// TestConfigureDependencyLoggingIdempotent pins that reconfiguration
// replaces the logrus forwarder instead of stacking a second one — repeated
// setup (tests, library-style reuse) must not duplicate every argocd line.
func TestConfigureDependencyLoggingIdempotent(t *testing.T) {
	// Pre-set the env vars the function would otherwise mutate process-wide.
	t.Setenv("ARGOCD_LOG_FORMAT", "text")
	t.Setenv("ARGOCD_LOG_LEVEL", "error")

	var buf bytes.Buffer
	logger := log.New(&buf)
	configureDependencyLogging(logger, true)
	configureDependencyLogging(logger, true)

	logrus.Error("hook-dup-probe")
	if got := strings.Count(buf.String(), "hook-dup-probe"); got != 1 {
		t.Errorf("logrus record forwarded %d time(s), want exactly 1 (hooks stacked across reconfiguration)", got)
	}
}

func TestKlogHandler_EnabledDropsHighVerbosity(t *testing.T) {
	var levels []slog.Level
	h := klogHandler{inner: captureHandler{levels: &levels}}
	// V(8) internals (per-request body hexdumps) must not pass Enabled.
	if h.Enabled(context.Background(), slog.Level(-8)) {
		t.Fatal("Enabled(-8) = true, want false")
	}
	if !h.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("Enabled(info) = false, want true")
	}
}

// forwardOne runs one logrus record through the forwarder and returns what
// argocdf's own logger printed. The charm logger's level is a parameter because
// the demotion's whole effect is what a DEFAULT (info-level) run shows.
func forwardOne(t *testing.T, level log.Level, e *logrus.Entry) string {
	t.Helper()
	var buf bytes.Buffer
	logger := log.New(&buf)
	logger.SetLevel(level)
	h := &logrusForwarder{
		logger: logger.WithPrefix("argocd"),
		exec:   logger.WithPrefix("argocd/exec"),
	}
	if err := h.Fire(e); err != nil {
		t.Fatalf("Fire() error: %v", err)
	}
	return buf.String()
}

// The prefix has to say which of the global logrus stream's sources a line came
// from: a subprocess (read the tool's stderr) or ArgoCD's Go code.
func TestLogrusForwarderPrefixesSubprocessRecords(t *testing.T) {
	tests := []struct {
		name string
		data logrus.Fields
		want string
	}{
		{name: "a util/exec record", data: logrus.Fields{argocdExecIDField: "ce5f4"}, want: "argocd/exec:"},
		{name: "anything else", data: nil, want: "argocd:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := forwardOne(t, log.DebugLevel, &logrus.Entry{
				Level:   logrus.ErrorLevel,
				Message: "kustomize build failed",
				Data:    tt.data,
			})
			if !strings.Contains(got, tt.want) {
				t.Errorf("forwarded line %q, want prefix %q", got, tt.want)
			}
		})
	}
}

// The expected probe must not reach a default run's stderr, and a real failure
// must. Both are asserted at the same logger level, because "quiet" here means
// quiet relative to what else gets through — not a missing record.
func TestLogrusForwarderDemotesRetriedMissingDependency(t *testing.T) {
	const probe = "`helm template . --include-crds` failed exit status 1: Error: " +
		"An error occurred while checking for chart dependencies. You may need to run " +
		"`helm dependency build` to fetch missing dependencies: " +
		"found in Chart.yaml, but missing in charts/ directory: argocd-template"
	const real = "`helm dependency build` failed exit status 1: Error: no cached repo found"

	probeEntry := &logrus.Entry{
		Level:   logrus.ErrorLevel,
		Message: probe,
		Data:    logrus.Fields{argocdExecIDField: "ce5f4"},
	}

	if got := forwardOne(t, log.InfoLevel, probeEntry); got != "" {
		t.Errorf("default run printed the expected dependency probe: %q", got)
	}
	// Demoted, not dropped: --verbose still shows it, next to the dependency
	// build that follows.
	if got := forwardOne(t, log.DebugLevel, probeEntry); !strings.Contains(got, "DEBU") {
		t.Errorf("verbose run should keep the probe at debug, got %q", got)
	}
	got := forwardOne(t, log.InfoLevel, &logrus.Entry{
		Level:   logrus.ErrorLevel,
		Message: real,
		Data:    logrus.Fields{argocdExecIDField: "82c64"},
	})
	if !strings.Contains(got, "ERRO") {
		t.Errorf("a dependency build that really failed must stay loud, got %q", got)
	}
}

// A 16,000-character line is unreadable wherever it lands - terminal, pager, or
// GitHub - and the --api-versions list is never the diagnosis. The forwarder
// shortens it on the way through, at every level (--verbose included).
func TestLogrusForwarderElidesAPIVersions(t *testing.T) {
	var flood strings.Builder
	for i := 0; i < 12; i++ {
		fmt.Fprintf(&flood, "--api-versions group%d.example.com/v1 ", i)
	}
	got := forwardOne(t, log.DebugLevel, &logrus.Entry{
		Level:   logrus.ErrorLevel,
		Message: "`helm template . " + flood.String() + "--include-crds` failed exit status 1: Error: boom",
		Data:    logrus.Fields{argocdExecIDField: "ce5f4"},
	})
	if strings.Contains(got, "group7.example.com") {
		t.Errorf("api-versions list survived into the log line: %q", got)
	}
	// The COUNT stays: it is the one informative part, saying whether argocdf
	// passed the cluster's API set at all.
	if !strings.Contains(got, "--api-versions <12 elided>") {
		t.Errorf("line %q, want the elided count", got)
	}
	if !strings.Contains(got, "Error: boom") {
		t.Errorf("line %q lost the actual failure", got)
	}
}
