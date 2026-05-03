package ratelimit

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/notify"
	"github.com/tofutools/tclaude/pkg/claude/common/usageapi"
)

// WaitForRateLimit checks the 5-hour rate limit and blocks until it resets when needed.
// out receives user-facing progress messages when non-nil; pass nil to suppress terminal output
// Returns true if the context was cancelled during the wait.
func WaitForRateLimit(ctx context.Context, out io.Writer, sessionId, cwd string) bool {
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
		slog.Info("Waiting for 5 hour rate limit to reset", "time", resetsAt)
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
	if usage.SevenDay != nil && usage.SevenDay.Pct > cfg.RateLimit.SevenDayPercentMaxUsed {
		resetsAt := usage.SevenDay.ResetsAt
		cancelled := waitForSevenDay(ctx, out, resetsAt, sessionId, cwd)
		if cancelled {
			return true
		}
	}
	if usage.SevenDaySonnet != nil && usage.SevenDaySonnet.Pct > cfg.RateLimit.SevenDayPercentMaxUsed {
		resetsAt := usage.SevenDaySonnet.ResetsAt
		cancelled := waitForSevenDay(ctx, out, resetsAt, sessionId, cwd)
		if cancelled {
			return true
		}
	}
	return false
}

func waitForSevenDay(ctx context.Context, out io.Writer, resetsAt time.Time, sessionId, cwd string) bool {
	resetsAfter := time.Until(resetsAt.Add(10 * time.Second))
	if resetsAfter.Seconds() < 12 {
		return false
	}
	slog.Info("Waiting for 7 day rate limit to reset", "time", resetsAt)
	msg := fmt.Sprintf("Waiting for 7 day rate limit to reset at %v...", resetsAt.Local().Format("2006-01-02 15:04"))
	if out != nil {
		_, _ = fmt.Fprintf(out, "%s\n", msg)
	}
	if resetsAfter.Hours() > 5 && notify.IsEnabled() {
		notify.Send(sessionId, "Rate limit", cwd, msg)
	}
	select {
	case <-ctx.Done():
		return true
	case <-time.After(resetsAfter):
	}
	if out != nil {
		_, _ = fmt.Fprintf(out, "Rate limit reset, proceeding\n")
	}
	return false
}
