package agentd_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/routebroker"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/federation/stream"
)

func skipFedRoutes(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "darwin" {
		t.Skip("Darwin route authority requires an enrolled production launch contract")
	}
}

// fedRouteAnswerFor waits for the daemon's answer to one route_open.
func fedRouteAnswerFor(t *testing.T, p *fedPeer, sid string) proto.RouteAnswerPayload {
	t.Helper()
	var out proto.RouteAnswerPayload
	fedEventually(t, "route answer", func() bool {
		for _, e := range p.envelopes(proto.KindRouteAnswer) {
			var a proto.RouteAnswerPayload
			if e.DecodePayload(&a) == nil && a.Stream == sid {
				out = a
				return true
			}
		}
		return false
	})
	return out
}

// fedPeerStream joins the hub stream sid from the scripted peer's side.
func fedPeerStream(t *testing.T, p *fedPeer, sid string, kp *stream.KeyPair, daemonKey []byte, initiator bool) *stream.Conn {
	t.Helper()
	keys, err := stream.DeriveKeys(kp, daemonKey, sid, initiator)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ws, err := p.cl.DialStream(ctx, sid, p.agentdID)
	require.NoError(t, err)
	c, err := stream.New(ws, keys)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func readBrokerFrameWithin(t *testing.T, conn net.Conn, d time.Duration) routebroker.Frame {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(d))
	frame, err := routebroker.ReadFrame(conn, routebroker.MaxFramePayload)
	require.NoError(t, err)
	return frame
}

// TestFederation_RoutesServeRemoteConsumer: a peer opens a route that this
// instance exports, and its bytes reach the local publisher helper.
func TestFederation_RoutesServeRemoteConsumer(t *testing.T) {
	skipFedRoutes(t)
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer

	const pub = "fedr1-pub-bbbb-cccc-000000000001"
	f.HaveGroup("svc")
	f.HaveGroup("internal")
	f.HaveConvWithTitle(pub, "api-server")
	f.HaveMember("svc", pub)
	f.HaveMember("internal", pub)
	f.HaveAliveSession(pub, "spwn-fedr1", "tclaude-spwn-fedr1", f.TestCwd("pub"))
	for _, name := range []string{"svc", "internal"} {
		g, err := db.GetAgentGroupByName(name)
		require.NoError(t, err)
		require.NoError(t, db.ReplaceAgentGroupPermissions(g.ID, []string{agentd.PermRoutesPublish}, "test"))
	}

	rec, route := serveRouteAgent(t, f, http.MethodPost, "/v1/routes/publish", pub, map[string]any{
		"group": "svc", "name": "api", "target": "tcp://127.0.0.1:43181",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	routeID := route["id"].(string)
	rec, hidden := serveRouteAgent(t, f, http.MethodPost, "/v1/routes/publish", pub, map[string]any{
		"group": "internal", "name": "db", "target": "tcp://127.0.0.1:43182",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// The publisher's helper, attached to the daemon's broker.
	helper, brokerSide := net.Pipe()
	t.Cleanup(func() { _ = helper.Close() })
	go func() {
		_ = agentd.GroupRouteBroker().AttachPublisher(context.Background(), routebroker.PublisherAuth{
			RouteID: routeID, AgentID: route["publisher_agent_id"].(string), ConvID: route["publisher_conv_id"].(string),
			LaunchGeneration: route["publisher_launch_generation"].(string), GroupGeneration: int64(route["group_generation"].(float64)),
		}, brokerSide)
	}()

	// Exporting with routes lists the ready route in the catalog.
	rec = fedGrantCaps(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"group": "svc", "peer": "bob", "caps": []string{"roster", "routes"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	fedEventually(t, "catalog with the route", func() bool {
		cats := p.envelopes(proto.KindCatalog)
		if len(cats) == 0 {
			return false
		}
		var cat proto.CatalogPayload
		require.NoError(t, cats[len(cats)-1].DecodePayload(&cat))
		return len(cat.Groups) == 1 && len(cat.Groups[0].Routes) == 1 &&
			cat.Groups[0].Routes[0] == proto.CatalogRoute{ID: routeID, Publisher: "api-server", Name: "api"}
	})

	// A route in an unexported group is refused, indistinguishable from none.
	kp, err := stream.NewKeyPair()
	require.NoError(t, err)
	sid := proto.NewEnvelopeID()
	p.send(p.envelope(proto.KindRouteOpen, proto.Endpoint{}, proto.RouteOpenPayload{Route: hidden["id"].(string), Stream: sid, Key: kp.Pub}))
	ans := fedRouteAnswerFor(t, p, sid)
	require.False(t, ans.OK)
	require.Equal(t, "no such ready route", ans.Reason)

	// Open the exported route: the helper sees an ordinary broker stream.
	sid = proto.NewEnvelopeID()
	open := p.envelope(proto.KindRouteOpen, proto.Endpoint{}, proto.RouteOpenPayload{Route: routeID, Stream: sid, Key: kp.Pub})
	p.send(open)
	fr := readBrokerFrameWithin(t, helper, 10*time.Second)
	require.Equal(t, routebroker.KindOpen, fr.Kind)
	require.NoError(t, routebroker.WriteFrame(helper, routebroker.Frame{Kind: routebroker.KindOpenOK, Stream: fr.Stream}, 0))
	// Server-speaks-first: the target greets before the peer has joined.
	require.NoError(t, routebroker.WriteFrame(helper, routebroker.Frame{Kind: routebroker.KindData, Stream: fr.Stream, Payload: []byte("220 ready\r\n")}, 0))
	ans = fedRouteAnswerFor(t, p, sid)
	require.True(t, ans.OK, ans.Reason)
	conn := fedPeerStream(t, p, sid, kp, ans.Key, true)
	greeting := make([]byte, len("220 ready\r\n"))
	_, err = io.ReadFull(conn, greeting)
	require.NoError(t, err)
	require.Equal(t, "220 ready\r\n", string(greeting))

	// The proxy lease stays out of route listings.
	rec, leases := serveRouteAgent(t, f, http.MethodGet, "/v1/routes/leases?group=svc", pub, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Empty(t, leases["leases"])

	_, err = conn.Write([]byte("GET / HTTP/1.0\r\n\r\n"))
	require.NoError(t, err)
	fr = readBrokerFrameWithin(t, helper, 10*time.Second)
	require.Equal(t, routebroker.KindData, fr.Kind)
	require.Equal(t, "GET / HTTP/1.0\r\n\r\n", string(fr.Payload))
	require.NoError(t, conn.CloseWrite())
	require.Equal(t, routebroker.KindHalfClose, readBrokerFrameWithin(t, helper, 10*time.Second).Kind)

	stream := fr.Stream
	require.NoError(t, routebroker.WriteFrame(helper, routebroker.Frame{Kind: routebroker.KindData, Stream: stream, Payload: []byte("HTTP/1.0 200 OK\r\n\r\nhi")}, 0))
	require.NoError(t, routebroker.WriteFrame(helper, routebroker.Frame{Kind: routebroker.KindHalfClose, Stream: stream}, 0))
	got, err := io.ReadAll(conn)
	require.NoError(t, err)
	require.Equal(t, "HTTP/1.0 200 OK\r\n\r\nhi", string(got))
	// Both directions finished: the broker retires the stream.
	fr = readBrokerFrameWithin(t, helper, 10*time.Second)
	require.Equal(t, routebroker.KindClose, fr.Kind)
	require.Equal(t, stream, fr.Stream)

	// A replayed open is ignored rather than opening another stream.
	p.send(open)

	// A peer that aborts mid-stream resets the local connection; it is
	// never presented to the target as a finished request.
	sid = proto.NewEnvelopeID()
	p.send(p.envelope(proto.KindRouteOpen, proto.Endpoint{}, proto.RouteOpenPayload{Route: routeID, Stream: sid, Key: kp.Pub}))
	fr = readBrokerFrameWithin(t, helper, 10*time.Second)
	require.Equal(t, routebroker.KindOpen, fr.Kind, "the replayed open must not have opened a stream")
	require.NoError(t, routebroker.WriteFrame(helper, routebroker.Frame{Kind: routebroker.KindOpenOK, Stream: fr.Stream}, 0))
	ans = fedRouteAnswerFor(t, p, sid)
	require.True(t, ans.OK, ans.Reason)
	aborted := fedPeerStream(t, p, sid, kp, ans.Key, true)
	_, err = aborted.Write([]byte("POST /upload HTTP/1.0\r\nContent-Length: 999\r\n\r\npart"))
	require.NoError(t, err)
	require.Equal(t, routebroker.KindData, readBrokerFrameWithin(t, helper, 10*time.Second).Kind)
	require.NoError(t, aborted.Close())
	fr = readBrokerFrameWithin(t, helper, 10*time.Second)
	require.Equal(t, routebroker.KindClose, fr.Kind)

	// A target that half-closes and then aborts resets the peer's side: a
	// CLOSE after one half-close is not the broker's orderly end.
	sid = proto.NewEnvelopeID()
	p.send(p.envelope(proto.KindRouteOpen, proto.Endpoint{}, proto.RouteOpenPayload{Route: routeID, Stream: sid, Key: kp.Pub}))
	fr = readBrokerFrameWithin(t, helper, 10*time.Second)
	require.Equal(t, routebroker.KindOpen, fr.Kind)
	require.NoError(t, routebroker.WriteFrame(helper, routebroker.Frame{Kind: routebroker.KindOpenOK, Stream: fr.Stream}, 0))
	ans = fedRouteAnswerFor(t, p, sid)
	require.True(t, ans.OK, ans.Reason)
	halfThenAbort := fedPeerStream(t, p, sid, kp, ans.Key, true)
	require.NoError(t, routebroker.WriteFrame(helper, routebroker.Frame{Kind: routebroker.KindHalfClose, Stream: fr.Stream}, 0))
	_, err = io.ReadAll(halfThenAbort)
	require.NoError(t, err)
	require.NoError(t, routebroker.WriteFrame(helper, routebroker.Frame{Kind: routebroker.KindClose, Stream: fr.Stream}, 0))
	_, err = halfThenAbort.Write([]byte("more"))
	if err == nil {
		err = halfThenAbort.Drain()
	}
	require.Error(t, err, "the peer must see the abort")

	// Unexporting withdraws authority: the proxy lease closes.
	rec = fedGrantCaps(t, f, http.MethodDelete, "/v1/federation/grants", map[string]any{"group": "svc", "peer": "bob"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	fedEventually(t, "proxy lease closed", func() bool {
		var open int
		d, err := db.Open()
		require.NoError(t, err)
		require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM agent_route_leases l JOIN federation_route_proxies p ON p.lease_id=l.id WHERE l.state=?`, string(db.RouteLeaseOpen)).Scan(&open))
		return open == 0
	})
}

// TestFederation_RoutesOpenRemoteRoute: a local agent opens a route a peer
// exports, through a private mirror its own helper consumes.
func TestFederation_RoutesOpenRemoteRoute(t *testing.T) {
	testFederationRoutesOpenRemoteRoute(t, false)
}

func TestFederation_UnrestrictedRoutesOpenRemoteRoute(t *testing.T) {
	testFederationRoutesOpenRemoteRoute(t, true)
}

func testFederationRoutesOpenRemoteRoute(t *testing.T, unrestricted bool) {
	skipFedRoutes(t)
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer

	const alice = "fedr2-alice-bbbb-cccc-000000000001"
	const carol = "fedr2-carol-bbbb-cccc-000000000002"
	f.HaveGroup("team")
	for i, c := range []string{alice, carol} {
		f.HaveConvWithTitle(c, []string{"alice", "carol"}[i])
		f.HaveMember("team", c)
		f.HaveAliveSession(c, "spwn-fedr2-"+c[6:11], "tclaude-spwn-fedr2-"+c[6:11], f.TestCwd(c[6:11]))
	}
	g, err := db.GetAgentGroupByName("team")
	require.NoError(t, err)
	require.NoError(t, db.ReplaceAgentGroupPermissions(g.ID, []string{agentd.PermRoutesConsume}, "test"))

	const remoteRoute = "rte_00112233445566778899aabbccddeeff"
	p.send(p.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{Groups: []proto.CatalogGroup{{
		Name: "builders", Caps: []string{proto.CapRoutes},
		Routes: []proto.CatalogRoute{{ID: remoteRoute, Publisher: "srv", Name: "api"}},
	}}}))
	fedEventually(t, "remote catalog visible", func() bool {
		for _, r := range fedStatus(t, f).Remote {
			if r.Label == "bob" && len(r.Groups) == 1 {
				return true
			}
		}
		return false
	})
	openRemote := func(conv string) (*http.Response, map[string]any) {
		rec, body := serveRouteAgent(t, f, http.MethodPost, "/v1/federation/routes/open", conv, map[string]any{"group": "team", "peer": "bob", "route": "srv/api"})
		return rec.Result(), body
	}
	res, body := openRemote(alice)
	require.Equal(t, http.StatusForbidden, res.StatusCode, body)
	require.Equal(t, "route_permission", body["code"])

	if unrestricted {
		// The existing unscoped group grant becomes usable towards this peer.
		setFedTrustLevel(t, fh, "unrestricted")
	} else {
		require.NoError(t, db.GrantAgentPermissionWithScope(alice, agentd.PermRoutesConsume, `{"peer":["`+p.id.ID()+`/builders"]}`, "test"))
	}
	res, lease := openRemote(alice)
	require.Equal(t, http.StatusCreated, res.StatusCode, lease)
	mirrorID := lease["route_id"].(string)

	// The mirror is private: not listed, and nobody else can open it.
	rec, routes := serveRouteAgent(t, f, http.MethodGet, "/v1/routes?group=team", carol, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Empty(t, routes["routes"])
	rec, _ = serveRouteAgent(t, f, http.MethodPost, "/v1/routes/open", carol, map[string]any{"route_id": mirrorID, "group": "team"})
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	// Alice's helper consumes the mirror like any local route.
	helper, brokerSide := net.Pipe()
	t.Cleanup(func() { _ = helper.Close() })
	go func() {
		_ = agentd.GroupRouteBroker().AttachConsumer(context.Background(), routebroker.ConsumerAuth{
			LeaseID: lease["id"].(string), RouteID: mirrorID, AgentID: lease["consumer_agent_id"].(string), ConvID: lease["consumer_conv_id"].(string),
			LaunchGeneration: lease["consumer_launch_generation"].(string), GroupGeneration: int64(lease["group_generation"].(float64)),
		}, brokerSide)
	}()
	require.Eventually(t, func() bool {
		_ = routebroker.WriteFrame(helper, routebroker.Frame{Kind: routebroker.KindPing}, 0)
		_ = helper.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		fr, err := routebroker.ReadFrame(helper, 0)
		return err == nil && fr.Kind == routebroker.KindPong
	}, 5*time.Second, 50*time.Millisecond)
	require.NoError(t, routebroker.WriteFrame(helper, routebroker.Frame{Kind: routebroker.KindOpen, Stream: 1}, 0))

	// The daemon asks bob to open the real route; bob accepts.
	var open *proto.Envelope
	fedEventually(t, "route_open sent", func() bool {
		if es := p.envelopes(proto.KindRouteOpen); len(es) > 0 {
			open = es[0]
			return true
		}
		return false
	})
	var req proto.RouteOpenPayload
	require.NoError(t, open.DecodePayload(&req))
	require.Equal(t, remoteRoute, req.Route)
	kp, err := stream.NewKeyPair()
	require.NoError(t, err)
	p.send(p.envelope(proto.KindRouteAnswer, proto.Endpoint{}, proto.RouteAnswerPayload{Stream: req.Stream, OK: true, Key: kp.Pub}))
	conn := fedPeerStream(t, p, req.Stream, kp, req.Key, false)
	require.Equal(t, routebroker.KindOpenOK, readBrokerFrameWithin(t, helper, 30*time.Second).Kind)

	require.NoError(t, routebroker.WriteFrame(helper, routebroker.Frame{Kind: routebroker.KindData, Stream: 1, Payload: []byte("ping")}, 0))
	buf := make([]byte, 4)
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	require.Equal(t, "ping", string(buf))
	_, err = conn.Write([]byte("pong"))
	require.NoError(t, err)
	fr := readBrokerFrameWithin(t, helper, 10*time.Second)
	require.Equal(t, routebroker.KindData, fr.Kind)
	require.Equal(t, "pong", string(fr.Payload))

	// A refused open surfaces as an ordinary open error.
	require.NoError(t, routebroker.WriteFrame(helper, routebroker.Frame{Kind: routebroker.KindOpen, Stream: 2}, 0))
	fedEventually(t, "second route_open", func() bool { return len(p.envelopes(proto.KindRouteOpen)) == 2 })
	require.NoError(t, p.envelopes(proto.KindRouteOpen)[1].DecodePayload(&req))
	p.send(p.envelope(proto.KindRouteAnswer, proto.Endpoint{}, proto.RouteAnswerPayload{Stream: req.Stream, Reason: "busy"}))
	fr = readBrokerFrameWithin(t, helper, 10*time.Second)
	require.Equal(t, routebroker.KindOpenError, fr.Kind)
	require.Equal(t, uint64(2), fr.Stream)

	// Revoking the remote grant withdraws the mirror and closes alice's lease.
	if unrestricted {
		setFedTrustLevel(t, fh, "restricted")
	} else {
		_, err = db.RevokeAgentPermission(alice, agentd.PermRoutesConsume)
		require.NoError(t, err)
	}
	fedEventually(t, "mirror withdrawn", func() bool {
		r, _ := db.GetAgentRoute(mirrorID)
		return r == nil || r.State != db.RouteStateReady
	})

	// Regranting remote authority lets alice open the route again with a fresh mirror.
	if unrestricted {
		// The existing unscoped group grant becomes usable towards this peer.
		setFedTrustLevel(t, fh, "unrestricted")
	} else {
		require.NoError(t, db.GrantAgentPermissionWithScope(alice, agentd.PermRoutesConsume, `{"peer":["`+p.id.ID()+`/builders"]}`, "test"))
	}
	res, lease = openRemote(alice)
	require.Equal(t, http.StatusCreated, res.StatusCode, lease)
	require.NotEqual(t, mirrorID, lease["route_id"])
	// Opening again while the mirror is live reuses it.
	res, again := openRemote(alice)
	require.Equal(t, http.StatusCreated, res.StatusCode, again)
	require.Equal(t, lease["route_id"], again["route_id"])
}

func TestFederation_PeerGrantPreservesLocalRouteAuthority(t *testing.T) {
	skipFedRoutes(t)
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	const publisher = "fed-local-publisher"
	const consumer = "fed-local-consumer"
	f.HaveGroup("team")
	f.HaveConvWithTitle(publisher, "publisher")
	f.HaveConvWithTitle(consumer, "consumer")
	f.HaveMember("team", publisher)
	f.HaveMember("team", consumer)
	group, err := db.GetAgentGroupByName("team")
	require.NoError(t, err)
	require.NoError(t, db.ReplaceAgentGroupPermissions(group.ID, []string{agentd.PermRoutesPublish, agentd.PermRoutesConsume}, "test"))
	rec, route := serveRouteAgent(t, f, http.MethodPost, "/v1/routes/publish", publisher, map[string]any{"group": "team", "name": "api", "target": "tcp://127.0.0.1:43177"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	grant := postPermissionScope(t, f, "grant", map[string]any{"target": consumer, "slug": agentd.PermRoutesConsume, "scope": map[string]any{"peer": []string{"bob/builders"}}})
	require.Equal(t, http.StatusOK, grant.Code, grant.Body)
	openLocal := func() *httptest.ResponseRecorder {
		rec, _ := serveRouteAgent(t, f, http.MethodPost, "/v1/routes/open", consumer, map[string]any{"group": "team", "route_id": route["id"]})
		return rec
	}
	// Adding peer reach leaves the inherited local group grant effective.
	rec = openLocal()
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	// A peer grant alone must never authorize a local route.
	require.NoError(t, db.ReplaceAgentGroupPermissions(group.ID, []string{agentd.PermRoutesPublish}, "test"))
	rec = openLocal()
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.NoError(t, db.ReplaceAgentGroupPermissions(group.ID, []string{agentd.PermRoutesPublish, agentd.PermRoutesConsume}, "test"))
	require.NoError(t, db.SetAgentPermissionOverride(consumer, agentd.PermRoutesConsume, db.PermEffectDeny, "test"))
	rec = openLocal()
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	cat := proto.CatalogPayload{Groups: []proto.CatalogGroup{{Name: "builders", Caps: []string{proto.CapRoutes}, Routes: []proto.CatalogRoute{{ID: "rte_00112233445566778899aabbccddeeff", Publisher: "srv", Name: "api"}}}}}
	raw, err := json.Marshal(cat)
	require.NoError(t, err)
	require.NoError(t, db.PutFederationCatalog(p.id.ID(), string(raw), time.Now()))
	rec, _ = serveRouteAgent(t, f, http.MethodPost, "/v1/federation/routes/open", consumer, map[string]any{"group": "team", "peer": "bob", "route": "srv/api"})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "route_permission")
}
