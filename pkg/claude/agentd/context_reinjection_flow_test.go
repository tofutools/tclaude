package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// reinjectedInbox returns the re-injected startup-context messages in the
// agent's inbox, read the way `tclaude agent inbox ls` reads it.
func reinjectedInbox(t *testing.T, convID, agentID string) []*db.AgentMessage {
	t.Helper()
	rows, err := db.ListInboxForActor(convID, agentID, 100)
	require.NoError(t, err)
	var out []*db.AgentMessage
	for _, m := range rows {
		if db.IsReinjectedContextSubject(m.Subject) {
			out = append(out, m)
		}
	}
	return out
}

// Scenario: the human spawns an agent into a group with shared startup
// context and a task brief. Later the agent's context is compacted, and
// later still it runs /clear.
//
// Expected:
//   - After the compaction its inbox holds one re-injected startup-context
//     message carrying its identity, the group's startup context as it reads
//     NOW (edited after spawn), and a pointer to the original briefing —
//     not the brief itself.
//   - After /clear a second message lands on the new conversation carrying
//     the identity only.
func TestContextReinjection_AfterCompactAndClear(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	_, err := db.SetAgentGroupDefaultContext("alpha", "Use git worktrees and PRs.")
	require.NoError(t, err)

	const brief = "Investigate the flaky federation test and report back"
	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name":            "investigator",
		"initial_message": brief,
	})
	require.Equal(t, http.StatusOK, spawn.Code, "spawn: %s", spawn.Raw)
	f.AssertSpawnName(spawn.ConvID, "investigator", 10*time.Second)

	briefing, err := db.ListAgentMessagesForConv(spawn.ConvID, 10)
	require.NoError(t, err)
	require.Len(t, briefing, 1, "the spawn briefing")

	// The operator edits the group context after the spawn.
	_, err = db.SetAgentGroupDefaultContext("alpha", "Use git worktrees and PRs. Report flaky tests to awb.")
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, f.Compact(spawn.ConvID).Code)
	var after []*db.AgentMessage
	require.Eventually(t, func() bool {
		after = reinjectedInbox(t, spawn.ConvID, spawn.AgentID)
		return len(after) == 1
	}, 10*time.Second, 20*time.Millisecond, "compaction should queue one re-injection")
	body := after[0].Body
	assert.Contains(t, body, "Your context was compacted")
	assert.Contains(t, body, `"investigator"`)
	assert.Contains(t, body, `in group "alpha"`)
	assert.Contains(t, body, "Report flaky tests to awb.", "group context is read live")
	assert.Contains(t, body, "tclaude agent inbox read", "points at the original briefing")
	assert.NotContains(t, body, brief, "the brief is referenced, never re-pasted")

	c := f.Clear(spawn.Label)
	var cleared []*db.AgentMessage
	require.Eventually(t, func() bool {
		cleared = reinjectedInbox(t, c.NewConv, spawn.AgentID)
		for _, m := range cleared {
			if m.ToConv == c.NewConv {
				return true
			}
		}
		return false
	}, 10*time.Second, 20*time.Millisecond, "/clear should queue an identity re-injection")
	var clearMsg *db.AgentMessage
	for _, m := range cleared {
		if m.ToConv == c.NewConv {
			clearMsg = m
		}
	}
	assert.Contains(t, clearMsg.Body, "Your conversation was cleared")
	assert.Contains(t, clearMsg.Body, `in group "alpha"`)
	assert.NotContains(t, clearMsg.Body, "Use git worktrees", "/clear re-injects identity only")
}

func setGroupReinject(t *testing.T, f *testharness.Flow, group, mode string) *httptest.ResponseRecorder {
	t.Helper()
	r := agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodPatch, "/v1/groups/"+group,
		map[string]any{"reinject_after_compact": mode}))
	return testharness.Serve(f.Mux, r)
}

// Scenario: the operator changes a group's "re-inject after compact" setting
// in group settings.
//
// Expected: the setting round-trips and rejects an unknown value; "identity"
// trims a compaction's re-injection to the identity, and "off" suppresses
// re-injection entirely (a /clear queues nothing).
func TestContextReinjection_GroupSetting(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	_, err := db.SetAgentGroupDefaultContext("alpha", "Use git worktrees and PRs.")
	require.NoError(t, err)

	rec := setGroupReinject(t, f, "alpha", "sometimes")
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	for _, mode := range []string{"off", "contexts", "identity"} {
		rec = setGroupReinject(t, f, "alpha", mode)
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		assert.Contains(t, rec.Body.String(), `"reinject_after_compact":"`+mode+`"`)
	}

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{"name": "worker"})
	require.Equal(t, http.StatusOK, spawn.Code, "spawn: %s", spawn.Raw)
	f.AssertSpawnName(spawn.ConvID, "worker", 10*time.Second)

	require.Equal(t, http.StatusOK, f.Compact(spawn.ConvID).Code)
	var msgs []*db.AgentMessage
	require.Eventually(t, func() bool {
		msgs = reinjectedInbox(t, spawn.ConvID, spawn.AgentID)
		return len(msgs) == 1
	}, 10*time.Second, 20*time.Millisecond)
	assert.Contains(t, msgs[0].Body, `in group "alpha"`)
	assert.NotContains(t, msgs[0].Body, "Use git worktrees", "identity mode skips the startup context")

	require.Equal(t, http.StatusOK, setGroupReinject(t, f, "alpha", "off").Code)
	c := f.Clear(spawn.Label)
	// f.Clear returns after the simulator's hooks ran synchronously, so a
	// queued message would already be visible.
	for _, m := range reinjectedInbox(t, c.NewConv, spawn.AgentID) {
		assert.NotEqual(t, db.ReinjectedAfterClearSubject, m.Subject, "off queues nothing after /clear")
	}
}

// An invalid re-inject value fails the whole PATCH before any field is written.
func TestContextReinjection_InvalidSettingWritesNothing(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	r := agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPatch, "/v1/groups/alpha",
		map[string]any{"descr": "changed", "reinject_after_compact": "bogus"}))
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	g, err := db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	assert.NotEqual(t, "changed", g.Descr)
}

// Scenario: a Copilot agent's context is compacted. Copilot has no
// post-compaction hook; the only record is session.compaction_complete in its
// event log. Nobody has the dashboard open.
//
// Expected:
//   - A compaction that failed queues nothing, and neither does a subagent's.
//   - The daemon's poller alone finds a real compaction and queues one
//     re-injection, which is not repeated on later ticks.
//   - A later /clear, also known only from the log, queues the identity.
func TestContextReinjection_CopilotCompactionFromEventLog(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("alpha")
	_, err := db.SetAgentGroupDefaultContext("alpha", "Use git worktrees and PRs.")
	require.NoError(t, err)
	resp, sim := spawnCopilot(t, f, "alpha", map[string]any{
		"trust_dir": true,
		"name":      "copilot-worker",
	})
	inbox := func() []*db.AgentMessage { return reinjectedInbox(t, resp.ConvID, resp.AgentID) }
	sweep := func() {
		time.Sleep(2100 * time.Millisecond) // past the per-session refresh throttle
		agentd.SweepCopilotUsageForTest()
	}
	agentd.SweepCopilotUsageForTest()

	require.NoError(t, sim.AppendEvent(map[string]any{
		"type": "session.compaction_complete",
		"data": map[string]any{"success": false},
	}))
	require.NoError(t, sim.AppendEvent(map[string]any{
		"type":    "session.compaction_complete",
		"agentId": "sub-1",
		"data":    map[string]any{"success": true},
	}))
	sweep()
	assert.Empty(t, inbox(), "a failed or subagent compaction leaves the briefing in place")

	require.NoError(t, sim.AppendEvent(map[string]any{
		"type": "session.compaction_complete",
		"data": map[string]any{"success": true, "postCompactionTokens": 12000, "tokenLimit": 128000},
	}))
	sweep()
	got := inbox()
	require.Len(t, got, 1, "the poller should queue one re-injection")
	assert.Contains(t, got[0].Body, "Your context was compacted")
	assert.Contains(t, got[0].Body, "Use git worktrees and PRs.")

	sweep()
	assert.Len(t, inbox(), 1, "one compaction is answered once")

	require.NoError(t, sim.AppendEvent(map[string]any{
		"type": "session.context_cleared",
		"data": map[string]any{"messagesCleared": 3},
	}))
	sweep()
	got = inbox()
	require.Len(t, got, 2, "a /clear in the log queues its own re-injection")
	assert.Contains(t, got[0].Body+got[1].Body, "Your conversation was cleared")
}

// Scenario: a Copilot conversation with compactions from an earlier lifetime
// is followed for the first time, as after a resume with no checkpoint.
//
// Expected: only a compaction after that first look queues a re-injection.
func TestContextReinjection_CopilotIgnoresEarlierCompactions(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("alpha")
	_, err := db.SetAgentGroupDefaultContext("alpha", "Use git worktrees and PRs.")
	require.NoError(t, err)
	resp, sim := spawnCopilot(t, f, "alpha", map[string]any{
		"trust_dir": true,
		"name":      "copilot-worker",
	})
	compaction := map[string]any{
		"type": "session.compaction_complete",
		"data": map[string]any{"success": true},
	}
	require.NoError(t, sim.AppendEvent(compaction))
	agentd.SweepCopilotUsageForTest()
	assert.Empty(t, reinjectedInbox(t, resp.ConvID, resp.AgentID))

	require.NoError(t, sim.AppendEvent(compaction))
	time.Sleep(2100 * time.Millisecond) // past the per-session refresh throttle
	agentd.SweepCopilotUsageForTest()
	assert.Len(t, reinjectedInbox(t, resp.ConvID, resp.AgentID), 1)
}
