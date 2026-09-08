//go:build linux || darwin

package host

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

// UnixControlIdentity identifies one already-bound private control endpoint.
// It is retained by the owning launch, rather than inferred from a native URL.
type UnixControlIdentity struct {
	Path   string
	Device uint64
	Inode  uint64
}

func InspectUnixControl(path string) (UnixControlIdentity, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return UnixControlIdentity{}, fmt.Errorf("control endpoint requires an exact absolute path")
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil || !parent.IsDir() || parent.Mode().Perm()&0077 != 0 {
		return UnixControlIdentity{}, fmt.Errorf("control endpoint requires a private directory")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return UnixControlIdentity{}, err
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 {
		return UnixControlIdentity{}, fmt.Errorf("control endpoint is not a private Unix socket")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return UnixControlIdentity{}, fmt.Errorf("control endpoint identity unavailable")
	}
	return UnixControlIdentity{Path: path, Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}, nil
}

// DialUnixControl proves the listener's creating process before any credential
// or request bytes are sent. The owning bootstrap binds before exec, retaining
// its PID across the native wrapper and its listener across inherited FDs.
func (p *Process) DialUnixControl(ctx context.Context, expected UnixControlIdentity) (net.Conn, error) {
	if !p.Observe().Running {
		return nil, fmt.Errorf("control endpoint owner is not running")
	}
	before, err := InspectUnixControl(expected.Path)
	if err != nil || before != expected {
		return nil, fmt.Errorf("control endpoint identity changed")
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", expected.Path)
	if err != nil {
		return nil, err
	}
	refuse := func(err error) (net.Conn, error) { _ = connection.Close(); return nil, err }
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return refuse(fmt.Errorf("control endpoint is not a Unix connection"))
	}
	raw, err := unixConnection.SyscallConn()
	if err != nil {
		return refuse(err)
	}
	var peer int
	var peerErr error
	if err := raw.Control(func(fd uintptr) { peer, peerErr = unixControlPeerPID(fd) }); err != nil {
		return refuse(err)
	}
	if peerErr != nil || peer != p.identity.PID || !p.Observe().Running {
		return refuse(fmt.Errorf("control endpoint does not belong to the retained process"))
	}
	after, err := InspectUnixControl(expected.Path)
	if err != nil || after != expected {
		return refuse(fmt.Errorf("control endpoint changed during connection"))
	}
	return connection, nil
}
