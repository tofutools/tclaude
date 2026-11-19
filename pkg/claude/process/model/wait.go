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
// until, or the earlier of the two. Both fields are already bound. An until in
// the past ends the wait straight away (the deadline is start); one more than
// maxWaitDuration away is refused.
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
		if at.After(start.Add(maxWaitDuration)) {
			return time.Time{}, fmt.Errorf("until: %q is more than 10 years away", until)
		}
		if deadline.IsZero() || at.Before(deadline) {
			deadline = at
		}
	}
	if deadline.Before(start) {
		deadline = start
	}
	return deadline, nil
}

// hasWaitRefs reports whether a wait field is bound later (a param or an
// output reference), so authoring cannot parse it yet.
func hasWaitRefs(value string) bool {
	return paramRefPattern.MatchString(value) || captureRefPattern.MatchString(value)
}

// A wait for a criterion (wait.check) checks it straight away, then again
// every Every (backing off, with exponential, by Factor after each check up to
// MaxEvery) until it holds: the step then goes on along its route (its met
// route, with onLimit: timedOut). A check is a rule condition (When, the
// language of rule decisions) or a program or agent that answers yes or no.
// A program answers by exit code (0 yes, 1 not yet, anything else or its
// timeout a failed check); an agent by its verdict. MaxChecks and Within bound
// the wait, at least one of them; reaching one fails the step, or takes its
// timedOut route. MaxFailedChecks failed checks in a row fail the step.
const (
	WaitBackoffFixed       = "fixed"
	WaitBackoffExponential = "exponential"
	WaitOnLimitFail        = "fail"
	WaitOnLimitTimedOut    = "timedOut"
	// WaitOutcomeMet and WaitOutcomeTimedOut are a check wait's routes when
	// it has onLimit: timedOut.
	WaitOutcomeMet      = "met"
	WaitOutcomeTimedOut = "timedOut"
	// DefaultMaxFailedChecks is how many failed checks in a row a wait takes
	// when it does not say.
	DefaultMaxFailedChecks = 3
	// DefaultWaitBackoffFactor is an exponential backoff's factor when it does
	// not say.
	DefaultWaitBackoffFactor = 2.0
	// Agent check verdicts.
	WaitCheckYes = "yes"
	WaitCheckNo  = "no"
)

// MaxFailed is the number of failed checks in a row that fail the step.
func (c WaitCheck) MaxFailed() int {
	if c.MaxFailedChecks > 0 {
		return c.MaxFailedChecks
	}
	return DefaultMaxFailedChecks
}

// TimesOut reports whether reaching a limit takes the timedOut route rather
// than failing the step.
func (c WaitCheck) TimesOut() bool { return c.OnLimit == WaitOnLimitTimedOut }

// Interval is how long to wait after checks checks before the next one:
// Every, or with exponential backoff Every*Factor^(checks-1), capped at
// MaxEvery. Every and MaxEvery must already be valid.
func (c WaitCheck) Interval(checks int) time.Duration {
	every, err := ParseWaitDuration(c.Every)
	if err != nil {
		return time.Minute
	}
	if c.Backoff != WaitBackoffExponential || checks <= 1 {
		return every
	}
	factor := c.Factor
	if factor <= 1 {
		factor = DefaultWaitBackoffFactor
	}
	limit := maxWaitDuration
	if c.MaxEvery != "" {
		if d, err := ParseWaitDuration(c.MaxEvery); err == nil {
			limit = d
		}
	}
	interval := float64(every)
	for range checks - 1 {
		interval *= factor
		if interval >= float64(limit) {
			return limit
		}
	}
	return time.Duration(interval)
}

// validateWaitCheck checks a wait's criterion and its knobs, and the routes
// it needs.
func validateWaitCheck(tmpl *Template, nodeID string, node Node, add func(code, at, message string)) {
	path := "nodes." + nodeID + ".wait"
	check := node.Wait.Check
	at := path + ".check"
	if !isBlank(node.Wait.Duration) || !isBlank(node.Wait.Until) || !isBlank(node.Wait.Signal) {
		add("invalid_wait_check", path, "a wait for a criterion bounds itself with check.within; it takes no duration, until or signal")
	}
	switch {
	case isBlank(check.When) && check.Performer == nil:
		add("invalid_wait_check", at, "a check needs when (a condition) or a performer (a program or an agent that answers yes or no)")
	case !isBlank(check.When) && check.Performer != nil:
		add("invalid_wait_check", at, "a check has when or a performer, not both")
	case !isBlank(check.When):
		cond, err := ParseCondition(check.When)
		if err != nil {
			add("invalid_rule_condition", at+".when", err.Error())
			break
		}
		outputs := map[string]bool{}
		for _, other := range tmpl.Nodes {
			for _, name := range other.Captures {
				outputs[name] = true
			}
		}
		// A wait may read a step on another branch: that is what it waits for.
		validateConditionOperands(tmpl, cond, at+".when", nil, outputs, add)
	case check.Performer.Kind != PerformerProgram && check.Performer.Kind != PerformerAgent:
		add("invalid_wait_check", at+".performer.kind", "a check is answered by a program or an agent")
	}
	durations := []struct{ name, value string }{{"every", check.Every}, {"maxEvery", check.MaxEvery}, {"timeout", check.Timeout}, {"within", check.Within}}
	for _, field := range durations {
		if isBlank(field.value) {
			continue
		}
		if _, err := ParseWaitDuration(field.value); err != nil {
			add("invalid_duration", at+"."+field.name, err.Error())
		}
	}
	if isBlank(check.Every) {
		add("invalid_wait_check", at+".every", "a check needs every: how long to wait between checks, such as 5m")
	}
	if check.MaxChecks <= 0 && isBlank(check.Within) {
		add("invalid_wait_check", at, "a check needs maxChecks or within, so it does not check forever")
	}
	if check.MaxChecks < 0 || check.MaxFailedChecks < 0 {
		add("invalid_wait_check", at, "maxChecks and maxFailedChecks cannot be negative")
	}
	switch check.Backoff {
	case "", WaitBackoffFixed:
		if check.Factor != 0 || !isBlank(check.MaxEvery) {
			add("invalid_wait_check", at+".backoff", "factor and maxEvery go with backoff: exponential")
		}
	case WaitBackoffExponential:
		if check.Factor != 0 && check.Factor <= 1 {
			add("invalid_wait_check", at+".factor", "an exponential backoff's factor must be more than 1")
		}
	default:
		add("invalid_wait_check", at+".backoff", fmt.Sprintf("backoff is fixed or exponential; got %q", check.Backoff))
	}
	if !isBlank(check.Timeout) && check.Performer != nil && check.Performer.Kind != PerformerProgram {
		add("invalid_wait_check", at+".timeout", "a timeout bounds a program's check")
	}
	switch check.OnLimit {
	case "", WaitOnLimitFail:
		if len(node.Next) != 1 {
			add("invalid_wait_check", "nodes."+nodeID+".next", "a wait has one route; with check.onLimit: timedOut it has two, met and timedOut")
		}
	case WaitOnLimitTimedOut:
		_, met := node.Next[WaitOutcomeMet]
		_, timedOut := node.Next[WaitOutcomeTimedOut]
		if len(node.Next) != 2 || !met || !timedOut {
			add("invalid_wait_check", "nodes."+nodeID+".next", "with check.onLimit: timedOut a wait has two routes, met and timedOut")
		}
	default:
		add("invalid_wait_check", at+".onLimit", fmt.Sprintf("onLimit is fail or timedOut; got %q", check.OnLimit))
	}
}
