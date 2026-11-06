package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/federation/stream"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// peerViewAudit returns the receiver's peer view audit rows.
func peerViewAudit(t *testing.T) []db.AuditLogEntry {
	t.Helper()
	rows, err := db.ListAuditLog(db.AuditLogFilter{Verb: "federation.peer_view"})
	require.NoError(t, err)
	return rows
}

// Pins: a peer view opened by the remote operator (no agent principal)
// keeps its behaviour. A restricted peer acts under its grants and is
// audited as operator@<inst>; an unrestricted peer runs the as-local paths.
func TestPeerViewOperatorPrincipalPins(t *testing.T) {
	fh := newFedHarness(t)
	f := fh.f
	f.HaveGroup("crew")
	f.HaveAliveSession(moveSourceConv, "pin-source", "pin-pane", testutil.CanonicalTempDir(t))
	f.HaveMember("crew", moveSourceConv)
	aid, err := db.AgentIDForConv(moveSourceConv)
	require.NoError(t, err)
	inst := fh.peer.id.ID()
	h := agentd.PeerViewHandler(inst)
	serve := func(method, path string, body any) int {
		return testharness.Serve(h, testharness.JSONRequest(t, method, path, body)).Code
	}
	for _, slug := range []string{agentd.PermGroupsRosterRead, agentd.PermGroupsMembersStop} {
		rec := fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug, "scope": "group=crew"})
		require.Equal(t, 200, rec.Code, rec.Body.String())
	}
	require.Equal(t, 200, serve("POST", "/api/agents/"+aid+"/stop", nil))
	rows := peerViewAudit(t)
	require.NotEmpty(t, rows)
	require.Equal(t, "remote:operator@"+inst, rows[0].ActorLabel)
	require.Contains(t, rows[0].Detail, "POST /api/agents/{id}/stop")
	require.Equal(t, 403, serve("GET", "/api/resilience/policies", nil), "a restricted peer never reaches the local dashboard")
	require.Equal(t, 403, serve("POST", "/api/agents/"+aid+"/tags", map[string]any{"tags": []string{"remote"}}), "no manage grant")

	peer, err := db.GetFederationPeer(inst)
	require.NoError(t, err)
	peer.TrustLevel = db.FederationTrustUnrestricted
	require.NoError(t, db.TrustFederationPeer(*peer))
	require.Equal(t, 200, serve("GET", "/api/resilience/policies", nil), "an unrestricted operator runs the local dashboard route")
	require.Equal(t, http.StatusOK, serve("POST", "/api/agents/"+aid+"/tags", map[string]any{"tags": []string{"remote"}}), "an unrestricted operator needs no grant")
}

// openAgentPeerView opens an inbound peer view whose open envelope names an
// agent principal of the test peer, and returns the answer.
func openAgentPeerView(t *testing.T, p *fedPeer, principal *proto.PeerViewAgent) (*stream.Conn, proto.PeerViewAnswerPayload) {
	t.Helper()
	kp, err := stream.NewKeyPair()
	require.NoError(t, err)
	sid := proto.NewEnvelopeID()
	env := p.envelope(proto.KindPeerViewOpen, proto.Endpoint{}, proto.PeerViewOpenPayload{Stream: sid, Key: kp.Pub, Agent: principal})
	p.send(env)
	var answer proto.PeerViewAnswerPayload
	fedEventually(t, "peer view answer", func() bool {
		for _, a := range p.envelopes(proto.KindPeerViewAnswer) {
			if a.InReplyTo == env.ID {
				require.NoError(t, a.DecodePayload(&answer))
				return true
			}
		}
		return false
	})
	if !answer.OK {
		return nil, answer
	}
	return fedPeerStream(t, p, sid, kp, answer.Key, true), answer
}

// agentPeerViewExchange runs one request through the real tunnel under an
// agent principal.
func agentPeerViewExchange(t *testing.T, p *fedPeer, principal *proto.PeerViewAgent, method, uri string, body any) peerViewWire {
	t.Helper()
	c, answer := openAgentPeerView(t, p, principal)
	require.True(t, answer.OK, answer.Reason)
	defer c.Close()
	var raw []byte
	if body != nil {
		raw = mustJSON(t, body)
	}
	writePeerViewWire(t, c, peerViewWire{Method: method, URI: uri, Header: http.Header{"Content-Type": {"application/json"}}, Body: raw})
	return readPeerViewWire(t, c)
}

// The receiving node serves a peer's agent under the peer's grants, never
// as its own operator, even when it trusts the peer unrestricted.
func TestPeerViewAgentPrincipalReceiver(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	f.HaveGroup("crew")
	f.HaveAliveSession(moveSourceConv, "agent-view-source", "agent-view-pane", testutil.CanonicalTempDir(t))
	f.HaveMember("crew", moveSourceConv)
	aid, err := db.AgentIDForConv(moveSourceConv)
	require.NoError(t, err)
	inst := p.id.ID()
	worker := &proto.PeerViewAgent{ID: "agt_bobworker000000000000000", Name: "bob-worker", Groups: []string{"crew"}}
	for _, slug := range []string{agentd.PermGroupsRosterRead, agentd.PermGroupsMembersStop} {
		rec := fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug, "scope": "group=crew"})
		require.Equal(t, 200, rec.Code, rec.Body.String())
	}

	// A malformed principal is refused at the open, never served as the operator.
	_, answer := openAgentPeerView(t, p, &proto.PeerViewAgent{ID: "agt_bobworker000000000000000", Name: "bob;rm -rf"})
	require.False(t, answer.OK)
	require.Equal(t, "invalid agent principal", answer.Reason)
	_, answer = openAgentPeerView(t, p, &proto.PeerViewAgent{ID: "not-an-agent", Name: "bob-worker"})
	require.False(t, answer.OK)

	reply := agentPeerViewExchange(t, p, worker, "GET", "/api/snapshot", nil)
	require.Equal(t, 200, reply.Status, string(reply.Body))
	reply = agentPeerViewExchange(t, p, worker, "POST", "/api/agents/"+aid+"/stop", nil)
	require.Equal(t, 200, reply.Status, string(reply.Body))
	rows := peerViewAudit(t)
	require.NotEmpty(t, rows)
	require.Equal(t, "remote:agent:bob-worker/"+worker.ID+"@"+inst, rows[0].ActorLabel, "the receiver audits the agent principal")
	// The receiver refuses a request outside the footprint the sender checked.
	narrow := &proto.PeerViewAgent{ID: worker.ID, Name: worker.Name, Groups: []string{"elsewhere"}}
	reply = agentPeerViewExchange(t, p, narrow, "POST", "/api/agents/"+aid+"/stop", nil)
	require.Equal(t, 403, reply.Status, string(reply.Body))
	require.Contains(t, string(reply.Body), "agent_footprint")
	reply = agentPeerViewExchange(t, p, narrow, "PATCH", "/api/groups/crew", map[string]any{"descr": "x"})
	require.Contains(t, string(reply.Body), "agent_footprint")
	reply = agentPeerViewExchange(t, p, worker, "POST", "/api/agents/"+aid+"/tags", map[string]any{"tags": []string{"x"}})
	require.Equal(t, 403, reply.Status, "the peer's grants still decide: no manage grant")
	reply = agentPeerViewExchange(t, p, worker, "POST", "/api/groups/crew/spawn", map[string]any{"brief": "x"})
	require.Equal(t, 403, reply.Status, "spawn is not an agent peer view route")
	require.Contains(t, string(reply.Body), "agent_principal")

	// Unrestricted trust: the operator acts as this node's operator, the
	// agent only within the grants that trust holds, evaluated restricted-style.
	peer, err := db.GetFederationPeer(inst)
	require.NoError(t, err)
	peer.TrustLevel = db.FederationTrustUnrestricted
	require.NoError(t, db.TrustFederationPeer(*peer))
	for _, uri := range []string{"/api/resilience/policies", "/api/pr-checks", "/api/peer-view-jobs/x"} {
		reply = agentPeerViewExchange(t, p, worker, "GET", uri, nil)
		require.Equal(t, 403, reply.Status, "%s: an agent never reaches an operator-only route", uri)
		require.Contains(t, string(reply.Body), "agent_principal")
	}
	reply = agentPeerViewExchange(t, p, worker, "POST", "/api/agents/"+aid+"/retire?shutdown=0&delete_worktree=1", nil)
	require.Equal(t, 400, reply.Status, "an agent takes the restricted retire path, which refuses worktree deletion: %s", reply.Body)
	reply = agentPeerViewExchange(t, p, worker, "POST", "/api/agents/"+aid+"/tags", map[string]any{"tags": []string{"x"}})
	require.Equal(t, 200, reply.Status, "an unrestricted peer holds every grantable peer grant: %s", reply.Body)
	f.HaveGroup("other")
	reply = agentPeerViewExchange(t, p, worker, "POST", "/api/agents/"+aid+"/clone", map[string]any{"group": "other"})
	require.Equal(t, 403, reply.Status, "a clone's destination is part of the footprint: %s", reply.Body)
	require.Contains(t, string(reply.Body), "agent_footprint")
	// Settings that shape the group's future members stay the operator's.
	for _, field := range []string{"default_context", "default_prompts", "environment", "permissions", "default_profile"} {
		reply = agentPeerViewExchange(t, p, worker, "PATCH", "/api/groups/crew", map[string]any{field: nil, "descr": "x"})
		require.Equal(t, 403, reply.Status, "%s: %s", field, reply.Body)
		require.Contains(t, string(reply.Body), "agent_field_not_allowed")
	}
	reply = agentPeerViewExchange(t, p, worker, "PATCH", "/api/groups/crew", map[string]any{"descr": "set by a remote agent"})
	require.Equal(t, 200, reply.Status, string(reply.Body))
	g, err := db.GetAgentGroupByName("crew")
	require.NoError(t, err)
	require.Equal(t, "set by a remote agent", g.Descr)

	// The operator turns the peer's agent views off without touching its grants.
	rec := fedHuman(t, f, "POST", "/v1/federation/peers/agent-views", map[string]any{"instance": "bob", "policy": "deny"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	reply = agentPeerViewExchange(t, p, worker, "GET", "/api/snapshot", nil)
	require.Equal(t, 403, reply.Status)
	require.Contains(t, string(reply.Body), "agent_views_off")
	c := openInboundPeerView(t, p)
	writePeerViewWire(t, c, peerViewWire{Method: "GET", URI: "/api/snapshot"})
	require.Equal(t, 200, readPeerViewWire(t, c).Status, "the operator's view is unaffected")
	_ = c.Close()
	rec = fedHuman(t, f, "GET", "/v1/federation/peers/agent-views?peer=bob", nil)
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), `"policy":"deny"`)
	rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, "POST", "/v1/federation/peers/agent-views", map[string]any{"instance": "bob", "policy": "allow"}), moveSourceConv))
	require.Equal(t, 403, rec.Code, "only the operator sets the policy")
}

// The sending node's gate: the agent needs the route's slug held
// peer=<inst>/<group> over the route's whole footprint, and the peer must
// know agent principals.
func TestPeerViewAgentSender(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	agentd.ResetAgentPeerViewLimitsForTest()
	t.Cleanup(agentd.ResetAgentPeerViewLimitsForTest)
	inst := p.id.ID()
	const caller = "019fe740-43a4-7023-b8ae-0000000a6e01"
	f.HaveEnrolledAgent(caller)
	callerAgent, err := db.AgentIDForConv(caller)
	require.NoError(t, err)
	remote := "agt_remotetarget000000000000"
	catalog := func(agentViews bool) {
		t.Helper()
		before, _, _ := db.GetFederationCatalog(inst)
		p.send(p.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{PeerViewAgents: agentViews, Groups: []proto.CatalogGroup{
			{Name: "lab", Members: []proto.CatalogMember{{Agent: remote, Name: "target"}}},
			{Name: "ops", Members: []proto.CatalogMember{{Agent: remote, Name: "target"}}},
			{Name: "ci"},
		}}))
		fedEventually(t, "catalog stored", func() bool {
			after, _, _ := db.GetFederationCatalog(inst)
			return after != before && after != ""
		})
	}
	catalog(true)
	call := func(method, tail string, body any) *httptest.ResponseRecorder {
		t.Helper()
		return testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, method, "/v1/federation/peer/bob/"+tail, body), caller))
	}
	grant := func(slug, scope string) {
		t.Helper()
		require.NoError(t, db.GrantAgentPermissionWithScope(caller, slug, `{"peer":["`+scope+`"]}`, "test"))
	}

	rec := call("POST", "agents/"+remote+"/stop", nil)
	require.Equal(t, 403, rec.Code, "no grant: %s", rec.Body.String())
	// Only an explicit peer= grant counts, even toward a peer this node
	// trusts unrestricted, where an unscoped grant would otherwise bind.
	require.NoError(t, db.SetAgentPermissionOverride(caller, agentd.PermAgentStop, db.PermEffectGrant, "test"))
	sender, err := db.GetFederationPeer(inst)
	require.NoError(t, err)
	sender.TrustLevel = db.FederationTrustUnrestricted
	require.NoError(t, db.TrustFederationPeer(*sender))
	rec = call("POST", "agents/"+remote+"/stop", nil)
	require.Equal(t, 403, rec.Code, "an unscoped grant never opens an agent peer view: %s", rec.Body.String())
	grant(agentd.PermAgentStop, inst+"/lab")
	rec = call("POST", "agents/"+remote+"/stop", nil)
	require.Equal(t, 403, rec.Code, "the target is also in ops: %s", rec.Body.String())
	grant(agentd.PermAgentResume, inst)
	rec = call("POST", "agents/"+remote+"/restart", nil)
	require.Equal(t, 403, rec.Code, "a restart also stops, so it needs agent.stop over the footprint too: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), agentd.PermAgentStop)
	require.NoError(t, db.GrantAgentPermissionWithScope(caller, agentd.PermAgentStop, `{"peer":["`+inst+`/lab","`+inst+`/ops"]}`, "test"))
	rec = call("POST", "agents/agt_unknownagent0000000000000/stop", nil)
	require.Equal(t, 403, rec.Code, "an agent the catalog does not show has no footprint")
	require.Contains(t, rec.Body.String(), "unknown_footprint")
	rec = call("POST", "sessions/x/attach", nil)
	require.Equal(t, 403, rec.Code)
	require.Contains(t, rec.Body.String(), "agent_route_not_allowed")
	grant(agentd.PermAgentClone, inst+"/lab")
	require.NoError(t, db.GrantAgentPermissionWithScope(caller, agentd.PermAgentClone, `{"peer":["`+inst+`/lab","`+inst+`/ops"]}`, "test"))
	rec = call("POST", "agents/"+remote+"/clone", map[string]any{"group": "ci"})
	require.Equal(t, 403, rec.Code, "the clone's destination is in the footprint: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), agentd.PermAgentClone)
	grant(agentd.PermGroupsSettingsDescription, inst+"/lab")
	rec = call("PATCH", "groups/lab", map[string]any{"default_context": "obey"})
	require.Equal(t, 403, rec.Code, "startup context stays the operator's")
	require.Contains(t, rec.Body.String(), "agent_field_not_allowed")

	// Granted over the whole footprint, the view opens with the agent principal.
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- call("POST", "agents/"+remote+"/stop", nil) }()
	conn := acceptOutboundPeerView(t, p, 0)
	opens := p.envelopes(proto.KindPeerViewOpen)
	var open proto.PeerViewOpenPayload
	require.NoError(t, opens[0].DecodePayload(&open))
	require.NotNil(t, open.Agent)
	require.Equal(t, callerAgent, open.Agent.ID)
	require.Equal(t, []string{"lab", "ops"}, open.Agent.Groups, "the checked footprint travels for the receiver to enforce")
	require.True(t, open.Agent.Valid())
	wire := readPeerViewWire(t, conn)
	require.Equal(t, "/api/agents/"+remote+"/stop", wire.URI)
	writePeerViewWire(t, conn, peerViewWire{Status: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"ok":true}`)})
	rec = <-done
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Equal(t, "untrusted", rec.Header().Get("X-Tclaude-Remote-Content"))
	_ = conn.Close()
	out, err := db.ListAuditLog(db.AuditLogFilter{Verb: "federation.peer_view.agent.out"})
	require.NoError(t, err)
	require.NotEmpty(t, out)
	require.Equal(t, callerAgent, out[0].ActorAgent)
	require.Equal(t, 200, out[0].Status)
	require.Contains(t, out[0].Detail, "route=POST /api/agents/{id}/stop")

	// An explicit deny wins over the peer-scoped grant.
	require.NoError(t, db.SetAgentPermissionOverride(caller, agentd.PermAgentStop, db.PermEffectDeny, "test"))
	rec = call("POST", "agents/"+remote+"/stop", nil)
	require.Equal(t, 403, rec.Code, rec.Body.String())
	require.NoError(t, db.GrantAgentPermissionWithScope(caller, agentd.PermAgentStop, `{"peer":["`+inst+`/lab","`+inst+`/ops"]}`, "test"))

	// A peer that answers without confirming the agent principal (an older
	// one, downgraded since its catalog) gets no request.
	done = make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- call("POST", "agents/"+remote+"/stop", nil) }()
	var env *proto.Envelope
	fedEventually(t, "second open", func() bool {
		es := p.envelopes(proto.KindPeerViewOpen)
		if len(es) < 2 {
			return false
		}
		env = es[1]
		return true
	})
	var second proto.PeerViewOpenPayload
	require.NoError(t, env.DecodePayload(&second))
	kp, err := stream.NewKeyPair()
	require.NoError(t, err)
	legacy := p.envelope(proto.KindPeerViewAnswer, proto.Endpoint{}, proto.PeerViewAnswerPayload{Stream: second.Stream, OK: true, Key: kp.Pub})
	legacy.InReplyTo = env.ID
	p.send(legacy)
	rec = <-done
	require.Equal(t, 409, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "peer_lacks_agent_views")

	// A peer that does not advertise agent views never sees an agent principal.
	catalog(false)
	before := len(p.envelopes(proto.KindPeerViewOpen))
	rec = call("POST", "agents/"+remote+"/stop", nil)
	require.Equal(t, 409, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "peer_lacks_agent_views")
	require.Len(t, p.envelopes(proto.KindPeerViewOpen), before)
}

// Every attempt counts toward an agent's peer view limit, refused ones too,
// so a prompt-injected agent cannot probe what it holds.
func TestPeerViewAgentRateLimitCountsRefusals(t *testing.T) {
	fh := newFedHarness(t)
	f := fh.f
	agentd.ResetAgentPeerViewLimitsForTest()
	t.Cleanup(agentd.ResetAgentPeerViewLimitsForTest)
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	t.Cleanup(agentd.SetAgentPeerViewClockForTest(func() time.Time { return now }))
	const caller = "019fe740-43a4-7023-b8ae-0000000a6e02"
	f.HaveEnrolledAgent(caller)
	call := func() int {
		return testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, "POST", "/v1/federation/peer/bob/agents/agt_x0000000000000000000000000/stop", nil), caller)).Code
	}
	for i := 0; i < 60; i++ {
		require.Equal(t, 403, call(), "attempt %d is refused (no catalog footprint)", i)
	}
	require.Equal(t, http.StatusTooManyRequests, call(), "refused attempts count")
	now = now.Add(time.Minute)
	require.Equal(t, 403, call(), "the window moves on")
}

// A peer-scoped grant of a session-ending slug warns.
func TestPeerViewAgentDestructiveGrantWarns(t *testing.T) {
	fh := newFedHarness(t)
	const caller = "019fe740-43a4-7023-b8ae-0000000a6e03"
	fh.f.HaveConvWithTitle(caller, "ops-lead")
	fh.f.HaveEnrolledAgent(caller)
	for slug, warns := range map[string]bool{agentd.PermAgentStop: true, agentd.PermAgentRetire: true, agentd.PermAgentResume: true, agentd.PermAgentRename: false} {
		rec := fedHuman(t, fh.f, "POST", "/v1/permissions/peer-reach", map[string]any{"target": caller, "slugs": []string{slug}, "peer": "bob/lab"})
		require.Equal(t, 200, rec.Code, rec.Body.String())
		if warns {
			require.Contains(t, rec.Body.String(), "end sessions on bob/lab", slug)
		} else {
			require.NotContains(t, rec.Body.String(), "end sessions", slug)
		}
	}
}
