package agentd_test

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Drawing boards across nodes for agents (tk_8d5c1f6e): an agent lists,
// reads and exports a linked node's drawing boards through an agent peer
// view, never edits them, and attaches a linked node's export to a task
// here with the node in its reference.

// The receiving node serves an agent's board reads and exports under the
// grants it gave the agent's node, groups.resources.read in the board's
// group, and refuses its edits.
func TestPeerDrawingBoardsAgentReceiver(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	crew := f.HaveGroup("crew")
	secret := f.HaveGroup("secret")
	dash := agentd.BuildDashboardHandlerForTest()
	for id, group := range map[string]int64{"plan": crew.ID, "hidden": secret.ID} {
		_, err := db.CreateCollabDrawingBoard(id, id, "operator", 500)
		require.NoError(t, err)
		require.NoError(t, db.SetCollabDrawingBoardGroup(id, group))
		rec := testharness.Serve(dash, drawingBoardDashReq(t, http.MethodPatch, "/api/drawing-boards/"+id, map[string]any{"elements": []any{rect("box", 1, 1, 0)}}))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	worker := &proto.PeerViewAgent{ID: "agt_bobworker000000000000000", Name: "bob-worker"}
	exchange := func(method, uri string, body any) peerViewWire {
		t.Helper()
		return agentPeerViewExchange(t, p, worker, method, uri, body)
	}

	reply := exchange("POST", "/api/drawing-boards/plan/export", map[string]any{"format": "png"})
	require.Equal(t, http.StatusNotFound, reply.Status, "no grant in its group: not the peer's to know: %s", reply.Body)

	rec := fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermGroupsResourcesRead, "scope": "group=crew"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	reply = exchange("GET", "/api/drawing-boards", nil)
	require.Equal(t, http.StatusOK, reply.Status, string(reply.Body))
	var list struct {
		DrawingBoards []struct {
			ID string `json:"id"`
		} `json:"drawingBoards"`
	}
	require.NoError(t, json.Unmarshal(reply.Body, &list))
	require.Len(t, list.DrawingBoards, 1)
	assert.Equal(t, "plan", list.DrawingBoards[0].ID, "only the boards of the groups it was granted")

	reply = exchange("GET", "/api/drawing-boards/plan", nil)
	require.Equal(t, http.StatusOK, reply.Status, string(reply.Body))
	assert.Contains(t, string(reply.Body), `"box"`)
	reply = exchange("GET", "/api/drawing-boards/hidden", nil)
	assert.Equal(t, http.StatusNotFound, reply.Status, string(reply.Body))

	reply = exchange("POST", "/api/drawing-boards/plan/export", map[string]any{"format": "png"})
	require.Equal(t, http.StatusOK, reply.Status, string(reply.Body))
	var out struct {
		ContentType string `json:"content_type"`
		Data        []byte `json:"data"`
	}
	require.NoError(t, json.Unmarshal(reply.Body, &out))
	assert.Equal(t, "image/png", out.ContentType)
	_, err := png.DecodeConfig(bytes.NewReader(out.Data))
	require.NoError(t, err, "the data is the PNG")
	reply = exchange("POST", "/api/drawing-boards/hidden/export", map[string]any{"format": "png"})
	assert.Equal(t, http.StatusNotFound, reply.Status, string(reply.Body))

	// An agent never edits a peer's board, whatever the peer was granted.
	rec = fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermGroupsResourcesWrite, "scope": "group=crew"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	reply = exchange("PATCH", "/api/drawing-boards/plan", map[string]any{"elements": []any{rect("box", 2, 2, 50)}})
	require.Equal(t, http.StatusForbidden, reply.Status, string(reply.Body))
	assert.Contains(t, string(reply.Body), "agent_principal")

	// The operator turns the peer's agent views off.
	rec = fedHuman(t, f, "POST", "/v1/federation/peers/agent-views", map[string]any{"instance": "bob", "policy": "deny"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	reply = exchange("POST", "/api/drawing-boards/plan/export", map[string]any{"format": "png"})
	require.Equal(t, http.StatusForbidden, reply.Status)
	assert.Contains(t, string(reply.Body), "agent_views_off")
}

// The sending node's gate: an agent reaches a linked node's drawing boards
// with drawing-boards.read granted peer=<node>, for the whole node, since a
// board id names no group there.
func TestPeerDrawingBoardsAgentSender(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	agentd.ResetAgentPeerViewLimitsForTest()
	t.Cleanup(agentd.ResetAgentPeerViewLimitsForTest)
	inst := p.id.ID()
	const caller = "019fe740-43a4-7023-b8ae-0000000db0a1"
	f.HaveEnrolledAgent(caller)
	sendPeerCatalog(t, p, inst)
	call := func(method, tail string, body any) *httptest.ResponseRecorder {
		t.Helper()
		return testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, method, "/v1/federation/peer/bob/"+tail, body), caller))
	}
	grant := func(scope string) {
		t.Helper()
		require.NoError(t, db.GrantAgentPermissionWithScope(caller, agentd.PermDrawingBoardsRead, `{"peer":["`+scope+`"]}`, "test"))
	}

	rec := call("GET", "drawing-boards/plan", nil)
	require.Equal(t, http.StatusForbidden, rec.Code, "no grant: %s", rec.Body.String())
	require.NoError(t, db.SetAgentPermissionOverride(caller, agentd.PermDrawingBoardsRead, db.PermEffectGrant, "test"))
	rec = call("GET", "drawing-boards/plan", nil)
	require.Equal(t, http.StatusForbidden, rec.Code, "an unscoped grant never opens an agent peer view: %s", rec.Body.String())
	grant(inst + "/lab")
	rec = call("GET", "drawing-boards/plan", nil)
	require.Equal(t, http.StatusForbidden, rec.Code, "a board id names no group: a group's grant does not reach it: %s", rec.Body.String())
	rec = call("PATCH", "drawing-boards/plan", map[string]any{"elements": []any{}})
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "agent_route_not_allowed", "an agent never edits a peer's board")

	grant(inst)
	for _, c := range []struct{ method, tail, uri string }{
		{"GET", "drawing-boards", "/api/drawing-boards"},
		{"GET", "drawing-boards/plan?since=3", "/api/drawing-boards/plan?since=3"},
		{"POST", "drawing-boards/plan/export?format=json", "/api/drawing-boards/plan/export?format=json"},
	} {
		after := len(p.envelopes(proto.KindPeerViewOpen))
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- call(c.method, c.tail, map[string]any{"format": "png"}) }()
		conn := acceptOutboundPeerView(t, p, after)
		wire := readPeerViewWire(t, conn)
		assert.Equal(t, c.method, wire.Method)
		assert.Equal(t, c.uri, wire.URI)
		writePeerViewWire(t, conn, peerViewWire{Status: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"ok":true}`)})
		rec := <-done
		require.Equal(t, http.StatusOK, rec.Code, "%s %s: %s", c.method, c.tail, rec.Body.String())
		_ = conn.Close()
	}
}

// export --node --attach-to: the linked node draws the board through its
// peer view, as the caller, and the task here keeps the PNG with the node
// in its reference. Exporting the same part again updates that file; a
// board of the same id on this node is another file.
func TestTaskboardDrawingFromALinkedNode(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	agentd.ResetAgentPeerViewLimitsForTest()
	t.Cleanup(agentd.ResetAgentPeerViewLimitsForTest)
	inst := p.id.ID()
	f.HaveGroup("crew")
	sendPeerCatalog(t, p, inst)
	rec := fedHuman(t, f, "POST", "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "explain the design"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var task struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &task))
	drawings := "/v1/taskboard/tasks/" + task.ID + "/drawings"

	var pngData bytes.Buffer
	require.NoError(t, png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 4, 3))))
	peerReply := func(body []byte) {
		t.Helper()
		after := len(p.envelopes(proto.KindPeerViewOpen))
		conn := acceptOutboundPeerView(t, p, after-1)
		wire := readPeerViewWire(t, conn)
		assert.Equal(t, "POST", wire.Method)
		assert.Equal(t, "/api/drawing-boards/plan/export?format=json", wire.URI)
		var asked map[string]any
		require.NoError(t, json.Unmarshal(wire.Body, &asked))
		assert.Equal(t, "png", asked["format"], "the node draws a PNG")
		writePeerViewWire(t, conn, peerViewWire{Status: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: body})
		_ = conn.Close()
	}
	exported := mustJSON(t, map[string]any{"content_type": "image/png", "filename": "plan.png", "data": pngData.Bytes(),
		"width": 4, "height": 3, "version": 9, "elements": 1, "crop": map[string]any{"x": 0, "y": 0, "width": 4, "height": 3}})
	attach := func(req *http.Request, reply []byte) *httptest.ResponseRecorder {
		t.Helper()
		before := len(p.envelopes(proto.KindPeerViewOpen))
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- testharness.Serve(f.Mux, req) }()
		fedEventually(t, "the export goes out", func() bool { return len(p.envelopes(proto.KindPeerViewOpen)) > before })
		peerReply(reply)
		return <-done
	}
	type drawingReply struct {
		Replaced   bool `json:"replaced"`
		Attachment struct {
			ID      int64 `json:"id"`
			Drawing struct {
				Board   string `json:"board"`
				Node    string `json:"node"`
				Version int64  `json:"version"`
			} `json:"drawing"`
		} `json:"attachment"`
	}
	decode := func(rec *httptest.ResponseRecorder, want int) drawingReply {
		t.Helper()
		require.Equal(t, want, rec.Code, rec.Body.String())
		var out drawingReply
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		return out
	}

	first := decode(attach(agentd.AsHumanPeer(testharness.JSONRequest(t, "POST", drawings, map[string]any{"board": "plan", "node": "bob"})), exported), http.StatusCreated)
	assert.Equal(t, "plan", first.Attachment.Drawing.Board)
	assert.Equal(t, inst, first.Attachment.Drawing.Node, "the reference pins the node's instance, not its label")
	assert.Equal(t, int64(9), first.Attachment.Drawing.Version)
	again := decode(attach(agentd.AsHumanPeer(testharness.JSONRequest(t, "POST", drawings, map[string]any{"board": "plan", "node": "bob"})), exported), http.StatusOK)
	assert.True(t, again.Replaced, "the same part of the same node's board updates its file")
	assert.Equal(t, first.Attachment.ID, again.Attachment.ID)

	// What the node answers is its content: not a PNG, it is not stored.
	bad := mustJSON(t, map[string]any{"content_type": "image/png", "data": []byte("<svg/>"), "version": 9})
	rec = attach(agentd.AsHumanPeer(testharness.JSONRequest(t, "POST", drawings, map[string]any{"board": "plan", "node": "bob"})), bad)
	assert.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())

	// An agent attaches under its peer= grant, and is refused without it
	// before anything goes out.
	const caller = "019fe740-43a4-7023-b8ae-0000000db0a2"
	f.HaveEnrolledAgent(caller)
	f.HaveMember("crew", caller)
	require.NoError(t, db.GrantAgentPermissionWithScope(caller, agentd.PermTaskboardWrite, `{"group":["crew"]}`, "test"))
	agentReq := func() *http.Request {
		return agentd.AsAgentPeer(testharness.JSONRequest(t, "POST", drawings, map[string]any{"board": "plan", "node": "bob", "elements": []string{"box"}}), caller)
	}
	before := len(p.envelopes(proto.KindPeerViewOpen))
	rec = testharness.Serve(f.Mux, agentReq())
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), agentd.PermDrawingBoardsRead)
	assert.Len(t, p.envelopes(proto.KindPeerViewOpen), before, "nothing went out")
	require.NoError(t, db.GrantAgentPermissionWithScope(caller, agentd.PermDrawingBoardsRead, `{"peer":["`+inst+`"]}`, "test"))
	// A node it names that does not exist is counted and audited as any
	// agent peer view is: no uncounted probing of which nodes there are.
	probe := agentd.AsAgentPeer(testharness.JSONRequest(t, "POST", drawings, map[string]any{"board": "plan", "node": "nosuch"}), caller)
	rec = testharness.Serve(f.Mux, probe)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	probes, err := db.ListAuditLog(db.AuditLogFilter{Verb: "federation.peer_view.agent.out"})
	require.NoError(t, err)
	require.NotEmpty(t, probes)
	assert.Contains(t, probes[0].Detail, "peer=nosuch")
	part := decode(attach(agentReq(), exported), http.StatusCreated)
	assert.NotEqual(t, first.Attachment.ID, part.Attachment.ID, "a part of the board is a file of its own")
	assert.Equal(t, inst, part.Attachment.Drawing.Node)
}

// sendPeerCatalog has the test peer advertise agent peer views, with a lab
// group, and waits until this node stored it.
func sendPeerCatalog(t *testing.T, p *fedPeer, inst string) {
	t.Helper()
	before, _, _ := db.GetFederationCatalog(inst)
	p.send(p.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{PeerViewAgents: true, Groups: []proto.CatalogGroup{{Name: "lab"}}}))
	fedEventually(t, "catalog stored", func() bool {
		after, _, _ := db.GetFederationCatalog(inst)
		return after != before && after != ""
	})
}
