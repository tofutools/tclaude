package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
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
				return
			}
			assert.Contains(t, rec.Body.String(), "not_busy")
			assert.Empty(t, sent, "an agent between turns gets no keys")
		})
	}
}
