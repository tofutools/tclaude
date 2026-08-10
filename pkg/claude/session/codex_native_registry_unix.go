//go:build unix

package session

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func fileOwnerUID(info os.FileInfo) (uint32, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return stat.Uid, true
}

func currentUID() uint32 { return uint32(os.Getuid()) }

type nativeRegistryLock struct{ file *os.File }

func acquireNativeRegistryLock(path string) (*nativeRegistryLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &nativeRegistryLock{file: file}, nil
}

func (l *nativeRegistryLock) Close() error {
	_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	return l.file.Close()
}
