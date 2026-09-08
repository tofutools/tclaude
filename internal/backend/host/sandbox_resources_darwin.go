package host

import (
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

func openSandboxProviderResource(path, kind string) (*os.File, error) {
	// Darwin cannot open a Unix socket vnode, even with O_EVTONLY. Hold its
	// parent for descriptor-relative identity inspection; Seatbelt still receives
	// only the socket's literal path, never a grant for this parent directory.
	if kind == "socket" {
		path = filepath.Dir(path)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func sandboxProviderResourceMatches(file *os.File, path, kind string, expected os.FileInfo) (bool, error) {
	if kind != "socket" {
		current, err := file.Stat()
		return err == nil && os.SameFile(expected, current), err
	}
	var current unix.Stat_t
	if err := unix.Fstatat(int(file.Fd()), filepath.Base(path), &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return false, err
	}
	identity, ok := expected.Sys().(*syscall.Stat_t)
	return ok && current.Mode&unix.S_IFMT == unix.S_IFSOCK && current.Dev == identity.Dev && current.Ino == identity.Ino, nil
}
