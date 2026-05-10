package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

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
	if err != nil {
		t.Fatalf("InsertAgentCronJob: %v", err)
	}

	r := agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodPatch, "/v1/cron/"+strconv.FormatInt(id, 10),
		map[string]any{"enabled": false}))
	rec := testharness.Serve(f.Mux, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH: status=%d body=%s", rec.Code, rec.Body.String())
	}
	got, _ := db.GetAgentCronJob(id)
	if got == nil {
		t.Fatalf("job vanished after PATCH")
	}
	if got.Enabled {
		t.Errorf("expected enabled=false after PATCH, got true")
	}
	if got.Name != "before" || got.Subject != "subj" || got.Body != "body-pre" ||
		got.IntervalSeconds != 600 || got.OwnerConv != convA || got.TargetConv != convB {
		t.Errorf("non-patched fields changed: %+v", got)
	}
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
	if err != nil {
		t.Fatalf("InsertAgentCronJob: %v", err)
	}
	stamped := time.Now().Add(-30 * time.Minute).UTC().Truncate(time.Second)
	if err := db.UpdateAgentCronJobLastRun(id, stamped, "ok"); err != nil {
		t.Fatalf("stamp: %v", err)
	}

	r := agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodPatch, "/v1/cron/"+strconv.FormatInt(id, 10),
		map[string]any{"interval": "5m"}))
	rec := testharness.Serve(f.Mux, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH: status=%d body=%s", rec.Code, rec.Body.String())
	}
	got, _ := db.GetAgentCronJob(id)
	if got.IntervalSeconds != 300 {
		t.Errorf("interval after PATCH = %d, want 300", got.IntervalSeconds)
	}
	if !got.LastRunAt.Equal(stamped) {
		t.Errorf("last_run_at changed after interval PATCH: got %v, want %v",
			got.LastRunAt, stamped)
	}
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
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PATCH name=%q status=%d body=%s, want 400",
				bad, rec.Code, rec.Body.String())
		}
	}
	// And the name we stored is unchanged.
	got, _ := db.GetAgentCronJob(id)
	if got.Name != "ok-name" {
		t.Errorf("name should be unchanged after rejected PATCHes; got %q", got.Name)
	}
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
	if rec.Code != http.StatusBadRequest {
		t.Errorf("PATCH interval=5s status=%d body=%s, want 400",
			rec.Code, rec.Body.String())
	}
}

// Scenario: PATCH /v1/cron/{id} on a missing id returns 404.
func TestCronPatch_NotFound(t *testing.T) {
	f := newFlow(t)

	r := agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodPatch, "/v1/cron/9999",
		map[string]any{"enabled": false}))
	rec := testharness.Serve(f.Mux, r)
	if rec.Code != http.StatusNotFound {
		t.Errorf("PATCH unknown id: status=%d body=%s, want 404",
			rec.Code, rec.Body.String())
	}
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
	if rec.Code != http.StatusForbidden {
		t.Errorf("unrelated agent PATCH: status=%d body=%s, want 403",
			rec.Code, rec.Body.String())
	}
	got, _ := db.GetAgentCronJob(id)
	if !got.Enabled {
		t.Errorf("denied PATCH must not change state; enabled=false leaked through")
	}
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
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard PATCH: status=%d body=%s", rec.Code, rec.Body.String())
	}
	got, _ := db.GetAgentCronJob(id)
	if got.Name != "new-name" || got.Subject != "new-subj" || got.Body != "new-body" {
		t.Errorf("PATCH didn't apply: %+v", got)
	}
	// Untouched.
	if got.IntervalSeconds != 60 || !got.Enabled {
		t.Errorf("untouched fields changed: %+v", got)
	}
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
	if rec.Code == http.StatusOK {
		t.Errorf("dashboard PATCH without cookie should fail; got 200 body=%s", rec.Body.String())
	}
	got, _ := db.GetAgentCronJob(id)
	if !got.Enabled {
		t.Errorf("auth-failed PATCH must not change state")
	}
}
