package nativeactivity

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"testing"
	"time"
)

func TestLateHookCannotUndoNewerActivity(t *testing.T) {
	var state State
	now := time.Now().UTC()
	state.Record(ports.AgentActivityActive, now)
	state.Record(ports.AgentActivityIdle, now.Add(-time.Second))
	value, at := state.Observation()
	require.Equal(t, ports.AgentActivityActive, value)
	require.Equal(t, now, at)
	state.Record(ports.AgentActivityIdle, now)
	value, _ = state.Observation()
	require.Equal(t, ports.AgentActivityUnknown, value, "ambiguous ordering cannot establish idle")
	state.Record(ports.AgentActivityIdle, now.Add(time.Second))
	value, _ = state.Observation()
	require.Equal(t, ports.AgentActivityIdle, value)
}
