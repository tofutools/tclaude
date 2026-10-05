package client

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// StreamURL returns the hub's stream relay URL derived from its main URL.
func (c *Client) StreamURL() string {
	u := *c.u
	u.Path = strings.TrimSuffix(u.Path, proto.WSPath) + proto.StreamPath
	return u.String()
}

// DialStream joins stream sid on the hub, expecting peer on the other end,
// and returns the websocket once the hub reports both sides present. The
// caller wraps it in an encrypted stream; the hub only forwards bytes.
func (c *Client) DialStream(ctx context.Context, sid, peer string) (*websocket.Conn, error) {
	d := websocket.Dialer{TLSClientConfig: c.opts.TLS, HandshakeTimeout: 15 * time.Second, Proxy: http.ProxyFromEnvironment}
	ws, _, err := d.DialContext(ctx, c.StreamURL(), nil)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = ws.Close()
		}
	}()
	stop := context.AfterFunc(ctx, func() { _ = ws.Close() })
	defer stop()
	ws.SetReadLimit(proto.MaxStreamMessage + 4<<10)
	_ = ws.SetReadDeadline(time.Now().Add(15 * time.Second))
	var ch proto.Frame
	if err := ws.ReadJSON(&ch); err != nil {
		return nil, err
	}
	if ch.Type != proto.FrameChallenge {
		return nil, fmt.Errorf("expected challenge, got %q", ch.Type)
	}
	id := c.opts.Identity
	hello := &proto.Frame{
		Type: proto.FrameHello, Proto: proto.ProtocolVersion,
		InstanceID: id.ID(), PubKey: id.Pub, Version: c.opts.Version,
		Sig: proto.SignHello(id, ch.HubID, ch.Nonce), Stream: sid, Peer: peer,
	}
	_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := ws.WriteJSON(hello); err != nil {
		return nil, err
	}
	_ = ws.SetWriteDeadline(time.Time{})
	// The hub answers once the peer joins (or refuses / times out).
	_ = ws.SetReadDeadline(time.Now().Add(60 * time.Second))
	var ready proto.Frame
	if err := ws.ReadJSON(&ready); err != nil {
		return nil, err
	}
	if ready.Type == proto.FrameError {
		return nil, &RefusedError{Code: ready.Code, Message: ready.Message}
	}
	if ready.Type != proto.FrameStreamReady || ready.Stream != sid {
		return nil, fmt.Errorf("expected stream_ready, got %q", ready.Type)
	}
	_ = ws.SetReadDeadline(time.Time{})
	ok = true
	return ws, nil
}
