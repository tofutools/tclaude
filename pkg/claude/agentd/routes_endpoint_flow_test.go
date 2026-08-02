//go:build linux

package agentd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestRoutesEndpointRegistrationAsyncFlow(t *testing.T) {
	setupTestDB(t)
	const (
		publisher = "route-endpoint-publisher"
		consumer  = "route-endpoint-consumer"
		groupName = "route-endpoint-group"
	)
	_, _, err := db.EnsureAgentForConv(publisher, "publisher")
	require.NoError(t, err)
	consumerAgent, _, err := db.EnsureAgentForConv(consumer, "consumer")
	require.NoError(t, err)
	groupID, err := db.CreateAgentGroup(groupName, "")
	require.NoError(t, err)
	require.NoError(t, db.ReplaceAgentGroupPermissions(groupID, []string{PermRoutesPublish, PermRoutesConsume}, "test"))
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: groupID, ConvID: publisher, Role: "worker"}))
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: groupID, ConvID: consumer, Role: "worker"}))
	credential, launchGeneration, err := mintRouteHelperCredential(consumerAgent, consumer)
	require.NoError(t, err)
	t.Cleanup(func() { revokeRouteHelperCredentials(consumer, "") })

	response, route := serveEndpointRouteRequest(t, http.MethodPost, "/v1/routes/publish", publisher, map[string]any{
		"group": groupName, "name": "api", "target": "tcp://127.0.0.1:43127",
	})
	require.Equal(t, http.StatusCreated, response.Code, response.Body.String())
	response, lease := serveEndpointRouteRequest(t, http.MethodPost, "/v1/routes/open", consumer, map[string]any{
		"route_id": route["id"], "group": groupName, "launch_generation": launchGeneration,
	})
	require.Equal(t, http.StatusCreated, response.Code, response.Body.String())
	require.Equal(t, "pending", lease["endpoint_state"], "open is acknowledged before the sibling helper attaches")

	leaseID := lease["id"].(string)
	statusResponse := serveEndpointHelperStatus(t, credential, leaseID, map[string]any{
		"state": "ready", "endpoint": "tcp://127.0.0.1:45810",
	})
	require.Equal(t, http.StatusOK, statusResponse.Code, statusResponse.Body.String())

	response, leases := serveEndpointHelperList(t, credential, groupID)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Len(t, leases, 1)
	require.Equal(t, "ready", leases[0]["endpoint_state"])
	require.Equal(t, "tcp://127.0.0.1:45810", leases[0]["endpoint"])

	// A helper/adapter failure is terminal for this open attempt and remains
	// visible through the same stable lease read path.
	statusResponse = serveEndpointHelperStatus(t, credential, leaseID, map[string]any{
		"state": "refused", "error": "route adapter channel refused",
	})
	require.Equal(t, http.StatusOK, statusResponse.Code, statusResponse.Body.String())
	response, leases = serveEndpointHelperList(t, credential, groupID)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Equal(t, db.RouteLeaseClosed, leases[0]["state"])
	require.Equal(t, "refused", leases[0]["endpoint_state"])
	require.Equal(t, "route adapter channel refused", leases[0]["endpoint_error"])
}

func serveEndpointRouteRequest(t *testing.T, method, path, convID string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	req = AsAgentPeer(req, convID)
	rec := httptest.NewRecorder()
	buildMux().ServeHTTP(rec, req)
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), rec.Body.String())
	return rec, out
}

func serveEndpointHelperStatus(t *testing.T, credential, leaseID string, body any) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/v1/routes/leases/"+leaseID+"/endpoint", bytes.NewReader(payload))
	req.Header.Set(routeHelperCredentialHeader, credential)
	rec := httptest.NewRecorder()
	buildMux().ServeHTTP(rec, req)
	return rec
}

func serveEndpointHelperList(t *testing.T, credential string, groupID int64) (*httptest.ResponseRecorder, []map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/routes/leases?group_id="+strconv.FormatInt(groupID, 10), nil)
	req.Header.Set(routeHelperCredentialHeader, credential)
	rec := httptest.NewRecorder()
	buildMux().ServeHTTP(rec, req)
	var body struct {
		Leases []map[string]any `json:"leases"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return rec, body.Leases
}
