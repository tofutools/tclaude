package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Background — the orphaned-reply bug
//
// A spawn's "Startup context" briefing used to be inserted with no
// FromConv. `tclaude agent reply <brief-id>` then walked an empty
// sender, inserted the reply with to_conv="" (an orphan row no inbox
// query matches), and the CLI reported "queued; target not online" —
// a status that never resolves.
//
// Fix B: handleGroupSpawn stamps the briefing's sender. Default = the
// spawn requester (an agent → its conv-id; a human → ""); the optional
// `reply_to` selector overrides it.
// Fix A: handleMessageReply rejects a reply whose resolved sender is
// empty instead of orphaning it.

// Scenario B / explicit knob: a PO spawns a worker but routes the
// worker's startup-brief replies to a THIRD agent (a coordinator),
// named by group alias via `reply_to`. The briefing's FromConv must be
// the resolved coordinator, not the spawner.
func TestSpawn_ReplyTo_RoutesStartupBriefToNamedTarget(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	const poConv = "popo-aaaa-bbbb-cccc-111111111111"
	const coordConv = "coor-aaaa-bbbb-cccc-222222222222"
	f.HaveMember("alpha", poConv, "PO")
	f.HaveMember("alpha", coordConv, "coordinator")
	require.NoError(t, db.GrantAgentPermission(poConv, agentd.PermGroupsSpawn, "test"))

	resp := f.AsAgent(poConv).SpawnWith("alpha", map[string]any{
		"alias":           "worker",
		"initial_message": "Task: do the thing",
		"reply_to":        "coordinator", // resolved via the group alias
	})
	require.Equal(t, http.StatusOK, resp.Code, "spawn body=%s", resp.Raw)
	require.NotEmpty(t, resp.ConvID, "spawn must return a conv-id")

	rows, err := db.ListAgentMessagesForConv(resp.ConvID, 100)
	require.NoError(t, err, "ListAgentMessagesForConv(worker)")
	require.Len(t, rows, 1, "worker should have exactly the startup-context message")
	assert.Equal(t, "Startup context", rows[0].Subject, "brief subject")
	assert.Equal(t, coordConv, rows[0].FromConv,
		"brief FromConv must be the --reply-to target, not the spawner")
}

// Scenario B / default: an agent (a PO) spawns a worker with no
// reply_to. The briefing's sender defaults to the PO, and — the point
// of the whole fix — the worker can `reply` to its brief and the reply
// lands in the PO's inbox, threaded under the brief.
func TestSpawn_NoReplyTo_AgentCaller_WorkerReplyReachesSpawner(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	const poConv = "popo-aaaa-bbbb-cccc-111111111111"
	f.HaveMember("alpha", poConv, "PO")
	require.NoError(t, db.GrantAgentPermission(poConv, agentd.PermGroupsSpawn, "test"))

	resp := f.AsAgent(poConv).SpawnWith("alpha", map[string]any{
		"alias":           "worker",
		"initial_message": "Task: do the thing",
	})
	require.Equal(t, http.StatusOK, resp.Code, "spawn body=%s", resp.Raw)
	require.NotEmpty(t, resp.ConvID, "spawn must return a conv-id")

	rows, err := db.ListAgentMessagesForConv(resp.ConvID, 100)
	require.NoError(t, err, "ListAgentMessagesForConv(worker)")
	require.Len(t, rows, 1, "worker should have exactly the startup-context message")
	startup := rows[0]
	assert.Equal(t, poConv, startup.FromConv, "brief sender defaults to the agent spawner")

	// The worker replies to its startup brief — this is the very call
	// the brief's "reply to PO" instruction tells it to make.
	path := "/v1/messages/" + itoa64(startup.ID) + "/reply"
	rr := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodPost, path, map[string]any{"body": "ack, on it"}), resp.ConvID)
	rec := testharness.Serve(f.Mux, rr)
	require.Equal(t, http.StatusOK, rec.Code, "reply body=%s", rec.Body.String())

	poInbox, err := db.ListAgentMessagesForConv(poConv, 100)
	require.NoError(t, err, "ListAgentMessagesForConv(PO)")
	require.Len(t, poInbox, 1, "PO should have received the worker's reply")
	assert.Equal(t, resp.ConvID, poInbox[0].FromConv, "reply is from the worker")
	assert.Equal(t, "ack, on it", poInbox[0].Body, "reply body")
	assert.Equal(t, startup.ID, poInbox[0].ParentID, "reply threaded under the brief")
}

// Scenario A: a human spawns a worker (no conv-id). The briefing's
// FromConv is empty — there is genuinely no one to reply to. A reply
// attempt must be rejected with 400, NOT silently inserted as an
// orphan with to_conv="".
func TestSpawn_HumanCaller_BriefHasNoSender_ReplyRejected(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	resp := f.AsHuman().SpawnWith("alpha", map[string]any{
		"alias":           "worker",
		"initial_message": "Task: do the thing",
	})
	require.Equal(t, http.StatusOK, resp.Code, "spawn body=%s", resp.Raw)
	require.NotEmpty(t, resp.ConvID, "spawn must return a conv-id")

	rows, err := db.ListAgentMessagesForConv(resp.ConvID, 100)
	require.NoError(t, err, "ListAgentMessagesForConv(worker)")
	require.Len(t, rows, 1, "worker should have exactly the startup-context message")
	startup := rows[0]
	assert.Empty(t, startup.FromConv, "a human-initiated spawn leaves the brief sender empty")

	// Reply to the senderless brief — must be refused observably.
	path := "/v1/messages/" + itoa64(startup.ID) + "/reply"
	rr := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodPost, path, map[string]any{"body": "done!"}), resp.ConvID)
	rec := testharness.Serve(f.Mux, rr)
	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"reply to a senderless brief must be rejected; body=%s", rec.Body.String())

	// And the rejected reply left no orphan: nothing was inserted
	// addressed to an empty to_conv.
	orphans, err := db.ListAgentMessagesForConv("", 100)
	require.NoError(t, err, "ListAgentMessagesForConv(\"\")")
	assert.Empty(t, orphans, "no message should be inserted with an empty to_conv")
}

// Scenario A / bad selector: a reply_to that resolves to nothing is a
// 400 at spawn time — the spawn fails fast rather than silently
// falling back to the spawner.
func TestSpawn_ReplyTo_UnresolvableSelector_Rejected(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	const poConv = "popo-aaaa-bbbb-cccc-111111111111"
	f.HaveMember("alpha", poConv, "PO")
	require.NoError(t, db.GrantAgentPermission(poConv, agentd.PermGroupsSpawn, "test"))

	resp := f.AsAgent(poConv).SpawnWith("alpha", map[string]any{
		"alias":           "worker",
		"initial_message": "Task: do the thing",
		"reply_to":        "nobody-by-this-name",
	})
	assert.Equal(t, http.StatusBadRequest, resp.Code,
		"an unresolvable reply_to must fail the spawn; body=%s", resp.Raw)
}
