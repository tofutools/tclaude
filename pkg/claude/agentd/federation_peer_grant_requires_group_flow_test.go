package agentd_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// fedAutoTeleport sends an automatic teleport of an agent carrying perms into
// group and returns the landed agent's permission overrides and last brief.
func fedAutoTeleport(t *testing.T, fh *fedHarness, n int, group string, perms []agentbundle.Permission) (map[string]string, string) {
	t.Helper()
	b := fedAgentBundle(t)
	b.Manifest.Agent.CarryPermissions = true
	b.Manifest.Agent.Permissions = perms
	// The test peer signs as this agent (fedPeer.envelope).
	source := "agt_bobremote0000000000000000"
	sourceConv := fmt.Sprintf("019fe740-43a4-7023-b8ae-%012d", n)
	transcript := append(mustJSON(t, map[string]any{"type": "user", "sessionId": sourceConv, "cwd": "/source",
		"message": map[string]string{"content": "Continue the test task."}}), '\n')
	b.SetHistory("claude-jsonl", sourceConv, transcript)
	raw, err := b.Encode()
	require.NoError(t, err)
	d := bundletransfer.New(bundletransfer.Agent, raw, "Teleport continuation", time.Now().Add(time.Hour))
	d.Group = group
	d.Teleport = &bundletransfer.TeleportIntent{Version: 1, Chain: proto.NewEnvelopeID(), OriginInstance: fh.peer.id.ID(),
		OriginAgent: source, SourceAgent: source, SourceConv: sourceConv, Note: "Finish the task",
		Hops: []bundletransfer.TeleportHop{{Offer: d.ID, FromInstance: fh.peer.id.ID(), FromAgent: source,
			ToInstance: fh.peer.agentdID, ToGroup: group, At: time.Now().UTC()}}}
	d.Move = &bundletransfer.MoveIntent{SourceAgent: source, SourceConv: sourceConv}
	env := fh.peer.envelope(proto.KindBundleOffer, proto.Endpoint{}, d)
	env.ID = d.ID
	fh.peer.send(env)
	ack := fedAckFor(t, fh.peer, d.ID)
	require.Equal(t, proto.AckAccepted, ack.Status, "%+v", ack)
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
	inbox, err := db.ListInboxForActor(a.CurrentConvID, a.AgentID, 10)
	require.NoError(t, err)
	require.NotEmpty(t, inbox)
	return landed, inbox[len(inbox)-1].Body
}

// A node profile saved before its validation refused an unscoped
// agents.receive.permissions still writes that grant when applied. Unscoped,
// it would let permissions carry into every group the peer lands agents in,
// through the automatic teleport landing that no operator reviews. It
// authorizes nothing, and the operator sees it marked inert (tcl-kpksgl).
func TestFederationUnscopedRequiresGroupGrantIsInert(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("receiver")
	receiver, err := db.GetAgentGroupByName("receiver")
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "landing", Harness: "claude", Approval: "default"})
	require.NoError(t, err)
	p := fedNodeProfile(t, fh, "legacy-node", db.FederationNodeProfileSpec{
		PeerGrants: []db.FederationPeerGrant{
			{Slug: agentd.PermAgentsTeleportReceive, Scope: "group=receiver"},
			{Slug: agentd.PermAgentsReceivePermissions, Scope: "group=receiver"},
		},
		TeleportLanding: &db.FederationTeleportLanding{Group: "receiver", Cwd: testutil.CanonicalTempDir(t), SpawnProfile: "landing", MaxLive: 2},
	})
	// As stored before the validation: the carry grant without a group.
	stored, err := db.GetFederationNodeProfile(p.ID)
	require.NoError(t, err)
	for i, g := range stored.Definition.PeerGrants {
		if g.Slug == agentd.PermAgentsReceivePermissions {
			stored.Definition.PeerGrants[i].Scope = ""
		}
	}
	require.NoError(t, db.SaveFederationNodeProfile(stored))
	stored, err = db.GetFederationNodeProfile(p.ID)
	require.NoError(t, err)
	fedApplyNodeProfile(t, fh, stored)

	carried := []agentbundle.Permission{{Slug: agentd.PermFleetRead, Effect: "grant"}}
	landed, brief := fedAutoTeleport(t, fh, 1, "receiver", carried)
	assert.NotContains(t, landed, agentd.PermFleetRead, "an unscoped carry grant must not admit carried permissions")
	assert.Contains(t, brief, "Dropped: "+agentd.PermFleetRead+": receiver has not granted agents.receive.permissions")

	rec := fedHuman(t, fh.f, http.MethodGet, "/v1/federation/grants?peer=bob", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var grants struct {
		Grants []struct {
			Slug  string `json:"slug"`
			Scope string `json:"scope"`
			Inert string `json:"inert"`
		} `json:"grants"`
	}
	testharness.DecodeJSON(t, rec, &grants)
	inert := map[string]string{}
	for _, g := range grants.Grants {
		inert[g.Slug+"|"+g.Scope] = g.Inert
	}
	assert.Equal(t, "needs a group scope: grant it per group", inert[agentd.PermAgentsReceivePermissions+"|"], "the unscoped grant is marked inert")
	assert.Empty(t, inert[agentd.PermAgentsTeleportReceive+"|group_id="+itoa64(receiver.ID)], "a scoped grant is not")

	rec = fedHuman(t, fh.f, http.MethodGet, "/v1/federation/profiles/legacy-node", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var view struct {
		Warnings []string `json:"warnings"`
	}
	testharness.DecodeJSON(t, rec, &view)
	assert.Equal(t, []string{"peer grant agents.receive.permissions is inert: needs a group scope: grant it per group"}, view.Warnings)

	// Granted on the group, the same carry lands.
	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: fh.peer.id.ID(), Slug: agentd.PermAgentsReceivePermissions, Scope: db.FederationGroupScope(receiver.ID)}))
	landed, _ = fedAutoTeleport(t, fh, 2, "receiver", carried)
	assert.Equal(t, "grant", landed[agentd.PermFleetRead], "a group-scoped carry grant admits it")
}
