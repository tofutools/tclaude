package agentd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestHTTPProxyBootstrapProofRequiresLiveGenerationAndAncestry(t *testing.T) {
	setupTestDB(t)
	const label = "http-launch"
	const generation = "http-generation"
	require.NoError(t, db.SaveSession(&db.SessionRow{ID: label, ConvID: "http-conv", TmuxSession: "http-pane", Harness: harness.CodexName, Status: "idle", ExitLaunchGeneration: generation}))
	fakeProcTree{name: map[int]string{9101: "tclaude", 9090: "bash"}, parent: map[int]int{9101: 9090, 9090: 1}}.install(t)
	previous := brokerLivePaneProbe
	paneGeneration := generation
	brokerLivePaneProbe = func(string) (lifecyclePaneProbe, error) {
		return lifecyclePaneProbe{state: paneProbeLive, panePID: 9090, generation: paneGeneration}, nil
	}
	t.Cleanup(func() { brokerLivePaneProbe = previous })
	// The ordinary legacy resolver cannot recognize a pre-harness tclaude
	// bridge. The gateway's proof is rooted in the live pane instead.
	proof := proveLaunchPaneCallerIn(newBrokerProcTable(), 9101, label, false)
	require.NotNil(t, proof.row)
	assert.Equal(t, "http-conv", proof.row.ConvID)
	assert.Nil(t, proveLaunchPaneCallerIn(newBrokerProcTable(), 9200, label, false).row)
	paneGeneration = "foreign-generation"
	// Bypass the memoized pane fact for the negative check.

	assert.Nil(t, proveLaunchPaneCallerIn(newBrokerProcTable(), 9101, label, false).row)
}

func TestHTTPProxySeedLaunchProjectsScopedGroupPermissions(t *testing.T) {
	setupTestDB(t)
	groupID, err := db.CreateAgentGroup("http-seed", "")
	require.NoError(t, err)
	group, err := db.GetAgentGroupByID(groupID)
	require.NoError(t, err)
	require.NoError(t, db.ReplaceAgentGroupPermissionGrants(group.ID, []db.PermissionGrant{{Slug: PermHTTP, ScopeSpecified: true, Scope: `{"http_proxy":["inventory"]}`}}, "test"))
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{HTTPProxies: map[string]config.HTTPProxyConfig{
		"inventory": {URL: "https://inventory.example", Header: "Authorization", HeaderValue: "secret"},
		"billing":   {URL: "https://billing.example", Header: "Authorization", HeaderValue: "private"},
	}}}))
	rememberHTTPProxyLaunchGroup("http-seed-launch", group)
	t.Cleanup(func() { httpProxyLaunchGroups.Delete("http-seed-launch") })
	req := httptest.NewRequest(http.MethodGet, "/v1/http/environment", nil)
	req = req.WithContext(context.WithValue(req.Context(), httpProxyLaunchRowKey{}, &db.SessionRow{ID: "http-seed-launch"}))
	rec := httptest.NewRecorder()
	handleHTTPProxyEnvironment(rec, req)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"names":["inventory"]}`, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "secret")
}
