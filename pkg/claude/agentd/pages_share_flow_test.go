package agentd_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// pageShareDefaults installs the default slugs, as setup does.
func pageShareDefaults(t *testing.T, slugs ...string) {
	t.Helper()
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{DefaultPermissions: slugs}}))
}

// shared calls a page action on another agent's page, as `page <verb>
// <owner>/<name>` does.
func (e *pageEnv) shared(t *testing.T, caller, owner pageAgent, method, suffix string, body any) int {
	t.Helper()
	return e.call(t, caller, method, "/v1/pages/app"+suffix+"?owner="+owner.id, body).Code
}

// A page is shared in its shower's group: another member reads its marks,
// DOM, snapshot and journal and live-patches it; eval and close stay with the
// shower unless granted. A non-member reaches nothing, not even its name.
func TestPageSharedInTheShowersGroup(t *testing.T) {
	e := newPageEnv(t)
	pageShareDefaults(t, agentd.PermSelfPage, agentd.PermGroupsPagesRead, agentd.PermGroupsPagesPatch)
	shower := e.agent(t, "shower", true)
	member := e.agent(t, "member", true)
	outsider := e.agent(t, "outsider", true)
	e.f.HaveGroup("crew")
	e.f.HaveMember("crew", shower.conv)
	e.f.HaveMember("crew", member.conv)
	e.f.HaveGroup("others")
	e.f.HaveMember("others", outsider.conv)

	p, rec := e.show(t, shower, ":"+strconv.Itoa(devServer(t, "x")), "app", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, "crew", p.Group, "shown in the shower's only group")
	assert.Contains(t, rec.Body.String(), "every member of that group", "show says who reads what the operator types")
	e.stubSnapshot(t)

	// Gate passed: an answer, or "no view open", never 403/404.
	for _, c := range []struct{ method, suffix string }{
		{http.MethodGet, "/marks"}, {http.MethodGet, "/patches"}, {http.MethodPost, "/snapshot"}, {http.MethodPost, "/dom"},
	} {
		var body any
		if c.suffix == "/dom" {
			body = map[string]any{"selector": "body"}
		}
		code := e.shared(t, member, shower, c.method, c.suffix, body)
		assert.Contains(t, []int{http.StatusOK, http.StatusConflict}, code, "member %s %s", c.method, c.suffix)
		assert.Equal(t, http.StatusNotFound, e.shared(t, outsider, shower, c.method, c.suffix, body), "outsider %s %s", c.method, c.suffix)
	}
	assert.Equal(t, http.StatusOK, e.shared(t, member, shower, http.MethodPost, "/patches", map[string]any{"kind": "css", "css": "a { color: red }"}))
	assert.Equal(t, http.StatusNotFound, e.shared(t, outsider, shower, http.MethodPost, "/patches", map[string]any{"kind": "css", "css": "a{}"}))

	// eval and close need groups.pages.manage, which is no default.
	assert.Equal(t, http.StatusForbidden, e.shared(t, member, shower, http.MethodPost, "/eval", map[string]any{"js": "1"}))
	assert.Equal(t, http.StatusForbidden, e.shared(t, member, shower, http.MethodDelete, "", nil))

	// The member lists it under its owner.
	rec = e.call(t, member, http.MethodGet, "/v1/pages", nil)
	var ls struct {
		Pages, Shared []pageOut
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ls))
	require.Len(t, ls.Shared, 1)
	assert.Equal(t, shower.id, ls.Shared[0].Owner)
	assert.Equal(t, "crew", ls.Shared[0].Group)
	rec = e.call(t, outsider, http.MethodGet, "/v1/pages", nil)
	assert.NotContains(t, rec.Body.String(), `"app"`, "nothing of it is listed to a non-member")

	// Granted manage, scoped to another group: still no.
	require.NoError(t, db.GrantAgentPermissionWithScope(member.conv, agentd.PermGroupsPagesManage, `{"group":["others"]}`, "test"))
	assert.Equal(t, http.StatusForbidden, e.shared(t, member, shower, http.MethodPost, "/eval", map[string]any{"js": "1"}))
	// Scoped to the page's group: the gate passes (no view is open to run it).
	require.NoError(t, db.GrantAgentPermissionWithScope(member.conv, agentd.PermGroupsPagesManage, `{"group":["crew"]}`, "test"))
	assert.Equal(t, http.StatusConflict, e.shared(t, member, shower, http.MethodPost, "/eval", map[string]any{"js": "1"}))
}

// Without the default grants a member reaches the page no more than before:
// read and patch are refused.
func TestPageSharedNeedsTheDefaultGrants(t *testing.T) {
	e := newPageEnv(t)
	pageShareDefaults(t, agentd.PermSelfPage)
	shower := e.agent(t, "shower2", true)
	member := e.agent(t, "member2", true)
	e.f.HaveGroup("crew2")
	e.f.HaveMember("crew2", shower.conv)
	e.f.HaveMember("crew2", member.conv)
	_, rec := e.show(t, shower, ":"+strconv.Itoa(devServer(t, "x")), "app", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	assert.Equal(t, http.StatusForbidden, e.shared(t, member, shower, http.MethodGet, "/marks", nil))
	assert.Equal(t, http.StatusForbidden, e.shared(t, member, shower, http.MethodPost, "/patches", map[string]any{"kind": "css", "css": "a{}"}))

	// A deny override stops the default for one agent.
	pageShareDefaults(t, agentd.PermSelfPage, agentd.PermGroupsPagesRead)
	assert.Equal(t, http.StatusOK, e.shared(t, member, shower, http.MethodGet, "/marks", nil))
	require.NoError(t, db.SetAgentPermissionOverride(member.conv, agentd.PermGroupsPagesRead, db.PermEffectDeny, "test"))
	assert.Equal(t, http.StatusForbidden, e.shared(t, member, shower, http.MethodGet, "/marks", nil))
}

// A page shown --private, or by a shower in several groups that names none,
// is shared with no one; --group names one of the shower's groups only.
func TestPageSharedOnlyWhereTheShowerSaid(t *testing.T) {
	e := newPageEnv(t)
	pageShareDefaults(t, agentd.PermSelfPage, agentd.PermGroupsPagesRead, agentd.PermGroupsPagesPatch)
	shower := e.agent(t, "shower3", true)
	member := e.agent(t, "member3", true)
	e.f.HaveGroup("one")
	e.f.HaveGroup("two")
	e.f.HaveGroup("three")
	e.f.HaveMember("one", shower.conv)
	e.f.HaveMember("one", member.conv)
	e.f.HaveMember("two", shower.conv)
	port := ":" + strconv.Itoa(devServer(t, "x"))

	show := func(body map[string]any) (int, pageOut, string) {
		body["target"], body["name"] = port, "app"
		rec := e.call(t, shower, http.MethodPost, "/v1/pages", body)
		var out struct {
			pageOut
			ShareNote string `json:"share_note"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out.pageOut, out.ShareNote
	}
	code, p, note := show(map[string]any{})
	require.Less(t, code, 300)
	assert.Empty(t, p.Group)
	assert.Contains(t, note, "several groups")
	assert.Equal(t, http.StatusNotFound, e.shared(t, member, shower, http.MethodGet, "/marks", nil))

	code, p, _ = show(map[string]any{"group": "one"})
	require.Less(t, code, 300)
	assert.Equal(t, "one", p.Group)
	assert.Equal(t, http.StatusOK, e.shared(t, member, shower, http.MethodGet, "/marks", nil))

	code, _, _ = show(map[string]any{"group": "three"})
	assert.Equal(t, http.StatusBadRequest, code, "not a group of the shower's")

	code, p, note = show(map[string]any{"group": "one", "private": true})
	require.Less(t, code, 300)
	assert.Empty(t, p.Group)
	assert.Contains(t, note, "private")
	assert.Equal(t, http.StatusNotFound, e.shared(t, member, shower, http.MethodGet, "/marks", nil))

	// Leaving the group ends the sharing at once.
	code, _, _ = show(map[string]any{"group": "one"})
	require.Less(t, code, 300)
	require.Equal(t, http.StatusOK, e.shared(t, member, shower, http.MethodGet, "/marks", nil))
	g, err := db.GetAgentGroupByName("one")
	require.NoError(t, err)
	require.NoError(t, db.RemoveAgentGroupMember(g.ID, shower.conv))
	assert.Equal(t, http.StatusNotFound, e.shared(t, member, shower, http.MethodGet, "/marks", nil))
}

// A member reads a static page back only as its shower could: when the
// shower's view of the files is exact. Under the shower's own harness
// sandbox, it is refused to both.
func TestPageSharedStaticReadBackFollowsTheShower(t *testing.T) {
	e := newPageEnv(t)
	pageShareDefaults(t, agentd.PermSelfPage, agentd.PermGroupsPagesRead, agentd.PermGroupsPagesPatch)
	shower := e.agent(t, "shower4", true)
	member := e.agent(t, "member4", true)
	e.f.HaveGroup("crew4")
	e.f.HaveMember("crew4", shower.conv)
	e.f.HaveMember("crew4", member.conv)
	site := staticSite(t, shower.cwd)
	e.sandboxAgent(t, shower, "harness-builtin", siteProfile(shower.cwd), claudeSandbox("on", false))
	_, rec := e.show(t, shower, site, "app", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	for _, caller := range []pageAgent{shower, member} {
		rec = e.call(t, caller, http.MethodPost, "/v1/pages/app/snapshot?owner="+shower.id, nil)
		assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "cannot be read back")
	}
}
