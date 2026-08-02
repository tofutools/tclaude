//go:build darwin

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

// Darwin's filtering proxy consumes the same launch-scoped capability as the
// Linux helper. The capability travels through the existing private Unix
// descriptor handoff and never through argv, environment, or a file.
func prepareRouteHelperCredentialHandoff(credential string) (string, func(), error) {
	credential = strings.TrimSpace(credential)
	if credential == "" {
		return "", func() {}, fmt.Errorf("route helper credential is empty")
	}
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
		defer conn.Close()
		if err := sendRouteHelperCredentialFD(conn, int(readEnd.Fd())); err != nil {
			return
		}
		_, _ = io.WriteString(writeEnd, credential)
	}()
	return path, cleanup, nil
}

func sendRouteHelperCredentialFD(conn *net.UnixConn, fd int) error {
	if conn == nil || fd <= 0 {
		return errors.New("route helper handoff descriptor is invalid")
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
