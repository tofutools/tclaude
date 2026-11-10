package agentd_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type queuedFedRequestRow struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	Position int    `json:"position"`
	Reason   string `json:"reason"`
}

func fedRequestsByName(t *testing.T, f *testharness.Flow) map[string]queuedFedRequestRow {
	t.Helper()
	rec := fedHuman(t, f, http.MethodGet, "/v1/federation/spawn-requests", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var rows []queuedFedRequestRow
	testharness.DecodeJSON(t, rec, &rows)
	out := map[string]queuedFedRequestRow{}
	for _, r := range rows {
		out[r.Name] = r
	}
	return out
}

func queueFedHarness(t *testing.T) (*fedHarness, *fedPeer) {
	t.Helper()
	fh := newFedHarness(t)
	q := addPlacementPeer(t, fh, "carol")
	fh.f.HaveGroup("team")
	g, err := db.GetAgentGroupByName("team")
	require.NoError(t, err)
	for _, peer := range []*fedPeer{fh.peer, q} {
		require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: peer.id.ID(), Slug: agentd.PermGroupsRosterRead, Scope: db.FederationGroupScope(g.ID)}))
	}
	setCapacity(t, config.CapacityConfig{MaxLiveAgents: 1, WhenFull: config.CapacityWhenFullQueue})
	return fh, q
}

// Scenario: max 1, queue mode. A pending remote request holds the slot; a
// second peer's request is accepted as queued at position 1 (old requesters
// just see accepted), shows queued in `federation requests` and the spawn
// queue, cannot be approved past the queue, and a later local spawn waits
// behind it. When the first request is denied, the queue hands the remote
// request on as pending for the operator, and the local spawn moves up.
func TestSpawnQueue_RemoteRequestQueuesInSharedFIFO(t *testing.T) {
	fh, q := queueFedHarness(t)
	f, p := fh.f, fh.peer

	first := p.envelope(proto.KindSpawnReq, proto.Endpoint{}, proto.SpawnRequestPayload{Group: "team", Name: "first", Brief: "first"})
	p.send(first)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, p, first.ID).Status)

	second := q.envelope(proto.KindSpawnReq, proto.Endpoint{}, proto.SpawnRequestPayload{Group: "team", Name: "second", Brief: "second brief text"})
	q.send(second)
	ack := fedAckFor(t, q, second.ID)
	require.Equal(t, proto.AckAccepted, ack.Status, "an older requester sees an accepted request")
	assert.Equal(t, "queued", ack.Code)
	assert.Equal(t, "position 1", ack.Reason)

	rows := fedRequestsByName(t, f)
	require.Equal(t, "pending", rows["first"].Status)
	require.Equal(t, "queued", rows["second"].Status)
	assert.Equal(t, 1, rows["second"].Position)

	rec := fedHuman(t, f, http.MethodPost, fmt.Sprintf("/v1/federation/spawn-requests/%d/approve", rows["second"].ID), map[string]any{})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "capacity queue")

	local := queuedResp(t, f.AsHuman().SpawnWith("team", map[string]any{"name": "local"}))
	assert.Equal(t, 2, local.Position, "local and remote spawns share one line")

	list, raw := spawnQueueList(t, f, "")
	require.Len(t, list.Entries, 2)
	assert.Equal(t, "federation", list.Entries[0].RequesterKind)
	assert.Contains(t, list.Entries[0].RequestedBy, "(remote)")
	assert.NotContains(t, raw, "brief text", "the remote brief is not listed")

	// A resend while it waits gets the same queued answer, and no second row.
	before := len(q.envelopes(proto.KindAck))
	q.send(second)
	fedEventually(t, "duplicate queued ack", func() bool { return len(q.envelopes(proto.KindAck)) > before })
	acks := q.envelopes(proto.KindAck)
	var dup proto.AckPayload
	require.NoError(t, acks[len(acks)-1].DecodePayload(&dup))
	assert.Equal(t, "queued", dup.Code)
	assert.Len(t, fedRequestsByName(t, f), 2)

	// Still full: a pass changes nothing.
	agentd.RunSpawnQueueForTest()
	require.Equal(t, "queued", fedRequestsByName(t, f)["second"].Status)

	rec = fedHuman(t, f, http.MethodPost, fmt.Sprintf("/v1/federation/spawn-requests/%d/deny", rows["first"].ID), map[string]any{"reason": "free the slot"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	inboxBefore := len(fedInbox(t, f))
	agentd.RunSpawnQueueForTest()

	rows = fedRequestsByName(t, f)
	require.Equal(t, "pending", rows["second"].Status, "handed on to the operator's decision")
	assert.Greater(t, len(fedInbox(t, f)), inboxBefore, "the operator is told once it is pending")
	list, _ = spawnQueueList(t, f, "")
	require.Len(t, list.Entries, 1)
	assert.Equal(t, 1, list.Entries[0].Position, "the local spawn moved up")
	assert.Equal(t, "team", list.Entries[0].Group)
}

// Scenario: cancelling a queued remote request through `agent queue rm`, or
// denying it through `federation requests deny`, ends both the request and
// its queue entry, and tells the requester.
func TestSpawnQueue_RemoteRequestCancelAndDeny(t *testing.T) {
	fh, q := queueFedHarness(t)
	f, p := fh.f, fh.peer

	hold := p.envelope(proto.KindSpawnReq, proto.Endpoint{}, proto.SpawnRequestPayload{Group: "team", Name: "hold", Brief: "hold"})
	p.send(hold)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, p, hold.ID).Status)
	a := q.envelope(proto.KindSpawnReq, proto.Endpoint{}, proto.SpawnRequestPayload{Group: "team", Name: "a", Brief: "a"})
	q.send(a)
	require.Equal(t, "queued", fedAckFor(t, q, a.ID).Code)
	b := q.envelope(proto.KindSpawnReq, proto.Endpoint{}, proto.SpawnRequestPayload{Group: "team", Name: "b", Brief: "b"})
	q.send(b)
	require.Equal(t, "position 2", fedAckFor(t, q, b.ID).Reason)

	list, _ := spawnQueueList(t, f, "")
	require.Len(t, list.Entries, 2)
	require.Equal(t, http.StatusOK, cancelQueued(t, f, list.Entries[0].ID, ""))

	rows := fedRequestsByName(t, f)
	require.Equal(t, "denied", rows["a"].Status)
	assert.Contains(t, rows["a"].Reason, "cancelled")
	assert.Equal(t, 1, rows["b"].Position)

	rec := fedHuman(t, f, http.MethodPost, fmt.Sprintf("/v1/federation/spawn-requests/%d/deny", rows["b"].ID), map[string]any{"reason": "not now"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "denied", fedRequestsByName(t, f)["b"].Status)
	list, _ = spawnQueueList(t, f, "")
	assert.Empty(t, list.Entries)

	fedEventually(t, "denied results sent", func() bool {
		denied := 0
		for _, env := range q.envelopes(proto.KindSpawnRes) {
			var res proto.SpawnResultPayload
			if env.DecodePayload(&res) == nil && res.Status == proto.SpawnDenied {
				denied++
			}
		}
		return denied == 2
	})
}

// Scenario: refuse mode keeps the old terminal busy answer, and an
// automatically placed request is refused, not queued, even in queue mode,
// so placement can try the next node.
func TestSpawnQueue_RemoteRefusedWhenNotQueueable(t *testing.T) {
	fh, q := queueFedHarness(t)
	p := fh.peer

	hold := p.envelope(proto.KindSpawnReq, proto.Endpoint{}, proto.SpawnRequestPayload{Group: "team", Name: "hold", Brief: "hold"})
	p.send(hold)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, p, hold.ID).Status)

	placed := q.envelope(proto.KindSpawnReq, proto.Endpoint{}, proto.SpawnRequestPayload{Group: "team", Name: "placed", Brief: "placed", PlacementVersion: 1})
	q.send(placed)
	ack := fedAckFor(t, q, placed.ID)
	require.Equal(t, proto.AckRefused, ack.Status)
	assert.Equal(t, "node_busy", ack.Code)

	setCapacity(t, config.CapacityConfig{MaxLiveAgents: 1})
	plain := q.envelope(proto.KindSpawnReq, proto.Endpoint{}, proto.SpawnRequestPayload{Group: "team", Name: "plain", Brief: "plain"})
	q.send(plain)
	ack = fedAckFor(t, q, plain.ID)
	require.Equal(t, proto.AckRefused, ack.Status)
	assert.Equal(t, "node_busy", ack.Code)
}

// Scenario: the requester records a peer's queued acknowledgement as
// "queued at peer (#N)" on its outbound request.
func TestSpawnQueue_RequesterSeesQueuedAtPeer(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	publishPlacementNode(t, p, placementNode(0), "builders")
	rec := fedHuman(t, f, http.MethodPost, "/v1/federation/spawn-requests", map[string]any{"peer": "bob", "group": "builders", "brief": "work"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var sent struct {
		EnvelopeID string `json:"envelope_id"`
	}
	testharness.DecodeJSON(t, rec, &sent)
	fedEventually(t, "spawn request delivered", func() bool { return len(p.envelopes(proto.KindSpawnReq)) == 1 })
	ack := p.envelope(proto.KindAck, proto.Endpoint{}, proto.AckPayload{Status: proto.AckAccepted, Code: "queued", Reason: "position 3"})
	ack.InReplyTo = p.envelopes(proto.KindSpawnReq)[0].ID
	p.send(ack)
	fedEventually(t, "queued note", func() bool {
		row, err := db.GetFederationOutbox(sent.EnvelopeID)
		return err == nil && row != nil && row.State == db.FedOutboxAccepted && row.LastError == "queued at peer (#3)"
	})
}

// Scenario: placement passes over a node that advertises queued spawns, even
// when it is the least loaded, for one with free capacity.
func TestSpawnQueue_PlacementSkipsNodeWithQueue(t *testing.T) {
	fh := newFedHarness(t)
	p := fh.peer
	q := addPlacementPeer(t, fh, "carol")
	queued := placementNode(0)
	queued.SpawnWhenFull = "queue"
	queued.Resources.Agents.QueuedSpawns = 2
	publishPlacementNode(t, p, queued, "builders")
	publishPlacementNode(t, q, placementNode(3), "builders")
	replyPlacementRequests(t, q, "", nil)
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/spawn-requests", map[string]any{"node": "auto", "brief": "work"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out placementResult
	testharness.DecodeJSON(t, rec, &out)
	require.Equal(t, q.id.ID(), out.Placement.Selected)
	require.Empty(t, p.envelopes(proto.KindSpawnReq))
	for _, c := range out.Placement.Candidates {
		if c.Instance == p.id.ID() {
			assert.Contains(t, c.Reason, "queued")
		}
	}
}
