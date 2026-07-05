package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// accessReqSnapshot is the slice of the dashboard snapshot this feature adds:
// the in-flight human-approval requests + their count.
type accessReqSnapshot struct {
	AccessRequests []struct {
		ID            string `json:"id"`
		Perm          string `json:"perm"`
		ConvID        string `json:"conv_id"`
		ConvTitle     string `json:"conv_title"`
		AutoGrantable bool   `json:"auto_grantable"`
		CreatedAt     string `json:"created_at"`
		Deadline      string `json:"deadline"`
	} `json:"access_requests"`
	AccessRequestsPending int `json:"access_requests_pending"`
}

func fetchAccessReqSnapshot(t *testing.T, mux http.Handler) accessReqSnapshot {
	t.Helper()
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil))
	require.Equal(t, http.StatusOK, rec.Code, "/api/snapshot body=%s", rec.Body.String())
	var snap accessReqSnapshot
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snap), "decode snapshot")
	return snap
}

// Scenario: a pending human-approval request surfaces on the dashboard
// snapshot's access_requests, and the operator's "approve" decision — POSTed to
// the new dashboard endpoint that replaced the loopback /approve popup — resolves
// the blocked waiter. This is the end-to-end path that makes approvals work
// remotely (over the dashboard's own auth) instead of via a host browser popup.
func TestAccessRequests_SurfaceOnSnapshotAndApprove(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t) // DB setup (peerAgentID resolution on the snapshot side)

	const conv = "acc0-1111-2222-3333-4444"
	const id = "acc-req-0001"
	done, cleanup := agentd.SeedApprovalWithWaiterForTest(id, "self.rename", conv, false)
	t.Cleanup(cleanup)

	h := agentd.BuildDashboardHandlerForTest()

	// It appears on the snapshot with its metadata + a live deadline.
	snap := fetchAccessReqSnapshot(t, h)
	require.Equal(t, 1, snap.AccessRequestsPending, "the pending request must be counted")
	require.Len(t, snap.AccessRequests, 1)
	got := snap.AccessRequests[0]
	assert.Equal(t, id, got.ID)
	assert.Equal(t, "self.rename", got.Perm)
	assert.Equal(t, conv, got.ConvID)
	assert.NotEmpty(t, got.CreatedAt)
	assert.NotEmpty(t, got.Deadline, "the waiter stamps a live auto-deny deadline")

	// The operator approves via the dashboard endpoint; the blocked waiter
	// consumes exactly that decision and proceeds.
	rec := testharness.Serve(h, testharness.JSONRequest(t, http.MethodPost,
		"/api/access-requests/"+id+"/decision", map[string]any{"decision": "approve"}))
	require.Equal(t, http.StatusOK, rec.Code, "approve decision; body=%s", rec.Body.String())
	require.True(t, <-done, "the approved request must proceed")
}

// Scenario: a "deny" decision resolves the waiter as not-approved.
func TestAccessRequests_DenyResolvesFalse(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)

	const id = "acc-req-deny-1"
	done, cleanup := agentd.SeedApprovalWithWaiterForTest(id, "self.rename", "acc1-1111-2222-3333-4444", false)
	t.Cleanup(cleanup)

	h := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(h, testharness.JSONRequest(t, http.MethodPost,
		"/api/access-requests/"+id+"/decision", map[string]any{"decision": "deny"}))
	require.Equal(t, http.StatusOK, rec.Code, "deny decision; body=%s", rec.Body.String())
	require.False(t, <-done, "a denied request must not proceed")
}

// Scenario: a decision for an unknown/expired id is a clean 404, not a hang or
// a 500 — the request already got decided, timed out, or never existed.
func TestAccessRequests_UnknownIdIs404(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)
	h := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(h, testharness.JSONRequest(t, http.MethodPost,
		"/api/access-requests/no-such-id/decision", map[string]any{"decision": "approve"}))
	assert.Equal(t, http.StatusNotFound, rec.Code, "unknown id must 404; body=%s", rec.Body.String())
}

// Scenario: the decision endpoint is gated by the dashboard auth (cookie +
// Origin). A raw request without the injected cookie is refused, proving the
// gate is real rather than the test harness always injecting one.
func TestAccessRequests_DecisionRequiresAuth(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)

	const id = "acc-req-auth-1"
	t.Cleanup(agentd.SeedApprovalForTest(id, "self.rename", false))

	mux := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(mux) // no cookie-injection wrapper
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost,
		"/api/access-requests/"+id+"/decision", map[string]any{"decision": "approve"}))
	assert.Equal(t, http.StatusForbidden, rec.Code, "uncookied decision must be refused; body=%s", rec.Body.String())
}
