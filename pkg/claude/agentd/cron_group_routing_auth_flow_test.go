package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"strconv"
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

func TestCronPatchRoutingGroup_UsesCanonicalGroupAuthority(t *testing.T) {
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
			const caller = "crgr-authorized-aaaa-bbbb-cccc-000000000001"
			job := createSelfManagedCron(t, f, caller)
			g := f.HaveGroup("authorized-route")
			tc.grant(t, f, g, caller)

			rec := patchCronAsAgent(t, f, caller, job.ID, map[string]any{
				"group_id": g.ID, "subject": "authorized-route",
			})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			after, err := db.GetAgentCronJob(job.ID)
			require.NoError(t, err)
			assert.False(t, after.IsGroupTarget(), "routing metadata changed the execution-target kind")
			assert.Equal(t, caller, after.TargetConv)
			assert.Equal(t, g.ID, after.GroupID)
			assert.Equal(t, "authorized-route", after.Subject)
		})
	}

	t.Run("cross-group route is denied before any mutation or fire", func(t *testing.T) {
		f := newFlow(t)
		const caller = "crgr-foreign-aaaa-bbbb-cccc-000000000001"
		job := createSelfManagedCron(t, f, caller)
		foreign := f.HaveGroup("foreign-route")
		before, err := db.GetAgentCronJob(job.ID)
		require.NoError(t, err)

		rec := patchCronAsAgent(t, f, caller, job.ID, map[string]any{
			"group_id": foreign.ID, "name": "after", "subject": "after-subject",
			"body": "after-body", "interval": "30s", "enabled": true,
			"run_immediately": true, "queue_when_offline": false,
		})
		assertDeniedCronRetargetHasNoSideEffects(t, f, before, rec, caller)
	})

	t.Run("explicit current-target deny is not bypassed by group authority", func(t *testing.T) {
		f := newFlow(t)
		const caller = "crgr-denied-aaaa-bbbb-cccc-000000000001"
		job := createSelfManagedCron(t, f, caller)
		g := f.HaveGroup("member-route")
		f.HaveMember(g.Name, caller)
		require.NoError(t, db.SetAgentPermissionOverride(
			caller, agentd.PermSelfSchedule, db.PermEffectDeny, "test"))
		before, err := db.GetAgentCronJob(job.ID)
		require.NoError(t, err)

		rec := patchCronAsAgent(t, f, caller, job.ID, map[string]any{
			"group_id": g.ID, "body": "must-not-land",
		})
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		after, err := db.GetAgentCronJob(job.ID)
		require.NoError(t, err)
		assert.Equal(t, before, after)
		runs, err := db.ListAgentCronRunsForJob(job.ID, 0)
		require.NoError(t, err)
		assert.Empty(t, runs)
		assert.Zero(t, msgRowCount(t, caller))
	})
}

func TestCronPatchRoutingGroup_TargetNormalizationCannotHideRawRouteChange(t *testing.T) {
	f := newFlow(t)
	const caller = "crgr-normalize-aaaa-bbbb-cccc-000000000001"
	job := createSelfManagedCron(t, f, caller)
	foreign := f.HaveGroup("normalize-foreign")
	before, err := db.GetAgentCronJob(job.ID)
	require.NoError(t, err)

	// The target title resolves canonically to the stored actor. The sibling raw
	// group_id is still a distinct routing mutation and must take its own gate.
	rec := patchCronAsAgent(t, f, caller, job.ID, map[string]any{
		"target": "cron-caller", "group_id": foreign.ID,
		"body": "must-not-land", "enabled": true, "run_immediately": true,
	})
	assertDeniedCronRetargetHasNoSideEffects(t, f, before, rec, caller)
}

func TestCronPatchRoutingGroup_UnchangedCanonicalMetadataNeedsNoFreshAuthority(t *testing.T) {
	f := newFlow(t)
	const caller = "crgr-unchanged-aaaa-bbbb-cccc-000000000001"
	f.HaveConvWithTitle(caller, "unchanged-caller")
	f.HaveEnrolledAgent(caller)
	require.NoError(t, db.SetAgentPermissionOverride(
		caller, agentd.PermSelfSchedule, db.PermEffectGrant, "test"))
	foreign := f.HaveGroup("unchanged-foreign")
	job := createCronAsHuman(t, f, map[string]any{
		"owner": caller, "target": caller, "group_id": foreign.ID,
		"interval": "1h", "body": "before", "enabled": false,
	})
	agentID, err := db.AgentIDForConv(caller)
	require.NoError(t, err)
	require.NotEmpty(t, agentID)

	// The stable agent id and stored conv id are the same execution actor, and
	// the raw routing group is unchanged. Compatibility requires no redundant
	// authority over an existing metadata value.
	rec := patchCronAsAgent(t, f, caller, job.ID, map[string]any{
		"target": agentID, "group_id": foreign.ID, "body": "after",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	after, err := db.GetAgentCronJob(job.ID)
	require.NoError(t, err)
	assert.Equal(t, caller, after.TargetConv)
	assert.Equal(t, foreign.ID, after.GroupID)
	assert.Equal(t, "after", after.Body)
}

func TestCronPatchRoutingGroup_MissingArchivedAndUnauthorizedAreNonEnumerating(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	approvalCalls, restoreApproval := agentd.StubCountingApprovalForTest(true)
	t.Cleanup(restoreApproval)

	var denialBodies []string
	for _, kind := range []string{"unauthorized", "missing", "archived"} {
		t.Run(kind, func(t *testing.T) {
			f := newFlow(t)
			const caller = "crgr-hidden-aaaa-bbbb-cccc-000000000001"
			job := createSelfManagedCron(t, f, caller)
			g := f.HaveGroup("hidden-" + kind)
			groupID := g.ID
			switch kind {
			case "missing":
				require.NoError(t, db.DeleteAgentGroup(g.Name))
			case "archived":
				f.HaveMember(g.Name, caller)
				require.NoError(t, db.ArchiveAgentGroup(g.Name))
			}
			before, err := db.GetAgentCronJob(job.ID)
			require.NoError(t, err)

			req := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPatch,
				"/v1/cron/"+strconv.FormatInt(job.ID, 10), map[string]any{
					"group_id": groupID, "body": "must-not-land",
					"enabled": true, "run_immediately": true,
				}), caller)
			req.Header.Set("X-Tclaude-Ask-Human", "5s")
			result := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				result <- testharness.Serve(f.Mux, req)
			}()
			var rec *httptest.ResponseRecorder
			select {
			case rec = <-result:
			case <-time.After(time.Second):
				t.Fatal("routing-group denial waited for interactive approval under cron authority")
			}
			assertDeniedCronRetargetHasNoSideEffects(t, f, before, rec, caller)
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

func TestCronPatchRoutingGroup_RefreshesStoredRouteBeforeUnchangedDecision(t *testing.T) {
	f := newFlow(t)
	const caller = "crgr-race-aaaa-bbbb-cccc-000000000001"
	f.HaveConvWithTitle(caller, "race-caller")
	f.HaveEnrolledAgent(caller)
	require.NoError(t, db.SetAgentPermissionOverride(
		caller, agentd.PermSelfSchedule, db.PermEffectGrant, "test"))
	allowed := f.HaveGroup("race-allowed")
	foreign := f.HaveGroup("race-foreign")
	f.HaveMember(allowed.Name, caller)
	job := createCronAsHuman(t, f, map[string]any{
		"owner": caller, "target": caller, "group_id": foreign.ID,
		"interval": "1h", "body": "before", "enabled": false,
	})

	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var hookCalls atomic.Int32
	t.Cleanup(agentd.SetCronBeforeAuthorityLockForTest(func(operation string) {
		if operation == "patch" && hookCalls.Add(1) == 1 {
			close(entered)
			<-release
		}
	}))

	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		result <- patchCronAsAgent(t, f, caller, job.ID, map[string]any{
			"group_id": foreign.ID, "body": "stale-route-write",
		})
	}()
	<-entered
	patchCronAsHuman(t, f, job.ID, map[string]any{"group_id": allowed.ID})
	afterHuman, err := db.GetAgentCronJob(job.ID)
	require.NoError(t, err)
	releaseOnce.Do(func() { close(release) })

	rec := <-result
	requireCronRetargetDenied(t, rec)
	after, err := db.GetAgentCronJob(job.ID)
	require.NoError(t, err)
	assert.Equal(t, afterHuman, after, "stale unchanged decision overwrote the refreshed route")
	assert.Equal(t, allowed.ID, after.GroupID)
	assert.Equal(t, "before", after.Body)
	assert.Zero(t, msgRowCount(t, caller))
}

func TestDashboardCronPatchRoutingGroup_HumanUsesSharedHandler(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	const owner = "crgr-dashboard-aaaa-bbbb-cccc-000000000001"
	f.HaveConvWithTitle(owner, "dashboard-routing-owner")
	job := createCronAsHuman(t, f, map[string]any{
		"owner": owner, "target": owner, "interval": "1h",
		"body": "before", "enabled": false,
	})
	g := f.HaveGroup("dashboard-routing-group")

	rec := testharness.Serve(agentd.BuildDashboardHandlerForTest(),
		testharness.JSONRequest(t, http.MethodPatch,
			"/api/cron/"+strconv.FormatInt(job.ID, 10), map[string]any{"group_id": g.ID}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	after, err := db.GetAgentCronJob(job.ID)
	require.NoError(t, err)
	assert.Equal(t, g.ID, after.GroupID)
}

func TestCronPatchRoutingGroup_HumanGetsPreciseMissingAndArchivedDiagnostics(t *testing.T) {
	for _, kind := range []string{"missing", "archived"} {
		t.Run(kind, func(t *testing.T) {
			f := newFlow(t)
			const owner = "crgr-human-aaaa-bbbb-cccc-000000000001"
			f.HaveConvWithTitle(owner, "human-routing-owner")
			job := createCronAsHuman(t, f, map[string]any{
				"owner": owner, "target": owner, "interval": "1h",
				"body": "before", "enabled": false,
			})
			g := f.HaveGroup("human-" + kind)
			if kind == "missing" {
				require.NoError(t, db.DeleteAgentGroup(g.Name))
			} else {
				require.NoError(t, db.ArchiveAgentGroup(g.Name))
			}
			before, err := db.GetAgentCronJob(job.ID)
			require.NoError(t, err)

			rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t,
				http.MethodPatch, "/v1/cron/"+strconv.FormatInt(job.ID, 10),
				map[string]any{"group_id": g.ID})))
			if kind == "missing" {
				require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
				assert.Contains(t, rec.Body.String(), "resolve routing group")
			} else {
				require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
				assert.Contains(t, rec.Body.String(), "archived")
			}
			after, err := db.GetAgentCronJob(job.ID)
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
}
