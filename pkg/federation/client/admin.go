package client

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// AdminCall never retries. A disconnect after execution has an ambiguous
// outcome; callers inspect state before submitting a fresh explicit mutation.
func (c *Client) AdminCall(ctx context.Context, method string, payload any) (*proto.HubAdminResult, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if payload == nil {
		raw = []byte(`{}`)
	}
	if len(raw) > proto.MaxAdminPayload {
		return nil, errors.New("hub admin payload exceeds 64 KiB")
	}
	c.mu.Lock()
	ws, nonce, generation, hubID := c.ws, c.adminNonce, c.adminGeneration, c.status.HubID
	if ws == nil {
		c.mu.Unlock()
		return nil, ErrNotConnected
	}
	if c.status.HubAdminVersion != 1 {
		c.mu.Unlock()
		return nil, errors.New("connected hub does not support admin RPCs; upgrade it locally")
	}
	now := time.Now().UTC()
	request := &proto.HubAdminRequest{ID: proto.NewEnvelopeID(), HubID: hubID, Nonce: nonce, Generation: generation, Method: method, Payload: raw, IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
	request.Sign(c.opts.Identity)
	ch := make(chan *proto.Frame, 1)
	c.pending[request.ID] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, request.ID); c.mu.Unlock() }()
	c.wmu.Lock()
	_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err = ws.WriteJSON(&proto.Frame{Type: proto.FrameAdminRequest, AdminRequest: request})
	c.wmu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case frame, ok := <-ch:
		if !ok {
			return nil, ErrNotConnected
		}
		if frame.AdminResult == nil {
			return nil, errors.New("invalid hub admin result")
		}
		return frame.AdminResult, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
