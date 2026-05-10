package agentd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// dashboardRequest builds a request with the dashboard cookie + a
// matching Origin so checkDashboardAuth passes. Tests bypass the
// real popupBaseURL bind by setting both vars directly.
func dashboardRequest(method, path string, body string) *http.Request {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.Header.Set("Origin", popupBaseURL)
	r.AddCookie(&http.Cookie{Name: dashboardCookieName, Value: dashboardSessionToken})
	return r
}

// withDashboardAuth swaps in test values for the global cookie/origin
// state and restores them on cleanup. Required because checkDashboardAuth
// short-circuits if dashboardSessionToken is empty.
func withDashboardAuth(t *testing.T) {
	t.Helper()
	prevToken := dashboardSessionToken
	prevURL := popupBaseURL
	dashboardSessionToken = "test-token"
	popupBaseURL = "http://127.0.0.1:0"
	t.Cleanup(func() {
		dashboardSessionToken = prevToken
		popupBaseURL = prevURL
	})
}

func TestDashboardEdit_RemoveMember(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "worker", Alias: "w"})

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodDelete, "/api/groups/team/members/w", "")
	handleDashboardGroupsAPI(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	members, _ := db.ListAgentGroupMembers(gID)
	if len(members) != 0 {
		t.Errorf("expected member removed; got %d remaining", len(members))
	}
}

func TestDashboardEdit_GrantOwner(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "worker", Alias: "w"})

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodPost, "/api/groups/team/owners", `{"conv":"w"}`)
	handleDashboardGroupsAPI(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	owners, _ := db.ListAgentGroupOwners(gID)
	if len(owners) != 1 || owners[0].ConvID != "worker" {
		t.Errorf("expected worker as owner, got %+v", owners)
	}
	if owners[0].GrantedBy != dashboardGranter {
		t.Errorf("granted_by = %q, want %q", owners[0].GrantedBy, dashboardGranter)
	}
}

func TestDashboardEdit_RevokeOwner(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "worker", Alias: "w"})
	_ = db.AddAgentGroupOwner(gID, "worker", "<test>")

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodDelete, "/api/groups/team/owners/w", "")
	handleDashboardGroupsAPI(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	owners, _ := db.ListAgentGroupOwners(gID)
	if len(owners) != 0 {
		t.Errorf("expected owner revoked; got %d remaining", len(owners))
	}
}

func TestDashboardEdit_RevokeOwner_NotAnOwner(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "worker", Alias: "w"})
	// no owner row

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodDelete, "/api/groups/team/owners/w", "")
	handleDashboardGroupsAPI(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestDashboardEdit_DeleteGroup(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "worker", Alias: "w"})
	_ = db.AddAgentGroupOwner(gID, "worker", "<test>")

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodDelete, "/api/groups/team", "")
	handleDashboardGroupsAPI(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	g, _ := db.GetAgentGroupByName("team")
	if g != nil {
		t.Errorf("expected group deleted; still exists: %+v", g)
	}
}

// PATCH /api/groups/{name}/members/{conv} with one or more of
// alias/role/descr updates the row. Verifies the dashboard mirror
// of the /v1 PATCH that the CLI's `groups update-member` already
// uses — ensures edits initiated from the dashboard land in the
// same agent_group_members row the CLI would touch.
func TestDashboardEdit_UpdateMember(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: gID, ConvID: "worker", Alias: "old-alias", Role: "old-role", Descr: "old descr",
	})

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodPatch, "/api/groups/team/members/worker",
		`{"alias":"new-alias","role":"new-role","descr":"new descr"}`)
	handleDashboardGroupsAPI(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	members, _ := db.ListAgentGroupMembers(gID)
	if len(members) != 1 {
		t.Fatalf("expected 1 member, got %d", len(members))
	}
	got := members[0]
	if got.Alias != "new-alias" || got.Role != "new-role" || got.Descr != "new descr" {
		t.Errorf("update did not land; got %+v", got)
	}
}

// PATCH with only one field touches only that field — the others
// stay at their current values. Mirrors the daemon's nil-as-leave-
// alone semantics so the dashboard's "only send changed fields"
// optimization is safe.
func TestDashboardEdit_UpdateMember_PartialFields(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: gID, ConvID: "worker", Alias: "stay-alias", Role: "stay-role", Descr: "stay-descr",
	})

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodPatch, "/api/groups/team/members/worker",
		`{"role":"only-role-changed"}`)
	handleDashboardGroupsAPI(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	members, _ := db.ListAgentGroupMembers(gID)
	got := members[0]
	if got.Role != "only-role-changed" {
		t.Errorf("role should be updated; got %q", got.Role)
	}
	if got.Alias != "stay-alias" || got.Descr != "stay-descr" {
		t.Errorf("untouched fields should remain; got alias=%q descr=%q", got.Alias, got.Descr)
	}
}

// PATCH with an empty body (or all-nil fields) → 400. Pins the
// "at least one field is required" rule so a buggy UI that sends
// {} doesn't silently no-op.
func TestDashboardEdit_UpdateMember_EmptyBodyIs400(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "worker", Alias: "a"})

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodPatch, "/api/groups/team/members/worker", `{}`)
	handleDashboardGroupsAPI(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("empty body: status = %d, want 400", w.Code)
	}
}

// PATCH on a missing member → 404 (not 200 with zero rows updated).
// Pins the "no such member" surface so a typo'd selector is loud.
func TestDashboardEdit_UpdateMember_MissingIs404(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	_, _ = db.CreateAgentGroup("team", "")

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodPatch, "/api/groups/team/members/no-such-conv",
		`{"alias":"x"}`)
	handleDashboardGroupsAPI(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("missing member: status = %d, want 404", w.Code)
	}
}

func TestDashboardEdit_RenameGroup(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "worker", Alias: "w"})

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodPost, "/api/groups/team/rename", `{"new_name":"team-renamed"}`)
	handleDashboardGroupsAPI(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got, _ := db.GetAgentGroupByName("team"); got != nil {
		t.Errorf("old name should 404 after rename; got %+v", got)
	}
	got, _ := db.GetAgentGroupByName("team-renamed")
	if got == nil || got.ID != gID {
		t.Errorf("new name should resolve to same id %d; got %+v", gID, got)
	}
	// Members survived via stable id.
	members, _ := db.ListAgentGroupMembers(gID)
	if len(members) != 1 {
		t.Errorf("members should survive dashboard rename; got %d", len(members))
	}
}

func TestDashboardEdit_RenameGroup_Collision(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	_, _ = db.CreateAgentGroup("team", "")
	_, _ = db.CreateAgentGroup("team-renamed", "")

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodPost, "/api/groups/team/rename", `{"new_name":"team-renamed"}`)
	handleDashboardGroupsAPI(w, r)
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
}

func TestDashboardEdit_RenameGroup_BadName(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	_, _ = db.CreateAgentGroup("team", "")
	for _, bad := range []string{`{"new_name":""}`, `{"new_name":"has/slash"}`, "{\"new_name\":\"\x01ctrl\"}"} {
		w := httptest.NewRecorder()
		r := dashboardRequest(http.MethodPost, "/api/groups/team/rename", bad)
		handleDashboardGroupsAPI(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", bad, w.Code)
		}
		// team untouched.
		if got, _ := db.GetAgentGroupByName("team"); got == nil {
			t.Fatalf("team should still exist after rejecting %q", bad)
		}
	}
}

func TestDashboardEdit_DeleteGroup_NotFound(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodDelete, "/api/groups/nope", "")
	handleDashboardGroupsAPI(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestDashboardEdit_DeleteGroup_WrongMethod(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	_, _ = db.CreateAgentGroup("team", "")

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodPost, "/api/groups/team", "")
	handleDashboardGroupsAPI(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestDashboardEdit_DeleteAgent_OrphanCleanup(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	// Simulate the bug: an agent whose conv-index row is gone (e.g.
	// previously deleted via `conv rm`) but whose membership /
	// ownership / permission rows are still in the DB. The dashboard
	// shows it as "(unknown)" and the user clicks delete to clean it
	// up. Endpoint should accept the raw UUID, skip the conv wipe
	// (it's already gone), and drop the orphan rows.
	const orphanConv = "ab887fe0-3816-4a8f-a2f4-1c2607405f9e"
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: orphanConv, Alias: "ghost"})
	_ = db.AddAgentGroupOwner(gID, orphanConv, "<test>")
	_ = db.GrantAgentPermission(orphanConv, "self.clone", "<test>")

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodDelete, "/api/agents/"+orphanConv, "")
	handleDashboardAgentsAPI(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("orphan delete: status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if members, _ := db.ListAgentGroupMembers(gID); len(members) != 0 {
		t.Errorf("expected membership row dropped; got %d", len(members))
	}
	if owners, _ := db.ListAgentGroupOwners(gID); len(owners) != 0 {
		t.Errorf("expected ownership row dropped; got %d", len(owners))
	}
	if perms, _ := db.ListAgentPermissionsForConv(orphanConv); len(perms) != 0 {
		t.Errorf("expected permission rows dropped; got %d", len(perms))
	}
}

func TestDashboardEdit_DeleteAgent_RejectsNonUUIDInput(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	// Defence-in-depth: the orphan-cleanup path only accepts UUID-
	// shaped input, so a junk selector that doesn't resolve gets a
	// 404 instead of running DELETE WHERE conv_id = '<arbitrary>'.
	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodDelete, "/api/agents/not-a-uuid", "")
	handleDashboardAgentsAPI(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("non-UUID input: status = %d, want 404", w.Code)
	}
}

func TestDashboardEdit_DeleteAgent_WrongMethod(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodGet, "/api/agents/00000000-1111-2222-3333-444444444444", "")
	handleDashboardAgentsAPI(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestDashboardEdit_DeleteAgent_MissingConv(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodDelete, "/api/agents/", "")
	handleDashboardAgentsAPI(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestDashboardEdit_Jump_NoLiveSession(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	// No conv with this id has been indexed — resolver fails first.
	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodPost, "/api/jump/00000000-1111-2222-3333-444444444444", "")
	handleDashboardJumpAPI(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestDashboardEdit_Jump_WrongMethod(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodGet, "/api/jump/00000000-1111-2222-3333-444444444444", "")
	handleDashboardJumpAPI(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestDashboardEdit_Jump_MissingConv(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodPost, "/api/jump/", "")
	handleDashboardJumpAPI(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestDashboardEdit_NoCookieRefused(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodDelete, "/api/groups/team/members/w", nil)
	r.Header.Set("Origin", popupBaseURL)
	// no cookie added
	handleDashboardGroupsAPI(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("missing cookie should 403; got %d", w.Code)
	}
}

func TestDashboardEdit_BadOriginRefused(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodDelete, "/api/groups/team/members/w", nil)
	r.Header.Set("Origin", "http://evil.example.com")
	r.AddCookie(&http.Cookie{Name: dashboardCookieName, Value: dashboardSessionToken})
	handleDashboardGroupsAPI(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("bad Origin should 403; got %d", w.Code)
	}
}
