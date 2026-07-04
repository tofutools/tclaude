package agentd_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// These tests exercise group-multicast cron jobs — a cron job whose
// target is a whole group rather than a single conv. The scheduler
// resolves the group's membership AT FIRE TIME and fans the body out to
// every current member, reusing the same fanOutToGroup path that backs
// `group:` multicast sends.
//
// Surfaces under test: POST /v1/cron (handleCronCreate group resolution),
// POST /v1/cron/{id}/run-now (fireCronJob → fireCronGroupJob fan-out),
// and the recipients' agent_messages inboxes (db.ListAgentMessagesForConv).

// createCronJobAsAgent POSTs /v1/cron as an agent peer and returns the
// new job's id. Fails the test if the create is not a 200.
func createCronJobAsAgent(t *testing.T, f *testharness.Flow, asConv string, body map[string]any) int64 {
	t.Helper()
	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost, "/v1/cron", body), asConv))
	require.Equal(t, http.StatusOK, rec.Code, "POST /v1/cron body=%s", rec.Body.String())
	var resp struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "decode create resp")
	require.NotZero(t, resp.ID, "created job id")
	return resp.ID
}

// fireCronNow triggers one fire via POST /v1/cron/{id}/run-now (as the
// human, who bypasses the auth gate) and returns the status tag.
func fireCronNow(t *testing.T, f *testharness.Flow, id int64) string {
	t.Helper()
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(
		t, http.MethodPost, "/v1/cron/"+strconv.FormatInt(id, 10)+"/run-now", nil)))
	require.Equal(t, http.StatusOK, rec.Code, "run-now body=%s", rec.Body.String())
	var resp struct {
		Status string `json:"status"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "decode run-now resp")
	return resp.Status
}

// msgRowCount returns how many agent_messages rows a conv has received.
func msgRowCount(t *testing.T, conv string) int {
	t.Helper()
	rows, err := db.ListAgentMessagesForConv(conv, 1000)
	require.NoError(t, err, "ListAgentMessagesForConv(%s)", conv)
	return len(rows)
}

// Scenario 1: a group-target cron job, when it fires, fans the body out
// to every CURRENT member of the group except the job owner — each gets
// its own agent_messages row, and an online member is nudged.
func TestCronGroupMulticast_FiresToEveryMember(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("team")
	const po = "cgm1-popo-aaaa-bbbb-cccc-000000000001"
	const w1 = "cgm1-wkr1-aaaa-bbbb-cccc-000000000002"
	const w2 = "cgm1-wkr2-aaaa-bbbb-cccc-000000000003"
	f.HaveMember("team", po)
	f.HaveMember("team", w1)
	f.HaveMember("team", w2)
	f.HaveAliveSession(w1, "spwn-cgm1-w1", "tclaude-spwn-cgm1-w1", "/tmp/work")

	// The PO — a member — schedules a recurring multicast into the group.
	id := createCronJobAsAgent(t, f, po, map[string]any{
		"target":   "group:team",
		"interval": "10m",
		"name":     "standup",
		"body":     "status please",
	})

	// The stored job is group-kind, points group_id at "team", and has
	// no conv target.
	job, err := db.GetAgentCronJob(id)
	require.NoError(t, err)
	require.NotNil(t, job, "job row")
	assert.Equal(t, db.CronTargetGroup, job.TargetKind, "stored as a group-target job")
	assert.True(t, job.IsGroupTarget())
	assert.Empty(t, job.TargetConv, "group-target job has no conv target")
	assert.NotZero(t, job.GroupID, "group_id points at the target group")

	require.Equal(t, "ok", fireCronNow(t, f, id), "fire status")

	// Every member except the owner received exactly one row.
	for _, m := range []string{w1, w2} {
		rows, err := db.ListAgentMessagesForConv(m, 100)
		require.NoError(t, err)
		require.Len(t, rows, 1, "member %s got the cron multicast", m)
		assert.Equal(t, "status please", rows[0].Body)
		assert.Contains(t, rows[0].Subject, "[cron:standup]",
			"subject carries the cron-name tag")
		assert.Equal(t, job.GroupID, rows[0].GroupID, "row stamped with the target group")
	}
	// The owner is skipped — a PO scheduling a team ping does not ping
	// itself, exactly as a `group:` multicast skips its sender.
	assert.Zero(t, msgRowCount(t, po), "the job owner is skipped from its own multicast")

	// The online member was nudged in its tmux pane.
	f.AssertSentContains("tclaude-spwn-cgm1-w1:0.0", "new agent message", 2*time.Second)
}

// Scenario 2: membership is resolved AT FIRE TIME, not at create time.
// A member added after the job is created starts receiving on the next
// fire; a member removed stops receiving — the recurring job tracks the
// live roster.
func TestCronGroupMulticast_MembershipResolvedAtFireTime(t *testing.T) {
	f := newFlow(t)

	g := f.HaveGroup("team")
	const po = "cgm2-popo-aaaa-bbbb-cccc-000000000001"
	const w1 = "cgm2-wkr1-aaaa-bbbb-cccc-000000000002"
	const w2 = "cgm2-wkr2-aaaa-bbbb-cccc-000000000003"
	const w3 = "cgm2-wkr3-aaaa-bbbb-cccc-000000000004"
	f.HaveMember("team", po)
	f.HaveMember("team", w1)
	f.HaveMember("team", w2)

	id := createCronJobAsAgent(t, f, po, map[string]any{
		"target":   "group:team",
		"interval": "10m",
		"body":     "tick",
	})

	// Fire #1 — roster is {po, w1, w2}; po (owner) is skipped.
	require.Equal(t, "ok", fireCronNow(t, f, id), "fire #1 status")
	assert.Equal(t, 1, msgRowCount(t, w1), "w1 after fire #1")
	assert.Equal(t, 1, msgRowCount(t, w2), "w2 after fire #1")
	assert.Equal(t, 0, msgRowCount(t, w3), "w3 not a member yet")

	// Roster changes BETWEEN fires: w3 joins, w2 leaves.
	f.HaveMember("team", w3)
	require.NoError(t, db.RemoveAgentGroupMember(g.ID, w2), "remove w2")

	// Fire #2 — roster is now {po, w1, w3}; the fan-out must track it.
	require.Equal(t, "ok", fireCronNow(t, f, id), "fire #2 status")
	assert.Equal(t, 2, msgRowCount(t, w1), "w1 still a member — got fire #2 too")
	assert.Equal(t, 1, msgRowCount(t, w2),
		"w2 left before fire #2 — no new row, membership resolved at fire time")
	assert.Equal(t, 1, msgRowCount(t, w3),
		"w3 joined before fire #2 — fan-out picked up the new member")
}

// Scenario 3: a conv-target (non-group) cron job delivers to exactly one
// inbox — it does NOT fan out to the rest of the target's group. Pins
// that target_kind, not the presence of a routing group_id, decides
// whether a job multicasts.
func TestCronGroupMulticast_ConvTargetDeliversToExactlyOne(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("team")
	const po = "cgm3-popo-aaaa-bbbb-cccc-000000000001"
	const w1 = "cgm3-wkr1-aaaa-bbbb-cccc-000000000002"
	const w2 = "cgm3-wkr2-aaaa-bbbb-cccc-000000000003"
	f.HaveConvWithTitle(po, "po-agent")
	f.HaveConvWithTitle(w1, "worker-one")
	f.HaveMember("team", po)
	f.HaveMember("team", w1)
	f.HaveMember("team", w2)

	// The human schedules a 1:1 nudge for ONE worker, attributed to the
	// PO. owner(po) != target(w1) and they share "team", so the daemon
	// routes it through that group (group_id set) — but it stays a conv
	// target, not a multicast.
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(
		t, http.MethodPost, "/v1/cron", map[string]any{
			"target":   w1,
			"owner":    po,
			"interval": "10m",
			"body":     "one-to-one nudge",
		})))
	require.Equal(t, http.StatusOK, rec.Code, "POST /v1/cron body=%s", rec.Body.String())
	var cr struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &cr))

	job, err := db.GetAgentCronJob(cr.ID)
	require.NoError(t, err)
	require.NotNil(t, job)
	assert.Equal(t, db.CronTargetConv, job.TargetKind, "stored as a conv-target job")
	assert.False(t, job.IsGroupTarget())
	assert.Equal(t, w1, job.TargetConv, "conv target preserved")
	assert.NotZero(t, job.GroupID, "routed via the shared group (non-zero group_id)")

	require.Equal(t, "ok", fireCronNow(t, f, cr.ID), "fire status")

	// Exactly the addressed worker got it — the rest of the group did not.
	assert.Equal(t, 1, msgRowCount(t, w1), "the addressed conv received the nudge")
	assert.Equal(t, 0, msgRowCount(t, w2), "a non-addressed group member got nothing")
	assert.Zero(t, msgRowCount(t, po), "the owner got nothing")
}

// Scenario 4: an agent that is neither a member nor an owner of the
// target group cannot schedule a recurring multicast into it. Pins the
// auth gate — without it any agent could broadcast into any group on a
// schedule.
func TestCronGroupMulticast_AuthGate_DeniesNonMember(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("team")
	const member = "cgm4-mmbr-aaaa-bbbb-cccc-000000000001"
	const stranger = "cgm4-strg-aaaa-bbbb-cccc-000000000002"
	f.HaveMember("team", member)
	f.HaveEnrolledAgent(stranger)

	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(
		t, http.MethodPost, "/v1/cron", map[string]any{
			"target":   "group:team",
			"interval": "10m",
			"body":     "let me in",
		}), stranger))
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"non-member scheduling a group multicast body=%s", rec.Body.String())

	jobs, err := db.ListAgentCronJobs()
	require.NoError(t, err)
	assert.Empty(t, jobs, "the denied create wrote no job row")
}

// Scenario 5: the dashboard's Group (multicast) cron form — and the
// per-group ⏰-multicast button that opens it — POSTs /api/cron with
// target=group:<name>. Pins that wire path end to end: the cookie-auth
// twin resolves the group and stores a group-kind job.
func TestCronGroupMulticast_DashboardCreate(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	g := f.HaveGroup("team")
	const w1 = "cgm5-wkr1-aaaa-bbbb-cccc-000000000001"
	f.HaveMember("team", w1)

	mux := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(mux, testharness.JSONRequest(
		t, http.MethodPost, "/api/cron", map[string]any{
			"target":   "group:team",
			"interval": "15m",
			"name":     "team-ping",
			"body":     "dashboard multicast",
		}))
	require.Equal(t, http.StatusOK, rec.Code, "dashboard POST /api/cron body=%s", rec.Body.String())

	var resp struct {
		ID         int64  `json:"id"`
		TargetKind string `json:"target_kind"`
		GroupID    int64  `json:"group_id"`
		GroupName  string `json:"group_name"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "decode resp")
	assert.Equal(t, "group", resp.TargetKind, "response marks the job group-kind")
	assert.Equal(t, g.ID, resp.GroupID, "response carries the target group id")
	assert.Equal(t, "team", resp.GroupName, "response carries the target group name")

	job, err := db.GetAgentCronJob(resp.ID)
	require.NoError(t, err)
	require.NotNil(t, job, "job row")
	assert.True(t, job.IsGroupTarget(), "stored as a group-target job")
	assert.Equal(t, g.ID, job.GroupID, "stored group_id")
	assert.Empty(t, job.TargetConv, "group-target job has no conv target")
	assert.Empty(t, job.OwnerConv, "human-created job has no agent owner")

	// And it actually fans out when fired.
	require.Equal(t, "ok", fireCronNow(t, f, resp.ID))
	assert.Equal(t, 1, msgRowCount(t, w1), "fired dashboard-created job reached the member")
}

// Scenario 6: deleting a group removes its group-target cron jobs (JOH-244) —
// a group-target job (incl. a seeded rhythm) is meaningless once its group is
// gone, so DeleteAgentGroup sweeps it in the same cleanup transaction.
func TestCronGroupMulticast_DeletedGroup_RemovesGroupJob(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("doomed")
	const po = "cgm6-popo-aaaa-bbbb-cccc-000000000001"
	const w1 = "cgm6-wkr1-aaaa-bbbb-cccc-000000000002"
	f.HaveMember("doomed", po)
	f.HaveMember("doomed", w1)

	id := createCronJobAsAgent(t, f, po, map[string]any{
		"target":   "group:doomed",
		"interval": "10m",
		"body":     "anyone home",
	})

	// The group vanishes after the job is scheduled — its group-target job goes
	// with it.
	require.NoError(t, db.DeleteAgentGroup("doomed"), "delete group")

	job, err := db.GetAgentCronJob(id)
	require.NoError(t, err)
	assert.Nil(t, job, "the group-target job was removed with its group")
	assert.Zero(t, msgRowCount(t, w1), "no rows fanned out for a deleted group")
}

// Scenario 6b: a group-target job that somehow outlives its group (a stray
// row pointing at a group id that no longer resolves) still fires cleanly with
// a "no_target" status — the defense-in-depth behind the delete-time cleanup,
// so a lost sweep never errors the scheduler tick.
func TestCronGroupMulticast_StaleGroupJob_NoTarget(t *testing.T) {
	f := newFlow(t)
	_ = f

	// A group-target job whose group id never resolves (never existed / already
	// swept). Inserted directly so we model the survived-row case.
	id, err := db.InsertAgentCronJob(&db.AgentCronJob{
		Name:            "orphan",
		TargetKind:      db.CronTargetGroup,
		GroupID:         987654,
		IntervalSeconds: 600,
		Body:            "anyone home",
		Enabled:         true,
	})
	require.NoError(t, err, "insert stale group job")

	assert.Equal(t, "no_target", fireCronNow(t, f, id),
		"firing a group job whose group is gone is a clean no_target")
}
