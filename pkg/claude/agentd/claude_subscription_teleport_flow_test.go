package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// A teleport onto the sender operator's own Claude subscription: the offer
// carries only the mode, the token arrives separately through the peer
// view, and the landed agent runs on it. Without the token the agent lands
// offline and marked, never on this node's own login, and the token sent
// later goes to that agent.
func TestTeleportOnTheSendersClaudeSubscription(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("receiver")
	fedReceiveAgents(t, fh, "receiver")
	setFedTrustLevel(t, fh, db.FederationTrustUnrestricted)
	spawns := recordSpawns(t)
	h := agentd.PeerViewHandler(fh.peer.id.ID())
	send := func(offer string) (int, string) {
		rec := testharness.Serve(h, testharness.JSONRequest(t, http.MethodPost, "/api/teleport-subscription", map[string]any{"offer": offer, "claude_subscription_token": fakeSubscriptionToken}))
		return rec.Code, rec.Body.String()
	}
	land := func(offer string) map[string]any {
		t.Helper()
		rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/bundle-offers/"+offer+"/import", map[string]any{"cwd": testutil.CanonicalTempDir(t), "apply": true})
		require.Equal(t, 200, rec.Code, rec.Body.String())
		require.NotContains(t, rec.Body.String(), fakeSubscriptionToken)
		var out map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		return out
	}

	d := fedIncomingTeleport(t, fh, "claude-subscription", nil)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, d.ID).Status)
	code, body := send(d.ID)
	require.Equal(t, 200, code, body)
	require.Contains(t, body, `"held":"offer"`)
	before := len(spawns())
	land(d.ID)
	launched := spawns()
	require.Greater(t, len(launched), before, "the agent starts")
	last := launched[len(launched)-1]
	require.True(t, last.ClaudeSubscription, "on the sender's subscription")
	marker, err := db.GetAgentClaudeSubscription(last.ClaudeSubscriptionAgentID)
	require.NoError(t, err)
	require.NotNil(t, marker)
	require.Equal(t, fh.peer.id.ID(), marker.Peer)

	// Without the token it lands offline, marked, and the token sent later
	// goes to that agent.
	d2 := fedIncomingTeleport(t, fh, "claude-subscription", nil)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, d2.ID).Status)
	before = len(spawns())
	land(d2.ID)
	require.Equal(t, before, len(spawns()), "nothing starts on this node's own login")
	row, err := db.GetFederationTeleport("in", fh.peer.id.ID(), d2.ID)
	require.NoError(t, err)
	require.NotEmpty(t, row.TargetAgent)
	marker, err = db.GetAgentClaudeSubscription(row.TargetAgent)
	require.NoError(t, err)
	require.NotNil(t, marker, "marked for its owner to send the token")
	code, body = send(d2.ID)
	require.Equal(t, 200, code, body)
	require.Contains(t, body, `"held":"agent"`)

	// A local teleport's offer takes no token.
	d3 := fedIncomingTeleport(t, fh, "local", nil)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, d3.ID).Status)
	code, _ = send(d3.ID)
	require.Equal(t, 409, code)
}
