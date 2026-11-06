package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
)

// startOperatorRun starts a run as this node's operator and returns its id.
func startOperatorRun(t *testing.T, h *peerProcessHarness, id, template string, extra map[string]any) string {
	t.Helper()
	body := map[string]any{"id": id, "templateId": template, "authorizeProgramProfiles": []string{}}
	for k, v := range extra {
		body[k] = v
	}
	rec := processRuntimeRequest(t, h.f, http.MethodPost, "/v1/process/runs", body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	agentd.WaitForProcessDispatchForTest()
	return id
}

// Pins: a restricted peer reaches no process run manage verb, whatever it is
// granted, while unrestricted trust runs them as this node's operator.
func TestPeerProcessManagePins(t *testing.T) {
	h := newPeerProcessHarness(t)
	h.grant(t, agentd.PermGroupsRosterRead, "group=crew")
	h.grant(t, agentd.PermProcessRunsRead, "")
	run := startOperatorRun(t, h, "run_pin_gate", "gate", map[string]any{"group": "crew"})
	for _, c := range []struct {
		path string
		body any
	}{
		{"/v1/process/runs/" + run + "/pause", map[string]any{}},
		{"/v1/process/runs/" + run + "/resume", map[string]any{}},
		{"/v1/process/runs/" + run + "/decide", map[string]any{"nodeId": "choose", "verdict": "approve"}},
		{"/v1/process/runs/" + run + "/rescope", map[string]any{"none": true}},
		{"/v1/process/runs/" + run + "/nodes/choose/assign", map[string]any{"agent": h.member}},
	} {
		code, body := h.call("POST", c.path, c.body)
		require.Equal(t, 403, code, "%s: %s", c.path, body)
	}

	h.trustUnrestricted(t)
	code, body := h.call("POST", "/v1/process/runs/"+run+"/pause", map[string]any{})
	require.Equal(t, 200, code, body)
	require.Contains(t, body, `"changed":true`)
	code, body = h.call("POST", "/v1/process/runs/"+run+"/decide", map[string]any{"nodeId": "choose", "verdict": "approve"})
	require.Equal(t, http.StatusAccepted, code, body)
}
