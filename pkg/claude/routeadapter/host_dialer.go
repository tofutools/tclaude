package routeadapter

import (
	"context"
	"errors"
	"net"
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
		select {
		case slots <- struct{}{}:
		default:
			return nil, ErrHostStreamLimit
		}
		client, server := net.Pipe()
		go func() {
			defer func() { <-slots }()
			a.consumerStream(channelCtx, server, consumer)
		}()
		return client, nil
	}, nil
}
