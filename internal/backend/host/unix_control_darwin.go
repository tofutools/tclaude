//go:build darwin

package host

import "golang.org/x/sys/unix"

func unixControlPeerPID(fd uintptr) (int, error) {
	return unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
}
