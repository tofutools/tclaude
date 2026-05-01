package ratelimit

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/usageapi"
)

// WaitForRateLimit checks the 5-hour rate limit and blocks until it resets when
// needed. out receives user-facing progress messages when non-nil; pass nil to suppress terminal output
// Returns true if the context was cancelled during the wait.
func WaitForRateLimit(ctx context.Context, out io.Writer) bool {
	cfg, _ := config.Load()
	if cfg.RateLimit == nil {
		return false
	}
	slog.Debug("checking rate limit")
	usage, err := usageapi.GetCached()
	if usage == nil {
		if err != nil {
			slog.Warn("unable to check rate limit", "error", err)
		}
		return false
	}
	if err != nil {
		slog.Warn("using stale usage cache", "error", err)
	}
	if usage.FiveHour != nil && usage.FiveHour.Pct > cfg.RateLimit.FiveHourPercentMaxUsed {
		resetsAt := usage.FiveHour.ResetsAt
		slog.Debug("Waiting for 5 hour rate limit to reset", "time", resetsAt)
		if out != nil {
			_, _ = fmt.Fprintf(out, "Waiting for 5 hour rate limit to reset at %v...\n", resetsAt.Local().Format("15:04"))
		}
		select {
		case <-ctx.Done():
			return true
		case <-time.After(time.Until(resetsAt.Add(10 * time.Second))):
		}
		if out != nil {
			_, _ = fmt.Fprintf(out, "Rate limit reset, proceeding\n")
		}
	}
	return false
}
