package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// tcl-i9elme: a trusted peer's operator locks this node's group resources
// from their own dashboard only as far as this node's grants to that peer
// reach (groups.resources.lock, in every group the resource belongs to). The
// peer is never this node's operator, and no agent, here or federated,
// reaches the lock.

type peerResourcesOut struct {
	Group     string `json:"group"`
	CanLock   bool   `json:"canLock"`
	Processes []struct {
		TemplateID   string `json:"templateId"`
		AgentsLocked bool   `json:"agentsLocked"`
	} `json:"processes"`
	Boards []struct {
		ID           string `json:"id"`
		AgentsLocked bool   `json:"agentsLocked"`
	} `json:"boards"`
	Pages []struct {
		ID           string `json:"id"`
		AgentsLocked bool   `json:"agentsLocked"`
	} `json:"pages"`
}

// peerLockEnv is one node seen by peer "bob" through its peer view, with the
// crew group's resources: board "plan", process "ship" (pinned to crew) and
// process "shared" (pinned to crew and backstage), and a page.
type peerLockEnv struct {
	fh   *fedHarness
	view http.Handler
	page string
}

func newPeerLockEnv(t *testing.T) *peerLockEnv {
	t.Helper()
	fh := newFedHarness(t)
	f := fh.f
	require.NoError(t, os.MkdirAll(filepath.Dir(config.ConfigPath()), 0o755))
	require.NoError(t, os.WriteFile(config.ConfigPath(), []byte(`{"features":{"processes":true}}`), 0o644))
	t.Cleanup(agentd.SetProcessStoreRootForTest(filepath.Join(f.World.HomeDir, ".tclaude", "processes")))
	const origin = "http://localhost:12345"
	t.Cleanup(agentd.SetPopupBaseURLForTest(origin))
	agentd.CloseAllLivePagesForTest()
	t.Cleanup(agentd.CloseAllLivePagesForTest)
	f.HaveGroup("crew")
	f.HaveGroup("backstage")
	f.HaveGroup("hidden")

	dash := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		r := testharness.JSONRequest(t, method, path, body)
		r.Header.Set("Origin", origin)
		return testharness.Serve(agentd.BuildDashboardHandlerForTest(), r)
	}
	rec := dash(http.MethodPost, "/api/boards", map[string]any{"id": "plan", "title": "Plan"})
	require.Less(t, rec.Code, 300, rec.Body.String())
	rec = dash(http.MethodPost, "/api/boards/plan/group", map[string]any{"group": "crew"})
	require.Less(t, rec.Code, 300, rec.Body.String())
	rec = dash(http.MethodPost, "/api/boards", map[string]any{"id": "secret", "title": "Secret"})
	require.Less(t, rec.Code, 300, rec.Body.String())
	rec = dash(http.MethodPost, "/api/boards/secret/group", map[string]any{"group": "hidden"})
	require.Less(t, rec.Code, 300, rec.Body.String())
	for _, id := range []string{"ship", "shared"} {
		rec = processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/"+id, map[string]any{"template": shipFeatureTemplate(id)})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		rec = processTemplateRequest(t, f, http.MethodPost, "/v1/process/groups/crew/processes", map[string]any{"templateId": id})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	rec = processTemplateRequest(t, f, http.MethodPost, "/v1/process/groups/backstage/processes", map[string]any{"templateId": "shared"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = dash(http.MethodPost, "/api/pages", map[string]any{"group": "crew", "name": "sketch", "title": "Sketch"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var page struct {
		ID string `json:"id"`
	}
	testharness.DecodeJSON(t, rec, &page)
	require.NotEmpty(t, page.ID)
	return &peerLockEnv{fh: fh, view: agentd.PeerViewHandler(fh.peer.id.ID()), page: page.ID}
}

func (e *peerLockEnv) grant(t *testing.T, slug, group string) {
	t.Helper()
	rec := fedHuman(t, e.fh.f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug, "scope": "group=" + group})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func (e *peerLockEnv) revoke(t *testing.T, slug, group string) {
	t.Helper()
	g, err := db.GetAgentGroupByName(group)
	require.NoError(t, err)
	_, err = db.DeleteFederationPeerGrant(e.fh.peer.id.ID(), slug, db.FederationGroupScope(g.ID))
	require.NoError(t, err)
}

func (e *peerLockEnv) lock(t *testing.T, kind, id string, locked bool) *httptest.ResponseRecorder {
	t.Helper()
	return testharness.Serve(e.view, testharness.JSONRequest(t, http.MethodPost, "/api/resource-locks", map[string]any{"kind": kind, "id": id, "locked": locked}))
}

func (e *peerLockEnv) resources(t *testing.T, group string) (peerResourcesOut, *httptest.ResponseRecorder) {
	t.Helper()
	rec := testharness.Serve(e.view, testharness.JSONRequest(t, http.MethodGet, "/api/groups/"+group+"/resources", nil))
	var out peerResourcesOut
	if rec.Code == http.StatusOK {
		testharness.DecodeJSON(t, rec, &out)
	}
	return out, rec
}

func pageLocked(t *testing.T, e *peerLockEnv) bool {
	t.Helper()
	out, rec := e.resources(t, "crew")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	for _, p := range out.Pages {
		if p.ID == e.page {
			return p.AgentsLocked
		}
	}
	t.Fatalf("page %s is not listed", e.page)
	return false
}

func requirePeerRefused(t *testing.T, rec *httptest.ResponseRecorder, where string) {
	t.Helper()
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), agentd.PermGroupsResourcesLock)
	assert.Contains(t, rec.Body.String(), where)
}

// Without a grant a peer's lock toggle is refused, and nothing changes; with
// groups.resources.lock on the group it works, for every kind; revoked, it is
// refused again.
func TestPeerViewResourceLockFollowsPeerGrants(t *testing.T) {
	e := newPeerLockEnv(t)

	// A group the peer does not see: its resources are not found, not refused.
	rec := e.lock(t, "board", "plan", true)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	_, rec = e.resources(t, "crew")
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	// The roster grant shows the group and its resources, read-only.
	e.grant(t, agentd.PermGroupsRosterRead, "crew")
	out, rec := e.resources(t, "crew")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.False(t, out.CanLock, "no lock grant yet")
	require.Len(t, out.Boards, 1)
	assert.Equal(t, "plan", out.Boards[0].ID)
	require.Len(t, out.Processes, 2)
	require.Len(t, out.Pages, 1)
	assert.NotContains(t, rec.Body.String(), "secret", "another group's board is not listed")

	for _, c := range []struct{ kind, id string }{{"board", "plan"}, {"process", "ship"}, {"page", e.page}} {
		requirePeerRefused(t, e.lock(t, c.kind, c.id, true), "group:crew")
	}
	assert.False(t, db.GroupResourceLocked(db.GroupResourceBoard, "plan"), "a refused toggle changes nothing")
	assert.False(t, db.GroupResourceLocked(db.GroupResourceProcess, "ship"))
	assert.False(t, pageLocked(t, e))

	// Granted: every kind locks, and the peer's read says so.
	e.grant(t, agentd.PermGroupsResourcesLock, "crew")
	out, _ = e.resources(t, "crew")
	assert.True(t, out.CanLock)
	for _, c := range []struct{ kind, id string }{{"board", "plan"}, {"process", "ship"}, {"page", e.page}} {
		rec = e.lock(t, c.kind, c.id, true)
		require.Equal(t, http.StatusOK, rec.Code, "%s %s: %s", c.kind, c.id, rec.Body.String())
	}
	assert.True(t, db.GroupResourceLocked(db.GroupResourceBoard, "plan"))
	assert.True(t, db.GroupResourceLocked(db.GroupResourceProcess, "ship"))
	assert.True(t, pageLocked(t, e))

	// A template pinned to a group the peer is not granted (here: not even
	// shown) stays out of reach: its lock holds in every group it is in.
	requirePeerRefused(t, e.lock(t, "process", "shared", true), "a group this node does not show you")
	e.grant(t, agentd.PermGroupsRosterRead, "backstage")
	requirePeerRefused(t, e.lock(t, "process", "shared", true), "group:backstage")
	e.grant(t, agentd.PermGroupsResourcesLock, "backstage")
	rec = e.lock(t, "process", "shared", true)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, db.GroupResourceLocked(db.GroupResourceProcess, "shared"))

	// Another group's board stays not found, granted or not elsewhere.
	rec = e.lock(t, "board", "secret", true)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.False(t, db.GroupResourceLocked(db.GroupResourceBoard, "secret"))

	// The toggle is audited as the peer's operator, never as this node's.
	audit, err := db.ListFederationActivity(e.fh.peer.id.ID(), time.Time{}, 200)
	require.NoError(t, err)
	found := false
	for _, row := range audit {
		if row.Kind == "federation.peer_view" && row.Status == http.StatusOK {
			assert.Equal(t, "remote:operator@"+e.fh.peer.id.ID(), row.Actor)
			found = true
		}
	}
	assert.True(t, found, "a peer lock toggle leaves a federation audit row")

	// Revoked: unlocking is refused, and the lock holds.
	e.revoke(t, agentd.PermGroupsResourcesLock, "crew")
	requirePeerRefused(t, e.lock(t, "board", "plan", false), "group:crew")
	assert.True(t, db.GroupResourceLocked(db.GroupResourceBoard, "plan"))
}

// Unrestricted trust holds the grant in every group it sees.
func TestPeerViewResourceLockUnrestrictedTrust(t *testing.T) {
	e := newPeerLockEnv(t)
	setFedTrustLevel(t, e.fh, db.FederationTrustUnrestricted)
	out, rec := e.resources(t, "crew")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, out.CanLock)
	rec = e.lock(t, "process", "shared", true)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, db.GroupResourceLocked(db.GroupResourceProcess, "shared"))
	rec = e.lock(t, "board", "plan", true)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// No agent toggles a lock, whatever it holds: not a member of the group with
// every grant (the peer slug included), and not an agent asking its own node
// to reach a peer's lock through the peer proxy.
func TestResourceLockNeverReachesAFederatedAgent(t *testing.T) {
	e := newPeerLockEnv(t)
	f := e.fh.f
	e.grant(t, agentd.PermGroupsRosterRead, "crew")
	e.grant(t, agentd.PermGroupsResourcesLock, "crew")
	const conv = "fed-lock-agent"
	f.HaveConvWithTitle(conv, "visitor")
	f.HaveMember("crew", conv)
	grantEverything(t, conv, agentd.PermGroupsResourcesLock, agentd.PermBoardsWrite, agentd.PermProcessTemplatesManage)
	agent := func(method, path string, body any) *httptest.ResponseRecorder {
		return testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, method, path, body), conv))
	}
	body := map[string]any{"kind": "board", "id": "plan", "locked": true}
	for _, path := range []string{
		"/api/resource-locks",
		"/api/peer/" + e.fh.peer.id.ID() + "/api/resource-locks",
		"/v1/resource-locks",
	} {
		rec := agent(http.MethodPost, path, body)
		assert.NotEqual(t, http.StatusOK, rec.Code, "%s: %s", path, rec.Body.String())
	}
	assert.False(t, db.GroupResourceLocked(db.GroupResourceBoard, "plan"), "no agent request locked it")
}
