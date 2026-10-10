package agentd_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// All federation arrival modes use the receiver's spawn enrollment. Its
// snapshot must refer to local policy and the local briefing, never a source
// node's group identity or immutable startup snapshot.
func TestFederationImportedStartupContextAfterCompaction(t *testing.T) {
	for _, mode := range []string{"clone", "move", "teleport", "teleport_home"} {
		t.Run(mode, func(t *testing.T) {
			fh := newFedHarness(t)
			fh.f.HaveGroup("receiver")
			fedReceiveAgents(t, fh, "receiver")
			_, err := db.SetAgentGroupDefaultContext("receiver", "Target operator startup guidance.")
			require.NoError(t, err)
			var d bundletransfer.Descriptor
			if mode == "teleport" || mode == "teleport_home" {
				d = fedIncomingTeleport(t, fh, "local", func(in *bundletransfer.TeleportIntent) {
					if mode == "teleport_home" {
						in.Home = true
						in.OriginInstance = fh.peer.agentdID
						in.Clone = false
						last := in.Hops[0]
						first := bundletransfer.TeleportHop{Offer: proto.NewEnvelopeID(), FromInstance: fh.peer.agentdID, FromAgent: in.OriginAgent, ToInstance: fh.peer.id.ID(), ToGroup: "source", At: time.Now().Add(-24 * time.Hour)}
						in.Hops = []bundletransfer.TeleportHop{first, last}
					}
				})
			} else {
				b := fedAgentBundle(t)
				b.Manifest.Agent.StartupContext = "Origin-only startup text."
				b.Manifest.Agent.InitialMessage = "Origin task brief, archived but not re-pasted."
				b.SetHistory("claude-jsonl", moveSourceConv, []byte(`{"type":"user","sessionId":"`+moveSourceConv+`","cwd":"/source","message":{"content":"source history"}}`+"\n"))
				raw, err := b.Encode()
				require.NoError(t, err)
				d = bundletransfer.New(bundletransfer.Agent, raw, "Imported worker", time.Now().Add(time.Hour))
				d.Group = "receiver"
				if mode == "move" {
					d.Move = &bundletransfer.MoveIntent{SourceAgent: "agt_bobremote0000000000000000", SourceConv: moveSourceConv}
				}
				env := fh.peer.envelope(proto.KindBundleOffer, proto.Endpoint{}, d)
				env.ID = d.ID
				fh.peer.send(env)
			}
			ack := fedAckFor(t, fh.peer, d.ID)
			require.Equal(t, proto.AckAccepted, ack.Status, ack.Reason)
			rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/bundle-offers/"+d.ID+"/import", map[string]any{"cwd": testutil.CanonicalTempDir(t), "name": "arriving-worker", "apply": true})
			require.Equal(t, 200, rec.Code, rec.Body.String())
			var result struct {
				Spawn struct {
					ConvID  string `json:"conv_id"`
					AgentID string `json:"agent_id"`
				} `json:"spawn"`
			}
			testharness.DecodeJSON(t, rec, &result)
			actor, err := db.GetAgentByConv(result.Spawn.ConvID)
			require.NoError(t, err)
			require.NotNil(t, actor)
			brief := arrivalInbox(t, result.Spawn.ConvID)
			require.Contains(t, brief, "Arrival briefing")
			require.Contains(t, brief, actor.AgentID)
			require.Contains(t, brief, fh.peer.id.ID())
			require.Contains(t, brief, "source permissions not copied")
			if mode == "teleport_home" {
				require.Contains(t, brief, "Arrival briefing — teleport home")
			}

			snapshot, err := db.GetAgentStartupSnapshot(actor.AgentID)
			require.NoError(t, err)
			require.NotNil(t, snapshot, "receiving spawn must record its startup snapshot")
			group, err := db.GetAgentGroupByName("receiver")
			require.NoError(t, err)
			require.Equal(t, group.ID, snapshot.SpawnGroupID)
			require.True(t, snapshot.IncludeGroupContext)
			require.Empty(t, snapshot.ProfileContext, "source profile text is not receiver policy")
			require.Positive(t, snapshot.BriefMessageID)
			// Re-injection must follow the receiving operator's current group policy.
			_, err = db.SetAgentGroupDefaultContext("receiver", "Updated target operator guidance after arrival.")
			require.NoError(t, err)
			require.Equal(t, 200, fh.f.Compact(result.Spawn.ConvID).Code)
			var messages []*db.AgentMessage
			require.Eventually(t, func() bool {
				messages = reinjectedInbox(t, result.Spawn.ConvID, actor.AgentID)
				return len(messages) == 1
			}, 10*time.Second, 20*time.Millisecond)
			body := messages[0].Body
			require.Contains(t, body, "Your context was compacted")
			require.Contains(t, body, `in group "receiver"`)
			require.Contains(t, body, "Updated target operator guidance after arrival.")
			require.Contains(t, body, fmt.Sprintf("tclaude agent inbox read %d", snapshot.BriefMessageID))
			require.NotContains(t, body, "Origin-only startup text.")
			require.NotContains(t, body, "Origin task brief")
		})
	}
}
