package model

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A wait step holds its branch until a deadline: wait.duration after the
// step starts, wait.until (a timestamp), or, with both, whichever comes
// first ("until X, but at most Y"). Both fields may refer to
// {{ params.name }}, bound when the run starts, and to {{ outputs.name }},
// bound when the step starts.

// waitDurationPart is one number and unit of a wait duration: "3 days",
// "90m", "1.5h".
var waitDurationPart = regexp.MustCompile(`^(\d+(?:\.\d+)?)\s*([a-zµ]+)`)

// waitDurationUnits are the units a wait duration may use, Go's own included.
var waitDurationUnits = map[string]time.Duration{
	"ns": time.Nanosecond, "us": time.Microsecond, "µs": time.Microsecond, "ms": time.Millisecond,
	"s": time.Second, "sec": time.Second, "secs": time.Second, "second": time.Second, "seconds": time.Second,
	"m": time.Minute, "min": time.Minute, "mins": time.Minute, "minute": time.Minute, "minutes": time.Minute,
	"h": time.Hour, "hr": time.Hour, "hrs": time.Hour, "hour": time.Hour, "hours": time.Hour,
	"d": 24 * time.Hour, "day": 24 * time.Hour, "days": 24 * time.Hour,
	"w": 7 * 24 * time.Hour, "wk": 7 * 24 * time.Hour, "week": 7 * 24 * time.Hour, "weeks": 7 * 24 * time.Hour,
}

// maxWaitDuration bounds a wait duration, well past any real process.
const maxWaitDuration = 10 * 365 * 24 * time.Hour

// ParseWaitDuration reads a wait duration: Go's syntax (90m, 1h30m) plus days
// and weeks (3d, 2w, 1d12h), or words ("3 days", "1 hour 30 minutes",
// "2 weeks and 1 day"). It must be positive.
func ParseWaitDuration(value string) (time.Duration, error) {
	rest := strings.ToLower(strings.TrimSpace(value))
	if rest == "" {
		return 0, fmt.Errorf("a wait duration is required")
	}
	var total time.Duration
	for rest != "" {
		match := waitDurationPart.FindStringSubmatch(rest)
		if match == nil {
			return 0, fmt.Errorf("%q is not a duration such as 30m, 2h, 3d or \"3 days\"", value)
		}
		unit, ok := waitDurationUnits[match[2]]
		if !ok {
			return 0, fmt.Errorf("%q: unknown unit %q (use s, m, h, d, w, or seconds, minutes, hours, days, weeks)", value, match[2])
		}
		number, err := strconv.ParseFloat(match[1], 64)
		if err != nil || number*float64(unit) > float64(maxWaitDuration) {
			return 0, fmt.Errorf("%q is longer than a wait can be", value)
		}
		total += time.Duration(number * float64(unit))
		rest = strings.TrimSpace(rest[len(match[0]):])
		rest = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(rest, ","), "and "))
	}
	if total <= 0 {
		return 0, fmt.Errorf("%q: a wait duration must be positive", value)
	}
	if total > maxWaitDuration {
		return 0, fmt.Errorf("%q is longer than a wait can be", value)
	}
	return total, nil
}

// InterpolateWait binds the run's params into a wait's duration and until.
// Outputs stay as written; they are bound when the step starts.
func InterpolateWait(wait WaitConfig, params map[string]string) WaitConfig {
	wait.Duration = interpolateParams(wait.Duration, params)
	wait.Until = interpolateParams(wait.Until, params)
	return wait
}

// WaitDeadline is when a wait that started at start ends: start+duration,
// until, or the earlier of the two. Both fields are already bound. A deadline
// in the past is allowed; the wait then ends straight away.
func WaitDeadline(duration, until string, start time.Time) (time.Time, error) {
	duration, until = strings.TrimSpace(duration), strings.TrimSpace(until)
	if duration == "" && until == "" {
		return time.Time{}, fmt.Errorf("the wait has neither a duration nor an until time")
	}
	var deadline time.Time
	if duration != "" {
		d, err := ParseWaitDuration(duration)
		if err != nil {
			return time.Time{}, fmt.Errorf("duration: %w", err)
		}
		deadline = start.Add(d)
	}
	if until != "" {
		at, err := ParseRFC3339(until)
		if err != nil {
			return time.Time{}, fmt.Errorf("until: %q is not a time such as 2026-07-14T10:00:00Z", until)
		}
		if deadline.IsZero() || at.Before(deadline) {
			deadline = at
		}
	}
	return deadline, nil
}

// hasWaitRefs reports whether a wait field is bound later (a param or an
// output reference), so authoring cannot parse it yet.
func hasWaitRefs(value string) bool {
	return paramRefPattern.MatchString(value) || captureRefPattern.MatchString(value)
}
