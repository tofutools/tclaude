package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// sentTo is every send-keys the tmux sim saw for one pane.
func sentTo(f *testharness.Flow, pane string) []string {
	var out []string
	for _, s := range f.World.Tmux.Sent() {
		if s.Target == pane {
			out = append(out, s.Text)
		}
	}
	return out
}

// Scenario: the operator holds a busy Claude Code agent's row on the phone and
// picks Interrupt. The dashboard route presses Esc in its pane, exactly once,
// while it is working or asking; an agent between turns is refused and its
// pane is left alone.
func TestInterrupt_DashboardSendsEscToBusyClaudeAgent(t *testing.T) {
	cases := []struct {
		status string
		want   int
	}{
		{"working", http.StatusOK},
		{"awaiting_permission", http.StatusOK},
		{"awaiting_input", http.StatusOK},
		{"idle", http.StatusConflict},
		{"", http.StatusConflict}, // never reported a status: not known to be busy
	}
	for _, tc := range cases {
		t.Run("status="+tc.status, func(t *testing.T) {
			t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
			f := newFlow(t)
			f.HaveGroup("crew")
			const conv = "019ec004-7777-2222-3333-444444444444"
			f.HaveAliveSession(conv, "cc-int", "tmux-cc-int", "/work")
			f.HaveMember("crew", conv)
			if tc.status != "" {
				f.SetSessionStatus(conv, tc.status)
			}

			mux := agentd.BuildDashboardHandlerForTest()
			rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost, "/api/agents/"+conv+"/interrupt", nil))
			require.Equal(t, tc.want, rec.Code, rec.Body.String())
			var body map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
			sent := sentTo(f, "tmux-cc-int:0.0")
			if tc.want == http.StatusOK {
				assert.Equal(t, "escape", body["via"])
				assert.Equal(t, []string{"Escape"}, sent, "exactly one Esc, to the agent's own pane")
				// Claude Code reports nothing when Esc cancels its turn, so the
				// row is idle now and a second tap sends no second Esc (it would
				// land on the prompt: a cleared line, or the rewind menu).
				again := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost, "/api/agents/"+conv+"/interrupt", nil))
				assert.Equal(t, http.StatusConflict, again.Code, again.Body.String())
				assert.Contains(t, again.Body.String(), "not_busy")
				assert.Equal(t, []string{"Escape"}, sentTo(f, "tmux-cc-int:0.0"), "still one Esc")
				return
			}
			assert.Contains(t, rec.Body.String(), "not_busy")
			assert.Empty(t, sent, "an agent between turns gets no keys")
		})
	}
}

// Scenario: the Esc is this node's operator's alone. The agent CLI's interrupt
// stays Codex-only, and a peer node's view, even under unrestricted trust,
// gets the Codex-only answer too: no key reaches a busy Claude Code pane.
func TestInterrupt_EscIsNotForAgentsOrPeers(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	f.HaveGroup("crew")
	const conv = "019ec004-8888-2222-3333-444444444444"
	f.HaveAliveSession(conv, "cc-busy", "tmux-cc-busy", f.World.HomeDir)
	f.HaveMember("crew", conv)
	f.SetSessionStatus(conv, "working")
	agentID, err := db.AgentIDForConv(conv)
	require.NoError(t, err)

	// Granted, so the answer is the route's own, not the permission gate's.
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermAgentInterrupt, "test"))
	code, body := f.AsAgent(conv).Interrupt(conv)
	assert.Equal(t, http.StatusBadRequest, code, body)
	assert.Contains(t, body, "unsupported")

	peer, err := db.GetFederationPeer(p.id.ID())
	require.NoError(t, err)
	peer.TrustLevel = db.FederationTrustUnrestricted
	require.NoError(t, db.TrustFederationPeer(*peer))
	rec := testharness.Serve(agentd.PeerViewHandler(p.id.ID()), testharness.JSONRequest(t, http.MethodPost, "/api/agents/"+agentID+"/interrupt", nil))
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "unsupported")

	assert.Empty(t, sentTo(f, "tmux-cc-busy:0.0"), "no Esc from an agent or a peer")
}
