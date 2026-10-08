//go:build linux

package agentd_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/routeadapter"
	"github.com/tofutools/tclaude/pkg/claude/routebroker"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/federation/stream"
)

// fedFlowChannel is a helper channel on which agentd granted flow control,
// as the Unix-socket handshake reports it.
type fedFlowChannel struct {
	net.Conn
	window int
}

func (c fedFlowChannel) FlowWindow() int { return c.window }

func fedRandom(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// TestFederation_RouteFlowControlServesBulkToSlowPeer runs a real,
// flow-capable publisher helper behind a route a peer opens. The target
// sends far more than one window while the peer is not reading yet, and the
// hub's bandwidth cap holds the stream to its rate the whole way: the
// target is held back by credit instead of overrunning agentd, and every
// byte arrives in both directions.
func TestFederation_RouteFlowControlServesBulkToSlowPeer(t *testing.T) {
	skipFedRoutes(t)
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer

	download, upload := fedRandom(3<<20), fedRandom(1<<20)
	target, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = target.Close() })
	uploaded := make(chan []byte, 1)
	go func() {
		c, err := target.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		go func() {
			_, _ = c.Write(download)
			_ = c.(*net.TCPConn).CloseWrite()
		}()
		time.Sleep(500 * time.Millisecond) // a target slow to start reading
		got, _ := io.ReadAll(c)
		uploaded <- got
	}()

	const pub = "fedfc-pub-bbbb-cccc-000000000001"
	f.HaveGroup("svc")
	f.HaveConvWithTitle(pub, "api-server")
	f.HaveMember("svc", pub)
	f.HaveAliveSession(pub, "spwn-fedfc", "tclaude-spwn-fedfc", f.TestCwd("pub"))
	g, err := db.GetAgentGroupByName("svc")
	require.NoError(t, err)
	require.NoError(t, db.ReplaceAgentGroupPermissions(g.ID, []string{agentd.PermRoutesPublish}, "test"))
	rec, route := serveRouteAgent(t, f, http.MethodPost, "/v1/routes/publish", pub, map[string]any{
		"group": "svc", "name": "bulk", "target": "tcp://" + target.Addr().String(),
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	routeID := route["id"].(string)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	helper, brokerSide := net.Pipe()
	go func() {
		_ = agentd.GroupRouteBroker().AttachPublisher(ctx, routebroker.PublisherAuth{
			RouteID: routeID, AgentID: route["publisher_agent_id"].(string), ConvID: route["publisher_conv_id"].(string),
			LaunchGeneration: route["publisher_launch_generation"].(string), GroupGeneration: int64(route["group_generation"].(float64)),
			FlowWindow: routebroker.InitialWindow,
		}, brokerSide)
	}()
	go func() {
		_ = routeadapter.RunPublisher(ctx, fedFlowChannel{helper, routebroker.InitialWindow}, "tcp://"+target.Addr().String())
	}()

	rec = fedGrantCaps(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"group": "svc", "peer": "bob", "caps": []string{"routes"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	fedEventually(t, "catalog with the route", func() bool {
		cats := p.envelopes(proto.KindCatalog)
		if len(cats) == 0 {
			return false
		}
		var cat proto.CatalogPayload
		require.NoError(t, cats[len(cats)-1].DecodePayload(&cat))
		return len(cat.Groups) == 1 && len(cat.Groups[0].Routes) == 1
	})

	kp, err := stream.NewKeyPair()
	require.NoError(t, err)
	sid := proto.NewEnvelopeID()
	p.send(p.envelope(proto.KindRouteOpen, proto.Endpoint{}, proto.RouteOpenPayload{Route: routeID, Stream: sid, Key: kp.Pub}))
	ans := fedRouteAnswerFor(t, p, sid)
	require.True(t, ans.OK, ans.Reason)
	conn := fedPeerStream(t, p, sid, kp, ans.Key, true)

	go func() {
		_, _ = conn.Write(upload)
		_ = conn.CloseWrite()
	}()
	time.Sleep(500 * time.Millisecond) // a peer slow to start reading
	got, err := io.ReadAll(conn)
	require.NoError(t, err)
	require.True(t, bytes.Equal(download, got), "download: got %d of %d bytes", len(got), len(download))
	select {
	case up := <-uploaded:
		require.True(t, bytes.Equal(upload, up), "upload: got %d of %d bytes", len(up), len(upload))
	case <-time.After(30 * time.Second):
		t.Fatal("upload did not reach the target")
	}
}

// TestFederation_RouteFlowControlOpensRemoteRouteInBulk is the consumer
// side: a real, flow-capable consumer helper on a mirror lease, a peer
// serving the remote route, and bulk both ways to readers that stall.
func TestFederation_RouteFlowControlOpensRemoteRouteInBulk(t *testing.T) {
	skipFedRoutes(t)
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer

	const alice = "fedfc-alice-bbbb-cccc-000000000001"
	f.HaveGroup("team")
	f.HaveConvWithTitle(alice, "alice")
	f.HaveMember("team", alice)
	f.HaveAliveSession(alice, "spwn-fedfc-alice", "tclaude-spwn-fedfc-alice", f.TestCwd("alice"))
	g, err := db.GetAgentGroupByName("team")
	require.NoError(t, err)
	require.NoError(t, db.ReplaceAgentGroupPermissions(g.ID, []string{agentd.PermRoutesConsume}, "test"))

	const remoteRoute = "rte_00112233445566778899aabbccddeef0"
	p.send(p.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{Groups: []proto.CatalogGroup{{
		Name: "builders", Caps: []string{proto.CapRoutes},
		Routes: []proto.CatalogRoute{{ID: remoteRoute, Publisher: "srv", Name: "bulk"}},
	}}}))
	require.NoError(t, db.GrantAgentPermissionWithScope(alice, agentd.PermRoutesConsume, `{"peer":["`+p.id.ID()+`/builders"]}`, "test"))
	var lease map[string]any
	fedEventually(t, "remote route opens", func() bool {
		r, body := serveRouteAgent(t, f, http.MethodPost, "/v1/federation/routes/open", alice, map[string]any{"group": "team", "peer": "bob", "route": "srv/bulk"})
		lease = body
		return r.Code == http.StatusCreated
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	helper, brokerSide := net.Pipe()
	attached := make(chan struct{})
	go func() {
		_ = agentd.GroupRouteBroker().AttachConsumerWithReady(ctx, routebroker.ConsumerAuth{
			LeaseID: lease["id"].(string), RouteID: lease["route_id"].(string), AgentID: lease["consumer_agent_id"].(string), ConvID: lease["consumer_conv_id"].(string),
			LaunchGeneration: lease["consumer_launch_generation"].(string), GroupGeneration: int64(lease["group_generation"].(float64)),
			FlowWindow: routebroker.InitialWindow,
		}, brokerSide, func() error { close(attached); return nil })
	}()
	<-attached
	local, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = routeadapter.RunConsumer(ctx, fedFlowChannel{helper, routebroker.InitialWindow}, local) }()

	download, upload := fedRandom(3<<20), fedRandom(1<<20)
	client, err := net.Dial("tcp4", local.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	// Bob serves the real route.
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
	kp, err := stream.NewKeyPair()
	require.NoError(t, err)
	p.send(p.envelope(proto.KindRouteAnswer, proto.Endpoint{}, proto.RouteAnswerPayload{Stream: req.Stream, OK: true, Key: kp.Pub}))
	conn := fedPeerStream(t, p, req.Stream, kp, req.Key, false)
	uploaded := make(chan []byte, 1)
	go func() {
		_, _ = conn.Write(download)
		_ = conn.CloseWrite()
	}()
	go func() {
		time.Sleep(500 * time.Millisecond) // the remote server is slow to read
		got, _ := io.ReadAll(conn)
		uploaded <- got
	}()

	go func() {
		_, _ = client.Write(upload)
		_ = client.(*net.TCPConn).CloseWrite()
	}()
	time.Sleep(500 * time.Millisecond) // and so is the local client
	_ = client.SetReadDeadline(time.Now().Add(60 * time.Second))
	got, err := io.ReadAll(client)
	require.NoError(t, err)
	require.True(t, bytes.Equal(download, got), "download: got %d of %d bytes", len(got), len(download))
	select {
	case up := <-uploaded:
		require.True(t, bytes.Equal(upload, up), "upload: got %d of %d bytes", len(up), len(upload))
	case <-time.After(30 * time.Second):
		t.Fatal("upload did not reach the remote server")
	}
}
