package agentd_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// This pins the import's merge of carried permissions into a teleport
// landing's receiver policy: a carried grant of a slug the receiving node's
// worker policy denies is dropped (the brief says so) and the landed agent
// keeps the deny, while a carried row the policy leaves alone lands.
func TestAgentBundleImport_ReceiverWorkerDenyWinsOverACarriedGrant(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("receiver")
	setFedTrustLevel(t, fh, "unrestricted")
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "landing", Harness: "claude", Approval: "default"})
	require.NoError(t, err)
	p := fedNodeProfile(t, fh, "landing-node", db.FederationNodeProfileSpec{
		PeerGrants: []db.FederationPeerGrant{
			{Slug: agentd.PermAgentsTeleportReceive, Scope: "group=receiver"},
			{Slug: "agents.receive.permissions", Scope: "group=receiver"},
		},
		WorkerPermissions: map[string]db.PermissionOverride{"groups.members.spawn": {Effect: "deny"}},
		TeleportLanding:   &db.FederationTeleportLanding{Group: "receiver", Cwd: testutil.CanonicalTempDir(t), SpawnProfile: "landing", MaxLive: 1},
	})
	fedApplyNodeProfile(t, fh, p)

	b := fedAgentBundle(t)
	b.Manifest.Agent.CarryPermissions = true
	b.Manifest.Agent.Permissions = []agentbundle.Permission{
		{Slug: "groups.members.spawn", Effect: "grant", Scope: json.RawMessage(`{"group":["source"]}`)},
		{Slug: "permissions.revoke", Effect: "deny"},
	}
	transcript := append(mustJSON(t, map[string]any{"type": "user", "sessionId": moveSourceConv, "cwd": "/source",
		"message": map[string]string{"content": "Continue the test task."}}), '\n')
	b.SetHistory("claude-jsonl", moveSourceConv, transcript)
	raw, err := b.Encode()
	require.NoError(t, err)
	d := bundletransfer.New(bundletransfer.Agent, raw, "Teleport continuation", time.Now().Add(time.Hour))
	d.Group = "receiver"
	source := "agt_bobremote0000000000000000"
	d.Teleport = &bundletransfer.TeleportIntent{Version: 1, Chain: proto.NewEnvelopeID(), OriginInstance: fh.peer.id.ID(),
		OriginAgent: source, SourceAgent: source, SourceConv: moveSourceConv, Note: "Finish the task",
		Hops: []bundletransfer.TeleportHop{{Offer: d.ID, FromInstance: fh.peer.id.ID(), FromAgent: source,
			ToInstance: fh.peer.agentdID, ToGroup: "receiver", At: time.Now().UTC()}}}
	d.Move = &bundletransfer.MoveIntent{SourceAgent: source, SourceConv: moveSourceConv}
	env := fh.peer.envelope(proto.KindBundleOffer, proto.Endpoint{}, d)
	env.ID = d.ID
	fh.peer.send(env)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, d.ID).Status)

	var row *db.FederationTeleport
	fedEventually(t, "automatic teleport landing", func() bool {
		row, _ = db.GetFederationTeleport("in", fh.peer.id.ID(), d.ID)
		return row != nil && row.State != "auto_pending" && row.State != "admitting"
	})
	require.Equal(t, "landed", row.State)
	a, err := db.GetAgent(row.TargetAgent)
	require.NoError(t, err)
	require.NotNil(t, a)
	landed, err := db.ListAgentPermissionOverridesForConv(a.CurrentConvID)
	require.NoError(t, err)
	require.Equal(t, "deny", landed["groups.members.spawn"], "the receiver's worker deny wins over the carried grant")
	require.Equal(t, "deny", landed["permissions.revoke"], "a carried deny the policy leaves alone lands")
	inbox, err := db.ListInboxForActor(a.CurrentConvID, a.AgentID, 10)
	require.NoError(t, err)
	require.NotEmpty(t, inbox)
	assert.Contains(t, inbox[len(inbox)-1].Body,
		"Dropped: groups.members.spawn: receiver worker policy deny wins",
		"the brief reports the carried grant as dropped")
}
