package agentd

import (
	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"testing"
)

func TestCopilotBackgroundAgentsWithoutAPI(t *testing.T) {
	setupTestDB(t)
	resetCopilotContextRefreshStateForTest()
	t.Cleanup(resetCopilotContextRefreshStateForTest)
	home := copilotRefreshHome(t)
	path := copilotRefreshLogPath(home)
	row := copilotRefreshSession(t, "copilot-background-log")
	alive := map[string]struct{}{row.TmuxSession: {}}
	appendCopilotRefreshEvents(t, path, `{"type":"session.start","data":{}}`,
		`{"type":"subagent.started","data":{"toolCallId":"a"}}`)
	// The standalone terminal path must work before a dashboard poll.
	state, online := terminalStatusForSessions([]*db.SessionRow{row}, alive)
	assert.True(t, online)
	assert.Equal(t, session.StatusMainAgentIdle, state.Status)
	assert.Equal(t, 1, state.SubagentCount)
	full := stateForConvInSessions([]*db.SessionRow{row}, alive)
	assert.Equal(t, state.Status, full.Status)
	assert.Equal(t, state.SubagentCount, full.SubagentCount)

	row.Status = session.StatusAwaitingInput
	state, _ = terminalStatusForSessions([]*db.SessionRow{row}, alive)
	assert.Equal(t, session.StatusAwaitingInput, state.Status)
	row.Status = session.StatusIdle
	appendCopilotRefreshEvents(t, path, `{"type":"subagent.failed","data":{"toolCallId":"a"}}`)
	expireCopilotContextRefreshThrottle()
	state, _ = terminalStatusForSessions([]*db.SessionRow{row}, alive)
	assert.Equal(t, session.StatusIdle, state.Status)
	assert.Zero(t, state.SubagentCount)
	appendCopilotRefreshEvents(t, path, `{"type":"subagent.started","data":{"toolCallId":"b"}}`)
	expireCopilotContextRefreshThrottle()
	state, _ = terminalStatusForSessions([]*db.SessionRow{row}, nil)
	assert.Equal(t, session.StatusExited, state.Status)
	assert.Zero(t, state.SubagentCount)
}
