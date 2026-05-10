package agentd_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario: agent calls sudo for `groups.spawn`, popup approves, the
// slug holds. requirePermission elsewhere now passes for that conv
// against that slug while the window is open.
//
// Pins the core promise: sudo is a third source for requirePermission
// that lives alongside defaults and per-conv grants.
func TestSudo_Approved_GrantsForDuration(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)
	restoreApproval := agentd.StubApprovalForTest(true)
	t.Cleanup(restoreApproval)

	f := newFlow(t)

	const conv = "sudo-aaaa-bbbb-cccc-1111"
	f.HaveConvWithTitle(conv, "worker")

	body := map[string]any{
		"slugs":    []string{"groups.spawn"},
		"duration": "5m",
		"reason":   "team-bootstrap",
	}
	r := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodPost, "/v1/sudo", body), conv)
	rec := testharness.Serve(f.Mux, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /v1/sudo status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Active grant exists in DB.
	rows, err := db.ListActiveSudoGrants(conv)
	if err != nil {
		t.Fatalf("ListActiveSudoGrants: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("active grants for %s: got %d, want 1", conv, len(rows))
	}
	got := rows[0]
	if got.Slug != "groups.spawn" {
		t.Errorf("slug = %q, want %q", got.Slug, "groups.spawn")
	}
	if got.Reason != "team-bootstrap" {
		t.Errorf("reason = %q, want %q", got.Reason, "team-bootstrap")
	}
	if !strings.HasPrefix(got.GrantedBy, "human:popup-id=") {
		t.Errorf("granted_by = %q, want prefix human:popup-id=", got.GrantedBy)
	}
	if !got.IsActive(time.Now()) {
		t.Errorf("freshly-inserted grant must be active; expires_at=%v revoked_at=%v",
			got.ExpiresAt, got.RevokedAt)
	}

	// HasActiveSudoGrant — the hot path requirePermission calls.
	ok, err := db.HasActiveSudoGrant(conv, "groups.spawn")
	if err != nil {
		t.Fatalf("HasActiveSudoGrant: %v", err)
	}
	if !ok {
		t.Errorf("HasActiveSudoGrant must return true for the freshly-granted slug")
	}
}

// Scenario: popup denies; no rows are inserted. Pins the explicit
// deny path — without it, a denied request might silently leak rows
// (defense-in-depth against handler ordering bugs).
func TestSudo_Denied_NoGrant(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)
	restoreApproval := agentd.StubApprovalForTest(false)
	t.Cleanup(restoreApproval)

	f := newFlow(t)
	const conv = "deny-aaaa-bbbb-cccc-1111"
	f.HaveConvWithTitle(conv, "worker")

	body := map[string]any{
		"slugs":    []string{"groups.spawn"},
		"duration": "5m",
	}
	r := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodPost, "/v1/sudo", body), conv)
	rec := testharness.Serve(f.Mux, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("denied request: status=%d body=%s, want 403", rec.Code, rec.Body.String())
	}
	rows, _ := db.ListActiveSudoGrants(conv)
	if len(rows) != 0 {
		t.Errorf("denied request must not insert rows; got %d", len(rows))
	}
}

// Scenario: blocklisted slugs (`permissions.grant` / `permissions.revoke`)
// are refused without ever popping the popup. Pins the validator gate
// at the request layer — these slugs would enable permanent
// escalation if elevated even briefly.
func TestSudo_Blocklist_RefusesWithoutPopup(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)
	// Approve every popup that opens — to prove the request never
	// reaches the popup, we'd see a row appear if it did.
	restoreApproval := agentd.StubApprovalForTest(true)
	t.Cleanup(restoreApproval)

	f := newFlow(t)
	const conv = "block-aaaa-bbbb-cccc-1111"
	f.HaveConvWithTitle(conv, "worker")

	body := map[string]any{
		"slugs":    []string{"groups.spawn", "permissions.grant"},
		"duration": "5m",
	}
	r := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodPost, "/v1/sudo", body), conv)
	rec := testharness.Serve(f.Mux, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("blocklisted request: status=%d body=%s, want 403", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "permissions.grant") {
		t.Errorf("response must name the blocked slug; body=%s", rec.Body.String())
	}
	rows, _ := db.ListActiveSudoGrants(conv)
	if len(rows) != 0 {
		t.Errorf("blocklisted request must not insert ANY rows (even the non-blocklisted ones); got %d", len(rows))
	}
}

// Scenario: a duration that exceeds sudoMaxDuration is rejected
// before the popup opens. Pins the cap — without it an agent could
// ask for "168h" and a distracted human might approve.
func TestSudo_DurationCap_RejectedWithoutPopup(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)
	restoreApproval := agentd.StubApprovalForTest(true)
	t.Cleanup(restoreApproval)

	f := newFlow(t)
	const conv = "cap-aaaa-bbbb-cccc-1111"
	f.HaveConvWithTitle(conv, "worker")

	body := map[string]any{
		"slugs":    []string{"groups.spawn"},
		"duration": "24h",
	}
	r := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodPost, "/v1/sudo", body), conv)
	rec := testharness.Serve(f.Mux, r)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("over-cap request: status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	rows, _ := db.ListActiveSudoGrants(conv)
	if len(rows) != 0 {
		t.Errorf("over-cap request must not insert rows; got %d", len(rows))
	}
}

// Scenario: human revokes a grant via DELETE /v1/sudo/{id} mid-window;
// the slug stops applying immediately. Pins the early-revoke path —
// crucial for incident response (a grant turned out badly, kill it
// now).
func TestSudo_RevokedEarly_TakesEffectImmediately(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)
	restoreApproval := agentd.StubApprovalForTest(true)
	t.Cleanup(restoreApproval)

	f := newFlow(t)
	const conv = "rev-aaaa-bbbb-cccc-1111"
	f.HaveConvWithTitle(conv, "worker")

	// Request + approve.
	body := map[string]any{"slugs": []string{"groups.spawn"}, "duration": "5m"}
	r := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodPost, "/v1/sudo", body), conv)
	rec := testharness.Serve(f.Mux, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Grants []struct {
			ID int64 `json:"id"`
		} `json:"grants"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Grants) != 1 || resp.Grants[0].ID == 0 {
		t.Fatalf("expected one grant with a non-zero id; got %+v", resp.Grants)
	}
	grantID := resp.Grants[0].ID

	// Pre-revoke: HasActiveSudoGrant returns true.
	if ok, _ := db.HasActiveSudoGrant(conv, "groups.spawn"); !ok {
		t.Fatalf("pre-revoke: grant should be active")
	}

	// Revoke as human.
	delPath := "/v1/sudo/" + itoa64(grantID)
	delReq := agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodDelete, delPath, nil))
	delRec := testharness.Serve(f.Mux, delReq)
	if delRec.Code != http.StatusOK {
		t.Fatalf("DELETE status=%d body=%s", delRec.Code, delRec.Body.String())
	}

	// Post-revoke: HasActiveSudoGrant returns false (no time travel
	// needed — revoked_at filter does the work).
	if ok, _ := db.HasActiveSudoGrant(conv, "groups.spawn"); ok {
		t.Errorf("post-revoke: grant must NOT be active")
	}
}

// Scenario: GET /v1/sudo from an agent returns its own active grants;
// agents can't see other agents' grants via the same call. Pins the
// per-conv scoping that requirePermission's whole "additive third
// source" model depends on.
func TestSudo_Ls_AgentSeesOnlyOwnGrants(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)
	restoreApproval := agentd.StubApprovalForTest(true)
	t.Cleanup(restoreApproval)

	f := newFlow(t)
	const aliceConv = "alic-aaaa-bbbb-cccc-1111"
	const bobConv = "bobl-aaaa-bbbb-cccc-2222"
	f.HaveConvWithTitle(aliceConv, "alice")
	f.HaveConvWithTitle(bobConv, "bob")

	// Both agents request and get approved.
	for _, conv := range []string{aliceConv, bobConv} {
		body := map[string]any{"slugs": []string{"groups.spawn"}, "duration": "5m"}
		r := agentd.AsAgentPeer(testharness.JSONRequest(t,
			http.MethodPost, "/v1/sudo", body), conv)
		if rec := testharness.Serve(f.Mux, r); rec.Code != http.StatusOK {
			t.Fatalf("POST for %s: status=%d body=%s", conv, rec.Code, rec.Body.String())
		}
	}

	// Alice GET /v1/sudo — sees only her own row.
	lsReq := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodGet, "/v1/sudo", nil), aliceConv)
	lsRec := testharness.Serve(f.Mux, lsReq)
	if lsRec.Code != http.StatusOK {
		t.Fatalf("GET /v1/sudo as alice status=%d body=%s", lsRec.Code, lsRec.Body.String())
	}
	var rows []map[string]any
	_ = json.Unmarshal(lsRec.Body.Bytes(), &rows)
	if len(rows) != 1 {
		t.Fatalf("alice ls: got %d rows, want 1; body=%s", len(rows), lsRec.Body.String())
	}
	if rows[0]["conv_id"] != aliceConv {
		t.Errorf("alice ls row conv_id = %v, want %s", rows[0]["conv_id"], aliceConv)
	}

	// Human ls --all — sees both. Cross-conv listing is human-only.
	allReq := agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodGet, "/v1/sudo?all=1", nil))
	allRec := testharness.Serve(f.Mux, allReq)
	if allRec.Code != http.StatusOK {
		t.Fatalf("GET /v1/sudo?all=1 status=%d body=%s", allRec.Code, allRec.Body.String())
	}
	var allRows []map[string]any
	_ = json.Unmarshal(allRec.Body.Bytes(), &allRows)
	if len(allRows) != 2 {
		t.Errorf("human ls --all: got %d rows, want 2", len(allRows))
	}

	// Agent ls --all — refused.
	agentAllReq := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodGet, "/v1/sudo?all=1", nil), aliceConv)
	agentAllRec := testharness.Serve(f.Mux, agentAllReq)
	if agentAllRec.Code != http.StatusForbidden {
		t.Errorf("agent ls --all: status=%d body=%s, want 403", agentAllRec.Code, agentAllRec.Body.String())
	}
}

// writeSudoConfig drops a config.json under $HOME/.tclaude with the
// supplied agent.sudo block. Each test gets a fresh tmpdir-HOME via
// testharness.New, so this scopes cleanly and reverts implicitly when
// the temp dir is removed.
func writeSudoConfig(t *testing.T, body string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("HOME"), ".tclaude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir tclaude config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}
}

// Scenario: config sets agent.sudo.max_duration to 30m. A request for
// 1h is rejected with 400 before the popup opens — the config cap
// kicks in below the hardcoded ceiling. Pins the global-default
// override path: v1's hardcoded const is now a fallback, not an
// inviolable ceiling.
func TestSudo_ConfigMaxDuration_LowersTheCap(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)
	restoreApproval := agentd.StubApprovalForTest(true)
	t.Cleanup(restoreApproval)

	f := newFlow(t)
	writeSudoConfig(t, `{
	  "agent": {
	    "sudo": {
	      "max_duration": "30m"
	    }
	  }
	}`)
	const conv = "cfg-aaaa-bbbb-cccc-1111"
	f.HaveConvWithTitle(conv, "worker")

	body := map[string]any{
		"slugs":    []string{"groups.spawn"},
		"duration": "1h",
	}
	r := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodPost, "/v1/sudo", body), conv)
	rec := testharness.Serve(f.Mux, r)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("over-config-cap request: status=%d body=%s, want 400",
			rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "30m") {
		t.Errorf("error must surface the resolved cap (30m) so the human knows what was overridden; body=%s",
			rec.Body.String())
	}
	rows, _ := db.ListActiveSudoGrants(conv)
	if len(rows) != 0 {
		t.Errorf("over-config-cap request must not insert rows; got %d", len(rows))
	}
}

// Scenario: config sets a tight global cap (15m) AND a per-conv
// override raising it for one specific title ("manager-bot": 45m).
// The matching agent's request for 30m is approved; a non-matching
// agent's same request is rejected. Pins the per-conv override path
// keyed by title — the doc's "specific manager agent gets a longer
// window" example.
func TestSudo_PerConvOverrideMaxDuration_AppliedByTitle(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)
	restoreApproval := agentd.StubApprovalForTest(true)
	t.Cleanup(restoreApproval)

	f := newFlow(t)
	writeSudoConfig(t, `{
	  "agent": {
	    "sudo": {
	      "max_duration": "15m",
	      "overrides": {
	        "manager-bot": {
	          "max_duration": "45m"
	        }
	      }
	    }
	  }
	}`)
	const managerConv = "mgr-aaaa-bbbb-cccc-1111"
	const workerConv = "wrk-aaaa-bbbb-cccc-2222"
	f.HaveConvWithTitle(managerConv, "manager-bot")
	f.HaveConvWithTitle(workerConv, "plain-worker")

	// Manager: 30m is allowed (under the 45m override).
	mgrBody := map[string]any{"slugs": []string{"groups.spawn"}, "duration": "30m"}
	mgrReq := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodPost, "/v1/sudo", mgrBody), managerConv)
	mgrRec := testharness.Serve(f.Mux, mgrReq)
	if mgrRec.Code != http.StatusOK {
		t.Errorf("manager 30m under override (45m): status=%d body=%s, want 200",
			mgrRec.Code, mgrRec.Body.String())
	}
	if rows, _ := db.ListActiveSudoGrants(managerConv); len(rows) != 1 {
		t.Errorf("manager grant must land; got %d rows", len(rows))
	}

	// Worker: same 30m is rejected against the global 15m cap.
	wrkBody := map[string]any{"slugs": []string{"groups.spawn"}, "duration": "30m"}
	wrkReq := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodPost, "/v1/sudo", wrkBody), workerConv)
	wrkRec := testharness.Serve(f.Mux, wrkReq)
	if wrkRec.Code != http.StatusBadRequest {
		t.Errorf("worker 30m over global cap (15m): status=%d body=%s, want 400",
			wrkRec.Code, wrkRec.Body.String())
	}
	if rows, _ := db.ListActiveSudoGrants(workerConv); len(rows) != 0 {
		t.Errorf("worker over-cap request must not insert rows; got %d", len(rows))
	}
}

// Scenario: config-supplied blocklist replaces the hardcoded default.
// A slug that v1 hardcoded as blocked (permissions.grant) becomes
// allowable when config sets blocklist to a different list — and a
// fresh entry the human added (groups.own) becomes blocked. Pins the
// "replace, not merge" semantics: the human is fully in control.
func TestSudo_ConfigBlocklist_ReplacesDefaults(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)
	restoreApproval := agentd.StubApprovalForTest(true)
	t.Cleanup(restoreApproval)

	f := newFlow(t)
	writeSudoConfig(t, `{
	  "agent": {
	    "sudo": {
	      "blocklist": ["groups.own"]
	    }
	  }
	}`)
	const conv = "blk-aaaa-bbbb-cccc-1111"
	f.HaveConvWithTitle(conv, "worker")

	// permissions.grant — hardcoded-blocked in v1, but config replaced
	// the list with just [groups.own]. Request should now reach the
	// popup (and succeed under the stub-approval).
	allowBody := map[string]any{"slugs": []string{"permissions.grant"}, "duration": "5m"}
	allowReq := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodPost, "/v1/sudo", allowBody), conv)
	allowRec := testharness.Serve(f.Mux, allowReq)
	if allowRec.Code != http.StatusOK {
		t.Errorf("config replaced blocklist; permissions.grant should be allowable now: status=%d body=%s",
			allowRec.Code, allowRec.Body.String())
	}

	// groups.own — newly blocked by config. Should 403 without popup.
	denyBody := map[string]any{"slugs": []string{"groups.own"}, "duration": "5m"}
	denyReq := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodPost, "/v1/sudo", denyBody), conv)
	denyRec := testharness.Serve(f.Mux, denyReq)
	if denyRec.Code != http.StatusForbidden {
		t.Errorf("groups.own newly blocked by config: status=%d body=%s, want 403",
			denyRec.Code, denyRec.Body.String())
	}
	if !strings.Contains(denyRec.Body.String(), "groups.own") {
		t.Errorf("error must name the blocked slug; body=%s", denyRec.Body.String())
	}
}
