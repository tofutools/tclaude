package routeadapter

import (
	"context"
	"errors"
	"net"
	"time"
)

// ErrHostStreamLimit indicates that every stream of a host HTTP lease is busy.
var ErrHostStreamLimit = errors.New("host route stream limit reached")

// OpenHostDialer creates an in-memory consumer endpoint for an HTTP transport.
// Slots cover the complete broker stream lifetime, including HTTP upgrades and
// idle keep-alive connections. No additional host TCP listener is exposed.
func (a *Adapter) OpenHostDialer(ctx context.Context, consumer Consumer) (func(context.Context, string, string) (net.Conn, error), error) {
	channelCtx, cancel := context.WithCancel(ctx)
	a.mu.Lock()
	if a.closed || a.leases[consumer.LeaseID] != nil {
		a.mu.Unlock()
		cancel()
		return nil, errors.New("host route adapter closed or lease already exists")
	}
	a.leases[consumer.LeaseID] = &consumerState{routeID: consumer.RouteID, agentID: consumer.AgentID, cancel: cancel}
	a.mu.Unlock()
	slots := make(chan struct{}, 8)
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := channelCtx.Err(); err != nil {
			return nil, err
		}
		timer := time.NewTimer(250 * time.Millisecond)
		defer timer.Stop()
		select {
		case slots <- struct{}{}:
		case <-timer.C:
			return nil, ErrHostStreamLimit
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-channelCtx.Done():
			return nil, channelCtx.Err()
		}
		client, server := net.Pipe()
		streamCtx, streamCancel := context.WithCancel(channelCtx)
		go func() {
			defer func() { <-slots }()
			defer streamCancel()
			a.consumerStream(streamCtx, server, consumer)
		}()
		return &hostDialConn{Conn: client, cancel: streamCancel}, nil
	}, nil
}

// A transport Close is a full close, unlike a raw TCP CloseWrite. Cancel the
// broker stream too, even when the target keeps its response half open.
type hostDialConn struct {
	net.Conn
	cancel context.CancelFunc
}

func (c *hostDialConn) Close() error {
	c.cancel()
	return c.Conn.Close()
}
