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

// Scenario: PATCH /v1/cron/{id} with just `enabled:false` flips
// enabled and leaves every other field untouched. Pins the partial-
// update contract — the dashboard's edit form relies on this so
// re-saving an unchanged field doesn't overwrite it.
func TestCronPatch_PartialFields(t *testing.T) {
	f := newFlow(t)

	const convA = "owna-aaaa-bbbb-cccc-1111"
	const convB = "tgta-aaaa-bbbb-cccc-2222"
	f.HaveConvWithTitle(convA, "owner")
	f.HaveConvWithTitle(convB, "target")

	id, err := db.InsertAgentCronJob(&db.AgentCronJob{
		Name:            "before",
		OwnerConv:       convA,
		TargetConv:      convB,
		IntervalSeconds: 600,
		Subject:         "subj",
		Body:            "body-pre",
		Enabled:         true,
	})
	require.NoError(t, err, "InsertAgentCronJob")

	r := agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodPatch, "/v1/cron/"+strconv.FormatInt(id, 10),
		map[string]any{"enabled": false}))
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "PATCH body=%s", rec.Body.String())
	got, _ := db.GetAgentCronJob(id)
	require.NotNil(t, got, "job vanished after PATCH")
	assert.False(t, got.Enabled, "expected enabled=false after PATCH")
	assert.Equal(t, "before", got.Name, "name")
	assert.Equal(t, "subj", got.Subject, "subject")
	assert.Equal(t, "body-pre", got.Body, "body")
	assert.EqualValues(t, 600, got.IntervalSeconds, "interval")
	assert.Equal(t, convA, got.OwnerConv, "owner")
	assert.Equal(t, convB, got.TargetConv, "target")
}

// Scenario: PATCH that touches `interval` must NOT bump last_run_at.
// Re-enabling a paused job after a long idle should not fire 50
// catch-ups — pin the rule via the most likely accidental code path
// (touching the schedule with a non-zero last_run_at).
func TestCronPatch_IntervalDoesNotBumpLastRunAt(t *testing.T) {
	f := newFlow(t)

	const conv = "tgtb-aaaa-bbbb-cccc-2222"
	f.HaveConvWithTitle(conv, "target")

	id, err := db.InsertAgentCronJob(&db.AgentCronJob{
		Name: "n", OwnerConv: conv, TargetConv: conv,
		IntervalSeconds: 60, Body: "x", Enabled: true,
	})
	require.NoError(t, err, "InsertAgentCronJob")
	stamped := time.Now().Add(-30 * time.Minute).UTC().Truncate(time.Second)
	require.NoError(t, db.UpdateAgentCronJobLastRun(id, stamped, "ok"), "stamp")

	r := agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodPatch, "/v1/cron/"+strconv.FormatInt(id, 10),
		map[string]any{"interval": "5m"}))
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "PATCH body=%s", rec.Body.String())
	got, _ := db.GetAgentCronJob(id)
	assert.EqualValues(t, 300, got.IntervalSeconds, "interval after PATCH")
	assert.True(t, got.LastRunAt.Equal(stamped),
		"last_run_at changed after interval PATCH: got %v, want %v", got.LastRunAt, stamped)
}

// Scenario: PATCH with a name containing spaces / special chars is
// rejected. Pins the charset gate so dashboard form errors surface as
// 400 (the JS shows them inline) rather than letting a weird name
// flow into the prefixed subject "[cron:foo bar] ...".
func TestCronPatch_RejectsBadCharset(t *testing.T) {
	f := newFlow(t)

	const conv = "tgtc-aaaa-bbbb-cccc-2222"
	f.HaveConvWithTitle(conv, "target")

	id, _ := db.InsertAgentCronJob(&db.AgentCronJob{
		Name: "ok-name", OwnerConv: conv, TargetConv: conv,
		IntervalSeconds: 60, Body: "x", Enabled: true,
	})

	cases := []string{
		"has spaces",
		"with/slash",
		"semi;colon",
		"emoji😀",
	}
	for _, bad := range cases {
		r := agentd.AsHumanPeer(testharness.JSONRequest(t,
			http.MethodPatch, "/v1/cron/"+strconv.FormatInt(id, 10),
			map[string]any{"name": bad}))
		rec := testharness.Serve(f.Mux, r)
		assert.Equal(t, http.StatusBadRequest, rec.Code,
			"PATCH name=%q body=%s", bad, rec.Body.String())
	}
	// And the name we stored is unchanged.
	got, _ := db.GetAgentCronJob(id)
	assert.Equal(t, "ok-name", got.Name, "name should be unchanged after rejected PATCHes")
}

// Scenario: PATCH /v1/cron/{id} with an interval shorter than the
// scheduler tick is rejected. Same gate as POST so the dashboard form
// surfaces the same error in either flow.
func TestCronPatch_RejectsTooShortInterval(t *testing.T) {
	f := newFlow(t)

	const conv = "tgtd-aaaa-bbbb-cccc-2222"
	f.HaveConvWithTitle(conv, "target")

	id, _ := db.InsertAgentCronJob(&db.AgentCronJob{
		Name: "n", OwnerConv: conv, TargetConv: conv,
		IntervalSeconds: 60, Body: "x", Enabled: true,
	})

	r := agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodPatch, "/v1/cron/"+strconv.FormatInt(id, 10),
		map[string]any{"interval": "5s"}))
	rec := testharness.Serve(f.Mux, r)
	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"PATCH interval=5s body=%s", rec.Body.String())
}

// Scenario: PATCH /v1/cron/{id} on a missing id returns 404.
func TestCronPatch_NotFound(t *testing.T) {
	f := newFlow(t)

	r := agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodPatch, "/v1/cron/9999",
		map[string]any{"enabled": false}))
	rec := testharness.Serve(f.Mux, r)
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"PATCH unknown id body=%s", rec.Body.String())
}

// Scenario: an agent without `agent.schedule` or group-ownership over
// the target cannot PATCH a job that doesn't belong to it. Pins the
// auth gate — without it, any agent could mass-edit cron jobs.
func TestCronPatch_AuthGate_DeniesUnrelatedAgent(t *testing.T) {
	f := newFlow(t)

	const ownerConv = "ownb-aaaa-bbbb-cccc-1111"
	const targetConv = "tgte-aaaa-bbbb-cccc-2222"
	const otherConv = "othr-aaaa-bbbb-cccc-3333"
	f.HaveConvWithTitle(ownerConv, "owner")
	f.HaveConvWithTitle(targetConv, "target")
	f.HaveConvWithTitle(otherConv, "other")

	id, _ := db.InsertAgentCronJob(&db.AgentCronJob{
		Name: "n", OwnerConv: ownerConv, TargetConv: targetConv,
		IntervalSeconds: 60, Body: "x", Enabled: true,
	})

	r := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodPatch, "/v1/cron/"+strconv.FormatInt(id, 10),
		map[string]any{"enabled": false}), otherConv)
	rec := testharness.Serve(f.Mux, r)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"unrelated agent PATCH body=%s", rec.Body.String())
	got, _ := db.GetAgentCronJob(id)
	assert.True(t, got.Enabled,
		"denied PATCH must not change state; enabled=false leaked through")
}

// Scenario: dashboard cookie-auth twin PATCH /api/cron/{id} flows the
// same DB writes as /v1/cron/{id}. Pins the dashboard's edit form
// path end-to-end.
func TestDashboardCron_Patch_RoundTrips(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)

	const conv = "tgtf-aaaa-bbbb-cccc-2222"
	f.HaveConvWithTitle(conv, "target")

	id, _ := db.InsertAgentCronJob(&db.AgentCronJob{
		Name: "old-name", OwnerConv: conv, TargetConv: conv,
		IntervalSeconds: 60, Subject: "old-subj", Body: "old-body", Enabled: true,
	})

	mux := agentd.BuildDashboardHandlerForTest()
	body, _ := json.Marshal(map[string]any{
		"name":    "new-name",
		"subject": "new-subj",
		"body":    "new-body",
	})
	r := httptest.NewRequest(http.MethodPatch,
		"/api/cron/"+strconv.FormatInt(id, 10), strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	rec := testharness.Serve(mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "dashboard PATCH body=%s", rec.Body.String())
	got, _ := db.GetAgentCronJob(id)
	assert.Equal(t, "new-name", got.Name, "name")
	assert.Equal(t, "new-subj", got.Subject, "subject")
	assert.Equal(t, "new-body", got.Body, "body")
	// Untouched.
	assert.EqualValues(t, 60, got.IntervalSeconds, "interval untouched")
	assert.True(t, got.Enabled, "enabled untouched")
}

// Scenario: dashboard cookie-auth twin POST /api/cron creates a job
// with a human-supplied owner override. Pins the new `owner` field
// the dashboard form sends — without it humans couldn't attribute a
// job to a specific agent (e.g. a PO agent that monitors workers).
func TestDashboardCron_Create_OwnerOverride(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)

	const ownerConv = "ownr-aaaa-bbbb-cccc-1111"
	const targetConv = "tgth-aaaa-bbbb-cccc-2222"
	f.HaveConvWithTitle(ownerConv, "po-agent")
	f.HaveConvWithTitle(targetConv, "worker")

	mux := agentd.BuildDashboardHandlerForTest()
	body, _ := json.Marshal(map[string]any{
		"name":     "po-pings",
		"target":   targetConv,
		"owner":    ownerConv,
		"interval": "10m",
		"body":     "status check",
	})
	r := httptest.NewRequest(http.MethodPost, "/api/cron", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	rec := testharness.Serve(mux, r)
	require.Equal(t, http.StatusOK, rec.Code,
		"dashboard POST /api/cron body=%s", rec.Body.String())
	var resp struct {
		ID         int64  `json:"id"`
		OwnerConv  string `json:"owner_conv"`
		TargetConv string `json:"target_conv"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	assert.Equal(t, ownerConv, resp.OwnerConv, "owner_conv")
	assert.Equal(t, targetConv, resp.TargetConv, "target_conv")
	got, _ := db.GetAgentCronJob(resp.ID)
	if assert.NotNil(t, got, "DB row missing") {
		assert.Equal(t, ownerConv, got.OwnerConv, "DB row owner")
	}
}

// Scenario: dashboard POST /api/cron without the dashboard cookie is
// refused. Mirrors PatchAuthRequired so the create path is gated too.
func TestDashboardCron_CreateAuthRequired(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	_ = newFlow(t)

	mux := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(mux)
	body, _ := json.Marshal(map[string]any{
		"name":     "x",
		"target":   "anywhere",
		"interval": "10m",
		"body":     "hi",
	})
	r := httptest.NewRequest(http.MethodPost, "/api/cron", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	rec := testharness.Serve(mux, r)
	assert.NotEqual(t, http.StatusOK, rec.Code,
		"dashboard POST /api/cron without cookie should fail; body=%s", rec.Body.String())
}

// Scenario: PATCH /api/cron/{id} without the dashboard cookie returns
// non-200 (auth refused). Pins the cookie gate on the new endpoint.
func TestDashboardCron_PatchAuthRequired(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)

	const conv = "tgtg-aaaa-bbbb-cccc-2222"
	f.HaveConvWithTitle(conv, "target")

	id, _ := db.InsertAgentCronJob(&db.AgentCronJob{
		Name: "n", OwnerConv: conv, TargetConv: conv,
		IntervalSeconds: 60, Body: "x", Enabled: true,
	})

	// Hit the dashboard mux directly without the cookie injection —
	// the inner check fails 401/403.
	mux := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(mux)
	r := httptest.NewRequest(http.MethodPatch,
		"/api/cron/"+strconv.FormatInt(id, 10),
		strings.NewReader(`{"enabled":false}`))
	r.Header.Set("Content-Type", "application/json")
	rec := testharness.Serve(mux, r)
	assert.NotEqual(t, http.StatusOK, rec.Code,
		"dashboard PATCH without cookie should fail; body=%s", rec.Body.String())
	got, _ := db.GetAgentCronJob(id)
	assert.True(t, got.Enabled, "auth-failed PATCH must not change state")
}
