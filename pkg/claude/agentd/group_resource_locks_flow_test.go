package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/configbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// tcl-dwzsq0: the operator locks a group resource against every agent
// change. A locked resource refuses an agent's change with resource_locked
// whatever the agent is granted; only the operator's dashboard sets or clears
// the lock, and the operator still changes the resource.

func setResourceLock(t *testing.T, kind, id string, locked bool) *httptest.ResponseRecorder {
	t.Helper()
	return testharness.Serve(agentd.BuildDashboardHandlerForTest(), boardDashReq(t, http.MethodPost, "/api/resource-locks",
		map[string]any{"kind": kind, "id": id, "locked": locked}))
}

func requireLocked(t *testing.T, rec *httptest.ResponseRecorder, msg string) {
	t.Helper()
	assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", msg, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"resource_locked"`, msg)
}

// Granting the agent every slug the routes need, and admin over its group,
// changes nothing while the lock holds.
func grantEverything(t *testing.T, conv string, slugs ...string) {
	t.Helper()
	for _, s := range append(slugs, agentd.PermGroupsAdmin) {
		require.NoError(t, db.GrantAgentPermission(conv, s, "test"))
	}
}

func TestResourceLockRefusesAgentProcessChanges(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	f.HaveGroup("webshop")
	const conv = "lock-process-agent"
	f.HaveConvWithTitle(conv, "editor")
	f.HaveMember("webshop", conv)
	grantEverything(t, conv, agentd.PermProcessTemplatesManage, agentd.PermProcessTemplatesRead, agentd.PermConfigImport)
	agent := func(method, path string, body any) *httptest.ResponseRecorder {
		return testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, method, path, body), conv))
	}

	var err error
	save := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/ship", map[string]any{"template": shipFeatureTemplate("ship")})
	require.Equal(t, http.StatusCreated, save.Code, save.Body.String())
	require.Equal(t, http.StatusOK, processTemplateRequest(t, f, http.MethodPost, "/v1/process/groups/webshop/processes", map[string]any{"templateId": "ship"}).Code)

	// No agent route toggles the lock.
	rec := agent(http.MethodPost, "/api/resource-locks", map[string]any{"kind": "process", "id": "ship", "locked": false})
	assert.NotEqual(t, http.StatusOK, rec.Code, "the agent socket has no lock route: %s", rec.Body.String())

	require.Equal(t, http.StatusOK, setResourceLock(t, "process", "ship", true).Code)

	requireLocked(t, agent(http.MethodPost, "/v1/process/templates/ship", map[string]any{"template": shipFeatureTemplate("ship")}), "save")
	requireLocked(t, agent(http.MethodDelete, "/v1/process/templates/ship", nil), "delete")
	requireLocked(t, agent(http.MethodPost, "/v1/process/templates/ship/listing", map[string]any{"listed": true}), "listing")
	requireLocked(t, agent(http.MethodDelete, "/v1/process/groups/webshop/processes/ship", nil), "unpin")
	// Reads stay open.
	assert.Equal(t, http.StatusOK, agent(http.MethodGet, "/v1/process/templates/ship", nil).Code)

	// A config bundle over it previews, and refuses to apply.
	rec = processTemplateRequest(t, f, http.MethodGet, "/v1/config-bundle/export?only=process-templates/ship", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var bundle configbundle.Bundle
	testharness.DecodeJSON(t, rec, &bundle)
	item := bundle.Sections["process-templates"][0]
	var source map[string]string
	require.NoError(t, json.Unmarshal(item.Value, &source))
	source["source"] = strings.ReplaceAll(source["source"], "Implement {{issue}}.", "Ship {{issue}}.")
	item.Value, err = json.Marshal(source)
	require.NoError(t, err)
	bundle.Sections["process-templates"][0] = item
	rec = agent(http.MethodPost, "/v1/config-bundle/import", map[string]any{"bundle": bundle, "replace": true})
	assert.Equal(t, http.StatusOK, rec.Code, "preview: %s", rec.Body.String())
	requireLocked(t, agent(http.MethodPost, "/v1/config-bundle/import", map[string]any{"bundle": bundle, "replace": true, "apply": true}), "bundle import")

	// The operator still changes it.
	rec = processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/ship/listing", map[string]any{"listed": true})
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Unlocked, the agent is back to its grants.
	require.Equal(t, http.StatusOK, setResourceLock(t, "process", "ship", false).Code)
	rec = agent(http.MethodPost, "/v1/process/templates/ship/listing", map[string]any{"listed": false})
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	assert.Equal(t, http.StatusNotFound, setResourceLock(t, "process", "nope", true).Code, "only an existing template is locked")
	assert.Equal(t, http.StatusBadRequest, setResourceLock(t, "group", "ship", true).Code, "an unknown kind is refused")
}

func TestResourceLockRefusesAgentBoardPatches(t *testing.T) {
	f := newFlow(t)
	const conv = "lock-board-agent"
	f.HaveConvWithTitle(conv, "drawer")
	grantEverything(t, conv, agentd.PermBoardsRead, agentd.PermBoardsWrite)
	agent := func(method, path string, body any) *httptest.ResponseRecorder {
		return testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, method, path, body), conv))
	}
	dash := agentd.BuildDashboardHandlerForTest()
	require.Equal(t, http.StatusCreated, agent(http.MethodPost, "/v1/boards", map[string]any{"id": "arch", "title": "Architecture"}).Code)

	require.Equal(t, http.StatusOK, setResourceLock(t, "board", "arch", true).Code)
	requireLocked(t, agent(http.MethodPatch, "/v1/boards/arch", map[string]any{"elements": []any{rect("n1", 1, 1, 0)}}), "agent patch")

	b := agent(http.MethodGet, "/v1/boards/arch", nil)
	require.Equal(t, http.StatusOK, b.Code)
	assert.Contains(t, b.Body.String(), `"agents_locked":true`, "the board says it is locked")

	rec := testharness.Serve(dash, boardDashReq(t, http.MethodPatch, "/api/boards/arch", map[string]any{"elements": []any{rect("n1", 1, 1, 0)}}))
	assert.Equal(t, http.StatusOK, rec.Code, "the operator still draws: %s", rec.Body.String())

	require.Equal(t, http.StatusOK, setResourceLock(t, "board", "arch", false).Code)
	rec = agent(http.MethodPatch, "/v1/boards/arch", map[string]any{"elements": []any{rect("n2", 1, 2, 0)}})
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// A deleted board's lock goes with it: a new board by its id is unlocked.
	require.Equal(t, http.StatusOK, setResourceLock(t, "board", "arch", true).Code)
	rec = testharness.Serve(dash, boardDashReq(t, http.MethodDelete, "/api/boards/arch", nil))
	require.Less(t, rec.Code, 300, rec.Body.String())
	require.Equal(t, http.StatusCreated, agent(http.MethodPost, "/v1/boards", map[string]any{"id": "arch", "title": "Again"}).Code)
	rec = agent(http.MethodPatch, "/v1/boards/arch", map[string]any{"elements": []any{rect("n3", 1, 3, 0)}})
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestResourceLockRefusesAgentPageChanges(t *testing.T) {
	e := newPageEnv(t)
	pageShareDefaults(t, agentd.PermSelfPage, agentd.PermGroupsPagesRead, agentd.PermGroupsPagesPatch)
	shower := e.agent(t, "lockshower", true)
	member := e.agent(t, "lockmember", true)
	e.f.HaveGroup("crew")
	e.f.HaveMember("crew", shower.conv)
	e.f.HaveMember("crew", member.conv)
	require.NoError(t, db.GrantAgentPermission(member.conv, agentd.PermGroupsPagesManage, "test"))
	port := ":" + strconv.Itoa(devServer(t, "x"))
	p, rec := e.show(t, shower, port, "app", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	e.stubSnapshot(t)

	rec = e.dashJSON(t, http.MethodPost, "/api/resource-locks", map[string]any{"kind": "page", "id": p.ID, "locked": true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	css := map[string]any{"kind": "css", "css": "a { color: red }"}
	for _, c := range []struct {
		who          pageAgent
		method, path string
		body         any
	}{
		{shower, http.MethodPost, "/v1/pages/app/patches", css},
		{shower, http.MethodPost, "/v1/pages/app/eval", map[string]any{"js": "1"}},
		{shower, http.MethodDelete, "/v1/pages/app", nil},
		{member, http.MethodPost, "/v1/pages/app/patches?owner=" + shower.id, css},
		{member, http.MethodPost, "/v1/pages/app/eval?owner=" + shower.id, map[string]any{"js": "1"}},
		{member, http.MethodDelete, "/v1/pages/app?owner=" + shower.id, nil},
	} {
		requireLocked(t, e.call(t, c.who, c.method, c.path, c.body), c.method+" "+c.path)
	}
	_, rec = e.show(t, shower, port, "app", "")
	requireLocked(t, rec, "re-show")

	// Reads stay open, and the page says it is locked.
	assert.Equal(t, http.StatusOK, e.call(t, member, http.MethodGet, "/v1/pages/app/marks?owner="+shower.id, nil).Code)
	assert.Contains(t, e.call(t, shower, http.MethodGet, "/v1/pages", nil).Body.String(), `"agents_locked":true`)

	rec = e.dashJSON(t, http.MethodPost, "/api/resource-locks", map[string]any{"kind": "page", "id": p.ID, "locked": false})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, http.StatusOK, e.call(t, shower, http.MethodPost, "/v1/pages/app/patches", css).Code)
}

// The group context lists a group's boards and pages, with their locks, to
// the operator only: they feed the dashboard's Resources section.
func TestGroupContextListsResourcesToTheOperatorOnly(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	f.HaveGroup("webshop")
	const conv = "resources-member"
	f.HaveConvWithTitle(conv, "member")
	f.HaveMember("webshop", conv)
	dash := agentd.BuildDashboardHandlerForTest()
	require.Equal(t, http.StatusCreated, testharness.Serve(dash, boardDashReq(t, http.MethodPost, "/api/boards", map[string]any{"id": "sprint", "title": "Sprint"})).Code)
	require.Equal(t, http.StatusOK, testharness.Serve(dash, boardDashReq(t, http.MethodPost, "/api/boards/sprint/group", map[string]any{"group": "webshop"})).Code)
	require.Equal(t, http.StatusOK, setResourceLock(t, "board", "sprint", true).Code)

	rec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/context", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var view struct {
		Groups []struct {
			Group  string `json:"group"`
			Boards []struct {
				ID           string `json:"id"`
				AgentsLocked bool   `json:"agentsLocked"`
			} `json:"boards"`
		} `json:"groups"`
	}
	testharness.DecodeJSON(t, rec, &view)
	var found bool
	for _, g := range view.Groups {
		if g.Group == "webshop" {
			require.Len(t, g.Boards, 1)
			assert.Equal(t, "sprint", g.Boards[0].ID)
			assert.True(t, g.Boards[0].AgentsLocked)
			found = true
		}
	}
	assert.True(t, found, "the operator sees the group's board: %s", rec.Body.String())

	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermProcessRunsRead, "test"))
	rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/process/context", nil), conv))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), `"boards"`, "an agent's context lists no resources")
}
