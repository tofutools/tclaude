package agentd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.Equal(t, http.StatusNoContent, w.Code, "body=%s", w.Body.String())
	members, _ := db.ListAgentGroupMembers(gID)
	assert.Empty(t, members, "expected member removed")
}

func TestDashboardEdit_GrantOwner(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "worker", Alias: "w"})

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodPost, "/api/groups/team/owners", `{"conv":"w"}`)
	handleDashboardGroupsAPI(w, r)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	owners, _ := db.ListAgentGroupOwners(gID)
	if assert.Len(t, owners, 1, "expected one owner") {
		assert.Equal(t, "worker", owners[0].ConvID, "owner conv")
		assert.Equal(t, dashboardGranter, owners[0].GrantedBy, "granted_by")
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
	require.Equal(t, http.StatusNoContent, w.Code, "body=%s", w.Body.String())
	owners, _ := db.ListAgentGroupOwners(gID)
	assert.Empty(t, owners, "expected owner revoked")
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
	assert.Equal(t, http.StatusNotFound, w.Code, "status")
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
	require.Equal(t, http.StatusNoContent, w.Code, "body=%s", w.Body.String())
	g, _ := db.GetAgentGroupByName("team")
	assert.Nil(t, g, "expected group deleted; still exists: %+v", g)
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
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	members, _ := db.ListAgentGroupMembers(gID)
	require.Len(t, members, 1, "expected 1 member")
	got := members[0]
	assert.Equal(t, "new-alias", got.Alias, "alias")
	assert.Equal(t, "new-role", got.Role, "role")
	assert.Equal(t, "new descr", got.Descr, "descr")
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
	require.Equal(t, http.StatusOK, w.Code, "status")
	members, _ := db.ListAgentGroupMembers(gID)
	got := members[0]
	assert.Equal(t, "only-role-changed", got.Role, "role should be updated")
	assert.Equal(t, "stay-alias", got.Alias, "alias untouched")
	assert.Equal(t, "stay-descr", got.Descr, "descr untouched")
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
	assert.Equal(t, http.StatusBadRequest, w.Code, "empty body")
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
	assert.Equal(t, http.StatusNotFound, w.Code, "missing member")
}

func TestDashboardEdit_RenameGroup(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "worker", Alias: "w"})

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodPost, "/api/groups/team/rename", `{"new_name":"team-renamed"}`)
	handleDashboardGroupsAPI(w, r)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	oldGroup, _ := db.GetAgentGroupByName("team")
	assert.Nil(t, oldGroup, "old name should 404 after rename")
	got, _ := db.GetAgentGroupByName("team-renamed")
	if assert.NotNil(t, got, "new name should resolve") {
		assert.Equal(t, gID, got.ID, "new name should resolve to same id")
	}
	// Members survived via stable id.
	members, _ := db.ListAgentGroupMembers(gID)
	assert.Len(t, members, 1, "members should survive dashboard rename")
}

func TestDashboardEdit_RenameGroup_Collision(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	_, _ = db.CreateAgentGroup("team", "")
	_, _ = db.CreateAgentGroup("team-renamed", "")

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodPost, "/api/groups/team/rename", `{"new_name":"team-renamed"}`)
	handleDashboardGroupsAPI(w, r)
	assert.Equal(t, http.StatusConflict, w.Code, "status")
}

func TestDashboardEdit_RenameGroup_BadName(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	_, _ = db.CreateAgentGroup("team", "")
	for _, bad := range []string{`{"new_name":""}`, `{"new_name":"has/slash"}`, "{\"new_name\":\"\x01ctrl\"}"} {
		w := httptest.NewRecorder()
		r := dashboardRequest(http.MethodPost, "/api/groups/team/rename", bad)
		handleDashboardGroupsAPI(w, r)
		assert.Equal(t, http.StatusBadRequest, w.Code, "body %q", bad)
		// team untouched.
		got, _ := db.GetAgentGroupByName("team")
		require.NotNil(t, got, "team should still exist after rejecting %q", bad)
	}
}

func TestDashboardEdit_DeleteGroup_NotFound(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodDelete, "/api/groups/nope", "")
	handleDashboardGroupsAPI(w, r)
	assert.Equal(t, http.StatusNotFound, w.Code, "status")
}

func TestDashboardEdit_DeleteGroup_WrongMethod(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	_, _ = db.CreateAgentGroup("team", "")

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodPost, "/api/groups/team", "")
	handleDashboardGroupsAPI(w, r)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code, "status")
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
	require.Equal(t, http.StatusNoContent, w.Code,
		"orphan delete body=%s", w.Body.String())
	members, _ := db.ListAgentGroupMembers(gID)
	assert.Empty(t, members, "expected membership row dropped")
	owners, _ := db.ListAgentGroupOwners(gID)
	assert.Empty(t, owners, "expected ownership row dropped")
	perms, _ := db.ListAgentPermissionsForConv(orphanConv)
	assert.Empty(t, perms, "expected permission rows dropped")
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
	assert.Equal(t, http.StatusNotFound, w.Code, "non-UUID input")
}

func TestDashboardEdit_DeleteAgent_WrongMethod(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodGet, "/api/agents/00000000-1111-2222-3333-444444444444", "")
	handleDashboardAgentsAPI(w, r)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code, "status")
}

// POST /api/agents/{conv}/stop on an offline conv → 200 with action
// "skipped:already_offline". Pins the dashboard route as a thin
// pass-through to stopOneConv (the same helper /v1/agent/{conv}/stop
// uses) without exercising the side-effecting tmux send-keys path.
func TestDashboardEdit_StopAgent_OfflineSkipped(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	const convID = "11111111-2222-3333-4444-555555555555"
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{ConvID: convID, CustomTitle: "alice"}), "UpsertConvIndex")

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodPost, "/api/agents/"+convID+"/stop", "")
	handleDashboardAgentsAPI(w, r)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	body := w.Body.String()
	assert.Contains(t, body, "skipped:already_offline", "body should announce already_offline")
}

// Stop on an unresolvable selector → 404.
func TestDashboardEdit_StopAgent_NotFound(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodPost, "/api/agents/no-such-conv/stop", "")
	handleDashboardAgentsAPI(w, r)
	assert.Equal(t, http.StatusNotFound, w.Code, "status")
}

// Resume on an unresolvable selector → 404. (We don't exercise the
// happy path here since it would call SpawnDetachedTclaudeResume,
// which spawns a real subprocess in unit-test scope.)
func TestDashboardEdit_ResumeAgent_NotFound(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodPost, "/api/agents/no-such-conv/resume", "")
	handleDashboardAgentsAPI(w, r)
	assert.Equal(t, http.StatusNotFound, w.Code, "status")
}

// GET on the lifecycle subpaths → 405.
func TestDashboardEdit_StopResume_WrongMethod(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	const convID = "11111111-2222-3333-4444-555555555555"
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{ConvID: convID, CustomTitle: "alice"}), "UpsertConvIndex")

	for _, verb := range []string{"stop", "resume"} {
		w := httptest.NewRecorder()
		r := dashboardRequest(http.MethodGet, "/api/agents/"+convID+"/"+verb, "")
		handleDashboardAgentsAPI(w, r)
		assert.Equal(t, http.StatusMethodNotAllowed, w.Code, "GET %s", verb)
	}
}

func TestDashboardEdit_DeleteAgent_MissingConv(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodDelete, "/api/agents/", "")
	handleDashboardAgentsAPI(w, r)
	assert.Equal(t, http.StatusNotFound, w.Code, "status")
}

func TestDashboardEdit_Jump_NoLiveSession(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	// No conv with this id has been indexed — resolver fails first.
	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodPost, "/api/jump/00000000-1111-2222-3333-444444444444", "")
	handleDashboardJumpAPI(w, r)
	assert.Equal(t, http.StatusNotFound, w.Code, "body=%s", w.Body.String())
}

func TestDashboardEdit_Jump_WrongMethod(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodGet, "/api/jump/00000000-1111-2222-3333-444444444444", "")
	handleDashboardJumpAPI(w, r)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code, "status")
}

func TestDashboardEdit_Jump_MissingConv(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	w := httptest.NewRecorder()
	r := dashboardRequest(http.MethodPost, "/api/jump/", "")
	handleDashboardJumpAPI(w, r)
	assert.Equal(t, http.StatusNotFound, w.Code, "status")
}

func TestDashboardEdit_NoCookieRefused(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodDelete, "/api/groups/team/members/w", nil)
	r.Header.Set("Origin", popupBaseURL)
	// no cookie added
	handleDashboardGroupsAPI(w, r)
	assert.Equal(t, http.StatusForbidden, w.Code, "missing cookie should 403")
}

func TestDashboardEdit_BadOriginRefused(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodDelete, "/api/groups/team/members/w", nil)
	r.Header.Set("Origin", "http://evil.example.com")
	r.AddCookie(&http.Cookie{Name: dashboardCookieName, Value: dashboardSessionToken})
	handleDashboardGroupsAPI(w, r)
	assert.Equal(t, http.StatusForbidden, w.Code, "bad Origin should 403")
}
