package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// TestDashboardSnapshot_GroupsRouteMapFeatureFlag pins the dark-by-default
// contract at the production snapshot boundary: disabled snapshots omit the
// route-only projection, while an explicit config opt-in carries it and the
// feature state needed by the Groups subview. Config-tab load/save is covered
// by the embedded asset contract and the common/config round-trip test.
func TestDashboardSnapshot_GroupsRouteMapFeatureFlag(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)
	handler := agentd.BuildDashboardHandlerForTest()
	fetch := func() (dashSnapshot, string) {
		t.Helper()
		req := testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil)
		rec := testharness.Serve(handler, req)
		require.Equal(t, http.StatusOK, rec.Code, "snapshot body=%s", rec.Body.String())
		var snap dashSnapshot
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snap), "decode snapshot")
		return snap, rec.Body.String()
	}

	off, offBody := fetch()
	assert.False(t, off.GroupsRouteMapEnabled)
	assert.NotContains(t, offBody, `"route_map"`, "route projection stays absent while flag is off")

	require.NoError(t, config.Save(&config.Config{
		Features: &config.FeaturesConfig{GroupsRouteMap: true},
	}))
	on, onBody := fetch()
	assert.True(t, on.GroupsRouteMapEnabled)
	assert.Contains(t, onBody, `"route_map"`, "route projection appears after explicit opt-in")
}
