package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario: `tclaude agent ls` is the agent-facing roster. Its /v1/peers row
// must carry the same runtime facts as the dashboard row: harness, model,
// effort, settled activity state, and live sub-agent count. This uses
// the production hook path for activity and the production statusline write
// path for model metadata.
func TestAgentLs_RuntimeSummaryMatchesDashboardState(t *testing.T) {
	const conv = "peer-1111-2222-3333-444444444444"
	const label = "spwn-peer-runtime"

	f := newFlow(t)
	f.HaveGroup("squad")
	f.HaveAliveSession(conv, label, "tmux-peer-runtime", f.TestCwd("peer-runtime"))
	f.HaveMember("squad", conv)

	require.NoError(t, db.UpdateSessionModel(label, "Opus 4.8"), "record model")
	require.NoError(t, db.UpdateSessionEffort(label, "high"), "record effort")

	apply := func(event, agentID string) {
		t.Helper()
		input := session.HookCallbackInput{
			HookEventName: event,
			ConvID:        conv,
			Cwd:           f.TestCwd("peer-runtime"),
			AgentID:       agentID,
		}
		if agentID != "" {
			input.AgentType = "Explore"
		}
		require.NoError(t, session.ApplyHook(input, label), "ApplyHook(%s)", event)
	}
	apply("SubagentStart", "sub-1")
	apply("Stop", "")

	peer := f.AsHuman().FindPeer(conv)
	require.NotNil(t, peer, "peer missing from /v1/peers")
	assert.Equal(t, "claude", peer.State.Harness)
	assert.Equal(t, "Opus 4.8", peer.State.Model)
	assert.Equal(t, "high", peer.State.EffortLevel)
	assert.Equal(t, session.StatusMainAgentIdle, peer.State.Status)
	assert.Equal(t, 1, peer.State.SubagentCount)

	// StatusDetail can contain user-controlled permission/elicitation text. It
	// belongs on the operator-only dashboard, not the shared-group peer API.
	req := agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/peers", nil))
	rec := testharness.Serve(f.Mux, req)
	require.Equal(t, http.StatusOK, rec.Code, "raw /v1/peers body=%s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), `"status_detail"`)
}
