package agentd_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
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
)

type peerViewWire struct {
	Method string      `json:"method"`
	URI    string      `json:"uri"`
	Status int         `json:"status"`
	Header http.Header `json:"header"`
	Body   []byte      `json:"body"`
}

func readPeerViewWire(t *testing.T, c *stream.Conn) peerViewWire {
	t.Helper()
	require.NoError(t, c.SetDeadline(time.Now().Add(5*time.Second)))
	var prefix [4]byte
	_, err := io.ReadFull(c, prefix[:])
	require.NoError(t, err)
	size := binary.BigEndian.Uint32(prefix[:])
	require.Less(t, size, uint32(12<<20))
	raw := make([]byte, size)
	_, err = io.ReadFull(c, raw)
	require.NoError(t, err)
	var out peerViewWire
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func writePeerViewWire(t *testing.T, c *stream.Conn, w peerViewWire) {
	t.Helper()
	raw, err := json.Marshal(w)
	require.NoError(t, err)
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(raw)))
	_, err = c.Write(prefix[:])
	require.NoError(t, err)
	_, err = c.Write(raw)
	require.NoError(t, err)
}

func acceptOutboundPeerView(t *testing.T, p *fedPeer, after int) *stream.Conn {
	t.Helper()
	var env *proto.Envelope
	fedEventually(t, "peer view open", func() bool {
		es := p.envelopes(proto.KindPeerViewOpen)
		if len(es) <= after {
			return false
		}
		env = es[after]
		return true
	})
	var payload proto.PeerViewOpenPayload
	require.NoError(t, env.DecodePayload(&payload))
	kp, err := stream.NewKeyPair()
	require.NoError(t, err)
	answer := p.envelope(proto.KindPeerViewAnswer, proto.Endpoint{}, proto.PeerViewAnswerPayload{Stream: payload.Stream, OK: true, Key: kp.Pub})
	answer.InReplyTo = "wrong-request"
	p.send(answer)
	answer.InReplyTo = env.ID
	p.send(answer)
	return fedPeerStream(t, p, payload.Stream, kp, payload.Key, false)
}

func openInboundPeerView(t *testing.T, p *fedPeer) *stream.Conn {
	t.Helper()
	kp, err := stream.NewKeyPair()
	require.NoError(t, err)
	sid := proto.NewEnvelopeID()
	env := p.envelope(proto.KindPeerViewOpen, proto.Endpoint{}, proto.PeerViewOpenPayload{Stream: sid, Key: kp.Pub})
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
	require.True(t, answer.OK, answer.Reason)
	return fedPeerStream(t, p, sid, kp, answer.Key, true)
}

func TestFederationPeerViewProxyWireAndFailures(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	p := fh.peer
	h := agentd.BuildDashboardHandlerForTest()
	request := func(method, tail string, body any) chan *httptest.ResponseRecorder {
		t.Helper()
		ch := make(chan *httptest.ResponseRecorder, 1)
		req := testharness.JSONRequest(t, method, "/api/peer/"+p.id.ID()+"/"+tail, body)
		req.Header.Set("Authorization", "Bearer private-token")
		req.Header.Set("If-None-Match", `"map-version"`)
		go func() { ch <- testharness.Serve(h, req) }()
		return ch
	}
	readResult := func(ch chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
		t.Helper()
		select {
		case rec := <-ch:
			return rec
		case <-time.After(5 * time.Second):
			t.Fatal("proxy request did not finish")
			return nil
		}
	}
	ch := request("GET", "node-summary?scope=visible", nil)
	conn := acceptOutboundPeerView(t, p, 0)
	wire := readPeerViewWire(t, conn)
	require.Equal(t, "GET", wire.Method)
	require.Equal(t, "/api/node-summary?scope=visible", wire.URI)
	require.Equal(t, `"map-version"`, wire.Header.Get("If-None-Match"))
	require.Empty(t, wire.Header.Get("Cookie"))
	require.Empty(t, wire.Header.Get("Authorization"))
	writePeerViewWire(t, conn, peerViewWire{Status: 304, Header: http.Header{"Etag": {`"map-version"`}, "Cache-Control": {"private, no-cache"}, "Set-Cookie": {"remote-cookie=secret"}}})
	rec := readResult(ch)
	require.Equal(t, 304, rec.Code, rec.Body.String())
	require.Empty(t, rec.Body.String())
	require.Equal(t, `"map-version"`, rec.Header().Get("ETag"))
	require.Empty(t, rec.Header().Get("Set-Cookie"))
	_ = conn.Close()
	ch = request("POST", "operator-message", map[string]any{"to": "hidden", "body": "remote"})
	conn = acceptOutboundPeerView(t, p, 1)
	wire = readPeerViewWire(t, conn)
	require.Equal(t, "POST", wire.Method)
	require.Equal(t, "/api/operator-message", wire.URI)
	require.Contains(t, string(wire.Body), "remote")
	writePeerViewWire(t, conn, peerViewWire{Status: 404, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"error":"not found","code":"not_found"}`)})
	rec = readResult(ch)
	require.Equal(t, 404, rec.Code)
	require.JSONEq(t, `{"error":"not found","code":"not_found"}`, rec.Body.String())
	_ = conn.Close()
	ch = request("GET", "snapshot", nil)
	conn = acceptOutboundPeerView(t, p, 2)
	_ = readPeerViewWire(t, conn)
	writePeerViewWire(t, conn, peerViewWire{Status: 200, Header: http.Header{"Content-Type": {"text/html"}}, Body: []byte(`<script>fetch('/api/operator-message',{method:'POST'})</script>`)})
	rec = readResult(ch)
	require.Equal(t, 502, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "peer_invalid_response")
	require.NotContains(t, rec.Body.String(), "<script>")
	require.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	require.Contains(t, rec.Header().Get("Content-Security-Policy"), "sandbox")
	_ = conn.Close()
	// A slow peer cannot hang the browser, even if it never answers the open.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	rec = testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/peer/"+p.id.ID()+"/snapshot", nil).WithContext(ctx))
	require.Equal(t, 504, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "peer_timeout")
	require.Contains(t, rec.Body.String(), "last_seen")
	// With no live runtime, fail immediately rather than starting a retry loop.
	agentd.ResetFederationForTest()
	rec = testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/peer/"+p.id.ID()+"/snapshot", nil))
	require.Equal(t, 502, rec.Code)
	require.Contains(t, rec.Body.String(), "peer_unreachable")
	require.Contains(t, rec.Body.String(), "peer_offline")
	_, err := db.UntrustFederationPeer(p.id.ID())
	require.NoError(t, err)
	rec = testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/peer/"+p.id.ID()+"/snapshot", nil))
	require.Equal(t, 403, rec.Code)
	require.Contains(t, rec.Body.String(), "not_trusted")
}

func TestFederationPeerViewIncomingDispatcherAndRevocation(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	f.HaveGroup("visible")
	f.HaveGroup("secret")
	f.HaveConvWithTitle("visible-conv", "shown")
	f.HaveMember("visible", "visible-conv")
	f.HaveConvWithTitle("secret-conv", "hidden-name")
	f.HaveMember("secret", "secret-conv")
	rec := fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermGroupsRosterRead, "scope": "group=visible"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	exchange := func(uri string) peerViewWire {
		t.Helper()
		c := openInboundPeerView(t, p)
		defer c.Close()
		writePeerViewWire(t, c, peerViewWire{Method: "GET", URI: uri, Header: http.Header{"Cookie": {"operator-token=forged"}, "Authorization": {"Bearer forged"}}})
		return readPeerViewWire(t, c)
	}
	reply := exchange("/api/snapshot")
	require.Equal(t, 200, reply.Status, string(reply.Body))
	require.Contains(t, string(reply.Body), "shown")
	require.NotContains(t, string(reply.Body), "hidden-name")
	require.Contains(t, string(reply.Body), "peer_view")
	// Cookie-like wire credentials never route the peer to local-human wrappers.
	reply = exchange("/api/config")
	require.Equal(t, 403, reply.Status, string(reply.Body))
	require.Contains(t, string(reply.Body), "local")
	// Trust is checked at dispatch, not only when the stream was opened.
	c := openInboundPeerView(t, p)
	defer c.Close()
	_, err := db.UntrustFederationPeer(p.id.ID())
	require.NoError(t, err)
	writePeerViewWire(t, c, peerViewWire{Method: "GET", URI: "/api/snapshot"})
	reply = readPeerViewWire(t, c)
	require.Equal(t, 403, reply.Status, string(reply.Body))
}
