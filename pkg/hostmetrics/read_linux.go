//go:build linux

package hostmetrics

import (
	"golang.org/x/sys/unix"
	"os"
)

func readLoad() ([3]float64, error) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return [3]float64{}, err
	}
	return parseLoad(string(b))
}
func readMemory() (*Memory, error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return nil, err
	}
	return parseLinuxMemory(string(b))
}

func statfsBlockSize(s *unix.Statfs_t) uint64 {
	if s.Frsize > 0 {
		return uint64(s.Frsize)
	}
	if s.Bsize > 0 {
		return uint64(s.Bsize)
	}
	return 0
}
