package session

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/convops"
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

func TestContextReinjection_GroupModeIdentityAndOff(t *testing.T) {
	reinjectionFixture(t)

	_, err := db.SetAgentGroupReinjectAfterCompact("tclaude", db.ReinjectIdentity)
	require.NoError(t, err)
	body := composeForTest(t, startupctx.BoundaryCompact)
	assert.Contains(t, body, `in group "tclaude"`)
	assert.NotContains(t, body, "Use git worktrees and PRs.")
	assert.NotContains(t, body, "You are a careful reviewer.")
	assert.NotContains(t, body, "#4242")

	_, err = db.SetAgentGroupReinjectAfterCompact("tclaude", db.ReinjectOff)
	require.NoError(t, err)
	QueueContextReinjection(sessionStart("compact"), "sess-1")
	QueueContextReinjection(sessionStart("clear"), "sess-1")
	assert.Empty(t, reinjectedMessages(t))
}

// claudeTranscriptFixture gives the fixture session a launch directory and
// returns the main-thread transcript Claude Code would be appending to.
func claudeTranscriptFixture(t *testing.T) string {
	t.Helper()
	state, err := LoadSessionState("sess-1")
	require.NoError(t, err)
	state.Cwd = filepath.Join(t.TempDir(), "repo")
	require.NoError(t, SaveSessionState(state))
	path := filepath.Join(convops.GetClaudeProjectPath(state.Cwd), "conv-1.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(`{"type":"user"}`+"\n"), 0o644))
	return path
}

func appendCompactBoundary(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	_, err = f.WriteString(`{"type":"system","subtype":"compact_boundary"}` + "\n")
	require.NoError(t, err)
}

func fireHook(t *testing.T, input HookCallbackInput) {
	t.Helper()
	input.ConvID = "conv-1"
	var buf bytes.Buffer
	require.NoError(t, DispatchHookEvent(context.Background(), input, "sess-1", LocalHookAmbient(), &buf))
}

// autoCompactionHooks is what Claude Code 2.1.314 fires for an automatic
// compaction, in order. A subagent's compaction fires exactly these too, with
// the main session_id and no agent_id: nothing in them says whose it was.
func autoCompactionHooks(t *testing.T) {
	t.Helper()
	fireHook(t, HookCallbackInput{HookEventName: "PreCompact", Trigger: "auto"})
	fireHook(t, HookCallbackInput{HookEventName: "SessionStart", Source: "compact"})
	fireHook(t, HookCallbackInput{HookEventName: "PostCompact", Trigger: "auto"})
}

func TestContextReinjection_SubagentCompactionDoesNotReachMainAgent(t *testing.T) {
	reinjectionFixture(t)
	claudeTranscriptFixture(t)

	fireHook(t, HookCallbackInput{HookEventName: "SubagentStart", AgentID: "sub-1", AgentType: "general-purpose"})
	autoCompactionHooks(t)
	fireHook(t, HookCallbackInput{HookEventName: "SubagentStop", AgentID: "sub-1", AgentType: "general-purpose"})
	fireHook(t, HookCallbackInput{HookEventName: "PostToolUse", ToolName: "Agent"})

	assert.Empty(t, reinjectedMessages(t))
}

func TestContextReinjection_MainCompactionBesideSubagentQueuesAtNextMainHook(t *testing.T) {
	reinjectionFixture(t)
	transcript := claudeTranscriptFixture(t)

	fireHook(t, HookCallbackInput{HookEventName: "SubagentStart", AgentID: "sub-1"})
	autoCompactionHooks(t)
	assert.Empty(t, reinjectedMessages(t), "the hooks alone cannot say whose compaction this was")
	appendCompactBoundary(t, transcript)

	// A subagent's own activity is no evidence about the main thread.
	fireHook(t, HookCallbackInput{HookEventName: "PreToolUse", ToolName: "Read", AgentID: "sub-1"})
	assert.Empty(t, reinjectedMessages(t))

	fireHook(t, HookCallbackInput{HookEventName: "PreToolUse", ToolName: "Read"})
	assert.Len(t, reinjectedMessages(t), 1)
}

func TestContextReinjection_ForeignHookDoesNotSettleAHold(t *testing.T) {
	reinjectionFixture(t)
	transcript := claudeTranscriptFixture(t)

	fireHook(t, HookCallbackInput{HookEventName: "SubagentStart", AgentID: "sub-1"})
	autoCompactionHooks(t)
	appendCompactBoundary(t, transcript)

	QueueContextReinjection(HookCallbackInput{HookEventName: "Stop", ConvID: "conv-child"}, "sess-1")
	assert.Empty(t, reinjectedMessages(t))

	fireHook(t, HookCallbackInput{HookEventName: "Stop"})
	assert.Len(t, reinjectedMessages(t), 1)
}

func TestContextReinjection_ManualCompactBesideSubagentQueuesOnce(t *testing.T) {
	reinjectionFixture(t)
	claudeTranscriptFixture(t)

	fireHook(t, HookCallbackInput{HookEventName: "SubagentStart", AgentID: "sub-1"})
	fireHook(t, HookCallbackInput{HookEventName: "SessionStart", Source: "compact"})
	fireHook(t, HookCallbackInput{HookEventName: "PostCompact", Trigger: "manual"})
	assert.Len(t, reinjectedMessages(t), 1)

	held, err := db.PeekHeldContextReinjection("sess-1")
	require.NoError(t, err)
	assert.Nil(t, held, "the manual compaction answered the hold")
}

func TestContextReinjection_UnreadableTranscriptStillQueues(t *testing.T) {
	reinjectionFixture(t)
	transcript := claudeTranscriptFixture(t)
	require.NoError(t, os.Remove(transcript))

	fireHook(t, HookCallbackInput{HookEventName: "SubagentStart", AgentID: "sub-1"})
	autoCompactionHooks(t)

	assert.Len(t, reinjectedMessages(t), 1, "a spurious reminder beats a lost briefing")
}

func TestContextReinjection_TranscriptLostWhileHeldStillQueues(t *testing.T) {
	reinjectionFixture(t)
	transcript := claudeTranscriptFixture(t)

	fireHook(t, HookCallbackInput{HookEventName: "SubagentStart", AgentID: "sub-1"})
	autoCompactionHooks(t)
	require.NoError(t, os.Remove(transcript))
	fireHook(t, HookCallbackInput{HookEventName: "Stop"})

	assert.Len(t, reinjectedMessages(t), 1)
}

func TestContextReinjection_SubagentCompactionKeepsTheSubagentLedger(t *testing.T) {
	reinjectionFixture(t)
	claudeTranscriptFixture(t)

	fireHook(t, HookCallbackInput{HookEventName: "SubagentStart", AgentID: "sub-1"})
	autoCompactionHooks(t)

	state, err := LoadSessionState("sess-1")
	require.NoError(t, err)
	assert.True(t, state.Subagents.Has("sub-1"))
}
