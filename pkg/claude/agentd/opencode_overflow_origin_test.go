package agentd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// A turn a standing order started stays one through a context overflow:
// OpenCode reports session.error, compacts by itself and carries the same
// turn on. Its tool calls after the compaction must stay suppressed, or a
// tool automation fires inside the turn its own delivery started.
func TestOpenCodeOverflowKeepsTheStandingOrderTurnOrigin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	db.ResetForTest()
	cleanupAgentdTestDB(t)

	const (
		sessionID = "spwn-opencode-overflow-origin"
		convID    = "ses_overflow_origin"
	)
	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, session.SaveSessionState(&session.SessionState{
		ID: sessionID, TmuxSession: sessionID, ConvID: convID, Cwd: home,
		Harness: harness.OpenCodeName, Status: session.StatusIdle,
	}))
	_, err = db.InsertStandingOrder(&db.StandingOrder{
		Name: "after-tool", TargetKind: db.StandingTargetConv, TargetAgent: agentID,
		Summary: "Run the linter.", TriggerEvent: db.StandingTriggerToolAfter,
		Timing: db.StandingTimingNextTurn, Cadence: db.StandingCadenceAlways, Enabled: true,
	})
	require.NoError(t, err)

	// A queued standing-order message starts the turn.
	messageID, err := db.InsertStandingOrderAgentMessage(&db.AgentMessage{
		ToConv: convID, Subject: "[standing-order:other]", Body: "internal",
	}, 41, 3)
	require.NoError(t, err)
	origin, err := db.AgentMessageStandingOrderOrigin(messageID)
	require.NoError(t, err)
	require.NotNil(t, origin)
	require.NoError(t, db.ArmStandingOrderTurnOrigin(agentID, convID, messageID,
		origin.OpenCodeMessageID, time.Now(), time.Minute))

	runtime := db.OpenCodeRuntime{SessionID: sessionID, ConvID: convID, Cwd: home}
	projector := newOpenCodeEventProjector(convID, home)
	consume := func(id, eventType, props string) {
		t.Helper()
		consumeOpenCodeEvent(context.Background(), runtime, projector,
			json.RawMessage(openCodeTestEvent(id, eventType, convID, props)))
	}
	tool := func(id, call, status string) {
		t.Helper()
		consume(id, "message.part.updated", `"part":{"id":"prt_`+call+`","sessionID":"`+convID+
			`","type":"tool","tool":"bash","callID":"`+call+`","state":{"status":"`+status+`","input":{"command":"ls"}}}`)
	}
	fired := func() int {
		t.Helper()
		messages, err := db.ListAgentMessagesForConv(convID, 50)
		require.NoError(t, err)
		n := 0
		for _, m := range messages {
			if strings.Contains(m.Subject, "after-tool") {
				n++
			}
		}
		return n
	}

	consume("evt_busy", "session.status", `"status":{"type":"busy"}`)
	consume("evt_assistant", "message.updated", `"info":{"id":"msg_assistant","sessionID":"`+convID+
		`","role":"assistant","parentID":"`+origin.OpenCodeMessageID+`"}`)
	tool("evt_t1_run", "call_1", "running")
	tool("evt_t1_done", "call_1", "completed")
	require.True(t, projector.standingOrderTurn)
	require.Zero(t, fired(), "the order's own turn does not trigger it")

	consume("evt_overflow", "session.error",
		`"error":{"name":"ContextOverflowError","data":{"message":"request exceeds the available context size"}}`)
	consume("evt_compact", "message.part.updated",
		`"part":{"id":"prt_compact","sessionID":"`+convID+`","type":"compaction","auto":true,"overflow":true}`)
	consume("evt_compacted", "session.compacted", ``)
	tool("evt_t2_run", "call_2", "running")
	tool("evt_t2_done", "call_2", "completed")
	assert.Zero(t, fired(), "the turn carried on after the compaction is still the order's own")
	assert.True(t, projector.standingOrderTurn)

	consume("evt_idle", "session.status", `"status":{"type":"idle"}`)
	active, err := db.GetStandingOrderTurnOrigin(agentID, convID, time.Now())
	require.NoError(t, err)
	assert.Nil(t, active, "the turn's real end releases suppression")
	assert.False(t, projector.standingOrderTurn)
}

// An overflow OpenCode does not recover from (no compaction follows) still
// ends the turn at the idle after it, so the next turn, maybe the operator's,
// is not taken for the order's.
func TestOpenCodeOverflowWithoutCompactionEndsTheTurnAtIdle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	db.ResetForTest()
	cleanupAgentdTestDB(t)

	const (
		sessionID = "spwn-opencode-overflow-idle"
		convID    = "ses_overflow_idle"
	)
	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, session.SaveSessionState(&session.SessionState{
		ID: sessionID, TmuxSession: sessionID, ConvID: convID, Cwd: home,
		Harness: harness.OpenCodeName, Status: session.StatusIdle,
	}))
	messageID, err := db.InsertStandingOrderAgentMessage(&db.AgentMessage{
		ToConv: convID, Subject: "[standing-order:other]", Body: "internal",
	}, 41, 3)
	require.NoError(t, err)
	origin, err := db.AgentMessageStandingOrderOrigin(messageID)
	require.NoError(t, err)
	require.NotNil(t, origin)
	require.NoError(t, db.ArmStandingOrderTurnOrigin(agentID, convID, messageID,
		origin.OpenCodeMessageID, time.Now(), time.Minute))

	runtime := db.OpenCodeRuntime{SessionID: sessionID, ConvID: convID, Cwd: home}
	projector := newOpenCodeEventProjector(convID, home)
	consume := func(id, eventType, props string) {
		t.Helper()
		consumeOpenCodeEvent(context.Background(), runtime, projector,
			json.RawMessage(openCodeTestEvent(id, eventType, convID, props)))
	}
	consume("evt_busy", "session.status", `"status":{"type":"busy"}`)
	consume("evt_assistant", "message.updated", `"info":{"id":"msg_assistant","sessionID":"`+convID+
		`","role":"assistant","parentID":"`+origin.OpenCodeMessageID+`"}`)
	consume("evt_tool", "message.part.updated", `"part":{"id":"prt_1","sessionID":"`+convID+
		`","type":"tool","tool":"bash","callID":"call_1","state":{"status":"running","input":{"command":"ls"}}}`)
	require.True(t, projector.standingOrderTurn)

	consume("evt_overflow", "session.error",
		`"error":{"name":"ContextOverflowError","data":{"message":"too long"}}`)
	assert.True(t, projector.standingOrderTurn, "the overflow alone does not end the turn")

	consume("evt_idle", "session.status", `"status":{"type":"idle"}`)
	assert.False(t, projector.standingOrderTurn)
	active, err := db.GetStandingOrderTurnOrigin(agentID, convID, time.Now())
	require.NoError(t, err)
	assert.Nil(t, active, "the idle's Stop ends the turn")
}
