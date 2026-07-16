package agentd_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// JOH-317: the agent inbox + per-message ownership surface keys on the STABLE
// agent_id, not the caller's current conv generation. Before the fix /v1/inbox
// and the read / reply / delete / prune gates keyed on to_conv literally, so an
// agent that reincarnated (or ran /clear) could neither SEE nor ACT ON mail it
// received under its predecessor conv — that mail was stranded on the dead
// generation. These flow tests pin the rotation case at the production surfaces
// the `tclaude agent inbox` CLI and the inbox-watch TUI drive (GET /v1/inbox,
// GET/POST/DELETE /v1/messages/{id}, POST /v1/inbox/prune).
//
// The sibling guarantee — DELIVERY of mail queued before the rotation —
// already has coverage in TestNudgeQueue_SurvivesReincarnation (JOH-310);
// these tests cover mail that is already sitting in the inbox.

// rotationParties stands up a sender + a worker agent (both enrolled group
// members) and returns the group. The worker is intentionally left OFFLINE so
// the scenarios exercise the durable inbox without an async tmux nudge in play.
func rotationParties(t *testing.T, f *testharness.Flow, sender, gen1 string) {
	t.Helper()
	f.HaveGroup("team")
	f.HaveConvWithTitle(sender, "po")
	f.HaveConvWithTitle(gen1, "worker")
	f.HaveEnrolledAgent(sender)
	f.HaveEnrolledAgent(gen1)
	f.HaveMember("team", sender)
	f.HaveMember("team", gen1)
}

// Scenario: a worker receives mail, then reincarnates (gen1 → gen2). The
// predecessor's mail must still be readable from the new generation, and a
// single-message read + reply must resolve ownership by actor.
func TestInboxRotation_PredecessorMailReadableAfterReincarnate(t *testing.T) {
	f := newFlow(t)
	const sender = "ir01-send-bbbb-cccc-000000000001"
	const gen1 = "ir01-gen1-bbbb-cccc-000000000002"
	const gen2 = "ir01-gen2-bbbb-cccc-000000000003"
	rotationParties(t, f, sender, gen1)

	// Sender mails the worker; it lands in the worker's inbox addressed to gen1.
	r := mustSend(t, f, sender, map[string]any{"to": gen1, "subject": "pre-rotation", "body": "are you there?"})
	agentd.WaitForBackgroundForTest()
	require.Equal(t, 1, inboxCount(t, f.Mux, gen1), "precondition: worker sees the message under gen1")

	// The worker reincarnates: its actor's live conv rotates gen1 → gen2.
	_, err := db.RotateAgentConv(gen1, gen2, "reincarnate")
	require.NoError(t, err, "RotateAgentConv")

	// THE FIX: the predecessor's mail is still in the one inbox, readable from
	// the live generation. Pre-JOH-317 this was 0 — stranded on gen1.
	assert.Equal(t, 1, inboxCount(t, f.Mux, gen2),
		"predecessor inbox mail must be readable from the post-reincarnate generation")

	// A single-message read resolves ownership by actor, not conv.
	readRec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodGet, "/v1/messages/"+strconv.FormatInt(r.ID, 10), nil), gen2))
	require.Equal(t, http.StatusOK, readRec.Code,
		"GET /v1/messages/{id} from the new generation must be allowed; body=%s", readRec.Body.String())
	var readDetail struct {
		Replyable bool   `json:"replyable"`
		ReplyTo   string `json:"reply_to"`
		ReplyCmd  string `json:"reply_cmd"`
	}
	require.NoError(t, json.Unmarshal(readRec.Body.Bytes(), &readDetail))
	assert.True(t, readDetail.Replyable)
	assert.NotEmpty(t, readDetail.ReplyTo)
	assert.NotEmpty(t, readDetail.ReplyCmd)

	// Reply from the new generation is accepted (the recipient gate is
	// agent-keyed) and routes back to the sender.
	replyRec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost, "/v1/messages/"+strconv.FormatInt(r.ID, 10)+"/reply",
			map[string]any{"body": "yes, reincarnated"}), gen2))
	require.Equal(t, http.StatusOK, replyRec.Code,
		"reply from the new generation must be allowed; body=%s", replyRec.Body.String())
	agentd.WaitForBackgroundForTest()
	assert.Equal(t, 1, inboxCount(t, f.Mux, sender), "reply routed back to the sender's inbox")
}

// Scenario: a worker receives mail, then reincarnates. Deleting the
// predecessor's message from the new generation must succeed — ownership is
// agent-keyed. Pre-JOH-317 the conv-keyed delete matched neither from_conv nor
// to_conv = gen2, so it 404'd and the mail could never be cleared.
func TestInboxRotation_PredecessorMailDeletableAfterReincarnate(t *testing.T) {
	f := newFlow(t)
	const sender = "ir02-send-bbbb-cccc-000000000001"
	const gen1 = "ir02-gen1-bbbb-cccc-000000000002"
	const gen2 = "ir02-gen2-bbbb-cccc-000000000003"
	rotationParties(t, f, sender, gen1)

	r := mustSend(t, f, sender, map[string]any{"to": gen1, "body": "delete me after rotation"})
	agentd.WaitForBackgroundForTest()

	_, err := db.RotateAgentConv(gen1, gen2, "reincarnate")
	require.NoError(t, err, "RotateAgentConv")

	delRec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodDelete, "/v1/messages/"+strconv.FormatInt(r.ID, 10), nil), gen2))
	require.Equal(t, http.StatusOK, delRec.Code,
		"DELETE from the new generation must remove the predecessor's row; body=%s", delRec.Body.String())
	assert.Equal(t, 0, inboxCount(t, f.Mux, gen2), "the predecessor's row is gone from the one inbox")
}

// Scenario: a worker has old mail, then reincarnates. `inbox prune` from the
// new generation must reap the predecessor's aged mail — the prune is
// agent-scoped, not limited to the current conv's slice.
func TestInboxRotation_PredecessorMailPrunableAfterReincarnate(t *testing.T) {
	f := newFlow(t)
	const sender = "ir03-send-bbbb-cccc-000000000001"
	const gen1 = "ir03-gen1-bbbb-cccc-000000000002"
	const gen2 = "ir03-gen2-bbbb-cccc-000000000003"
	rotationParties(t, f, sender, gen1)

	// Insert an aged message directly so it predates the prune cutoff. to_agent
	// is denormalised from gen1 (an enrolled agent) at insert.
	msgID, err := db.InsertAgentMessage(&db.AgentMessage{
		FromConv:  sender,
		ToConv:    gen1,
		Body:      "ancient mail",
		CreatedAt: time.Now().Add(-time.Hour),
	})
	require.NoError(t, err, "InsertAgentMessage")
	require.Equal(t, 1, inboxCount(t, f.Mux, gen1), "precondition: aged mail in gen1's inbox")

	_, err = db.RotateAgentConv(gen1, gen2, "reincarnate")
	require.NoError(t, err, "RotateAgentConv")

	pruneRec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost, "/v1/inbox/prune",
			map[string]any{"older_than_seconds": 60}), gen2))
	require.Equal(t, http.StatusOK, pruneRec.Code, "prune from the new generation; body=%s", pruneRec.Body.String())

	// The prune must report it actually reaped the predecessor's row — the
	// discriminating assertion. Pre-JOH-317 the conv-keyed prune matched
	// nothing under gen2 and returned deleted=0 (the row would survive,
	// merely invisible to the equally-broken inbox read).
	var pruneResp struct {
		Deleted int64 `json:"deleted"`
	}
	require.NoError(t, json.Unmarshal(pruneRec.Body.Bytes(), &pruneResp), "decode prune resp")
	assert.Equal(t, int64(1), pruneResp.Deleted,
		"prune from the new generation must reap the predecessor's aged mail")

	// And the row is genuinely gone from storage, not just hidden.
	gone, err := db.GetAgentMessage(msgID)
	require.NoError(t, err)
	assert.Nil(t, gone, "the pruned row must be deleted from storage")
}

// Scenario: a conv is messaged BEFORE it enrolls as an agent, so the row's
// to_agent companion is ” (derived from an unmapped to_conv at insert) and is
// never backfilled. After the conv enrolls, its inbox must STILL surface — and
// let it delete — that mail. A purely agent-keyed query (to_agent = ?) would
// miss it; the actor-keyed query keeps the to_conv term precisely for this
// case, mirroring the delivery layer's conv-keyed drain.
func TestInboxRotation_PreEnrollmentMailVisibleAfterEnroll(t *testing.T) {
	f := newFlow(t)
	const sender = "ir04-send-bbbb-cccc-000000000001"
	const late = "ir04-late-bbbb-cccc-000000000002"
	f.HaveGroup("team")
	f.HaveConvWithTitle(sender, "po")
	f.HaveEnrolledAgent(sender)
	f.HaveMember("team", sender)

	// `late` is NOT yet an agent: a message to it stores to_agent=''.
	id, err := db.InsertAgentMessage(&db.AgentMessage{
		FromConv: sender,
		ToConv:   late,
		Body:     "hello, future agent",
	})
	require.NoError(t, err, "InsertAgentMessage")
	stored, err := db.GetAgentMessage(id)
	require.NoError(t, err)
	require.Empty(t, stored.ToAgent, "precondition: to_agent is '' for a pre-enrollment recipient")

	// `late` now enrolls. The existing row's to_agent companion is not
	// backfilled, so only the to_conv term can still reach it.
	f.HaveConvWithTitle(late, "latecomer")
	f.HaveEnrolledAgent(late)
	f.HaveMember("team", late)
	gotAgent, err := db.AgentIDForConv(late)
	require.NoError(t, err)
	require.NotEmpty(t, gotAgent, "late must now resolve to an actor")

	// The inbox surfaces the pre-enrollment mail (kept by the to_conv term).
	assert.Equal(t, 1, inboxCount(t, f.Mux, late),
		"pre-enrollment mail must stay visible after the recipient enrolls")

	// And it remains deletable from that same conv.
	delRec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodDelete, "/v1/messages/"+strconv.FormatInt(id, 10), nil), late))
	require.Equal(t, http.StatusOK, delRec.Code,
		"pre-enrollment mail must stay deletable; body=%s", delRec.Body.String())
	assert.Equal(t, 0, inboxCount(t, f.Mux, late), "the row is gone after delete")
}
