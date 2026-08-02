//go:build linux

package agentd

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const routeHelperHandoffTimeout = 5 * time.Minute

// prepareRouteHelperCredentialHandoff creates a private, one-shot descriptor
// handoff. The socket name is only a rendezvous endpoint; the bearer travels
// from daemon memory through an inherited pipe and is never represented by a
// filesystem object, argv value, environment entry, or mount.
func prepareRouteHelperCredentialHandoff(credential string) (string, func(), error) {
	credential = strings.TrimSpace(credential)
	if credential == "" {
		return "", func() {}, fmt.Errorf("route helper credential is empty")
	}
	// Unix pathname sockets have a small kernel path limit. Keep the endpoint
	// in a fresh mode-0700 directory directly below /tmp, whose name is opaque
	// and whose contents are removed after this one-shot handoff.
	dir, err := os.MkdirTemp("/tmp", "tclaude-route-helper-")
	if err != nil {
		return "", func() {}, fmt.Errorf("create route helper handoff directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.Remove(dir)
		return "", func() {}, fmt.Errorf("protect route helper handoff directory: %w", err)
	}
	name, err := randomRouteHelperSecret(16)
	if err != nil {
		_ = os.Remove(dir)
		return "", func() {}, err
	}
	path := dir + "/handoff-" + name + ".sock"
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		_ = os.Remove(dir)
		return "", func() {}, fmt.Errorf("create route helper handoff socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		_ = os.Remove(dir)
		return "", func() {}, fmt.Errorf("protect route helper handoff socket: %w", err)
	}
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		_ = os.Remove(dir)
		return "", func() {}, fmt.Errorf("create route helper credential pipe: %w", err)
	}
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			_ = listener.Close()
			_ = readEnd.Close()
			_ = writeEnd.Close()
			_ = os.Remove(path)
			_ = os.Remove(dir)
		})
	}
	go func() {
		defer cleanup()
		_ = listener.SetDeadline(time.Now().Add(routeHelperHandoffTimeout))
		conn, acceptErr := listener.AcceptUnix()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		if err := sendRouteHelperCredentialFD(conn, int(readEnd.Fd())); err != nil {
			return
		}
		// This write blocks only until the helper consumes the inherited pipe;
		// cleanup closes both ends on every timeout, launch, and failure path.
		_, _ = io.WriteString(writeEnd, credential)
	}()
	return path, cleanup, nil
}

func sendRouteHelperCredentialFD(conn *net.UnixConn, fd int) error {
	if conn == nil || fd <= 0 {
		return fmt.Errorf("route helper handoff descriptor is invalid")
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var sendErr error
	if err := raw.Write(func(socketFD uintptr) bool {
		_, sendErr = unix.SendmsgN(int(socketFD), []byte{'r'}, unix.UnixRights(fd), nil, 0)
		if errors.Is(sendErr, unix.EAGAIN) || errors.Is(sendErr, unix.EWOULDBLOCK) {
			return false
		}
		return true
	}); err != nil {
		return err
	}
	return sendErr
}
