//go:build darwin

package session

import (
	"context"
	"net"
	"net/netip"
)

func darwinHostRouteDial(ctx context.Context, endpoint netip.AddrPort) (net.Conn, error) {
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp4", endpoint.String())
}
