package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// The Remote Access session link read from an agent's /rc pill reaches the
// operator's own dashboard snapshot ("Open in Claude app ↗"). The link
// controls the session, so a federated peer gets it only for agents it may
// attach to interactively: status or watch-only grants show no link.
func TestRemoteControlSessionURL_LocalSnapshotAndPeerAttachGate(t *testing.T) {
	fh := newFedHarness(t)
	f := fh.f
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f.HaveGroup("crew")
	const conv = "019ec0b0-1111-2222-3333-444444444444"
	f.HaveAliveSession(conv, "cc-url", "tmux-cc-url", f.TestCwd("url"))
	f.HaveMember("crew", conv)

	local := func() string {
		t.Helper()
		rec := testharness.Serve(agentd.BuildDashboardHandlerForTest(), testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil))
		require.Equal(t, http.StatusOK, rec.Code)
		return rec.Body.String()
	}
	require.NotContains(t, local(), "remote_control_url", "no link before Remote Access is on")

	// Remote Access is armed in the pane, and the dashboard reads it once.
	cc := f.World.CCs.GetByConvID(conv)
	require.NotNil(t, cc)
	cc.Receive("/remote-control")
	cc.Receive("Enter")
	st := f.AsHuman().RemoteControl(conv, "status")
	require.Equal(t, http.StatusOK, st.Code, st.Raw)
	require.Contains(t, st.SessionURL, "https://claude.ai/code/")
	link := `"remote_control_url":"` + st.SessionURL + `"`
	require.Contains(t, local(), link, "the operator's snapshot carries the link it read")

	peer := agentd.PeerViewHandler(fh.peer.id.ID())
	peerSnapshot := func() string {
		t.Helper()
		rec := testharness.Serve(peer, testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		return rec.Body.String()
	}
	grant := func(slug string) {
		t.Helper()
		rec := fedHuman(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug, "scope": "group=crew"})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	grant(agentd.PermAgentsStatusRead)
	body := peerSnapshot()
	require.Contains(t, body, conv, "the peer sees the agent's status")
	require.NotContains(t, body, "remote_control_url", "status alone shows no link")
	grant(agentd.PermSessionsWatch)
	require.NotContains(t, peerSnapshot(), "remote_control_url", "watch-only shows no link")
	grant(agentd.PermSessionsAttach)
	require.Contains(t, peerSnapshot(), link, "a peer that may attach gets the link")

	// Switching Remote Access drops the link until the next read.
	off := f.AsHuman().RemoteControl(conv, "off")
	require.Equal(t, http.StatusOK, off.Code, off.Raw)
	require.NotContains(t, local(), "remote_control_url")
}
