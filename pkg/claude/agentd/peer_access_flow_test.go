package agentd_test

import (
	"encoding/json"
	"fmt"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
	"strings"
	"testing"
	"time"
)

func TestPeerAccessProductionAdmissionApprovalExpiry(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("shared")
	group, err := db.GetAgentGroupByName("shared")
	require.NoError(t, err)
	h := agentd.PeerViewHandler(fh.peer.id.ID())
	request := func(body any) *db.FederationPeerAccessRequest {
		rec := testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/peer-access-requests", body))
		require.Equal(t, 202, rec.Code, rec.Body.String())
		var row db.FederationPeerAccessRequest
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &row))
		return &row
	}
	denied := testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/peer-access-requests", map[string]any{"permission": agentd.PermMessageDirect}))
	require.Equal(t, 403, denied.Code, "trust/public summaries do not admit requests")
	require.Equal(t, 200, fedHuman(t, fh.f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermGroupsRosterRead, "scope": "group=shared"}).Code)
	denied = testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/peer-access-requests", map[string]any{"permission": agentd.PermSessionsAttach}))
	require.Equal(t, 400, denied.Code, "local-only cannot be requested")
	row := request(map[string]any{"permission": agentd.PermMessageDirect, "group_id": group.ID, "reason": "coordinate", "grant_ttl_seconds": 1})
	require.Equal(t, 404, testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/peer-access-requests/not-this-id", nil)).Code)
	require.Equal(t, 403, testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/federation/access-requests/"+row.ID+"/decision", map[string]any{"decision": "approve"})).Code, "peer cannot enter local administration")
	rec := fedHuman(t, fh.f, "GET", "/v1/federation/access-requests", nil)
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), "origin_peer")
	require.Contains(t, rec.Body.String(), "operator")
	rec = fedHuman(t, fh.f, "POST", "/v1/federation/access-requests/"+row.ID+"/decision", map[string]any{"decision": "always"})
	require.Equal(t, 400, rec.Code)
	rec = fedHuman(t, fh.f, "POST", "/v1/federation/access-requests/"+row.ID+"/decision", map[string]any{"decision": "approve", "group_id": group.ID + 1})
	require.Equal(t, 400, rec.Code, "cannot widen a scoped request")
	rec = fedHuman(t, fh.f, "POST", "/v1/federation/access-requests/"+row.ID+"/decision", map[string]any{"decision": "approve"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	fedEventually(t, "peer approval persisted", func() bool {
		r, e := db.GetFederationPeerAccessRequest(row.ID)
		return e == nil && r != nil && r.Status == "approved"
	})
	grants, err := db.ListEffectiveFederationPeerGrants(fh.peer.id.ID())
	require.NoError(t, err)
	require.Len(t, grants, 2)
	fedEventually(t, "peer grant expires", func() bool {
		g, e := db.ListEffectiveFederationPeerGrants(fh.peer.id.ID())
		return e == nil && len(g) == 1
	})
	// The ordinary permission store for local agents is not a beneficiary.
	audits, err := db.ListAuditLog(db.AuditLogFilter{Verb: "federation.access.approve", Limit: 10})
	require.NoError(t, err)
	require.NotEmpty(t, audits)
	row = request(map[string]any{"permission": agentd.PermMessageDirect})
	_, err = db.DeleteFederationPeerGrant(fh.peer.id.ID(), agentd.PermGroupsRosterRead, db.FederationGroupScope(group.ID))
	require.NoError(t, err)
	require.Equal(t, 200, fedHuman(t, fh.f, "POST", "/v1/federation/access-requests/"+row.ID+"/decision", map[string]any{"decision": "approve"}).Code)
	fedEventually(t, "revoked admission declines", func() bool {
		r, e := db.GetFederationPeerAccessRequest(row.ID)
		return e == nil && r.Status == "declined"
	})
	grants, err = db.ListEffectiveFederationPeerGrants(fh.peer.id.ID())
	require.NoError(t, err)
	require.Empty(t, grants)
}
func TestPeerAccessAwayCoverCannotApprove(t *testing.T) {
	for _, self := range []bool{false, true} {
		t.Run(fmt.Sprint(self), func(t *testing.T) {
			fh := newFedHarness(t)
			fh.f.HaveGroup("shared")
			grantFedAnswers(t, fh)
			require.Equal(t, 200, fedHuman(t, fh.f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermGroupsRosterRead, "scope": "group=shared"}).Code)
			setFedAway(t, fh, time.Time{})
			origin := fh.peer.id.ID()
			if !self {
				other, err := proto.NewIdentity()
				require.NoError(t, err)
				require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: other.ID(), PubKey: other.Pub, Label: "other"}))
				group, err := db.GetAgentGroupByName("shared")
				require.NoError(t, err)
				require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: other.ID(), Slug: agentd.PermGroupsRosterRead, Scope: db.FederationGroupScope(group.ID)}))
				origin = other.ID()
			}
			rec := testharness.Serve(agentd.PeerViewHandler(origin), testharness.JSONRequest(t, "POST", "/api/peer-access-requests", map[string]any{"permission": agentd.PermNodeUpdate}))
			require.Equal(t, 202, rec.Code, rec.Body.String())
			var row db.FederationPeerAccessRequest
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &row))
			// Obtain a live epoch through an ordinary AGENT request, which remains
			// delegated; the peer request must be refused even with that epoch.
			fh.f.HaveConvWithTitle("ordinary-away-requester", "ordinary requester")
			ordinaryID := strings.Repeat("a", 32)
			done, cleanup := agentd.StartFederationAwayApprovalForTest(ordinaryID, "ordinary-away-requester", 8*time.Second)
			t.Cleanup(cleanup)
			_, epoch := fedAwayTicket(t, fh.peer, ordinaryID)
			env := sendFedAwayDecision(t, fh.peer, row.ID, epoch, "approve")
			awaitFedAwayAck(t, fh.peer, env.ID, proto.AckRefused)
			r, err := db.GetFederationPeerAccessRequest(row.ID)
			require.NoError(t, err)
			require.Equal(t, "pending", r.Status)
			ordinaryDeny := sendFedAwayDecision(t, fh.peer, ordinaryID, epoch, "deny")
			awaitFedAwayAck(t, fh.peer, ordinaryDeny.ID, proto.AckAccepted)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("ordinary waiter did not resolve")
			}

			for _, notice := range fh.peer.envelopes(proto.KindAwayNotice) {
				var mail proto.MailPayload
				require.NoError(t, notice.DecodePayload(&mail))
				require.False(t, strings.Contains(mail.Body, row.ID), "peer request never forwarded to cover")
			}
			require.Equal(t, 200, fedHuman(t, fh.f, "POST", "/v1/federation/access-requests/"+row.ID+"/decision", map[string]any{"decision": "deny"}).Code)
			fedEventually(t, "local deny resolves", func() bool { r, _ := db.GetFederationPeerAccessRequest(row.ID); return r.Status == "declined" })
			grants, err := db.ListEffectiveFederationPeerGrants(origin)
			require.NoError(t, err)
			for _, g := range grants {
				require.NotEqual(t, agentd.PermNodeUpdate, g.Slug)
			}
		})
	}
}

func TestPeerAccessAgentAsksOwnOperatorOnlyForSharedTarget(t *testing.T) {
	fh := newFedHarness(t)
	aid := fedMoveSource(t, fh)
	count, reset := agentd.StubCountingApprovalForTest(false)
	t.Cleanup(reset)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:9999"))
	call := func() int {
		r := agentd.AsAgentPeer(testharness.JSONRequest(t, "POST", "/v1/federation/move-agent", map[string]any{"agent": aid, "peer": "bob", "group": "receiver"}), moveSourceConv)
		r.Header.Set("X-Tclaude-Ask-Human", "30s")
		rec := testharness.Serve(fh.f.Mux, r)
		return rec.Code
	}
	require.Equal(t, 403, call())
	require.Zero(t, count(), "public placement metadata cannot prompt own operator for nonexistent peer access")
	fh.peer.send(fh.peer.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{AgentMoves: true, Groups: []proto.CatalogGroup{{Name: "receiver", Caps: []string{proto.CapRoster}}}}))
	fedEventually(t, "shared catalog group", func() bool {
		raw, _, _ := db.GetFederationCatalog(fh.peer.id.ID())
		var c proto.CatalogPayload
		return json.Unmarshal([]byte(raw), &c) == nil && len(c.Groups) == 1
	})
	require.Equal(t, 403, call())
	require.EqualValues(t, 1, count(), "missing local permission prompts own operator after peer access exists")
}

func TestPeerAccessModelOnlyAdmission(t *testing.T) {
	fh := newFedHarness(t)
	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: fh.peer.id.ID(), Slug: agentd.PermModelsProxy, Scope: "http_proxy=model"}))
	rec := testharness.Serve(agentd.PeerViewHandler(fh.peer.id.ID()), testharness.JSONRequest(t, "POST", "/api/peer-access-requests", map[string]any{"permission": agentd.PermNodeUpdate}))
	require.Equal(t, 202, rec.Code, rec.Body.String())
	var row db.FederationPeerAccessRequest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &row))
	require.Equal(t, 200, fedHuman(t, fh.f, "POST", "/v1/federation/access-requests/"+row.ID+"/decision", map[string]any{"decision": "approve"}).Code)
	fedEventually(t, "model-only access approval", func() bool {
		r, e := db.GetFederationPeerAccessRequest(row.ID)
		return e == nil && r != nil && r.Status == "approved"
	})
}
