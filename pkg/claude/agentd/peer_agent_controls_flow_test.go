package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Pins: a restricted peer's grants reach none of an agent's compaction or
// reincarnation, while unrestricted trust runs them through this node's own
// handlers, every option included; a peer-invoked rename runs through the
// dashboard twin under the peer's grant.
func TestPeerAgentControlsPins(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	f.HaveGroup("crew")
	f.HaveConvWithTitle("ctl-keep", "keep")
	f.HaveAliveSession("ctl-keep", "spwn-ctl-keep", "tmux-ctl-keep", f.World.HomeDir)
	f.HaveMember("crew", "ctl-keep")
	keep, err := db.AgentIDForConv("ctl-keep")
	require.NoError(t, err)
	for _, slug := range []string{agentd.PermGroupsRosterRead, agentd.PermGroupsMembersManage} {
		rec := fedHuman(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug, "scope": "group=crew"})
		require.Equal(t, 200, rec.Code, rec.Body.String())
	}
	h := agentd.PeerViewHandler(p.id.ID())
	call := func(method, path string, body any) (int, string) {
		t.Helper()
		rec := testharness.Serve(h, testharness.JSONRequest(t, method, path, body))
		return rec.Code, rec.Body.String()
	}
	for _, verb := range []string{"compact", "reincarnate"} {
		code, body := call("POST", "/api/agents/"+keep+"/"+verb, nil)
		require.Equal(t, 403, code, "%s: %s", verb, body)
	}
	code, body := call("POST", "/api/agents/"+keep+"/rename", map[string]any{"title": "kept"})
	require.Equal(t, 200, code, body)
	require.Contains(t, body, "kept")

	peer, err := db.GetFederationPeer(p.id.ID())
	require.NoError(t, err)
	peer.TrustLevel = db.FederationTrustUnrestricted
	require.NoError(t, db.TrustFederationPeer(*peer))
	code, body = call("POST", "/api/agents/"+keep+"/reincarnate", map[string]any{"mode": "self", "focus_hint": "the parser"})
	require.Equal(t, 200, code, body)
	var out struct {
		Mode   string `json:"mode"`
		Queued bool   `json:"queued"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	require.Equal(t, "self", out.Mode)
	require.True(t, out.Queued)
	code, body = call("POST", "/api/agents/"+keep+"/compact", nil)
	require.NotEqual(t, 403, code, "unrestricted trust reaches compaction: %s", body)
}
