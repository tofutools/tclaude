//go:build linux || darwin || freebsd || netbsd || openbsd

package filefollow

import (
	"os"
	"syscall"
)

func fileIdentity(info os.FileInfo) (device, inode uint64, ok bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(stat.Dev), uint64(stat.Ino), true
}
