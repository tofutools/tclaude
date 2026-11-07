package agentd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// setGroupGrant replaces group's group grants with one slug and scope, the
// way the group ⚙ permission editor saves them.
func setGroupGrant(t *testing.T, call func(method, path string, body any) *httptest.ResponseRecorder, group, slug string, scope map[string][]string) {
	t.Helper()
	rec := call(http.MethodPatch, "/v1/groups/"+group, map[string]any{
		"permissions": []any{map[string]any{"slug": slug, "scope": scope}},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// Scenario: the operator gives the whole website group read, then write, on
// crew's board, as a group grant scoped group=crew. Every member of website
// then reaches crew's board through the CLI, with no grant of its own; a
// member of neither group still does not.
func TestTaskboardGroupGrantOpensAnotherGroupsBoard(t *testing.T) {
	h := newTaskboardFlow(t)
	const (
		member   = "tb-gg-member-1111"
		outsider = "tb-gg-outsider-2222"
		crewMate = "tb-gg-crew-3333"
	)
	h.agent(t, member)
	h.agent(t, outsider)
	h.agent(t, crewMate)
	h.f.HaveGroup("crew")
	h.f.HaveGroup("website")
	h.f.HaveGroup("ops")
	h.f.HaveMember("website", member)
	h.f.HaveMember("ops", outsider)
	h.f.HaveMember("crew", crewMate)
	task := h.task(h.call("", http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "crew plan"}), http.StatusCreated)
	operator := func(method, path string, body any) *httptest.ResponseRecorder { return h.call("", method, path, body) }
	bridgeAgentClientToMuxAsAgent(t, h.f.Mux, member)
	run := func(args ...string) (int, string, string) {
		t.Helper()
		stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
		rc := agent.RunTaskboardCLI(args, stdout, stderr)
		return rc, stdout.String(), stderr.String()
	}

	rc, out, errOut := run("ls", "crew")
	require.NotEqual(t, 0, rc, "no grant yet: out=%s err=%s", out, errOut)

	setGroupGrant(t, operator, "website", agentd.PermTaskboardRead, map[string][]string{"group": {"crew"}})
	rc, out, errOut = run("ls", "crew")
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)
	assert.Contains(t, out, "crew plan")
	rc, out, errOut = run("show", task.ID)
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)
	assert.Contains(t, out, "crew plan")
	rc, out, errOut = run("add", "crew", "from website")
	assert.NotEqual(t, 0, rc, "read does not write: out=%s err=%s", out, errOut)
	assert.Contains(t, out+errOut, agentd.PermTaskboardWrite)

	setGroupGrant(t, operator, "website", agentd.PermTaskboardWrite, map[string][]string{"group": {"crew"}})
	rc, out, errOut = run("add", "crew", "from website")
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)
	rc, out, errOut = run("promote", task.ID)
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)
	rc, out, errOut = run("ls", "crew")
	require.Equal(t, 0, rc, "write implies read: out=%s err=%s", out, errOut)
	assert.Contains(t, out, "from website")

	// Like an explicit per-agent grant, the group grant passes crew's writer
	// lock, which names someone else.
	rec := h.call("", http.MethodPost, "/v1/groups/crew/taskboard/writers", map[string]any{"agents": []string{crewMate}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rc, out, errOut = run("add", "crew", "past the lock")
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)

	// The grant is website's, not everyone's, and not ops's board.
	assert.Equal(t, http.StatusForbidden, h.call(outsider, http.MethodGet, "/v1/groups/crew/taskboard", nil).Code)
	assert.Equal(t, http.StatusForbidden, h.call(member, http.MethodGet, "/v1/groups/ops/taskboard", nil).Code)

	// Leaving website ends it at once.
	website, err := db.GetAgentGroupByName("website")
	require.NoError(t, err)
	require.NoError(t, db.RemoveAgentGroupMember(website.ID, member))
	assert.Equal(t, http.StatusNotFound, h.call(member, http.MethodGet, "/v1/taskboard/tasks/"+task.ID, nil).Code)
}

// Scenario: an agent of the local website group reads bob's crew board. Two
// layers decide, and each refusal names the one that is missing: this node's
// group grant (taskboard.read scoped peer=bob/crew), and bob's peer grant to
// this node (federation grant … taskboard.read --scope group=crew). The test
// node plays bob's receiving side for itself, as the other --peer CLI tests do.
func TestTaskboardGroupGrantReachesAPeersBoard(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Features = &config.FeaturesConfig{Taskboard: new(true)}
	require.NoError(t, config.Save(cfg))
	agentd.ResetAgentPeerViewLimitsForTest()
	t.Cleanup(agentd.ResetAgentPeerViewLimitsForTest)
	inst := p.id.ID()
	const member = "019fe740-43a4-7023-b8ae-0000000a7b01"
	f.HaveEnrolledAgent(member)
	f.HaveGroup("website")
	f.HaveMember("website", member)
	crew := f.HaveGroup("crew")
	_, err = db.CreateGroupTask(db.NewGroupTask{GroupID: crew.ID, Title: "bob's plan", Actor: "test"})
	require.NoError(t, err)
	sendCatalog := func(groups ...string) {
		t.Helper()
		before, _, _ := db.GetFederationCatalog(inst)
		cat := proto.CatalogPayload{PeerViewAgents: true}
		for _, g := range groups {
			cat.Groups = append(cat.Groups, proto.CatalogGroup{Name: g})
		}
		p.send(p.envelope(proto.KindCatalog, proto.Endpoint{}, cat))
		fedEventually(t, "catalog stored", func() bool { after, _, _ := db.GetFederationCatalog(inst); return after != before && after != "" })
	}
	sendCatalog("other")
	operator := func(method, path string, body any) *httptest.ResponseRecorder {
		return fedHuman(t, f, method, path, body)
	}
	bridgeAgentClientToMuxAsAgent(t, f.Mux, member)
	receiver := agentd.PeerViewHandler(inst)
	opened := 0
	// run runs the CLI; answered says whether the sender opens a view for bob
	// to answer, which it does only once its own gate passed.
	run := func(answered bool, args ...string) (int, string) {
		t.Helper()
		stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
		done := make(chan int, 1)
		go func() { done <- agent.RunTaskboardCLI(args, stdout, stderr) }()
		if answered {
			conn := acceptOutboundPeerView(t, p, opened)
			opened++
			wire := readPeerViewWire(t, conn)
			req := httptest.NewRequest(wire.Method, wire.URI, bytes.NewReader(wire.Body))
			req.Header = wire.Header
			rec := testharness.Serve(receiver, req)
			writePeerViewWire(t, conn, peerViewWire{Status: rec.Code, Header: rec.Header(), Body: rec.Body.Bytes()})
			defer func() { _ = conn.Close() }()
		}
		rc := <-done
		return rc, stdout.String() + stderr.String()
	}

	// Layer 1 missing: bob grants this node nothing on crew, so crew is not in
	// bob's catalog here, and the refusal says whose grant that is.
	setGroupGrant(t, operator, "website", agentd.PermTaskboardRead, map[string][]string{"peer": {"bob/crew"}})
	rc, out := run(false, "ls", "crew", "--peer", "bob")
	require.NotEqual(t, 0, rc, out)
	assert.Contains(t, out, `bob has not granted this node anything on group "crew"`)
	assert.Contains(t, out, "tclaude federation grant <this node> taskboard.read --scope group=crew")

	// Bob shows crew (it grants this node something there) but not its board:
	// bob's own refusal names its missing peer grant.
	sendCatalog("crew")
	fhGrant := &peerProcessHarness{fedHarness: fh}
	fhGrant.grant(t, agentd.PermGroupsRosterRead, "group=crew")
	rc, out = run(true, "ls", "crew", "--peer", "bob")
	require.NotEqual(t, 0, rc, out)
	assert.Contains(t, out, "a peer grant this node has not given your node")
	assert.Contains(t, out, "--scope group=crew")

	// Both layers: the board answers.
	fhGrant.grant(t, agentd.PermTaskboardRead, "group=crew")
	rc, out = run(true, "ls", "crew", "--peer", "bob")
	require.Equal(t, 0, rc, out)
	assert.Contains(t, out, "bob's plan")

	// Layer 2 missing: without this node's group grant, the sender refuses
	// before opening anything, naming the peer= scope it needs.
	setGroupGrant(t, operator, "website", agentd.PermTaskboardWrite, map[string][]string{"group": {"crew"}})
	rc, out = run(false, "ls", "crew", "--peer", "bob")
	require.NotEqual(t, 0, rc, "a local group= scope never reaches a peer: %s", out)
	assert.Contains(t, out, "for peer="+inst+"/crew")
}

// The permission editor's peer= picker lists each linked node, and the groups
// its catalog shows this node, by label, valued in the stored instance-id form.
// A linked node reaching the editor as local does not see this node's peers.
func TestPermissionScopeValues_PeerCatalogue(t *testing.T) {
	fh := newFedHarness(t)
	p := fh.peer
	inst := p.id.ID()
	p.send(p.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{Groups: []proto.CatalogGroup{{Name: "crew"}, {Name: "ops"}}}))
	fedEventually(t, "catalog stored", func() bool { raw, _, _ := db.GetFederationCatalog(inst); return raw != "" })
	mux := agentd.BuildDashboardHandlerForTest()
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/permissions/scope-values?dim=peer", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		Items []struct {
			Kind, ID, Title, Value, Node string
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	var values, titles []string
	for _, item := range out.Items {
		assert.Equal(t, "peer", item.Kind)
		assert.Equal(t, "bob", item.Node)
		values = append(values, item.Value)
		titles = append(titles, item.Title)
	}
	assert.Equal(t, []string{inst, inst + "/crew", inst + "/ops"}, values)
	assert.Equal(t, []string{"bob (whole node)", "bob / crew", "bob / ops"}, titles)

	view := agentd.PeerViewHandler(inst)
	require.NoError(t, db.TrustFederationPeer(func() db.FederationPeer {
		peer, err := db.GetFederationPeer(inst)
		require.NoError(t, err)
		peer.TrustLevel = db.FederationTrustUnrestricted
		return *peer
	}()))
	rec = testharness.Serve(view, testharness.JSONRequest(t, http.MethodGet, "/api/permissions/scope-values?dim=peer", nil))
	assert.Equal(t, http.StatusForbidden, rec.Code, "an unrestricted peer does not list this node's peers: %s", rec.Body.String())
}
