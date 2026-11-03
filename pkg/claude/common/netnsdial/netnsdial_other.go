//go:build !linux

package netnsdial

import (
	"context"
	"errors"
	"net"
)

// HelperMode is the private argv token of the in-namespace helper.
const HelperMode = "__tclaude_stacked_probe_netns_dial"

// ErrRefused reports that nothing accepted the connection inside the
// namespace.
var ErrRefused = errors.New("connection refused inside the namespace")

// Dial is Linux-only: other platforms have no private network namespaces.
func Dial(context.Context, string, int, int) (net.Conn, error) {
	return nil, errors.New("network namespaces are Linux-only")
}

// RunHelper never handles anything off Linux.
func RunHelper([]string) (bool, int) { return false, 0 }
