package agentd_test

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func fedDirectMoveDescriptor(t *testing.T, fh *fedHarness, alter ...func(*bundletransfer.MoveIntent)) bundletransfer.Descriptor {
	t.Helper()
	b := fedAgentBundle(t)
	b.Manifest.Agent.Profile = json.RawMessage(`{}`)
	transcript := append(mustJSON(t, map[string]any{"type": "user", "sessionId": moveSourceConv, "cwd": "/source", "message": map[string]string{"content": "Continue the focused test."}}), '\n')
	b.SetHistory("claude-jsonl", moveSourceConv, transcript)
	raw, err := b.Encode()
	require.NoError(t, err)
	d := bundletransfer.New(bundletransfer.Agent, raw, "Direct move", time.Now().Add(time.Hour))
	d.Group = "receiver"
	d.Move = &bundletransfer.MoveIntent{SourceAgent: "agt_bobremote0000000000000000", SourceConv: moveSourceConv, DirectIfAllowed: true}
	for _, fn := range alter {
		fn(d.Move)
	}
	return d
}

func fedIncomingDirectMove(t *testing.T, fh *fedHarness, alter ...func(*bundletransfer.MoveIntent)) bundletransfer.Descriptor {
	t.Helper()
	d := fedDirectMoveDescriptor(t, fh, alter...)
	env := fh.peer.envelope(proto.KindBundleOffer, proto.Endpoint{}, d)
	env.ID = d.ID
	fh.peer.send(env)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, d.ID).Status)
	return d
}

func TestFederation_DirectMoveReceiverDecidesAndReportsDirectory(t *testing.T) {
	for _, mode := range []string{"unrestricted", "receive_policy", "receive_only", "unresolved", "restricted_override", "revoked_at_dispatch"} {
		t.Run(mode, func(t *testing.T) {
			fh := newFedHarness(t)
			fh.f.HaveGroup("receiver")
			t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
			dashboard := agentd.BuildDashboardHandlerForTest()
			cwd := testutil.CanonicalTempDir(t)
			if mode != "unresolved" {
				_, err := db.SetAgentGroupDefaultCwd("receiver", cwd)
				require.NoError(t, err)
			}
			if mode == "unrestricted" || mode == "unresolved" {
				setFedTrustLevel(t, fh, "unrestricted")
			} else {
				fedReceiveAgents(t, fh, "receiver")
			}
			if mode == "receive_policy" || mode == "restricted_override" || mode == "revoked_at_dispatch" {
				_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "landing", Harness: "claude", Model: "sonnet", Approval: "default"})
				require.NoError(t, err)
				p := fedNodeProfile(t, fh, "direct-node", db.FederationNodeProfileSpec{PeerGrants: []db.FederationPeerGrant{{Slug: agentd.PermAgentsReceive, Scope: "group=receiver"}, {Slug: agentd.PermAgentsTeleportReceive, Scope: "group=receiver"}}, TeleportLanding: &db.FederationTeleportLanding{Group: "receiver", Cwd: cwd, SpawnProfile: "landing", MaxLive: 1}})
				fedApplyNodeProfile(t, fh, p)
			}
			previous := agentd.Spawn
			var births atomic.Int32
			agentd.Spawn = &fedTeleportBirthSpawner{inner: previous, check: func(args clcommon.SpawnArgs) {
				births.Add(1)
				require.Equal(t, cwd, args.Cwd)
				if mode == "receive_policy" || mode == "restricted_override" || mode == "revoked_at_dispatch" {
					require.Equal(t, "sonnet", args.Model)
				}
			}}
			t.Cleanup(func() { agentd.Spawn = previous })
			if mode == "revoked_at_dispatch" {
				database, err := db.Open()
				require.NoError(t, err)
				_, err = database.Exec(`CREATE TRIGGER revoke_direct_at_dispatch AFTER UPDATE OF import_label ON federation_bundle_offers WHEN NEW.import_label <> '' BEGIN DELETE FROM federation_peer_grants WHERE slug='agents.receive'; END`)
				require.NoError(t, err)
			}
			d := fedIncomingDirectMove(t, fh, func(m *bundletransfer.MoveIntent) {
				if mode == "restricted_override" {
					m.Cwd = testutil.CanonicalTempDir(t)
				}
			})
			if mode == "receive_only" || mode == "unresolved" || mode == "revoked_at_dispatch" {
				fedEventually(t, "pending receiver acceptance", func() bool {
					m, _ := db.GetFederationAgentMove("in", fh.peer.id.ID(), d.ID)
					return m != nil && m.Disposition == "pending_acceptance"
				})
				require.Zero(t, births.Load())
				rec := testharness.Serve(dashboard, testharness.JSONRequest(t, http.MethodGet, "/api/federation/moves/"+d.ID, nil))
				require.Equal(t, 200, rec.Code, rec.Body.String())
				require.Contains(t, rec.Body.String(), `"disposition":"pending_acceptance"`)
				if mode == "receive_only" {
					refused := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/bundle-offers/"+d.ID+"/decline", nil)
					require.Equal(t, 200, refused.Code, refused.Body.String())
					fedEventually(t, "declined incoming move is terminal", func() bool {
						m, _ := db.GetFederationAgentMove("in", fh.peer.id.ID(), d.ID)
						return m != nil && m.State == "declined"
					})
				}
				if mode == "unresolved" {
					m, err := db.GetFederationAgentMove("in", fh.peer.id.ID(), d.ID)
					require.NoError(t, err)
					m.ExpiresAt = time.Now().Add(-time.Second)
					_, err = db.TransitionFederationAgentMove(*m, m.State)
					require.NoError(t, err)
					fedEventually(t, "expired incoming move is terminal", func() bool {
						m, _ := db.GetFederationAgentMove("in", fh.peer.id.ID(), d.ID)
						return m != nil && m.State == "expired"
					})
				}

				return
			}
			fedEventually(t, "direct target running", func() bool {
				m, _ := db.GetFederationAgentMove("in", fh.peer.id.ID(), d.ID)
				return m != nil && m.State == "running"
			})
			require.EqualValues(t, 1, births.Load())
			rec := testharness.Serve(dashboard, testharness.JSONRequest(t, http.MethodGet, "/api/federation/moves/"+d.ID, nil))
			require.Equal(t, 200, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), `"disposition":"landed"`)
			require.Contains(t, rec.Body.String(), cwd)
			fedEventually(t, "running confirmation with cwd", func() bool {
				for _, env := range fh.peer.envelopes(proto.KindAgentMoveConfirm) {
					var c bundletransfer.MoveConfirmation
					if env.DecodePayload(&c) == nil && c.Offer == d.ID {
						return c.Cwd == cwd
					}
				}
				return false
			})
			if mode == "receive_policy" {
				first, err := db.GetFederationBundleOffer("in", fh.peer.id.ID(), d.ID)
				require.NoError(t, err)
				a, err := db.GetAgent(first.ImportAgent)
				require.NoError(t, err)
				reincarnated := fh.f.AsHuman().Reincarnate(a.CurrentConvID, "Continue in a fresh context")
				require.NotEmpty(t, reincarnated.NewConv)
				teleport := fedIncomingTeleport(t, fh, "local", nil)
				require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, teleport.ID).Status)
				fedEventually(t, "direct worker also occupies teleport policy slot", func() bool {
					row, _ := db.GetFederationTeleport("in", fh.peer.id.ID(), teleport.ID)
					return row != nil && row.State == "pending" && row.TargetAgent == ""
				})
				second := fedIncomingDirectMove(t, fh)
				fedEventually(t, "live worker occupies policy slot", func() bool {
					m, _ := db.GetFederationAgentMove("in", fh.peer.id.ID(), second.ID)
					return m != nil && m.Disposition == "pending_acceptance"
				})
				require.EqualValues(t, 2, births.Load())
				ses, err := db.FindSessionByConvID(reincarnated.NewConv)
				require.NoError(t, err)
				require.NotNil(t, ses)
				fh.f.World.Tmux.KillBySignalForTest(ses.TmuxSession)
				third := fedIncomingDirectMove(t, fh)
				fedEventually(t, "stopped worker releases policy slot", func() bool {
					m, _ := db.GetFederationAgentMove("in", fh.peer.id.ID(), third.ID)
					return m != nil && m.State == "running"
				})
				require.EqualValues(t, 3, births.Load())
			}

		})
	}
}

func TestFederation_DirectMoveSourceDispositionAndConfirmedRetirement(t *testing.T) {
	fh := newFedHarness(t)
	aid := fedMoveSource(t, fh)
	legacy := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/move-agent", map[string]any{"agent": moveSourceConv, "peer": "bob", "group": "receiver", "direct_if_allowed": true})
	require.Equal(t, 200, legacy.Code, legacy.Body.String())
	require.Contains(t, legacy.Body.String(), `"disposition":"pending_acceptance"`)
	require.NotContains(t, legacy.Body.String(), `"direct_if_allowed":true`, "older receivers retain the manual offer flow")
	var oldMove struct {
		MoveID string `json:"move_id"`
	}
	require.NoError(t, json.Unmarshal(legacy.Body.Bytes(), &oldMove))
	abandoned := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/moves/"+oldMove.MoveID+"/abandon", nil)
	require.Equal(t, 200, abandoned.Code, abandoned.Body.String())
	require.NoError(t, db.PutFederationCatalog(fh.peer.id.ID(), string(mustJSON(t, proto.CatalogPayload{AgentMoves: true, DirectAgentMoves: true, Groups: []proto.CatalogGroup{}})), time.Now()))
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/move-agent", map[string]any{"agent": moveSourceConv, "peer": "bob", "group": "receiver", "direct_if_allowed": true})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var response struct {
		MoveID      string `json:"move_id"`
		Disposition string `json:"disposition"`
		Offer       struct {
			D bundletransfer.Descriptor `json:"offer"`
		} `json:"offer"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	require.Equal(t, "checking", response.Disposition)
	require.Equal(t, response.Offer.D.ID, response.MoveID)
	require.True(t, response.Offer.D.Move.DirectIfAllowed)
	pending := fh.peer.envelope(proto.KindBundleResult, proto.Endpoint{}, bundletransfer.Result{Offer: response.MoveID, State: "pending", Disposition: "pending_acceptance"})
	pending.From.Agent = ""
	fh.peer.send(pending)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, pending.ID).Status)
	m, err := db.GetFederationAgentMove("out", fh.peer.id.ID(), response.MoveID)
	require.NoError(t, err)
	require.Equal(t, "pending_acceptance", m.Disposition)
	a, err := db.GetAgent(aid)
	require.NoError(t, err)
	require.True(t, a.Active())
	env := fh.peer.envelope(proto.KindAgentMoveConfirm, proto.Endpoint{}, bundletransfer.MoveConfirmation{ObservedAt: time.Now().UTC(), Offer: response.MoveID, SHA256: response.Offer.D.SHA256, SourceAgent: aid, SourceConv: moveSourceConv, TargetAgent: "agt_bobremote0000000000000000", TargetConv: "019fe740-43a4-7023-b8ae-1ee64459f2a2", Cwd: "/receiver/project"})
	env.From.Agent = ""
	fh.peer.send(env)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, env.ID).Status)
	fedEventually(t, "source retired only after running confirmation", func() bool {
		m, _ = db.GetFederationAgentMove("out", fh.peer.id.ID(), response.MoveID)
		return m != nil && m.State == "moved"
	})
	require.Equal(t, "landed", m.Disposition)
	require.Equal(t, "/receiver/project", m.Cwd)
}

func TestFederation_DirectMoveFetchRefusalSettlesPending(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("receiver")
	setFedTrustLevel(t, fh, "unrestricted")
	d := fedDirectMoveDescriptor(t, fh)
	d.Inline = nil
	env := fh.peer.envelope(proto.KindBundleOffer, proto.Endpoint{}, d)
	env.ID = d.ID
	fh.peer.send(env)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, d.ID).Status)
	fedEventually(t, "automatic history fetch", func() bool { return len(fh.peer.envelopes(proto.KindBundleFetch)) > 0 })
	var request bundletransfer.Request
	require.NoError(t, fh.peer.envelopes(proto.KindBundleFetch)[0].DecodePayload(&request))
	answer := fh.peer.envelope(proto.KindBundleAnswer, proto.Endpoint{}, bundletransfer.Answer{Request: request, OK: false, Reason: "sender cancelled transfer"})
	answer.From.Agent = ""
	fh.peer.send(answer)
	fedEventually(t, "refused fetch leaves actionable pending move", func() bool {
		m, _ := db.GetFederationAgentMove("in", fh.peer.id.ID(), d.ID)
		return m != nil && m.Disposition == "pending_acceptance"
	})
	offer, err := db.GetFederationBundleOffer("in", fh.peer.id.ID(), d.ID)
	require.NoError(t, err)
	require.Equal(t, "pending", offer.State, "a failed fetch does not fabricate a ready archive")
	require.Contains(t, offer.LastError, "sender cancelled transfer")
	fedEventually(t, "pending disposition reaches source", func() bool {
		for _, e := range fh.peer.envelopes(proto.KindBundleResult) {
			var result bundletransfer.Result
			if e.DecodePayload(&result) == nil && result.Offer == d.ID && result.Disposition == "pending_acceptance" {
				return true
			}
		}
		return false
	})
}

func TestFederation_DirectMovePriorFetchErrorDoesNotStickChecking(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("receiver")
	setFedTrustLevel(t, fh, "unrestricted")
	database, err := db.Open()
	require.NoError(t, err)
	_, err = database.Exec(`CREATE TRIGGER prior_fetch_error AFTER INSERT ON federation_bundle_offers WHEN NEW.direction='in' BEGIN UPDATE federation_bundle_offers SET last_error='earlier fetch refused' WHERE direction=NEW.direction AND peer=NEW.peer AND id=NEW.id; END`)
	require.NoError(t, err)
	d := fedDirectMoveDescriptor(t, fh)
	d.Inline = nil
	env := fh.peer.envelope(proto.KindBundleOffer, proto.Endpoint{}, d)
	env.ID = d.ID
	fh.peer.send(env)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, d.ID).Status)
	fedEventually(t, "prior error settles actionable pending move", func() bool {
		m, _ := db.GetFederationAgentMove("in", fh.peer.id.ID(), d.ID)
		return m != nil && m.Disposition == "pending_acceptance"
	})
	require.Empty(t, fh.peer.envelopes(proto.KindBundleFetch), "a prior failed transfer does not loop automatically")
}
