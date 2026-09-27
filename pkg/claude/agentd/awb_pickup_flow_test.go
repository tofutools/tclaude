package agentd_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// awb_pickup_flow_test.go drives `tclaude pickup`'s daemon surface — the
// operator's status view of the AWB ready pollers and the reset that unblocks
// one — through the real mux, with only the outbound AWB HTTP stubbed.

func pickupWorld(t *testing.T) (*testharness.Flow, *awbRecorder) {
	t.Helper()
	f := newFlow(t)
	f.HaveGroup("builders")
	cwd := t.TempDir()
	polling := func(workspace string) config.AWBReadyPollingConfig {
		return config.AWBReadyPollingConfig{Workspace: workspace, Group: "builders", Cwd: cwd, MonitorPR: true}
	}
	cfg := &config.Config{Agent: &config.AgentConfig{AWBProxy: &config.AWBProxyConfig{
		URL: "https://awb.example", Username: "tclaude-bot", AllowWrite: true,
		AllowedWorkspaces: []string{"tcl", "web", "ops", "doc"},
		ReadyPolling: map[string]config.AWBReadyPollingConfig{
			"alpha": polling("tcl"), "beta": polling("web"), "gamma": polling("ops"), "delta": polling("doc"),
		},
	}}}
	require.NoError(t, config.Save(cfg))
	t.Cleanup(agentd.RegisterAWBReadyProcessesForTest(cfg))

	rec := &awbRecorder{response: func(req agentd.AWBProxyRequest) (int, string) {
		issues := map[string]string{
			"/api/issues/tcl-1": `{"id":"tcl-1","workspace":"tcl","title":"Working issue","status":"in_progress","assignees":["tclaude-bot"]}`,
			"/api/issues/web-2": `{"id":"web-2","workspace":"web","title":"Abandoned issue","status":"in_progress"}`,
			"/api/issues/doc-4": `{"id":"doc-4","workspace":"doc","title":"Merged","status":"open","pull_request_url":"https://github.com/o/r/pull/1"}`,
		}
		for path, body := range issues {
			if strings.HasSuffix(req.URL, path) {
				return http.StatusOK, body
			}
		}
		return http.StatusNotFound, `{"error":"not found"}`
	}}
	t.Cleanup(agentd.SetAWBTransportForTest(rec.do))
	t.Setenv("AWB_PASSWORD", "hunter2")
	return f, rec
}

func haveDispatch(t *testing.T, process, workspace, issue, convID string) {
	t.Helper()
	agentID := "agt_missing_" + process
	if convID != "" {
		id, err := db.AgentIDForConv(convID)
		require.NoError(t, err)
		agentID = id
	}
	selected, err := db.SelectAWBReadyDispatch(process, workspace, issue, agentID)
	require.NoError(t, err)
	require.True(t, selected)
	_, err = db.UpdateAWBReadyDispatch(process, issue, "spawned", "")
	require.NoError(t, err)
}

func pickupList(t *testing.T, f *testharness.Flow) map[string]agent.AWBPickupProcess {
	t.Helper()
	res := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/pickup", nil)))
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	var list agent.AWBPickupList
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &list))
	out := map[string]agent.AWBPickupProcess{}
	for _, p := range list.Processes {
		out[p.Process] = p
	}
	return out
}

func pickupReset(t *testing.T, f *testharness.Flow, process, issue string) *httptest.ResponseRecorder {
	t.Helper()
	return testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/pickup/"+process+"/reset", agent.AWBPickupResetRequest{IssueID: issue})))
}

func TestAWBPickup_ListJoinsDispatchWithLiveIssueAndAgent(t *testing.T) {
	f, _ := pickupWorld(t)
	f.HaveConvWithTitle("conv-alpha", "alpha-worker")
	f.HaveEnrolledAgent("conv-alpha")
	f.HaveAliveSession("conv-alpha", "alpha", "tmux-alpha", f.TestCwd("alpha"))
	f.SetSessionStatus("conv-alpha", session.StatusWorking)
	haveDispatch(t, "alpha", "tcl", "tcl-1", "conv-alpha")

	f.HaveRetiredAgent("conv-beta")
	haveDispatch(t, "beta", "web", "web-2", "conv-beta")

	f.HaveEnrolledAgent("conv-delta")
	f.HaveAliveSession("conv-delta", "delta", "tmux-delta", f.TestCwd("delta"))
	f.SetSessionStatus("conv-delta", session.StatusIdle)
	haveDispatch(t, "delta", "doc", "doc-4", "conv-delta")

	agentd.RecordAWBReadyPollForTest("gamma", errors.New("group \"builders\" does not exist"))

	// A dispatch left behind by a process that is no longer configured.
	selected, err := db.SelectAWBReadyDispatch("retired-proc", "tcl", "tcl-9", "agt_gone")
	require.NoError(t, err)
	require.True(t, selected)

	procs := pickupList(t, f)
	require.Len(t, procs, 5)

	alpha := procs["alpha"]
	assert.Equal(t, agent.AWBPickupStateWorking, alpha.State)
	require.NotNil(t, alpha.Dispatch)
	require.NotNil(t, alpha.Dispatch.Issue)
	assert.Equal(t, "in_progress", alpha.Dispatch.Issue.Status)
	assert.Equal(t, "Working issue", alpha.Dispatch.Issue.Title)
	assert.Equal(t, "https://awb.example/#/issues/tcl-1", alpha.Dispatch.Issue.URL)
	require.NotNil(t, alpha.Dispatch.Agent)
	assert.True(t, alpha.Dispatch.Agent.Exists)
	assert.Equal(t, "alpha-worker", alpha.Dispatch.Agent.Name)
	assert.Equal(t, session.StatusWorking, alpha.Dispatch.Agent.SessionStatus)

	beta := procs["beta"]
	assert.Equal(t, agent.AWBPickupStateStuck, beta.State)
	assert.Contains(t, beta.Hint, "retired")
	assert.True(t, beta.Dispatch.Agent.Retired)

	assert.Equal(t, agent.AWBPickupStateError, procs["gamma"].State)
	assert.Contains(t, procs["gamma"].Hint, "does not exist")
	assert.Nil(t, procs["gamma"].Dispatch)

	assert.Equal(t, agent.AWBPickupStateAwaiting, procs["delta"].State,
		"an idle agent with a recorded PR is waiting for the merge, not stuck")

	orphan := procs["retired-proc"]
	assert.False(t, orphan.Configured)
	assert.Equal(t, agent.AWBPickupStateOrphaned, orphan.State)
	assert.Nil(t, orphan.Dispatch.Issue, "an unconfigured process has no AWB session to read the issue with")
}

func TestAWBPickup_ResetReleasesOnlyTheNamedProcess(t *testing.T) {
	f, _ := pickupWorld(t)
	f.HaveRetiredAgent("conv-beta")
	haveDispatch(t, "beta", "web", "web-2", "conv-beta")
	haveDispatch(t, "alpha", "tcl", "tcl-1", "")

	res := pickupReset(t, f, "beta", "web-other")
	assert.Equal(t, http.StatusConflict, res.Code, "a stale issue guard must not reset the process")

	res = pickupReset(t, f, "beta", "web-2")
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	var out agent.AWBPickupResetResponse
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	assert.True(t, out.Reset)
	require.NotNil(t, out.Dispatch)
	assert.Equal(t, "web-2", out.Dispatch.IssueID)
	audits, err := db.ListAuditLog(db.AuditLogFilter{Verb: "pickup.reset", Outcome: "success"})
	require.NoError(t, err)
	require.Len(t, audits, 1, "an operator reset is recorded in the audit trail")
	assert.Equal(t, "beta", audits[0].TargetLabel)
	assert.Contains(t, audits[0].Detail, "issue=web-2")

	procs := pickupList(t, f)
	assert.Equal(t, agent.AWBPickupStatePolling, procs["beta"].State)
	assert.Nil(t, procs["beta"].Dispatch)
	require.NotNil(t, procs["alpha"].Dispatch, "other processes keep their dispatch")

	res = pickupReset(t, f, "beta", "")
	require.Equal(t, http.StatusOK, res.Code)
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	assert.False(t, out.Reset, "resetting an idle process is a no-op")

	res = pickupReset(t, f, "nope", "")
	assert.Equal(t, http.StatusNotFound, res.Code)

	// The orphan row of an unconfigured process can be cleared too.
	selected, err := db.SelectAWBReadyDispatch("retired-proc", "tcl", "tcl-9", "agt_gone")
	require.NoError(t, err)
	require.True(t, selected)
	res = pickupReset(t, f, "retired-proc", "")
	require.Equal(t, http.StatusOK, res.Code)
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	assert.True(t, out.Reset)
}

func TestAWBPickup_AgentsCannotInspectOrReset(t *testing.T) {
	f, rec := pickupWorld(t)
	f.HaveEnrolledAgent("conv-agent")
	haveDispatch(t, "alpha", "tcl", "tcl-1", "")

	res := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodGet, "/v1/pickup", nil), "conv-agent"))
	assert.Equal(t, http.StatusForbidden, res.Code)
	res = testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost, "/v1/pickup/alpha/reset", nil), "conv-agent"))
	assert.Equal(t, http.StatusForbidden, res.Code)

	d, err := db.GetAWBReadyDispatch("alpha")
	require.NoError(t, err)
	assert.NotNil(t, d, "a refused reset leaves the dispatch in place")
	assert.False(t, rec.sawAnyCall(), "a refused caller never reaches AWB")
}

func TestAWBPickup_ResetWaitsForAnInProgressPoll(t *testing.T) {
	f, _ := pickupWorld(t)
	haveDispatch(t, "alpha", "tcl", "tcl-1", "")

	release := agentd.HoldAWBReadyProcessForTest("alpha", 50*time.Millisecond)
	res := pickupReset(t, f, "alpha", "")
	assert.Equal(t, http.StatusConflict, res.Code, "a reset must not race a poll that is claiming or spawning")
	assert.Contains(t, res.Body.String(), "busy")
	d, err := db.GetAWBReadyDispatch("alpha")
	require.NoError(t, err)
	require.NotNil(t, d, "the refused reset leaves the dispatch in place")

	release()
	res = pickupReset(t, f, "alpha", "")
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
}

func TestAWBPickup_DeadTmuxSessionIsStuckWhateverTheStoredStatus(t *testing.T) {
	f, _ := pickupWorld(t)
	f.HaveEnrolledAgent("conv-alpha")
	f.HaveAliveSession("conv-alpha", "alpha", "tmux-alpha", f.TestCwd("alpha"))
	f.SetSessionStatus("conv-alpha", session.StatusWorking)
	haveDispatch(t, "alpha", "tcl", "tcl-1", "conv-alpha")
	require.Equal(t, agent.AWBPickupStateWorking, pickupList(t, f)["alpha"].State)

	f.MarkOffline("tmux-alpha")
	alpha := pickupList(t, f)["alpha"]
	assert.Equal(t, agent.AWBPickupStateStuck, alpha.State,
		"a pane that died without a final hook must not look healthy")
	assert.Equal(t, session.StatusExited, alpha.Dispatch.Agent.SessionStatus)
}
