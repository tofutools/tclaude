package agentd_test

import (
	"encoding/json"
	"net/http"
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

func fedIncomingDirectMove(t *testing.T, fh *fedHarness) bundletransfer.Descriptor {
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
	env := fh.peer.envelope(proto.KindBundleOffer, proto.Endpoint{}, d)
	env.ID = d.ID
	fh.peer.send(env)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, d.ID).Status)
	return d
}

func TestFederation_DirectMoveReceiverDecidesAndReportsDirectory(t *testing.T) {
	for _, mode := range []string{"unrestricted", "receive_policy", "receive_only", "unresolved"} {
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
			if mode == "receive_policy" {
				_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "landing", Harness: "claude", Model: "sonnet", Approval: "default"})
				require.NoError(t, err)
				p := fedNodeProfile(t, fh, "direct-node", db.FederationNodeProfileSpec{PeerGrants: []db.FederationPeerGrant{{Slug: agentd.PermAgentsReceive, Scope: "group=receiver"}}, TeleportLanding: &db.FederationTeleportLanding{Group: "receiver", Cwd: cwd, SpawnProfile: "landing", MaxLive: 1}})
				fedApplyNodeProfile(t, fh, p)
			}
			previous := agentd.Spawn
			births := 0
			agentd.Spawn = &fedTeleportBirthSpawner{inner: previous, check: func(args clcommon.SpawnArgs) {
				births++
				require.Equal(t, cwd, args.Cwd)
				if mode == "receive_policy" {
					require.Equal(t, "sonnet", args.Model)
				}
			}}
			t.Cleanup(func() { agentd.Spawn = previous })
			d := fedIncomingDirectMove(t, fh)
			if mode == "receive_only" || mode == "unresolved" {
				fedEventually(t, "pending receiver acceptance", func() bool {
					m, _ := db.GetFederationAgentMove("in", fh.peer.id.ID(), d.ID)
					return m != nil && m.Disposition == "pending_acceptance"
				})
				require.Zero(t, births)
				rec := testharness.Serve(dashboard, testharness.JSONRequest(t, http.MethodGet, "/api/federation/moves/"+d.ID, nil))
				require.Equal(t, 200, rec.Code, rec.Body.String())
				require.Contains(t, rec.Body.String(), `"disposition":"pending_acceptance"`)
				return
			}
			fedEventually(t, "direct target running", func() bool {
				m, _ := db.GetFederationAgentMove("in", fh.peer.id.ID(), d.ID)
				return m != nil && m.State == "running"
			})
			require.Equal(t, 1, births)
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
		})
	}
}
