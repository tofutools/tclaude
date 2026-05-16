package agentd

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
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// TestDashboardSudo_RevokeByID exercises the cookie-auth twin of
// DELETE /v1/sudo/{id}. Pins the per-grant revoke path: the dashboard
// should kill one grant by id, leaving sibling grants for the same
// conv intact.
func TestDashboardSudo_RevokeByID(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	now := time.Now()
	id1, err := db.InsertSudoGrant(&db.SudoGrant{
		ConvID:    "alice",
		Slug:      "groups.spawn",
		GrantedAt: now,
		ExpiresAt: now.Add(10 * time.Minute),
		GrantedBy: "human:popup-id=test",
	})
	require.NoError(t, err, "seed grant 1")
	id2, err := db.InsertSudoGrant(&db.SudoGrant{
		ConvID:    "alice",
		Slug:      "member.add",
		GrantedAt: now,
		ExpiresAt: now.Add(10 * time.Minute),
		GrantedBy: "human:popup-id=test",
	})
	require.NoError(t, err, "seed grant 2")

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodDelete, "/api/sudo/"+strconv.FormatInt(id1, 10), "")
	handleDashboardSudoAPI(w, r)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	rows, _ := db.ListActiveSudoGrants("alice")
	if assert.Len(t, rows, 1, "after revoke id=%d (id2=%d); rows=%+v", id1, id2, rows) {
		assert.Equal(t, id2, rows[0].ID, "remaining row")
	}
}

// TestDashboardSudo_RevokeByConv kills every active grant for one
// conv selector. Pins the bulk-per-conv path: targeted incident
// response (a single agent went off the rails, kill ALL its
// elevations).
func TestDashboardSudo_RevokeByConv(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	const aliceConv = "alic-aaaa-bbbb-cccc-1111"
	const bobConv = "bobl-aaaa-bbbb-cccc-2222"
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID: aliceConv, CustomTitle: "alice", IndexedAt: time.Now(),
	}), "seed alice conv_index")
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID: bobConv, CustomTitle: "bob", IndexedAt: time.Now(),
	}), "seed bob conv_index")

	now := time.Now()
	// Two grants for alice, one for bob — only alice's should die.
	for _, slug := range []string{"groups.spawn", "member.add"} {
		_, err := db.InsertSudoGrant(&db.SudoGrant{
			ConvID: aliceConv, Slug: slug,
			GrantedAt: now, ExpiresAt: now.Add(10 * time.Minute),
			GrantedBy: "human:popup-id=test",
		})
		require.NoError(t, err, "seed alice grant %s", slug)
	}
	_, err := db.InsertSudoGrant(&db.SudoGrant{
		ConvID: bobConv, Slug: "groups.spawn",
		GrantedAt: now, ExpiresAt: now.Add(10 * time.Minute),
		GrantedBy: "human:popup-id=test",
	})
	require.NoError(t, err, "seed bob grant")

	w := httptest.NewRecorder()
	// Use the alice conv-id directly as the selector. ResolveSelector
	// matches a full UUID-shape on conv-id.
	r := dashboardRequest(http.MethodDelete, "/api/sudo?conv="+aliceConv, "")
	handleDashboardSudoAPI(w, r)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	aliceRows, _ := db.ListActiveSudoGrants(aliceConv)
	assert.Empty(t, aliceRows, "alice after bulk revoke")
	bobRows, _ := db.ListActiveSudoGrants(bobConv)
	assert.Len(t, bobRows, 1, "bob after alice's bulk revoke (collateral)")
}

// TestDashboardSudo_RevokeAll nukes every active grant. Pins the
// emergency-stop path (all-grants kill switch).
func TestDashboardSudo_RevokeAll(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	now := time.Now()
	for _, conv := range []string{"alice", "bob", "carol"} {
		_, err := db.InsertSudoGrant(&db.SudoGrant{
			ConvID: conv, Slug: "groups.spawn",
			GrantedAt: now, ExpiresAt: now.Add(10 * time.Minute),
			GrantedBy: "human:popup-id=test",
		})
		require.NoError(t, err, "seed %s grant", conv)
	}

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodDelete, "/api/sudo?all=1", "")
	handleDashboardSudoAPI(w, r)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	rows, _ := db.ListAllActiveSudoGrants()
	assert.Empty(t, rows, "after all-revoke")
}

// TestDashboardSudo_BadRequest_NoSelector pins the validation: a
// bare DELETE /api/sudo (no id, no ?conv=, no ?all=1) should fail
// before any DB writes — otherwise a copy-paste typo in the JS
// could become an accidental nuke.
func TestDashboardSudo_BadRequest_NoSelector(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	now := time.Now()
	_, err := db.InsertSudoGrant(&db.SudoGrant{
		ConvID: "alice", Slug: "groups.spawn",
		GrantedAt: now, ExpiresAt: now.Add(10 * time.Minute),
		GrantedBy: "human:popup-id=test",
	})
	require.NoError(t, err, "seed grant")

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodDelete, "/api/sudo", "")
	handleDashboardSudoAPI(w, r)
	assert.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	rows, _ := db.ListActiveSudoGrants("alice")
	assert.Len(t, rows, 1, "invalid request must not touch existing grants")
}

// TestDashboardSudo_AuthRequired pins the cookie-auth gate. A
// request without the dashboard cookie is rejected before any
// state changes.
func TestDashboardSudo_AuthRequired(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	now := time.Now()
	id, err := db.InsertSudoGrant(&db.SudoGrant{
		ConvID: "alice", Slug: "groups.spawn",
		GrantedAt: now, ExpiresAt: now.Add(10 * time.Minute),
		GrantedBy: "human:popup-id=test",
	})
	require.NoError(t, err, "seed grant")

	w := httptest.NewRecorder()
	// Request without cookie / Origin.
	r := httptest.NewRequest(http.MethodDelete, "/api/sudo/"+strconv.FormatInt(id, 10), nil)
	handleDashboardSudoAPI(w, r)
	assert.NotEqual(t, http.StatusOK, w.Code, "unauth DELETE succeeded")
	rows, _ := db.ListActiveSudoGrants("alice")
	assert.Len(t, rows, 1, "unauth call must not touch grants")
}

// TestSnapshot_ActiveSudoSurfaces pins the snapshot extension that
// powers the dashboard's per-row 🔓 indicator + dedicated tab. One
// active grant for one agent → that agent's row carries
// active_sudo[] AND the top-level Sudo[] mirrors it.
func TestSnapshot_ActiveSudoSurfaces(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	gID, _ := db.CreateAgentGroup("team", "")
	const aliceConv = "alic-aaaa-bbbb-cccc-1111"
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: gID, ConvID: aliceConv,
	}), "AddAgentGroupMember")

	now := time.Now()
	grantID, err := db.InsertSudoGrant(&db.SudoGrant{
		ConvID:    aliceConv,
		Slug:      "groups.spawn",
		GrantedAt: now,
		ExpiresAt: now.Add(15 * time.Minute),
		GrantedBy: "human:popup-id=test",
		Reason:    "team-bootstrap",
	})
	require.NoError(t, err, "InsertSudoGrant")

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodGet, "/api/snapshot", "")
	handleDashboardSnapshot(w, r)
	require.Equal(t, http.StatusOK, w.Code, "snapshot body=%s", w.Body.String())

	var got snapshotPayload
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got), "unmarshal snapshot")

	// Top-level Sudo carries the row.
	require.Len(t, got.Sudo, 1, "snapshot Sudo entries; body=%s", w.Body.String())
	assert.Equal(t, grantID, got.Sudo[0].ID, "snapshot Sudo[0].ID")
	assert.Equal(t, aliceConv, got.Sudo[0].ConvID, "snapshot Sudo[0].ConvID")
	assert.Equal(t, "groups.spawn", got.Sudo[0].Slug, "snapshot Sudo[0].Slug")
	assert.Greater(t, got.Sudo[0].RemainingSeconds, int64(0),
		"snapshot Sudo[0].RemainingSeconds should be positive")

	// Per-agent ActiveSudo surfaces on alice's row, omits ConvID
	// (caller already knows who).
	var alice *dashboardAgent
	for i := range got.Agents {
		if got.Agents[i].ConvID == aliceConv {
			alice = &got.Agents[i]
			break
		}
	}
	require.NotNil(t, alice, "alice agent row not found in snapshot; agents=%+v", got.Agents)
	require.Len(t, alice.ActiveSudo, 1, "alice.ActiveSudo entries")
	assert.Equal(t, grantID, alice.ActiveSudo[0].ID, "alice.ActiveSudo[0].ID")
	assert.Empty(t, alice.ActiveSudo[0].ConvID,
		"alice.ActiveSudo[0].ConvID should be empty (already implied by row)")
	assert.Contains(t, alice.ActiveSudo[0].Slug, "groups.spawn", "alice.ActiveSudo[0].Slug")
}

// TestDashboardSudo_GrantProactive exercises the cookie-auth twin
// of POST /v1/sudo: the human seeds a time-bounded grant from the
// dashboard without involving the popup. Pins the new path's
// audit label so a forensic query can tell proactive grants apart
// from agent-requested + popup-approved ones.
func TestDashboardSudo_GrantProactive(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	const conv = "alic-aaaa-bbbb-cccc-1111"
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID: conv, CustomTitle: "alice", IndexedAt: time.Now(),
	}), "seed conv_index")

	body := `{"conv":"` + conv + `","slugs":["groups.spawn"],"duration":"5m","reason":"bootstrap"}`
	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodPost, "/api/sudo", body)
	handleDashboardSudoAPI(w, r)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	rows, err := db.ListActiveSudoGrants(conv)
	require.NoError(t, err, "ListActiveSudoGrants")
	require.Len(t, rows, 1, "active grants")
	got := rows[0]
	assert.Equal(t, "groups.spawn", got.Slug, "slug")
	assert.Equal(t, "bootstrap", got.Reason, "reason")
	assert.Equal(t, dashboardSudoGranter, got.GrantedBy,
		"granted_by (proactive label distinguishes from popup-approved)")
	assert.True(t, got.IsActive(time.Now()),
		"freshly-inserted grant must be active; expires_at=%v revoked_at=%v",
		got.ExpiresAt, got.RevokedAt)
}

// TestDashboardSudo_GrantBlocklist pins the policy-applies rule.
// The same blocklist that protects agent-initiated /v1/sudo applies
// to the dashboard's POST — a human typo can't grant
// permissions.grant for "just 5 minutes" because the cap on
// recursive escalation is structural, not advisory.
func TestDashboardSudo_GrantBlocklist(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	const conv = "blok-aaaa-bbbb-cccc-1111"
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID: conv, CustomTitle: "alice", IndexedAt: time.Now(),
	}), "seed conv_index")

	body := `{"conv":"` + conv + `","slugs":["groups.spawn","permissions.grant"],"duration":"5m"}`
	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodPost, "/api/sudo", body)
	handleDashboardSudoAPI(w, r)
	assert.Equal(t, http.StatusForbidden, w.Code,
		"blocklisted POST body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "permissions.grant",
		"error must name the blocked slug")
	rows, _ := db.ListActiveSudoGrants(conv)
	assert.Empty(t, rows,
		"blocklisted POST must not insert ANY rows (even the non-blocked ones)")
}

// TestDashboardSudo_GrantDurationCap pins the cap rule: a dashboard
// POST exceeding sudoDefaultMaxDuration is rejected with 400 before
// any DB writes. Same value the agent-initiated path enforces.
func TestDashboardSudo_GrantDurationCap(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	const conv = "capd-aaaa-bbbb-cccc-1111"
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID: conv, CustomTitle: "alice", IndexedAt: time.Now(),
	}), "seed conv_index")

	body := `{"conv":"` + conv + `","slugs":["groups.spawn"],"duration":"24h"}`
	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodPost, "/api/sudo", body)
	handleDashboardSudoAPI(w, r)
	assert.Equal(t, http.StatusBadRequest, w.Code,
		"over-cap POST body=%s", w.Body.String())
	rows, _ := db.ListActiveSudoGrants(conv)
	assert.Empty(t, rows, "over-cap POST must not insert rows")
}

// TestDashboardSudo_GrantAuthRequired pins the cookie gate on the
// new POST path — without cookie + Origin, the request is refused
// before any state changes.
func TestDashboardSudo_GrantAuthRequired(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	const conv = "auth-aaaa-bbbb-cccc-1111"
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID: conv, CustomTitle: "alice", IndexedAt: time.Now(),
	}), "seed conv_index")

	body := `{"conv":"` + conv + `","slugs":["groups.spawn"],"duration":"5m"}`
	w := httptest.NewRecorder()
	// Request without cookie / Origin.
	r := httptest.NewRequest(http.MethodPost, "/api/sudo", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	handleDashboardSudoAPI(w, r)
	assert.NotEqual(t, http.StatusOK, w.Code, "unauth POST succeeded")
	rows, _ := db.ListActiveSudoGrants(conv)
	assert.Empty(t, rows, "unauth call must not insert grants")
}

// TestSnapshot_ActiveSudoEmptyByDefault pins the no-grants case:
// agents without any active grants get either no ActiveSudo field
// at all (omitempty) or an empty slice. Either way nothing extra
// surfaces in the JSON; the top-level Sudo[] is empty (not null).
func TestSnapshot_ActiveSudoEmptyByDefault(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	gID, _ := db.CreateAgentGroup("team", "")
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: gID, ConvID: "alice",
	}), "AddAgentGroupMember")

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodGet, "/api/snapshot", "")
	handleDashboardSnapshot(w, r)
	require.Equal(t, http.StatusOK, w.Code, "snapshot body=%s", w.Body.String())

	// The top-level Sudo[] field MUST serialize as [] (not null) so
	// the dashboard's JS can call .length on it without a guard.
	assert.Contains(t, w.Body.String(), `"sudo":[]`,
		"snapshot must surface Sudo as [] when empty")
}
