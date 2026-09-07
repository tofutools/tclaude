package nativeguidance

import (
	"strings"
	"time"

	"github.com/tofutools/tclaude/internal/backend/ports"
)

// ActivityObservation returns only the latest correlated native observation.
// It is intentionally not recovered: a restarted host has no proof of activity
// during its absence. Repeated process observations never refresh this time.
func (r *Runtime) ActivityObservation() (ports.AgentActivityObservedState, time.Time) {
	r.activityMu.Lock()
	defer r.activityMu.Unlock()
	if r.activity == "" {
		return ports.AgentActivityUnknown, time.Time{}
	}
	return r.activity, r.activityAt
}

// InvalidateActivity records only negative knowledge after sending input. It
// never interprets successful terminal input delivery as native activity.
func (r *Runtime) InvalidateActivity() {
	r.activityMu.Lock()
	defer r.activityMu.Unlock()
	r.activity = ports.AgentActivityUnknown
	r.activityAt = r.now()
}

func (r *Runtime) observeActivity(event ports.NormalizedNativeEvent) bool {
	onlyActivity := strings.HasPrefix(event.Kind, "activity_")
	if r.Correlation == nil || !r.Correlation(event.NativeCorrelation) || event.ObservedAt.IsZero() || event.ObservedAt.After(r.now().Add(time.Minute)) {
		return onlyActivity
	}
	var state ports.AgentActivityObservedState
	switch event.Kind {
	case "session_start", "activity_unknown":
		state = ports.AgentActivityUnknown
	case "user_prompt", "activity_active":
		state = ports.AgentActivityActive
	case "activity_idle":
		state = ports.AgentActivityIdle
	case "activity_awaiting_input":
		state = ports.AgentActivityAwaitingInput
	default:
		return onlyActivity
	}
	r.activityMu.Lock()
	defer r.activityMu.Unlock()
	if !event.ObservedAt.Before(r.activityAt) {
		r.activity = state
		r.activityAt = event.ObservedAt
	}
	return onlyActivity
}
