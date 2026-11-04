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
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// tcl-1lth8e: a page shared in a group is saved with it. Closing keeps it in
// the group's Resources as closed, a restart closes the open ones, Reopen
// brings it back under the same id, and only Delete removes it.

type groupContextPage struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	State        string `json:"state"`
	ClosedReason string `json:"closedReason"`
	AgentsLocked bool   `json:"agentsLocked"`
}

func (e *pageEnv) groupPages(t *testing.T, group string) []groupContextPage {
	t.Helper()
	rec := testharness.Serve(e.f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/process/context", nil)))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var ctx struct {
		Groups []struct {
			Name  string             `json:"group"`
			Pages []groupContextPage `json:"pages"`
		} `json:"groups"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ctx))
	for _, g := range ctx.Groups {
		if g.Name == group {
			return g.Pages
		}
	}
	return nil
}

func TestPageSavedWithItsGroupClosesReopensAndDeletes(t *testing.T) {
	e := newPageEnv(t)
	require.NoError(t, config.Save(&config.Config{
		Agent:    &config.AgentConfig{DefaultPermissions: []string{agentd.PermSelfPage}},
		Features: &config.FeaturesConfig{Processes: true},
	}))
	shower := e.agent(t, "saver", true)
	e.f.HaveGroup("crew")
	e.f.HaveMember("crew", shower.conv)
	target := ":" + strconv.Itoa(devServer(t, "saved"))

	p, rec := e.show(t, shower, target, "app", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.Equal(t, "crew", p.Group)
	pages := e.groupPages(t, "crew")
	require.Len(t, pages, 1)
	assert.Equal(t, groupContextPage{ID: p.ID, Name: "app", State: "open"}, pages[0])

	// The operator closes the tab: the page stays in the group, closed.
	require.Equal(t, http.StatusOK, e.dash(t, http.MethodDelete, "/api/pages/"+p.ID).Code)
	assert.Empty(t, e.snapshotPages(t), "no stage serves it")
	pages = e.groupPages(t, "crew")
	require.Len(t, pages, 1)
	assert.Equal(t, groupContextPage{ID: p.ID, Name: "app", State: "closed", ClosedReason: "closed by the operator"}, pages[0])

	// Reopen: the same id, a fresh origin.
	rec = e.dashJSON(t, http.MethodPost, "/api/pages/"+p.ID+"/reopen", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var reopened pageOut
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &reopened))
	assert.Equal(t, p.ID, reopened.ID)
	assert.NotEqual(t, p.Origin, reopened.Origin, "the old origin's token is gone for good")
	assert.Equal(t, "open", e.groupPages(t, "crew")[0].State)

	// The agent closes it; the operator locks the closed page, and the lock
	// refuses the agent's re-show.
	require.Equal(t, http.StatusNoContent, e.call(t, shower, http.MethodDelete, "/v1/pages/app", nil).Code)
	rec = e.dashJSON(t, http.MethodPost, "/api/resource-locks", map[string]any{"kind": "page", "id": p.ID, "locked": true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	pages = e.groupPages(t, "crew")
	require.Len(t, pages, 1)
	assert.Equal(t, groupContextPage{ID: p.ID, Name: "app", State: "closed", ClosedReason: "closed by its agent", AgentsLocked: true}, pages[0])
	_, rec = e.show(t, shower, target, "app", "")
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "resource_locked")
	rec = e.dashJSON(t, http.MethodPost, "/api/resource-locks", map[string]any{"kind": "page", "id": p.ID, "locked": false})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// The agent's re-show of the name reopens the saved page under its id.
	again, rec := e.show(t, shower, target, "app", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, p.ID, again.ID)

	// A daemon restart closes it; it reopens on request.
	agentd.RestartLivePagesForTest()
	pages = e.groupPages(t, "crew")
	require.Len(t, pages, 1)
	assert.Equal(t, "the daemon restarted", pages[0].ClosedReason)
	require.Equal(t, http.StatusOK, e.dashJSON(t, http.MethodPost, "/api/pages/"+p.ID+"/reopen", nil).Code)

	// Delete removes it, live or closed.
	require.Equal(t, http.StatusOK, e.dashJSON(t, http.MethodPost, "/api/pages/"+p.ID+"/delete", nil).Code)
	assert.Empty(t, e.groupPages(t, "crew"))
	assert.Empty(t, e.snapshotPages(t))
	assert.Equal(t, http.StatusNotFound, e.dashJSON(t, http.MethodPost, "/api/pages/"+p.ID+"/delete", nil).Code)
	assert.Equal(t, http.StatusNotFound, e.dashJSON(t, http.MethodPost, "/api/pages/"+p.ID+"/reopen", nil).Code)
}

// A port page whose dev server is gone stays closed, with why.
func TestPageSavedReopenNeedsItsSource(t *testing.T) {
	e := newPageEnv(t)
	pageShareDefaults(t, agentd.PermSelfPage)
	shower := e.agent(t, "gone-src", true)
	e.f.HaveGroup("crew")
	e.f.HaveMember("crew", shower.conv)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body>brief</body></html>"))
	}))
	port := listenerPort(t, srv)
	p, rec := e.show(t, shower, ":"+strconv.Itoa(port), "app", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.Equal(t, http.StatusOK, e.dash(t, http.MethodDelete, "/api/pages/"+p.ID).Code)
	srv.Close()

	rec = e.dashJSON(t, http.MethodPost, "/api/pages/"+p.ID+"/reopen", nil)
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "target_unavailable")
	assert.Empty(t, e.snapshotPages(t))
}

// A private page is not saved: closing it ends it.
func TestPagePrivateIsNotSaved(t *testing.T) {
	e := newPageEnv(t)
	require.NoError(t, config.Save(&config.Config{
		Agent:    &config.AgentConfig{DefaultPermissions: []string{agentd.PermSelfPage}},
		Features: &config.FeaturesConfig{Processes: true},
	}))
	shower := e.agent(t, "private", true)
	e.f.HaveGroup("crew")
	e.f.HaveMember("crew", shower.conv)
	rec := e.call(t, shower, http.MethodPost, "/v1/pages", map[string]any{"target": ":" + strconv.Itoa(devServer(t, "p")), "name": "app", "private": true})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Empty(t, e.groupPages(t, "crew"))
	require.Equal(t, http.StatusNoContent, e.call(t, shower, http.MethodDelete, "/v1/pages/app", nil).Code)
	assert.Empty(t, e.groupPages(t, "crew"))
}

// The operator's lock on a saved page holds against a show of the same name
// from another group, and reopen needs the shower still in the group.
func TestPageSavedLockAndMembershipHold(t *testing.T) {
	e := newPageEnv(t)
	require.NoError(t, config.Save(&config.Config{
		Agent:    &config.AgentConfig{DefaultPermissions: []string{agentd.PermSelfPage}},
		Features: &config.FeaturesConfig{Processes: true},
	}))
	shower := e.agent(t, "two-groups", true)
	e.f.HaveGroup("crew")
	e.f.HaveGroup("side")
	e.f.HaveMember("crew", shower.conv)
	e.f.HaveMember("side", shower.conv)
	target := ":" + strconv.Itoa(devServer(t, "locked"))
	show := func(group string) *httptest.ResponseRecorder {
		return e.call(t, shower, http.MethodPost, "/v1/pages", map[string]any{"target": target, "name": "demo", "group": group})
	}

	rec := show("crew")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var p pageOut
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	require.Equal(t, http.StatusOK, e.dash(t, http.MethodDelete, "/api/pages/"+p.ID).Code)
	rec = e.dashJSON(t, http.MethodPost, "/api/resource-locks", map[string]any{"kind": "page", "id": p.ID, "locked": true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Shown in side, then moved back into crew: the locked crew page stays.
	require.Equal(t, http.StatusCreated, show("side").Code)
	rec = show("crew")
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "resource_locked")
	crew := e.groupPages(t, "crew")
	require.Len(t, crew, 1)
	assert.Equal(t, p.ID, crew[0].ID)
	assert.True(t, crew[0].AgentsLocked)

	// Out of the group, the page no longer reopens there.
	g, err := db.GetAgentGroupByName("crew")
	require.NoError(t, err)
	require.NoError(t, db.RemoveAgentGroupMember(g.ID, shower.conv))
	rec = e.dashJSON(t, http.MethodPost, "/api/pages/"+p.ID+"/reopen", nil)
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "not_member")
}

// The operator reloads a live page in every view; a closed one has nothing
// to reload.
func TestPageReloadFromTheGroup(t *testing.T) {
	e := newPageEnv(t)
	pageShareDefaults(t, agentd.PermSelfPage)
	shower := e.agent(t, "reloader", true)
	e.f.HaveGroup("crew")
	e.f.HaveMember("crew", shower.conv)
	p, rec := e.show(t, shower, ":"+strconv.Itoa(devServer(t, "r")), "app", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, http.StatusOK, e.dashJSON(t, http.MethodPost, "/api/pages/"+p.ID+"/reload", nil).Code)
	require.Equal(t, http.StatusOK, e.dash(t, http.MethodDelete, "/api/pages/"+p.ID).Code)
	assert.Equal(t, http.StatusNotFound, e.dashJSON(t, http.MethodPost, "/api/pages/"+p.ID+"/reload", nil).Code)
	assert.NotEqual(t, http.StatusOK, e.call(t, shower, http.MethodPost, "/api/pages/"+p.ID+"/reload", nil).Code, "no agent route")
}
