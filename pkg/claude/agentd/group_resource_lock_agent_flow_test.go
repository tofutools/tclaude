package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// An agent locks its group's drawing board under groups.resources.lock, which no
// membership or ownership confers. A locked drawing board stays readable to members
// and refuses their changes; the lock holder and the operator still change it.
func TestAgentsLockGroupResourcesUnderTheLockSlug(t *testing.T) {
	f := newFlow(t)
	const (
		member = "lock-member-1111"
		keeper = "lock-keeper-2222"
	)
	f.HaveConvWithTitle(member, "member")
	f.HaveConvWithTitle(keeper, "keeper")
	f.HaveGroup("design")
	f.HaveMember("design", member)
	f.HaveMember("design", keeper)
	for _, conv := range []string{member, keeper} {
		_, _, err := db.EnsureAgentForConv(conv, "test")
		require.NoError(t, err)
	}
	dash := agentd.BuildDashboardHandlerForTest()
	operator := func(method, path string, body any) *httptest.ResponseRecorder {
		return testharness.Serve(dash, drawingBoardDashReq(t, method, path, body))
	}
	agent := func(conv, method, path string, body any) *httptest.ResponseRecorder {
		return testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, method, path, body), conv))
	}
	lock := func(conv string, locked bool) *httptest.ResponseRecorder {
		return agent(conv, http.MethodPost, "/v1/resource-locks", map[string]any{"kind": "drawing_board", "id": "ux", "locked": locked})
	}
	draw := func(conv, el string, v int64) *httptest.ResponseRecorder {
		return agent(conv, http.MethodPatch, "/v1/drawing-boards/ux", map[string]any{"elements": []any{rect(el, v, 1, 0)}})
	}
	type resources struct {
		DrawingBoards []struct {
			ID           string `json:"id"`
			AgentsLocked bool   `json:"agentsLocked"`
		} `json:"drawingBoards"`
		CanLock bool `json:"canLock"`
	}
	listed := func(conv string) resources {
		t.Helper()
		rec := agent(conv, http.MethodGet, "/v1/groups/design/resources", nil)
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		var out resources
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		return out
	}
	require.Equal(t, http.StatusCreated, agent(member, http.MethodPost, "/v1/drawing-boards", map[string]any{"id": "ux", "title": "UX", "group": "design"}).Code)

	// Locking needs the slug: membership does not confer it.
	rec := lock(member, true)
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), agentd.PermGroupsResourcesLock)
	assert.False(t, listed(member).CanLock)

	// A grant scoped to another group does not reach this one.
	require.NoError(t, db.SetAgentPermissionOverrideWithScope(keeper, agentd.PermGroupsResourcesLock, db.PermEffectGrant, `{"group":["other"]}`, "test"))
	assert.Equal(t, http.StatusForbidden, lock(keeper, true).Code)
	require.NoError(t, db.SetAgentPermissionOverrideWithScope(keeper, agentd.PermGroupsResourcesLock, db.PermEffectGrant, `{"group":["design"]}`, "test"))
	assert.True(t, listed(keeper).CanLock)
	rec = lock(keeper, true)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	// Locked: the member reads but cannot change it, and is told it is locked.
	got := listed(member)
	require.Len(t, got.DrawingBoards, 1)
	assert.True(t, got.DrawingBoards[0].AgentsLocked)
	assert.Equal(t, http.StatusOK, agent(member, http.MethodGet, "/v1/drawing-boards/ux", nil).Code)
	rec = draw(member, "a", 1)
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "resource_locked")
	assert.Contains(t, rec.Body.String(), "is locked")
	// The lock holder and the operator still change it.
	require.Equal(t, http.StatusOK, draw(keeper, "k", 1).Code)
	require.Equal(t, http.StatusOK, operator(http.MethodPatch, "/api/drawing-boards/ux", map[string]any{"elements": []any{rect("op", 1, 1, 0)}}).Code)
	assert.Equal(t, http.StatusForbidden, lock(member, false).Code, "unlocking needs the slug too")

	// The holder unlocks it; the member draws again.
	require.Equal(t, http.StatusOK, lock(keeper, false).Code)
	require.Equal(t, http.StatusOK, draw(member, "a", 1).Code)

	// A non-member is not shown the group's resources.
	const outsider = "lock-outsider-3333"
	f.HaveConvWithTitle(outsider, "outsider")
	_, _, err := db.EnsureAgentForConv(outsider, "test")
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, agent(outsider, http.MethodGet, "/v1/groups/design/resources", nil).Code)
	// A page saved with the group and closed now locks too.
	g, err := db.GetAgentGroupByName("design")
	require.NoError(t, err)
	require.NoError(t, db.SaveOpenGroupPage(db.GroupPage{ID: "pg_saved", GroupID: g.ID, Owner: "group:" + strconv.FormatInt(g.ID, 10),
		Name: "notes", Title: "Notes", Kind: "scratch"}))
	require.NoError(t, db.CloseGroupPage("pg_saved", "closed in test"))
	rec = agent(keeper, http.MethodPost, "/v1/resource-locks", map[string]any{"kind": "page", "id": "pg_saved", "locked": true})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	row, err := db.GetGroupPage("pg_saved")
	require.NoError(t, err)
	assert.True(t, row.AgentsLocked)
	assert.Equal(t, http.StatusForbidden, agent(member, http.MethodPost, "/v1/resource-locks",
		map[string]any{"kind": "page", "id": "pg_saved", "locked": false}).Code)

	assert.Equal(t, http.StatusNotFound, agent(keeper, http.MethodPost, "/v1/resource-locks",
		map[string]any{"kind": "drawing_board", "id": "nope", "locked": true}).Code)
}

// tcl-xrs9v5: an agent's resource list says per template whether it may lock
// it, since a template pinned to several groups needs the grant in each.
func TestAgentResourceListCanLockIsPerTemplate(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	const conv = "lock-lister-4444"
	f.HaveConvWithTitle(conv, "lister")
	f.HaveGroup("crew")
	f.HaveGroup("backstage")
	f.HaveMember("crew", conv)
	_, _, err := db.EnsureAgentForConv(conv, "test")
	require.NoError(t, err)
	for _, id := range []string{"ship", "shared"} {
		rec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/"+id, map[string]any{"template": shipFeatureTemplate(id)})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		rec = processTemplateRequest(t, f, http.MethodPost, "/v1/process/groups/crew/processes", map[string]any{"templateId": id})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	rec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/groups/backstage/processes", map[string]any{"templateId": "shared"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	agent := func(method, path string, body any) *httptest.ResponseRecorder {
		return testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, method, path, body), conv))
	}
	canLock := func() (bool, map[string]bool) {
		t.Helper()
		rec := agent(http.MethodGet, "/v1/groups/crew/resources", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var out struct {
			CanLock   bool `json:"canLock"`
			Processes []struct {
				TemplateID string `json:"templateId"`
				CanLock    bool   `json:"canLock"`
			} `json:"processes"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		got := map[string]bool{}
		for _, p := range out.Processes {
			got[p.TemplateID] = p.CanLock
		}
		return out.CanLock, got
	}
	group, procs := canLock()
	assert.False(t, group)
	assert.Equal(t, map[string]bool{"ship": false, "shared": false}, procs)

	require.NoError(t, db.SetAgentPermissionOverrideWithScope(conv, agentd.PermGroupsResourcesLock, db.PermEffectGrant, `{"group":["crew"]}`, "test"))
	group, procs = canLock()
	assert.True(t, group)
	assert.Equal(t, map[string]bool{"ship": true, "shared": false}, procs, "shared is pinned to backstage too")
	assert.Equal(t, http.StatusForbidden, agent(http.MethodPost, "/v1/resource-locks", map[string]any{"kind": "process", "id": "shared", "locked": true}).Code)

	require.NoError(t, db.SetAgentPermissionOverrideWithScope(conv, agentd.PermGroupsResourcesLock, db.PermEffectGrant, `{"group":["crew","backstage"]}`, "test"))
	_, procs = canLock()
	assert.Equal(t, map[string]bool{"ship": true, "shared": true}, procs)
	assert.Equal(t, http.StatusOK, agent(http.MethodPost, "/v1/resource-locks", map[string]any{"kind": "process", "id": "shared", "locked": true}).Code)
}
