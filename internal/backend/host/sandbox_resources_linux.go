package host

import (
	"golang.org/x/sys/unix"
	"os"
)

func openSandboxProviderResource(path, kind string) (*os.File, error) {
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if kind == "socket" {
		flags = unix.O_PATH | unix.O_CLOEXEC | unix.O_NOFOLLOW
	}
	fd, err := unix.Open(path, flags, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func sandboxProviderResourceMatches(file *os.File, _ string, _ string, expected os.FileInfo) (bool, error) {
	current, err := file.Stat()
	return err == nil && os.SameFile(expected, current), err
}
