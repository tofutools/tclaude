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
		"inventory": {URL: "https://inventory.example", Header: "Authorization", HeaderValue: "secret", EnvironmentVariable: "INVENTORY_API_URL"},
		"billing":   {URL: "https://billing.example", Header: "Authorization", HeaderValue: "private"},
	}}}))
	rememberHTTPProxyLaunchGroup("http-seed-launch", group)
	t.Cleanup(func() { httpProxyLaunchGroups.Delete("http-seed-launch") })
	req := httptest.NewRequest(http.MethodGet, "/v1/http/environment", nil)
	req = req.WithContext(context.WithValue(req.Context(), httpProxyLaunchRowKey{}, &db.SessionRow{ID: "http-seed-launch"}))
	rec := httptest.NewRecorder()
	handleHTTPProxyEnvironment(rec, req)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"names":["inventory"],"environment_variables":{"inventory":"INVENTORY_API_URL"}}`, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "secret")
}

func TestHTTPProxyRuntimeIdentityRequiresItsOwnProcessAndEndpoint(t *testing.T) {
	setupTestDB(t)
	const label = "http-opencode-runtime"
	const conv = "ses_http_proxy"
	fakeProcTree{name: map[int]string{9101: "tclaude", 9100: "bwrap"}, parent: map[int]int{9101: 9100, 9100: 1}}.install(t)
	openCodeProcesses.Lock()
	openCodeProcesses.bySession[label] = &openCodeProcess{pid: 9100}
	openCodeProcesses.Unlock()
	t.Cleanup(func() {
		openCodeProcesses.Lock()
		delete(openCodeProcesses.bySession, label)
		openCodeProcesses.Unlock()
	})
	row, identity := httpProxyRuntimeCaller(9101, label)
	require.NotNil(t, row)
	assert.Empty(t, identity, "bootstrap can only discover names")
	row, identity = httpProxyRuntimeCaller(9200, label)
	assert.Nil(t, row)
	assert.Empty(t, identity)
	require.NoError(t, db.UpsertOpenCodeRuntime(db.OpenCodeRuntime{SessionID: label, ConvID: conv, ServerURL: "http://127.0.0.1:43210", Password: "private", PID: 9100, Cwd: "/tmp/project"}))
	verified := openCodeRuntimeVerified
	t.Cleanup(func() { openCodeRuntimeVerified = verified })
	openCodeRuntimeVerified = func(db.OpenCodeRuntime) bool { return false }
	row, identity = httpProxyRuntimeCaller(9101, label)
	require.NotNil(t, row)
	assert.Empty(t, identity)
	openCodeRuntimeVerified = func(db.OpenCodeRuntime) bool { return true }
	_, identity = httpProxyRuntimeCaller(9101, label)
	assert.Equal(t, conv, identity)
	openCodeProcesses.Lock()
	openCodeProcesses.bySession[label].stopping = true
	openCodeProcesses.Unlock()
	row, identity = httpProxyRuntimeCaller(9101, label)
	assert.Nil(t, row)
	assert.Empty(t, identity)
}

func TestHTTPProxySeedProjectionIncludesBirthOverrides(t *testing.T) {
	setupTestDB(t)
	groupID, err := db.CreateAgentGroup("http-birth", "")
	require.NoError(t, err)
	group, err := db.GetAgentGroupByID(groupID)
	require.NoError(t, err)
	require.NoError(t, db.ReplaceAgentGroupPermissionGrants(group.ID, []db.PermissionGrant{{Slug: PermHTTP, Scope: `{"http_proxy":["inventory"]}`, ScopeSpecified: true}}, "test"))
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{HTTPProxies: map[string]config.HTTPProxyConfig{"inventory": {}, "billing": {}}}}))
	for _, tc := range []struct{ effect, scope, expected string }{
		{db.PermEffectGrant, `{"http_proxy":["billing"]}`, `{"names":["billing"]}`},
		{db.PermEffectDeny, "", `{"names":[]}`},
	} {
		rememberHTTPProxyLaunchGroup("birth-launch", group, map[string]db.PermissionOverride{PermHTTP: {Effect: tc.effect, Scope: tc.scope}})
		req := httptest.NewRequest(http.MethodGet, "/v1/http/environment", nil)
		req = req.WithContext(context.WithValue(req.Context(), httpProxyLaunchRowKey{}, &db.SessionRow{ID: "birth-launch"}))
		rec := httptest.NewRecorder()
		handleHTTPProxyEnvironment(rec, req)
		require.Equal(t, 200, rec.Code)
		assert.JSONEq(t, tc.expected, rec.Body.String())
	}
	httpProxyLaunchGroups.Delete("birth-launch")
}

func TestHTTPProxyProofRateRoutingRequiresAVerifiedConnectionSubject(t *testing.T) {
	req := httptest.NewRequest("GET", "/v1/http/environment", nil)
	cache := &httpProxyProofSubject{}
	req = req.WithContext(context.WithValue(req.Context(), httpProxyProofSubjectKey{}, cache))
	assert.Equal(t, brokerProofKey, httpProxyRateKey(req, "pane:worker"))
	rememberHTTPProxyProofSubject(req, "pane:worker", "worker")
	assert.Equal(t, brokerProofKeyForRow("worker"), httpProxyRateKey(req, "pane:worker"))
	assert.Equal(t, brokerProofKey, httpProxyRateKey(req, "pane:victim"))
	assert.Equal(t, brokerProofKey, httpProxyRateKey(req, "runtime:worker"))
}
