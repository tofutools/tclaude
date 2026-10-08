//go:build linux || darwin

package hostmetrics

import (
	"fmt"
	"golang.org/x/sys/unix"
)

func readDisk(path string) (uint64, uint64, error) {
	var s unix.Statfs_t
	if err := unix.Statfs(path, &s); err != nil {
		return 0, 0, err
	}
	unit := statfsBlockSize(&s)
	if unit == 0 {
		return 0, 0, fmt.Errorf("invalid filesystem block size")
	}
	total, err := multiply(s.Blocks, unit)
	if err != nil {
		return 0, 0, err
	}
	available, err := multiply(s.Bavail, unit)
	if err != nil {
		return 0, 0, err
	}
	return total, min(total, available), nil
}
