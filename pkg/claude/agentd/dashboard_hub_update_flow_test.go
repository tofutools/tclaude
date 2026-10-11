package agentd_test

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestDashboardHubUpdateCapabilityUnsupervisedAndHumanOnly(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	handler := agentd.BuildDashboardHandlerForTest()
	call := func(method, tail string, body any) *httptest.ResponseRecorder {
		return testharness.Serve(handler, testharness.JSONRequest(t, method, "/api/federation/hub/"+tail, body))
	}
	must := func(method, tail string, body any) map[string]any {
		t.Helper()
		rec := call(method, tail, body)
		require.Equal(t, 200, rec.Code, rec.Body.String())
		var out map[string]any
		testharness.DecodeJSON(t, rec, &out)
		return out
	}
	token, err := fh.store.PrepareAdminClaim(false, time.Now())
	require.NoError(t, err)
	call("GET", "status", nil)
	status := must("GET", "status", nil)
	must("POST", "claim", map[string]any{"token": token})
	require.Equal(t, 403, call("GET", "update", nil).Code, "bootstrap does not implicitly acquire future update authority")
	local := status["instance"].(string)
	caps := append(append([]string{}, proto.HubAdminBootstrapCapabilities...), "hub.update")
	must("POST", "admins", map[string]any{"instance": local, "capabilities": caps})
	update := must("GET", "update", nil)
	require.Nil(t, update["supervisor"])
	require.Equal(t, "not_supervised", update["blocked"].(map[string]any)["code"])
	for _, action := range []string{"apply", "rollback"} {
		rec := call("POST", "update", map[string]any{"action": action})
		require.Equal(t, 409, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), "not_supervised")
	}
	require.Equal(t, 400, call("POST", "update", map[string]any{"action": "execute", "script": "no"}).Code)
	require.Equal(t, 404, call("GET", "update/jobs/not-a-job", nil).Code)
	// The same /v1 path has the identical hub capability and supervisor gates.
	rec := fedHuman(t, fh.f, "POST", "/v1/federation/hub/update", map[string]any{"action": "apply"})
	require.Equal(t, 409, rec.Code, rec.Body.String())
}

func TestDashboardHubUpdateRoutesAreLocalHumanOnly(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	handler := agentd.BuildDashboardHandlerForTest()
	for _, route := range []struct{ method, tail string }{{"GET", "update"}, {"POST", "update"}, {"GET", "update/jobs/00000000000000000000000000000000"}} {
		req := testharness.JSONRequest(t, route.method, "/api/federation/hub/"+route.tail, map[string]any{})
		require.Equal(t, 403, testharness.Serve(agentd.PeerViewHandler(fh.peer.id.ID()), req).Code)
		req = testharness.JSONRequest(t, route.method, "/v1/federation/hub/"+route.tail, map[string]any{})
		require.Equal(t, 403, testharness.Serve(fh.f.Mux, agentd.AsAgentPeer(req, "hub-agent")).Code)
	}
	req := testharness.JSONRequest(t, "POST", "/api/federation/hub/update", map[string]any{"action": "apply"})
	req.Header.Set("Origin", "https://evil.invalid")
	require.Equal(t, 403, testharness.Serve(handler, req).Code)
}
