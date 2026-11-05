package agentd

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// An unconfirmed launch-enrollment spawn (no live pane before the deadline)
// keeps its route-helper credential: the enrollment is preserved because the
// child may still be coming up, and a revoked credential would leave that
// child without its routes. Unlike a confirmed failure, which revokes it.
func TestRouteHelperUnconfirmedLaunchKeepsCredential(t *testing.T) {
	setupTestDB(t)
	t.Cleanup(SetTclaudeLayerHostAvailabilitiesForTest(func() error { return nil }, func() error { return nil }))
	groupID, err := db.CreateAgentGroup("enrolled-routes", "")
	require.NoError(t, err)
	require.NoError(t, db.ReplaceAgentGroupPermissions(groupID, []string{PermRoutesPublish, PermRoutesConsume}, "test"))
	spawner := &pendingRouteHelperSpawner{}
	previous := Spawn
	Spawn = spawner
	t.Cleanup(func() { Spawn = previous })
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Global: &sandboxpolicy.Profile{Name: "private", NetworkAccess: sandboxpolicy.NetworkAccessNone}})
	require.NoError(t, err)
	snapshot := sandboxpolicy.NewSnapshot(effective, nil)
	_, failure := executeSpawn(&db.AgentGroup{ID: groupID, Name: "enrolled-routes"}, spawnParams{
		Name: "enrolled-worker", Harness: "claude", Cwd: testutil.CanonicalTempDir(t), Timeout: time.Millisecond,
		SandboxImplementation: "tclaude-layer",
		EffectiveSandbox:      &snapshot,
	})
	require.NotNil(t, failure)
	require.Equal(t, spawnUnconfirmedKind, failure.Kind, failure.Msg)
	require.NotEmpty(t, spawner.args.RouteHelperCredential, "the enrolled launch carries a route helper")
	req := httptest.NewRequest(http.MethodGet, "/v1/routes/helper-identity", nil)
	req.Header.Set(routeHelperCredentialHeader, spawner.args.RouteHelperCredential)
	_, _, valid := routeHelperCredentialForRequest(req)
	require.True(t, valid, "an unconfirmed launch keeps its route-helper credential")
}
