package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Cron-expression schedules (the interval alternative): POST/PATCH carry a
// `cron_expr`, the daemon validates it through the same cronexpr parser the
// scheduler's due check uses, and the exactly-one-mode invariant (interval
// XOR expression) holds across every write path. Fire/delivery is the same
// machinery as interval jobs — pinned end-to-end once below, not re-proven
// per scenario. Due-check *timing* math lives in the db unit tests
// (TestAgentCronJob_DueLogic_CronExpr), where timestamps can be pinned.

// Scenario: create an expression job over HTTP, fire it, and watch the
// message land. Pins the wire shape (cron_expr echoed, interval_seconds 0)
// and that expression jobs ride the existing delivery path unchanged.
func TestCronExpr_CreateAndFire(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("team")
	const po = "cxp1-popo-aaaa-bbbb-cccc-000000000001"
	const worker = "cxp1-work-aaaa-bbbb-cccc-000000000002"
	f.HaveConvWithTitle(po, "po-agent")
	f.HaveConvWithTitle(worker, "worker")
	f.HaveMember("team", po)
	f.HaveMember("team", worker)
	f.HaveAliveSession(worker, "spwn-cxp1-worker", "tclaude-spwn-cxp1-worker", "/tmp/work")

	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(
		testharness.JSONRequest(t, http.MethodPost, "/v1/cron", map[string]any{
			"target":    worker,
			"owner":     po,
			"cron_expr": "*/5 * * * *",
			"body":      "expr status please",
		})))
	require.Equal(t, http.StatusOK, rec.Code, "POST /v1/cron body=%s", rec.Body.String())
	var resp struct {
		ID              int64  `json:"id"`
		CronExpr        string `json:"cron_expr"`
		IntervalSeconds int64  `json:"interval_seconds"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "decode create resp")
	assert.Equal(t, "*/5 * * * *", resp.CronExpr, "cron_expr echoed on the wire")
	assert.Zero(t, resp.IntervalSeconds, "expression job carries no interval")

	row, err := db.GetAgentCronJob(resp.ID)
	require.NoError(t, err)
	require.NotNil(t, row, "DB row missing")
	assert.Equal(t, "*/5 * * * *", row.CronExpr, "cron_expr persisted")
	assert.Zero(t, row.IntervalSeconds, "interval_seconds zero in the row")

	// Fire + delivery are schedule-mode-agnostic: run-now sends the body and
	// stamps last_run, exactly like an interval job.
	require.Equal(t, "ok", fireCronNow(t, f, resp.ID), "fire status")
	findCronMsg(t, worker, "expr status please")
	row, _ = db.GetAgentCronJob(resp.ID)
	assert.False(t, row.LastRunAt.IsZero(), "last_run_at stamped after fire")
}

// Scenario: creation rejects an unparseable expression, a never-fires
// expression (Feb 30), and the ambiguous both-modes form — each a 400
// before anything is written.
func TestCronExpr_CreateRejectsInvalid(t *testing.T) {
	f := newFlow(t)

	const conv = "cxp2-tgt0-aaaa-bbbb-cccc-000000000001"
	f.HaveConvWithTitle(conv, "target")

	cases := []struct {
		name string
		body map[string]any
	}{
		{"garbage expr", map[string]any{"target": conv, "cron_expr": "not an expr", "body": "x"}},
		{"never fires", map[string]any{"target": conv, "cron_expr": "0 0 30 2 *", "body": "x"}},
		{"both modes", map[string]any{"target": conv, "cron_expr": "*/5 * * * *", "interval": "10m", "body": "x"}},
	}
	for _, tc := range cases {
		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(
			testharness.JSONRequest(t, http.MethodPost, "/v1/cron", tc.body)))
		assert.Equal(t, http.StatusBadRequest, rec.Code,
			"%s: body=%s", tc.name, rec.Body.String())
	}
	jobs, err := db.ListAgentCronJobs()
	require.NoError(t, err)
	assert.Empty(t, jobs, "rejected creates must not leave rows behind")
}

// Scenario: PATCH switches a job between schedule modes and the
// exactly-one-mode invariant holds in the row after each hop:
// interval → expression (interval zeroed), expression → interval
// (expression cleared, sent as interval + explicit cron_expr:"").
func TestCronExpr_PatchSwitchesModes(t *testing.T) {
	f := newFlow(t)

	const conv = "cxp3-tgt0-aaaa-bbbb-cccc-000000000001"
	f.HaveConvWithTitle(conv, "target")

	id, err := db.InsertAgentCronJob(&db.AgentCronJob{
		Name: "modal", OwnerConv: conv, TargetConv: conv,
		IntervalSeconds: 600, Body: "x", Enabled: true,
	})
	require.NoError(t, err, "InsertAgentCronJob")

	patch := func(body map[string]any) *httptest.ResponseRecorder {
		return testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t,
			http.MethodPatch, "/v1/cron/"+strconv.FormatInt(id, 10), body)))
	}

	// interval → expression.
	rec := patch(map[string]any{"cron_expr": "0 9 * * 1-5"})
	require.Equal(t, http.StatusOK, rec.Code, "to-expr PATCH body=%s", rec.Body.String())
	row, _ := db.GetAgentCronJob(id)
	assert.Equal(t, "0 9 * * 1-5", row.CronExpr, "expression stored")
	assert.Zero(t, row.IntervalSeconds, "interval zeroed on the switch")

	// expression → interval (the dashboard's interval-mode save shape).
	rec = patch(map[string]any{"interval": "15m", "cron_expr": ""})
	require.Equal(t, http.StatusOK, rec.Code, "to-interval PATCH body=%s", rec.Body.String())
	row, _ = db.GetAgentCronJob(id)
	assert.Empty(t, row.CronExpr, "expression cleared on the switch back")
	assert.EqualValues(t, 900, row.IntervalSeconds, "interval stored")
}

// Scenario: switching an existing job to an expression re-anchors the
// schedule at the edit. Without the re-anchor, a match that already passed
// relative to the job's OLD last_run_at would fire within one scheduler
// tick of saving the edit — real crond evaluates an edited schedule forward
// from the moment of the edit, and so do we.
func TestCronExpr_PatchReanchorsSchedule(t *testing.T) {
	f := newFlow(t)

	const conv = "cxp5-tgt0-aaaa-bbbb-cccc-000000000001"
	f.HaveConvWithTitle(conv, "target")

	id, err := db.InsertAgentCronJob(&db.AgentCronJob{
		Name: "anchor", OwnerConv: conv, TargetConv: conv,
		IntervalSeconds: 86400, Body: "x", Enabled: true,
	})
	require.NoError(t, err, "InsertAgentCronJob")
	// The job last fired hours ago — the stale anchor an unfixed PATCH
	// would keep evaluating from.
	require.NoError(t, db.UpdateAgentCronJobLastRun(id, time.Now().Add(-6*time.Hour), "ok"))

	// The fix is pinned directly by the last_run_at bump below. "9am on
	// Feb 29" (a match years away) keeps the follow-up not-due sweep
	// deterministic — a daily expression would flake if the test straddled
	// its fire minute.
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodPatch, "/v1/cron/"+strconv.FormatInt(id, 10),
		map[string]any{"cron_expr": "0 9 29 2 *"})))
	require.Equal(t, http.StatusOK, rec.Code, "PATCH body=%s", rec.Body.String())

	row, _ := db.GetAgentCronJob(id)
	assert.Equal(t, "0 9 29 2 *", row.CronExpr, "expression stored")
	assert.WithinDuration(t, time.Now(), row.LastRunAt, time.Minute,
		"expression PATCH re-anchors last_run_at at the edit")
	assert.Equal(t, "ok", row.LastRunStatus, "status pill preserved across the re-anchor")

	due, err := db.ListDueAgentCronJobs(time.Now())
	require.NoError(t, err)
	for _, j := range due {
		assert.NotEqual(t, id, j.ID, "re-anchored expr job must not be immediately due")
	}
}

// Scenario: the PATCH forms that would break the invariant are refused —
// both modes at once, an invalid expression, and a bare cron_expr:"" that
// would leave the job with no schedule at all.
func TestCronExpr_PatchRejectsInvalid(t *testing.T) {
	f := newFlow(t)

	const conv = "cxp4-tgt0-aaaa-bbbb-cccc-000000000001"
	f.HaveConvWithTitle(conv, "target")

	id, err := db.InsertAgentCronJob(&db.AgentCronJob{
		Name: "guard", OwnerConv: conv, TargetConv: conv,
		CronExpr: "*/10 * * * *", Body: "x", Enabled: true,
	})
	require.NoError(t, err, "InsertAgentCronJob")

	cases := []struct {
		name string
		body map[string]any
	}{
		{"both modes", map[string]any{"interval": "5m", "cron_expr": "*/5 * * * *"}},
		{"garbage expr", map[string]any{"cron_expr": "banana"}},
		{"bare clear", map[string]any{"cron_expr": ""}},
	}
	for _, tc := range cases {
		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t,
			http.MethodPatch, "/v1/cron/"+strconv.FormatInt(id, 10), tc.body)))
		assert.Equal(t, http.StatusBadRequest, rec.Code,
			"%s: body=%s", tc.name, rec.Body.String())
	}
	row, _ := db.GetAgentCronJob(id)
	assert.Equal(t, "*/10 * * * *", row.CronExpr,
		"rejected PATCHes must leave the schedule untouched")
}

// Scenario: the dashboard's explain endpoint answers a valid expression
// with concrete next-fire times (+ the evaluation timezone) and an invalid
// one with valid:false + the parse error — both 200s, since "invalid" is a
// first-class answer the dialog renders inline, not a transport failure.
func TestDashboardCronExplain(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	_ = newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	explain := func(expr string) map[string]any {
		body, _ := json.Marshal(map[string]any{"expr": expr})
		r := httptest.NewRequest(http.MethodPost, "/api/cron/explain", strings.NewReader(string(body)))
		r.Header.Set("Content-Type", "application/json")
		rec := testharness.Serve(mux, r)
		require.Equal(t, http.StatusOK, rec.Code, "explain(%q) body=%s", expr, rec.Body.String())
		var resp map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "decode explain resp")
		return resp
	}

	good := explain("*/5 * * * *")
	assert.Equal(t, true, good["valid"], "valid expression")
	assert.NotEmpty(t, good["next"], "next fire times present")
	assert.NotEmpty(t, good["tz"], "evaluation timezone named")

	bad := explain("not an expr")
	assert.Equal(t, false, bad["valid"], "invalid expression flagged")
	assert.NotEmpty(t, bad["error"], "parse error surfaced")
}

// Scenario: /api/cron/explain sits behind the dashboard cookie gate like
// its sibling mutation routes.
func TestDashboardCronExplain_AuthRequired(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	_ = newFlow(t)
	mux := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(mux)
	r := httptest.NewRequest(http.MethodPost, "/api/cron/explain",
		strings.NewReader(`{"expr":"*/5 * * * *"}`))
	r.Header.Set("Content-Type", "application/json")
	rec := testharness.Serve(mux, r)
	assert.NotEqual(t, http.StatusOK, rec.Code,
		"explain without the dashboard cookie should fail; body=%s", rec.Body.String())
}
