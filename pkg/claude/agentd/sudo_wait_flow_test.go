package agentd_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// pendingAccessRequest polls the dashboard snapshot until a pending request
// for perm shows up, and returns it.
func pendingAccessRequest(t *testing.T, dashboard http.Handler, perm string) (id, created, deadline, ceiling string) {
	t.Helper()
	require.Eventually(t, func() bool {
		for _, r := range fetchAccessReqSnapshot(t, dashboard).AccessRequests {
			if r.Status == db.AccessRequestStatusPending && r.Perm == perm {
				id, created, deadline, ceiling = r.ID, r.CreatedAt, r.Deadline, r.WaitCeiling
				return true
			}
		}
		return false
	}, 10*time.Second, 5*time.Millisecond, "a pending %s request must reach the dashboard", perm)
	return
}

func accessRequestStatus(t *testing.T, dashboard http.Handler, id string) string {
	t.Helper()
	for _, r := range fetchAccessReqSnapshot(t, dashboard).AccessRequests {
		if r.ID == id {
			return r.Status
		}
	}
	return ""
}

// Scenario: an agent's `sudo request` gives up (its client times out, or it
// is killed) while the popup is still open, and the operator approves
// afterwards. The request is withdrawn the moment the caller hangs up, so
// the late approval is refused and no grant lands behind a caller that
// already reported failure. This is the 2026-11-27 phone incident: the
// CLI errored with a client timeout, then the operator's approval minted
// grants nobody was waiting for.
func TestSudo_CallerGone_WithdrawsRequestAndLateApprovalGrantsNothing(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	const conv = "sudo-gone-aaaa-bbbb-1111"
	f.HaveConvWithTitle(conv, "worker")

	ctx, hangUp := context.WithCancel(context.Background())
	t.Cleanup(hangUp)
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		// The peer rides on the context, so the cancellable one goes first.
		r := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/sudo", map[string]any{
			"slugs": []string{"groups.create"}, "duration": "15m", "wait": "10m",
		}).WithContext(ctx), conv)
		result <- testharness.Serve(f.Mux, r)
	}()

	dashboard := agentd.BuildDashboardHandlerForTest()
	id, _, _, _ := pendingAccessRequest(t, dashboard, "sudo.groups.create")

	hangUp()
	rec := <-result
	assert.Equal(t, http.StatusForbidden, rec.Code, "a withdrawn request is refused; body=%s", rec.Body.String())
	assert.Equal(t, "withdrawn", accessRequestStatus(t, dashboard, id),
		"the history card must say the caller withdrew, not that anyone declined")

	late := testharness.Serve(dashboard, testharness.JSONRequest(t, http.MethodPost,
		"/api/access-requests/"+id+"/decision", map[string]any{"decision": "approve"}))
	assert.Equal(t, http.StatusNotFound, late.Code, "an approval after the caller left must be refused; body=%s", late.Body.String())

	rows, err := db.ListActiveSudoGrants(conv)
	require.NoError(t, err)
	assert.Empty(t, rows, "no orphan grant may appear after the caller gave up")
}

// Scenario: the caller declares how long it will wait. The popup still
// auto-denies after the configured popup timeout (60s by default), but
// "+extend" can only push the deadline as far as the declared wait, since
// the caller's client hangs up just after it. The dashboard shows that
// ceiling, and the extend answer says it was capped.
func TestSudo_WaitCapsPopupDeadlineAndExtend(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	now := time.Date(2026, 11, 27, 17, 15, 0, 0, time.UTC)
	t.Cleanup(agentd.SetApprovalClockForTest(func() time.Time { return now }))
	f := newFlow(t)
	const conv = "sudo-wait-aaaa-bbbb-1111"
	f.HaveConvWithTitle(conv, "worker")

	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/sudo", map[string]any{
			"slugs": []string{"groups.create"}, "duration": "15m", "wait": "2m",
		}), conv)
		result <- testharness.Serve(f.Mux, r)
	}()

	dashboard := agentd.BuildDashboardHandlerForTest()
	id, created, deadline, ceiling := pendingAccessRequest(t, dashboard, "sudo.groups.create")
	rfc := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	assert.Equal(t, rfc(0), created)
	assert.Equal(t, rfc(60*time.Second), deadline, "the unattended popup still auto-denies at the popup timeout")
	assert.Equal(t, rfc(2*time.Minute), ceiling, "the ceiling is the caller's declared wait")

	ext := testharness.Serve(dashboard, testharness.JSONRequest(t, http.MethodPost,
		"/api/access-requests/"+id+"/decision", map[string]any{"decision": "extend", "secs": 300}))
	require.Equal(t, http.StatusOK, ext.Code, "extend; body=%s", ext.Body.String())
	var extBody struct {
		ExtendedBySecs int  `json:"extended_by_secs"`
		Capped         bool `json:"capped"`
	}
	require.NoError(t, json.Unmarshal(ext.Body.Bytes(), &extBody))
	assert.True(t, extBody.Capped, "a +5m extend past a 2m wait is capped")
	assert.Equal(t, 120, extBody.ExtendedBySecs, "the extend buys only the time the caller still waits")
	require.Eventually(t, func() bool {
		for _, r := range fetchAccessReqSnapshot(t, dashboard).AccessRequests {
			if r.ID == id {
				return r.Deadline == rfc(2*time.Minute)
			}
		}
		return false
	}, 10*time.Second, 5*time.Millisecond, "the extended deadline stops at the caller's wait ceiling")

	approve := testharness.Serve(dashboard, testharness.JSONRequest(t, http.MethodPost,
		"/api/access-requests/"+id+"/decision", map[string]any{"decision": "approve"}))
	require.Equal(t, http.StatusOK, approve.Code, "approve; body=%s", approve.Body.String())
	rec := <-result
	require.Equal(t, http.StatusOK, rec.Code, "an approval inside the wait grants; body=%s", rec.Body.String())
	rows, err := db.ListActiveSudoGrants(conv)
	require.NoError(t, err)
	assert.Len(t, rows, 1, "the approved grant lands")
}

func TestSudo_MalformedWaitRejectedBeforeApproval(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	const conv = "sudo-badwait-aaaa-bbbb-1111"
	f.HaveConvWithTitle(conv, "worker")
	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/sudo", map[string]any{
		"slugs": []string{"groups.create"}, "wait": "soon",
	}), conv))
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Zero(t, fetchAccessReqSnapshot(t, agentd.BuildDashboardHandlerForTest()).AccessRequestsPending,
		"a malformed wait must never reach approval")
}

// Scenario: the same hang-up on an --ask-human gate. The gated action (here
// a cross-agent resume) must not run for a caller that already gave up, and
// the ask-human wait is the ceiling "+extend" cannot pass.
func TestAskHuman_CallerGone_ActionDoesNotRun(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	const (
		target     = "askgone-target-111111111111"
		targetTmux = "tmux-askgone-target"
		caller     = "askgone-caller-111111111111"
	)
	f.HaveConvWithTitle(target, "askgone-target")
	f.HaveAliveSession(target, "spwn-askgone-target", targetTmux, t.TempDir())
	f.MarkOffline(targetTmux)
	f.HaveGroup("askgone-team")
	f.HaveMember("askgone-team", target)

	ctx, hangUp := context.WithCancel(context.Background())
	t.Cleanup(hangUp)
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost,
			"/v1/agent/"+target+"/resume", nil).WithContext(ctx), caller)
		req.Header.Set("X-Tclaude-Ask-Human", "2m")
		result <- testharness.Serve(f.Mux, req)
	}()

	dashboard := agentd.BuildDashboardHandlerForTest()
	id, created, _, ceiling := pendingAccessRequest(t, dashboard, agentd.PermAgentResume)
	createdAt, err := time.Parse(time.RFC3339, created)
	require.NoError(t, err)
	assert.Equal(t, createdAt.Add(2*time.Minute).Format(time.RFC3339), ceiling,
		"the ask-human duration is the caller's wait ceiling")

	hangUp()
	rec := <-result
	assert.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, "withdrawn", accessRequestStatus(t, dashboard, id))
	late := testharness.Serve(dashboard, testharness.JSONRequest(t, http.MethodPost,
		"/api/access-requests/"+id+"/decision", map[string]any{"decision": "approve"}))
	assert.Equal(t, http.StatusNotFound, late.Code, "body=%s", late.Body.String())
	_, launched := f.World.SpawnCwdWriteProof(target)
	assert.False(t, launched, "a withdrawn resume must never launch")
}
