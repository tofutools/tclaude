//go:build linux

package agentd

import (
	"fmt"
	"net"
	"syscall"
)

// Linux: SO_PEERCRED returns struct ucred {pid, uid, gid}.
func peerPID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var ucred *syscall.Ucred
	var sockErr error
	err = raw.Control(func(fd uintptr) {
		ucred, sockErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if err != nil {
		return 0, err
	}
	if sockErr != nil {
		return 0, fmt.Errorf("getsockopt SO_PEERCRED: %w", sockErr)
	}
	return int(ucred.Pid), nil
}
