package agentd_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// tcl-20ihu3: the operator creates a page in a group's Collab space. The
// group owns it until a member takes it over: members reach it as
// <group>/<name> under the groups.pages.* slugs, and it serves a fixed
// scratch sheet the operator draws on.

func (e *pageEnv) createGroupPage(t *testing.T, group, name string) pageOut {
	t.Helper()
	rec := e.dashJSON(t, http.MethodPost, "/api/pages", map[string]any{"group": group, "name": name, "title": "Sketch"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var out pageOut
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

func TestPageOperatorCreatedInAGroupIsTheGroupsUntilAMemberTakesItOver(t *testing.T) {
	e := newPageEnv(t)
	pageShareDefaults(t, agentd.PermSelfPage, agentd.PermGroupsPagesRead, agentd.PermGroupsPagesPatch)
	member := e.agent(t, "gp-member", true)
	other := e.agent(t, "gp-other", true)
	outsider := e.agent(t, "gp-outsider", true)
	e.f.HaveGroup("crew")
	e.f.HaveMember("crew", member.conv)
	e.f.HaveMember("crew", other.conv)
	e.f.HaveGroup("elsewhere")
	e.f.HaveMember("elsewhere", outsider.conv)

	p := e.createGroupPage(t, "crew", "sketch")
	assert.Equal(t, "scratch", p.Kind)
	assert.Empty(t, p.Owner, "no agent owns it")
	rec := e.dashJSON(t, http.MethodPost, "/api/pages", map[string]any{"group": "crew", "name": "sketch"})
	assert.Equal(t, http.StatusConflict, rec.Code, "one name per group")
	rec = e.dashJSON(t, http.MethodPost, "/api/pages", map[string]any{"group": "nope", "name": "x"})
	assert.Equal(t, http.StatusNotFound, rec.Code)
	// No agent route creates one.
	assert.NotEqual(t, http.StatusCreated, e.call(t, member, http.MethodPost, "/api/pages", map[string]any{"group": "crew", "name": "y"}).Code)

	// The operator draws on it and sends; no agent is told, members read it.
	rec = e.dashJSON(t, http.MethodPost, "/api/pages/"+p.ID+"/marks", sampleMarks())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"told_agent":false`)
	rec = e.call(t, member, http.MethodGet, "/v1/pages/sketch/marks?owner=crew", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "Make the pay button blue")
	assert.Equal(t, http.StatusNotFound, e.call(t, outsider, http.MethodGet, "/v1/pages/sketch/marks?owner=crew", nil).Code,
		"a non-member reaches nothing")
	assert.Equal(t, http.StatusNotFound, e.call(t, outsider, http.MethodGet, "/v1/pages/sketch/marks?owner=elsewhere", nil).Code)

	// Members list it under the group's name.
	rec = e.call(t, member, http.MethodGet, "/v1/pages", nil)
	assert.Contains(t, rec.Body.String(), `"group_owned":true`)

	// A member live-patches the scratch page; the takeover must not carry it.
	rec = e.call(t, other, http.MethodPost, "/v1/pages/sketch/patches?owner=crew", map[string]any{"kind": "css", "css": "body { color: red }"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	port := ":" + strconv.Itoa(devServer(t, "built"))
	adopt := func(a pageAgent, body map[string]any) *http.Response {
		t.Helper()
		body["target"] = port
		return e.call(t, a, http.MethodPost, "/v1/pages", body).Result()
	}

	// Locked, no member takes it over.
	require.Equal(t, http.StatusOK, e.dashJSON(t, http.MethodPost, "/api/resource-locks", map[string]any{"kind": "page", "id": p.ID, "locked": true}).Code)
	assert.Equal(t, http.StatusForbidden, adopt(member, map[string]any{"adopt": "crew/sketch"}).StatusCode)
	require.Equal(t, http.StatusOK, e.dashJSON(t, http.MethodPost, "/api/resource-locks", map[string]any{"kind": "page", "id": p.ID, "locked": false}).Code)

	assert.Equal(t, http.StatusNotFound, adopt(outsider, map[string]any{"adopt": "crew/sketch"}).StatusCode, "a non-member cannot")
	assert.Equal(t, http.StatusBadRequest, adopt(member, map[string]any{"adopt": "crew/sketch", "name": "other"}).StatusCode, "it keeps its name")
	_, rec = e.show(t, other, port, "sketch", "")
	require.Less(t, rec.Code, 300, rec.Body.String())
	assert.Equal(t, http.StatusConflict, adopt(other, map[string]any{"adopt": "crew/sketch"}).StatusCode, "an adopter with a page of that name closes it first")

	rec = e.call(t, member, http.MethodPost, "/v1/pages", map[string]any{"target": port, "adopt": "crew/sketch"})
	require.Less(t, rec.Code, 300, rec.Body.String())
	var took pageOut
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &took))
	assert.Equal(t, p.ID, took.ID, "the same page")
	assert.Equal(t, member.id, took.Owner, "now the member's")
	var snap pageOut
	for _, sp := range e.snapshotPages(t) {
		if sp.ID == p.ID {
			snap = sp
		}
	}
	assert.NotEqual(t, p.Origin, snap.Origin, "a takeover gets a fresh origin")
	// The operator's marks are kept for the adopter, and the group still shares it.
	rec = e.call(t, member, http.MethodGet, "/v1/pages/sketch/marks", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "Make the pay button blue")
	assert.Equal(t, http.StatusOK, e.call(t, other, http.MethodGet, "/v1/pages/sketch/marks?owner="+member.id, nil).Code)
	assert.Equal(t, http.StatusNotFound, e.call(t, other, http.MethodGet, "/v1/pages/sketch/marks?owner=crew", nil).Code, "the group no longer owns it")
	rec = e.call(t, member, http.MethodGet, "/v1/pages/sketch/patches", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "color: red", "the scratch page's live patches do not follow it to the adopter's source")
}

// A group's own pages close when it is archived: no member can reach them.
func TestPageGroupOwnedClosesWithItsGroup(t *testing.T) {
	e := newPageEnv(t)
	e.f.HaveGroup("crew")
	p := e.createGroupPage(t, "crew", "sketch")
	req := agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/groups/crew/archive", nil))
	rec := testharness.Serve(e.f.Mux, req)
	require.Less(t, rec.Code, 300, rec.Body.String())
	for _, sp := range e.snapshotPages(t) {
		assert.NotEqual(t, p.ID, sp.ID, "the archived group's page is closed")
	}
}
