package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestDashboardOperatorTeleportKeepsHumanAuthority(t *testing.T) {
	for _, selector := range []string{"agent", "conversation"} {
		t.Run(selector, func(t *testing.T) {
			fh := newFedHarness(t)
			aid := fedTeleportSource(t, fh)
			t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
			require.NoError(t, db.SetAgentPermissionOverride(moveSourceConv, agentd.PermSelfTeleport, db.PermEffectDeny, "test"))
			require.NoError(t, db.SetAgentPermissionOverride(moveSourceConv, agentd.PermSelfTeleportPermissions, db.PermEffectDeny, "test"))
			id := aid
			if selector == "conversation" {
				id = moveSourceConv
			}
			body := map[string]any{"peer": "bob", "group": "receiver", "carry_permissions": true}
			denied := fedSelfTeleport(t, fh, body)
			require.Equal(t, http.StatusForbidden, denied.Code, denied.Body.String())
			// The new route does not let an agent borrow the operator's actor.
			denied = testharness.Serve(fh.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, "POST", "/v1/agents/"+id+"/teleport", body), moveSourceConv))
			require.Equal(t, http.StatusForbidden, denied.Code, denied.Body.String())
			h := agentd.BuildDashboardHandlerForTest()
			rec := testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/agents/"+id+"/teleport", body))
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var result struct {
				MoveID       string `json:"move_id"`
				SourceActive bool   `json:"source_active"`
				Offer        struct {
					D bundletransfer.Descriptor `json:"offer"`
				} `json:"offer"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
			require.NotEmpty(t, result.MoveID)
			require.True(t, result.SourceActive)
			m, err := db.GetFederationAgentMove("out", fh.peer.id.ID(), result.MoveID)
			require.NoError(t, err)
			require.True(t, m.Human)
			require.Empty(t, m.Initiator)
			require.Equal(t, aid, m.SourceAgent)
			a, err := db.GetAgent(aid)
			require.NoError(t, err)
			require.True(t, a.Active())
			fedMoveConfirm(t, fh, result.Offer.D, result.Offer.D.SHA256)
			fedEventually(t, "operator teleport retires only after confirmation", func() bool {
				m, _ := db.GetFederationAgentMove("out", fh.peer.id.ID(), result.MoveID)
				return m != nil && m.State == "moved"
			})
		})
	}
}

func TestPeerRecallHomeRequiresRecordedOrigin(t *testing.T) {
	for _, recordedHome := range []string{"requester", "other"} {
		t.Run(recordedHome, func(t *testing.T) {
			fh := newFedHarness(t)
			home := fh.peer.id.ID()
			if recordedHome == "other" {
				home = "inst_other-home"
			}
			_, err := config.Update(func(c *config.Config, e error) error {
				if e != nil {
					return e
				}
				if c.Federation == nil {
					c.Federation = &config.FederationConfig{}
				}
				c.Federation.Teleport = &config.FederationTeleportConfig{Limits: config.TeleportLimits{AllowReturn: true}}
				return nil
			})
			require.NoError(t, err)
			identity := db.FederationIdentity{Agent: db.NewAgentID(), Home: home, Hops: 1, Proofs: map[string]string{home: "proof"}}
			_, err = db.ReserveFederationIdentity(identity, fh.peer.agentdID, home, "arrival")
			require.NoError(t, err)
			_, _, err = db.EnsureAgentForConvWithID(moveSourceConv, identity.Agent, "visit")
			require.NoError(t, err)
			aid := fedTeleportSource(t, fh)
			require.Equal(t, identity.Agent, aid)
			for _, slug := range []string{agentd.PermAgentMove, agentd.PermGroupsMembersRetire} {
				rec := fedHuman(t, fh.f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug, "scope": "group=source"})
				require.Equal(t, 200, rec.Code, rec.Body.String())
			}
			// A recent hop from home normally triggers the revisit guard.
			intent := bundletransfer.TeleportIntent{Version: 1, Chain: "chain", OriginInstance: home, OriginAgent: aid, SourceAgent: aid,
				Hops: []bundletransfer.TeleportHop{{FromInstance: home, ToInstance: fh.peer.agentdID, At: time.Now()}}}
			require.NoError(t, db.RecordFederationTeleport(db.FederationTeleport{Direction: "in", Peer: home, Offer: "arrival", State: "running", TargetAgent: aid, Intent: intent}, (config.TeleportLimits{AllowReturn: true}).Effective()))
			rec := testharness.Serve(agentd.PeerViewHandler(fh.peer.id.ID()), testharness.JSONRequest(t, "POST", "/api/agents/"+aid+"/teleport", map[string]any{"group": "receiver", "home": true, "note": "return"}))
			if recordedHome == "other" {
				require.Equal(t, 403, rec.Code, rec.Body.String())
				require.Contains(t, rec.Body.String(), "not_home")
				return
			}
			require.Equal(t, 200, rec.Code, rec.Body.String())
			var result struct {
				MoveID string `json:"move_id"`
				Offer  struct {
					D bundletransfer.Descriptor `json:"offer"`
				} `json:"offer"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
			require.NotEmpty(t, result.MoveID)
			require.True(t, result.Offer.D.Teleport.Home)
			require.Equal(t, home, result.Offer.D.Teleport.OriginInstance)
			require.Equal(t, aid, result.Offer.D.Move.SourceAgent)
		})
	}
}

func TestOperatorRecallPausedBackupUsesExistingLease(t *testing.T) {
	for _, actor := range []string{"local", "home"} {
		t.Run(actor, func(t *testing.T) {
			fh := newFedHarness(t)
			fedBackupPolicy(t, "auto")
			identity := db.FederationIdentity{Agent: "agt_bobremote0000000000000000", Home: fh.peer.id.ID(), Hops: 1, Proofs: map[string]string{fh.peer.id.ID(): "home-proof"}}
			d, a := fedLandedBackupIdentity(t, fh, &identity)
			body := map[string]any{"home": true, "group": "receiver", "note": "Work completed; return to the original backup."}
			var rec *httptest.ResponseRecorder
			if actor == "local" {
				rec = fedHuman(t, fh.f, "POST", "/v1/agents/"+a.AgentID+"/teleport", body)
			} else {
				denied := testharness.Serve(agentd.PeerViewHandler(fh.peer.id.ID()), testharness.JSONRequest(t, "POST", "/api/agents/"+a.AgentID+"/teleport", body))
				require.Equal(t, 403, denied.Code, denied.Body.String())
				for _, slug := range []string{agentd.PermAgentMove, agentd.PermGroupsMembersRetire} {
					grant := fedHuman(t, fh.f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug, "scope": "group=receiver"})
					require.Equal(t, 200, grant.Code, grant.Body.String())
				}
				rec = testharness.Serve(agentd.PeerViewHandler(fh.peer.id.ID()), testharness.JSONRequest(t, "POST", "/api/agents/"+a.AgentID+"/teleport", body))
			}
			require.Equal(t, 202, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), "return_id")
			fedEventually(t, "operator recall uses durable backup return", func() bool {
				l, _ := db.GetFederationTeleportLease("in", fh.peer.id.ID(), d.ID)
				return l != nil && l.State == "stopped" && l.Findings == body["note"]
			})
			p, err := db.GetAgentFederationPresence(a.AgentID)
			require.NoError(t, err)
			require.Equal(t, "away", p.State)
		})
	}
}
