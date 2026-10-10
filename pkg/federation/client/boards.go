package client

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// BoardCall uses a separate board-scoped connection and never retries a
// mutation. It does not start a fleet connection or request a peer directory.
func BoardCall(ctx context.Context, opts Options, token, method string, payload any) (*proto.HubAdminResult, error) {
	u, err := ValidateURL(opts.URL)
	if err != nil {
		return nil, err
	}
	u.Path = proto.BoardWSPath
	u.RawQuery = ""
	u.Fragment = ""
	d := websocket.Dialer{TLSClientConfig: opts.TLS, HandshakeTimeout: 10 * time.Second}
	ws, _, err := d.DialContext(ctx, u.String(), nil)
	if err != nil {
		return nil, err
	}
	defer ws.Close()
	ws.SetReadLimit(proto.MaxAdminResult + 16384)
	deadline := time.Now().Add(30 * time.Second)
	if v, ok := ctx.Deadline(); ok && v.Before(deadline) {
		deadline = v
	}
	ws.SetReadDeadline(deadline)
	ws.SetWriteDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { ws.Close() })
	defer stop()
	var ch proto.Frame
	if err = ws.ReadJSON(&ch); err != nil {
		return nil, err
	}
	if ch.Type != proto.FrameChallenge {
		return nil, fmt.Errorf("invalid board challenge")
	}
	if opts.Identity == nil {
		return nil, fmt.Errorf("board instance identity required")
	}
	hello := &proto.Frame{Type: proto.FrameHello, Proto: proto.ProtocolVersion, InstanceID: opts.Identity.ID(), PubKey: opts.Identity.Pub, Sig: proto.SignHello(opts.Identity, ch.HubID, ch.Nonce), BoardToken: token}
	if err = ws.WriteJSON(hello); err != nil {
		return nil, err
	}
	var welcome proto.Frame
	if err = ws.ReadJSON(&welcome); err != nil {
		return nil, err
	}
	if welcome.Type == proto.FrameError {
		return nil, &RefusedError{welcome.Code, welcome.Message}
	}
	if welcome.Type != proto.FrameWelcome {
		return nil, fmt.Errorf("invalid board welcome")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if len(raw) > proto.MaxAdminPayload {
		return nil, fmt.Errorf("board payload exceeds limit")
	}
	now := time.Now()
	req := &proto.BoardRequest{ID: proto.NewEnvelopeID(), HubID: ch.HubID, Nonce: ch.Nonce, Generation: welcome.AdminGeneration, Method: method, Payload: raw, IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
	req.Sign(opts.Identity)
	if err = ws.WriteJSON(&proto.Frame{Type: proto.FrameBoardRequest, BoardRequest: req}); err != nil {
		return nil, err
	}
	var result proto.Frame
	if err = ws.ReadJSON(&result); err != nil {
		return nil, err
	}
	if result.Type != proto.FrameBoardResult || result.BoardResult == nil || result.BoardResult.ID != req.ID || result.BoardResult.Status < 200 || result.BoardResult.Status > 599 {
		return nil, fmt.Errorf("invalid board result")
	}
	return result.BoardResult, nil
}
