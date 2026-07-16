//go:build linux

package resumeprovenance

import (
	"fmt"
	"os"
	"syscall"
)

func platformFileIdentity(info os.FileInfo) (uint64, uint64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return 0, 0, fmt.Errorf("unsupported Linux stat payload %T", info.Sys())
	}
	return linuxStatIdentity(stat)
}

func linuxStatIdentity(stat *syscall.Stat_t) (uint64, uint64, error) {
	device, inode := uint64(stat.Dev), uint64(stat.Ino)
	if device == 0 || inode == 0 {
		return 0, 0, fmt.Errorf("invalid device/inode %d/%d", device, inode)
	}
	return device, inode, nil
}
