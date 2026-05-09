//go:build darwin

package agentd

import (
	"fmt"
	"net"
	"syscall"
)

// macOS / BSDs: SOL_LOCAL (=0) + LOCAL_PEERPID (=2) returns the connected
// peer's PID. The constants aren't exported by stdlib syscall on darwin;
// values come from <sys/un.h>.
const (
	solLocal     = 0
	localPeerPID = 2
)

func peerPID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pid int
	var sockErr error
	err = raw.Control(func(fd uintptr) {
		pid, sockErr = syscall.GetsockoptInt(int(fd), solLocal, localPeerPID)
	})
	if err != nil {
		return 0, err
	}
	if sockErr != nil {
		return 0, fmt.Errorf("getsockopt LOCAL_PEERPID: %w", sockErr)
	}
	return pid, nil
}
