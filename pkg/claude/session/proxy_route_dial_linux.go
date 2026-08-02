//go:build linux

package session

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"strconv"

	"golang.org/x/sys/unix"
)

func linuxNamespaceRouteDial(namespacePID int) func(context.Context, netip.AddrPort) (net.Conn, error) {
	return func(ctx context.Context, endpoint netip.AddrPort) (net.Conn, error) {
		if namespacePID <= 0 || !endpoint.IsValid() || endpoint.Addr().Zone() != "" ||
			!endpoint.Addr().IsLoopback() || endpoint.Addr().IsUnspecified() {
			return nil, fmt.Errorf("route namespace dial endpoint is not a loopback literal")
		}
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		original, err := unix.Open("/proc/self/ns/net", unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, fmt.Errorf("open original network namespace: %w", err)
		}
		defer func() { _ = unix.Close(original) }()
		target, err := unix.Open("/proc/"+strconv.Itoa(namespacePID)+"/ns/net", unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, fmt.Errorf("open sandbox network namespace: %w", err)
		}
		defer func() { _ = unix.Close(target) }()
		if err := unix.Setns(target, unix.CLONE_NEWNET); err != nil {
			return nil, fmt.Errorf("enter sandbox network namespace: %w", err)
		}
		var conn net.Conn
		dialErr := func() error {
			if !endpoint.Addr().IsLoopback() || endpoint.Addr().IsUnspecified() {
				return fmt.Errorf("route namespace dial endpoint changed from loopback")
			}
			var dialer net.Dialer
			conn, err = dialer.DialContext(ctx, "tcp4", endpoint.String())
			return err
		}()
		restoreErr := unix.Setns(original, unix.CLONE_NEWNET)
		if restoreErr != nil {
			if conn != nil {
				_ = conn.Close()
			}
			return nil, fmt.Errorf("restore original network namespace: %w", restoreErr)
		}
		if dialErr != nil {
			return nil, dialErr
		}
		return conn, nil
	}
}
