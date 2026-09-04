package common

import (
	"log/slog"
	"os"
	"sync/atomic"
	"time"
)

var startupTraceSequence atomic.Uint64

// StartupTiming records sequential startup milestones when explicitly enabled
// in the daemon's environment. Its session-new children inherit the switch.
// Each trace is confined to one goroutine; nested/overlapping traces must not
// be summed. Callers supply identifiers only, never prompts or config contents.
// The returned function is a no-op when disabled.
func StartupTiming(component string, attrs ...any) func(string, ...any) {
	if os.Getenv("TCLAUDE_STARTUP_TIMING") != "1" {
		return func(string, ...any) {}
	}
	start := time.Now()
	previous := start
	base := []any{"component", component, "pid", os.Getpid(), "trace", startupTraceSequence.Add(1)}
	base = append(base, attrs...)
	mark := func(stage string, fields ...any) {
		now := time.Now()
		args := append([]any{}, base...)
		args = append(args, "stage", stage,
			"elapsed_ms", float64(now.Sub(start).Microseconds())/1000,
			"step_ms", float64(now.Sub(previous).Microseconds())/1000)
		args = append(args, fields...)
		slog.Info("startup timing", args...)
		previous = now
	}
	mark("start")
	return mark
}
