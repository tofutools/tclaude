package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// umbrellaEnv is a manager agent in no group, beside two groups' resources:
//   - boards: plan (crew), secret (hidden), loose (no group);
//   - templates: ship (crew), shared (crew and hidden), solo (pinned nowhere).
type umbrellaEnv struct {
	t       *testing.T
	f       *testharness.Flow
	manager string
}

func newUmbrellaEnv(t *testing.T) *umbrellaEnv {
	t.Helper()
	f, _ := processAuthoringFlow(t)
	f.HaveGroup("crew")
	f.HaveGroup("hidden")
	e := &umbrellaEnv{t: t, f: f, manager: "umbrella-manager-1111"}
	f.HaveConvWithTitle(e.manager, "manager")
	_, _, err := db.EnsureAgentForConv(e.manager, "test")
	require.NoError(t, err)
	for id, group := range map[string]string{"plan": "crew", "secret": "hidden", "loose": ""} {
		require.Less(t, e.operator(http.MethodPost, "/api/boards", map[string]any{"id": id, "title": id}).Code, 300)
		if group != "" {
			rec := e.operator(http.MethodPost, "/api/boards/"+id+"/group", map[string]any{"group": group})
			require.Less(t, rec.Code, 300, rec.Body.String())
		}
	}
	for id, groups := range map[string][]string{"ship": {"crew"}, "shared": {"crew", "hidden"}, "solo": nil} {
		rec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/"+id, map[string]any{"template": shipFeatureTemplate(id)})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		for _, g := range groups {
			rec = processTemplateRequest(t, f, http.MethodPost, "/v1/process/groups/"+g+"/processes", map[string]any{"templateId": id})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		}
	}
	return e
}

func (e *umbrellaEnv) operator(method, path string, body any) *httptest.ResponseRecorder {
	return testharness.Serve(agentd.BuildDashboardHandlerForTest(), boardDashReq(e.t, method, path, body))
}

func (e *umbrellaEnv) agent(method, path string, body any) *httptest.ResponseRecorder {
	return testharness.Serve(e.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(e.t, method, path, body), e.manager))
}

func (e *umbrellaEnv) grant(slug, scope string) {
	e.t.Helper()
	require.NoError(e.t, db.SetAgentPermissionOverrideWithScope(e.manager, slug, db.PermEffectGrant, scope, "test"))
}

// codes is each path's status for a GET.
func (e *umbrellaEnv) reads(paths ...string) map[string]int {
	out := map[string]int{}
	for _, p := range paths {
		out[p] = e.agent(http.MethodGet, p, nil).Code
	}
	return out
}

func (e *umbrellaEnv) draw(board string) int {
	return e.agent(http.MethodPatch, "/v1/boards/"+board, map[string]any{"elements": []any{rect("u", 1, 1, 0)}}).Code
}

// save saves template's current head again, as the editor would.
func (e *umbrellaEnv) save(template string, extra map[string]any) *httptest.ResponseRecorder {
	e.t.Helper()
	rec := processTemplateRequest(e.t, e.f, http.MethodGet, "/v1/process/templates/"+template, nil)
	require.Equal(e.t, http.StatusOK, rec.Code, rec.Body.String())
	var head struct {
		CurrentRef string `json:"currentRef"`
		SourceHash string `json:"sourceHash"`
	}
	require.NoError(e.t, json.Unmarshal(rec.Body.Bytes(), &head))
	tmpl := shipFeatureTemplate(template)
	tmpl.Description = "saved by the manager"
	body := map[string]any{"template": tmpl, "currentRef": head.CurrentRef, "sourceHash": head.SourceHash}
	for k, v := range extra {
		body[k] = v
	}
	return e.agent(http.MethodPost, "/v1/process/templates/"+template, body)
}

func (e *umbrellaEnv) listedBoards() []string {
	e.t.Helper()
	rec := e.agent(http.MethodGet, "/v1/boards", nil)
	require.Equal(e.t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct{ Boards []struct{ ID string } }
	require.NoError(e.t, json.Unmarshal(rec.Body.Bytes(), &out))
	ids := []string{}
	for _, b := range out.Boards {
		ids = append(ids, b.ID)
	}
	slices.Sort(ids)
	return ids
}

func (e *umbrellaEnv) listedTemplates() []string {
	e.t.Helper()
	rec := e.agent(http.MethodGet, "/v1/process/templates?all=1", nil)
	require.Equal(e.t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct{ Templates []struct{ ID string } }
	require.NoError(e.t, json.Unmarshal(rec.Body.Bytes(), &out))
	ids := []string{}
	for _, tmpl := range out.Templates {
		ids = append(ids, tmpl.ID)
	}
	slices.Sort(ids)
	return ids
}

const (
	pathPlan   = "/v1/boards/plan"
	pathSecret = "/v1/boards/secret"
	pathLoose  = "/v1/boards/loose"
	pathShip   = "/v1/process/templates/ship"
	pathShared = "/v1/process/templates/shared"
	pathSolo   = "/v1/process/templates/solo"
)

var umbrellaReadPaths = []string{pathPlan, pathSecret, pathLoose, pathShip, pathShared, pathSolo}

// An unscoped resources.read reads every board and template, wherever it is
// linked, and changes none of them.
func TestResourcesReadUmbrellaReadsEveryResourceKind(t *testing.T) {
	e := newUmbrellaEnv(t)
	for p, code := range e.reads(umbrellaReadPaths...) {
		assert.Equal(t, http.StatusForbidden, code, "%s before the grant", p)
	}

	e.grant(agentd.PermResourcesRead, "")
	for p, code := range e.reads(umbrellaReadPaths...) {
		assert.Equal(t, http.StatusOK, code, p)
	}
	assert.Equal(t, []string{"loose", "plan", "secret"}, e.listedBoards())
	assert.Equal(t, []string{"shared", "ship", "solo"}, e.listedTemplates())
	assert.Equal(t, http.StatusOK, e.agent(http.MethodGet, "/v1/process/runs", nil).Code, "process.runs.read is implied too")

	assert.Equal(t, http.StatusForbidden, e.draw("loose"), "read does not write")
	assert.Equal(t, http.StatusForbidden, e.save("solo", nil).Code)
}

// An unscoped resources.write changes boards and templates, but never past an
// operator lock, and neither unlocks nor steers runs.
func TestResourcesWriteUmbrellaChangesButNotPastALock(t *testing.T) {
	e := newUmbrellaEnv(t)
	e.grant(agentd.PermResourcesWrite, "")
	assert.Equal(t, http.StatusOK, e.draw("loose"))
	assert.Equal(t, http.StatusOK, e.draw("secret"))
	rec := e.save("shared", nil)
	assert.Less(t, rec.Code, 300, rec.Body.String())

	for _, lock := range []map[string]any{{"kind": "board", "id": "loose", "locked": true}, {"kind": "process", "id": "shared", "locked": true}} {
		rec = e.operator(http.MethodPost, "/api/resource-locks", lock)
		require.Less(t, rec.Code, 300, rec.Body.String())
	}
	assert.NotEqual(t, http.StatusOK, e.draw("loose"), "a lock holds against resources.write")
	rec = e.save("shared", nil)
	assert.GreaterOrEqual(t, rec.Code, 400, "a lock holds against resources.write: %s", rec.Body.String())
	assert.Equal(t, http.StatusOK, e.agent(http.MethodGet, pathLoose, nil).Code, "write implies read, and a lock does not stop a read")
	rec = e.agent(http.MethodPost, "/v1/resource-locks", map[string]any{"kind": "board", "id": "loose", "locked": false})
	assert.Equal(t, http.StatusForbidden, rec.Code, "only groups.resources.lock or the human unlocks")
	assert.Contains(t, rec.Body.String(), agentd.PermGroupsResourcesLock)
}

// A group= scope reaches the resources linked to that group: a read through
// any of a resource's groups, a change only through all of them. Nothing the
// scope does not reach is listed, and the scoped grant cannot republish or
// re-pin a template.
func TestResourcesUmbrellaScopedToAGroup(t *testing.T) {
	e := newUmbrellaEnv(t)
	e.grant(agentd.PermResourcesRead, `{"group":["crew"]}`)
	assert.Equal(t, map[string]int{
		pathPlan: http.StatusOK, pathSecret: http.StatusForbidden, pathLoose: http.StatusForbidden,
		pathShip: http.StatusOK, pathShared: http.StatusOK, pathSolo: http.StatusForbidden,
	}, e.reads(umbrellaReadPaths...))
	assert.Equal(t, []string{"plan"}, e.listedBoards())
	assert.Equal(t, []string{"shared", "ship"}, e.listedTemplates())
	assert.Equal(t, http.StatusOK, e.agent(http.MethodGet, "/v1/process/runs?group=crew", nil).Code)
	assert.Equal(t, http.StatusForbidden, e.agent(http.MethodGet, "/v1/process/runs?group=hidden", nil).Code)
	assert.Equal(t, http.StatusForbidden, e.agent(http.MethodGet, "/v1/process/runs", nil).Code,
		"a scoped grant does not list every run")

	e.grant(agentd.PermResourcesWrite, `{"group":["crew"]}`)
	assert.Equal(t, http.StatusOK, e.draw("plan"))
	assert.Equal(t, http.StatusForbidden, e.draw("secret"))
	assert.Equal(t, http.StatusForbidden, e.draw("loose"))
	rec := e.save("ship", nil)
	assert.Less(t, rec.Code, 300, rec.Body.String())
	assert.Equal(t, http.StatusForbidden, e.save("shared", nil).Code, "shared is also hidden's, which the scope does not reach")
	assert.Equal(t, http.StatusForbidden, e.save("ship", map[string]any{"pinGroup": "hidden"}).Code,
		"pinning stays process.templates.manage's")
	assert.Equal(t, http.StatusForbidden, e.save("ship", map[string]any{"listed": true}).Code,
		"listing stays process.templates.manage's")
	// The scope reaches a new board in crew, but linking a board to a group
	// still takes membership: the link mails the board's activity to it.
	rec = e.agent(http.MethodPost, "/v1/boards", map[string]any{"id": "new", "title": "New", "group": "crew"})
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "not_member")
	assert.Equal(t, http.StatusForbidden, e.agent(http.MethodPost, "/v1/boards", map[string]any{"id": "new2", "title": "New"}).Code)
}

// A resource= scope reaches exactly the resources it names, linked to a group
// or not.
func TestResourcesUmbrellaScopedToResources(t *testing.T) {
	e := newUmbrellaEnv(t)
	e.grant(agentd.PermResourcesRead, `{"resource":["board:secret","process_template:solo"]}`)
	assert.Equal(t, map[string]int{
		pathPlan: http.StatusForbidden, pathSecret: http.StatusOK, pathLoose: http.StatusForbidden,
		pathShip: http.StatusForbidden, pathShared: http.StatusForbidden, pathSolo: http.StatusOK,
	}, e.reads(umbrellaReadPaths...))
	assert.Equal(t, []string{"secret"}, e.listedBoards())
	assert.Equal(t, []string{"solo"}, e.listedTemplates())

	e.grant(agentd.PermResourcesWrite, `{"resource":["board:loose","process_template:shared"]}`)
	assert.Equal(t, http.StatusOK, e.draw("loose"))
	assert.Equal(t, http.StatusForbidden, e.draw("plan"))
	rec := e.save("shared", nil)
	assert.Less(t, rec.Code, 300, rec.Body.String())
	assert.Equal(t, http.StatusForbidden, e.save("ship", nil).Code)
}

// Dimensions AND: group=crew with resource=board:plan reaches plan only.
func TestResourcesUmbrellaScopeDimensionsAnd(t *testing.T) {
	e := newUmbrellaEnv(t)
	e.grant(agentd.PermResourcesRead, `{"group":["crew"],"resource":["board:plan","board:secret"]}`)
	assert.Equal(t, map[string]int{
		pathPlan: http.StatusOK, pathSecret: http.StatusForbidden, pathLoose: http.StatusForbidden,
		pathShip: http.StatusForbidden, pathShared: http.StatusForbidden, pathSolo: http.StatusForbidden,
	}, e.reads(umbrellaReadPaths...))
}

// A dedicated deny of one kind's slug wins over the umbrella; the other
// kinds stay reachable.
func TestResourcesUmbrellaYieldsToADedicatedDeny(t *testing.T) {
	e := newUmbrellaEnv(t)
	e.grant(agentd.PermResourcesRead, "")
	require.NoError(t, db.SetAgentPermissionOverride(e.manager, agentd.PermBoardsRead, db.PermEffectDeny, "test"))
	assert.Equal(t, http.StatusForbidden, e.agent(http.MethodGet, pathPlan, nil).Code)
	assert.Equal(t, http.StatusForbidden, e.agent(http.MethodGet, pathLoose, nil).Code)
	assert.Equal(t, http.StatusOK, e.agent(http.MethodGet, pathShip, nil).Code)
}

// sudo is the ordinary sudo path: a time-bounded grant of the umbrella with a
// scope reaches what the scope names and ends when it expires.
func TestResourcesUmbrellaSudoIsScopedAndTimeBounded(t *testing.T) {
	e := newUmbrellaEnv(t)
	now := time.Now()
	id, err := db.InsertSudoGrant(&db.SudoGrant{
		ConvID: e.manager, Slug: agentd.PermResourcesRead, ScopeJSON: `{"group":["crew"]}`,
		GrantedAt: now, ExpiresAt: now.Add(time.Minute), GrantedBy: "human:test",
	})
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, e.agent(http.MethodGet, pathPlan, nil).Code)
	assert.Equal(t, http.StatusForbidden, e.agent(http.MethodGet, pathSecret, nil).Code)
	assert.Equal(t, []string{"plan"}, e.listedBoards())

	_, err = db.RevokeSudoGrant(id)
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, e.agent(http.MethodGet, pathPlan, nil).Code)
}

// Membership and ownership confer the group slugs, never the umbrella: a crew
// member and owner reads crew's resources and nothing else.
func TestResourcesUmbrellaIsNotMemberOrOwnerImplied(t *testing.T) {
	e := newUmbrellaEnv(t)
	e.f.HaveMember("crew", e.manager)
	g, err := db.GetAgentGroupByName("crew")
	require.NoError(t, err)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, e.manager, "test"))
	assert.Equal(t, http.StatusOK, e.agent(http.MethodGet, pathPlan, nil).Code)
	assert.Equal(t, http.StatusForbidden, e.agent(http.MethodGet, pathSecret, nil).Code)
	assert.Equal(t, http.StatusForbidden, e.agent(http.MethodGet, pathLoose, nil).Code)
	assert.Equal(t, http.StatusForbidden, e.agent(http.MethodGet, pathSolo, nil).Code)
}

// A scoped grant of the per-kind slug itself (process.runs.read takes a peer=
// scope) does not hide a scoped umbrella that reaches the resource.
func TestResourcesUmbrellaIsNotHiddenByAScopedKindGrant(t *testing.T) {
	e := newUmbrellaEnv(t)
	e.grant(agentd.PermProcessRunsRead, `{"peer":["inst_elsewhere"]}`)
	e.grant(agentd.PermResourcesRead, `{"group":["crew"]}`)
	assert.Equal(t, http.StatusOK, e.agent(http.MethodGet, "/v1/process/runs?group=crew", nil).Code)
	assert.Equal(t, http.StatusForbidden, e.agent(http.MethodGet, "/v1/process/runs?group=hidden", nil).Code)
}

// resources.write implies resources.read, scope included, below a deny of
// resources.read.
func TestResourcesWriteUmbrellaImpliesRead(t *testing.T) {
	e := newUmbrellaEnv(t)
	e.grant(agentd.PermResourcesWrite, `{"group":["crew"]}`)
	assert.Equal(t, map[string]int{
		pathPlan: http.StatusOK, pathSecret: http.StatusForbidden, pathLoose: http.StatusForbidden,
		pathShip: http.StatusOK, pathShared: http.StatusOK, pathSolo: http.StatusForbidden,
	}, e.reads(umbrellaReadPaths...))
	assert.Equal(t, []string{"plan"}, e.listedBoards())
	assert.Equal(t, []string{"shared", "ship"}, e.listedTemplates())

	require.NoError(t, db.SetAgentPermissionOverride(e.manager, agentd.PermResourcesRead, db.PermEffectDeny, "test"))
	assert.Equal(t, http.StatusForbidden, e.agent(http.MethodGet, pathPlan, nil).Code, "a deny of resources.read wins")
	assert.Equal(t, http.StatusOK, e.draw("plan"), "the write itself stays")
}

// Saving a template that does not exist creates it, which is a library-wide
// change: a resource= scope naming that id does not reach it, while an
// unscoped grant does.
func TestResourcesUmbrellaScopedWriteCannotCreateATemplate(t *testing.T) {
	e := newUmbrellaEnv(t)
	e.grant(agentd.PermResourcesWrite, `{"resource":["process_template:fresh"],"group":["crew"]}`)
	create := func() int {
		return e.agent(http.MethodPost, "/v1/process/templates/fresh", map[string]any{"template": shipFeatureTemplate("fresh")}).Code
	}
	assert.Equal(t, http.StatusForbidden, create())
	e.grant(agentd.PermResourcesWrite, `{"resource":["process_template:fresh"]}`)
	assert.Equal(t, http.StatusForbidden, create())
	e.grant(agentd.PermResourcesWrite, "")
	assert.Equal(t, http.StatusCreated, create())
}
