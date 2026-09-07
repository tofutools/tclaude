package harness

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestCopilotSubagentLifecycleCheckpoint(t *testing.T) {
	home, id := copilotTelemetryHome(t, []string{
		`{"type":"session.start","data":{}}`,
		`{"type":"subagent.started","data":{"toolCallId":"a"}}`,
		`{"type":"subagent.started","data":{"toolCallId":"a"}}`,
		`{"type":"subagent.started","agentId":"a","data":{"toolCallId":"b"}}`,
	})
	f := &CopilotTelemetryFollower{}
	snap, ok, err := f.RuntimeTelemetry(home, id)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 2, snap.SubagentCount)
	cp, ok, err := f.Checkpoint()
	require.NoError(t, err)
	require.True(t, ok)
	f = &CopilotTelemetryFollower{}
	require.NoError(t, f.RestoreCheckpoint(cp))
	path := copilotLogPath(home, id)
	appendCopilotLog(t, path, `{"type":"subagent.failed","data":{"toolCallId":"b"}}`+"\n")
	snap, _, err = f.RuntimeTelemetry(home, id)
	require.NoError(t, err)
	require.Equal(t, 1, snap.SubagentCount)
	appendCopilotLog(t, path, `{"type":"subagent.completed","data":{"toolCallId":"a"}}`)
	snap, _, err = f.RuntimeTelemetry(home, id)
	require.NoError(t, err)
	require.Equal(t, 1, snap.SubagentCount, "incomplete record must be retried")
	appendCopilotLog(t, path, "\n")
	snap, _, err = f.RuntimeTelemetry(home, id)
	require.NoError(t, err)
	require.Zero(t, snap.SubagentCount)
	for _, end := range []string{"session.resume", "session.shutdown"} {
		appendCopilotLog(t, path, `{"type":"subagent.started","data":{"toolCallId":"orphan"}}`+"\n"+`{"type":"`+end+`","data":{}}`+"\n")
		snap, _, err = f.RuntimeTelemetry(home, id)
		require.NoError(t, err)
		require.Zero(t, snap.SubagentCount)
	}
	// Replacement of a log must abandon the previous lifecycle projection.
	writeCopilotLog(t, path, []string{`{"type":"subagent.started","data":{"toolCallId":"new"}}`})
	snap, _, err = f.RuntimeTelemetry(home, id)
	require.NoError(t, err)
	require.Equal(t, 1, snap.SubagentCount)
	writeCopilotLog(t, path, []string{`{"type":"session.start","data":{}}`})
	snap, _, err = f.RuntimeTelemetry(home, id)
	require.NoError(t, err)
	require.Zero(t, snap.SubagentCount)
}
