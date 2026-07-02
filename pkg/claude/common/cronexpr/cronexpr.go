// Package cronexpr is the single home for cron-expression parsing in
// tclaude. The scheduler's due check, the dashboard's explain endpoint, and
// the CLI all resolve expressions through here so they can never disagree
// about what an expression means.
//
// Syntax is robfig/cron/v3's standard parser: 5-field expressions
// (minute hour dom month dow), @descriptors (@hourly, @daily, @weekly,
// @monthly, @yearly, @every <duration>), and an optional CRON_TZ=<zone>
// prefix. Without CRON_TZ, expressions evaluate in the daemon's local
// timezone: robfig's Next(t) matches a zone-less schedule against t's OWN
// location — and our stored bases are RFC3339 "Z" strings that parse as
// UTC — so Next/NextN normalize the base to time.Local first. That is what
// makes "0 9 * * *" mean 9am on the wall clock of the machine running
// agentd regardless of what location the base timestamp carries.
package cronexpr

import (
	"fmt"
	"sync"
	"time"

	lcron "github.com/lnquy/cron"
	rcron "github.com/robfig/cron/v3"
)

// Parse validates expr and returns its schedule. This is the only parser in
// the codebase — validity here IS validity everywhere.
func Parse(expr string) (rcron.Schedule, error) {
	return rcron.ParseStandard(expr)
}

// Next returns the first fire time strictly after base, or the zero time if
// the expression can never fire again (robfig gives up after a bounded
// search, e.g. "0 0 30 2 *"). The base is normalized to time.Local so a
// zone-less expression matches the daemon's wall clock even when the base
// was parsed from a UTC-serialized timestamp (see the package doc).
func Next(expr string, base time.Time) (time.Time, error) {
	sched, err := Parse(expr)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(base.In(time.Local)), nil
}

// NextN returns up to n consecutive fire times after base. Fewer than n
// (possibly zero) come back when the expression stops matching. Same
// local-normalization as Next.
func NextN(expr string, base time.Time, n int) ([]time.Time, error) {
	sched, err := Parse(expr)
	if err != nil {
		return nil, err
	}
	out := make([]time.Time, 0, n)
	t := base.In(time.Local)
	for range n {
		t = sched.Next(t)
		if t.IsZero() {
			break
		}
		out = append(out, t)
	}
	return out, nil
}

// descriptor is the lazily-built lnquy/cron English describer. Built once —
// construction loads locale tables. A nil descriptor (construction failed)
// just means Describe degrades to "".
var (
	descOnce   sync.Once
	descriptor *lcron.ExpressionDescriptor
)

// Describe renders expr as an English sentence, e.g. "Every 5 minutes,
// Monday through Friday". Best-effort sugar on top of the next-fire times:
// it returns "" for anything the describer can't handle (@descriptors,
// CRON_TZ prefixes), never an error — callers must not gate validity on it,
// that's Parse's job.
func Describe(expr string) string {
	descOnce.Do(func() {
		d, err := lcron.NewDescriptor(lcron.SetLocales(lcron.Locale_en))
		if err == nil {
			descriptor = d
		}
	})
	if descriptor == nil {
		return ""
	}
	desc, err := descriptor.ToDescription(expr, lcron.Locale_en)
	if err != nil {
		return ""
	}
	return desc
}

// minEveryDelay floors "@every <duration>" schedules. It mirrors the 30s
// minimum the interval mode enforces (the agentd scheduler tick): a finer
// @every would be silently capped to one fire per tick anyway, so the
// explainer would predict fire times that can't happen.
const minEveryDelay = 30 * time.Second

// Validate is the write-path gate: a non-empty expression must parse and
// must produce at least one future fire time, so an impossible date like
// "0 0 30 2 *" (Feb 30) can't be stored as a job that silently never runs.
// "@every" delays below the scheduler tick are rejected for the same
// prediction-must-match-reality reason (standard 5-field syntax is
// minute-resolution and can't go that fine). Returns a human-readable error.
func Validate(expr string) error {
	sched, err := Parse(expr)
	if err != nil {
		return fmt.Errorf("invalid cron expression: %w", err)
	}
	if cd, ok := sched.(rcron.ConstantDelaySchedule); ok && cd.Delay < minEveryDelay {
		return fmt.Errorf("@every delay must be >= %s (the scheduler tick interval)", minEveryDelay)
	}
	if sched.Next(time.Now()).IsZero() {
		return fmt.Errorf("cron expression never fires")
	}
	return nil
}
