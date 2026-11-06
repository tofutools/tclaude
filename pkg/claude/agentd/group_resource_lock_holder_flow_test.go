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

// tcl-o18rrt: an agent holding groups.resources.lock for a resource changes
// it while it is locked, as the operator does. These are the three changes
// that used to stay refused to the holder until the resource was unlocked.

func TestLockHolderReShowsOverItsLockedPage(t *testing.T) {
	e := newPageEnv(t)
	pageShareDefaults(t, agentd.PermSelfPage, agentd.PermGroupsPagesRead, agentd.PermGroupsPagesPatch)
	shower := e.agent(t, "holdshower", true)
	e.f.HaveGroup("crew")
	e.f.HaveMember("crew", shower.conv)
	port := ":" + strconv.Itoa(devServer(t, "x"))
	p, rec := e.show(t, shower, port, "app", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = e.dashJSON(t, http.MethodPost, "/api/resource-locks", map[string]any{"kind": "page", "id": p.ID, "locked": true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	_, rec = e.show(t, shower, port, "app", "")
	requireLocked(t, rec, "re-show without the slug")

	require.NoError(t, db.GrantAgentPermission(shower.conv, agentd.PermGroupsResourcesLock, "test"))
	again, rec := e.show(t, shower, port, "app", "")
	require.Less(t, rec.Code, 300, "the holder re-shows: %s", rec.Body.String())
	assert.Equal(t, p.ID, again.ID, "the same page")
	assert.Contains(t, e.call(t, shower, http.MethodGet, "/v1/pages", nil).Body.String(), `"agents_locked":true`,
		"the re-show keeps the operator's lock")
}

func TestLockHolderAdoptsALockedGroupPage(t *testing.T) {
	e := newPageEnv(t)
	pageShareDefaults(t, agentd.PermSelfPage, agentd.PermGroupsPagesRead, agentd.PermGroupsPagesPatch)
	member := e.agent(t, "holdadopter", true)
	e.f.HaveGroup("crew")
	e.f.HaveMember("crew", member.conv)
	p := e.createGroupPage(t, "crew", "sketch")
	require.Equal(t, http.StatusOK, e.dashJSON(t, http.MethodPost, "/api/resource-locks",
		map[string]any{"kind": "page", "id": p.ID, "locked": true}).Code)
	port := ":" + strconv.Itoa(devServer(t, "built"))
	adopt := func() *httptest.ResponseRecorder {
		return e.call(t, member, http.MethodPost, "/v1/pages", map[string]any{"target": port, "adopt": "crew/sketch"})
	}

	requireLocked(t, adopt(), "adopt without the slug")
	// A grant scoped to another group does not reach the page.
	require.NoError(t, db.SetAgentPermissionOverrideWithScope(member.conv, agentd.PermGroupsResourcesLock, db.PermEffectGrant, `{"group":["other"]}`, "test"))
	requireLocked(t, adopt(), "adopt with the slug for another group")

	require.NoError(t, db.SetAgentPermissionOverrideWithScope(member.conv, agentd.PermGroupsResourcesLock, db.PermEffectGrant, `{"group":["crew"]}`, "test"))
	rec := adopt()
	require.Less(t, rec.Code, 300, "the holder adopts: %s", rec.Body.String())
	var took pageOut
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &took))
	assert.Equal(t, p.ID, took.ID, "the same page")
	assert.Equal(t, member.id, took.Owner, "now the holder's")
}

func TestLockHolderImportsABundleOverALockedTemplate(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	f.HaveGroup("webshop")
	const conv = "hold-process-agent"
	f.HaveConvWithTitle(conv, "editor")
	f.HaveMember("webshop", conv)
	grantEverything(t, conv, agentd.PermProcessTemplatesManage, agentd.PermProcessTemplatesRead, agentd.PermConfigImport)
	agent := func(method, path string, body any) *httptest.ResponseRecorder {
		return testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, method, path, body), conv))
	}
	save := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/ship", map[string]any{"template": shipFeatureTemplate("ship")})
	require.Equal(t, http.StatusCreated, save.Code, save.Body.String())
	require.Equal(t, http.StatusOK, processTemplateRequest(t, f, http.MethodPost, "/v1/process/groups/webshop/processes", map[string]any{"templateId": "ship"}).Code)
	require.Equal(t, http.StatusOK, setResourceLock(t, "process", "ship", true).Code)

	rec := processTemplateRequest(t, f, http.MethodGet, "/v1/config-bundle/export?only=process-templates/ship", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var bundle configbundle.Bundle
	testharness.DecodeJSON(t, rec, &bundle)
	item := bundle.Sections["process-templates"][0]
	var source map[string]string
	require.NoError(t, json.Unmarshal(item.Value, &source))
	source["source"] = strings.ReplaceAll(source["source"], "Implement {{issue}}.", "Ship {{issue}}.")
	var err error
	item.Value, err = json.Marshal(source)
	require.NoError(t, err)
	bundle.Sections["process-templates"][0] = item
	apply := map[string]any{"bundle": bundle, "replace": true, "apply": true}

	requireLocked(t, agent(http.MethodPost, "/v1/config-bundle/import", apply), "import without the slug")

	require.NoError(t, db.SetAgentPermissionOverrideWithScope(conv, agentd.PermGroupsResourcesLock, db.PermEffectGrant, `{"group":["webshop"]}`, "test"))
	rec = agent(http.MethodPost, "/v1/config-bundle/import", apply)
	require.Equal(t, http.StatusOK, rec.Code, "the holder imports: %s", rec.Body.String())
	rec = agent(http.MethodGet, "/v1/process/templates/ship", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "Ship {{issue}}.", "the import applied")
}
