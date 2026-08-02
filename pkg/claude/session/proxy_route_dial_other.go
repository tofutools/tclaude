//go:build !linux && !darwin

package session

import (
	"context"
	"net"
	"net/netip"
)

func unsupportedRouteDial(context.Context, netip.AddrPort) (net.Conn, error) {
	return nil, net.ErrClosed
}
