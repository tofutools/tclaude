package agentd

import (
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A shell row's Age and Last are its tmux session's #{session_created} and
// the later of #{session_activity} (input, attach) and its current window's
// #{window_activity} (output), so a noisy build counts as activity. The snapshot's alive set carries names only, so the
// times take one more list-sessions: made only when the snapshot lists a
// shell, and shared for shellTimesTTL across polls and parallel callers. It
// is best effort: on an error, or a tmux that renders no times, the cells
// stay blank.

// shellTimesTTL bounds how stale a shell's Last may be: a few seconds of a
// coarse "Nm ago" cell, against one tmux read per TTL.
const shellTimesTTL = 5 * time.Second

const shellTimesFormat = "#{session_name}\t#{session_created}\t#{session_activity}\t#{window_activity}"

type shellTimes struct{ created, activity time.Time }

type shellTimesCache struct {
	mu      sync.Mutex
	now     func() time.Time
	probe   func() map[string]shellTimes
	expires time.Time
	times   map[string]shellTimes
	// probing is set while a background probe runs, so a burst of stale
	// reads starts only one.
	probing bool
	// inline makes an expired read probe on the caller's goroutine. The
	// agentd test binary sets it so no probe outlives the flow test that
	// swapped the tmux runner.
	inline bool
}

var shellTimesProbe = &shellTimesCache{now: time.Now, probe: probeShellTimes}

// get returns the session times. Its caller is the dashboard snapshot poll,
// so an expired read does not wait for tmux: it serves the last times and
// starts one background probe. Only the first read, with nothing cached yet,
// probes inline. A failed probe is cached too (as no times), so a wedged tmux
// costs one probe per TTL.
func (c *shellTimesCache) get() map[string]shellTimes {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.times != nil && c.now().Before(c.expires) {
		return c.times
	}
	if c.times == nil || c.inline {
		c.times = c.probe()
		c.expires = c.now().Add(shellTimesTTL)
		return c.times
	}
	if !c.probing {
		c.probing = true
		go c.refresh()
	}
	return c.times
}

// refresh probes tmux off the poll path and publishes the result. It runs
// outside any HTTP handler, so a panic is recovered rather than taking the
// daemon down; the previous times stay and are retried after another TTL.
func (c *shellTimesCache) refresh() {
	var times map[string]shellTimes
	defer func() {
		if r := recover(); r != nil {
			slog.Error("group resources: shell times refresh panicked", "panic", r)
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		c.probing = false
		c.expires = c.now().Add(shellTimesTTL)
		if times != nil {
			c.times = times
		}
	}()
	times = c.probe()
}

func probeShellTimes() map[string]shellTimes {
	raw, err := tmuxOutputWithTimeout("list-sessions", "-F", shellTimesFormat)
	if err != nil {
		if !tmuxServerNotRunning(err) {
			slog.Debug("group resources: shell times unavailable", "error", err)
		}
		return map[string]shellTimes{}
	}
	return parseShellTimes(string(raw))
}

// parseShellTimes reads shellTimesFormat lines; a missing window field
// leaves the session's own activity.
func parseShellTimes(raw string) map[string]shellTimes {
	out := map[string]shellTimes{}
	for line := range strings.SplitSeq(raw, "\n") {
		fields := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(fields) < 3 || fields[0] == "" {
			continue
		}
		activity := tmuxUnixTime(fields[2])
		if len(fields) > 3 {
			if window := tmuxUnixTime(fields[3]); window.After(activity) {
				activity = window
			}
		}
		out[fields[0]] = shellTimes{created: tmuxUnixTime(fields[1]), activity: activity}
	}
	return out
}

// tmuxUnixTime parses a tmux epoch-seconds field; a missing, unrendered or
// zero one is the zero time.
func tmuxUnixTime(field string) time.Time {
	sec, err := strconv.ParseInt(strings.TrimSpace(field), 10, 64)
	if err != nil || sec <= 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

// addShellTimes fills the shell rows' CreatedAt and LastAt from their tmux
// sessions, probing only when there is a shell row.
func addShellTimes(rows []groupResource) {
	var times map[string]shellTimes
	for i := range rows {
		r := &rows[i]
		if r.Kind != resourceKindShell || r.Session == "" {
			continue
		}
		if times == nil {
			times = shellTimesProbe.get()
		}
		t := times[r.Session]
		r.CreatedAt, r.LastAt = resourceTime(t.created), resourceTime(t.activity)
	}
}
