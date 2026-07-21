package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// accessReqSnapshot is the slice of the dashboard snapshot this feature adds:
// the in-flight human-approval requests + their count.
type accessReqSnapshot struct {
	AccessRequests []struct {
		ID            string `json:"id"`
		Perm          string `json:"perm"`
		ConvID        string `json:"conv_id"`
		AgentID       string `json:"agent_id"`
		CurrentConvID string `json:"current_conv_id"`
		ConvTitle     string `json:"conv_title"`
		CallerState   string `json:"caller_state"`
		TitleStatus   string `json:"title_status"`
		AutoGrantable bool   `json:"auto_grantable"`
		Path          string `json:"path"`
		Body          string `json:"body"`
		CreatedAt     string `json:"created_at"`
		Deadline      string `json:"deadline"`
		Status        string `json:"status"`
		DecidedAt     string `json:"decided_at"`
	} `json:"access_requests"`
	AccessRequestsPending int `json:"access_requests_pending"`
}

func TestAccessRequests_RefreshesCallerMetadataByStableAgent(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)

	const requestConv = "access-old-generation"
	const currentConv = "access-current-generation"
	const unrelatedConv = "access-unrelated-generation"
	const id = "access-refresh-stable-caller"
	agentID, _, err := db.EnsureAgentForConv(requestConv, "test")
	require.NoError(t, err)
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID: requestConv, CustomTitle: "title-at-request",
	}))
	unrelatedID, _, err := db.EnsureAgentForConv(unrelatedConv, "test")
	require.NoError(t, err)
	require.NotEqual(t, agentID, unrelatedID)
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID: unrelatedConv, CustomTitle: "wrong-actor-title",
	}))

	done, cleanup := agentd.SeedApprovalCallerWithWaiterForTest(
		id, "self.rename", unrelatedConv, agentID, false)
	t.Cleanup(cleanup)
	h := agentd.BuildDashboardHandlerForTest()

	assertCaller := func(wantTitle, wantCurrent, wantState, wantTitleStatus string) accessReqSnapshot {
		t.Helper()
		snap := fetchAccessReqSnapshot(t, h)
		require.Len(t, snap.AccessRequests, 1)
		got := snap.AccessRequests[0]
		assert.Equal(t, agentID, got.AgentID, "captured stable caller leads identity")
		assert.Equal(t, unrelatedConv, got.ConvID, "request correlation generation stays immutable")
		assert.Equal(t, wantCurrent, got.CurrentConvID)
		assert.Equal(t, wantTitle, got.ConvTitle)
		assert.Equal(t, wantState, got.CallerState)
		assert.Equal(t, wantTitleStatus, got.TitleStatus)
		assert.NotEqual(t, "wrong-actor-title", got.ConvTitle,
			"display refresh must never borrow metadata from the request conv's other actor")
		return snap
	}

	assertCaller("title-at-request", requestConv, "active", "current")
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID: requestConv, CustomTitle: "renamed-while-pending",
	}))
	assertCaller("renamed-while-pending", requestConv, "active", "current")

	require.NoError(t, db.LinkConvToAgent(currentConv, agentID, db.ConvRoleHead, "reincarnate"))
	moved, err := db.SetAgentCurrentConv(agentID, requestConv, currentConv)
	require.NoError(t, err)
	require.True(t, moved)
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID: currentConv, CustomTitle: "current-incarnation",
	}))
	assertCaller("current-incarnation", currentConv, "active", "current")

	retired, err := db.RetireAgentByID(agentID, "human", "test")
	require.NoError(t, err)
	require.True(t, retired)
	assertCaller("current-incarnation", currentConv, "retired", "current")

	require.NoError(t, db.DeleteConvIndex(currentConv))
	assertCaller("(title unavailable)", currentConv, "retired", "unavailable")

	rec := testharness.Serve(h, testharness.JSONRequest(t, http.MethodPost,
		"/api/access-requests/"+id+"/decision", map[string]any{"decision": "deny"}))
	require.Equal(t, http.StatusOK, rec.Code)
	require.False(t, <-done)

	// Handled history preserves the captured stable identity and continues to
	// degrade explicitly after the in-memory request is gone.
	assertCaller("(title unavailable)", currentConv, "retired", "unavailable")
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

// Scenario: after a decision, the request STAYS in the access-requests list as
// history — marked with the outcome + a decided_at — instead of vanishing, so
// the operator can see what they chose. It no longer counts as pending.
func TestAccessRequests_HandledStaysInListAsHistory(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)

	const conv = "acc2-1111-2222-3333-4444"
	const id = "acc-hist-0001"
	done, cleanup := agentd.SeedApprovalWithWaiterForTest(id, "self.rename", conv, false)
	t.Cleanup(cleanup)
	h := agentd.BuildDashboardHandlerForTest()

	// Pending first.
	snap := fetchAccessReqSnapshot(t, h)
	require.Equal(t, 1, snap.AccessRequestsPending)
	require.Len(t, snap.AccessRequests, 1)
	assert.Equal(t, "pending", snap.AccessRequests[0].Status)

	// Approve, and wait for the waiter to resolve + record it.
	rec := testharness.Serve(h, testharness.JSONRequest(t, http.MethodPost,
		"/api/access-requests/"+id+"/decision", map[string]any{"decision": "approve"}))
	require.Equal(t, http.StatusOK, rec.Code, "approve; body=%s", rec.Body.String())
	require.True(t, <-done, "the approved request must proceed")

	// It stays in the list, now marked handled; pending drops to 0.
	snap = fetchAccessReqSnapshot(t, h)
	assert.Equal(t, 0, snap.AccessRequestsPending, "no longer pending")
	require.Len(t, snap.AccessRequests, 1, "the decided request stays as history")
	got := snap.AccessRequests[0]
	assert.Equal(t, id, got.ID)
	assert.Equal(t, "approved", got.Status)
	assert.NotEmpty(t, got.DecidedAt, "handled entries carry a decided_at")

	// Simulate an agentd restart: the in-memory approval registry is empty, but
	// the handled history must still come back from SQLite.
	agentd.ResetApprovalsForTest()
	snap = fetchAccessReqSnapshot(t, h)
	assert.Equal(t, 0, snap.AccessRequestsPending, "no in-memory pending request after restart")
	require.Len(t, snap.AccessRequests, 1, "handled access-request history survives restart")
	assert.Equal(t, id, snap.AccessRequests[0].ID)
	assert.Equal(t, "approved", snap.AccessRequests[0].Status)
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
