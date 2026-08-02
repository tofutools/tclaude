package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunRoutesPublishJSONUsesVersionedDaemonSurface(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(`{"api_version":"v1","id":"rte_api","group_id":7,"group":"team","publisher_agent_id":"agt_pub","name":"api","reference":"agt_pub/api","stable_reference":"7/agt_pub/api","transport":"tcp","target":"tcp://127.0.0.1:43127","state":"ready"}`))
	var stdout, stderr bytes.Buffer

	rc := runRoutesPublish(&routesPublishParams{Name: "api", Group: "team", Target: "tcp://127.0.0.1:43127", JSON: true}, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())
	require.Len(t, calls, 1)
	assert.Equal(t, http.MethodPost, calls[0].method)
	assert.Equal(t, "/v1/routes/publish", calls[0].path)
	body, ok := calls[0].body.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "api", body["name"])
	assert.Equal(t, "team", body["group"])
	assert.Equal(t, "tcp://127.0.0.1:43127", body["target"])
	var got routeCLI
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &got))
	assert.Equal(t, "agt_pub/api", got.Reference)
	assert.Empty(t, stderr.String())
}

func TestRunRoutesOpenResolvesFriendlyReferenceAndPrintsEndpoint(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, func(method, path string) (int, string, string) {
		switch {
		case method == http.MethodGet && path == "/v1/routes?group=team":
			return 200, "", `{"api_version":"v1","group_id":7,"group":"team","routes":[{"id":"rte_api","group_id":7,"group":"team","publisher_agent_id":"agt_pub","name":"api","reference":"agt_pub/api","state":"ready"}]}`
		case method == http.MethodPost && path == "/v1/routes/open":
			return 201, "", `{"api_version":"v1","id":"rlease_api","route_id":"rte_api","route_reference":"agt_pub/api","state":"open","endpoint":"tcp://127.0.0.1:45810"}`
		default:
			return 500, "unexpected", ""
		}
	})
	var stdout, stderr bytes.Buffer

	rc := runRoutesOpen(&routesOpenParams{Reference: "agt_pub/api", Group: "team"}, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())
	require.Len(t, calls, 2)
	assert.Equal(t, "/v1/routes?group=team", calls[0].path)
	assert.Equal(t, "/v1/routes/open", calls[1].path)
	body, ok := calls[1].body.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "rte_api", body["route_id"])
	assert.Equal(t, "team", body["group"])
	assert.Contains(t, stdout.String(), "Consumer endpoint: tcp://127.0.0.1:45810")
	assert.Empty(t, stderr.String())
}

func TestRunRoutesLsAmbiguousGroupIsNonZero(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, func(method, path string) (int, string, string) {
		return http.StatusConflict, "ambiguous", `{"code":"ambiguous"}`
	})
	var stdout, stderr bytes.Buffer

	rc := runRoutesLs(&routesLsParams{JSON: true}, &stdout, &stderr)
	assert.Equal(t, rcAmbiguous, rc)
	assert.Empty(t, stdout.String())
	assert.Contains(t, stderr.String(), "stub error")
}

func TestRunRoutesRejectsUnsafeReferenceBeforeDaemon(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(`{}`))
	var stdout, stderr bytes.Buffer

	rc := runRoutesOpen(&routesOpenParams{Reference: "agt_pub/api\nmessage", Group: "team"}, &stdout, &stderr)
	assert.Equal(t, rcInvalidArg, rc)
	assert.Empty(t, calls)
	assert.Contains(t, stderr.String(), "control character")
}

func TestRunRoutesCloseByFriendlyReferenceFindsOwnLease(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, func(method, path string) (int, string, string) {
		switch {
		case method == http.MethodGet && path == "/v1/routes?group=team":
			return 200, "", `{"group":"team","routes":[{"id":"rte_api","group":"team","publisher_agent_id":"agt_pub","name":"api","reference":"agt_pub/api","state":"ready"}]}`
		case method == http.MethodGet && path == "/v1/routes/leases?group=team":
			return 200, "", `{"leases":[{"id":"rlease_api","route_id":"rte_api","state":"open"}]}`
		case method == http.MethodDelete && path == "/v1/routes/rte_api":
			return http.StatusForbidden, "route_permission", ""
		case method == http.MethodDelete && path == "/v1/routes/leases/rlease_api":
			return 200, "", `{"id":"rlease_api","route_id":"rte_api","state":"closed"}`
		default:
			return 500, "unexpected", ""
		}
	})
	var stdout, stderr bytes.Buffer

	rc := runRoutesClose(&routesCloseParams{Reference: "agt_pub/api", Group: "team", JSON: true}, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())
	require.Len(t, calls, 4)
	assert.Equal(t, http.MethodDelete, calls[3].method)
	assert.Equal(t, "/v1/routes/leases/rlease_api", calls[3].path)
	var got routeLeaseCLI
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &got))
	assert.Equal(t, "closed", got.State)
}
