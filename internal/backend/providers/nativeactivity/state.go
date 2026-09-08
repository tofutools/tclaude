// Package nativeactivity projects correlated native turn observations without
// interpreting a running process or a delivered keystroke as agent activity.
package nativeactivity

import (
	"time"

	"github.com/tofutools/tclaude/internal/backend/ports"
)

// State is protected by its owning provider runtime's lock.
type State struct {
	value ports.AgentActivityObservedState
	at    time.Time
}

func (s *State) Record(value ports.AgentActivityObservedState, at time.Time) {
	if at.IsZero() || at.Before(s.at) {
		return
	}
	if at.Equal(s.at) && value != s.value {
		s.value = ports.AgentActivityUnknown
		return
	}
	s.value, s.at = value, at
}
func (s *State) Observation() (ports.AgentActivityObservedState, time.Time) {
	if s.value == "" {
		return ports.AgentActivityUnknown, time.Time{}
	}
	return s.value, s.at
}
func (s *State) Invalidate() { s.Record(ports.AgentActivityUnknown, time.Now().UTC()) }

func HookState(kind string) (ports.AgentActivityObservedState, bool) {
	switch kind {
	case "UserPromptSubmit":
		return ports.AgentActivityActive, true
	case "Stop":
		return ports.AgentActivityIdle, true
	case "SessionStart", "SessionEnd":
		return ports.AgentActivityUnknown, true
	default:
		return ports.AgentActivityUnknown, false
	}
}
