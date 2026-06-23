package agentd_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	synctest.Test(t, func(t *testing.T) {
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
		require.Equal(t, http.StatusOK, rec.Code,
			"POST /v1/sudo body=%s", rec.Body.String())

		// Active grant exists in DB.
		rows, err := db.ListActiveSudoGrants(conv)
		require.NoError(t, err, "ListActiveSudoGrants")
		require.Len(t, rows, 1, "active grants for %s", conv)
		got := rows[0]
		assert.Equal(t, "groups.spawn", got.Slug, "slug")
		assert.Equal(t, "team-bootstrap", got.Reason, "reason")
		assert.True(t, strings.HasPrefix(got.GrantedBy, "human:popup-id="),
			"granted_by = %q, want prefix human:popup-id=", got.GrantedBy)
		assert.True(t, got.IsActive(time.Now()),
			"freshly-inserted grant must be active; expires_at=%v revoked_at=%v",
			got.ExpiresAt, got.RevokedAt)

		// HasActiveSudoGrant — the hot path requirePermission calls.
		ok, err := db.HasActiveSudoGrant(conv, "groups.spawn")
		require.NoError(t, err, "HasActiveSudoGrant")
		assert.True(t, ok, "HasActiveSudoGrant must return true for the freshly-granted slug")
	})
}

// Scenario: popup denies; no rows are inserted. Pins the explicit
// deny path — without it, a denied request might silently leak rows
// (defense-in-depth against handler ordering bugs).
func TestSudo_Denied_NoGrant(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"denied request body=%s", rec.Body.String())
		rows, _ := db.ListActiveSudoGrants(conv)
		assert.Empty(t, rows, "denied request must not insert rows")
	})
}

// Scenario: blocklisted slugs (`permissions.grant` / `permissions.revoke`)
// are refused without ever popping the popup. Pins the validator gate
// at the request layer — these slugs would enable permanent
// escalation if elevated even briefly.
func TestSudo_Blocklist_RefusesWithoutPopup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"blocklisted request body=%s", rec.Body.String())
		assert.Contains(t, rec.Body.String(), "permissions.grant",
			"response must name the blocked slug")
		rows, _ := db.ListActiveSudoGrants(conv)
		assert.Empty(t, rows,
			"blocklisted request must not insert ANY rows (even the non-blocklisted ones)")
	})
}

// Scenario: a duration that exceeds sudoMaxDuration is rejected
// before the popup opens. Pins the cap — without it an agent could
// ask for "168h" and a distracted human might approve.
func TestSudo_DurationCap_RejectedWithoutPopup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
		assert.Equal(t, http.StatusBadRequest, rec.Code,
			"over-cap request body=%s", rec.Body.String())
		rows, _ := db.ListActiveSudoGrants(conv)
		assert.Empty(t, rows, "over-cap request must not insert rows")
	})
}

// Scenario: human revokes a grant via DELETE /v1/sudo/{id} mid-window;
// the slug stops applying immediately. Pins the early-revoke path —
// crucial for incident response (a grant turned out badly, kill it
// now).
func TestSudo_RevokedEarly_TakesEffectImmediately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		var resp struct {
			Grants []struct {
				ID int64 `json:"id"`
			} `json:"grants"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		require.Len(t, resp.Grants, 1, "grants")
		require.NotZero(t, resp.Grants[0].ID, "grant id")
		grantID := resp.Grants[0].ID

		// Pre-revoke: HasActiveSudoGrant returns true.
		ok, _ := db.HasActiveSudoGrant(conv, "groups.spawn")
		require.True(t, ok, "pre-revoke: grant should be active")

		// Revoke as human.
		delPath := "/v1/sudo/" + itoa64(grantID)
		delReq := agentd.AsHumanPeer(testharness.JSONRequest(t,
			http.MethodDelete, delPath, nil))
		delRec := testharness.Serve(f.Mux, delReq)
		require.Equal(t, http.StatusOK, delRec.Code, "DELETE body=%s", delRec.Body.String())

		// Post-revoke: HasActiveSudoGrant returns false (no time travel
		// needed — revoked_at filter does the work).
		ok, _ = db.HasActiveSudoGrant(conv, "groups.spawn")
		assert.False(t, ok, "post-revoke: grant must NOT be active")
	})
}

// Scenario: GET /v1/sudo from an agent returns its own active grants;
// agents can't see other agents' grants via the same call. Pins the
// per-conv scoping that requirePermission's whole "additive third
// source" model depends on.
func TestSudo_Ls_AgentSeesOnlyOwnGrants(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
			rec := testharness.Serve(f.Mux, r)
			require.Equal(t, http.StatusOK, rec.Code,
				"POST for %s: body=%s", conv, rec.Body.String())
		}

		// Alice GET /v1/sudo — sees only her own row.
		lsReq := agentd.AsAgentPeer(testharness.JSONRequest(t,
			http.MethodGet, "/v1/sudo", nil), aliceConv)
		lsRec := testharness.Serve(f.Mux, lsReq)
		require.Equal(t, http.StatusOK, lsRec.Code,
			"GET /v1/sudo as alice body=%s", lsRec.Body.String())
		var rows []map[string]any
		_ = json.Unmarshal(lsRec.Body.Bytes(), &rows)
		require.Len(t, rows, 1, "alice ls; body=%s", lsRec.Body.String())
		assert.Equal(t, aliceConv, rows[0]["conv_id"], "alice ls row conv_id")

		// Human ls --all — sees both. Cross-conv listing is human-only.
		allReq := agentd.AsHumanPeer(testharness.JSONRequest(t,
			http.MethodGet, "/v1/sudo?all=1", nil))
		allRec := testharness.Serve(f.Mux, allReq)
		require.Equal(t, http.StatusOK, allRec.Code,
			"GET /v1/sudo?all=1 body=%s", allRec.Body.String())
		var allRows []map[string]any
		_ = json.Unmarshal(allRec.Body.Bytes(), &allRows)
		assert.Len(t, allRows, 2, "human ls --all")

		// Agent ls --all — refused.
		agentAllReq := agentd.AsAgentPeer(testharness.JSONRequest(t,
			http.MethodGet, "/v1/sudo?all=1", nil), aliceConv)
		agentAllRec := testharness.Serve(f.Mux, agentAllReq)
		assert.Equal(t, http.StatusForbidden, agentAllRec.Code,
			"agent ls --all body=%s", agentAllRec.Body.String())
	})
}

// writeSudoConfig drops a config.json under $HOME/.tclaude with the
// supplied agent.sudo block. Each test gets a fresh tmpdir-HOME via
// testharness.New, so this scopes cleanly and reverts implicitly when
// the temp dir is removed.
func writeSudoConfig(t *testing.T, body string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("HOME"), ".tclaude")
	require.NoError(t, os.MkdirAll(dir, 0o755), "mkdir tclaude config dir")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o644), "write config.json")
}

// Scenario: config sets agent.sudo.max_duration to 30m. A request for
// 1h is rejected with 400 before the popup opens — the config cap
// kicks in below the hardcoded ceiling. Pins the global-default
// override path: v1's hardcoded const is now a fallback, not an
// inviolable ceiling.
func TestSudo_ConfigMaxDuration_LowersTheCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
		assert.Equal(t, http.StatusBadRequest, rec.Code,
			"over-config-cap request body=%s", rec.Body.String())
		assert.Contains(t, rec.Body.String(), "30m",
			"error must surface the resolved cap (30m) so the human knows what was overridden")
		rows, _ := db.ListActiveSudoGrants(conv)
		assert.Empty(t, rows, "over-config-cap request must not insert rows")
	})
}

// Scenario: config sets a tight global cap (15m) AND a per-conv
// override raising it for one specific title ("manager-bot": 45m).
// The matching agent's request for 30m is approved; a non-matching
// agent's same request is rejected. Pins the per-conv override path
// keyed by title — the doc's "specific manager agent gets a longer
// window" example.
func TestSudo_PerConvOverrideMaxDuration_AppliedByTitle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
		assert.Equal(t, http.StatusOK, mgrRec.Code,
			"manager 30m under override (45m) body=%s", mgrRec.Body.String())
		mgrRows, _ := db.ListActiveSudoGrants(managerConv)
		assert.Len(t, mgrRows, 1, "manager grant must land")

		// Worker: same 30m is rejected against the global 15m cap.
		wrkBody := map[string]any{"slugs": []string{"groups.spawn"}, "duration": "30m"}
		wrkReq := agentd.AsAgentPeer(testharness.JSONRequest(t,
			http.MethodPost, "/v1/sudo", wrkBody), workerConv)
		wrkRec := testharness.Serve(f.Mux, wrkReq)
		assert.Equal(t, http.StatusBadRequest, wrkRec.Code,
			"worker 30m over global cap (15m) body=%s", wrkRec.Body.String())
		wrkRows, _ := db.ListActiveSudoGrants(workerConv)
		assert.Empty(t, wrkRows, "worker over-cap request must not insert rows")
	})
}

// Scenario: human runs `tclaude agent sudo request --target alice
// groups.spawn -d 5m`. POST /v1/sudo carries body.target → daemon
// takes the proactive (no-popup) path. Grant lands with the CLI's
// proactive audit label. Pins the new --target dispatch on the
// daemon side; the same body shape is what the CLI sends.
func TestSudo_Proactive_HumanWithTarget_NoPopup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// No popup stub here — if the proactive path mistakenly
		// fell through to the popup, the absent stub would 503 (no popup
		// base URL configured) or block waiting on a decision channel.
		// Reaching 200 with a row inserted IS the assertion.
		f := newFlow(t)
		const targetConv = "tgt-aaaa-bbbb-cccc-1111"
		f.HaveConvWithTitle(targetConv, "alice")

		body := map[string]any{
			"slugs":    []string{"groups.spawn"},
			"duration": "5m",
			"reason":   "team-bootstrap",
			"target":   targetConv,
		}
		r := agentd.AsHumanPeer(testharness.JSONRequest(t,
			http.MethodPost, "/v1/sudo", body))
		rec := testharness.Serve(f.Mux, r)
		require.Equal(t, http.StatusOK, rec.Code,
			"proactive POST body=%s", rec.Body.String())

		rows, err := db.ListActiveSudoGrants(targetConv)
		require.NoError(t, err, "ListActiveSudoGrants")
		require.Len(t, rows, 1, "active grants")
		got := rows[0]
		assert.Equal(t, "groups.spawn", got.Slug, "slug")
		assert.Equal(t, "<human-cli>:proactive", got.GrantedBy,
			"granted_by (CLI label distinguishes from dashboard + popup-approved)")
	})
}

// Scenario: an AGENT calls /v1/sudo with target set. That's
// manager-pattern approval (agent grants sudo to a peer) — explicitly
// deferred in v1, must 403. Without this gate, an agent with the
// daemon socket could elevate any peer it can name, defeating the
// human-in-the-loop promise.
func TestSudo_Proactive_AgentWithTarget_Refused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)
		const callerConv = "atk-aaaa-bbbb-cccc-1111"
		const targetConv = "tgt-aaaa-bbbb-cccc-2222"
		f.HaveConvWithTitle(callerConv, "attacker")
		f.HaveConvWithTitle(targetConv, "victim")

		body := map[string]any{
			"slugs":    []string{"groups.spawn"},
			"duration": "5m",
			"target":   targetConv,
		}
		r := agentd.AsAgentPeer(testharness.JSONRequest(t,
			http.MethodPost, "/v1/sudo", body), callerConv)
		rec := testharness.Serve(f.Mux, r)
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"agent + target body=%s", rec.Body.String())
		rows, _ := db.ListActiveSudoGrants(targetConv)
		assert.Empty(t, rows, "agent + target must not insert rows on the target")
	})
}

// Scenario: agent sudoes groups.create then creates a group. The
// auto-granted ownership row carries `via-sudo:grant-id=<N>` in
// granted_by, so a forensic query "what did agent X do during the
// elevation window?" can spot the row by LIKE-matching the annotation.
//
// Pins the audit-string annotation across the requirePermission →
// downstream-write boundary. Without the annotation, an op carried
// out under sudo looks identical to one carried out via a normal
// permission grant, defeating the purpose of the time-bounded audit
// trail.
func TestSudo_DownstreamAuditAnnotation_GroupsCreateOwner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
		t.Cleanup(restoreURL)
		restoreApproval := agentd.StubApprovalForTest(true)
		t.Cleanup(restoreApproval)

		f := newFlow(t)
		const conv = "aud-aaaa-bbbb-cccc-1111"
		f.HaveConvWithTitle(conv, "worker")

		// 1. Sudo for groups.create. Worker has NO non-sudo source for
		// the slug — that's the precondition for the via-sudo annotation
		// (agents with default-granted slugs aren't elevated; auditedCaller
		// returns plain conv-id for them).
		sudoBody := map[string]any{
			"slugs":    []string{"groups.create"},
			"duration": "5m",
		}
		sudoReq := agentd.AsAgentPeer(testharness.JSONRequest(t,
			http.MethodPost, "/v1/sudo", sudoBody), conv)
		sudoRec := testharness.Serve(f.Mux, sudoReq)
		require.Equal(t, http.StatusOK, sudoRec.Code,
			"sudo request body=%s", sudoRec.Body.String())
		var sudoResp struct {
			Grants []struct {
				ID int64 `json:"id"`
			} `json:"grants"`
		}
		_ = json.Unmarshal(sudoRec.Body.Bytes(), &sudoResp)
		require.Len(t, sudoResp.Grants, 1, "grants")
		require.NotZero(t, sudoResp.Grants[0].ID, "grant id")
		grantID := sudoResp.Grants[0].ID

		// 2. Create a group. The auto-owner write inside handleGroupCreate
		// goes through auditedCaller(creator, PermGroupsCreate) and should
		// therefore stamp the via-sudo annotation onto the granted_by
		// column.
		createBody := map[string]any{"name": "team-via-sudo"}
		createReq := agentd.AsAgentPeer(testharness.JSONRequest(t,
			http.MethodPost, "/v1/groups", createBody), conv)
		createRec := testharness.Serve(f.Mux, createReq)
		require.Equal(t, http.StatusCreated, createRec.Code,
			"groups create under sudo body=%s", createRec.Body.String())

		// 3. Inspect the auto-granted ownership row.
		g, err := db.GetAgentGroupByName("team-via-sudo")
		require.NoError(t, err, "get group")
		require.NotNil(t, g, "get group nil")
		owners, err := db.ListAgentGroupOwners(g.ID)
		require.NoError(t, err, "ListAgentGroupOwners")
		require.Len(t, owners, 1, "owner rows")
		want := fmt.Sprintf("%s:via-sudo:grant-id=%d", conv, grantID)
		assert.Equal(t, want, owners[0].GrantedBy, "granted_by")
	})
}

// Scenario: agent has groups.create via the normal permission_overrides
// path (agent_permissions row), then creates a group. The auto-owner's
// granted_by is the plain conv-id — NO via-sudo annotation, because
// the call didn't need an elevation. Pins the "only annotate when
// sudo was actually load-bearing" rule.
func TestSudo_DownstreamAuditAnnotation_NoSudoNoAnnotation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)
		const conv = "nau-aaaa-bbbb-cccc-1111"
		f.HaveConvWithTitle(conv, "trusted-worker")
		require.NoError(t, db.GrantAgentPermission(conv, "groups.create", "<test>"), "seed permission")

		createBody := map[string]any{"name": "team-no-sudo"}
		createReq := agentd.AsAgentPeer(testharness.JSONRequest(t,
			http.MethodPost, "/v1/groups", createBody), conv)
		createRec := testharness.Serve(f.Mux, createReq)
		require.Equal(t, http.StatusCreated, createRec.Code,
			"groups create with permission row body=%s", createRec.Body.String())

		g, err := db.GetAgentGroupByName("team-no-sudo")
		require.NoError(t, err, "get group")
		require.NotNil(t, g, "get group nil")
		owners, _ := db.ListAgentGroupOwners(g.ID)
		require.Len(t, owners, 1, "owner rows")
		assert.Equal(t, conv, owners[0].GrantedBy,
			"granted_by should be plain conv-id (no via-sudo annotation when sudo wasn't load-bearing)")
	})
}

// Scenario: config-supplied blocklist replaces the hardcoded default.
// A slug that v1 hardcoded as blocked (permissions.grant) becomes
// allowable when config sets blocklist to a different list — and a
// fresh entry the human added (groups.own) becomes blocked. Pins the
// "replace, not merge" semantics: the human is fully in control.
func TestSudo_ConfigBlocklist_ReplacesDefaults(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
		assert.Equal(t, http.StatusOK, allowRec.Code,
			"config replaced blocklist; permissions.grant should be allowable now: body=%s",
			allowRec.Body.String())

		// groups.own — newly blocked by config. Should 403 without popup.
		denyBody := map[string]any{"slugs": []string{"groups.own"}, "duration": "5m"}
		denyReq := agentd.AsAgentPeer(testharness.JSONRequest(t,
			http.MethodPost, "/v1/sudo", denyBody), conv)
		denyRec := testharness.Serve(f.Mux, denyReq)
		assert.Equal(t, http.StatusForbidden, denyRec.Code,
			"groups.own newly blocked by config body=%s", denyRec.Body.String())
		assert.Contains(t, denyRec.Body.String(), "groups.own",
			"error must name the blocked slug")
	})
}
