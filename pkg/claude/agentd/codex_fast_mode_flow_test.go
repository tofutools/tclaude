package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func postDashboardFastDisable(t *testing.T, handler http.Handler, convID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/"+convID+"/fast-mode/disable", nil)
	req.Header.Set("Origin", "http://127.0.0.1:0")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, agentd.AsHumanPeer(req))
	return rec
}

func TestDashboardCodexFastMode_LiveIndicatorAndDisable(t *testing.T) {
	const (
		conv  = "019ec064-4250-79b1-9ade-ebaea4170640"
		label = "spwn-fast-live"
	)
	agentd.ResetCodexContextRefreshForTest()
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("fast-squad")
	cx := f.HaveAliveCodexSession(conv, label, "tmux-fast-live", f.TestCwd("fast-live"))
	f.HaveMember("fast-squad", conv)
	require.NoError(t, cx.WriteThreadSettingsApplied("priority"))

	handler := agentd.BuildDashboardHandlerForTest()
	snap := fetchDashSnapshot(t, handler)
	agentRow := findDashAgent(snap, conv)
	require.NotNil(t, agentRow)
	require.NotNil(t, agentRow.State.FastMode, "authoritative live state is surfaced")
	assert.True(t, *agentRow.State.FastMode)
	memberRow := findDashMember(snap, "fast-squad", conv)
	require.NotNil(t, memberRow)
	require.NotNil(t, memberRow.State.FastMode)
	assert.True(t, *memberRow.State.FastMode)

	// Model Codex's no-argument toggle: the pane appends a standard-tier
	// settings snapshot as soon as it processes /fast.
	cx.OnInput("/fast", func(c *testharness.CodexSim, _ string) bool {
		require.NoError(t, c.WriteThreadSettingsApplied("default"))
		return true
	})
	rec := postDashboardFastDisable(t, handler, conv)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	f.AssertSentContains("tmux-fast-live:0.0", "/fast", 10*time.Second)

	snap = fetchDashSnapshot(t, handler)
	agentRow = findDashAgent(snap, conv)
	require.NotNil(t, agentRow)
	require.NotNil(t, agentRow.State.FastMode, "known-off remains distinct from unknown on the wire")
	assert.False(t, *agentRow.State.FastMode)

	// The endpoint re-reads the follower and refuses to toggle a known-off
	// thread back on.
	rec = postDashboardFastDisable(t, handler, conv)
	assert.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "already_off")
}

func TestDashboardCodexFastMode_UnknownRefusesWithoutInference(t *testing.T) {
	const conv = "019ec064-4250-79b1-9ade-ebaea4170641"
	agentd.ResetCodexContextRefreshForTest()
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("fast-squad")
	f.HaveAliveCodexSession(conv, "spwn-fast-unknown", "tmux-fast-unknown", f.TestCwd("fast-unknown"))
	f.HaveMember("fast-squad", conv)

	handler := agentd.BuildDashboardHandlerForTest()
	snap := fetchDashSnapshot(t, handler)
	row := findDashAgent(snap, conv)
	require.NotNil(t, row)
	assert.Nil(t, row.State.FastMode, "launch posture must not fabricate live state")

	rec := postDashboardFastDisable(t, handler, conv)
	assert.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "unknown")
	for _, sent := range f.World.Tmux.Sent() {
		assert.NotEqual(t, "/fast", sent.Text, "unknown state must not reach the injection sink")
	}
}
