package agentd_test

import (
	"encoding/json"
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

const cronOwnerDeniedMessage = "caller is not authorized to assign the proposed cron owner"

func requireCronOwnerDenied(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	var body struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	testharness.DecodeJSON(t, rec, &body)
	assert.Equal(t, "permission", body.Code)
	assert.Equal(t, cronOwnerDeniedMessage, body.Error)
}

func assertDeniedCronOwnerHasNoSideEffects(
	t *testing.T,
	f *testharness.Flow,
	before *db.AgentCronJob,
	rec *httptest.ResponseRecorder,
	destinations ...string,
) {
	t.Helper()
	requireCronOwnerDenied(t, rec)
	after, err := db.GetAgentCronJob(before.ID)
	require.NoError(t, err)
	assert.Equal(t, before, after, "owner denial changed the stored cron row")
	runs, err := db.ListAgentCronRunsForJob(before.ID, 0)
	require.NoError(t, err)
	assert.Empty(t, runs, "owner denial recorded an immediate/scheduled run")
	for _, destination := range destinations {
		assert.Zero(t, msgRowCount(t, destination), "owner denial delivered to %s", destination)
	}
	rows, err := db.ListAuditLog(db.AuditLogFilter{Verb: "cron.update", Outcome: "success"})
	require.NoError(t, err)
	assert.Empty(t, rows, "owner denial was recorded as a successful cron update")
	rows, err = db.ListAuditLog(db.AuditLogFilter{Verb: "cron.update", Outcome: "failure"})
	require.NoError(t, err)
	require.Len(t, rows, 1, "owner denial must retain its failed-attempt audit record")
	assert.Equal(t, http.StatusForbidden, rows[0].Status)

	// There is no scheduler wake channel: a future sweep is the scheduler-
	// visible probe. The exact disabled row must remain inert after denial.
	agentd.RunCronTickForTest(before.CreatedAt.Add(24 * time.Hour))
	afterTick, err := db.GetAgentCronJob(before.ID)
	require.NoError(t, err)
	assert.Equal(t, before, afterTick)
	for _, destination := range destinations {
		assert.Zero(t, msgRowCount(t, destination),
			"owner denial became visible to scheduler for %s", destination)
	}
}

func TestCronPatchOwner_MissingUnauthorizedAndRetiredAreNonEnumerating(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	approvalCalls, restoreApproval := agentd.StubCountingApprovalForTest(true)
	t.Cleanup(restoreApproval)

	var denialBodies []string
	for _, kind := range []string{"unauthorized", "missing", "retired"} {
		t.Run(kind, func(t *testing.T) {
			f := newFlow(t)
			const caller = "croa-hidden-caller-aaaa-bbbb-cccc-000000000001"
			const proposed = "croa-hidden-owner-aaaa-bbbb-cccc-000000000002"
			job := createSelfManagedCron(t, f, caller)
			ownerSelector := "missing-cron-owner"
			if kind != "missing" {
				f.HaveConvWithTitle(proposed, "private-cron-owner")
				f.HaveEnrolledAgent(proposed)
				ownerSelector = "private-cron-owner"
				if kind == "retired" {
					retired, err := db.RetireAgent(proposed, "test", "done")
					require.NoError(t, err)
					require.True(t, retired)
				}
			}
			before, err := db.GetAgentCronJob(job.ID)
			require.NoError(t, err)

			req := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPatch,
				"/v1/cron/"+strconv.FormatInt(job.ID, 10), map[string]any{
					"owner": ownerSelector, "name": "after", "body": "must-not-land",
					"enabled": true, "run_immediately": true,
				}), caller)
			req.Header.Set("X-Tclaude-Ask-Human", "5s")
			result := make(chan *httptest.ResponseRecorder, 1)
			go func() { result <- testharness.Serve(f.Mux, req) }()
			var rec *httptest.ResponseRecorder
			select {
			case rec = <-result:
			case <-time.After(time.Second):
				t.Fatal("owner denial waited for interactive approval under cron authority")
			}
			assertDeniedCronOwnerHasNoSideEffects(t, f, before, rec, caller, proposed)
			assert.NotContains(t, rec.Body.String(), ownerSelector)
			assert.NotContains(t, rec.Body.String(), proposed)
			denialBodies = append(denialBodies, rec.Body.String())

			rows, err := db.ListAuditLog(db.AuditLogFilter{Verb: "approval.request"})
			require.NoError(t, err)
			assert.Empty(t, rows, "owner denial created an approval audit")
			snapshot := fetchAccessReqSnapshot(t, agentd.BuildDashboardHandlerForTest())
			assert.Zero(t, snapshot.AccessRequestsPending)
			assert.Empty(t, snapshot.AccessRequests, "owner denial created pending approval state")
		})
	}
	require.Len(t, denialBodies, 3)
	assert.Equal(t, denialBodies[0], denialBodies[1])
	assert.Equal(t, denialBodies[0], denialBodies[2])
	assert.Zero(t, approvalCalls())
}

func TestCronPatchOwner_UsesCanonicalSchedulingAuthority(t *testing.T) {
	t.Run("self schedule allows self attribution", func(t *testing.T) {
		f := newFlow(t)
		const caller = "croa-self-caller-aaaa-bbbb-cccc-000000000001"
		const priorOwner = "croa-self-prior-aaaa-bbbb-cccc-000000000002"
		f.HaveConvWithTitle(caller, "self-owner-caller")
		f.HaveEnrolledAgent(caller)
		f.HaveConvWithTitle(priorOwner, "prior-owner")
		f.HaveEnrolledAgent(priorOwner)
		require.NoError(t, db.SetAgentPermissionOverride(
			caller, agentd.PermSelfSchedule, db.PermEffectGrant, "test"))
		job := createCronAsHuman(t, f, map[string]any{
			"owner": priorOwner, "target": caller, "interval": "1h", "body": "before",
		})

		rec := patchCronAsAgent(t, f, caller, job.ID, map[string]any{"owner": caller})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		after, err := db.GetAgentCronJob(job.ID)
		require.NoError(t, err)
		assert.True(t, sameCronActor(t, after.OwnerConv, caller))
	})

	t.Run("agent schedule grant allows cross-agent attribution", func(t *testing.T) {
		f := newFlow(t)
		const caller = "croa-grant-caller-aaaa-bbbb-cccc-000000000001"
		const proposed = "croa-grant-owner-aaaa-bbbb-cccc-000000000002"
		job := createSelfManagedCron(t, f, caller)
		f.HaveConvWithTitle(proposed, "granted-owner")
		f.HaveEnrolledAgent(proposed)
		require.NoError(t, db.SetAgentPermissionOverride(
			caller, agentd.PermAgentSchedule, db.PermEffectGrant, "test"))

		rec := patchCronAsAgent(t, f, caller, job.ID, map[string]any{"owner": "granted-owner"})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		after, err := db.GetAgentCronJob(job.ID)
		require.NoError(t, err)
		assert.True(t, sameCronActor(t, after.OwnerConv, proposed))
	})

	t.Run("group owner may attribute to a managed member", func(t *testing.T) {
		f := newFlow(t)
		const caller = "croa-manager-caller-aaaa-bbbb-cccc-000000000001"
		const proposed = "croa-managed-owner-aaaa-bbbb-cccc-000000000002"
		job := createSelfManagedCron(t, f, caller)
		f.HaveConvWithTitle(proposed, "managed-owner")
		g := f.HaveGroup("owner-managed")
		f.HaveMember(g.Name, proposed)
		require.NoError(t, db.AddAgentGroupOwner(g.ID, caller, "test"))

		rec := patchCronAsAgent(t, f, caller, job.ID, map[string]any{"owner": proposed})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		after, err := db.GetAgentCronJob(job.ID)
		require.NoError(t, err)
		assert.True(t, sameCronActor(t, after.OwnerConv, proposed))
	})

	t.Run("explicit deny suppresses group-owner bypass", func(t *testing.T) {
		f := newFlow(t)
		const caller = "croa-deny-caller-aaaa-bbbb-cccc-000000000001"
		const proposed = "croa-deny-owner-aaaa-bbbb-cccc-000000000002"
		job := createSelfManagedCron(t, f, caller)
		f.HaveConvWithTitle(proposed, "denied-owner")
		g := f.HaveGroup("owner-denied")
		f.HaveMember(g.Name, proposed)
		require.NoError(t, db.AddAgentGroupOwner(g.ID, caller, "test"))
		require.NoError(t, db.SetAgentPermissionOverride(
			caller, agentd.PermAgentSchedule, db.PermEffectDeny, "test"))
		before, err := db.GetAgentCronJob(job.ID)
		require.NoError(t, err)

		rec := patchCronAsAgent(t, f, caller, job.ID, map[string]any{"owner": proposed})
		assertDeniedCronOwnerHasNoSideEffects(t, f, before, rec, caller, proposed)
	})
}

func sameCronActor(t *testing.T, a, b string) bool {
	t.Helper()
	aID, err := db.AgentIDForConv(a)
	require.NoError(t, err)
	bID, err := db.AgentIDForConv(b)
	require.NoError(t, err)
	return aID != "" && aID == bID
}

func TestCronPatchOwner_CanonicalEquivalentSpellingsNeedNoAdditionalAuthority(t *testing.T) {
	f := newFlow(t)
	const caller = "croa-alias-caller-aaaa-bbbb-cccc-000000000001"
	const owner = "croa-alias-owner-aaaa-bbbb-cccc-000000000002"
	f.HaveConvWithTitle(caller, "alias-caller")
	f.HaveEnrolledAgent(caller)
	f.HaveConvWithTitle(owner, "canonical-owner")
	f.HaveEnrolledAgent(owner)
	require.NoError(t, db.SetAgentPermissionOverride(
		caller, agentd.PermSelfSchedule, db.PermEffectGrant, "test"))
	require.NoError(t, db.SetAgentPermissionOverride(
		caller, agentd.PermAgentSchedule, db.PermEffectDeny, "test"))
	job := createCronAsHuman(t, f, map[string]any{
		"owner": owner, "target": caller, "interval": "1h", "body": "before",
	})
	ownerAgent, err := db.AgentIDForConv(owner)
	require.NoError(t, err)
	require.NotEmpty(t, ownerAgent)

	for i, selector := range []string{"canonical-owner", ownerAgent, owner} {
		rec := patchCronAsAgent(t, f, caller, job.ID, map[string]any{
			"owner": selector, "subject": "same-owner-" + strconv.Itoa(i),
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	after, err := db.GetAgentCronJob(job.ID)
	require.NoError(t, err)
	assert.Equal(t, ownerAgent, after.OwnerAgent, "canonical owner identity changed")
	assert.Equal(t, "same-owner-2", after.Subject)
}

func TestCronPatchOwner_EmptyAndRelativeSelectorsPreserveCanonicalBehavior(t *testing.T) {
	t.Run("empty owner is rejected without mutation", func(t *testing.T) {
		f := newFlow(t)
		const caller = "croa-empty-caller-aaaa-bbbb-cccc-000000000001"
		job := createSelfManagedCron(t, f, caller)
		before, err := db.GetAgentCronJob(job.ID)
		require.NoError(t, err)

		rec := patchCronAsAgent(t, f, caller, job.ID, map[string]any{
			"owner": "", "body": "must-not-land",
		})
		assertDeniedCronOwnerHasNoSideEffects(t, f, before, rec, caller)
	})

	for _, selector := range []string{".", "-"} {
		t.Run("relative owner "+selector+" resolves to self", func(t *testing.T) {
			f := newFlow(t)
			const caller = "croa-relative-caller-aaaa-bbbb-cccc-000000000001"
			const priorOwner = "croa-relative-prior-aaaa-bbbb-cccc-000000000002"
			const ambient = "croa-relative-ambient-aaaa-bbbb-cccc-000000000003"
			f.HaveConvWithTitle(caller, "relative-owner-caller")
			f.HaveEnrolledAgent(caller)
			f.HaveConvWithTitle(priorOwner, "relative-prior-owner")
			f.HaveEnrolledAgent(priorOwner)
			f.HaveConvWithTitle(ambient, "relative-ambient-owner")
			f.HaveEnrolledAgent(ambient)
			// "." / "-" must resolve to the AUTHENTICATED PEER (the caller),
			// never the daemon's own ambient identity. Point TCLAUDE_SESSION_ID
			// at a decoy conv so a regression that reads the process identity —
			// the leak that made this test flake under a tclaude-managed agent
			// (TCL-702) — resolves the wrong owner and fails deterministically.
			t.Setenv("TCLAUDE_SESSION_ID", ambient)
			require.NoError(t, db.SetAgentPermissionOverride(
				caller, agentd.PermSelfSchedule, db.PermEffectGrant, "test"))
			job := createCronAsHuman(t, f, map[string]any{
				"owner": priorOwner, "target": caller, "interval": "1h", "body": "before",
			})

			rec := patchCronAsAgent(t, f, caller, job.ID, map[string]any{"owner": selector})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			after, err := db.GetAgentCronJob(job.ID)
			require.NoError(t, err)
			assert.True(t, sameCronActor(t, after.OwnerConv, caller),
				"relative owner resolved to the peer, not the ambient identity")
		})
	}
}

func TestCronPatchOwner_PreservesTargetAndRoutingGates(t *testing.T) {
	t.Run("authorized retarget does not bypass owner authority", func(t *testing.T) {
		f := newFlow(t)
		const caller = "croa-combined-caller-aaaa-bbbb-cccc-000000000001"
		const proposedOwner = "croa-combined-owner-aaaa-bbbb-cccc-000000000002"
		job := createSelfManagedCron(t, f, caller)
		f.HaveConvWithTitle(proposedOwner, "combined-owner")
		f.HaveEnrolledAgent(proposedOwner)
		group := f.HaveGroup("combined-target")
		f.HaveMember(group.Name, caller)
		before, err := db.GetAgentCronJob(job.ID)
		require.NoError(t, err)

		rec := patchCronAsAgent(t, f, caller, job.ID, map[string]any{
			"target": "group:" + group.Name, "owner": proposedOwner,
			"body": "must-not-land", "enabled": true, "run_immediately": true,
		})
		assertDeniedCronOwnerHasNoSideEffects(t, f, before, rec, caller, proposedOwner)
	})

	t.Run("authorized raw route does not bypass owner authority", func(t *testing.T) {
		f := newFlow(t)
		const caller = "croa-route-caller-aaaa-bbbb-cccc-000000000001"
		const proposedOwner = "croa-route-owner-aaaa-bbbb-cccc-000000000002"
		job := createSelfManagedCron(t, f, caller)
		f.HaveConvWithTitle(proposedOwner, "route-owner")
		f.HaveEnrolledAgent(proposedOwner)
		group := f.HaveGroup("authorized-owner-route")
		f.HaveMember(group.Name, caller)
		before, err := db.GetAgentCronJob(job.ID)
		require.NoError(t, err)

		rec := patchCronAsAgent(t, f, caller, job.ID, map[string]any{
			"group_id": group.ID, "owner": proposedOwner,
			"body": "must-not-land", "enabled": true, "run_immediately": true,
		})
		assertDeniedCronOwnerHasNoSideEffects(t, f, before, rec, caller, proposedOwner)
	})
}

func TestCronPatchOwner_HumanKeepsPreciseDiagnosticsAndDashboardPath(t *testing.T) {
	t.Run("missing owner", func(t *testing.T) {
		f := newFlow(t)
		const owner = "croa-human-owner-aaaa-bbbb-cccc-000000000001"
		f.HaveConvWithTitle(owner, "human-owner")
		job := createCronAsHuman(t, f, map[string]any{
			"owner": owner, "target": owner, "interval": "1h", "body": "before",
		})
		before, err := db.GetAgentCronJob(job.ID)
		require.NoError(t, err)

		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t,
			http.MethodPatch, "/v1/cron/"+strconv.FormatInt(job.ID, 10),
			map[string]any{"owner": "missing-human-owner"})))
		require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "resolve owner")
		after, err := db.GetAgentCronJob(job.ID)
		require.NoError(t, err)
		assert.Equal(t, before, after)
	})

	t.Run("retired owner", func(t *testing.T) {
		f := newFlow(t)
		const owner = "croa-human-live-aaaa-bbbb-cccc-000000000001"
		const retiredOwner = "croa-human-retired-aaaa-bbbb-cccc-000000000002"
		f.HaveConvWithTitle(owner, "human-live-owner")
		f.HaveConvWithTitle(retiredOwner, "human-retired-owner")
		f.HaveEnrolledAgent(retiredOwner)
		retired, err := db.RetireAgent(retiredOwner, "test", "done")
		require.NoError(t, err)
		require.True(t, retired)
		job := createCronAsHuman(t, f, map[string]any{
			"owner": owner, "target": owner, "interval": "1h", "body": "before",
		})
		before, err := db.GetAgentCronJob(job.ID)
		require.NoError(t, err)

		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t,
			http.MethodPatch, "/v1/cron/"+strconv.FormatInt(job.ID, 10),
			map[string]any{"owner": retiredOwner})))
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), cronRetiredOwnerWireMessage)
		after, err := db.GetAgentCronJob(job.ID)
		require.NoError(t, err)
		assert.Equal(t, before, after)
	})

	t.Run("dashboard operator uses shared owner patch", func(t *testing.T) {
		t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
		f := newFlow(t)
		const owner = "croa-dashboard-owner-aaaa-bbbb-cccc-000000000001"
		const nextOwner = "croa-dashboard-next-aaaa-bbbb-cccc-000000000002"
		f.HaveConvWithTitle(owner, "dashboard-owner")
		f.HaveConvWithTitle(nextOwner, "dashboard-next-owner")
		job := createCronAsHuman(t, f, map[string]any{
			"owner": owner, "target": owner, "interval": "1h", "body": "before",
		})

		rec := testharness.Serve(agentd.BuildDashboardHandlerForTest(),
			testharness.JSONRequest(t, http.MethodPatch,
				"/api/cron/"+strconv.FormatInt(job.ID, 10), map[string]any{"owner": nextOwner}))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		after, err := db.GetAgentCronJob(job.ID)
		require.NoError(t, err)
		assert.True(t, sameCronActor(t, after.OwnerConv, nextOwner))
	})
}

func TestCronPatchOwner_RefreshesStoredOwnerBeforeCanonicalDecision(t *testing.T) {
	for _, winner := range []string{"direct DB", "dashboard"} {
		t.Run(winner, func(t *testing.T) {
			if winner == "dashboard" {
				t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
			}
			f := newFlow(t)
			const caller = "croa-race-caller-aaaa-bbbb-cccc-000000000001"
			const foreignOwner = "croa-race-owner-aaaa-bbbb-cccc-000000000002"
			f.HaveConvWithTitle(caller, "race-owner-caller")
			f.HaveEnrolledAgent(caller)
			f.HaveConvWithTitle(foreignOwner, "race-foreign-owner")
			f.HaveEnrolledAgent(foreignOwner)
			require.NoError(t, db.SetAgentPermissionOverride(
				caller, agentd.PermSelfSchedule, db.PermEffectGrant, "test"))
			job := createCronAsHuman(t, f, map[string]any{
				"owner": foreignOwner, "target": caller, "interval": "1h",
				"body": "before", "enabled": false,
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
					"owner": foreignOwner, "body": "stale-owner-write",
				})
			}()
			<-entered
			if winner == "direct DB" {
				winnerBody := "db-owner-winner"
				patchOwner := caller
				n, err := db.UpdateAgentCronJobFields(job.ID, db.UpdateCronPatch{
					OwnerConv: &patchOwner, Body: &winnerBody,
				})
				require.NoError(t, err)
				require.EqualValues(t, 1, n)
			} else {
				rec := testharness.Serve(agentd.BuildDashboardHandlerForTest(),
					testharness.JSONRequest(t, http.MethodPatch,
						"/api/cron/"+strconv.FormatInt(job.ID, 10), map[string]any{
							"owner": caller, "body": "dashboard-owner-winner",
						}))
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			}
			winnerRow, err := db.GetAgentCronJob(job.ID)
			require.NoError(t, err)
			releaseOnce.Do(func() { close(release) })

			rec := <-result
			requireCronOwnerDenied(t, rec)
			after, err := db.GetAgentCronJob(job.ID)
			require.NoError(t, err)
			assert.Equal(t, winnerRow, after,
				"stale canonical-equivalence decision overwrote the refreshed owner")
			assert.True(t, sameCronActor(t, after.OwnerConv, caller))
			assert.Zero(t, msgRowCount(t, caller))
			assert.Zero(t, msgRowCount(t, foreignOwner))
		})
	}
}

// TestCronCreate_RelativeTargetResolvesToPeerNotAmbient locks in the same
// peer-anchored self-selector contract for the CREATE path (TCL-702). The
// CLI sends a self-targeted `tclaude agent cron add` as target "." and
// expects the daemon to resolve it to the caller. Resolving "." against the
// daemon's own process identity (or an ambient TCLAUDE_SESSION_ID) instead
// mis-targets the job — and denies the caller who lacks agent.schedule for
// the decoy — under any tclaude-managed agent environment.
func TestCronCreate_RelativeTargetResolvesToPeerNotAmbient(t *testing.T) {
	for _, selector := range []string{".", "-"} {
		t.Run("relative target "+selector+" resolves to peer", func(t *testing.T) {
			f := newFlow(t)
			const caller = "croa-ctgt-caller-aaaa-bbbb-cccc-000000000001"
			const ambient = "croa-ctgt-ambient-aaaa-bbbb-cccc-000000000002"
			f.HaveConvWithTitle(caller, "ctgt-caller")
			f.HaveEnrolledAgent(caller)
			f.HaveConvWithTitle(ambient, "ctgt-ambient")
			f.HaveEnrolledAgent(ambient)
			require.NoError(t, db.SetAgentPermissionOverride(
				caller, agentd.PermSelfSchedule, db.PermEffectGrant, "test"))
			t.Setenv("TCLAUDE_SESSION_ID", ambient)

			rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(
				t, http.MethodPost, "/v1/cron", map[string]any{
					"target": selector, "interval": "1h", "body": "hi",
				}), caller))
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var out struct {
				ID int64 `json:"id"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
			job, err := db.GetAgentCronJob(out.ID)
			require.NoError(t, err)
			assert.Equal(t, caller, job.TargetConv,
				"relative target resolved to the peer, not the ambient identity")
			assert.Equal(t, caller, job.OwnerConv, "self-scheduled job is owned by the peer")
		})
	}
}
