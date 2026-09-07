package agentd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/copilotapi"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

func TestCopilotBackgroundAgentsDisplay(t *testing.T) {
	setupTestDB(t)
	resetCopilotAPIStateForTest()
	row := copilotAPIStateSession(t, "s-background", "conv-background", session.StatusIdle)
	server := newFakeCopilotServer(t)
	server.answer(copilotapi.MethodSessionPermissions, copilotAPITestNoPermissions)
	server.answer(copilotapi.MethodSessionContextInfo, copilotAPITestContextNull)
	server.answer(copilotapi.MethodSessionUsage, copilotAPITestUsage)
	server.answer(copilotapi.MethodSessionTasksList, `{"tasks":[
  {"type":"agent","status":"running"},
  {"type":"agent","status":"running"},
  {"type":"agent","status":"completed"},
  {"type":"agent","status":"idle"},
  {"type":"shell","status":"running"}]}`)
	startTestConsumer(t, server, row.ConvID, row.ConvID)
	require.Eventually(t, func() bool { return copilotAPISubagentCount(row, 0) == 2 }, 5*time.Second, 20*time.Millisecond)
	alive := map[string]struct{}{row.TmuxSession: {}}
	state, online := terminalStatusForSessions([]*db.SessionRow{row}, alive)
	require.True(t, online)
	assert.Equal(t, session.StatusMainAgentIdle, state.Status)
	assert.Equal(t, 2, state.SubagentCount)
	assert.Equal(t, session.BackgroundActivityDetail(2, 0, 0), state.StatusDetail)

	full := stateForConvInSessions([]*db.SessionRow{row}, alive)
	assert.Equal(t, state.Status, full.Status)
	assert.Equal(t, state.SubagentCount, full.SubagentCount)

	// Human prompts retain precedence over background work.
	row.Status = session.StatusAwaitingPermission
	state, _ = terminalStatusForSessions([]*db.SessionRow{row}, alive)
	assert.Equal(t, session.StatusAwaitingPermission, state.Status)
	row.Status = session.StatusIdle

	server.answer(copilotapi.MethodSessionTasksList, `{"tasks":[{"type":"agent","status":"completed"}]}`)
	server.push(copilotapi.MethodSessionEvent, `{"sessionId":"conv-background","event":{"type":"session.background_tasks_changed"}}`)
	require.Eventually(t, func() bool { return copilotAPISubagentCount(row, -1) == 0 }, 5*time.Second, 20*time.Millisecond)
	state, _ = terminalStatusForSessions([]*db.SessionRow{row}, alive)
	assert.Equal(t, session.StatusIdle, state.Status)
	assert.Zero(t, state.SubagentCount)

	// Loss of the connection cannot leave a permanent background badge.
	copilotAPIStates.Lock()
	copilotAPIStates.background[row.ConvID] = copilotAPIBackgroundReading{ObservedAt: time.Now().Add(-2 * copilotAPIStateFreshness), SubagentCount: 2}
	copilotAPIStates.Unlock()
	assert.Zero(t, copilotAPISubagentCount(row, 0))
	dropCopilotAPIState(row.ConvID)
	assert.Zero(t, copilotAPISubagentCount(row, 0))
}
