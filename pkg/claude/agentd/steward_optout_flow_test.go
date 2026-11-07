package agentd_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Collab is always on; the one switch left is the steward opt-out. With
// steward.disabled no Steward can be summoned and the snapshot tells the
// dashboard to leave out its window, while every other Collab surface (drawing boards,
// decisions) keeps working. A features.collab=false left by an older build
// changes nothing.
func TestStewardOptOutLeavesTheRestOfCollabOn(t *testing.T) {
	f := newFlow(t)
	const agent = "sopt-aaaa-bbbb-cccc-000000000001"
	f.HaveGroup("design")
	f.HaveMember("design", agent)
	f.HaveAliveSession(agent, "sopt", "tclaude-sopt", f.TestCwd("work"))
	require.NoError(t, db.GrantAgentPermission(agent, agentd.PermHumanNotify, "test"))
	require.NoError(t, db.GrantAgentPermission(agent, agentd.PermDrawingBoardsRead, "test"))
	dash := agentd.BuildDashboardHandlerForTest()
	stewardsDisabled := func() bool {
		rec := testharness.Serve(dashMessageMux(t), testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil))
		require.Equal(t, http.StatusOK, rec.Code)
		var s map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &s))
		assert.NotContains(t, s, "collab_enabled", "the Collab flag is gone from the snapshot")
		return s["stewards_disabled"] == true
	}
	collabWorks := func(name string) {
		t.Helper()
		rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/drawing-boards", nil), agent))
		assert.Equal(t, http.StatusOK, rec.Code, "%s: %s", name, rec.Body.String())
		rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/notify-human",
			map[string]any{"body": "pick", "options": []string{"a", "b"}}), agent))
		assert.Equal(t, http.StatusOK, rec.Code, "%s: %s", name, rec.Body.String())
	}

	// An old config's features.collab=false loads and turns nothing off.
	require.NoError(t, os.MkdirAll(filepath.Dir(config.ConfigPath()), 0o700))
	require.NoError(t, os.WriteFile(config.ConfigPath(), []byte(`{"features":{"collab":false}}`), 0o600))
	assert.False(t, stewardsDisabled())
	collabWorks("retired features.collab=false")

	setStewardsDisabled(t, true)
	assert.True(t, stewardsDisabled())
	collabWorks("stewards opted out")
	summon := testharness.Serve(dash, drawingBoardDashReq(t, http.MethodPost, "/api/scribe",
		map[string]any{"name": "steward", "scope": map[string]any{"kind": "steward"}}))
	require.Equal(t, http.StatusNotFound, summon.Code, summon.Body.String())
	assert.Contains(t, summon.Body.String(), "stewards_disabled")

	setStewardsDisabled(t, false)
	assert.False(t, stewardsDisabled(), "turning it back on needs no restart")
}
