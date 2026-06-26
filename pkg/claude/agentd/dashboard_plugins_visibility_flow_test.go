package agentd_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// TestDashboardSnapshot_PluginsTabVisibilityRule pins the Plugins-tab
// auto-hide flag the front-end keys off (applyPluginsTabVisibility in
// refresh.js → body.hide-plugins). Mirrors the Costs-tab rule:
//   - fresh, nothing installed, no opt-in → hidden (the empty-tab clutter the
//     auto-hide exists to remove).
//   - dashboard.always_show_plugins_tab opt-in → visible even with nothing
//     installed — the escape hatch to the install-from-catalog UI.
//   - ≥1 plugin installed → visible regardless of the opt-in (there's
//     something to manage).
func TestDashboardSnapshot_PluginsTabVisibilityRule(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	newFlow(t)

	// 1. Fresh, no plugins, no opt-in → tab hidden.
	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	assert.False(t, snap.PluginsTabVisible, "no plugins + no opt-in hides the Plugins tab")

	// 2. Opt in via config → visible even with nothing installed, so the
	//    human can reach the catalog and install one.
	require.NoError(t, config.Save(&config.Config{
		Dashboard: &config.DashboardConfig{AlwaysShowPluginsTab: true},
	}), "save config with always_show_plugins_tab")
	snap = fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	assert.True(t, snap.PluginsTabVisible, "the opt-in shows the Plugins tab with nothing installed")

	// 3. Clear the opt-in and install a plugin → visible because there's now
	//    something to manage, opt-in or not.
	require.NoError(t, config.Save(&config.Config{}), "clear the always-show opt-in")
	mux := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost, "/api/plugins", map[string]any{
		"name":  "demo",
		"steps": []map[string]any{{"name": "step1", "check": "true"}},
	}))
	require.Equal(t, http.StatusCreated, rec.Code, "install plugin body=%s", rec.Body.String())

	snap = fetchDashSnapshot(t, mux)
	assert.True(t, snap.PluginsTabVisible, "an installed plugin shows the Plugins tab even with the opt-in off")

	// 4. A broken plugins.json (no installed plugins parse out, opt-in still
	//    off) → visible, so the human can see + fix the parse error instead of
	//    a silently empty/hidden tab. This is the branch the whole
	//    PluginsError operand exists for.
	require.NoError(t, os.WriteFile(filepath.Join(config.ConfigDir(), "plugins.json"), []byte("{ not valid json"), 0o600), "write a broken plugins.json")
	snap = fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	assert.True(t, snap.PluginsTabVisible, "a broken plugins.json keeps the Plugins tab visible so the error isn't hidden")
}
