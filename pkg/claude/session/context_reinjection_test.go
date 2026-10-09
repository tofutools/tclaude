package session

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/startupctx"
)

func composeForTest(t *testing.T, boundary startupctx.Boundary) string {
	t.Helper()
	agentID, err := db.AgentIDForConv("conv-1")
	require.NoError(t, err)
	r, err := startupctx.ComposeReinjection(agentID, boundary)
	require.NoError(t, err)
	return r.Body
}

// reinjectionFixture is a spawned-looking agent: a session row, an actor, a
// spawn group with startup context, and a startup snapshot.
func reinjectionFixture(t *testing.T) (agentID string, groupID int64) {
	t.Helper()
	groupID = standingOrderFixture(t, harness.DefaultName)
	_, err := db.SetAgentGroupDefaultContext("tclaude", "Use git worktrees and PRs.")
	require.NoError(t, err)
	agentID, err = db.AgentIDForConv("conv-1")
	require.NoError(t, err)
	require.NoError(t, db.UpsertAgentStartupSnapshot(db.AgentStartupSnapshot{
		AgentID:             agentID,
		SpawnGroupID:        groupID,
		IncludeGroupContext: true,
		ProfileContext:      "You are a careful reviewer.",
		BriefMessageID:      4242,
	}))
	return agentID, groupID
}

func reinjectedMessages(t *testing.T) []*db.AgentMessage {
	t.Helper()
	msgs, err := db.ListAgentMessagesForConv("conv-1", 50)
	require.NoError(t, err)
	var out []*db.AgentMessage
	for _, m := range msgs {
		if db.IsReinjectedContextSubject(m.Subject) {
			out = append(out, m)
		}
	}
	return out
}

func TestContextReinjection_CompactQueuesIdentityContextsAndBriefPointer(t *testing.T) {
	reinjectionFixture(t)

	var buf bytes.Buffer
	require.NoError(t, DispatchHookEvent(context.Background(),
		sessionStart("compact"), "sess-1", LocalHookAmbient(), &buf))

	msgs := reinjectedMessages(t)
	require.Len(t, msgs, 1)
	body := msgs[0].Body
	assert.Contains(t, body, "Your context was compacted")
	assert.Contains(t, body, `in group "tclaude"`)
	assert.Contains(t, body, "(role: worker)")
	assert.Contains(t, body, "Use git worktrees and PRs.")
	assert.Contains(t, body, "You are a careful reviewer.")
	assert.Contains(t, body, "inbox message #4242")
	assert.NotContains(t, body, "Your task brief:", "the brief is referenced, never re-pasted")
}

func TestContextReinjection_GroupContextIsReadLive(t *testing.T) {
	reinjectionFixture(t)
	_, err := db.SetAgentGroupDefaultContext("tclaude", "Edited after spawn.")
	require.NoError(t, err)

	QueueContextReinjection(sessionStart("compact"), "sess-1")

	msgs := reinjectedMessages(t)
	require.Len(t, msgs, 1)
	assert.Contains(t, msgs[0].Body, "Edited after spawn.")
	assert.NotContains(t, msgs[0].Body, "Use git worktrees and PRs.")
}

func TestContextReinjection_ClearIsIdentityOnly(t *testing.T) {
	reinjectionFixture(t)

	QueueContextReinjection(sessionStart("clear"), "sess-1")

	msgs := reinjectedMessages(t)
	require.Len(t, msgs, 1)
	body := msgs[0].Body
	assert.Contains(t, body, "Your conversation was cleared")
	assert.Contains(t, body, `in group "tclaude"`)
	assert.NotContains(t, body, "Use git worktrees and PRs.")
	assert.NotContains(t, body, "You are a careful reviewer.")
	assert.NotContains(t, body, "#4242")
}

func TestContextReinjection_PostCompactAndSessionStartQueueOnce(t *testing.T) {
	reinjectionFixture(t)

	QueueContextReinjection(HookCallbackInput{HookEventName: "PostCompact", ConvID: "conv-1"}, "sess-1")
	QueueContextReinjection(sessionStart("compact"), "sess-1")
	assert.Len(t, reinjectedMessages(t), 1)

	// A /clear right after is a different boundary and is not swallowed.
	QueueContextReinjection(sessionStart("clear"), "sess-1")
	assert.Len(t, reinjectedMessages(t), 2)
}

func TestContextReinjection_SpawnGroupLeftFallsBackToOldestJoined(t *testing.T) {
	_, spawnGroup := reinjectionFixture(t)
	older, err := db.CreateAgentGroup("older", "")
	require.NoError(t, err)
	_, err = db.SetAgentGroupDefaultContext("older", "Older group rules.")
	require.NoError(t, err)
	newer, err := db.CreateAgentGroup("newer", "")
	require.NoError(t, err)
	_, err = db.SetAgentGroupDefaultContext("newer", "Newer group rules.")
	require.NoError(t, err)
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: older, ConvID: "conv-1"}))
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: newer, ConvID: "conv-1"}))

	// Still in the spawn group: its context wins, the others are named.
	QueueContextReinjection(sessionStart("compact"), "sess-1")
	msgs := reinjectedMessages(t)
	require.Len(t, msgs, 1)
	assert.Contains(t, msgs[0].Body, "Use git worktrees and PRs.")
	assert.Contains(t, msgs[0].Body, `Also a member of: "older", "newer"`)

	// Left the spawn group: the oldest-joined remaining group takes over.
	require.NoError(t, db.RemoveAgentGroupMember(spawnGroup, "conv-1"))
	composed := composeForTest(t, startupctx.BoundaryCompact)
	assert.Contains(t, composed, "Older group rules.")
	assert.Contains(t, composed, `in group "older"`)
	assert.NotContains(t, composed, "Newer group rules.")
}

func TestContextReinjection_RespectsGroupContextOptOut(t *testing.T) {
	agentID, groupID := reinjectionFixture(t)
	require.NoError(t, db.UpsertAgentStartupSnapshot(db.AgentStartupSnapshot{
		AgentID: agentID, SpawnGroupID: groupID, IncludeGroupContext: false,
	}))

	assert.NotContains(t, composeForTest(t, startupctx.BoundaryCompact), "Use git worktrees and PRs.")
}

func TestContextReinjection_IgnoresSubagentAndForeignConv(t *testing.T) {
	reinjectionFixture(t)

	sub := sessionStart("compact")
	sub.AgentID = "subagent-1"
	QueueContextReinjection(sub, "sess-1")
	foreign := sessionStart("compact")
	foreign.ConvID = "conv-other"
	QueueContextReinjection(foreign, "sess-1")
	QueueContextReinjection(sessionStart("startup"), "sess-1")

	assert.Empty(t, reinjectedMessages(t))
}

func TestContextReinjection_PeerReusingSubjectDoesNotSuppress(t *testing.T) {
	reinjectionFixture(t)
	_, _, err := db.EnsureAgentForConv("conv-peer", "test")
	require.NoError(t, err)
	_, err = db.InsertAgentMessage(&db.AgentMessage{
		FromConv: "conv-peer", ToConv: "conv-1", ToRecipients: []string{"conv-1"},
		Subject: db.ReinjectedAfterCompactSubject, Body: "spoof",
	})
	require.NoError(t, err)

	QueueContextReinjection(sessionStart("compact"), "sess-1")

	assert.Len(t, reinjectedMessages(t), 2, "the real re-injection is still queued")
}
