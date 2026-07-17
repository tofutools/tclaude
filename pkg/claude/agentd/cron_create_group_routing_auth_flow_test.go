package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func postCronCreateAsAgent(t *testing.T, f *testharness.Flow, caller string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost, "/v1/cron", body), caller))
}

func requireCronCreateRoutingDenied(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	var body struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	testharness.DecodeJSON(t, rec, &body)
	assert.Equal(t, "permission", body.Code)
	assert.Equal(t, cronRetargetDeniedMessage, body.Error)
}

func assertDeniedCronCreateHasNoSideEffects(t *testing.T, f *testharness.Flow, rec *httptest.ResponseRecorder, destinations ...string) {
	t.Helper()
	requireCronCreateRoutingDenied(t, rec)
	jobs, err := db.ListAgentCronJobs()
	require.NoError(t, err)
	assert.Empty(t, jobs, "denial inserted a cron row")
	for _, destination := range destinations {
		assert.Zero(t, msgRowCount(t, destination), "denial delivered to %s", destination)
	}
	rows, err := db.ListAuditLog(db.AuditLogFilter{Verb: "cron.add", Outcome: "success"})
	require.NoError(t, err)
	assert.Empty(t, rows, "denial was recorded as a successful cron create")
	rows, err = db.ListAuditLog(db.AuditLogFilter{Verb: "cron.add", Outcome: "failure"})
	require.NoError(t, err)
	require.Len(t, rows, 1, "denial must retain its failed-attempt audit record")
	assert.Equal(t, http.StatusForbidden, rows[0].Status)
}

func prepareCronRoutingCaller(t *testing.T, f *testharness.Flow, caller, title string) {
	t.Helper()
	f.HaveConvWithTitle(caller, title)
	f.HaveEnrolledAgent(caller)
	require.NoError(t, db.SetAgentPermissionOverride(
		caller, agentd.PermSelfSchedule, db.PermEffectGrant, "test"))
}

func TestCronCreateRoutingGroup_UsesCanonicalGroupAuthority(t *testing.T) {
	for _, tc := range []struct {
		name  string
		grant func(*testing.T, *testharness.Flow, *db.AgentGroup, string)
	}{
		{
			name: "shared group membership",
			grant: func(t *testing.T, f *testharness.Flow, g *db.AgentGroup, caller string) {
				f.HaveMember(g.Name, caller)
			},
		},
		{
			name: "group ownership",
			grant: func(t *testing.T, _ *testharness.Flow, g *db.AgentGroup, caller string) {
				require.NoError(t, db.AddAgentGroupOwner(g.ID, caller, "test"))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlow(t)
			const caller = "crcg-authorized-aaaa-bbbb-cccc-000000000001"
			prepareCronRoutingCaller(t, f, caller, "create-route-caller")
			g := f.HaveGroup("authorized-create-route")
			tc.grant(t, f, g, caller)

			rec := postCronCreateAsAgent(t, f, caller, map[string]any{
				"target": "create-route-caller", "group_id": g.ID,
				"interval": "1h", "body": "authorized", "enabled": false,
			})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			jobs, err := db.ListAgentCronJobs()
			require.NoError(t, err)
			require.Len(t, jobs, 1)
			assert.False(t, jobs[0].IsGroupTarget())
			assert.Equal(t, caller, jobs[0].TargetConv)
			assert.Equal(t, g.ID, jobs[0].GroupID)
		})
	}
}

func TestCronCreateRoutingGroup_CanonicalTargetCannotHideForeignRawRoute(t *testing.T) {
	f := newFlow(t)
	const caller = "crcg-normalize-aaaa-bbbb-cccc-000000000001"
	prepareCronRoutingCaller(t, f, caller, "normalized-create-caller")
	foreign := f.HaveGroup("foreign-create-route")
	agentID, err := db.AgentIDForConv(caller)
	require.NoError(t, err)
	require.NotEmpty(t, agentID)

	rec := postCronCreateAsAgent(t, f, caller, map[string]any{
		"target": agentID, "group_id": foreign.ID, "interval": "30s",
		"body": "must-not-land", "run_immediately": true, "queue_when_offline": true,
	})
	assertDeniedCronCreateHasNoSideEffects(t, f, rec, caller)
}

func TestCronCreateRoutingGroup_MissingArchivedAndUnauthorizedAreNonEnumeratingBeforeApproval(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	approvalCalls, restoreApproval := agentd.StubCountingApprovalForTest(true)
	t.Cleanup(restoreApproval)

	var denialBodies []string
	for _, kind := range []string{"unauthorized", "missing", "archived"} {
		t.Run(kind, func(t *testing.T) {
			f := newFlow(t)
			const caller = "crcg-hidden-aaaa-bbbb-cccc-000000000001"
			f.HaveConvWithTitle(caller, "hidden-create-caller")
			f.HaveEnrolledAgent(caller)
			// If target authorization runs first, this explicit deny plus the
			// ask-human header opens an approval. Invalid routing metadata must
			// reject before reaching that path.
			require.NoError(t, db.SetAgentPermissionOverride(
				caller, agentd.PermSelfSchedule, db.PermEffectDeny, "test"))
			g := f.HaveGroup("hidden-create-" + kind)
			groupID := g.ID
			switch kind {
			case "missing":
				require.NoError(t, db.DeleteAgentGroup(g.Name))
			case "archived":
				f.HaveMember(g.Name, caller)
				require.NoError(t, db.ArchiveAgentGroup(g.Name))
			}

			req := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost,
				"/v1/cron", map[string]any{
					"target": "hidden-create-caller", "group_id": groupID,
					"interval": "30s", "body": "must-not-land",
					"run_immediately": true, "queue_when_offline": true,
				}), caller)
			req.Header.Set("X-Tclaude-Ask-Human", "5s")
			result := make(chan *httptest.ResponseRecorder, 1)
			go func() { result <- testharness.Serve(f.Mux, req) }()
			var rec *httptest.ResponseRecorder
			select {
			case rec = <-result:
			case <-time.After(time.Second):
				t.Fatal("routing-group denial waited for current-target approval")
			}
			assertDeniedCronCreateHasNoSideEffects(t, f, rec, caller)
			denialBodies = append(denialBodies, rec.Body.String())

			rows, err := db.ListAuditLog(db.AuditLogFilter{Verb: "approval.request"})
			require.NoError(t, err)
			assert.Empty(t, rows)
			snapshot := fetchAccessReqSnapshot(t, agentd.BuildDashboardHandlerForTest())
			assert.Zero(t, snapshot.AccessRequestsPending)
			assert.Empty(t, snapshot.AccessRequests)
		})
	}
	require.Len(t, denialBodies, 3)
	assert.Equal(t, denialBodies[0], denialBodies[1])
	assert.Equal(t, denialBodies[0], denialBodies[2])
	assert.Zero(t, approvalCalls())
}

func TestCronCreateRoutingGroup_ReauthorizesAtomicallyBeforeImmediateInsertAndFire(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	approvalCalls, restoreApproval := agentd.StubCountingApprovalForTest(true)
	t.Cleanup(restoreApproval)
	f := newFlow(t)
	const caller = "crcg-race-aaaa-bbbb-cccc-000000000001"
	prepareCronRoutingCaller(t, f, caller, "race-create-caller")
	g := f.HaveGroup("race-create-route")
	f.HaveMember(g.Name, caller)

	var hookCalls atomic.Int32
	t.Cleanup(agentd.SetCronBeforeAuthorityLockForTest(func(operation string) {
		if operation == "create-immediate" && hookCalls.Add(1) == 1 {
			require.NoError(t, db.RemoveAgentGroupMember(g.ID, caller))
		}
	}))
	req := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/cron", map[string]any{
		"target": "race-create-caller", "group_id": g.ID, "interval": "30s",
		"body": "must-not-land", "run_immediately": true, "queue_when_offline": true,
	}), caller)
	req.Header.Set("X-Tclaude-Ask-Human", "5s")
	rec := testharness.Serve(f.Mux, req)

	assert.Equal(t, int32(1), hookCalls.Load())
	assertDeniedCronCreateHasNoSideEffects(t, f, rec, caller)
	assert.Zero(t, approvalCalls(), "locked reauthorization must remain non-interactive")
	rows, err := db.ListAuditLog(db.AuditLogFilter{Verb: "approval.request"})
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestCronCreateRoutingGroup_GroupTargetStillIgnoresSiblingRawOverride(t *testing.T) {
	f := newFlow(t)
	const caller = "crcg-group-aaaa-bbbb-cccc-000000000001"
	target := f.HaveGroup("create-group-target")
	foreign := f.HaveGroup("create-group-foreign")
	f.HaveMember(target.Name, caller)

	rec := postCronCreateAsAgent(t, f, caller, map[string]any{
		"target": "group:" + target.Name, "group_id": foreign.ID,
		"interval": "1h", "body": "group target", "enabled": false,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	jobs, err := db.ListAgentCronJobs()
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.True(t, jobs[0].IsGroupTarget())
	assert.Equal(t, target.ID, jobs[0].GroupID)
}

func TestDashboardCronCreateRoutingGroup_HumanKeepsPreciseDiagnostics(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	for _, kind := range []string{"missing", "archived"} {
		t.Run(kind, func(t *testing.T) {
			f := newFlow(t)
			const target = "crcg-dashboard-aaaa-bbbb-cccc-000000000001"
			f.HaveConvWithTitle(target, "dashboard-create-target")
			g := f.HaveGroup("dashboard-create-" + kind)
			if kind == "missing" {
				require.NoError(t, db.DeleteAgentGroup(g.Name))
			} else {
				require.NoError(t, db.ArchiveAgentGroup(g.Name))
			}

			rec := testharness.Serve(agentd.BuildDashboardHandlerForTest(),
				testharness.JSONRequest(t, http.MethodPost, "/api/cron", map[string]any{
					"target": target, "group_id": g.ID,
					"interval": "1h", "body": "must-not-land",
				}))
			if kind == "missing" {
				require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
				assert.Contains(t, rec.Body.String(), "resolve routing group")
			} else {
				require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
				assert.Contains(t, rec.Body.String(), "archived")
			}
			jobs, err := db.ListAgentCronJobs()
			require.NoError(t, err)
			assert.Empty(t, jobs)
		})
	}
}

func TestCronCreateRoutingGroup_ConcurrentRequestsDoNotShareAuthorizationState(t *testing.T) {
	f := newFlow(t)
	const caller = "crcg-parallel-aaaa-bbbb-cccc-000000000001"
	prepareCronRoutingCaller(t, f, caller, "parallel-create-caller")
	allowed := f.HaveGroup("parallel-create-allowed")
	foreign := f.HaveGroup("parallel-create-foreign")
	f.HaveMember(allowed.Name, caller)

	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for _, groupID := range []int64{allowed.ID, foreign.ID} {
		wg.Add(1)
		go func(groupID int64) {
			defer wg.Done()
			<-start
			results <- postCronCreateAsAgent(t, f, caller, map[string]any{
				"target": "parallel-create-caller", "group_id": groupID,
				"interval": "1h", "body": "parallel", "enabled": false,
			})
		}(groupID)
	}
	close(start)
	wg.Wait()
	close(results)
	var ok, denied int
	for rec := range results {
		switch rec.Code {
		case http.StatusOK:
			ok++
		case http.StatusForbidden:
			denied++
		default:
			t.Fatalf("unexpected response %d: %s", rec.Code, rec.Body.String())
		}
	}
	assert.Equal(t, 1, ok)
	assert.Equal(t, 1, denied)
	jobs, err := db.ListAgentCronJobs()
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, allowed.ID, jobs[0].GroupID)
	assert.NotEqual(t, foreign.ID, jobs[0].GroupID)
}
