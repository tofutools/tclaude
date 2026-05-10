package agentd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

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
	if err != nil {
		t.Fatalf("seed grant 1: %v", err)
	}
	id2, err := db.InsertSudoGrant(&db.SudoGrant{
		ConvID:    "alice",
		Slug:      "member.add",
		GrantedAt: now,
		ExpiresAt: now.Add(10 * time.Minute),
		GrantedBy: "human:popup-id=test",
	})
	if err != nil {
		t.Fatalf("seed grant 2: %v", err)
	}

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodDelete, "/api/sudo/"+strconv.FormatInt(id1, 10), "")
	handleDashboardSudoAPI(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", w.Code, w.Body.String())
	}
	rows, _ := db.ListActiveSudoGrants("alice")
	if len(rows) != 1 || rows[0].ID != id2 {
		t.Errorf("after revoke id=%d: got %d rows, want 1 (id=%d); rows=%+v",
			id1, len(rows), id2, rows)
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
	if err := db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID: aliceConv, CustomTitle: "alice", IndexedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed alice conv_index: %v", err)
	}
	if err := db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID: bobConv, CustomTitle: "bob", IndexedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed bob conv_index: %v", err)
	}

	now := time.Now()
	// Two grants for alice, one for bob — only alice's should die.
	for _, slug := range []string{"groups.spawn", "member.add"} {
		if _, err := db.InsertSudoGrant(&db.SudoGrant{
			ConvID: aliceConv, Slug: slug,
			GrantedAt: now, ExpiresAt: now.Add(10 * time.Minute),
			GrantedBy: "human:popup-id=test",
		}); err != nil {
			t.Fatalf("seed alice grant %s: %v", slug, err)
		}
	}
	if _, err := db.InsertSudoGrant(&db.SudoGrant{
		ConvID: bobConv, Slug: "groups.spawn",
		GrantedAt: now, ExpiresAt: now.Add(10 * time.Minute),
		GrantedBy: "human:popup-id=test",
	}); err != nil {
		t.Fatalf("seed bob grant: %v", err)
	}

	w := httptest.NewRecorder()
	// Use the alice conv-id directly as the selector. ResolveSelector
	// matches a full UUID-shape on conv-id.
	r := dashboardRequest(http.MethodDelete, "/api/sudo?conv="+aliceConv, "")
	handleDashboardSudoAPI(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", w.Code, w.Body.String())
	}

	if rows, _ := db.ListActiveSudoGrants(aliceConv); len(rows) != 0 {
		t.Errorf("alice after bulk revoke: got %d rows, want 0", len(rows))
	}
	if rows, _ := db.ListActiveSudoGrants(bobConv); len(rows) != 1 {
		t.Errorf("bob after alice's bulk revoke (collateral): got %d rows, want 1", len(rows))
	}
}

// TestDashboardSudo_RevokeAll nukes every active grant. Pins the
// emergency-stop path (all-grants kill switch).
func TestDashboardSudo_RevokeAll(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	now := time.Now()
	for _, conv := range []string{"alice", "bob", "carol"} {
		if _, err := db.InsertSudoGrant(&db.SudoGrant{
			ConvID: conv, Slug: "groups.spawn",
			GrantedAt: now, ExpiresAt: now.Add(10 * time.Minute),
			GrantedBy: "human:popup-id=test",
		}); err != nil {
			t.Fatalf("seed %s grant: %v", conv, err)
		}
	}

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodDelete, "/api/sudo?all=1", "")
	handleDashboardSudoAPI(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", w.Code, w.Body.String())
	}

	rows, _ := db.ListAllActiveSudoGrants()
	if len(rows) != 0 {
		t.Errorf("after all-revoke: got %d rows, want 0", len(rows))
	}
}

// TestDashboardSudo_BadRequest_NoSelector pins the validation: a
// bare DELETE /api/sudo (no id, no ?conv=, no ?all=1) should fail
// before any DB writes — otherwise a copy-paste typo in the JS
// could become an accidental nuke.
func TestDashboardSudo_BadRequest_NoSelector(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	now := time.Now()
	if _, err := db.InsertSudoGrant(&db.SudoGrant{
		ConvID: "alice", Slug: "groups.spawn",
		GrantedAt: now, ExpiresAt: now.Add(10 * time.Minute),
		GrantedBy: "human:popup-id=test",
	}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodDelete, "/api/sudo", "")
	handleDashboardSudoAPI(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d body=%s, want 400", w.Code, w.Body.String())
	}
	if rows, _ := db.ListActiveSudoGrants("alice"); len(rows) != 1 {
		t.Errorf("invalid request must not touch existing grants; got %d rows after, want 1",
			len(rows))
	}
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
	if err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	w := httptest.NewRecorder()
	// Request without cookie / Origin.
	r := httptest.NewRequest(http.MethodDelete, "/api/sudo/"+strconv.FormatInt(id, 10), nil)
	handleDashboardSudoAPI(w, r)
	if w.Code == http.StatusOK {
		t.Errorf("unauth DELETE succeeded; status = %d, want non-200", w.Code)
	}
	if rows, _ := db.ListActiveSudoGrants("alice"); len(rows) != 1 {
		t.Errorf("unauth call must not touch grants; got %d rows, want 1", len(rows))
	}
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
	if err := db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: gID, ConvID: aliceConv, Alias: "alice",
	}); err != nil {
		t.Fatalf("AddAgentGroupMember: %v", err)
	}

	now := time.Now()
	grantID, err := db.InsertSudoGrant(&db.SudoGrant{
		ConvID:    aliceConv,
		Slug:      "groups.spawn",
		GrantedAt: now,
		ExpiresAt: now.Add(15 * time.Minute),
		GrantedBy: "human:popup-id=test",
		Reason:    "team-bootstrap",
	})
	if err != nil {
		t.Fatalf("InsertSudoGrant: %v", err)
	}

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodGet, "/api/snapshot", "")
	handleDashboardSnapshot(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("snapshot status = %d body=%s, want 200", w.Code, w.Body.String())
	}

	var got snapshotPayload
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}

	// Top-level Sudo carries the row.
	if len(got.Sudo) != 1 {
		t.Fatalf("snapshot Sudo: got %d entries, want 1; body=%s", len(got.Sudo), w.Body.String())
	}
	if got.Sudo[0].ID != grantID {
		t.Errorf("snapshot Sudo[0].ID = %d, want %d", got.Sudo[0].ID, grantID)
	}
	if got.Sudo[0].ConvID != aliceConv {
		t.Errorf("snapshot Sudo[0].ConvID = %q, want %q", got.Sudo[0].ConvID, aliceConv)
	}
	if got.Sudo[0].Slug != "groups.spawn" {
		t.Errorf("snapshot Sudo[0].Slug = %q, want groups.spawn", got.Sudo[0].Slug)
	}
	if got.Sudo[0].RemainingSeconds <= 0 {
		t.Errorf("snapshot Sudo[0].RemainingSeconds = %d, want positive", got.Sudo[0].RemainingSeconds)
	}

	// Per-agent ActiveSudo surfaces on alice's row, omits ConvID
	// (caller already knows who).
	var alice *dashboardAgent
	for i := range got.Agents {
		if got.Agents[i].ConvID == aliceConv {
			alice = &got.Agents[i]
			break
		}
	}
	if alice == nil {
		t.Fatalf("alice agent row not found in snapshot; agents=%+v", got.Agents)
	}
	if len(alice.ActiveSudo) != 1 {
		t.Fatalf("alice.ActiveSudo: got %d entries, want 1", len(alice.ActiveSudo))
	}
	if alice.ActiveSudo[0].ID != grantID {
		t.Errorf("alice.ActiveSudo[0].ID = %d, want %d", alice.ActiveSudo[0].ID, grantID)
	}
	if alice.ActiveSudo[0].ConvID != "" {
		t.Errorf("alice.ActiveSudo[0].ConvID = %q, want empty (already implied by row)",
			alice.ActiveSudo[0].ConvID)
	}
	if !strings.Contains(alice.ActiveSudo[0].Slug, "groups.spawn") {
		t.Errorf("alice.ActiveSudo[0].Slug = %q, want groups.spawn",
			alice.ActiveSudo[0].Slug)
	}
}

// TestSnapshot_ActiveSudoEmptyByDefault pins the no-grants case:
// agents without any active grants get either no ActiveSudo field
// at all (omitempty) or an empty slice. Either way nothing extra
// surfaces in the JSON; the top-level Sudo[] is empty (not null).
func TestSnapshot_ActiveSudoEmptyByDefault(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	gID, _ := db.CreateAgentGroup("team", "")
	if err := db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: gID, ConvID: "alice", Alias: "alice",
	}); err != nil {
		t.Fatalf("AddAgentGroupMember: %v", err)
	}

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodGet, "/api/snapshot", "")
	handleDashboardSnapshot(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("snapshot status = %d body=%s, want 200", w.Code, w.Body.String())
	}

	// The top-level Sudo[] field MUST serialize as [] (not null) so
	// the dashboard's JS can call .length on it without a guard.
	if !strings.Contains(w.Body.String(), `"sudo":[]`) {
		t.Errorf("snapshot must surface Sudo as [] when empty; body=%s", w.Body.String())
	}
}
