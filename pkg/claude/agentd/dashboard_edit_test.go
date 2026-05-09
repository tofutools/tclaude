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

func TestDashboardEdit_DeleteAgent_NotFound(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	// No conv with this id has been indexed; the resolver should fail
	// before DeleteConvByID gets called.
	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodDelete, "/api/agents/00000000-1111-2222-3333-444444444444", "")
	handleDashboardAgentsAPI(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", w.Code, w.Body.String())
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
