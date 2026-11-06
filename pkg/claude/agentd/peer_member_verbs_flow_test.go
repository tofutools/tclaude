package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// Pins: a local agent whose title has the member@peer shape stays local, and
// a peer operator's view metadata names its real trust and grants.
func TestPeerMemberVerbPins(t *testing.T) {
	fh := newFedHarness(t)
	f := fh.f
	f.HaveGroup("crew")
	f.HaveAliveSession(moveSourceConv, "pin-local", "pin-local-pane", testutil.CanonicalTempDir(t))
	f.HaveConvWithTitle(moveSourceConv, "worker@bob")
	f.HaveMember("crew", moveSourceConv)
	rec := fedHuman(t, f, http.MethodPost, "/v1/agent/worker@bob/stop", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), moveSourceConv, "a local title wins over a peer label")

	inst := fh.peer.id.ID()
	rec = fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermGroupsMembersStop, "scope": "group=crew"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	meta := func() map[string]any {
		t.Helper()
		out := testharness.Serve(agentd.PeerViewHandler(inst), testharness.JSONRequest(t, "GET", "/api/groups", nil))
		require.Equal(t, 200, out.Code, out.Body.String())
		var body struct {
			PeerView map[string]any `json:"peer_view"`
		}
		require.NoError(t, json.Unmarshal(out.Body.Bytes(), &body))
		return body.PeerView
	}
	m := meta()
	require.Equal(t, "restricted", m["level"])
	require.Contains(t, m["groups"], "crew")
	peer, err := db.GetFederationPeer(inst)
	require.NoError(t, err)
	peer.TrustLevel = db.FederationTrustUnrestricted
	require.NoError(t, db.TrustFederationPeer(*peer))
	m = meta()
	require.Equal(t, "unrestricted", m["level"])
	require.Nil(t, m["groups"], "unrestricted trust holds every grant; none is listed")
}

// Two local agents titled member@peer are an ambiguous local selector, not a
// peer's agent: the verb never reaches the peer.
func TestRemoteMemberVerbAmbiguousLocalStaysLocal(t *testing.T) {
	fh := newFedHarness(t)
	f := fh.f
	remoteMemberCatalog(t, fh)
	f.HaveConvWithTitle("019fe740-43a4-7023-b8ae-0000000a1b01", "target@bob")
	f.HaveConvWithTitle("019fe740-43a4-7023-b8ae-0000000a1b02", "target@bob")
	rec := fedHuman(t, f, http.MethodPost, "/v1/agent/target@bob/stop", nil)
	require.Equal(t, 404, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "multiple")
	require.Empty(t, fh.peer.envelopes(proto.KindPeerViewOpen), "an ambiguous local selector opens no peer view")
}

// remoteMemberCatalog has the test peer advertise one shared agent, plus two
// that share a name.
func remoteMemberCatalog(t *testing.T, fh *fedHarness) {
	t.Helper()
	inst := fh.peer.id.ID()
	before, _, _ := db.GetFederationCatalog(inst)
	fh.peer.send(fh.peer.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{PeerViewAgents: true, Groups: []proto.CatalogGroup{
		{Name: "lab", Members: []proto.CatalogMember{{Agent: "agt_remotetarget000000000000", Name: "target"}, {Agent: "agt_twinone00000000000000000", Name: "twin"}, {Agent: "agt_twintwo00000000000000000", Name: "twin"}}},
	}}))
	fedEventually(t, "catalog stored", func() bool {
		after, _, _ := db.GetFederationCatalog(inst)
		return after != before && after != ""
	})
}

// answerRemoteMemberVerb accepts the n-th outbound peer view, returns its
// open payload and request, and answers with reply.
func answerRemoteMemberVerb(t *testing.T, p *fedPeer, n int, reply string) (proto.PeerViewOpenPayload, peerViewWire) {
	t.Helper()
	conn := acceptOutboundPeerView(t, p, n)
	defer conn.Close()
	var open proto.PeerViewOpenPayload
	require.NoError(t, p.envelopes(proto.KindPeerViewOpen)[n].DecodePayload(&open))
	wire := readPeerViewWire(t, conn)
	writePeerViewWire(t, conn, peerViewWire{Status: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(reply)})
	return open, wire
}

// The operator's member@peer verbs become the peer's dashboard routes over
// the operator's own peer view; local-only options never cross.
func TestRemoteMemberVerbsAsOperator(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	remoteMemberCatalog(t, fh)
	run := func(method, path string, body any) chan *httptest.ResponseRecorder {
		ch := make(chan *httptest.ResponseRecorder, 1)
		go func() { ch <- fedHuman(t, f, method, path, body) }()
		return ch
	}

	ch := run("POST", "/v1/agent/target@bob/stop?force=1", nil)
	open, wire := answerRemoteMemberVerb(t, p, 0, "{\"conv_id\":\"remote-conv\",\"action\":\"stopped\",\"detail\":\"\\u001b[31mred\\u001b[0m\"}")
	rec := <-ch
	require.Nil(t, open.Agent, "the operator's view carries no agent principal")
	require.Equal(t, "POST", wire.Method)
	require.Equal(t, "/api/agents/agt_remotetarget000000000000/stop", wire.URI)
	require.JSONEq(t, `{"force":true}`, string(wire.Body))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var reply map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &reply))
	require.Equal(t, "stopped", reply["action"])
	require.NotContains(t, reply["detail"], "\x1b", "control characters are stripped from remote replies")
	require.Equal(t, true, reply["remote"].(map[string]any)["untrusted"])

	ch = run("POST", "/v1/agent/agt_remotetarget000000000000@bob/retire?shutdown=1&delete_worktree=1", nil)
	_, wire = answerRemoteMemberVerb(t, p, 1, `{"conv_id":"remote-conv"}`)
	rec = <-ch
	require.Equal(t, "/api/agents/agt_remotetarget000000000000/retire?shutdown=1", wire.URI)
	require.Contains(t, rec.Body.String(), "remote_note", "the kept worktree is said")

	ch = run("POST", "/v1/agent/target@bob/rename", map[string]any{"title": "renamed"})
	_, wire = answerRemoteMemberVerb(t, p, 2, `{"title":"renamed"}`)
	<-ch
	require.Equal(t, "/api/agents/agt_remotetarget000000000000/rename", wire.URI)
	require.JSONEq(t, `{"title":"renamed"}`, string(wire.Body))

	ch = run("POST", "/v1/agent/target@bob/task", map[string]any{"url": "https://example.com/t/1", "label": "x"})
	_, wire = answerRemoteMemberVerb(t, p, 3, `{}`)
	<-ch
	require.Equal(t, "/api/agents/agt_remotetarget000000000000/task", wire.URI)
	require.Contains(t, string(wire.Body), "example.com/t/1")

	// Refused locally, before any peer view opens.
	for _, c := range []struct {
		method, path string
		body         any
		code         int
	}{
		{"POST", "/v1/agent/target@bob/resume?recreate=1", nil, 400},
		{"POST", "/v1/agent/target@bob/rename", map[string]any{"auto": true}, 400},
		{"GET", "/v1/agent/target@bob/task", nil, 400},
		{"POST", "/v1/agent/twin@bob/stop", nil, 409},
		{"POST", "/v1/agent/nobody@bob/stop", nil, 404},
	} {
		rec := fedHuman(t, f, c.method, c.path, c.body)
		require.Equal(t, c.code, rec.Code, "%s %s: %s", c.method, c.path, rec.Body.String())
	}
	require.Len(t, p.envelopes(proto.KindPeerViewOpen), 4)
}

// An agent's member@peer verb is an agent peer view under its peer= grants.
func TestRemoteMemberVerbsAsAgent(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	agentd.ResetAgentPeerViewLimitsForTest()
	t.Cleanup(agentd.ResetAgentPeerViewLimitsForTest)
	remoteMemberCatalog(t, fh)
	const caller = "019fe740-43a4-7023-b8ae-0000000a6e11"
	f.HaveEnrolledAgent(caller)
	call := func() chan *httptest.ResponseRecorder {
		ch := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			ch <- testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, "POST", "/v1/agent/target@bob/stop", nil), caller))
		}()
		return ch
	}
	rec := <-call()
	require.Equal(t, 403, rec.Code, "no peer= grant: %s", rec.Body.String())
	require.Empty(t, p.envelopes(proto.KindPeerViewOpen))
	require.NoError(t, db.GrantAgentPermissionWithScope(caller, agentd.PermAgentStop, `{"peer":["`+p.id.ID()+`/lab"]}`, "test"))
	ch := call()
	open, wire := answerRemoteMemberVerb(t, p, 0, `{"action":"stopped"}`)
	rec = <-ch
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.NotNil(t, open.Agent)
	require.Equal(t, []string{"lab"}, open.Agent.Groups)
	require.Equal(t, "/api/agents/agt_remotetarget000000000000/stop", wire.URI)
	require.Equal(t, "untrusted", rec.Header().Get("X-Tclaude-Remote-Content"))
}

// fleet.read is an instance peer grant: the peer reads the fleet brief, and
// a peer's agent reads it only with the grant too; its view metadata names
// the agent principal and only what agent views reach.
func TestPeerFleetReadAndAgentMetadata(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	inst := p.id.ID()
	f.HaveGroup("crew")
	f.HaveAliveSession(moveSourceConv, "fleet-source", "fleet-pane", testutil.CanonicalTempDir(t))
	f.HaveConvWithTitle(moveSourceConv, "fleet-worker")
	f.HaveMember("crew", moveSourceConv)
	h := agentd.PeerViewHandler(inst)
	rec := testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/fleet", nil))
	require.Equal(t, 403, rec.Code, rec.Body.String())
	rec = fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermFleetRead})
	require.Equal(t, 200, rec.Code, "fleet.read is a grantable instance peer grant: %s", rec.Body.String())
	rec = fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermFleetRead, "scope": "group=crew"})
	require.Equal(t, 400, rec.Code, "an instance grant refuses a scope: %s", rec.Body.String())
	rec = testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/fleet", nil))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "fleet-worker")

	worker := &proto.PeerViewAgent{ID: "agt_bobworker000000000000000", Name: "bob-worker"}
	reply := agentPeerViewExchange(t, p, worker, "GET", "/api/fleet", nil)
	require.Equal(t, 200, reply.Status, string(reply.Body))
	require.Contains(t, string(reply.Body), "fleet-worker")

	peer, err := db.GetFederationPeer(inst)
	require.NoError(t, err)
	peer.TrustLevel = db.FederationTrustUnrestricted
	require.NoError(t, db.TrustFederationPeer(*peer))
	reply = agentPeerViewExchange(t, p, worker, "GET", "/api/groups", nil)
	require.Equal(t, 200, reply.Status, string(reply.Body))
	var body struct {
		PeerView struct {
			Level     string   `json:"level"`
			Principal string   `json:"principal"`
			Included  []string `json:"included"`
			Omitted   []struct {
				Feature string `json:"feature"`
			} `json:"omitted"`
		} `json:"peer_view"`
	}
	require.NoError(t, json.Unmarshal(reply.Body, &body))
	require.Equal(t, "agent", body.PeerView.Principal)
	require.Equal(t, "unrestricted", body.PeerView.Level, "the level is the peer's real trust; the principal bounds it")
	require.Contains(t, body.PeerView.Included, "fleet")
	for _, feature := range append(body.PeerView.Included, func() (out []string) {
		for _, o := range body.PeerView.Omitted {
			out = append(out, o.Feature)
		}
		return
	}()...) {
		require.NotContains(t, []string{"node.exec", "spawn", "spawn.request", "terminals", "local_dashboard", "lifecycle.move"}, feature, "an agent view does not advertise %s", feature)
	}
}

// An agent that holds fleet.read for this node keeps it, and takes the peer
// reach through one of its groups.
func TestPeerFleetReachThroughGroupKeepsLocalGrant(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	agentd.ResetAgentPeerViewLimitsForTest()
	t.Cleanup(agentd.ResetAgentPeerViewLimitsForTest)
	remoteMemberCatalog(t, fh)
	const caller = "019fe740-43a4-7023-b8ae-0000000a6e12"
	f.HaveGroup("steward")
	f.HaveConvWithTitle(caller, "steward")
	f.HaveEnrolledAgent(caller)
	f.HaveMember("steward", caller)
	require.NoError(t, db.SetAgentPermissionOverride(caller, agentd.PermFleetRead, db.PermEffectGrant, "test"))
	rec := fedHuman(t, f, "POST", "/v1/permissions/peer-reach", map[string]any{"group": "steward", "slugs": []string{agentd.PermFleetRead}, "peer": "bob"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	local := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, "GET", "/v1/fleet", nil), caller))
	require.Equal(t, 200, local.Code, "the local grant still reads this node's fleet: %s", local.Body.String())
	ch := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		ch <- testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, "GET", "/v1/federation/peer/bob/fleet", nil), caller))
	}()
	open, wire := answerRemoteMemberVerb(t, p, 0, `{"agents":[]}`)
	rec = <-ch
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.NotNil(t, open.Agent)
	require.Equal(t, "/api/fleet", wire.URI)
}

// An agent view's metadata lists the grants of its footprint groups only; a
// node-wide route lists none.
func TestPeerViewAgentMetadataFootprint(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	f.HaveGroup("crew")
	f.HaveGroup("other")
	for _, g := range []string{"crew", "other"} {
		rec := fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermGroupsMembersStop, "scope": "group=" + g})
		require.Equal(t, 200, rec.Code, rec.Body.String())
	}
	groupsOf := func(worker *proto.PeerViewAgent) map[string]any {
		t.Helper()
		reply := agentPeerViewExchange(t, p, worker, "GET", "/api/groups", nil)
		require.Equal(t, 200, reply.Status, string(reply.Body))
		var body struct {
			PeerView struct {
				Groups map[string]any `json:"groups"`
			} `json:"peer_view"`
		}
		require.NoError(t, json.Unmarshal(reply.Body, &body))
		return body.PeerView.Groups
	}
	got := groupsOf(&proto.PeerViewAgent{ID: "agt_bobworker000000000000000", Name: "bob-worker", Groups: []string{"crew"}})
	require.Contains(t, got, "crew")
	require.NotContains(t, got, "other", "a group outside the agent's footprint is not listed")
	require.Empty(t, groupsOf(&proto.PeerViewAgent{ID: "agt_bobworker000000000000000", Name: "bob-worker"}))
}
