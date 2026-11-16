package agentd_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// The Remote Access session link Claude Code announces in its transcript (a
// bridge_status record, read by the transcript follower) reaches the
// operator's own dashboard snapshot ("Open in Claude app ↗"), without reading
// the pane, and survives a daemon restart (tk_5ef87574). The link controls
// the session, so a federated peer gets it only for agents it may attach to
// interactively: status or watch-only grants show no link.
func TestRemoteControlSessionURL_LocalSnapshotAndPeerAttachGate(t *testing.T) {
	fh := newFedHarness(t)
	f := fh.f
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	monitor := agentd.StartConvMonitorForTest(t, 10*time.Millisecond)
	if monitor == nil {
		t.Skip("no file watcher here")
	}
	agentd.WaitForConvMonitorStartupForTest(t, monitor)
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

	// linkIn waits for the snapshot to carry a link (the follower records it
	// shortly after the transcript grows), and returns it.
	linkIn := func(what string) string {
		t.Helper()
		var link string
		require.Eventually(t, func() bool {
			body := local()
			i := strings.Index(body, `"remote_control_url":"`)
			if i < 0 {
				return false
			}
			rest := body[i+len(`"remote_control_url":"`):]
			link = rest[:strings.IndexByte(rest, '"')]
			return true
		}, 10*time.Second, 20*time.Millisecond, what)
		return link
	}

	// Remote Access comes up (switched on from the dashboard): Claude Code
	// writes its link into the transcript, and the follower records it.
	on := f.AsHuman().RemoteControl(conv, "on")
	require.Equal(t, http.StatusOK, on.Code, on.Raw)
	url := linkIn("the follower records the link Remote Access announced")
	require.True(t, strings.HasPrefix(url, "https://claude.ai/code/session_"), url)
	st := f.AsHuman().RemoteControl(conv, "link")
	require.Equal(t, http.StatusOK, st.Code, st.Raw)
	require.Equal(t, url, st.SessionURL, "the link intent answers from what was recorded")
	link := `"remote_control_url":"` + url + `"`

	// A daemon restart keeps it: it is on the session row, and the new
	// follower's full read of the transcript changes nothing.
	monitor2 := agentd.StartConvMonitorForTest(t, 10*time.Millisecond)
	agentd.WaitForConvMonitorStartupForTest(t, monitor2)
	require.Contains(t, local(), link, "the link survives a restart")

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

	// The peer's agents cannot attach, so they never get it, even where the
	// peer's operator does.
	worker := &proto.PeerViewAgent{ID: "agt_bobworker000000000000000", Name: "bob-worker", Groups: []string{"crew"}}
	reply := agentPeerViewExchange(t, fh.peer, worker, http.MethodGet, "/api/snapshot", nil)
	require.Equal(t, http.StatusOK, reply.Status, string(reply.Body))
	require.Contains(t, string(reply.Body), conv, "the peer's agent sees the agent")
	require.NotContains(t, string(reply.Body), "remote_control_url", "a peer's agent gets no link")

	// A relaunch is a new session (whose Claude Code has a new link), even
	// where it reuses the tmux session's name: the old launch's record is
	// not this launch's link, whatever the follower reads again.
	f.HaveAliveSession(conv, "cc-url-2", "tmux-cc-url", f.TestCwd("url"))
	require.NoError(t, db.SetSessionRemoteControl("cc-url-2", true))
	require.NotContains(t, local(), "remote_control_url", "a relaunched session shows no old link")
	// The new launch starts with Remote Access off, and is switched on.
	relaunched := f.World.CCs.GetByConvID(conv)
	require.NotNil(t, relaunched)
	require.False(t, relaunched.RemoteControlOn())
	require.NoError(t, db.SetSessionRemoteControl("cc-url-2", false))
	on = f.AsHuman().RemoteControl(conv, "on")
	require.Equal(t, http.StatusOK, on.Code, on.Raw)
	url2 := linkIn("the new launch's link, once announced")
	require.NotEqual(t, url, url2)

	// Switching Remote Access drops the link, and a full re-read of the
	// transcript (a restart) does not bring the old record back.
	off := f.AsHuman().RemoteControl(conv, "off")
	require.Equal(t, http.StatusOK, off.Code, off.Raw)
	require.NotContains(t, local(), "remote_control_url")
	monitor3 := agentd.StartConvMonitorForTest(t, 10*time.Millisecond)
	agentd.WaitForConvMonitorStartupForTest(t, monitor3)
	sess, err := db.FindSessionByConvID(conv)
	require.NoError(t, err)
	require.Empty(t, sess.RCSessionURL, "a cleared link stays cleared")
}
