package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// stubApproval is a thin wrapper around the agentd-side helper so the
// test file reads top-to-bottom without juggling restore functions.
func stubApproval(t *testing.T, decision bool) {
	t.Helper()
	t.Cleanup(agentd.StubApprovalForTest(decision))
}

func TestCrossAgentAskHuman_ResumeCreatesPendingRequestAndApprovalLaunchesOnce(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	const (
		target     = "resume-approval-target-111111111111"
		targetTmux = "tmux-resume-approval-target"
		caller     = "resume-approval-caller-111111111111"
	)
	cwd := t.TempDir()
	f.HaveConvWithTitle(target, "resume-approval-target")
	f.HaveAliveSession(target, "spwn-resume-approval-target", targetTmux, cwd)
	f.MarkOffline(targetTmux)
	f.HaveGroup("resume-approval-team")
	f.HaveMember("resume-approval-team", target)

	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost,
			"/v1/agent/"+target+"/resume", nil), caller)
		req.Header.Set("X-Tclaude-Ask-Human", "5s")
		result <- testharness.Serve(f.Mux, req)
	}()

	dashboard := agentd.BuildDashboardHandlerForTest()
	pendingID := ""
	require.Eventually(t, func() bool {
		snap := fetchAccessReqSnapshot(t, dashboard)
		for _, request := range snap.AccessRequests {
			if request.Status == db.AccessRequestStatusPending && request.Perm == agentd.PermAgentResume {
				pendingID = request.ID
				return true
			}
		}
		return false
	}, 10*time.Second, 10*time.Millisecond, "resume must create a real pending access request")

	decision := testharness.Serve(dashboard, testharness.JSONRequest(t, http.MethodPost,
		"/api/access-requests/"+pendingID+"/decision", map[string]any{"decision": "approve"}))
	require.Equal(t, http.StatusOK, decision.Code, "decision body=%s", decision.Body.String())
	response := <-result
	require.Equal(t, http.StatusOK, response.Code, "resume body=%s", response.Body.String())
	proof, launched := f.World.SpawnCwdWriteProof(target)
	assert.True(t, launched, "approved resume must launch exactly once")
	assert.NotEmpty(t, proof,
		"daemon-owned launch pin must bind target provenance without asking the caller to write there")
}

func TestCrossAgentAskHuman_ResumeDenyAndTimeoutLeaveTargetStopped(t *testing.T) {
	for _, tc := range []struct {
		name     string
		timeout  string
		decision string
		status   string
	}{
		{name: "deny", timeout: "5s", decision: "deny", status: "declined"},
		{name: "timeout", timeout: "1ms", status: "timed out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
			f := newFlow(t)
			target := "resume-" + tc.name + "-target-111111111111"
			targetTmux := "tmux-resume-" + tc.name + "-target"
			caller := "resume-" + tc.name + "-caller-111111111111"
			f.HaveConvWithTitle(target, "resume-"+tc.name+"-target")
			f.HaveAliveSession(target, "spwn-resume-"+tc.name+"-target", targetTmux, t.TempDir())
			f.MarkOffline(targetTmux)
			f.HaveGroup("resume-" + tc.name + "-team")
			f.HaveMember("resume-"+tc.name+"-team", target)

			serve := func() *httptest.ResponseRecorder {
				req := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost,
					"/v1/agent/"+target+"/resume", nil), caller)
				req.Header.Set("X-Tclaude-Ask-Human", tc.timeout)
				return testharness.Serve(f.Mux, req)
			}
			var response *httptest.ResponseRecorder
			if tc.decision == "" {
				response = serve()
			} else {
				result := make(chan *httptest.ResponseRecorder, 1)
				go func() { result <- serve() }()
				dashboard := agentd.BuildDashboardHandlerForTest()
				pendingID := ""
				require.Eventually(t, func() bool {
					snap := fetchAccessReqSnapshot(t, dashboard)
					for _, request := range snap.AccessRequests {
						if request.Status == db.AccessRequestStatusPending && request.Perm == agentd.PermAgentResume {
							pendingID = request.ID
							return true
						}
					}
					return false
				}, 10*time.Second, 10*time.Millisecond)
				decision := testharness.Serve(dashboard, testharness.JSONRequest(t, http.MethodPost,
					"/api/access-requests/"+pendingID+"/decision", map[string]any{"decision": tc.decision}))
				require.Equal(t, http.StatusOK, decision.Code, "decision body=%s", decision.Body.String())
				response = <-result
			}

			require.Equal(t, http.StatusForbidden, response.Code, "body=%s", response.Body.String())
			_, launched := f.World.SpawnCwdWriteProof(target)
			assert.False(t, launched, "denied or timed-out resume must not launch")
			assert.False(t, f.World.Tmux.IsAlive(targetTmux), "target must remain stopped")
			rows, err := db.ListRecentHandledAccessRequests(10)
			require.NoError(t, err)
			require.NotEmpty(t, rows)
			assert.Equal(t, tc.status, rows[0].Status)
		})
	}
}

func TestPermittedResumeRecoversMissingProfileWithoutSecondApproval(t *testing.T) {
	f := newFlow(t)
	const (
		target    = "missing-row-target-111111111111"
		caller    = "missing-row-owner-111111111111"
		sessionID = "missing-row-session"
		tmuxName  = "missing-row-tmux"
	)
	cwd := f.TestCwd("missing-row")
	f.HaveConvWithTitle(target, "missing-row-target")
	f.HaveAliveSession(target, sessionID, tmuxName, cwd)
	f.MarkOffline(tmuxName)
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID: target, ProjectPath: cwd, CustomTitle: "missing-row-target", IndexedAt: time.Now(),
	}))
	group := f.HaveGroup("missing-row-team")
	f.HaveMember(group.Name, target)
	require.NoError(t, db.AddAgentGroupOwner(group.ID, caller, "test"))
	require.NoError(t, db.DeleteSession(sessionID))
	database, err := db.Open()
	require.NoError(t, err)
	_, err = database.Exec(`DELETE FROM conversation_resume_profiles WHERE conv_id = ?`, target)
	require.NoError(t, err)

	// Group ownership authorizes the resume verb. Recovering durable launch
	// inputs from the real harness conversation does not require another popup.
	plainReq := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/agent/"+target+"/resume", nil), caller)
	plain := testharness.Serve(f.Mux, plainReq)
	require.Equal(t, http.StatusOK, plain.Code, "body=%s", plain.Body.String())
	assert.Contains(t, plain.Body.String(), `"action":"resumed"`)
	rows, err := db.FindSessionsByConvID(target)
	require.NoError(t, err)
	require.NotEmpty(t, rows, "the successful resume creates a new process row")
	profile, err := db.ConversationResumeProfileForConv(target)
	require.NoError(t, err)
	require.NotNil(t, profile)
	_, launched := f.World.SpawnCwdWriteProof(target)
	assert.True(t, launched)
}

// Scenario: a peer agent with no slug, not a group owner, calls a
// cross-agent endpoint WITHOUT the X-Tclaude-Ask-Human header. The
// daemon must refuse with 403 — the slug + ownership are the only
// silent paths.
//
// Pins the baseline so the popup-approval test below proves the
// escape hatch is what flips the decision, not some other accidental
// auth bypass.
func TestCrossAgentAskHuman_NoHeaderStillRefuses(t *testing.T) {
	f := newFlow(t)

	const targetConv = "tttt-1111-2222-3333-4444"
	const targetLabel = "spwn-tt001"
	const targetTmux = "tclaude-spwn-tt001"
	f.HaveConvWithTitle(targetConv, "worker")
	f.HaveAliveSession(targetConv, targetLabel, targetTmux, f.World.HomeDir)
	f.HaveGroup("alpha")
	f.HaveMember("alpha", targetConv)

	// A separate caller conv that is NOT in the group and holds no
	// agent.reincarnate slug. The default newFlow human path bypasses
	// permissions, so we route as an agent peer.
	const callerConv = "cccc-1111-2222-3333-4444"
	f.HaveMember("alpha", callerConv)
	f.HaveAliveSession(callerConv, "caller-open", "tmux-caller-open", f.World.HomeDir)
	callerSession, err := db.LoadSession("caller-open")
	require.NoError(t, err)
	callerSession.Harness = harness.DefaultName
	callerSession.HarnessBuiltinMode = harness.ClaudeSandboxOff
	require.NoError(t, db.SaveSession(callerSession))
	r := testharness.JSONRequest(t, http.MethodPost,
		"/v1/agent/"+targetConv+"/reincarnate", map[string]any{})
	r = agentd.AsAgentPeer(r, callerConv)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusForbidden, rec.Code,
		"expected 403 without slug + without --ask-human, body=%s", rec.Body.String())
}

// Scenario: same caller, same denied paths, BUT the caller adds
// X-Tclaude-Ask-Human: 30s. Popup is stubbed to APPROVE — the
// reincarnate orchestration runs and returns 200.
//
// Real surface assertion: after approval, the caller is recorded on
// the new conv's `granted_by` audit columns (system:reincarnate:by=
// <caller>) — same forensic trail cross-agent calls leave when the
// silent paths grant. Verifies the popup branch returns the caller's
// conv-id, not the human's empty string, so the orchestration can
// stamp the right audit.
func TestCrossAgentAskHuman_HeaderAndApprovalAllowsCall(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)
	stubApproval(t, true)

	f := newFlow(t)

	const targetConv = "tttt-1111-2222-3333-4444"
	const targetLabel = "spwn-tt001"
	const targetTmux = "tclaude-spwn-tt001"
	f.HaveConvWithTitle(targetConv, "worker")
	f.HaveAliveSession(targetConv, targetLabel, targetTmux, f.World.HomeDir)
	f.HaveGroup("alpha")
	f.HaveMember("alpha", targetConv)

	const callerConv = "cccc-1111-2222-3333-4444"
	r := testharness.JSONRequest(t, http.MethodPost,
		"/v1/agent/"+targetConv+"/reincarnate",
		map[string]any{"follow_up": "fresh start"})
	r.Header.Set("X-Tclaude-Ask-Human", "30s")
	r = agentd.AsAgentPeer(r, callerConv)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code,
		"expected 200 immediately after lifecycle approval, body=%s", rec.Body.String())
	// reincarnate orchestration ran: there should be a succession row
	// from the old conv to a new one.
	successor, err := db.GetConvSuccessor(targetConv)
	require.NoError(t, err, "GetConvSuccessor")
	require.NotEmpty(t, successor, "reincarnate did not record a successor; the orchestration was not actually run")
}

// A title selector is resolved independently for every approved request. If
// the title moves to another agent, a later request must ask again rather than
// treating the first target's one-shot approval as authority over the second.
func TestCrossAgentAskHuman_RetargetedSelectorAsksAgain(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	approvalCalls, restoreApproval := agentd.StubCountingApprovalForTest(true)
	t.Cleanup(restoreApproval)

	f := newFlow(t)
	const (
		targetA  = "target-a-1111-2222-3333-444444444444"
		targetB  = "target-b-1111-2222-3333-444444444444"
		caller   = "caller-r-1111-2222-3333-444444444444"
		selector = "moving-target"
	)
	f.HaveConvWithTitle(targetA, selector)
	f.HaveConvWithTitle(targetB, "standby-target")
	f.HaveAliveSession(targetA, "target-a-session", "target-a-tmux", f.World.HomeDir)
	f.HaveAliveSession(targetB, "target-b-session", "target-b-tmux", f.World.HomeDir)
	f.HaveGroup("alpha")
	f.HaveMember("alpha", targetA)
	f.HaveMember("alpha", targetB)
	f.HaveMember("alpha", caller)

	body := map[string]any{"follow_up": "fresh start"}
	path := "/v1/agent/" + selector + "/reincarnate"
	firstReq := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, path, body), caller)
	firstReq.Header.Set("X-Tclaude-Ask-Human", "30s")
	first := testharness.Serve(f.Mux, firstReq)
	require.Equalf(t, http.StatusOK, first.Code, "first approved request; body=%s", first.Body.String())
	require.Equal(t, int32(1), approvalCalls(), "initial target should be approved once")
	successorA, err := db.GetConvSuccessor(targetA)
	require.NoError(t, err)
	require.NotEmpty(t, successorA)

	// Retarget the exact route string for a new request.
	require.NoError(t, db.SetConvIndexCustomTitle(targetA, "former-target", harness.DefaultName))
	require.NoError(t, db.SetConvIndexCustomTitle(successorA, "former-target", harness.DefaultName))
	require.NoError(t, db.SetConvIndexCustomTitle(targetB, selector, harness.DefaultName))
	secondReq := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, path, body), caller)
	secondReq.Header.Set("X-Tclaude-Ask-Human", "30s")
	second := testharness.Serve(f.Mux, secondReq)
	require.Equalf(t, http.StatusOK, second.Code, "retargeted request may proceed only after fresh approval; body=%s", second.Body.String())
	require.Equal(t, int32(2), approvalCalls(), "newly resolved target must require its own approval")

	successorB, err := db.GetConvSuccessor(targetB)
	require.NoError(t, err)
	require.NotEmpty(t, successorB, "second request should operate on the newly resolved target after approval")
}

// Same setup but popup DENIES. The cross-agent call must still
// return 403 — the popup is an escape hatch, not a free pass.
func TestCrossAgentAskHuman_HeaderAndDenialStillRefuses(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)
	stubApproval(t, false)

	f := newFlow(t)

	const targetConv = "tttt-1111-2222-3333-4444"
	const targetLabel = "spwn-tt001"
	const targetTmux = "tclaude-spwn-tt001"
	f.HaveConvWithTitle(targetConv, "worker")
	f.HaveAliveSession(targetConv, targetLabel, targetTmux, f.World.HomeDir)
	f.HaveGroup("alpha")
	f.HaveMember("alpha", targetConv)

	const callerConv = "cccc-1111-2222-3333-4444"
	r := testharness.JSONRequest(t, http.MethodPost,
		"/v1/agent/"+targetConv+"/reincarnate", map[string]any{})
	r.Header.Set("X-Tclaude-Ask-Human", "30s")
	r = agentd.AsAgentPeer(r, callerConv)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusForbidden, rec.Code,
		"expected 403 after popup-deny, body=%s", rec.Body.String())
	// And no succession row was written.
	successor, _ := db.GetConvSuccessor(targetConv)
	assert.Empty(t, successor, "reincarnate ran despite popup-deny — successor %s recorded", successor)
}
