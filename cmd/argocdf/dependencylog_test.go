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
