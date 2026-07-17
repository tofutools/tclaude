package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

const cronRetargetDeniedMessage = "caller is not authorized to schedule the proposed cron target"

func patchCronAsAgent(t *testing.T, f *testharness.Flow, caller string, id int64, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(
		t, http.MethodPatch, "/v1/cron/"+strconv.FormatInt(id, 10), body), caller))
}

func requireCronRetargetDenied(t *testing.T, rec *httptest.ResponseRecorder) {
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

func createSelfManagedCron(t *testing.T, f *testharness.Flow, caller string) *db.AgentCronJob {
	t.Helper()
	f.HaveConvWithTitle(caller, "cron-caller")
	f.HaveEnrolledAgent(caller)
	require.NoError(t, db.SetAgentPermissionOverride(
		caller, agentd.PermSelfSchedule, db.PermEffectGrant, "test"))
	return createCronAsHuman(t, f, map[string]any{
		"owner": caller, "target": caller, "interval": "1h",
		"name": "before", "subject": "before-subject", "body": "before-body",
		"enabled": false, "run_immediately": false, "queue_when_offline": true,
	})
}

func assertDeniedCronRetargetHasNoSideEffects(t *testing.T, f *testharness.Flow, before *db.AgentCronJob, rec *httptest.ResponseRecorder, destinations ...string) {
	t.Helper()
	requireCronRetargetDenied(t, rec)
	after, err := db.GetAgentCronJob(before.ID)
	require.NoError(t, err)
	assert.Equal(t, before, after, "denial changed the stored cron row")
	runs, err := db.ListAgentCronRunsForJob(before.ID, 0)
	require.NoError(t, err)
	assert.Empty(t, runs, "denial recorded an immediate/scheduled run")
	for _, destination := range destinations {
		assert.Zero(t, msgRowCount(t, destination), "denial delivered to %s", destination)
	}
	rows, err := db.ListAuditLog(db.AuditLogFilter{Verb: "cron.update", Outcome: "success"})
	require.NoError(t, err)
	assert.Empty(t, rows, "denial was recorded as a successful cron update")
	rows, err = db.ListAuditLog(db.AuditLogFilter{Verb: "cron.update", Outcome: "failure"})
	require.NoError(t, err)
	require.Len(t, rows, 1, "denial must retain its failed-attempt audit record")
	assert.Equal(t, http.StatusForbidden, rows[0].Status)

	// The scheduler has no mutation wake channel; a future sweep is the only
	// scheduler-visible probe. The original disabled row must remain inert.
	agentd.RunCronTickForTest(before.CreatedAt.Add(24 * time.Hour))
	afterTick, err := db.GetAgentCronJob(before.ID)
	require.NoError(t, err)
	assert.Equal(t, before, afterTick)
	for _, destination := range destinations {
		assert.Zero(t, msgRowCount(t, destination), "denial became visible to scheduler for %s", destination)
	}
}

func TestCronPatchRetarget_DeniesUnauthorizedAgentWithoutMutationOrTargetLeak(t *testing.T) {
	f := newFlow(t)
	const caller = "crta-caller-aaaa-bbbb-cccc-000000000001"
	const proposed = "crta-secret-aaaa-bbbb-cccc-000000000002"
	job := createSelfManagedCron(t, f, caller)
	f.HaveConvWithTitle(proposed, "private-destination")
	f.HaveEnrolledAgent(proposed)
	before, err := db.GetAgentCronJob(job.ID)
	require.NoError(t, err)

	rec := patchCronAsAgent(t, f, caller, job.ID, map[string]any{
		"target": proposed, "name": "after", "subject": "after-subject",
		"body": "after-body", "interval": "30s", "enabled": true,
		"run_immediately": true, "queue_when_offline": false,
	})
	assertDeniedCronRetargetHasNoSideEffects(t, f, before, rec, caller, proposed)
	assert.NotContains(t, rec.Body.String(), proposed)
	assert.NotContains(t, rec.Body.String(), "private-destination")
}

func TestCronPatchRetarget_UsesCanonicalAgentAndGroupAuthority(t *testing.T) {
	t.Run("agent schedule grant allows cross-agent retarget", func(t *testing.T) {
		f := newFlow(t)
		const caller = "crtg-caller-aaaa-bbbb-cccc-000000000001"
		const proposed = "crtg-target-aaaa-bbbb-cccc-000000000002"
		job := createSelfManagedCron(t, f, caller)
		f.HaveConvWithTitle(proposed, "granted-target")
		f.HaveEnrolledAgent(proposed)
		require.NoError(t, db.SetAgentPermissionOverride(
			caller, agentd.PermAgentSchedule, db.PermEffectGrant, "test"))

		rec := patchCronAsAgent(t, f, caller, job.ID, map[string]any{"target": "granted-target"})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		after, err := db.GetAgentCronJob(job.ID)
		require.NoError(t, err)
		assert.Equal(t, proposed, after.TargetConv)
	})

	t.Run("group owner may retarget to a managed member", func(t *testing.T) {
		f := newFlow(t)
		const caller = "crta-owner-aaaa-bbbb-cccc-000000000001"
		const proposed = "crta-member-aaaa-bbbb-cccc-000000000002"
		job := createSelfManagedCron(t, f, caller)
		f.HaveConvWithTitle(proposed, "managed-target")
		g := f.HaveGroup("agent-managed")
		f.HaveMember(g.Name, proposed)
		require.NoError(t, db.AddAgentGroupOwner(g.ID, caller, "test"))

		rec := patchCronAsAgent(t, f, caller, job.ID, map[string]any{"target": proposed})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		after, err := db.GetAgentCronJob(job.ID)
		require.NoError(t, err)
		assert.Equal(t, proposed, after.TargetConv)
	})

	t.Run("shared group membership allows group retarget", func(t *testing.T) {
		f := newFlow(t)
		const caller = "crtm-caller-aaaa-bbbb-cccc-000000000001"
		job := createSelfManagedCron(t, f, caller)
		g := f.HaveGroup("shared-destination")
		f.HaveMember(g.Name, caller)

		rec := patchCronAsAgent(t, f, caller, job.ID, map[string]any{"target": "group:" + g.Name})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		after, err := db.GetAgentCronJob(job.ID)
		require.NoError(t, err)
		assert.True(t, after.IsGroupTarget())
		assert.Equal(t, g.ID, after.GroupID)
	})

	t.Run("group ownership allows group retarget without membership", func(t *testing.T) {
		f := newFlow(t)
		const caller = "crto-caller-aaaa-bbbb-cccc-000000000001"
		job := createSelfManagedCron(t, f, caller)
		g := f.HaveGroup("owned-destination")
		require.NoError(t, db.AddAgentGroupOwner(g.ID, caller, "test"))

		rec := patchCronAsAgent(t, f, caller, job.ID, map[string]any{"target": "group:" + g.Name})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		after, err := db.GetAgentCronJob(job.ID)
		require.NoError(t, err)
		assert.True(t, after.IsGroupTarget())
		assert.Equal(t, g.ID, after.GroupID)
	})

	t.Run("cross-group retarget is denied", func(t *testing.T) {
		f := newFlow(t)
		const caller = "crtx-caller-aaaa-bbbb-cccc-000000000001"
		job := createSelfManagedCron(t, f, caller)
		g := f.HaveGroup("foreign-destination")
		before, err := db.GetAgentCronJob(job.ID)
		require.NoError(t, err)

		rec := patchCronAsAgent(t, f, caller, job.ID, map[string]any{"target": "group:" + g.Name})
		assertDeniedCronRetargetHasNoSideEffects(t, f, before, rec, caller)
	})

	t.Run("explicit deny suppresses group-owner agent bypass", func(t *testing.T) {
		f := newFlow(t)
		const caller = "crtd-caller-aaaa-bbbb-cccc-000000000001"
		const proposed = "crtd-target-aaaa-bbbb-cccc-000000000002"
		job := createSelfManagedCron(t, f, caller)
		f.HaveConvWithTitle(proposed, "owned-member")
		g := f.HaveGroup("managed")
		f.HaveMember(g.Name, proposed)
		require.NoError(t, db.AddAgentGroupOwner(g.ID, caller, "test"))
		require.NoError(t, db.SetAgentPermissionOverride(
			caller, agentd.PermAgentSchedule, db.PermEffectDeny, "test"))
		before, err := db.GetAgentCronJob(job.ID)
		require.NoError(t, err)

		rec := patchCronAsAgent(t, f, caller, job.ID, map[string]any{"target": proposed})
		assertDeniedCronRetargetHasNoSideEffects(t, f, before, rec, caller, proposed)
	})
}

func TestCronPatchRetarget_CanonicalEquivalentTargetNeedsNoAdditionalAuthority(t *testing.T) {
	t.Run("agent title and stable id are the stored actor", func(t *testing.T) {
		f := newFlow(t)
		const caller = "crte-caller-aaaa-bbbb-cccc-000000000001"
		job := createSelfManagedCron(t, f, caller)
		require.NoError(t, db.SetAgentPermissionOverride(
			caller, agentd.PermAgentSchedule, db.PermEffectDeny, "test"))
		agentID, err := db.AgentIDForConv(caller)
		require.NoError(t, err)
		require.NotEmpty(t, agentID)

		for _, selector := range []string{"cron-caller", agentID} {
			rec := patchCronAsAgent(t, f, caller, job.ID, map[string]any{
				"target": selector, "subject": "same-target-" + selector[:4],
			})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		}
	})

	t.Run("group name and numeric id are the stored group", func(t *testing.T) {
		f := newFlow(t)
		const caller = "crte-group-aaaa-bbbb-cccc-000000000001"
		f.HaveConvWithTitle(caller, "group-caller")
		g := f.HaveGroup("canonical-group")
		f.HaveMember(g.Name, caller)
		job := createCronAsHuman(t, f, map[string]any{
			"owner": caller, "target": "group:" + g.Name, "interval": "1h",
			"body": "before", "enabled": false,
		})

		rec := patchCronAsAgent(t, f, caller, job.ID, map[string]any{
			"target": "group:" + strconv.FormatInt(g.ID, 10), "subject": "same-group",
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})
}

func TestCronPatchRetarget_RawGroupIDCannotBypassDestinationGate(t *testing.T) {
	f := newFlow(t)
	const caller = "crtr-caller-aaaa-bbbb-cccc-000000000001"
	f.HaveConvWithTitle(caller, "raw-group-caller")
	current := f.HaveGroup("current-group")
	foreign := f.HaveGroup("foreign-group")
	f.HaveMember(current.Name, caller)
	job := createCronAsHuman(t, f, map[string]any{
		"owner": caller, "target": "group:" + current.Name, "interval": "1h",
		"body": "before", "enabled": false,
	})
	before, err := db.GetAgentCronJob(job.ID)
	require.NoError(t, err)

	rec := patchCronAsAgent(t, f, caller, job.ID, map[string]any{"group_id": foreign.ID})
	assertDeniedCronRetargetHasNoSideEffects(t, f, before, rec, caller)
}

func TestCronPatchRetarget_MissingAndRetiredTargetsRemainClassifiedWithoutMutation(t *testing.T) {
	t.Run("missing selector is non-enumerating for agent caller", func(t *testing.T) {
		f := newFlow(t)
		const caller = "crtn-caller-aaaa-bbbb-cccc-000000000001"
		job := createSelfManagedCron(t, f, caller)
		before, err := db.GetAgentCronJob(job.ID)
		require.NoError(t, err)

		rec := patchCronAsAgent(t, f, caller, job.ID, map[string]any{"target": "missing-cron-target"})
		assertDeniedCronRetargetHasNoSideEffects(t, f, before, rec, caller)
		assert.NotContains(t, rec.Body.String(), "missing-cron-target")
	})

	t.Run("missing selector stays not found for human operator", func(t *testing.T) {
		f := newFlow(t)
		const owner = "crtn-owner-aaaa-bbbb-cccc-000000000001"
		f.HaveConvWithTitle(owner, "missing-target-owner")
		job := createCronAsHuman(t, f, map[string]any{
			"owner": owner, "target": owner, "interval": "1h", "body": "before",
		})
		before, err := db.GetAgentCronJob(job.ID)
		require.NoError(t, err)

		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t,
			http.MethodPatch, "/v1/cron/"+strconv.FormatInt(job.ID, 10),
			map[string]any{"target": "missing-cron-target"})))
		require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
		var body struct {
			Code string `json:"code"`
		}
		testharness.DecodeJSON(t, rec, &body)
		assert.Equal(t, "not_found", body.Code)
		after, err := db.GetAgentCronJob(job.ID)
		require.NoError(t, err)
		assert.Equal(t, before, after)
	})

	t.Run("retired agent resolves canonically then receives stable denial", func(t *testing.T) {
		f := newFlow(t)
		const caller = "crtt-caller-aaaa-bbbb-cccc-000000000001"
		const retired = "crtt-retired-aaaa-bbbb-cccc-000000000002"
		job := createSelfManagedCron(t, f, caller)
		f.HaveConvWithTitle(retired, "retired-destination")
		f.HaveEnrolledAgent(retired)
		agentID, err := db.AgentIDForConv(retired)
		require.NoError(t, err)
		_, err = db.RetireAgentAuthorizationByConv(retired, "human", "test")
		require.NoError(t, err)
		before, err := db.GetAgentCronJob(job.ID)
		require.NoError(t, err)

		rec := patchCronAsAgent(t, f, caller, job.ID, map[string]any{"target": agentID})
		assertDeniedCronRetargetHasNoSideEffects(t, f, before, rec, caller, retired)
		assert.NotContains(t, rec.Body.String(), retired)
		assert.NotContains(t, rec.Body.String(), agentID)
	})
}

func TestCronPatchRetarget_HumanMayRetargetAgentAndGroup(t *testing.T) {
	f := newFlow(t)
	const owner = "crth-owner-aaaa-bbbb-cccc-000000000001"
	const target = "crth-target-aaaa-bbbb-cccc-000000000002"
	f.HaveConvWithTitle(owner, "human-owner")
	f.HaveConvWithTitle(target, "human-target")
	job := createCronAsHuman(t, f, map[string]any{
		"owner": owner, "target": owner, "interval": "1h", "body": "before",
	})

	patchCronAsHuman(t, f, job.ID, map[string]any{"target": target})
	afterAgent, err := db.GetAgentCronJob(job.ID)
	require.NoError(t, err)
	assert.Equal(t, target, afterAgent.TargetConv)

	g := f.HaveGroup("human-group")
	patchCronAsHuman(t, f, job.ID, map[string]any{"target": "group:" + g.Name})
	afterGroup, err := db.GetAgentCronJob(job.ID)
	require.NoError(t, err)
	assert.True(t, afterGroup.IsGroupTarget())
	assert.Equal(t, g.ID, afterGroup.GroupID)
}

func TestCronPatchRetarget_ReauthorizesRefreshedCurrentTargetBeforeWrite(t *testing.T) {
	f := newFlow(t)
	const caller = "crtc-caller-aaaa-bbbb-cccc-000000000001"
	const replacement = "crtc-target-aaaa-bbbb-cccc-000000000002"
	job := createSelfManagedCron(t, f, caller)
	f.HaveConvWithTitle(replacement, "replacement-target")

	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	t.Cleanup(agentd.SetCronBeforeAuthorityLockForTest(func(operation string) {
		if operation == "patch" && calls.Add(1) == 1 {
			close(entered)
			<-release
		}
	}))

	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		result <- patchCronAsAgent(t, f, caller, job.ID, map[string]any{"body": "stale-boundary-write"})
	}()
	<-entered
	patchCronAsHuman(t, f, job.ID, map[string]any{"target": replacement})
	close(release)
	rec := <-result
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	var denial struct {
		Code string `json:"code"`
	}
	testharness.DecodeJSON(t, rec, &denial)
	assert.Equal(t, "permission", denial.Code)

	after, err := db.GetAgentCronJob(job.ID)
	require.NoError(t, err)
	assert.Equal(t, replacement, after.TargetConv)
	assert.Equal(t, "before-body", after.Body, "request authorized against the stale target mutated the refreshed row")
	assert.False(t, strings.Contains(rec.Body.String(), "replacement-target"))
}

func TestDashboardCronPatchRetarget_HumanUsesSharedAuthorizedHandler(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	const owner = "crtdash-owner-aaaa-bbbb-cccc-000000000001"
	const target = "crtdash-target-aaaa-bbbb-cccc-000000000002"
	f.HaveConvWithTitle(owner, "dashboard-owner")
	f.HaveConvWithTitle(target, "dashboard-target")
	job := createCronAsHuman(t, f, map[string]any{
		"owner": owner, "target": owner, "interval": "1h", "body": "before",
	})

	// The dashboard is the authorized human/operator surface, so it may retarget
	// successfully. This pins the shared handler wiring; agent denial behavior is
	// covered above and the Jobs JS test pins its non-2xx error presentation.
	dashboard := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(dashboard, testharness.JSONRequest(t, http.MethodPatch,
		"/api/cron/"+strconv.FormatInt(job.ID, 10), map[string]any{"target": target}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var response map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	assert.Equal(t, target, response["target_conv"])
}
